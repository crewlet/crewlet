package node_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// callLog records, in order, the registrations and subscription creations one
// walk made, so a test can assert which came first.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *callLog) add(call string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, call)
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

type recordingRegistry struct {
	log  *callLog
	fail error
}

func (r *recordingRegistry) Register(_ context.Context, handle string) error {
	r.log.add("register " + handle)
	return r.fail
}

type recordingQueue struct {
	*qmem.Queue
	log *callLog
}

func (q *recordingQueue) EnsureSubscription(ctx context.Context, topic, group string) (bool, error) {
	q.log.add("ensure " + topic)
	return q.Queue.EnsureSubscription(ctx, topic, group)
}

func mailboxNode(t *testing.T, registry node.MailboxRegistry, log *callLog) (*node.Node, *recordingQueue) {
	t.Helper()
	q := &recordingQueue{Queue: qmem.New(), log: log}
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("queue.Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	n, err := node.New(node.Config{
		Queue: q, Coord: &coordmem.Backend{}, Mailboxes: registry,
		NodeID: "node-a", Owner: "node-a-1",
		Seats: func() []placement.Seat {
			return []placement.Seat{{Handle: "ceo"}, {Handle: "swe"}}
		},
		Turn: func(context.Context, string, []*events.Event) queue.Result { return queue.Ack() },
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	return n, q
}

// A MAILBOX IS REGISTERED BEFORE IT EXISTS. Once a seat leaves the company the
// fleet's record is the only thing that remembers its subscription, so a
// subscription created first and registered second is one a crash in between
// leaves unretirable for ever.
func TestEveryMailboxIsRegisteredBeforeItIsCreated(t *testing.T) {
	log := &callLog{}
	n, _ := mailboxNode(t, &recordingRegistry{log: log}, log)

	n.EnsureMailboxes(t.Context())

	want := []string{
		"register ceo", "ensure " + topics.AgentInbox("ceo"),
		"register swe", "ensure " + topics.AgentInbox("swe"),
	}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("walk made\n  %v\nwant\n  %v", got, want)
	}
}

// A registration that fails still creates the mailbox. A seat in the company
// losing its mail is strictly worse than a mailbox the sweep registers on its
// next tick.
func TestAFailedRegistrationStillCreatesTheMailbox(t *testing.T) {
	log := &callLog{}
	n, q := mailboxNode(t, &recordingRegistry{log: log, fail: errors.New("coordination store unreachable")}, log)

	n.EnsureMailboxes(t.Context())

	for _, handle := range []string{"ceo", "swe"} {
		made, err := q.Queue.EnsureSubscription(t.Context(), topics.AgentInbox(handle), topics.AgentInboxGroup(handle))
		if err != nil {
			t.Fatalf("probe %s: %v", handle, err)
		}
		if made {
			t.Errorf("seat %s has no mailbox because its registration failed, so its mail is dropped", handle)
		}
	}
}

// A node with no fleet store registers nothing and still creates every
// mailbox, rather than refusing to start.
func TestANodeWithoutARegistryStillCreatesEveryMailbox(t *testing.T) {
	log := &callLog{}
	n, _ := mailboxNode(t, nil, log)

	n.EnsureMailboxes(t.Context())

	want := []string{"ensure " + topics.AgentInbox("ceo"), "ensure " + topics.AgentInbox("swe")}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("walk made %v, want %v", got, want)
	}
}
