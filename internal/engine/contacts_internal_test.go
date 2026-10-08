package engine

import (
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// AN EPOCH RESOLVES ITS ROSTER'S CONTACTS THROUGH THE RESOLVER IT WAS BUILT
// WITH — on a running node its own chain, the secret store and then the
// environment it was handed — and never the process environment behind it, so
// a teammate's id reads in a seat's prompt as notification routing reads it.
// [Company.RunnerFor] hands this lookup to the prompt builder's roster.
func TestAnEpochResolvesItsRosterThroughTheResolverItWasBuiltWith(t *testing.T) {
	t.Parallel()
	const variable = "CREWLET_ENGINE_TEST_EPOCH_CONTACT"
	if _, set := os.LookupEnv(variable); set {
		t.Fatalf("the premise: %s is set in no process", variable)
	}
	cfg, err := config.ParseCompany([]byte(`
name: Acme
roles:
  - name: CEO
    handle: ceo
  - name: Founder
    kind: human
    contact:
      slack_user_id: ${` + variable + `}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	built, err := NewCompanyWith(cfg, config.NewResolver(config.MapSource{variable: "U0HANDED"}))
	if err != nil {
		t.Fatalf("NewCompanyWith: %v", err)
	}
	if got, ok := built.contacts(variable); !ok || got != "U0HANDED" {
		t.Errorf("the roster resolves %s to %q (%v), want the value of the resolver "+
			"the epoch was built with", variable, got, ok)
	}
	// THE CONTROL: an epoch built from the environment alone — `crewlet
	// validate`'s — reads the process's, which has no such variable.
	plain, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	if got, ok := plain.contacts(variable); ok {
		t.Errorf("an environment-only epoch resolved %s to %q", variable, got)
	}
}
