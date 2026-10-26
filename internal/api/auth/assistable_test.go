package auth

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// TestEveryClauseOfWhoMayRideAGrantRefusesAlone: each clause of [assistable]
// is held by a principal that breaks it and nothing else, because the guard's
// cases reach it through principals that break several at once (a Tier A
// token is a machine, with no seat and no session) and so cannot say which
// clause refused. Both ends ask this one predicate: the minter of who it
// derives for, the guard of who it re-composes. Mutation: drop any clause and
// its row is admitted.
func TestEveryClauseOfWhoMayRideAGrantRefusesAlone(t *testing.T) {
	t.Parallel()
	person := func() iam.Principal {
		return iam.Principal{ID: uuid.Must(uuid.NewV7()), Kind: iam.KindPerson,
			Login: "sarah.chen", Seat: "reviewer", Stage: iam.StageActive,
			Via: iam.SessionName(uuid.Must(uuid.NewV7()).String())}
	}
	if reason := assistable(person()); reason != "" {
		t.Fatalf("the control was refused: %s", reason)
	}
	for name, edit := range map[string]func(*iam.Principal){
		"a machine":                  func(p *iam.Principal) { p.Kind = iam.KindMachine },
		"a seat":                     func(p *iam.Principal) { p.Kind = iam.KindSeat },
		"a suspended person":         func(p *iam.Principal) { p.Stage = iam.StageSuspended },
		"a person with no seat":      func(p *iam.Principal) { p.Seat = "" },
		"through a machine token":    func(p *iam.Principal) { p.Via = iam.MachineTokenName(uuid.Must(uuid.NewV7()).String()) },
		"through another grant":      func(p *iam.Principal) { p.Via = iam.AssistName(uuid.Must(uuid.NewV7())) },
		"through no credential":      func(p *iam.Principal) { p.Via = "" },
		"a Tier A token's own login": func(p *iam.Principal) { p.Login = iam.TokenLogin("ci") },
		"with no id":                 func(p *iam.Principal) { p.ID = uuid.Nil },
	} {
		p := person()
		edit(&p)
		if assistable(p) == "" {
			t.Errorf("%s may ride a grant", name)
		}
	}
}
