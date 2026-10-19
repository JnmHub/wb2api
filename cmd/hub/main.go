// Command hub is the WorkBuddy2API gateway (Go rewrite).
//
// STATUS: it serves /v1/chat/completions, /v1/responses, /v1/models,
// /v1/balance-summary and /health through the real account pool, per-account transports and leak
// filter, plus the new dashboard; see docs/PROGRESS.md for what of the legacy
// surface is still missing.
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

	"wb2api/internal/activity"
	"wb2api/internal/api"
	"wb2api/internal/atrest"
	"wb2api/internal/catalog"
	"wb2api/internal/events"
	"wb2api/internal/gateway/leak"
	"wb2api/internal/gateway/openai"
	"wb2api/internal/guards"
	"wb2api/internal/jobs"
	"wb2api/internal/logbuf"
	"wb2api/internal/ops"
	"wb2api/internal/panelauth"
	"wb2api/internal/pool"
	"wb2api/internal/pricing"
	"wb2api/internal/quota"
	"wb2api/internal/realmswitch"
	"wb2api/internal/refresh"
	"wb2api/internal/scheduler"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/settings"
	"wb2api/internal/updates"
	"wb2api/internal/upstream"
	"wb2api/internal/usagedb"
	"wb2api/internal/usageest"
	"wb2api/internal/webui"
)

