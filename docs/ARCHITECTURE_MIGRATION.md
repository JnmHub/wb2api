# Architecture migration: Sub2API-style PostgreSQL + Redis

This document tracks the migration from the original single-process
files + SQLite deployment to the Sub2API-style split:

- PostgreSQL is the authoritative store for account, settings and request usage
  state.
- Redis carries cross-process runtime state (per-account in-flight slots, per-key
  concurrency slots and a short scheduler wait queue).
- The standalone file/SQLite mode remains available when no DSN is set.

## Implemented in this phase

- `internal/platform/pg`: pgx pool, schema migration and health ping.
- `internal/platform/redisx`: Redis client, health ping and an account
  in-flight limiter backed by a sorted set + Lua script.
- `internal/sched`: in-process TTFT/error EWMA feedback; sticky sessions escape
  a warm account when its recent TTFT exceeds `WB_STICKY_ESCAPE_TTFT_MS`
  (default 15s) or its error EWMA exceeds
  `WB_STICKY_ESCAPE_ERROR_RATE` (default 0.5). Client aborts are not counted.
- `Pool.PickForSessionBest`: among the conversation's warm accounts, choose the
  least-loaded one, then the one with the better TTFT/error score; the most
  recent account still wins on a tie. This keeps prefix cache while avoiding a
  single hot account when another warm account is idle.
- `Pool.pick` fallback now chooses the least-loaded ready account, with the
  existing cursor order as the tie-breaker (an idle pool still rotates exactly as
  before). This spreads new sessions across the pool without sacrificing the
  warm-session path.
- Slot acquisition is two-phase: a non-blocking probe visits every ready account
  first, so a free account always wins over waiting on a full sticky account.
  Only when every candidate is full does the request queue on the best-ranked one
  for `WB_REDIS_WAIT_MS`; a timeout returns a retryable 429 instead of sleeping
  once per account. Pinned routes keep their dedicated `AcquireWait` path.
