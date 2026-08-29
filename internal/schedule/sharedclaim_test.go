package schedule_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/schedule"
)

var fireAt = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

func fire(scopeID, name, label, target string) schedule.Run {
	return schedule.Run{
		FireKey: schedule.FireKey{
			Scope: types.ScheduleScopeRole, ScopeID: scopeID,
			ScheduleName: name, FireLabel: label, TargetHandle: target,
		},
		ScheduledAt: fireAt, FiredAt: fireAt, Outcome: schedule.OutcomeFired,
	}
}

// The failure this exists to stop. Two nodes, one company: the scheduler duty
// moved, and the new holder's catchup pass evaluates a minute the old holder
// already fired. While the ledger was the node's own database the second node
// saw nothing and dispatched again.
func TestTheDutyMovingDoesNotRefireWhatTheLastHolderClaimed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fleet := memory.NewFleet()

	// Each node has its own local audit ledger, which is the point: they
	// are not what makes the fire unique.
	first, err := schedule.NewSharedClaimer(fleet, schedule.NewMemoryLedger())
	if err != nil {
		t.Fatalf("NewSharedClaimer: %v", err)
	}
	second, err := schedule.NewSharedClaimer(fleet, schedule.NewMemoryLedger())
	if err != nil {
		t.Fatalf("NewSharedClaimer: %v", err)
	}

	run := fire("cto", "standup", "20260314T0900", "cto")
	won, err := first.Claim(ctx, run)
	if err != nil || !won {
		t.Fatalf("the first claim: won=%v err=%v", won, err)
	}
	won, err = second.Claim(ctx, run)
	if err != nil {
		t.Fatalf("the successor's claim: %v", err)
	}
	if won {
		t.Error("the successor re-fired a claim its predecessor made — two standups")
	}
}

func TestADistinctFireIsNotSuppressed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	claimer, err := schedule.NewSharedClaimer(memory.NewFleet(), schedule.NewMemoryLedger())
	if err != nil {
		t.Fatalf("NewSharedClaimer: %v", err)
	}
	// Every component of the identity separates a fire: an `each` fan-out
	// mints one per member so a slow member cannot suppress its siblings,
	// and the minute stamp is what lets the schedule run at all.
	for _, run := range []schedule.Run{
		fire("cto", "standup", "20260314T0900", "cto"),
		fire("cto", "standup", "20260314T0900", "eng-1"),
		fire("cto", "standup", "20260314T0901", "cto"),
		fire("cto", "retro", "20260314T0900", "cto"),
		fire("platform", "standup", "20260314T0900", "cto"),
	} {
		won, err := claimer.Claim(context.Background(), run)
		if err != nil {
			t.Fatalf("Claim(%+v): %v", run.FireKey, err)
		}
		if !won {
			t.Errorf("a distinct fire was suppressed: %+v", run.FireKey)
		}
	}
	_ = ctx
}

// The collision the FireKey struct exists to rule out, now that the identity
// has to become one string for the coordination store. Two DIFFERENT fires
// must never claim one key — the second is then silently never dispatched,
// which is the same "one of the two standups vanishes" the struct avoided by
// not joining at all.
func TestADelimiterInANameDoesNotCollideWithItsNeighbour(t *testing.T) {
	t.Parallel()
	// A pipe that moves across a field boundary. A naive join renders both
	// of these "unit|a|b|c|d" — a unit "a|b" with a schedule "c", and a
	// unit "a" with a schedule "b|c", are different fires.
	pipe := [2]schedule.FireKey{
		{Scope: types.ScheduleScopeUnit, ScopeID: "a|b", ScheduleName: "c", FireLabel: "d"},
		{Scope: types.ScheduleScopeUnit, ScopeID: "a", ScheduleName: "b|c", FireLabel: "d"},
	}
	// And the escape character itself. If the backslash did not escape
	// itself first, a literal `\p` in a name would render the same as an
	// escaped pipe — the escaping would just have moved the collision one
	// level down instead of removing it.
	escape := [2]schedule.FireKey{
		{Scope: types.ScheduleScopeUnit, ScopeID: `\p`, ScheduleName: "c"},
		{Scope: types.ScheduleScopeUnit, ScopeID: "|", ScheduleName: "c"},
	}
	for _, pair := range [][2]schedule.FireKey{pipe, escape} {
		first, second := schedule.FireClaimKey(pair[0]), schedule.FireClaimKey(pair[1])
		if first == second {
			t.Errorf("%+v and %+v both render %q — one of the two fires is suppressed",
				pair[0], pair[1], first)
		}
	}
}

type refusingFleet struct{ err error }

func (r refusingFleet) ClaimFire(context.Context, string, time.Time) (bool, error) {
	return false, r.err
}

// FAILS CLOSED, and the error has to reach the caller as an error. A store
// that could not be read reported as a lost race would tell the scheduler the
// fire was handled, and the next tick would skip it too — the schedule would
// stop, silently, for as long as the outage lasted plus the catchup window.
func TestAnUnreachableClaimStoreIsAnErrorNotALostRace(t *testing.T) {
	t.Parallel()
	boom := errors.New("the broker is unreachable")
	claimer, err := schedule.NewSharedClaimer(refusingFleet{err: boom}, schedule.NewMemoryLedger())
	if err != nil {
		t.Fatalf("NewSharedClaimer: %v", err)
	}
	won, err := claimer.Claim(context.Background(), fire("cto", "standup", "20260314T0900", "cto"))
	if won {
		t.Error("a fire was granted by a store that could not be read")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the store's own error wrapped", err)
	}
}

// A local audit row is a dashboard row. Losing one must not un-fire anything:
// the fleet has already granted the claim, and refusing here would turn a full
// disk into a company whose crons stop.
func TestALostAuditRowDoesNotUnfireTheClaim(t *testing.T) {
	t.Parallel()
	claimer, err := schedule.NewSharedClaimer(memory.NewFleet(), brokenLedger{})
	if err != nil {
		t.Fatalf("NewSharedClaimer: %v", err)
	}
	won, err := claimer.Claim(context.Background(), fire("cto", "standup", "20260314T0900", "cto"))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !won {
		t.Error("a granted fire was dropped because its audit row failed")
	}
}

type brokenLedger struct{ schedule.Ledger }

func (brokenLedger) Claim(context.Context, schedule.Run) (bool, error) {
	return false, errors.New("no space left on device")
}

func TestASharedClaimerWithoutTheFleetIsRefused(t *testing.T) {
	t.Parallel()
	// The whole point of the type. A claimer that fell back to the local
	// ledger would look identical to a correct one until a second node
	// joined — which is the failure this replaced.
	if _, err := schedule.NewSharedClaimer(nil, schedule.NewMemoryLedger()); !errors.Is(err, schedule.ErrNoFireClaims) {
		t.Errorf("err = %v, want ErrNoFireClaims", err)
	}
}
