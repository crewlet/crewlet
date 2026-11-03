package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
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

// activate moves the fleet's pointer to a revision, as an operator's write
// does, and answers the activation it made: the instant a node that applied it
// carries ([Company.ActivatedAt]).
func activate(t *testing.T, fleet *rosterFleet, revision string) coord.Activation {
	t.Helper()
	// EVERY ACTIVATION CARRIES ITS ORIGIN: the store refuses one without.
	activation, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: revision, Payload: []byte("{}"), At: time.Now(),
		Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorHuman,
			Source: "api", CreatedAt: time.Now()},
	})
	if err != nil {
		t.Fatalf("activate %s: %v", revision, err)
	}
	return activation
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
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
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
	// THE KEYRING every node [New] builds holds: the reconciler below opens
	// the revisions it applies with it, and refuses an engine without one.
	_, cipher := testKeyring(t)
	e := &Engine{backends: &Backends{Fleet: fleet}, cipher: cipher}
	e.epoch.current.Store(company)

	// Nothing activated: not a fault, and not a roster of nobody either.
	if _, err := e.activeSeats(t.Context()); !errors.Is(err, maintenance.ErrNoActiveRevision) {
		t.Fatalf("roster with no activation = %v, want ErrNoActiveRevision", err)
	}

	// EVERY ACTIVATION CARRIES ITS ORIGIN: the store refuses one without.
	origin := coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorHuman,
		Source: "api", CreatedAt: time.Now()}
	earlier, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Payload: []byte("{}"), At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	activation, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-2", Payload: []byte("{}"), At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// A company no activation has named — the file a node booted with —
	// cannot say it is the fleet's. On a node with no reconciler that is
	// where every node starts: unknown, and the quiet kind, as a reconciler
	// that has not ticked is.
	if handles, err := e.activeSeats(t.Context()); err == nil ||
		!errors.Is(err, maintenance.ErrRevisionPropagating) {
		t.Fatalf("a company no activation named, on a node with no reconciler, produced "+
			"(%v, %v), want ErrRevisionPropagating", handles, err)
	}

	// THE RECONCILER THE ENGINE BUILDS is the one the roster reads, or the
	// sweep could never judge a seat on a real node.
	r, err := e.NewReconciler(t.Context(), ReconcilerOptions{
		Store: refinementStore(t), Fleet: fleet, Queue: qmem.New(), NodeID: "node-a",
	})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	// A company applied from an EARLIER activation is behind the fleet: its
	// seats may be stale. Still applying the newer one, it is unknown and
	// the quiet kind — every node is here for the seconds of every
	// activation.
	company.ActivatedAt = earlier.At
	r.publish(applyProgress{applied: activation.Epoch - 1, target: activation.Epoch,
		behindSince: time.Now()})
	if handles, err := e.activeSeats(t.Context()); err == nil ||
		!errors.Is(err, maintenance.ErrRevisionPropagating) {
		t.Fatalf("a node still applying the fleet's activation produced (%v, %v), want "+
			"ErrRevisionPropagating; its seats may be stale", handles, err)
	}
	// BEHIND, and its apply failed: unknown, and a fault the sweep reports.
	r.publish(applyProgress{applied: activation.Epoch - 1, target: activation.Epoch, attempts: 1})
	if handles, err := e.activeSeats(t.Context()); err == nil ||
		errors.Is(err, maintenance.ErrRevisionPropagating) {
		t.Fatalf("a node that failed the fleet's activation produced (%v, %v), want an "+
			"unknown roster the sweep reports", handles, err)
	}

	// APPLIED FROM THE POINTER'S OWN ACTIVATION, the company is the fleet's —
	// even while the reconciler is still inside the apply that publishes it,
	// and has not recorded the epoch yet.
	company.ActivatedAt = activation.At
	r.publish(applyProgress{applied: activation.Epoch - 1, target: activation.Epoch,
		behindSince: time.Now()})
	if handles, err := e.activeSeats(t.Context()); err != nil || len(handles) != 2 {
		t.Fatalf("a node publishing the company of the fleet's activation, its apply still "+
			"in flight, produced (%v, %v), want the roster", handles, err)
	}
	r.publish(applyProgress{applied: activation.Epoch, target: activation.Epoch})
	handles, err := e.activeSeats(t.Context())
	if err != nil {
		t.Fatalf("a node serving the fleet's activation: %v", err)
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
	// AHEAD of the pointer it read — the pointer moved after the read, and
	// this node had already applied what it moved to — it serves a truer
	// revision than the one read, as the posture reads the same race.
	company.ActivatedAt = activation.At.Add(time.Second)
	r.publish(applyProgress{applied: activation.Epoch + 1, target: activation.Epoch + 1})
	if handles, err := e.activeSeats(t.Context()); err != nil || len(handles) != 2 {
		t.Fatalf("a node ahead of the pointer it read produced (%v, %v), want the roster", handles, err)
	}

	// An unreadable pointer is unknown, never the last roster it produced.
	fleet.down = true
	if handles, err := e.activeSeats(t.Context()); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("roster with an unreadable pointer = (%v, %v), want the store's error", handles, err)
	}
}
