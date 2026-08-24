package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// Internal because these are the engine's own hooks, not its API. Exporting
// park, pause and runTurn so an external test could reach them would put three
// methods on the public surface whose only caller is the dispatcher this file
// already builds.

func TestAnEmptyRoleListMeansEveryRole(t *testing.T) {
	t.Parallel()
	// Reading it as "no roles" produces a node that claims no seats, runs
	// no workers and hears no webhook — a process that starts cleanly and
	// does nothing at all.
	got := profileFor(t, nil).Roles
	if !got.Equal(placement.DefaultRoles()) {
		t.Errorf("roles = %v, want every role", got)
	}
	if !profileFor(t, []string{}).Roles.Equal(placement.DefaultRoles()) {
		t.Error("an empty slice is not the same as nil here, and must be")
	}
}

func TestConfiguredRolesAreTheOnesTaken(t *testing.T) {
	t.Parallel()
	// The counterfactual: "everything" is the default only when nothing is
	// named, and a node told to run one role must not quietly run three.
	got := profileFor(t, []string{string(placement.RoleIngress)}).Roles
	if !got.Has(placement.RoleIngress) {
		t.Error("the configured role was not taken")
	}
	if got.Has(placement.RoleSeats) || got.Has(placement.RoleWorkers) {
		t.Errorf("roles = %v, want only ingress", got)
	}
}

// profileFor builds a node profile the way the engine does — through the
// bootstrap's own accessor, which is the only parse of node.roles there is.
func profileFor(t *testing.T, roles []string) placement.NodeProfile {
	t.Helper()
	node := config.Node{Roles: roles}
	return node.Profile("n1")
}

// engineOn wires just enough engine for the park and pause hooks: they reach
// the queue and the topic grammar and nothing else.
func engineOn(t *testing.T) (*Engine, queue.EventQueue) {
	t.Helper()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })
	return &Engine{backends: &Backends{Queue: q}}, q
}

