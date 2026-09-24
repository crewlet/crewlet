package setupapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE STATE'S OWN RULES — sealed, scoped to this flow, expiring, and carrying
// who began the creation — are held by githubstate_internal_test.go, against
// the mint and the open themselves. What is held here is the callback around
// them.

// A NIL CLOCK MUST NOT REACH A REQUEST, and this one reached the worst
// possible request.
//
// Every caller but the tests leaves Options.Now nil, and the service stored
// it unguarded. The GitHub App callback then panicked on its first real use:
// GitHub had already created the app, and the panic landed BEFORE the private
// key was sealed. That key is issued once and never reissued, so the app was
// unrecoverable and had to be deleted at GitHub by hand.
//
// The constructor defaults it now, which fixes every path at once rather than
// the one that happened to be found.
func TestTheServiceAlwaysHasAClock(t *testing.T) {
	t.Parallel()
	s := newService(t, setupapi.Options{
		ExternalBase: fixtureExternalBase,
		Company:      companySource(t, &config.Company{}),
	})
	// The flow mints a state, which reads the clock. A nil one panics
	// here rather than in a request nobody can retry.
	flow := s.AppFlow()
	if flow == nil {
		t.Fatal("no app flow")
	}
	if got := flow.InstallURL("nobody"); got != "" {
		t.Errorf("a seat that does not exist has an install URL: %q", got)
	}
}

// A STATE IS SPENT THE FIRST TIME IT IS PRESENTED, and refused afterwards.
//
// Validation is a pure signature-and-expiry check, so without this the state
// is a BEARER CREDENTIAL that works as many times as it is presented for the
// whole fifteen minutes it lives — and it is the only authorization on a
// route the webhooks mux serves unauthenticated. It travels in a query
// string, which is exactly where a browser history and an ingress access log
// keep it.
//
// The company is nil here, so a state that survives the spend fails a step
// LATER with a different error. That is what separates the two answers: the
// first call gets past the spend, the second does not.
func TestACallbackStateIsRefusedTheSecondTime(t *testing.T) {
	t.Parallel()
	flow := newService(t, setupapi.Options{ExternalBase: fixtureExternalBase}).AppFlow()
	state, err := flow.MintState("sre-lead", beganBy)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if _, err := flow.Complete(t.Context(), "code-1", state); errors.Is(err, setupapi.ErrStateRefused) {
		t.Fatalf("the first use of a fresh state was refused: %v", err)
	}
	_, err = flow.Complete(t.Context(), "code-2", state)
	if !errors.Is(err, setupapi.ErrStateRefused) {
		t.Errorf("Complete twice = %v, want %v: a replayed link must not seal "+
			"another app over this seat's", err, setupapi.ErrStateRefused)
	}
}

// AND A REGISTRY THAT CANNOT ANSWER REFUSES, rather than assuming unspent.
//
// This inverts the policy [coord.Claims] documents for webhook dedupe, on
// purpose. A push suppressed by a store blip is a wake nobody notices, so
// that caller fails open; this is an authorization check, and a store that
// could not answer is not evidence the link is unused. Failing open here
// would mean an outage re-opened replay for as long as it lasted.
func TestAnUnreadableClaimRegistryRefusesTheCallback(t *testing.T) {
	t.Parallel()
	flow := newService(t, setupapi.Options{
		ExternalBase: fixtureExternalBase, StateClaims: blindClaims{},
	}).AppFlow()
	state, err := flow.MintState("sre-lead", beganBy)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	_, err = flow.Complete(t.Context(), "code-1", state)
	if !errors.Is(err, setupapi.ErrStateRefused) {
		t.Errorf("Complete = %v, want %v: an unreadable registry has not said "+
			"this state is unspent", err, setupapi.ErrStateRefused)
	}
}

// beganBy is the party a case's state was begun by.
var beganBy = iam.Actor{Name: "jane.doe", Kind: iam.ActorOperator,
	OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}

// blindClaims is a registry that cannot answer.
type blindClaims struct{}

func (blindClaims) ClaimSetup(context.Context, string, time.Time) (bool, error) {
	return false, errors.New("the coordination store could not be reached")
}

// A HANDLE THAT BEGINS WITH A DIGIT STILL GETS A REFERENCEABLE NAME.
//
// The org model accepts `7th-engineer`, and the name built for that seat used
// to lead with the handle: `7TH_ENGINEER_GITHUB_APP_KEY`. envref's
// whole-reference grammar requires a leading letter or underscore, so the
// `${VAR}` written beside it resolved to nothing — and the value it pointed at
// was the app's private key, which GitHub issues once and never reissues. The
// app was unusable and unrecoverable from the moment it was created.
func TestASecretNameIsReferenceableForEverySeatHandle(t *testing.T) {
	t.Parallel()
	for _, handle := range []string{
		"7th-engineer", "1", "sre-lead", "_leading", "n0va", "Ops Lead",
	} {
		for _, field := range []string{"APP_KEY", "APP_WEBHOOK_SECRET"} {
			name := setup.SecretNameFor(integration.KindGitHub, setup.Requirement{
				Field: field, Seat: handle,
			})
			if !setup.ValidSecretName(name) {
				t.Errorf("the name for %q/%s is %q, which no ${VAR} can reference",
					handle, field, name)
			}
		}
	}
}

// AND TWO SEATS NEVER SHARE ONE. These are per-seat credentials: one shared
// name would have the second agent's key overwrite the first's, and both
// seats would then authenticate as whichever app was created last.
func TestTwoSeatsDoNotShareASecretName(t *testing.T) {
	t.Parallel()
	first := setup.SecretNameFor(integration.KindGitHub,
		setup.Requirement{Field: "APP_KEY", Seat: "sre-lead"})
	second := setup.SecretNameFor(integration.KindGitHub,
		setup.Requirement{Field: "APP_KEY", Seat: "platform-lead"})
	if first == second {
		t.Errorf("both seats seal their app key under %q", first)
	}
}
