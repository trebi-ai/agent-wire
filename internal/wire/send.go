package wire

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// Delivery tells how Send delivered a prompt.
type Delivery string

const (
	// DeliverySteered means the prompt joined the running turn.
	DeliverySteered Delivery = "steered"
	// DeliveryPrompted means a new turn starts now, or the vendor queues it.
	DeliveryPrompted Delivery = "prompted"
	// DeliveryHeld means the prompt starts a new turn when the running turn ends.
	DeliveryHeld Delivery = "held"
)

// CodeHeldPromptDropped is the error code of held prompts that the session
// dropped, because the process exited before the turn ended.
const CodeHeldPromptDropped = "held_prompt_dropped"

// ErrNoActiveTurn reports a Steer with no running turn.
var ErrNoActiveTurn = errors.New("agentwire: no active turn")

// ErrTurnActive reports a Prompt while a turn runs on a harness that cannot
// take a second prompt then. The caller waits for the result, or steers.
var ErrTurnActive = errors.New("agentwire: a turn is active")

// TurnStater is the optional adapter capability that reports a running turn.
type TurnStater interface {
	TurnActive() bool
}

// SteerAdapter is the optional adapter capability that adds input to the
// running turn.
type SteerAdapter interface {
	Steer(ctx context.Context, p Prompt) error
}

// ConcurrentPrompter is the optional adapter capability of a vendor that
// queues a second prompt during a turn.
type ConcurrentPrompter interface {
	ConcurrentPrompt() bool
}

// SendOps are the session calls that one Send uses.
type SendOps struct {
	Active func() bool
	Prompt func(context.Context, Prompt) error
	// Steer is nil when the session cannot steer.
	Steer func(context.Context, Prompt) error
	// Concurrent means Prompt is safe during a turn.
	Concurrent bool
}

// Outbox holds the prompts that wait for the running turn to end.
type Outbox struct {
	mu       sync.Mutex
	held     []Prompt
	flushing bool
}

// Send steers the running turn, else prompts, else holds p until the turn
// ends.
func (o *Outbox) Send(ctx context.Context, ops SendOps, p Prompt) (Delivery, error) {
	if ops.Active() {
		switch {
		case ops.Steer != nil:
			err := ops.Steer(ctx, p)
			switch {
			case err == nil:
				return DeliverySteered, nil
			case errors.Is(err, ErrUnsupported):
				return o.hold(ctx, ops, p)
			case !errors.Is(err, ErrNoActiveTurn):
				return "", err
			}
		case ops.Concurrent:
			return DeliveryPrompted, ops.Prompt(ctx, p)
		default:
			return o.hold(ctx, ops, p)
		}
	}
	return o.hold(ctx, ops, p)
}

// hold queues p when a turn runs or a flush is in progress. Else it prompts.
func (o *Outbox) hold(ctx context.Context, ops SendOps, p Prompt) (Delivery, error) {
	o.mu.Lock()
	if !o.flushing && !ops.Active() {
		o.mu.Unlock()
		return DeliveryPrompted, ops.Prompt(ctx, p)
	}
	o.held = append(o.held, p)
	o.mu.Unlock()
	return DeliveryHeld, nil
}

// Pending reports whether prompts wait for the turn to end.
func (o *Outbox) Pending() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.held) > 0
}

// Flush sends the held prompts as one followup turn. The caller calls it
// after the turn ended. It returns the prompts it could not send.
func (o *Outbox) Flush(ctx context.Context, prompt func(context.Context, Prompt) error) []Prompt {
	o.mu.Lock()
	held := o.held
	o.held = nil
	o.flushing = len(held) > 0
	o.mu.Unlock()
	if len(held) == 0 {
		return nil
	}
	err := prompt(ctx, joinHeld(held))
	o.mu.Lock()
	o.flushing = false
	o.mu.Unlock()
	if err != nil {
		return held
	}
	return nil
}

// Drop empties the queue and returns what it held.
func (o *Outbox) Drop() []Prompt {
	o.mu.Lock()
	defer o.mu.Unlock()
	held := o.held
	o.held = nil
	return held
}

// joinHeld joins held prompts with a blank line into one followup prompt.
func joinHeld(held []Prompt) Prompt {
	texts := make([]string, 0, len(held))
	out := Prompt{Kind: "followup"}
	for _, p := range held {
		texts = append(texts, p.Text)
		out.Attachments = append(out.Attachments, p.Attachments...)
	}
	out.Text = strings.Join(texts, "\n\n")
	return out
}

// DroppedEvent reports held prompts that never reached the vendor.
func DroppedEvent(held []Prompt) Event {
	texts := make([]string, 0, len(held))
	for _, p := range held {
		texts = append(texts, p.Text)
	}
	return Event{
		Type: EventError, Code: CodeHeldPromptDropped,
		Error: "agentwire: the session ended before a held prompt was sent",
		Text:  strings.Join(texts, "\n\n"),
	}
}
