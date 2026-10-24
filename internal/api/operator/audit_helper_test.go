package operator_test

import (
	"context"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events"
)

// auditLog records every audit record a surface publishes, with the topic it
// was published on.
type auditLog struct {
	mu     sync.Mutex
	topics []string
	events []*events.Event
	err    error
}

func (a *auditLog) Publish(ctx context.Context, topic string, ev *events.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ctx.Err() != nil {
		// A cancelled context is what a publish made on the caller's
		// own request context would see once they hung up, which is the
		// record [operator.Audit] exists to still write.
		return ctx.Err()
	}
	if a.err != nil {
		return a.err
	}
	a.topics = append(a.topics, topic)
	a.events = append(a.events, ev)
	return nil
}

func (a *auditLog) published() []*events.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*events.Event(nil), a.events...)
}

// fixed is [operator.Options.Halves] for a node whose company is up and serves
// exactly these halves.
func fixed(h operator.Halves) func() (operator.Halves, bool) {
	return func() (operator.Halves, bool) { return h, true }
}

// newSurface builds a surface, filling an unset audit with a log nobody reads
// and an unset decision with the authority table over no chart — what every
// grant-only case is decided by — and fails the test on a refusal.
func newSurface(t *testing.T, opts operator.Options) *operator.Server {
	t.Helper()
	if opts.Audit == nil {
		opts.Audit = &auditLog{}
	}
	if opts.Authorize == nil {
		opts.Authorize = builtin.Decide(authz.NoChart{})
	}
	s, err := operator.New(opts)
	if err != nil {
		t.Fatalf("operator.New: %v", err)
	}
	return s
}
