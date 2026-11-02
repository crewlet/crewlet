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
// under. A harness that left them out used to be handed a read-only sink; now
// it is handed the fault an engine with no secret store is.
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

// A NODE WITH NO ACTIVE REVISION CONVERGES NOTHING, and says so with the
// sentinel the loop already handles.
//
// This is the ordinary boot posture, not an error: a node runs with no company
// before one is ever imported, and keeps running when the fleet's revision
// cannot be read. Every surface is then UNCONFIGURED, which makes the loop
// forget the row rather than record a fault against a company that does not
// exist — and the pass must not run at all, because there is no document to
// tell it what to converge.
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

// A PASS THAT CANNOT BE HANDED A SEALING SINK DOES NOT RUN, AND SAYS WHY AS A
// FAULT.
//
// The loop used to fall back to a read-only sink here, for a node with no
// keyring, and every pass then reported that posture as the operator's work.
// There is no such node now: every node holds the keyring its sink seals
// under, so a sink that cannot be built is an engine wired without its secret
// store — a fault the loop records and retries, never a state to converge in
// with nothing sealed.
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

// THE LOOP HANDS A PASS THE VENDORS' ADDRESS AND THE KEY IT IS READ FROM.
//
// A pass with no address reports `ingress_blocked` naming the setting to fill
// in, and only the engine knows which one that is: behind a public listener it
// is `api.public.external_url`, which every vendor used to name as
// `api.external_url` — a key that was set, that nothing building a vendor URL
// reads, on a deployment whose webhooks are served only on the other socket.
// So the key travels beside the address, from the same Tier A block.
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
// node with no coordination store as on any other. The roll-up reads it to
// tell a surface waiting for its first report from one that will never get
// one. Every surface is the first: Slack's pass converges its agents' apps.
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
