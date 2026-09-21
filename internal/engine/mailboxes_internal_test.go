package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/maintenance"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
)

// rosterFleet is the fleet store the roster reads the activation pointer from,
// made unreachable on demand. Only the pointer read is broken, because that is
// the only fleet read the roster makes.
type rosterFleet struct {
	*memoryFleet
	down bool
}

// memoryFleet names the embedded twin so the field is not called Fleet, which
// would shadow the Fleet method the coord.Plane contract needs promoted.
type memoryFleet = coordmem.Fleet

func (f *rosterFleet) Target(ctx context.Context) (coord.Activation, bool, error) {
	if f.down {
		return coord.Activation{}, false, coord.ErrUnavailable
	}
	return f.memoryFleet.Target(ctx)
}

// THE ROSTER A MAILBOX IS RETIRED AGAINST IS THE FLEET'S CURRENT REVISION, and
// only that. Every other answer is unknown, because a retirement deletes mail
// that cannot be recovered and a seat judged absent against a stale revision
// may be one the fleet has just added back.
func TestTheMailboxRosterIsKnownOnlyOnANodeServingTheFleetsRevision(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-roster-key"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: Software Engineer
    handle: swe
    llm: zulu
  - name: Founder
    kind: human
    contact:
      slack_user_id: U0FOUNDER
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}

	fleet := &rosterFleet{memoryFleet: coordmem.NewFleet()}
	e := &Engine{backends: &Backends{Fleet: fleet}}
	e.epoch.current.Store(company)

	// Nothing activated: not a fault, and not a roster of nobody either.
	if _, err := e.activeSeats(t.Context()); !errors.Is(err, maintenance.ErrNoActiveRevision) {
		t.Fatalf("roster with no activation = %v, want ErrNoActiveRevision", err)
	}

	activation, err := fleet.Activate(t.Context(), coord.ActivationRequest{RevisionID: "rev-1", At: time.Now()})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// A node with no reconciler cannot say which activation it serves.
	if handles, err := e.activeSeats(t.Context()); err == nil {
		t.Fatalf("a node with no reconciler produced a roster %v", handles)
	}

	// THE RECONCILER THE ENGINE BUILDS is the one the roster reads, or the
	// sweep could never judge a seat on a real node.
	r, err := e.NewReconciler(ReconcilerOptions{
		Store: refinementStore(t), Fleet: fleet, Queue: qmem.New(), NodeID: "node-a",
	})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	r.publish(applyProgress{applied: activation.Epoch - 1, target: activation.Epoch})
	if handles, err := e.activeSeats(t.Context()); err == nil {
		t.Fatalf("a node behind the fleet's epoch produced a roster %v; its seats may be stale", handles)
	}

	r.publish(applyProgress{applied: activation.Epoch, target: activation.Epoch})
	handles, err := e.activeSeats(t.Context())
	if err != nil {
		t.Fatalf("a node serving the fleet's epoch: %v", err)
	}
	// Agent seats only: a human seat has no mailbox to retire or keep. And
	// each carries the ID its mailbox is named by, because the sweep builds
	// every one of those names from it.
	var named []string
	for _, s := range handles {
		if s.ID == uuid.Nil {
			t.Errorf("roster entry %q carries no seat id, so the sweep can name "+
				"neither its mailbox nor the lease that excludes a node running it",
				s.Handle)
		}
		named = append(named, s.Handle)
	}
	if !slices.Equal(named, []string{"ceo", "swe"}) {
		t.Fatalf("roster = %v, want the agent seats", named)
	}

	// An unreadable pointer is unknown, never the last roster it produced.
	fleet.down = true
	if handles, err := e.activeSeats(t.Context()); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("roster with an unreadable pointer = (%v, %v), want the store's error", handles, err)
	}
}
