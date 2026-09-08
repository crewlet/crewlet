package provision_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/provision"
)

// A REFUSAL NAMES THE SHAPE AND NEVER THE VALUE.
//
// Every value [provision.SoleVar] refuses is a credential or a string holding
// one, and the sentence explaining the refusal travels: into a report an
// operator pastes into a ticket, and — since the reconcile loop — into
// `State.LastError`, which is written to the fleet's coordination store and
// served on the integrations query, a read anonymous by default. So the one
// thing this may never do is repeat what it was given.
func TestTheShapeOfAValueNeverRepeatsIt(t *testing.T) {
	t.Parallel()
	secrets := []string{
		"whsec_aRealLookingSigningSecretValue",
		"glpat-notARealTokenButShapedLikeOne",
		"https://user:hunter2@wiki.example.com",
	}
	for _, secret := range secrets {
		if got := provision.Shape(secret); strings.Contains(got, secret) {
			t.Errorf("Shape(%q) = %q, and it repeats the value", secret, got)
		}
	}
}

// AND IT KEEPS THE TWO CASES APART, because their fixes differ: a literal
// needs a variable, a composite needs the variable to be the whole value.
// Collapsed into "not a reference", an operator has to work out which.
func TestTheShapeTellsALiteralFromAComposite(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]string{
		"whsec_literal":                 "a literal",
		"":                              "a literal",
		"${WEBHOOK_SECRET}-with-a-tail": "a reference embedded in other text",
		"https://${HOST}/wiki":          "a reference embedded in other text",
	} {
		if got := provision.Shape(value); got != want {
			t.Errorf("Shape(%q) = %q, want %q", value, got, want)
		}
	}
}
