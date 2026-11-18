package host

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/iam"
)

// A SHARE IS THE HUMAN SEAT A CREDENTIAL ACTS AS, NOT THE CREDENTIAL: a
// person's tokens and sessions all name their seat, so two credentials cannot
// double their turn; a Tier A token bound to a seat nobody else holds is its
// holder, a person. A credential acting as nobody is a machine named by the
// credential (a Tier A token's login, a machine token), so a token per CI
// runner is a turn per runner. TestEveryTransferCarriesItsCredentialsShare
// holds the call sites through real git; several credentials per principal
// are this case's, composed as admit does (admitted).
//
// Mutations: share() naming a bound credential's operator id rather than its
// seat's handle — red; share() keying a machine by its owner
// (iam.RecordOwner) rather than its credential — red.
func TestAShareIsTheSeatACredentialActsAs(t *testing.T) {
	t.Parallel()
	laptop, script := iam.MachineTokenName(uuid.NewString()), iam.MachineTokenName(uuid.NewString())
	runner1, runner2 := iam.MachineTokenName(uuid.NewString()), iam.MachineTokenName(uuid.NewString())
	alice := func(via string) iam.Principal {
		return iam.Principal{ID: uuid.New(), Login: "alice.smith", Kind: iam.KindPerson, Seat: "alice",
			Stage: iam.StageActive, Via: via}
	}
	// A service account acting as itself through one of its machine tokens,
	// bound to no seat.
	release := func(via string) iam.Principal {
		return iam.Principal{ID: uuid.New(), Login: "ci:release", Kind: iam.KindMachine, Stage: iam.StageActive, Via: via}
	}
	person := func(seat string) gitproc.Share { return gitproc.Share{Class: gitproc.SharePerson, Principal: seat} }
	machine := func(credential string) gitproc.Share {
		return gitproc.Share{Class: gitproc.ShareMachine, Principal: credential}
	}
	for _, tc := range []struct {
		name string
		p    iam.Principal
		want gitproc.Share
	}{
		{"a person's machine token on their laptop", alice(laptop), person("alice")},
		{"the same person's machine token in a script", alice(script), person("alice")},
		{"the same person signed in", alice(iam.SessionName(uuid.NewString())), person("alice")},
		// Re-kinded as the seat's holder by the guard, its login unchanged.
		{"a Tier A token bound to a seat nobody holds", iam.Principal{ID: uuid.New(),
			Login: iam.TokenLogin("deploy-bot"), Kind: iam.KindPerson, Seat: "deploy", Stage: iam.StageActive},
			person("deploy")},
		{"a Tier A token bound to nobody", iam.Principal{ID: uuid.New(), Login: iam.TokenLogin("ci"),
			Kind: iam.KindMachine, Stage: iam.StageActive}, machine(iam.TokenLogin("ci"))},
		{"a service account's token on one runner", release(runner1), machine(runner1)},
		{"the same service account's token on another", release(runner2), machine(runner2)},
	} {
		got := admitted("web", tc.p).share()
		if got != tc.want {
			t.Errorf("%s (%+v) is charged to %+v, want %+v", tc.name, tc.p, got, tc.want)
		}
		if !got.Valid() {
			t.Errorf("%s's share %+v is one the pool refuses", tc.name, got)
		}
	}
}
