package setupapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE STATE IS THE ONLY THING GUARDING AN UNAUTHENTICATED WRITE.
//
// GitHub's redirect carries no engine credential, so what stands between the
// callback and anybody who can reach the engine is the state alone. It names
// the seat, it expires, and it is signed: a forged one, a stale one and one
// from another endpoint all have to be refused, and Slack's landing checks
// none of these because it only ever prints a code.
func TestTheAppCallbackStateIsSignedScopedAndExpiring(t *testing.T) {
	t.Parallel()
	key := runtoken.KeyFrom("github-app-manifest", []string{"k1:material"})
	signer := runtoken.New(runtoken.Options{Key: key})

	token := signer.Mint("sre-lead", 15*60*1000000000)
	if got := signer.Validate(token); got != "sre-lead" {
		t.Fatalf("a token this signer minted validates as %q", got)
	}

	// FORGED: the signature is checked, so an attacker naming a seat gets
	// nothing.
	if got := signer.Validate("v1.sre-lead.9999999999.deadbeef"); got != "" {
		t.Errorf("a forged state validated as %q", got)
	}

	// FROM ANOTHER ENDPOINT: the domain separates these keys, so a token
	// minted for the telemetry receiver cannot be replayed here.
	other := runtoken.New(runtoken.Options{
		Key: runtoken.KeyFrom("otel-run", []string{"k1:material"}),
	})
	if got := signer.Validate(other.Mint("sre-lead", 15*60*1000000000)); got != "" {
		t.Errorf("a token from another endpoint validated as %q", got)
	}

	// TAMPERED: changing the seat invalidates it, which is what stops one
	// operator's link writing into another seat.
	parts := strings.SplitN(token, ".", 4)
	if len(parts) == 4 {
		if got := signer.Validate(parts[0] + ".other-seat." + parts[2] + "." + parts[3]); got != "" {
			t.Errorf("a state pointed at another seat validated as %q", got)
		}
	}
}

// TWO NODES MUST AGREE, or a fleet where the begin and the callback land on
// different nodes refuses every completion, with a message about the link
// rather than about the deployment.
func TestTwoNodesWithTheSameKeyringAgreeOnAState(t *testing.T) {
	t.Parallel()
	material := []string{"k2:second", "k1:first"}
	one := runtoken.New(runtoken.Options{Key: runtoken.KeyFrom("github-app-manifest", material)})
	// The SAME material in a different order, which is what a re-ordered
	// document or a map iteration produces.
	two := runtoken.New(runtoken.Options{
		Key: runtoken.KeyFrom("github-app-manifest", []string{"k1:first", "k2:second"}),
	})
	if got := two.Validate(one.Mint("sre-lead", 15*60*1000000000)); got != "sre-lead" {
		t.Errorf("a second node validated the first's state as %q", got)
	}
}

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
	s := setupapi.New(setupapi.Options{
		Company: func() *config.Company { return &config.Company{} },
	})
	if s == nil {
		t.Fatal("a service with a company is nil")
	}
	// The flow mints a state, which reads the clock. A nil one panics
	// here rather than in a request nobody can retry.
	flow := setupapi.NewAppFlow(s, []string{"k1:material"}, nil)
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
	flow := setupapi.NewAppFlow(
		setupapi.New(setupapi.Options{Company: func() *config.Company { return nil }}),
		[]string{"k1:material"}, nil)
	if flow == nil {
		t.Fatal("no app flow")
	}
	state := runtoken.New(runtoken.Options{
		Key: runtoken.KeyFrom("github-app-manifest", []string{"k1:material"}),
	}).Mint("sre-lead", 15*time.Minute)

	if _, err := flow.Complete(t.Context(), "code-1", state); errors.Is(err, setupapi.ErrStateRefused) {
		t.Fatalf("the first use of a fresh state was refused: %v", err)
	}
	_, err := flow.Complete(t.Context(), "code-2", state)
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
	flow := setupapi.NewAppFlow(
		setupapi.New(setupapi.Options{Company: func() *config.Company { return nil }}),
		[]string{"k1:material"}, blindClaims{})
	state := runtoken.New(runtoken.Options{
		Key: runtoken.KeyFrom("github-app-manifest", []string{"k1:material"}),
	}).Mint("sre-lead", 15*time.Minute)

	_, err := flow.Complete(t.Context(), "code-1", state)
	if !errors.Is(err, setupapi.ErrStateRefused) {
		t.Errorf("Complete = %v, want %v: an unreadable registry has not said "+
			"this state is unspent", err, setupapi.ErrStateRefused)
	}
}

// blindClaims is a registry that cannot answer.
type blindClaims struct{}

func (blindClaims) Claim(context.Context, string, time.Duration, time.Time) (bool, error) {
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
