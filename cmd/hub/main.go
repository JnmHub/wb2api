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

	"github.com/jackc/pgx/v5/pgxpool"

	"wb2api/internal/activity"
	"wb2api/internal/agents"
	"wb2api/internal/api"
	"wb2api/internal/atrest"
	"wb2api/internal/catalog"
	"wb2api/internal/events"
	"wb2api/internal/gateway/leak"
	"wb2api/internal/gateway/openai"
	"wb2api/internal/guards"
	"wb2api/internal/jobs"
	"wb2api/internal/leader"
	"wb2api/internal/logbuf"
	"wb2api/internal/ops"
	"wb2api/internal/panelauth"
	"wb2api/internal/platform/pg"
	"wb2api/internal/platform/redisx"
	"wb2api/internal/pool"
	"wb2api/internal/pricing"
	"wb2api/internal/quota"
	"wb2api/internal/realmswitch"
	"wb2api/internal/refresh"
	"wb2api/internal/sched"
	"wb2api/internal/scheduler"
	"wb2api/internal/store/accounts"
	"wb2api/internal/store/pgaccounts"
	"wb2api/internal/store/pgjobs"
	"wb2api/internal/store/pgpricing"
	"wb2api/internal/store/pgquota"
	"wb2api/internal/store/pgrealm"
	"wb2api/internal/store/pgsessions"
	"wb2api/internal/store/pgsettings"
	"wb2api/internal/store/settings"
	"wb2api/internal/updates"
	"wb2api/internal/upstream"
	"wb2api/internal/usagedb"
	"wb2api/internal/usageest"
	"wb2api/internal/webui"
)

