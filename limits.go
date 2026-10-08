package agentwire

import (
	"context"
	"fmt"
	"time"
)

// limitsProbeTimeout bounds a limits probe without a live session.
const limitsProbeTimeout = 15 * time.Second

// LimitsReader is the optional driver capability that reads the plan limits
// of the account without a live session.
type LimitsReader interface {
	ReadLimits(ctx context.Context, q ModelQuery) (Limits, error)
}

// Limits reads the plan limits of one harness account. It starts a
// short-lived child and never caches. A harness with no reader returns
// ErrUnsupported.
func (rt *Runtime) Limits(ctx context.Context, h Harness, q ModelQuery) (Limits, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	d, ok := rt.Driver(h)
	if !ok {
		return Limits{}, fmt.Errorf("agentwire: no driver for harness %q", h)
	}
	reader, ok := d.(LimitsReader)
	if !ok {
		return Limits{}, fmt.Errorf("%w: %s reads no limits", ErrUnsupported, h)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limitsProbeTimeout)
		defer cancel()
	}
	return reader.ReadLimits(ctx, q)
}
