package openai

import "context"

// Lease-aware shared gates cancel the request if their reservation can no
// longer be confirmed. Keeping this optional preserves custom local limiters.
type leaseLossAware interface {
	WithLeaseLoss(context.Context, context.CancelCauseFunc) context.Context
}
