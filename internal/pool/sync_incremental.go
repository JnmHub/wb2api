package pool

import (
	"bytes"
	"encoding/json"
	"reflect"
	"time"

	"wb2api/internal/store/accounts"
)

// ApplyChanges applies final shared-store states, not a full snapshot. Unlisted
// accounts are untouched. The caller must serialize cursor advancement and
// local Save/LoadOne publication so an older poll cannot overwrite a newer save.
// With no membership changes this is O(changed accounts), not O(pool size), and
// an empty/unchanged result does not invalidate dispatch preferences.
// If a UID appears in both lists the upsert wins (the final state is live).
func (p *Pool) ApplyChanges(recs []*accounts.Record, deleted []string) {
	if len(recs) == 0 && len(deleted) == 0 {
		return
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.applyChangesLocked(recs, deleted)
}

// SyncSnapshot applies an authoritative full snapshot while retaining runtime
// state. Unlike incremental ApplyChanges it removes unlisted accounts. Startup
// and explicit cursor reset use this; ordinary polls should use ApplyChanges.
func (p *Pool) SyncSnapshot(recs []*accounts.Record) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	seen := make(map[string]bool, len(recs))
	for _, rec := range recs {
		if rec != nil {
			seen[rec.UID] = true
		}
	}
	var deleted []string
	if old := p.snap.Load(); old != nil {
		for uid := range old.byUID {
			if !seen[uid] {
				deleted = append(deleted, uid)
			}
		}
	}
	p.applyChangesLocked(recs, deleted)
}

func (p *Pool) applyChangesLocked(recs []*accounts.Record, deleted []string) {
	old := p.snap.Load()
	if old == nil {
		old = &snapshot{byUID: map[string]*Account{}}
	}
	// Only materialize an O(N) membership snapshot when actual membership (or
	// immutable Realm) changes. Existing pointers get O(batch) record updates.
	replacements := make(map[string]*Account)
	removed := make(map[string]bool, len(deleted))
	for _, uid := range deleted {
		if old.byUID[uid] != nil {
			removed[uid] = true
		}
	}
	for _, rec := range recs {
		if rec == nil || rec.UID == "" {
			continue
		}
		delete(removed, rec.UID)
		a := old.byUID[rec.UID]
		if a == nil || a.Realm != rec.Realm {
			if a != nil {
				a.enabled.Store(false)
			}
			replacements[rec.UID] = NewAccount(rec)
			continue
		}
		a.applySharedRecord(rec)
	}
	if len(replacements) == 0 && len(removed) == 0 {
		return
	}
	for uid := range removed {
		old.byUID[uid].enabled.Store(false)
	}
	list := make([]*Account, 0, len(old.all)+len(replacements))
	for _, a := range old.all {
		if removed[a.UID] {
			continue
		}
		if replacement := replacements[a.UID]; replacement != nil {
			list = append(list, replacement)
			delete(replacements, a.UID)
		} else {
			list = append(list, a)
		}
	}
	for _, a := range replacements {
		list = append(list, a)
	}
	p.replaceLocked(list)
}

func (a *Account) applySharedRecord(rec *accounts.Record) {
	a.updMu.Lock()
	defer a.updMu.Unlock()
	prev := a.Record()
	if sameSharedRecord(prev, rec) {
		return
	}
	// Preserve the locally consumed estimate unless persisted credits really
	// changed (including credits.updated_at). Never mutate an incoming record.
	if sameSharedCredits(prev.SharedBaseline, rec.SharedBaseline) {
		next := *rec
		next.Credits = prev.Credits
		rec = &next
	}
	a.rec.Store(rec)
	a.enabled.Store(rec.IsEnabled())
	a.expiresAt.Store(rec.ExpiresAt)
	a.weight.Store(int32(rec.Weight))
	if prev.AccessToken != rec.AccessToken {
		a.tokenBad.Store(rec.AccessToken == "")
	}
	if prev.RestrictedUntil != rec.RestrictedUntil {
		a.restrictedTo.Store(int64(rec.RestrictedUntil * 1e9))
	}
	if !reflect.DeepEqual(prev.DisabledModels, rec.DisabledModels) {
		dm := make(map[string]bool, len(rec.DisabledModels))
		for _, model := range rec.DisabledModels {
			dm[model] = true
		}
		a.mu.Lock()
		a.disabledMd = dm
		a.mu.Unlock()
	}
	if !reflect.DeepEqual(prev.ModelCaps, rec.ModelCaps) {
		a.restoreCaps(rec, time.Now())
	}
	if p := a.owner.Load(); p != nil {
		p.prefDirty.Store(true)
	}
	a.touch()
}

func sameSharedRecord(prev, next *accounts.Record) bool {
	if prev.SharedBaseline == nil || next.SharedBaseline == nil {
		return reflect.DeepEqual(prev, next)
	}
	// A local management Update can carry an old baseline with modified fields
	// (not just locally consumed credits). An equal baseline alone must not
	// suppress authoritative rollback after a failed/conflicting Save.
	aRecord, bRecord := *prev, *next
	aRecord.Credits, bRecord.Credits = nil, nil
	aRecord.SharedBaseline, bRecord.SharedBaseline = nil, nil
	if !reflect.DeepEqual(aRecord, bRecord) {
		return false
	}
	if bytes.Equal(prev.SharedBaseline, next.SharedBaseline) {
		return true
	}
	// Baselines produced by decode are canonical, but accept equivalent JSON
	// from external callers as well (including PostgreSQL jsonb key ordering).
	parse := func(raw []byte) (map[string]any, error) {
		var doc map[string]any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		err := d.Decode(&doc)
		return doc, err
	}
	a, errA := parse(prev.SharedBaseline)
	b, errB := parse(next.SharedBaseline)
	return errA == nil && errB == nil && a != nil && b != nil && reflect.DeepEqual(a, b)
}
