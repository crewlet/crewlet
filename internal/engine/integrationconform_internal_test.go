package engine

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/setup"
)

// convergedPass is a vendor whose world already matches the company: nothing
// outstanding, so nothing to write. It counts the writes it WOULD make: a
// converged pass is the state the loop spends its life in, and a stray write
// there is a credential rotated every ten minutes for ever.
type convergedPass struct {
	kind   integration.Kind
	writes int

	// findings is what this pass reports. Empty is the converged world;
	// the outstanding one carries something a person owes, which is what
	// the suite's findings clauses need in front of them — over a converged
	// world they walk an empty list and certify nothing.
	findings []integration.Finding
}

func (p *convergedPass) Kind() integration.Kind  { return p.kind }
func (*convergedPass) Needs() *setup.Requirement { return nil }

func (p *convergedPass) Run(ctx context.Context, _ setup.PassInput) ([]integration.Finding, error) {
	// A CANCELLED PASS IS A FAULT, not health: the suite drives this case,
	// and a vendor that ignored its context would report a converged
	// surface it never actually read.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.findings, nil
}

// THE ADAPTER'S OWN HALF OF THE CONTRACT, and only that half: the stub pass
// below writes nothing by construction, so integrationtest's contract binds in
// each third-party app's own package, where a world can be stood up converged
// and its writes counted. The adapter (passConverger) is on every pass's path,
// so its own behaviour is a contract no vendor harness exercises: the kind it
// reports, that the kind does not move across passes, that it adds no writes,
// and that it does not turn a cancelled context into a clean report.
func TestTheAdapterMeetsTheContract(t *testing.T) {
	t.Parallel()
	var pass *convergedPass
	adapter := func(findings ...integration.Finding) func(integrationtest.TB) integration.Reconciler {
		return func(integrationtest.TB) integration.Reconciler {
			pass = &convergedPass{kind: integration.KindGitLab, findings: findings}
			e := sealingNode(t)
			e.epoch.current.Store(companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    gateway:
      model: sonnet
`))
			return &passConverger{pass: pass, engine: e}
		}
	}
	integrationtest.Run(t, integrationtest.Reconciler{
		Converged: adapter(),
		// WHAT THE ADAPTER IS ON THE HOOK FOR HERE is that it hands a
		// vendor's findings BACK unchanged. It sits between every pass and
		// the loop, so a finding it dropped, reordered or re-kinded would
		// be one no vendor harness could ever notice missing.
		Outstanding: adapter(integration.Finding{
			Kind: integration.FindingGrantShort, Subject: "ceo",
			Detail: "ceo holds reporter on the platform group and its role asks for maintainer",
		}),
		Mutations: func() int { return pass.writes },
	})
}

// EVERY SURFACE THE LOOP KNOWS HAS AN ANSWER IN THE DOCUMENT TEST.
//
// [config.Company.DeclaresIntegration] is keyed on the surface's string so the
// config tier does not import integration, so only here, with both in scope,
// can a [integration.Kind] added there and forgotten here be noticed. An
// unknown surface deliberately answers TRUE (the caller deletes a status row on
// false, and an older node in a rolling upgrade must not erase a newer node's),
// so over the empty company a kind that fell through looks configured, and
// fails here rather than keeping a row alive for ever.
func TestEverySurfaceHasADocumentTest(t *testing.T) {
	t.Parallel()
	empty := companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    gateway:
      model: sonnet
`)
	for _, kind := range integration.Kinds {
		if empty.Config.DeclaresIntegration(kind.String()) {
			t.Errorf("a company that configures nothing declares %s, so it fell "+
				"through to the unknown-surface default: add it to "+
				"config.Company.DeclaresIntegration", kind)
		}
	}
}
