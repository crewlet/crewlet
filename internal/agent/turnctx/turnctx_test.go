package turnctx_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/org"
)

// THE ID A TURN REPORTS IS THE ONE THE ORG DERIVES — never a second rule.
//
// The derivation is the org's ([org.Organization.AgentIDFor]), and this type
// only spares its callers the two-value dance. A copy of the rule here would
// be a value that can disagree with the seat beside it on the same event.
func TestAgentIDIsTheOrgsOwnDerivation(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Staff Engineer"}
	o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}}
	want, ok := o.AgentIDFor(seat)
	if !ok {
		t.Fatal("the org derives no id for an agent seat")
	}
	tn := &turnctx.Turn{RunID: "run-1", WorkKey: "t-1", Seat: seat, Org: o}
	if got := tn.AgentID(); got != want.String() {
		t.Fatalf("AgentID() = %q, want the org's %q", got, want)
	}
}

// A SEAT WITH NO AGENT ID REPORTS NONE, rather than a zero uuid.
//
// A human seat is addressable and never spawned, so it has no agent id at
// all. Answering uuid.Nil would give every human in the company the same one
// — the exact confusion [org.Organization.AgentIDFor]'s two-value answer
// exists to prevent, and it would arrive on an event as a real-looking id.
func TestAgentIDIsEmptyForASeatThatHasNone(t *testing.T) {
	t.Parallel()
	human := &org.Role{Name: "Founder", Kind: org.KindHuman}
	o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{human}}
	if got := (&turnctx.Turn{Seat: human, Org: o}).AgentID(); got != "" {
		t.Fatalf("AgentID() = %q for a human seat, want empty", got)
	}
}

// A TURN BUILT OUTSIDE A COMPANY ANSWERS EMPTY rather than panicking.
//
// A tool surface built for a validate command or a test driving a runner
// directly legitimately has no seat and no org, which is the same bar
// Handle and Role already meet.
func TestAgentIDIsEmptyWithoutASeatOrAnOrg(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Staff Engineer"}
	o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}}
	for name, tn := range map[string]*turnctx.Turn{
		"nil turn": nil,
		"no seat":  {Org: o},
		"no org":   {Seat: seat},
	} {
		if got := tn.AgentID(); got != "" {
			t.Errorf("%s: AgentID() = %q, want empty", name, got)
		}
	}
}

// A DERIVED TURN, NOT A VIEW ONTO THE LOOP'S SLICE. The rounds are the one
// value that grows during a turn, and this type's whole purpose is to be safe
// to capture: a goroutine holding a Turn that aliased the loop's live slice
// would read what a later round appended — the exact data race the package
// exists to remove, on the field most likely to move underneath it.
func TestWithRoundsSnapshotsRatherThanAliases(t *testing.T) {
	t.Parallel()
	live := []ledger.Iteration{{Iteration: 1, Intent: "first"}}
	turn := (&turnctx.Turn{RunID: "run-1"}).WithRounds(live)

	// What the loop does between phases: append, and overwrite in place
	// when the append reuses the backing array.
	live = append(live, ledger.Iteration{Iteration: 2})
	live[0].Intent = "REWRITTEN"

	switch {
	case len(turn.Rounds) != 1:
		t.Errorf("the derived turn grew to %d rounds with the loop's slice",
			len(turn.Rounds))
	case turn.Rounds[0].Intent != "first":
		t.Errorf("the derived turn saw a later write: intent = %q",
			turn.Rounds[0].Intent)
	}
}

// DERIVED, so the turn it was derived FROM is unchanged — the immutability
// rule this package states, on the one field that has a setter at all.
func TestWithRoundsLeavesItsSourceAlone(t *testing.T) {
	t.Parallel()
	base := &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1"}
	next := base.WithRounds([]ledger.Iteration{{Iteration: 1}})
	switch {
	case len(base.Rounds) != 0:
		t.Error("WithRounds wrote the rounds onto the turn it derived from")
	case next.RunID != base.RunID || next.WorkKey != base.WorkKey:
		t.Error("the derived turn lost the identity it was derived from")
	}
}

// NIL IS A REAL ANSWER: round one of every turn, and every surface built
// outside a turn at all. A nil-Turn receiver is one of those, and it must not
// panic — a tool surface built by a validate command legitimately has none.
func TestWithRoundsOnNoTurnIsNil(t *testing.T) {
	t.Parallel()
	var none *turnctx.Turn
	if got := none.WithRounds([]ledger.Iteration{{Iteration: 1}}); got != nil {
		t.Errorf("WithRounds on no turn returned %+v, want nil", got)
	}
	if got := (&turnctx.Turn{}).WithRounds(nil); len(got.Rounds) != 0 {
		t.Errorf("a turn derived from no rounds carries %d", len(got.Rounds))
	}
}

