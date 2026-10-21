# Architecture migration: Sub2API-style PostgreSQL + Redis

This document tracks the migration from the original single-process
files + SQLite deployment to the Sub2API-style split:

- PostgreSQL is the authoritative store for account, settings and request usage
  state.
- Redis carries cross-process runtime state (per-account in-flight slots and a
  short scheduler wait queue).
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
- `internal/usagedb`: the same async writer and query API now supports both
  SQLite (`Open`) and PostgreSQL (`OpenPostgres`). Schema is created on first
  boot; SQLite `?` placeholders are rewritten to `$1..$n`, aggregates use
  `CASE` instead of SQLite booleans, and bucket expressions use `to_char` on
  PostgreSQL. `cmd/hub` selects PostgreSQL whenever `WB_POSTGRES_DSN` is set.
- A PostgreSQL-backed usage round-trip test runs in CI (`postgres` job, service
  container) in addition to the SQLite tests.
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

1. **Scheduler**: port the remaining Sub2API loop:
   - top-K weighted selection using load, queue, TTFT and error;
   - first-output timeout/failover before semantic output.
2. **Shared state**: move panel sessions, active realm, quota windows, pricing
   overrides, jobs/leader election and affinity to PostgreSQL/Redis so more than
   one wb2api replica is safe.
3. **Observability**: add backend status and scheduler metrics to the panel.
4. **Load tests**: compare file/SQLite vs PostgreSQL/Redis at 200+ concurrent
   streams, and verify a two-replica deployment does not double-schedule.

## Non-goals in this phase

- The account scheduler is still the existing pool scheduler plus the TTFT/error
  sticky escape, warm-account load preference and a short Redis wait queue;
  global top-K load scoring and first-output failover are not complete yet.
- SQLite usage data is migrated to PostgreSQL by the one-time `usage.jsonl`
  importer, but an existing `usage.db` file is not converted automatically: the
  standalone mode continues to use it, while PostgreSQL mode starts fresh (plus
  the legacy JSONL import).
- Running multiple wb2api processes against the same **file** data directory is
  still unsupported; use the PostgreSQL/Redis mode for that.
