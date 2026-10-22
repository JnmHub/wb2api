package redisx

import (
	"context"
	"errors"
)

// ErrLeaseLost means a shared reservation is no longer safe to use. Callers
// must stop upstream work; continuing on a local gate could exceed the cluster cap.
var ErrLeaseLost = errors.New("shared concurrency lease lost")

type leaseLossKey struct{}

// WithLeaseLoss associates request cancellation with all leases acquired using
// ctx. The callback is request scoped, never stored on a shared Limiter. It must
// only signal cancellation (as context.CancelCauseFunc does), not synchronously
// release a lease: release joins cancellation/renewal work.
func (l *Limiter) WithLeaseLoss(ctx context.Context, cancel context.CancelCauseFunc) context.Context {
	return context.WithValue(ctx, leaseLossKey{}, cancel)
}

func loseLease(ctx context.Context) {
	if cancel, ok := ctx.Value(leaseLossKey{}).(context.CancelCauseFunc); ok && cancel != nil {
		cancel(ErrLeaseLost)
	}
}