// A WORK-KEYED TURN IS BOUNDED BY ITS EARLIEST TRIGGER, not by when it ran.
//
// Its operation ids are derived from the work key, which a redelivery derives
// again, so an id can have been minted by any earlier attempt — and every
// attempt was woken by these events, so none minted anything before the
// earliest of them existed. The dispatch's own instant is later than an
// earlier attempt's writes, and a write stamped with it reads as newer than an
// adoption its first record predates.
//
// Mutation: answer the latest event's instant, or started, and this fails.
func TestAWorkKeyedTurnIsBoundedByItsEarliestTrigger(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	earliest := started.Add(-3 * time.Hour)
	// In another zone, because the instant is compared against ones stamped
	// in UTC and is reported the same way.
	zone := time.FixedZone("UTC+2", 2*60*60)
	trigger := []*events.Event{
		{Timestamp: started.Add(-time.Hour)},
		nil,
		{}, // no timestamp: passed over, never read as the zero instant
		{Timestamp: earliest.In(zone)},
	}
	got := turnctx.TriggerInstant("wk-1", trigger, started)
	if !got.Equal(earliest) || got.Location() != time.UTC {
		t.Errorf("TriggerInstant = %v, want the earliest trigger %v in UTC", got, earliest)
	}
}

// A TURN WITH NO WORK KEY IS BOUNDED BY ITS OWN START: its seed is the run, and
// nothing but this run derives its ids — whatever its events say.
func TestATurnWithNoWorkKeyIsBoundedByItsStart(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	trigger := []*events.Event{{Timestamp: started.Add(-time.Hour)}}
	if got := turnctx.TriggerInstant("", trigger, started); !got.Equal(started) {
		t.Errorf("TriggerInstant = %v, want the run's start %v", got, started)
	}
}

// A WORK KEY NONE OF WHOSE EVENTS CARRIES A TIME FALLS BACK TO THE START rather
// than to the zero instant, which reads as "nothing is known" and would stamp
// each write with its own call instead.
func TestAWorkKeyWithNoTimedTriggerFallsBackToTheStart(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if got := turnctx.TriggerInstant("wk-1", []*events.Event{{}, nil}, started); !got.Equal(started) {
		t.Errorf("TriggerInstant = %v, want the run's start %v", got, started)
	}
}

// THE CALLS BEFORE THIS ONE ARE THE CLOSED ROUNDS', IN ORDER, THEN EARLIER'S.
//
// A write is named after the writes before it, so the order is the naming:
// a list that put the running phase's calls first would name a redelivered
// turn's writes differently from its first attempt's whenever both halves
// wrote to one object.
func TestCallsAreTheClosedRoundsThenTheRunningPhases(t *testing.T) {
	t.Parallel()
	turn := (&turnctx.Turn{RunID: "run-1"}).
		WithRounds([]ledger.Iteration{
			{Iteration: 1, Calls: []ledger.Call{{Name: "a"}, {Name: "b"}}},
			{Iteration: 2, Calls: []ledger.Call{{Name: "c"}}},
		}).
		WithEarlier([]ledger.Call{{Name: "d"}})
	var names []string
	for _, c := range turn.Calls() {
		names = append(names, c.Name)
	}
	if want := []string{"a", "b", "c", "d"}; !slices.Equal(names, want) {
		t.Errorf("Calls() = %v, want %v", names, want)
	}
	var none *turnctx.Turn
	if got := none.Calls(); got != nil {
		t.Errorf("Calls() outside a turn = %v, want nil", got)
	}
}

// WITH EARLIER SNAPSHOTS, for WithRounds' reason: the running phase's calls
// grow while a goroutine may hold the Turn a call was handed, and a Turn
// aliasing the recorder's slice would read what a later call appended — and
// name a write after a call that came after it.
func TestWithEarlierSnapshotsAndLeavesItsSourceAlone(t *testing.T) {
	t.Parallel()
	base := &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1"}
	live := []ledger.Call{{Name: "first"}}
	turn := base.WithEarlier(live)
	live = append(live, ledger.Call{Name: "later"})
	live[0].Name = "REWRITTEN"
	switch {
	case len(turn.Earlier) != 1 || turn.Earlier[0].Name != "first":
		t.Errorf("the derived turn saw a later write: %+v", turn.Earlier)
	case len(base.Earlier) != 0:
		t.Error("WithEarlier wrote the calls onto the turn it derived from")
	case turn.RunID != base.RunID || turn.WorkKey != base.WorkKey:
		t.Error("the derived turn lost the identity it was derived from")
	}
	var none *turnctx.Turn
	if got := none.WithEarlier(live); got != nil {
		t.Errorf("WithEarlier on no turn returned %+v, want nil", got)
	}
}

// A SUB-AGENT CARRIES THE INSTANT ITS IDENTITIES SEED. The instant is a fact
// about the ids the run and the work key derive, and a worker writing under
// them without it would stamp each write with its own call — the instant a
// retry after an adoption reads as newer than the adoption.
func TestASubagentCarriesTheInstantItsIdentitiesSeed(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 26, 9, 0, 0, 0, time.UTC)
	parent := &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", TriggeredAt: at}
	child, err := parent.ForSubagent(&org.Role{Name: "Worker"}, 0)
	if err != nil {
		t.Fatalf("ForSubagent: %v", err)
	}
	if child.RunID != "run-1" || child.WorkKey != "wk-1" || !child.TriggeredAt.Equal(at) {
		t.Errorf("the worker's turn = (%q, %q, %v), want the parent's (run-1, wk-1, %v)",
			child.RunID, child.WorkKey, child.TriggeredAt, at)
	}
}
