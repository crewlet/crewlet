package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
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
	if _, err := e.activeSeatHandles(t.Context()); !errors.Is(err, maintenance.ErrNoActiveRevision) {
		t.Fatalf("roster with no activation = %v, want ErrNoActiveRevision", err)
	}

	activation, err := fleet.Activate(t.Context(), coord.ActivationRequest{RevisionID: "rev-1", At: time.Now(), Origin: coord.RevisionOrigin{AuthorKind: "operator", Source: "api", CreatedAt: time.Now()}})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// A node with no reconciler cannot say which activation it serves: it
	// has not reconciled yet, which is where every node starts — unknown,
	// and the quiet kind, as a reconciler that has not ticked is.
	if handles, err := e.activeSeatHandles(t.Context()); err == nil ||
		!errors.Is(err, maintenance.ErrRevisionPropagating) {
		t.Fatalf("a node with no reconciler produced (%v, %v), want ErrRevisionPropagating",
			handles, err)
	}

	// THE RECONCILER THE ENGINE BUILDS is the one the roster reads, or the
	// sweep could never judge a seat on a real node.
	r, err := e.NewReconciler(ReconcilerOptions{
		Store: refinementStore(t), Fleet: fleet, Queue: qmem.New(), NodeID: "node-a",
	})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	// BEHIND, and still applying it: unknown, and the quiet kind — every
	// node is here for the seconds of every activation.
	r.publish(applyProgress{applied: activation.Epoch - 1, target: activation.Epoch,
		behindSince: time.Now()})
	if handles, err := e.activeSeatHandles(t.Context()); err == nil ||
		!errors.Is(err, maintenance.ErrRevisionPropagating) {
		t.Fatalf("a node still applying the fleet's epoch produced (%v, %v), want "+
			"ErrRevisionPropagating; its seats may be stale", handles, err)
	}
	// BEHIND, and its apply failed: unknown, and a fault the sweep reports.
	r.publish(applyProgress{applied: activation.Epoch - 1, target: activation.Epoch, attempts: 1})
	if handles, err := e.activeSeatHandles(t.Context()); err == nil ||
		errors.Is(err, maintenance.ErrRevisionPropagating) {
		t.Fatalf("a node that failed the fleet's epoch produced (%v, %v), want an unknown "+
			"roster the sweep reports", handles, err)
	}

	r.publish(applyProgress{applied: activation.Epoch, target: activation.Epoch})
	handles, err := e.activeSeatHandles(t.Context())
	if err != nil {
		t.Fatalf("a node serving the fleet's epoch: %v", err)
	}
	// Agent seats only: a human seat has no mailbox to retire or keep.
	if !slices.Equal(handles, []string{"ceo", "swe"}) {
		t.Fatalf("roster = %v, want the agent seats", handles)
	}
	// AHEAD of the pointer it read — the pointer moved after the read, and
	// this node had already applied what it moved to — it serves a truer
	// revision than the one read, as the posture reads the same race.
	r.publish(applyProgress{applied: activation.Epoch + 1, target: activation.Epoch + 1})
	if handles, err := e.activeSeatHandles(t.Context()); err != nil || len(handles) != 2 {
		t.Fatalf("a node ahead of the pointer it read produced (%v, %v), want the roster", handles, err)
	}

	// An unreadable pointer is unknown, never the last roster it produced.
	fleet.down = true
	if handles, err := e.activeSeatHandles(t.Context()); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("roster with an unreadable pointer = (%v, %v), want the store's error", handles, err)
	}
}
