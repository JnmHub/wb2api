package pg

// accountSyncSchema is installed under an exclusive table lock in the same
// transaction as the base schema. Existing accounts need no backfill: clients
// start with LoadSnapshot, then consume only changes after its coherent cursor.
//
// A sequence is NOT a safe watermark: transaction N+1 can commit before N. This
// singleton row serializes account writers until commit (including direct SQL
// from older binaries), so observing version N proves all <= N are visible.
// The statement trigger takes the clock before DML takes account row locks;
// the row trigger increments only for actual persisted content changes.
const accountSyncSchema = `
lock table wb_accounts in access exclusive mode;
create table if not exists wb_account_sync_clock (
	id boolean primary key default true check (id),
	version bigint not null default 0 check (version >= 0)
);
insert into wb_account_sync_clock(id, version) values(true, 0) on conflict do nothing;
create table if not exists wb_account_changes (
	uid text primary key,
	version bigint not null,
	deleted boolean not null
);
create index if not exists wb_account_changes_version_idx on wb_account_changes(version);

create or replace function wb_accounts_sync_lock() returns trigger language plpgsql as $$
begin
	perform version from wb_account_sync_clock where id for update;
	return null;
end;
$$;
create or replace function wb_accounts_sync_change() returns trigger language plpgsql as $$
declare v bigint;
begin
	if TG_OP = 'UPDATE' and OLD.uid = NEW.uid and OLD.data = NEW.data then
		return null;
	end if;
	update wb_account_sync_clock set version = version + 1 where id returning version into v;
	if TG_OP = 'DELETE' or (TG_OP = 'UPDATE' and OLD.uid <> NEW.uid) then
		insert into wb_account_changes(uid,version,deleted) values(OLD.uid,v,true)
		on conflict(uid) do update set version=excluded.version, deleted=true;
	end if;
	if TG_OP <> 'DELETE' then
		insert into wb_account_changes(uid,version,deleted) values(NEW.uid,v,false)
		on conflict(uid) do update set version=excluded.version, deleted=false;
	end if;
	return null;
end;
$$;
create or replace function wb_accounts_sync_truncate() returns trigger language plpgsql as $$
declare v bigint;
begin
	update wb_account_sync_clock set version = version + 1 where id returning version into v;
	insert into wb_account_changes(uid,version,deleted) select uid,v,true from wb_accounts
	on conflict(uid) do update set version=excluded.version, deleted=true;
	return null;
end;
$$;
drop trigger if exists wb_accounts_sync_lock on wb_accounts;
create trigger wb_accounts_sync_lock before insert or update or delete on wb_accounts
	for each statement execute function wb_accounts_sync_lock();
drop trigger if exists wb_accounts_sync_change on wb_accounts;
create trigger wb_accounts_sync_change after insert or update or delete on wb_accounts
	for each row execute function wb_accounts_sync_change();
drop trigger if exists wb_accounts_sync_truncate on wb_accounts;
create trigger wb_accounts_sync_truncate before truncate on wb_accounts
	for each statement execute function wb_accounts_sync_truncate();
`
