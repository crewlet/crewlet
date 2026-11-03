package host

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/iam"
)

// A SHARE IS THE HUMAN SEAT A CREDENTIAL ACTS AS, NOT THE CREDENTIAL: a
// person's laptop, scripts and browser are one principal of the person class
// — each of their machine tokens and sessions names their seat — so a person
// cannot double their turn by holding two credentials; a Tier A token the
// directory binds to a human seat nobody else holds is that seat's one
// holder, and a person by it. A credential acting as nobody is a machine
// principal of its own, named by the credential — a Tier A token's login, or
// a service account's machine token, so a token per CI runner is a turn per
// runner. Every admission is composed from the principal the guard resolves,
// as admit composes it (admitted). (TestEveryTransferCarriesItsCredentialsShare
// holds the call sites through real git, where each person is ONE credential —
// codetest's Tier A token on a seat nobody else holds — and the token bound to
// nobody is one; the principals a person's several credentials resolve to, and
// a service account's per-runner tokens, are this case's.)
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
