package turnctx_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
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
