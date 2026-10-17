package operator_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
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

// nowhere is the seat controls a case that is not about them is built with,
// since [operator.New] requires them: a fleet holding no parked run and no
// paused seat, and one no node answers a note on. Nothing here is a fault a
// call could mistake for one of the case's own.
type nowhere struct{}

// errNowhere is every write the empty fleet is asked to make.
var errNowhere = errors.New("this fleet records nothing: the case is not about the seat controls")

func (nowhere) Run(context.Context, string) (sandbox.PendingRun, bool, error) {
	return sandbox.PendingRun{}, false, nil
}

func (nowhere) Deliver(context.Context, types.SandboxAnswerGiven) (uuid.UUID, error) {
	return uuid.Nil, errNowhere
}

func (nowhere) SeatPause(context.Context, uuid.UUID) (coord.SeatPause, bool, error) {
	return coord.SeatPause{}, false, nil
}

func (nowhere) CreateSeatPause(context.Context, coord.SeatPause) (coord.SeatPause, bool, error) {
	return coord.SeatPause{}, false, errNowhere
}

func (nowhere) UpdateSeatPause(context.Context, coord.SeatPause) (coord.SeatPause, bool, error) {
	return coord.SeatPause{}, false, errNowhere
}

func (nowhere) DeleteSeatPause(context.Context, uuid.UUID, uint64) (bool, error) {
	return false, errNowhere
}

func (nowhere) Ask(context.Context, string, []byte, int) ([][]byte, error) { return nil, nil }

// leadsNobody is a company chart in which nobody leads anybody, written down
// rather than read off [authz.NoChart], which answers no relation at all. A
// case that asks [operator.Server.Acts] what a person may act on decides it on
// this: every catalogue holds the seat controls, whose rows offer a verb on a
// lead relation, and a chart that cannot answer one makes the whole list an
// error rather than the grant-only answer the case is about.
type leadsNobody struct{}

func (leadsNobody) Leads(context.Context, string, string) (bool, error) { return false, nil }

func (leadsNobody) LeadsProject(context.Context, string, string) (bool, error) {
	return false, nil
}

func (leadsNobody) LeadsUnit(context.Context, string, string) (bool, error) { return false, nil }

func (leadsNobody) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, nil
}

func (leadsNobody) LeadsAnyone(context.Context, string) (bool, error) { return false, nil }

func (leadsNobody) MemberOf(context.Context, string, string) (bool, error) { return false, nil }

// newSurface builds a surface, filling an unset audit with a log nobody reads,
// an unset decision with the authority table over no chart — what every
// grant-only case is decided by — and unset seat controls with [nowhere], and
// fails the test on a refusal.
func newSurface(t *testing.T, opts operator.Options) *operator.Server {
	t.Helper()
	if opts.Audit == nil {
		opts.Audit = &auditLog{}
	}
	if opts.Authorize == nil {
		opts.Authorize = builtin.Decide(authz.NoChart{})
	}
	if opts.Runs.Desk == nil {
		opts.Runs.Desk = nowhere{}
	}
	if opts.Pauses.Pauses == nil {
		opts.Pauses.Pauses = nowhere{}
	}
	if opts.Steer.Asker == nil {
		opts.Steer.Asker = nowhere{}
	}
	s, err := operator.New(opts)
	if err != nil {
		t.Fatalf("operator.New: %v", err)
	}
	return s
}
