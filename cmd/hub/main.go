// Command hub is the WorkBuddy2API gateway (Go rewrite).
//
// STATUS: Phase 0/1 vertical slice. It serves /v1/chat/completions through the
// real account pool, per-account transports and leak filter; panel, models
// catalog, token refresh and the rest of the legacy surface are not ported yet.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"wb2api/internal/api"
	"wb2api/internal/gateway/leak"
	"wb2api/internal/gateway/openai"
	"wb2api/internal/panelauth"
	"wb2api/internal/pool"
	"wb2api/internal/refresh"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/upstream"
	"wb2api/internal/usagedb"
)

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	host := flag.String("host", envOr("HOST", "127.0.0.1"), "listen host")
	port := flag.Int("port", func() int { p, _ := strconv.Atoi(envOr("PORT", "8788")); return p }(), "listen port")
	lan := flag.Bool("lan", false, "listen on all interfaces")
	accountsDir := flag.String("accounts-dir", envOr("WB_ACCOUNTS_DIR", "accounts"), "directory with account credential files and settings.json")
	upstreamOverride := flag.String("upstream-override", os.Getenv("WB_UPSTREAM_OVERRIDE"), "send every realm to this base URL (testing)")
	legacyFilter := flag.Bool("legacy-leak-filter", os.Getenv("WB_LEGACY_LEAK_FILTER") == "1", "use the legacy hold policy of the tool-call leak filter")
	maxAttempts := flag.Int("max-attempts", 5, "account failover attempts per request")
	perAccount := flag.Int("per-account-inflight", 0, "max concurrent requests per account (0 = unlimited)")
	usageDir := flag.String("usage-dir", envOr("WB_PROXY_USAGE_DIR", "usage"), "directory for usage.db (and the legacy usage.jsonl to import)")
	panelPassword := flag.String("panel-password", os.Getenv("WB_PANEL_PASSWORD"), "set the dashboard password at start-up")
	debug := flag.Bool("debug", os.Getenv("WB_DEBUG") == "1", "expose /debug/runtime")
	flag.Parse()

	if *lan && *host == "127.0.0.1" {
		*host = "0.0.0.0"
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	recs, errs := accounts.Load(*accountsDir, time.Now())
	for _, e := range errs {
		log.Warn("skipping unreadable account file", "err", e)
	}
	p := pool.New(recs)
	if *perAccount > 0 {
		for _, a := range p.All() {
			a.SetMaxInflight(*perAccount)
		}
	}
	st := settings.Open(*accountsDir)

	mode := leak.Precise
	if *legacyFilter {
		mode = leak.Legacy
	}
	up := upstream.NewManager(upstream.DefaultTimeouts())
	persist := func(rec *accounts.Record) error {
		_, err := accounts.Save(*accountsDir, rec)
		return err
	}
	refresher := &refresh.Refresher{Pool: p, Up: up, Log: log, Persist: persist}
	if *upstreamOverride != "" {
		refresher.BaseURL = func(*pool.Account) string { return *upstreamOverride }
	}
	h := &openai.Handler{
		Pool: p, Up: up, Settings: st, Log: log, Refresher: refresher, Persist: persist,
		UpstreamOverride: *upstreamOverride, MaxAttempts: *maxAttempts, FilterMode: mode,
	}
	if err := os.MkdirAll(*usageDir, 0o755); err != nil {
		log.Error("cannot create the usage directory", "dir", *usageDir, "err", err)
		os.Exit(1)
	}
	udb, err := usagedb.Open(filepath.Join(*usageDir, "usage.db"), 0)
	if err != nil {
		log.Error("cannot open the usage database", "err", err)
		os.Exit(1)
	}
	defer udb.Close()
	realmOf := api.RealmOf(p)
	trace := os.Getenv("WB_TRACE") == "1"
	h.OnUsage = func(ev openai.UsageEvent) {
		udb.Record(api.EventFromGateway(ev, realmOf))
		if trace {
			log.Info("usage", "ttft_ms", ev.TTFT.Milliseconds(), "elapsed_ms", ev.Elapsed.Milliseconds(), "chunks", ev.Chunks, "outcome", ev.Outcome)
		}
	}
	// History written by the Python gateway is imported in the background; the
	// gateway serves requests while it runs and the import resumes where it
	// stopped if the process restarts.
	go func() {
		legacy := filepath.Join(*usageDir, "usage.jsonl")
		n, err := udb.ImportJSONL(context.Background(), legacy, nil)
		switch {
		case err != nil:
			log.Warn("usage.jsonl import stopped", "err", err, "imported", n)
		case n > 0:
			log.Info("usage.jsonl imported", "rows", n)
		}
	}()

	if *panelPassword != "" {
		if err := st.SetPanelPassword(*panelPassword); err != nil {
			log.Error("cannot set the panel password", "err", err)
		}
	}
	sessions, err := panelauth.OpenStore(*accountsDir)
	if err != nil {
		log.Error("cannot open the panel session store", "err", err)
		os.Exit(1)
	}
	sessions.StartFlusher(15 * time.Second)
	defer sessions.Close()
	auth := &panelauth.Auth{
		Store: sessions, Pass: st, Proxies: panelauth.TrustedProxiesFromEnv(),
		RequireChange: os.Getenv("WB_ALLOW_DEFAULT_PASSWORD") != "1",
	}

	mux := http.NewServeMux()
	auth.Register(mux, "/api/v2/session")
	mux.Handle("/v1/chat/completions", h)
	(&api.Usage{DB: udb}).Register(mux, auth.Require)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "accounts": p.Len()})
	})
	if *debug {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/heap", pprof.Handler("heap").ServeHTTP)
		mux.HandleFunc("/debug/runtime", func(w http.ResponseWriter, r *http.Request) {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"goroutines": runtime.NumGoroutine(), "heap_alloc": m.HeapAlloc, "sys": m.Sys,
				"num_gc": m.NumGC, "pause_total_ns": m.PauseTotalNs,
			})
		})
	}

	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	srv := &http.Server{
		Addr: addr, Handler: mux,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	bg, stopBG := context.WithCancel(context.Background())
	defer stopBG()
	go refresher.Run(bg)
	go func() {
		log.Info("listening", "addr", addr, "accounts", p.Len(), "dir", *accountsDir, "leak_filter", map[leak.Mode]string{leak.Precise: "precise", leak.Legacy: "legacy"}[mode])
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "listen failed:", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