func TestAParkRepublishesOntoTheSeatsOwnInbox(t *testing.T) {
	t.Parallel()
	// Onto the seat's inbox, not a side channel: the requeued copy has to
	// come back through the same subscription, or the seat never sees it
	// again.
	e, q := engineOn(t)
	var got []string
	if err := q.Subscribe(t.Context(), topics.AgentInbox("ceo"), "probe",
		func(_ context.Context, evt *events.Event) queue.Result {
			got = append(got, evt.ID.String())
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	one := &events.Event{ID: uuid.New(), Type: "external_notification"}
	two := &events.Event{ID: uuid.New(), Type: "external_notification"}
	if err := e.park(t.Context(), "ceo", []*events.Event{one, two}); err != nil {
		t.Fatalf("park: %v", err)
	}
	want := []string{one.ID.String(), two.ID.String()}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("requeued %v, want %v", got, want)
	}
}

func TestAFailedRequeueIsReportedRatherThanSwallowed(t *testing.T) {
	t.Parallel()
	// The dispatcher acks only when the park lands. A requeue that failed
	// and reported success is the one shape that loses the work outright.
	e, q := engineOn(t)
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	err := e.park(t.Context(), "ceo",
		[]*events.Event{{ID: uuid.New(), Type: "external_notification"}})
	if err == nil {
		t.Fatal("a requeue onto a stopped broker reported success")
	}
	if !strings.Contains(err.Error(), "ceo") {
		t.Errorf("the error does not name the seat: %v", err)
	}
}

func TestAPauseTakesAStableHoldNotTheProseReason(t *testing.T) {
	t.Parallel()
	// Pause holds are keyed by reason so two subsystems gating one inbox
	// cannot release each other's. That makes the key a contract between
	// the pause and the eventual resume, and deriving it from the
	// screening's human-readable reason would strand every parked seat the
	// day someone reworded a log line.
	e, q := engineOn(t)
	const prose = "no turn engine"
	if err := e.pause(t.Context(), "ceo", prose); err != nil {
		t.Fatalf("pause: %v", err)
	}
	holds := q.(*memory.Queue).PauseHolds(
		topics.AgentInbox("ceo"), topics.AgentInboxGroup("ceo"))
	if !slices.Contains(holds, pauseReasonNoTurnEngine) {
		t.Errorf("holds = %v, want the stable key %q", holds, pauseReasonNoTurnEngine)
	}
	if slices.Contains(holds, prose) {
		t.Errorf("holds = %v: the hold is keyed on a log message", holds)
	}
}

func TestAPausedInboxStopsDelivering(t *testing.T) {
	t.Parallel()
	// The hold has to actually gate the subscription. A pause that records
	// a reason and delivers anyway makes the park worse than doing
	// nothing: the requeued copies loop straight back at whatever rate the
	// broker will serve.
	e, q := engineOn(t)
	var delivered int
	if err := q.Subscribe(t.Context(), topics.AgentInbox("ceo"),
		topics.AgentInboxGroup("ceo"),
		func(context.Context, *events.Event) queue.Result {
			delivered++
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := e.pause(t.Context(), "ceo", "no turn engine"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := e.park(t.Context(), "ceo",
		[]*events.Event{{ID: uuid.New(), Type: "external_notification"}}); err != nil {
		t.Fatalf("park: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if delivered != 0 {
		t.Errorf("a paused inbox delivered %d events", delivered)
	}
}

func TestParkAndPauseRefuseAnUnroutableSeat(t *testing.T) {
	t.Parallel()
	// Refusing here rather than publishing to an empty subject is what
	// keeps the failure loud: a publish to "" reaches nobody and reports
	// nothing.
	e, _ := engineOn(t)
	if err := e.park(t.Context(), "",
		[]*events.Event{{ID: uuid.New(), Type: "external_notification"}}); err == nil {
		t.Error("a park onto an unroutable seat succeeded")
	}
	if err := e.pause(t.Context(), "", "reason"); err == nil {
		t.Error("a pause on an unroutable seat succeeded")
	}
}

// --- what this node advertises about itself -------------------------------

// A POSTURE READ THAT RAN OUT OF THE BEAT'S BUDGET DID NOT ANSWER.
//
// [Reconciler.Posture] fails OPEN — a control plane it cannot read reports
// as "serve", deliberately, so a database blip does not take every node out
// of rotation. That is the right answer for the readiness probe and the
// wrong one to broadcast: it would put a confident healthy posture on the
// one node an operator is looking for.
func TestATimedOutPostureReadPublishesNoPosture(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	e.SetPosture(func(ctx context.Context) string {
		<-ctx.Done()
		return "serve" // what a fail-open reporter hands back
	})

	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if got := e.nodeStatus(ctx).Posture; got != "" {
		t.Errorf("posture = %q, want none published", got)
	}
}

// AN ANSWERED READ IS PUBLISHED, which is the whole point of carrying it.
func TestAnAnsweredPostureReachesTheHeartbeat(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	e.SetPosture(func(context.Context) string { return "shed" })
	if got := e.nodeStatus(t.Context()).Posture; got != "shed" {
		t.Errorf("posture = %q, want shed", got)
	}
}

// A NODE WITH NO REPORTER PUBLISHES NO POSTURE, rather than an empty one
// that a reader would have to tell apart from a real answer.
func TestANodeWithNoPostureReporterPublishesNone(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	status := e.nodeStatus(t.Context())
	if status.Posture != "" {
		t.Errorf("posture = %q", status.Posture)
	}
	if _, published := status.Meta()["posture"]; published {
		t.Error("an absent posture reached the lease anyway")
	}
}

// THE START IS THIS ENGINE'S OWN. On a split deployment the API is a
// different process on a different clock, so a peer reading one uptime for
// both would report a number that is true of neither.
func TestTheAdvertisedStartIsTheEnginesOwn(t *testing.T) {
	t.Parallel()
	e := &Engine{startedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)}
	if got := e.nodeStatus(t.Context()).StartedAt; !got.Equal(e.StartedAt()) {
		t.Errorf("started_at = %v, want %v", got, e.StartedAt())
	}
}

// A DRAINING NODE SAYS SO, and its peers can tell it apart from one that
// simply died: a row that is both draining and expiring is a clean shutdown,
// one that vanished without it is a process that fell over.
func TestADrainingNodeAdvertisesIt(t *testing.T) {
	t.Parallel()
	n, err := node.New(node.Config{
		Queue:  memory.New(),
		Coord:  coordmemory.New(),
		NodeID: "node-a",
		Owner:  "node-a:1",
		Seats:  func() []placement.Seat { return nil },
		Turn: func(context.Context, string, []*events.Event) queue.Result {
			return queue.Ack()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{node: n}
	if e.nodeStatus(t.Context()).Draining {
		t.Fatal("a fresh node reported itself draining")
	}

	n.Host().BeginDrain(t.Context())

	if !e.nodeStatus(t.Context()).Draining {
		t.Error("a draining node did not say so")
	}
}

// THE IN-FLIGHT COUNT IS THIS NODE'S OWN, read live off the broker client
// rather than from a cached number: a cached one is a node reporting work it
// finished a minute ago, and the whole reason for carrying it fleet-wide is
// that an operator can see where the work actually is right now.
func TestTheAdvertisedInFlightCountIsTheBrokerClients(t *testing.T) {
	t.Parallel()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	e := &Engine{backends: &Backends{Queue: q}}
	if got := e.nodeStatus(t.Context()).InFlight; got != 0 {
		t.Fatalf("in flight = %d on an idle node", got)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	const topic = "crewlet.test.inbox"
	if err := q.Subscribe(t.Context(), topic, "g", func(context.Context, *events.Event) queue.Result {
		close(entered)
		<-release
		return queue.Ack()
	}); err != nil {
		t.Fatal(err)
	}

	// The memory broker runs the handler INLINE on Publish, so the publish
	// has to be the thing that waits, not this goroutine.
	delivered := make(chan error, 1)
	go func() {
		delivered <- q.Publish(context.Background(), topic,
			events.New(types.TaskAssigned{TaskID: "t-1"}, events.NewTrace()))
	}()
	<-entered

	got := e.nodeStatus(t.Context()).InFlight
	close(release)
	if err := <-delivered; err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("in flight = %d, want the handler this node is running", got)
	}
}