- The scheduler score currently combines the TTFT/error EWMA, in-flight load
  (normalised by the account's max-inflight cap) and the manual weight.
  `Pool.SetScorer` is wired to `Handler.AccountScore`; top-K selection and the
  non-top-K fallback both use the same score, with the original load tie-break
  as the last resort. Queue depth, explicit priority rank, quota headroom, reset
  window and upstream cost are not yet terms in this score.
- Redis limiter now has `AcquireWait`: after the non-blocking probe has found
  every ready account full, the request queues on one ranked account for
  `WB_REDIS_WAIT_MS` (compose default 1500 ms) before returning a retryable 429.
  Polling backs off from 40 ms to 500 ms, respects request cancellation, and
  uses the same Redis ZSET slots/TTL as `Acquire`.
- API-key concurrency uses the same Redis ZSET form (`wb:keyinflight:`) through
  `Limiter.AcquireKeyWait`; a process-local `keyGate` is the fallback when Redis
  is absent. A busy key queues briefly and then returns a retryable 429.
- Streaming requests are staged before response headers are sent:
  `stageFirstOutput` reads until the first semantic delta (text, reasoning, tool
  call, Responses delta or Anthropic content block) with a timeout. A silent or
  failed account is cancelled and retried before any client bytes are written;
  `WB_FIRST_OUTPUT_MS` controls the bound (compose default 30000).
- The shared upstream connection pool keeps up to 32 idle connections per host
  (raised from 4) so a burst does not pay a new TLS handshake per request.
- Request usage leaves the hot path through a bounded asynchronous queue. A
  dispatcher batches up to 256 events and spreads each batch over
  `WB_USAGE_WORKERS` transactions (default 1 for SQLite, 4 for PostgreSQL).
  Saturation drops by default; `WB_USAGE_OVERFLOW=sample` keeps one in
  `WB_USAGE_SAMPLE` once the queue is 75% full. `/api/v2/usage/summary` exposes
  depth/capacity, workers, written/dropped/failed batches and the last error;
  the usage page shows backlog and drop tags.
- `internal/store/pgaccounts`: account records stored as JSONB, with a one-time
  import from `accounts/*.json` when the table is empty.
- `internal/store/pgsettings`: singleton settings document stored as JSONB, with
  a one-time import from `settings.json`.
- `internal/usagedb`: the same async writer and query API now supports both
  SQLite (`Open`) and PostgreSQL (`OpenPostgres`). Schema is created on first
  boot; SQLite `?` placeholders are rewritten to `$1..$n`, aggregates use
  `CASE` instead of SQLite booleans, and bucket expressions use `to_char` on
  PostgreSQL. `cmd/hub` selects PostgreSQL whenever `WB_POSTGRES_DSN` is set.
- `usagedb.ImportSQLite`: when PostgreSQL mode starts and a legacy
  `usage/usage.db` exists, its request rows are copied in the background. The
  importer reads the old file read-only, commits each batch and its highest
  request id in one transaction, and resumes safely after a restart.
- `internal/store/pgsessions`: panel login sessions stored one row per session
  (`wb_panel_sessions`). Every replica reads the same device list and sees a
  revocation immediately; `panel_sessions.json` is imported only when the table
  is empty.
- A PostgreSQL-backed usage round-trip test runs in CI (`postgres` job, service
  container) in addition to the SQLite tests; the same job covers panel
  sessions, active realm, quota windows, shared jobs and leader election.
- `settings.OpenWith`: file-free settings store used by the PostgreSQL backend.
- `cmd/hub`: `WB_POSTGRES_DSN`, `WB_REDIS_ADDR`, `WB_REDIS_INFLIGHT`,
  `WB_REDIS_WAIT_MS`, `WB_SCHED_TOP_K`, `WB_FIRST_OUTPUT_MS`,
  `WB_USAGE_QUEUE`, `WB_USAGE_WORKERS`, `WB_USAGE_OVERFLOW` and
  `WB_USAGE_SAMPLE`; when configured, account persistence/import/delete and
  settings updates go to PostgreSQL, and `/health` reports backend status.
- `compose.yaml`: one command starts PostgreSQL, Redis and wb2api, with health
  dependencies.
- `wb_state`: small shared runtime-state table. The active realm is stored under
  `active_realm`; each replica refreshes it once per second, so a panel switch
  propagates without a restart. Per-model pricing overrides live under
  `pricing_policies`; `pricing_policies.json` seeds the row on first boot and
  replicas poll every five seconds. A panel write wins over a concurrent poll.
- Redis affinity: `wb:aff:<sha256-prefix>` holds a per-conversation sorted set
  (score = expiry, member = account uid). The session key is hashed before it
  reaches Redis. Each replica keeps a five-second local cache; a miss reads the
  shared set, a successful bind is queued so the request path never waits on
  Redis, and a failed-account demote is written synchronously so other replicas
  stop choosing it.
- Quota windows are shared in `wb_state` under `quota_windows`. A 429 recorded
  by any replica is visible to the others within two seconds; a local
  `model_windows.json` seeds the row on first boot, and a failed database write
  falls back to the local file. The probe itself still runs only on the leader.
- Background leader election uses a PostgreSQL session advisory lock
  (`pg_try_advisory_lock`). The scheduler cycle, credit refresh, quota window
  probe and active quota scan run only on the lock holder; the lock disappears
  with its connection, so a crashed replica does not block takeover. Standalone
  mode has no elector and keeps running every background task.
- Panel jobs are shared through `wb_jobs(id, data, updated_at)`. Each job stores
  its owning replica; progress is coalesced by a single background writer so a
  slow database never blocks batch workers. Every replica mirrors other owners'
  snapshots every two seconds, and `GET /api/v2/jobs` refreshes on demand. A
  cancel on a non-owner replica sets `cancel_requested`; the owner observes the
  flag on its next sync and cancels its local context. The file/SQLite mode
  keeps jobs in memory only.
- `GET /api/v2/scheduler/summary`: aggregate in-flight/capacity, average
  TTFT/error rate, Top-K and the twenty highest-scoring accounts. It also
  reports affinity queue depth, dropped writes and store errors. The accounts
  page polls it every ten seconds for its scheduler tiles.

### Compatibility

- `WB_POSTGRES_DSN` empty: exactly the previous file + SQLite behavior.
- `WB_REDIS_ADDR` empty: no Redis limiter and affinity stays process-local; the
  local `-per-account-inflight` still works. Compose sets
  `WB_REDIS_INFLIGHT=8`; the standalone default is 0.
- Existing account files/settings are imported only when the corresponding
  PostgreSQL table is empty, so an upgrade does not require manual migration.
- In PostgreSQL mode the legacy `usage.db` is imported automatically on first
  boot; the old file stays untouched and the marker is stored in the `meta`
  table, so the import does not repeat on later restarts.
- Startup imports hold `leader.KeyImport`, a session advisory lock, so two
  replicas cannot read the same resume offset and insert the same rows twice.
  The second replica waits for the first import and then sees the advanced
  cursor.
- In PostgreSQL mode `panel_sessions.json` is imported when
  `wb_panel_sessions` is empty. Existing browser cookies keep working, so the
  upgrade does not log users out.

## Next phases

1. **Remaining shared state**: the active quota-scan schedule is still
   process-local. It does not affect routing and only the leader runs scans.
   Shared EWMA feedback is also still open.
2. **Load tests**: `compose.ha.yaml` provides the two-replica + Caddy topology,
   but it has not been launched end-to-end. Compare file/SQLite vs
   PostgreSQL/Redis at 200+ concurrent streams and verify the two replicas do
   not double-schedule. This requires a running Docker daemon, which was
   unavailable during development.

## Non-goals in this phase

- Running multiple wb2api processes against the same **file** data directory is
  still unsupported; use the PostgreSQL/Redis mode for that.
- The scheduler uses in-process EWMA feedback. Two replicas can make different
  TTFT/error decisions because that feedback is not yet shared through Redis.
  The active quota-scan schedule also remains process-local; panel jobs, quota
  windows, pricing overrides, active realm and background leadership are shared.
