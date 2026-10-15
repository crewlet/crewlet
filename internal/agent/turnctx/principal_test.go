package turnctx_test

import (
	"context"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// A SEAT HOLDS EVERY APP AND NEVER ADMIN.
//
// What a seat may do is bounded by its role's registry, so its principal
// carries every app — and never admin, which would let an agent rewrite the
// company it runs inside. The answer is a CLONE: a caller appending admin to
// one turn's grants must not reach the list of apps every other turn's
// principal is built from. The control is a turn with no seat, which holds
// nothing. Mutation: hand back iam.AllGrants and the admin assertion goes red;
// hand back iam.Apps itself and the aliasing one does.
func TestASeatHoldsEveryAppAndNeverAdmin(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Staff Engineer"}
	o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}}
	held := turnctx.Principal(&turnctx.Turn{RunID: "run-1", Seat: seat, Org: o})
	if held.Kind != iam.KindSeat || held.Seat != seat.Handle() {
		t.Fatalf("the turn's principal is %+v, want the seat %q", held, seat.Handle())
	}
	if !slices.Equal(held.Grants, iam.Apps) {
		t.Errorf("a seat holds %v, want every app: %v", held.Grants, iam.Apps)
	}
	if held.Can(iam.GrantAdmin) || slices.Contains(held.Grants, iam.GrantAdmin) {
		t.Error("a seat holds admin, so an agent may run the company it works in")
	}
	before := slices.Clone(iam.Apps)
	if len(held.Grants) > 0 {
		held.Grants[0] = iam.GrantAdmin
	}
	if !slices.Equal(iam.Apps, before) {
		t.Fatalf("editing one turn's grants edited iam.Apps: %v", iam.Apps)
	}
	if again := turnctx.Principal(&turnctx.Turn{Seat: seat, Org: o}); again.Can(iam.GrantAdmin) {
		t.Error("a second turn's seat holds what the first turn's grants were edited to")
	}

	if none := turnctx.Principal(&turnctx.Turn{}); len(none.Grants) != 0 || none.Seat != "" {
		t.Errorf("a turn with no seat acts as %+v, want nobody holding nothing", none)
	}
	if _, how := iam.From(turnctx.WithPrincipal(context.Background(), &turnctx.Turn{})); how != iam.Unknown {
		t.Errorf("a context with no seat was answered %v, want left unknown", how)
	}
}
