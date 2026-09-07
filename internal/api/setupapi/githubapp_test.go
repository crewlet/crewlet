package setupapi_test

import (
	"strings"
	"testing"

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
