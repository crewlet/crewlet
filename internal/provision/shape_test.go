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
// Collapsed into "not a reference", an operator has to work out which. A slot
// holding nothing is neither, and says so: called "a literal", an unset field
// sent its operator looking for a value that is not there.
func TestTheShapeTellsALiteralFromAComposite(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]string{
		"whsec_literal":                 "a literal",
		"":                              "empty",
		"  ":                            "empty",
		"${WEBHOOK_SECRET}-with-a-tail": "a reference embedded in other text",
		"https://${HOST}/wiki":          "a reference embedded in other text",
	} {
		if got := provision.Shape(value); got != want {
			t.Errorf("Shape(%q) = %q, want %q", value, got, want)
		}
	}
}

// A SWITCH FOR A FRESH VALUE OVER A VALUE THE DOCUMENT OWNS IS REFUSED NAMING
// THE SWITCH, AS ITS CALLER SPELLS IT, AND BOTH WAYS ON.
//
// The document itself holds the webhook secret (a literal, or a reference
// inside other text), so there is no variable to mint a fresh one into. The
// refusal for a slot that resolved to nothing would say the value had not
// resolved, untrue of a literal, and omit that running without the switch
// registers the value as it is. It names the shape, never the value, and the
// switch as its surface spells it (the flag, or the setup route's body field).
func TestAFreshValueOverOneTheDocumentOwnsNamesTheSwitchAndBothWaysOn(t *testing.T) {
	t.Parallel()
	const path = "integrations.github.webhook_secret"
	for _, fresh := range []provision.FreshSwitch{"-recreate-webhooks", "recreate_webhooks"} {
		for value, shape := range map[string]string{
			"whsec_aLiteralNobodyShouldEverSee": "a literal",
			"prefix-${GITHUB_WEBHOOK_SECRET}":   "a reference embedded in other text",
		} {
			err := provision.RefuseFresh(path, fresh, value)
			if err == nil {
				t.Errorf("RefuseFresh(%s) over %s = nil, want a refusal", fresh, shape)
				continue
			}
			msg := err.Error()
			for _, want := range []string{
				path, shape,
				// The switch that was set, and why it cannot act.
				string(fresh) + " has no variable to mint a fresh one into",
				// The two ways on: give the switch a variable, or drop it.
				"point " + path + " at a whole ${VAR} and re-run with " + string(fresh),
				"run without " + string(fresh) + " to register the value it holds as it is",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal of %s over %s does not say %q:\n%s",
						fresh, shape, want, msg)
				}
			}
			if strings.Contains(msg, value) {
				t.Errorf("the refusal repeats the value:\n%s", msg)
			}
		}
	}
	// THE ROUTE'S SPELLING NAMES NOTHING OF THE COMMAND LINE'S: a refusal
	// of the body field telling its caller to re-run with a flag is the
	// one this switch carries its spelling to prevent.
	if err := provision.RefuseFresh(path, "recreate_webhooks",
		"whsec_aLiteralNobodyShouldEverSee"); err == nil ||
		strings.Contains(err.Error(), "-recreate") {
		t.Errorf("the route's refusal names a command-line flag: %v", err)
	}
	// NOTHING OF THE KIND TO REFUSE: a whole reference is where the fresh
	// value goes, and an empty slot is the caller's own refusal to make,
	// since no switch changes what an empty slot lacks.
	for _, value := range []string{"${GITHUB_WEBHOOK_SECRET}", " ${GITHUB_WEBHOOK_SECRET} ", "", "  "} {
		if err := provision.RefuseFresh(path, "-recreate-webhooks", value); err != nil {
			t.Errorf("RefuseFresh(%q) = %v, want nil", value, err)
		}
	}
	// AND A RUN THAT DID NOT ASK has nothing of the kind to refuse either,
	// whatever the slot holds: it registers a literal as it is.
	if err := provision.RefuseFresh(path, "", "whsec_aLiteralNobodyShouldEverSee"); err != nil {
		t.Errorf("RefuseFresh with no switch = %v, want nil", err)
	}
}