// version is what the update checker compares against.
const version = "2.0.0"

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
	catalogDir := flag.String("catalog-dir", envOr("WB_PROXY_CATALOG_DIR", "catalog"), "archive directory for the last /v3/config responses")
	noModelsDev := flag.Bool("no-modelsdev", os.Getenv("WB_NO_MODELSDEV") == "1", "never contact models.dev for context/output limits")
	debug := flag.Bool("debug", os.Getenv("WB_DEBUG") == "1", "expose /debug/runtime")
	flag.Parse()

	if *lan && *host == "127.0.0.1" {
		*host = "0.0.0.0"
	}
	logRing := logbuf.New(2000)
	log := slog.New(logbuf.NewHandler(logRing, slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.SetDefault(log)

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
	applyProxySlots := func() {
		var slots []pool.ProxySlot
		for _, sl := range st.ProxySlots() {
			slots = append(slots, pool.ProxySlot{ID: sl.ID, URL: sl.URL, Enabled: sl.Enabled})
		}
		p.ApplyProxySlots(slots)
	}
	applyProxySlots()

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
	realmSw := realmswitch.Open(*accountsDir, "intl")
	h.CurrentRealm = realmSw.Get
	cat := &catalog.Service{
		Pool: p, Up: up, Dir: *accountsDir, CacheDir: *catalogDir, Log: log,
		UpstreamOverride: *upstreamOverride, ModelsDev: !*noModelsDev,
	}
	h.Catalog = cat
	if err := os.MkdirAll(*usageDir, 0o755); err != nil {
		log.Error("cannot create the usage directory", "dir", *usageDir, "err", err)
		os.Exit(1)
	}
	bg, stopBG := context.WithCancel(context.Background())
	defer stopBG()
	udb, err := usagedb.Open(filepath.Join(*usageDir, "usage.db"), 0)
	if err != nil {
		log.Error("cannot open the usage database", "err", err)
		os.Exit(1)
	}
	defer udb.Close()
	limits := &usageest.Estimator{DB: udb, Pool: p, Dir: *usageDir, Log: log}
	h.Limits = limits
	realmOf := api.RealmOf(p)
	trace := os.Getenv("WB_TRACE") == "1"
	today := &api.Today{}
	if err := today.Seed(udb); err != nil {
		log.Warn("could not seed today's counters", "err", err)
	}
	h.Guards = &guards.Scanner{Pool: p, Settings: st, Usage: udb, FreeModels: cat.FreeModels, Log: log}
	windows := &quota.Windows{Dir: *accountsDir, Gate: st.WindowProbe, Prober: h, Log: log}
	h.Windows = windows
	scanner := &quota.Scanner{Dir: *accountsDir, Models: st.QuotaScanModels, Prober: h, Windows: windows, Log: log}
	keyring := &atrest.Keyring{}
	h.OnUsage = func(ev openai.UsageEvent) {
		ue := api.EventFromGateway(ev, realmOf)
		udb.Record(ue)
		today.Add(ue)
		if ue.Credit > 0 && ev.Account != "" {
			if a := p.Get(ev.Account); a != nil {
				a.ConsumeCredit(ue.Credit)
			}
		}
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
	responsesH := &openai.Responses{Chat: h}
	mux.Handle("/v1/responses", responsesH)
	mux.Handle("/responses", responsesH)
	messagesH := &openai.Messages{Chat: h}
	for _, p := range []string{"/v1/messages", "/messages", "/v1/messages/count_tokens", "/messages/count_tokens"} {
		mux.Handle(p, messagesH)
	}
	(&openai.Info{Chat: h, Pool: p, Catalog: cat}).Register(mux)
	mux.HandleFunc("GET /realm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"current": realmSw.Get(), "options": []string{"intl", "cn"}})
	})
	pricingStore := pricing.Open(*accountsDir)
	go refreshPricingLoop(pricingStore, log)
	(&api.Usage{DB: udb, Est: limits, Pricing: pricingStore}).Register(mux, auth.Require)
	bus := events.New(512)
	jobMgr := &jobs.Manager{Workers: 4, OnProgress: func(j jobs.Job) { bus.Publish("job.progress", j) }}
	accountsAPI := &api.Accounts{
		Pool: p, Today: today, Persist: persist, Refresher: refresher, Jobs: jobMgr, Bus: bus,
		Delete: accounts.Delete,
	}
	activityLog := &activity.Log{Dir: *usageDir}
	opsClient := &ops.Client{
		Pool: p, Up: up, Refresher: refresher, Settings: st, Activity: activityLog, Log: log,
		Persist: persist, BaseOverride: *upstreamOverride,
	}
	accountsAPI.Ops, accountsAPI.Prober = opsClient, h
	accountsAPI.Register(mux, auth.Require)
	accountsAPI.StartEvents(bg, time.Second)
	sched := &scheduler.Scheduler{
		Pool: p, Ops: opsClient, Refresher: refresher, Settings: st, Log: log, Windows: windows, Scan: scanner,
		Hold: func(realm string) time.Duration { return h.RiskHold(realm) },
	}
	(&api.Operations{Ops: opsClient, Pool: p, Prober: h, Scheduler: sched, Activity: activityLog, Scanner: scanner, AtRest: keyring}).Register(mux, auth.Require)
	(&api.Settings{Store: st, Pool: p, Realm: realmSw, Guards: h.Guards, Catalog: cat}).Register(mux, auth.Require)
	checker := &updates.Checker{Current: version, Enabled: st.UpdateCheckEnabled, Store: st, Log: log}
	(&api.Updates{Checker: checker}).Register(mux, auth.Require)
	go func() {
		select {
		case <-time.After(30 * time.Second):
		case <-bg.Done():
			return
		}
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			checker.Check(bg, false)
			select {
			case <-t.C:
			case <-bg.Done():
				return
			}
		}
	}()
	(&api.Logs{Ring: logRing}).Register(mux, auth.Require)
	ui, err := webui.New(webui.Dist())
	if err != nil {
		log.Error("cannot load the embedded dashboard", "err", err)
		os.Exit(1)
	}
	mux.Handle("/", ui) // everything not claimed above: the dashboard SPA
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
		Addr: addr, Handler: openai.PinnedRoutes(p, mux),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go refresher.Run(bg)
	go sched.Run(bg)
	go sched.RunCredits(bg)
	go cat.RunDaily(bg)
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

// refreshPricingLoop keeps the OpenRouter pricing snapshot warm without doing
// network work on the request path.
func refreshPricingLoop(s *pricing.Store, log *slog.Logger) {
	refresh := func() {
		if stale, _ := s.Status()["stale"].(bool); !stale {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		n, err := s.Refresh(ctx)
		if err != nil {
			log.Warn("pricing refresh failed", "err", err)
			return
		}
		log.Info("pricing catalogue refreshed", "models", n)
	}
	refresh()
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		refresh()
	}
}
