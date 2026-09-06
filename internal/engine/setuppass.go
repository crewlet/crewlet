package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// Running a vendor's provisioning from the API rather than from a shell.
//
// # This is where the loop's two deliberate absences are filled in
//
// The reconcile loop hands every vendor a pass with no sink and no webhook
// base, and states plainly why: a base is permission to register a hook and a
// sink is permission to mint a credential, and neither is a decision a timer
// gets to make. A pass here supplies both, because a person asked for it.
//
// The vendor function is the SAME one, in every case. Nothing about
// provisioning is reimplemented for the API: what an adapter does is resolve
// the config, build the client, and hand over the sink and the base the loop
// withholds.

// setupPasses are the vendors this build can provision over the API.
//
// GitHub first, and alone for now, because it is the vendor whose pass
// neither creates an account nor issues a credential at the vendor: it reads
// the organization, mints a webhook secret that is the ENGINE's own value on
// both ends, and registers a hook. Every other pass creates something a
// person owns, and putting one of those behind a button is a separate
// decision (see the note on the reconcile loop's registrations).
func (e *Engine) setupPasses() []setup.Pass {
	return []setup.Pass{&githubPass{engine: e}}
}

// SetupRunner is the pass runner this node serves, or nil when it has no
// secret store to mint into.
//
// Nil rather than a runner that refuses: a pass that cannot record what it
// mints must not run at all, and the surface above answers 503 naming the
// keyring rather than starting something it will have to unwind.
func (e *Engine) SetupRunner(now func() time.Time) *setup.Runner {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return setup.NewRunner(e.setupPasses(), e.setupDuty, now)
}

// setupDuty is the fleet lease one vendor's pass holds while it runs.
//
// The SAME mechanism the reconcile loop's singleton uses, under its own name,
// so a pass and a loop tick for one vendor never overlap either. The TTL is
// generous relative to a pass: a lease that expired mid-run would let a
// second node start minting while the first was still writing.
func (e *Engine) setupDuty(kind integration.Kind) setup.Duty {
	duty := e.workerDuty("setup-provision-"+string(kind), setupLeaseTTL)
	if duty == nil {
		return nil
	}
	return setup.Duty(duty)
}

// setupLeaseTTL bounds how long one pass may hold its vendor.
//
// Five minutes against passes measured in seconds: the value is a backstop
// for a node that died mid-run, not a deadline for the work. Shorter would
// risk a live pass losing its lease; much longer would leave a vendor locked
// out after a crash for no benefit.
const setupLeaseTTL = 5 * time.Minute

// sinkFor is the recorder a pass writes minted credentials through.
//
// The SAME type `crewlet <vendor> provision -secret-store` builds, so a
// credential minted from the dashboard and one minted from a shell land in
// the same place under the same envelope. Write-through, so a value is
// durable before the next one is minted.
func (e *Engine) SetupSink(operator string) (provision.TokenSink, error) {
	if e.cipher == nil {
		return nil, fmt.Errorf("engine: this node has no keyring, so a minted credential cannot be sealed")
	}
	return provision.NewSecretStoreSink(
		fleetsecrets.New(e.backends.Fleet, e.cipher), operator), nil
}

// githubPass adapts github.Reconcile to the pass contract.
type githubPass struct{ engine *Engine }

func (*githubPass) Kind() integration.Kind { return integration.KindGitHub }

// Needs is nil: GitHub issues nothing on a provisioner's behalf, so this pass
// asks for no transient administrator credential. What it writes at the
// vendor, it writes with the organization token already in the config.
func (*githubPass) Needs() *setup.Requirement { return nil }

func (p *githubPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	company := p.engine.Company()
	cfg := company.Config.Integrations.GitHub
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	env := p.engine.resolver()
	client, err := githubReconcileClient(cfg, env)
	if err != nil {
		return nil, fmt.Errorf("engine: github pass: %w", err)
	}
	res, err := github.Reconcile(ctx, github.Options{
		Client: client, Config: cfg, Org: company.Org, Value: env.Value,
		// THE TWO THE LOOP WITHHOLDS. A base is permission to register,
		// a sink is permission to mint, and a person asked for both.
		Sink:             in.Sink,
		WebhookBase:      in.WebhookBase,
		RecreateWebhooks: in.Recreate,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: github pass: %w", err)
	}
	return res.Findings(), nil
}
