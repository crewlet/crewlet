package turnctx_test

import (
	"testing"

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
	tn := &turnctx.Turn{ID: "t-1", Seat: seat, Org: o}
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
