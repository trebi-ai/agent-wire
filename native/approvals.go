package native

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/trebi-ai/agent-wire"
)

// ApprovalRequest is one tool call awaiting a decision.
type ApprovalRequest struct {
	CallID string
	Spec   ToolSpec
	Input  json.RawMessage
}

// Approver decides one tool call. The loop calls it before every mutating
// call, and before any call when the policy says ask.
type Approver interface {
	Approve(ctx context.Context, r ApprovalRequest) (agentwire.Decision, error)
}

// ApproverFunc adapts a function to Approver.
type ApproverFunc func(ctx context.Context, r ApprovalRequest) (agentwire.Decision, error)

func (f ApproverFunc) Approve(ctx context.Context, r ApprovalRequest) (agentwire.Decision, error) {
	return f(ctx, r)
}

// PolicyApprover is the default Approver. It asks the session's
// PermissionPolicy first; a policy that does not answer emits one
// EventPermission on out and blocks on the returned decision func until the
// consumer answers or the context ends (plan 2026-09-26 I.5).
type PolicyApprover struct {
	Policy agentwire.PermissionPolicy
	// Emit surfaces one permission request to the consumer. It must return
	// the channel the decision arrives on. Nil means the policy alone
	// decides and everything else is denied.
	Emit func(ctx context.Context, r ApprovalRequest) (wait func(ctx context.Context) (agentwire.Decision, error), cancel func())
}

func (a PolicyApprover) Approve(ctx context.Context, r ApprovalRequest) (agentwire.Decision, error) {
	if d, ok := a.Policy.Answer(r.Spec.Annotations.Kind, r.Spec.Name); ok {
		return d, nil
	}
	if a.Emit == nil {
		return agentwire.Decision{Message: "no approver configured"}, nil
	}
	wait, cancel := a.Emit(ctx, r)
	if wait == nil {
		return agentwire.Decision{Message: "approver unavailable"}, nil
	}
	if cancel != nil {
		defer cancel()
	}
	return wait(ctx)
}

// sessionApprovals carries the in-flight permission asks of one session.
type sessionApprovals struct {
	mu   sync.Mutex
	seq  int
	open map[string]chan agentwire.Decision
}

func newSessionApprovals() *sessionApprovals {
	return &sessionApprovals{open: map[string]chan agentwire.Decision{}}
}

// emit publishes one ask and returns the wait handle the loop blocks on.
func (a *sessionApprovals) emit(r ApprovalRequest, emit func(id string, r ApprovalRequest)) (func(context.Context) (agentwire.Decision, error), func()) {
	a.mu.Lock()
	a.seq++
	id := fmt.Sprintf("perm-%d", a.seq)
	ch := make(chan agentwire.Decision, 1)
	a.open[id] = ch
	a.mu.Unlock()

	emit(id, r)
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			a.mu.Lock()
			delete(a.open, id)
			a.mu.Unlock()
		})
	}
	wait := func(ctx context.Context) (agentwire.Decision, error) {
		defer cancel()
		select {
		case d := <-ch:
			return d, nil
		case <-ctx.Done():
			return agentwire.Decision{Message: "turn ended"}, ctx.Err()
		}
	}
	return wait, cancel
}

// decide routes one consumer decision. An unknown id is an error.
func (a *sessionApprovals) decide(id string, d agentwire.Decision) error {
	a.mu.Lock()
	ch, ok := a.open[id]
	delete(a.open, id)
	a.mu.Unlock()
	if !ok {
		return errPermissionGone
	}
	ch <- d
	return nil
}

var errPermissionGone = fmt.Errorf("native: unknown or answered permission")
