# Settings persistence and incident triage

## Preserve evidence before repair

Do not run `docker compose down -v`, remove volumes, initialize a replacement
cluster, overwrite `settings.json`, or restore a dump over the running database.
Do not automatically merge legacy JSON into a populated `wb_settings` row:
missing keys may represent intentional deletion, and the database may be newer.
Take a database dump and copy deployment metadata before restarting services.
Store all resulting artifacts privately; they contain credentials and API keys.

The following POSIX-shell commands are examples for the deployment host. Set the
actual Compose file and project used by that deployment, not a new project name.
Do not publish an unredacted Compose config, inspect output, log, or dump.

```sh
umask 077
EVIDENCE="wb2api-evidence-$(date +%Y%m%d-%H%M%S)"
mkdir "$EVIDENCE"
COMPOSE=compose.yaml  # use compose.ha.yaml only if that is the deployed stack

docker compose -f "$COMPOSE" ps -a > "$EVIDENCE/compose-ps.txt"
docker compose -f "$COMPOSE" config > "$EVIDENCE/compose-private.yaml"
docker compose -f "$COMPOSE" logs --no-color --timestamps > "$EVIDENCE/containers.log" 2>&1
for id in $(docker compose -f "$COMPOSE" ps -aq); do
  docker inspect "$id" > "$EVIDENCE/container-$id.json"
done
docker volume ls > "$EVIDENCE/volumes.txt"

docker compose -f "$COMPOSE" exec -T postgres sh -c \
  'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' \
  > "$EVIDENCE/database.dump"
# Check the command exit status and verify the archive before relying on it.
docker compose -f "$COMPOSE" exec -T postgres pg_restore --list \
  < "$EVIDENCE/database.dump" > "$EVIDENCE/database-contents.txt"
```

Also copy the actual bind-mounted data directory and any legacy accounts
folder to new backup paths. Confirm their absolute paths from `docker inspect`;
`./accounts` in the legacy Python stack and `./data/accounts` in the Go stack
are not interchangeable. If the app is still writing, a directory copy is an
evidence copy, not necessarily an application-consistent backup. Retain any
existing database backups and deployment logs unchanged.

## Check identity before attributing deletion

Single-node Compose uses the `pgdata` named volume; HA Compose uses `hapgdata`.
Names are normally prefixed by the Compose project. Changing project, Compose
file, working directory, mount source, DSN host, or database name can expose an
empty or different database without deleting the old one. Record image digests,
Compose project labels, PostgreSQL mounts, database identity, startup logs and
the exact upgrade time including timezone.

A non-secret summary can be obtained with the following read-only SQL. Run it
on the database that the running app actually uses; do not paste the entire
settings JSON into an issue.

```sql
select current_database(), current_user, inet_server_addr(), inet_server_port();
select id, updated_at, jsonb_typeof(data) as doc_type,
       (select count(*) from jsonb_object_keys(data)) as setting_fields,
       case when jsonb_typeof(data->'api_keys') = 'array'
            then jsonb_array_length(data->'api_keys') else null end as key_entries
from wb_settings;
```

`key_entries` counts all stored entries, including disabled/soft-deleted keys;
it is not the panel's live-key count. If the document is not an object, avoid
the `jsonb_object_keys` expression and investigate the malformed row separately.
Compare copies/backups offline or in a separately named recovery database. Do
not replace the current singleton merely because an older file has more keys.

## Writer protocol and migration guarantees

Panel/API mutations in PostgreSQL mode must use `pgsettings.Update`: one
transaction obtains a dedicated advisory lock, reads the current document,
runs the in-memory mutation callback exactly once, writes and commits. A lock
around only the final `Save` cannot protect an earlier read/modify/write from
stale replacement. Whole-document `Save` is an explicit replacement API, not a
safe incremental mutation API. All replicas must run the fixed writer protocol;
a rolling deployment with older writers can still lose changes until old
replicas are stopped. Take backups before upgrading.

Startup imports only when the singleton is absent. A valid existing row is
authoritative even when empty/partial; it is never merged with local settings.
First-time import must not overwrite a concurrent winner, and malformed or
unreadable seed files must fail rather than silently become `{}`. A missing
seed file remains a supported fresh-install case; therefore the operator must
verify the mounted path when migrating an existing installation. Runtime reads
must not recreate a missing settings row as an empty document.

A committed database change is published to that replica's local cache; peers
may see it after the normal refresh interval. The transaction fixes durable
lost updates; it is not a historical-data recovery facility, nor an audit log.
Unknown commit outcomes are returned as errors rather than replaying callbacks
that generate credentials/IDs. Investigate state before retrying key creation.

## Evidence boundary

A reproducible concurrency defect proves a possible loss mechanism, not the
cause of a specific historical incident. Attribute an incident only after
correlating the actual database/mount identity, deployment version, timestamps,
logs and backups. The inspected repository history begins in October 2026;
it cannot substantiate an upgrade reported for April 9, 2026. Confirm the
reported date on the deployment host before selecting a log or backup window.
