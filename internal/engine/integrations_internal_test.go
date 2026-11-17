package engine

import (
	"context"
	"errors"
	"testing"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

// sealingNode is an Engine holding what every node [New] builds holds for the
// integration loop: a fleet store, and the keyring the loop's sink seals
// under.
func sealingNode(t *testing.T) *Engine {
	t.Helper()
	_, cipher := testKeyring(t)
	return &Engine{backends: &Backends{Fleet: coordmem.NewFleet()}, cipher: cipher}
}

// countingPass records whether the loop actually ran it.
type countingPass struct {
	kind integration.Kind
	runs int
}

func (p *countingPass) Kind() integration.Kind  { return p.kind }
func (*countingPass) Needs() *setup.Requirement { return nil }

func (p *countingPass) Run(context.Context, setup.PassInput) ([]integration.Finding, error) {
	p.runs++
	return nil, nil
}

// A NODE WITH NO ACTIVE REVISION CONVERGES NOTHING: it answers the sentinel
// the loop forgets a row on (the ordinary boot posture, not a fault), and the
// pass never runs, having no document to converge.
func TestTheLoopConvergesNothingWithoutACompany(t *testing.T) {
	t.Parallel()
	pass := &countingPass{kind: integration.KindGitLab}
	c := &passConverger{pass: pass, engine: &Engine{}}

	findings, err := c.Reconcile(t.Context())

	if !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("Reconcile = %v, want %v", err, integration.ErrNotConfigured)
	}
	if findings != nil {
		t.Errorf("findings = %v, want none", findings)
	}
	if pass.runs != 0 {
		t.Errorf("the pass ran %d times with no company to converge", pass.runs)
	}
}

// A PASS THAT CANNOT BE HANDED A SEALING SINK DOES NOT RUN, AND IS A FAULT:
// every node holds the keyring, so an unbuildable sink is an engine wired
// without its secret store, recorded and retried, never a state to converge
// in with nothing sealed.
func TestAPassWithNoSinkToSealIntoIsAFault(t *testing.T) {
	t.Parallel()
	pass := &countingPass{kind: integration.KindGitLab}
	e := &Engine{}
	e.epoch.current.Store(&Company{})
	c := &passConverger{pass: pass, engine: e}

	findings, err := c.Reconcile(t.Context())

	if err == nil || errors.Is(err, integration.ErrNotConfigured) {
		t.Fatalf("Reconcile = (%v, %v), want the fault an unbuildable sink is",
			findings, err)
	}
	if pass.runs != 0 {
		t.Errorf("the pass ran %d times with nowhere to seal what it mints", pass.runs)
	}
}

// capturingPass records what the loop handed it.
type capturingPass struct {
	kind integration.Kind
	in   setup.PassInput
}

func (p *capturingPass) Kind() integration.Kind  { return p.kind }
func (*capturingPass) Needs() *setup.Requirement { return nil }

func (p *capturingPass) Run(_ context.Context, in setup.PassInput) ([]integration.Finding, error) {
	p.in = in
	return nil, nil
}

// THE LOOP HANDS A PASS THE VENDORS' ADDRESS AND THE KEY IT IS READ FROM, so
// a pass with no address reports `ingress_blocked` naming the right setting:
// behind a public listener `api.public.external_url`, not `api.external_url`,
// which only the engine knows.
func TestTheLoopHandsAPassTheVendorsAddressAndItsKey(t *testing.T) {
	t.Parallel()
	pass := &capturingPass{kind: integration.KindDatadog}
	e := sealingNode(t)
	e.epoch.current.Store(&Company{})
	e.webhookBase, e.webhookBaseKey = "https://hooks.example.com", "api.public.external_url"
	c := &passConverger{pass: pass, engine: e}

	if _, err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if pass.in.WebhookBase != e.webhookBase {
		t.Errorf("the pass was handed the address %q, want %q", pass.in.WebhookBase, e.webhookBase)
	}
	if pass.in.WebhookBaseKey != e.webhookBaseKey {
		t.Errorf("the pass was handed the key %q, want %q", pass.in.WebhookBaseKey, e.webhookBaseKey)
	}
}

// AND THE KIND IT REPORTS IS THE PASS'S OWN, which is what keys every status
// row the loop writes.
func TestTheConvergerReportsThePassesKind(t *testing.T) {
	t.Parallel()
	c := &passConverger{pass: &countingPass{kind: integration.KindDatadog}, engine: &Engine{}}
	if got := c.Kind(); got != integration.KindDatadog {
		t.Errorf("Kind() = %q, want %q", got, integration.KindDatadog)
	}
}

// WHICH SURFACES A PASS CONVERGES IS A PROPERTY OF THE BUILD, answered on a
// node with no coordination store as on any other: the roll-up reads it to
// tell a surface awaiting its first report from one that will never get one.
// Every surface has a pass.
func TestConvergesIsTheBuildsPassList(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	for _, kind := range integration.Kinds {
		if !e.Converges(kind) {
			t.Errorf("Converges(%s) = false, want every surface converged", kind)
		}
	}
	if e.Converges("forge") {
		t.Error("the Forge relay is a delivery path no pass converges")
	}
}
