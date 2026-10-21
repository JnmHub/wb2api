# Architecture migration: Sub2API-style PostgreSQL + Redis

This document tracks the migration from the original single-process
files + SQLite deployment to the Sub2API-style split:

- PostgreSQL is the authoritative store for account and settings state.
- Redis carries cross-process runtime state (currently per-account in-flight
  slots; scheduler statistics/concurrency wait queues are the next phase).
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
- Redis limiter now has `AcquireWait`: when an account is full, the request
  keeps its sticky account and polls for a slot for `WB_REDIS_WAIT_MS` (compose
  default 1500 ms) before giving up and failing over. Polling backs off from
  40 ms to 500 ms, respects request cancellation, and uses the same Redis ZSET
  slots/TTL as `Acquire`. This mirrors Sub2API's per-account queue and avoids
  cold-account spills caused by a momentary burst.
- `internal/store/pgaccounts`: account records stored as JSONB, with a one-time
  import from `accounts/*.json` when the table is empty.
- `internal/store/pgsettings`: singleton settings document stored as JSONB, with
  a one-time import from `settings.json`.
- `settings.OpenWith`: file-free settings store used by the PostgreSQL backend.
- `cmd/hub`: `WB_POSTGRES_DSN`, `WB_REDIS_ADDR`, `WB_REDIS_INFLIGHT`, `WB_REDIS_WAIT_MS`; when
  configured, account persistence/import/delete and settings updates go to
  PostgreSQL, and `/health` reports backend status.
- `compose.yaml`: one command starts PostgreSQL, Redis and wb2api, with health
  dependencies.

### Compatibility

- `WB_POSTGRES_DSN` empty: exactly the previous file + SQLite behavior.
- `WB_REDIS_ADDR` empty: no Redis limiter; local `-per-account-inflight`
  still works. Compose sets `WB_REDIS_INFLIGHT=8`; the standalone default is 0.
- Existing account files/settings are imported only when the corresponding
  PostgreSQL table is empty, so an upgrade does not require manual migration.

## Next phases

1. **Usage DB**: replace SQLite `usage.db` with PostgreSQL (or a repository
   interface with both backends); keep the async batch writer and the current
   query API.
2. **Scheduler**: port the remaining Sub2API loop:
   - top-K weighted selection using load, queue, TTFT and error;
   - a short account wait queue instead of immediate spill/reject;
   - first-output timeout/failover before semantic output.
3. **Shared state**: move panel sessions, active realm, quota windows, pricing
   overrides, jobs/leader election and affinity to PostgreSQL/Redis so more than
   one wb2api replica is safe.
4. **Observability**: add backend status and scheduler metrics to the panel.
5. **Load tests**: compare file/SQLite vs PostgreSQL/Redis at 200+ concurrent
   streams, and verify a two-replica deployment does not double-schedule.

## Non-goals in this phase

- The account scheduler itself is still the existing pool scheduler plus the
  TTFT/error sticky escape and warm-account load preference; global top-K load
  scoring and the Redis limiter wait queue are not complete yet.
- SQLite usage data is not migrated automatically yet. The compose stack keeps
  `/data/usage` as a volume so both modes can coexist during the transition.
- Running multiple wb2api processes against the same **file** data directory is
  still unsupported; use the PostgreSQL/Redis mode for that.
