package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
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
	got := nodeRoles(nil)
	if !got.Equal(placement.DefaultRoles()) {
		t.Errorf("roles = %v, want every role", got)
	}
	if !nodeRoles([]string{}).Equal(placement.DefaultRoles()) {
		t.Error("an empty slice is not the same as nil here, and must be")
	}
}

func TestConfiguredRolesAreTheOnesTaken(t *testing.T) {
	t.Parallel()
	// The counterfactual: "everything" is the default only when nothing is
	// named, and a node told to run one role must not quietly run three.
	got := nodeRoles([]string{string(placement.RoleIngress)})
	if !got.Has(placement.RoleIngress) {
		t.Error("the configured role was not taken")
	}
	if got.Has(placement.RoleSeats) || got.Has(placement.RoleWorkers) {
		t.Errorf("roles = %v: a node configured for ingress alone took more", got)
	}
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