// version is what the update checker compares against; release builds inject
// the tag with -X main.version=...
var version = "2.0.0"

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// openPanelSessions keeps panel logins shared across replicas when PostgreSQL
// is configured; standalone mode continues to use panel_sessions.json.
func openPanelSessions(db *pg.Pool, dir string) (*panelauth.Store, error) {
	if db == nil {
		return panelauth.OpenStore(dir)
	}
	b := pgsessions.New(db)
	if _, err := b.MigrateFile(dir); err != nil {
		return nil, err
	}
	return panelauth.OpenShared(b)
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloat(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
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
	stickyTTFT := flag.Int("sticky-escape-ttft-ms", envInt("WB_STICKY_ESCAPE_TTFT_MS", 15000), "escape a sticky account when its TTFT EWMA exceeds this (ms)")
	stickyErr := flag.Float64("sticky-escape-error-rate", envFloat("WB_STICKY_ESCAPE_ERROR_RATE", 0.5), "escape a sticky account when its error EWMA exceeds this")
	topK := flag.Int("sched-top-k", envInt("WB_SCHED_TOP_K", 1), "choose among the K least-loaded ready accounts (1 = strict least-loaded)")
	usageDir := flag.String("usage-dir", envOr("WB_PROXY_USAGE_DIR", "usage"), "directory for usage.db (and the legacy usage.jsonl to import)")
	usageQueue := flag.Int("usage-queue", envInt("WB_USAGE_QUEUE", 0), "usage write-queue capacity (0 = 8192)")
	usageWorkers := flag.Int("usage-workers", envInt("WB_USAGE_WORKERS", 0), "parallel usage writers (0 = 1 SQLite / 4 PostgreSQL)")
	usageOverflow := flag.String("usage-overflow", envOr("WB_USAGE_OVERFLOW", "drop"), "usage overflow policy: drop or sample")
	usageSample := flag.Int("usage-sample", envInt("WB_USAGE_SAMPLE", 20), "keep 1/N events under pressure when -usage-overflow=sample")
	panelPassword := flag.String("panel-password", os.Getenv("WB_PANEL_PASSWORD"), "set the dashboard password at start-up")
	catalogDir := flag.String("catalog-dir", envOr("WB_PROXY_CATALOG_DIR", "catalog"), "archive directory for the last /v3/config responses")
	postgresDSN := flag.String("postgres-dsn", os.Getenv("WB_POSTGRES_DSN"), "PostgreSQL DSN (empty keeps the file/SQLite stores)")
	redisAddr := flag.String("redis-addr", os.Getenv("WB_REDIS_ADDR"), "Redis address for shared runtime state (empty disables Redis)")
	redisPassword := flag.String("redis-password", os.Getenv("WB_REDIS_PASSWORD"), "Redis password")
	redisDB := flag.Int("redis-db", func() int { n, _ := strconv.Atoi(envOr("WB_REDIS_DB", "0")); return n }(), "Redis database index")
	redisInflight := flag.Int("redis-inflight", func() int { n, _ := strconv.Atoi(envOr("WB_REDIS_INFLIGHT", "0")); return n }(), "per-account in-flight limit shared through Redis (0 = off)")
	redisWaitMS := flag.Int("redis-wait-ms", func() int { n, _ := strconv.Atoi(envOr("WB_REDIS_WAIT_MS", "0")); return n }(), "queue this long for a full account before failing over (ms, 0 = off)")
	firstOutputMS := flag.Int("first-output-ms", func() int { n, _ := strconv.Atoi(envOr("WB_FIRST_OUTPUT_MS", "0")); return n }(), "fail over when a streaming account sends no semantic output within this time (ms, 0 = off)")
	noModelsDev := flag.Bool("no-modelsdev", os.Getenv("WB_NO_MODELSDEV") == "1", "never contact models.dev for context/output limits")
	debug := flag.Bool("debug", os.Getenv("WB_DEBUG") == "1", "expose /debug/runtime")
	maxBodyMB := flag.Int("max-body-mb", func() int {
		n, _ := strconv.Atoi(envOr("WB_MAX_BODY_MB", "16"))
		if n < 1 {
			return 16
		}
		return n
	}(), "per-request body ceiling in MiB (default 16)")
	flag.Parse()
	if *maxBodyMB > 1024 {
		*maxBodyMB = 1024
	}

	if *lan && *host == "127.0.0.1" {
		*host = "0.0.0.0"
	}
	logRing := logbuf.New(2000)
	log := slog.New(logbuf.NewHandler(logRing, slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.SetDefault(log)

	startupCtx, startupCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer startupCancel()
	var pgDB *pg.Pool
	if *postgresDSN != "" {
		var err error
		pgDB, err = pg.Open(startupCtx, *postgresDSN)
		if err != nil {
			log.Error("cannot open PostgreSQL", "err", err)
			os.Exit(1)
		}
		defer pgDB.Close()
		log.Info("postgres connected")
	}
	var rdb *redisx.Client
	if *redisAddr != "" {
		var err error
		rdb, err = redisx.Open(startupCtx, *redisAddr, *redisPassword, *redisDB)
		if err != nil {
			log.Error("cannot open Redis", "err", err)
			os.Exit(1)
		}
		defer rdb.Close()
		log.Info("redis connected", "addr", *redisAddr)
	}

	var recs []*accounts.Record
	var errs []accounts.LoadError
	persist := func(rec *accounts.Record) error {
		_, err := accounts.Save(*accountsDir, rec)
		return err
	}
	deleteFn := accounts.Delete
	if pgDB != nil {
		var err error
		recs, errs, err = pgaccounts.Load(startupCtx, pgDB.Pool, *accountsDir)
		if err != nil {
			log.Error("cannot load accounts from PostgreSQL", "err", err)
			os.Exit(1)
		}
		persist = func(rec *accounts.Record) error {
			return pgaccounts.Save(context.Background(), pgDB.Pool, rec)
		}
		deleteFn = func(rec *accounts.Record) error {
			return pgaccounts.Delete(context.Background(), pgDB.Pool, rec)
		}
	} else {
		recs, errs = accounts.Load(*accountsDir, time.Now())
	}
	for _, e := range errs {
		log.Warn("skipping unreadable account", "err", e)
	}
	p := pool.New(recs)
	p.SetTopK(*topK)
	if *perAccount > 0 {
		for _, a := range p.All() {
			a.SetMaxInflight(*perAccount)
		}
	}
	var st *settings.Store
	if pgDB != nil {
		if _, err := pgsettings.Load(startupCtx, pgDB.Pool, *accountsDir); err != nil {
			log.Error("cannot seed settings from PostgreSQL", "err", err)
			os.Exit(1)
		}
		st = settings.OpenWith(
			func() (settings.Doc, error) {
				return pgsettings.Load(context.Background(), pgDB.Pool, "")
			},
			func(doc settings.Doc) error {
				return pgsettings.Save(context.Background(), pgDB.Pool, doc)
			},
		)
	} else {
		st = settings.Open(*accountsDir)
	}
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
	refresher := &refresh.Refresher{Pool: p, Up: up, Log: log, Persist: persist}
	feedback := sched.NewStats(time.Duration(*stickyTTFT)*time.Millisecond, *stickyErr)
	if *upstreamOverride != "" {
		refresher.BaseURL = func(*pool.Account) string { return *upstreamOverride }
	}
	h := &openai.Handler{
		Pool: p, Up: up, Settings: st, Log: log, Refresher: refresher, Persist: persist,
		UpstreamOverride: *upstreamOverride, MaxAttempts: *maxAttempts, FilterMode: mode,
		MaxBody:            int64(*maxBodyMB) << 20,
		AccountInflight:    *redisInflight,
		AccountWait:        time.Duration(*redisWaitMS) * time.Millisecond,
		FirstOutputTimeout: time.Duration(*firstOutputMS) * time.Millisecond,
		Sched:              feedback,
	}
	p.SetScorer(h.AccountScore)
	if rdb != nil {
		lim := redisx.NewLimiter(rdb)
		h.KeyLimiter = lim
		if *redisInflight > 0 {
			h.Limiter = lim
		}
		// Conversation affinity is best-effort cross-replica state: a missed
		// binding only costs a cold prompt cache on another replica.
		p.Affinity().SetStore(redisx.NewAffinityStore(rdb.Client))
	}
	var realmSw *realmswitch.Switch
	if pgDB != nil {
		if _, err := pgrealm.Load(startupCtx, pgDB.Pool, *accountsDir, "intl"); err != nil {
			log.Error("cannot seed the active realm from PostgreSQL", "err", err)
			os.Exit(1)
		}
		realmSw = realmswitch.OpenWith(
			func() (string, error) {
				return pgrealm.Get(context.Background(), pgDB.Pool)
			},
			func(realm string) error {
				return pgrealm.Save(context.Background(), pgDB.Pool, realm)
			},
			"intl",
		)
	} else {
		realmSw = realmswitch.Open(*accountsDir, "intl")
	}
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
	var (
		udb *usagedb.DB
		err error
	)
	usageOpts := usagedb.Options{
		QueueSize: *usageQueue, Workers: *usageWorkers,
		Overflow: usagedb.OverflowPolicy(*usageOverflow), SampleEvery: *usageSample,
	}
	if *postgresDSN != "" {
		udb, err = usagedb.OpenPostgresWithOptions(*postgresDSN, usageOpts)
	} else {
		udb, err = usagedb.OpenWithOptions(filepath.Join(*usageDir, "usage.db"), usageOpts)
	}
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
	var elector *leader.Elector
	if pgDB != nil {
		elector = leader.New(pgDB.Pool, leader.KeyBackground, log)
		go elector.Run(bg)
	}
	isLeader := func() bool { return elector == nil || elector.IsLeader() }
	var windowStore quota.WindowStore
	var scanStore quota.ScanStore
	if pgDB != nil {
		qstore := pgquota.New(pgDB.Pool, *accountsDir)
		windowStore = qstore
		scanStore = qstore
	}
	windows := &quota.Windows{Dir: *accountsDir, Gate: st.WindowProbe, Prober: h, Log: log, Store: windowStore}
	h.Windows = windows
	scanner := &quota.Scanner{Dir: *accountsDir, Models: st.QuotaScanModels, Prober: h, Windows: windows, Log: log, Store: scanStore}
	keyring := &atrest.Keyring{}
	agentMgr := agents.New(*accountsDir)
	h.OnUsage = func(ev openai.UsageEvent) {
		feedback.Observe(ev.Account, ev.TTFT, sched.Failure(ev.Outcome))
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
	// stopped if the process restarts. Replicas serialize the import: the resume
	// cursor alone cannot stop two processes from inserting the same rows.
	var importPool *pgxpool.Pool
	if pgDB != nil {
		importPool = pgDB.Pool
	}
	go func() {
		err := leader.WithLock(context.Background(), importPool, leader.KeyImport, func() error {
			legacy := filepath.Join(*usageDir, "usage.jsonl")
			n, err := udb.ImportJSONL(context.Background(), legacy, nil)
			switch {
			case err != nil:
				log.Warn("usage.jsonl import stopped", "err", err, "imported", n)
			case n > 0:
				log.Info("usage.jsonl imported", "rows", n)
			}
			if *postgresDSN != "" {
				n, err := udb.ImportSQLite(context.Background(), filepath.Join(*usageDir, "usage.db"))
				switch {
				case err != nil:
					log.Warn("usage.db import stopped", "err", err, "imported", n)
				case n > 0:
					log.Info("usage.db imported", "rows", n)
				}
			}
			return nil
		})
		if err != nil {
			log.Warn("legacy usage import lock failed", "err", err)
		}
	}()

	if *panelPassword != "" {
		if err := st.SetPanelPassword(*panelPassword); err != nil {
			log.Error("cannot set the panel password", "err", err)
		}
	}
	sessions, err := openPanelSessions(pgDB, *accountsDir)
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
	infoH := &openai.Info{Chat: h, Pool: p, Catalog: cat}
	infoH.HealthExtra = func() map[string]any {
		out := map[string]any{}
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		if pgDB != nil {
			if err := pgDB.Ping(ctx); err != nil {
				out["postgres"] = "error"
			} else {
				out["postgres"] = "ok"
			}
		}
		if *postgresDSN != "" {
			out["usage"] = "postgres"
		} else {
			out["usage"] = "sqlite"
		}
		if rdb != nil {
			if err := rdb.Ping(ctx); err != nil {
				out["redis"] = "error"
			} else {
				out["redis"] = "ok"
			}
		}
		return out
	}
	infoH.Register(mux)
	mux.HandleFunc("GET /realm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"current": realmSw.Get(), "options": []string{"intl", "cn"}})
	})
	pricingStore := pricing.Open(*accountsDir)
	if pgDB != nil {
		if err := pricingStore.UsePolicySource(pgpricing.New(pgDB.Pool, *accountsDir)); err != nil {
			log.Warn("cannot load shared pricing policies; keeping the local file", "err", err)
		} else {
			pricingStore.StartPolicySync(bg, 5*time.Second)
			log.Info("shared pricing policies enabled")
		}
	}
	go refreshPricingLoop(pricingStore, log)
	(&api.Usage{DB: udb, Est: limits, Pricing: pricingStore}).Register(mux, auth.Require)
	bus := events.New(512)
	var jobStore jobs.Store
	if pgDB != nil {
		jobStore = pgjobs.New(pgDB.Pool)
	}
	jobMgr := &jobs.Manager{Workers: 4, Store: jobStore, OnProgress: func(j jobs.Job) { bus.Publish("job.progress", j) }}
	if jobStore != nil {
		jobMgr.StartSync(bg)
		log.Info("shared job history enabled")
	}
	opsGate := api.NewOpsGate(st.OpsConcurrency(), time.Duration(st.OpsIntervalMS())*time.Millisecond)
	accountsAPI := &api.Accounts{
		Pool: p, Today: today, Store: st, Gate: opsGate, Persist: persist, Refresher: refresher, Jobs: jobMgr, Bus: bus,
		Delete: deleteFn,
	}
	activityLog := &activity.Log{Dir: *usageDir}
	opsClient := &ops.Client{
		Pool: p, Up: up, Refresher: refresher, Settings: st, Activity: activityLog, Log: log,
		Persist: persist, Remove: deleteFn, BaseOverride: *upstreamOverride,
	}
	accountsAPI.Ops, accountsAPI.Prober = opsClient, h
	accountsAPI.Register(mux, auth.Require)
	accountsAPI.StartEvents(bg, time.Second)
	(&api.Scheduler{
		Pool: p, Feedback: feedback, TopK: p.TopK,
		Score: func(a *pool.Account) int64 { return h.AccountScore(a, "") },
		Realm: realmSw.Get, MaxInflight: *redisInflight,
	}).Register(mux, auth.Require)
	sched := &scheduler.Scheduler{
		Pool: p, Ops: opsClient, Refresher: refresher, Settings: st, Log: log, Windows: windows, Scan: scanner,
		Leader: isLeader,
		Hold:   func(realm string) time.Duration { return h.RiskHold(realm) },
	}
	(&api.Operations{Ops: opsClient, Pool: p, Prober: h, Scheduler: sched, Activity: activityLog, Scanner: scanner, AtRest: keyring, Agents: agentMgr}).Register(mux, auth.Require)
	(&api.Settings{Store: st, Pool: p, Realm: realmSw, Guards: h.Guards, Catalog: cat, Ops: opsGate}).Register(mux, auth.Require)
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
