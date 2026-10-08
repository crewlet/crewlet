package engine

import (
	"context"
	"os"
	"path/filepath"
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

// A SEAT READING A PERSON READS THEM UNDER BOTH OF THEIR NAMES. A change filed
// by somebody through the dashboard concerns them under the credential their
// seat binds, and the applier writes that person's notice under it — so a
// seat's get_person or work_inbox about them, the handle alone, never saw it.
// The seat surface answers the party as the operator surface does, with the
// binding resolved through this node's own chain: the variable here is set in
// no process, only in the environment the engine was handed.
func TestASeatReadsAPersonUnderBothOfTheirNames(t *testing.T) {
	t.Parallel()
	const variable = "CREWLET_ENGINE_TEST_BOUND_FOUNDER"
	if _, set := os.LookupEnv(variable); set {
		t.Fatalf("the premise: %s is set in no process", variable)
	}
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: Founder
    kind: human
    contact:
      crewlet_operator_id: ${` + variable + `}
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	SeedStore(t, &b)
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg,
		Environment: config.MapSource{variable: "founder-token"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	deps := e.workDeps(e.Company())
	if deps.Reader == nil {
		t.Fatal("the node has no native tracker, so this case shows nothing")
	}
	if deps.Party == nil {
		t.Fatal("a seat's work tools read a person under their handle alone")
	}
	if got := deps.Party("founder"); got.Handle != "founder" || got.OperatorID != "founder-token" {
		t.Errorf("the founder's party is %+v, want the seat and the credential the "+
			"handed environment binds to it", got)
	}
}
