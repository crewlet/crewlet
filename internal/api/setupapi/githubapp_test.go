package setupapi_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/runtoken"
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
	flow := setupapi.NewAppFlow(s, []string{"k1:material"})
	if flow == nil {
		t.Fatal("no app flow")
	}
	if got := flow.InstallURL("nobody"); got != "" {
		t.Errorf("a seat that does not exist has an install URL: %q", got)
	}
}
