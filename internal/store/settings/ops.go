package settings

import "fmt"

// Batch account operations (credit refresh, check-in, ...) all talk to the
// vendor from the same egress IP. These two knobs let the operator trade batch
// speed for a gentler request rate without restarting the gateway.
const (
	defaultOpsConcurrency = 4
	maxOpsConcurrency     = 16
	maxOpsIntervalMS      = 60000
)

// OpsConcurrency is how many vendor calls may run at once (1..16).
func (s *Store) OpsConcurrency() int {
	return s.View().intOr("ops_concurrency", defaultOpsConcurrency, 1, maxOpsConcurrency)
}

// OpsIntervalMS is a pause after each batch item (0..60000 ms).
func (s *Store) OpsIntervalMS() int {
	return s.View().intOr("ops_interval_ms", 0, 0, maxOpsIntervalMS)
}

// SetOpsConcurrency stores the batch concurrency.
func (s *Store) SetOpsConcurrency(n int) error {
	if n < 1 || n > maxOpsConcurrency {
		return fmt.Errorf("ops concurrency must be between 1 and %d", maxOpsConcurrency)
	}
	return s.Update(func(d Doc) error { d["ops_concurrency"] = n; return nil })
}

// SetOpsIntervalMS stores the pause between batch items.
func (s *Store) SetOpsIntervalMS(ms int) error {
	if ms < 0 || ms > maxOpsIntervalMS {
		return fmt.Errorf("ops interval must be between 0 and %d ms", maxOpsIntervalMS)
	}
	return s.Update(func(d Doc) error { d["ops_interval_ms"] = ms; return nil })
}
