package pool

import (
	"fmt"

	"wb2api/internal/store/accounts"
)

// SameRaccoonCredential reports whether an upstream result may still be
// applied to the current credential. Pool membership is checked separately.
func SameRaccoonCredential(current, observed *accounts.Record) bool {
	return current.MatchesRaccoonCredential(observed)
}

// ReplaceRaccoonRecord publishes a freshly loaded shared record only while
// the account still holds the snapshot that prompted the read.
func (a *Account) ReplaceRaccoonRecord(p *Pool, observed, latest *accounts.Record) bool {
	a.updMu.Lock()
	defer a.updMu.Unlock()
	if a.Record() != observed || (p != nil && p.Get(a.UID) != a) {
		return false
	}
	a.SetRecord(latest)
	return true
}

// SaveRaccoonUpdate changes only the operation's fields on the latest record.
// Reconciled stores publish their committed result and handle any concurrent
// write or deletion during persistence. Memory-only callers use the same
// generation check under the account update lock before publishing.
func SaveRaccoonUpdate(p *Pool, a *Account, observed *accounts.Record, persist func(*accounts.Record) error,
	apply func(*accounts.Record) *accounts.Record) error {
	var current *accounts.Record
	save := func() error {
		if p != nil && p.Get(a.UID) != a {
			return accounts.ErrDeleted
		}
		current = a.Record()
		if !SameRaccoonCredential(current, observed) {
			return fmt.Errorf("%w: raccoon credential replaced", accounts.ErrConflict)
		}
		return nil
	}
	if err := save(); err != nil {
		return err
	}
	if persist != nil {
		next := *apply(current)
		next.ObservedRaccoonCredential = observed
		if err := persist(&next); err != nil {
			return err
		}
	}
	a.updMu.Lock()
	defer a.updMu.Unlock()
	// The durable store may already have published a reconciled result.
	if persist != nil && a.Record() != current {
		return nil
	}
	if err := save(); err != nil {
		return err
	}
	a.SetRecord(apply(current))
	return nil
}
