package engine

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/setup"
)

// convergedPass is a vendor whose world already matches the company: nothing
// outstanding, so nothing to write.
//
// It counts the writes it WOULD make, which is the clause the suite exists
// for. A pass over a company that needs work is allowed to write; a pass over
// one that does not is the state the loop spends its life in, and a stray
// write there is a credential rotated every ten minutes for ever.
type convergedPass struct {
	kind   integration.Kind
	writes int
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
	return nil, nil
}

// THE ADAPTER'S OWN HALF OF THE CONTRACT, and only that half.
//
// This used to claim to be where integrationtest's contract binds, on the
// reasoning that passConverger is the one implementation in the tree and every
// surface reaches the loop through it. That reasoning is true and the
// conclusion was wrong: the pass it was pointed at is the stub below, whose
// Run returns (nil, nil), so "a converged pass writes nothing" — the clause the
// whole suite exists for — was true because nothing happened. Seven real
// reconcilers went uncertified while a green test said otherwise, which is the
// worst state a suite can be in.
//
// The contract binds in each third-party app's own package now, where a world
// can be stood up converged and its writes counted. What is left here is worth
// keeping and is worth being honest about: the ADAPTER is on the path of every
// pass, so its own behaviour — the kind it reports, that the kind does not move
// across passes, that it adds no writes of its own, that it does not turn a
// cancelled context into a clean report — is a contract too, and one no vendor
// harness exercises.
func TestTheAdapterMeetsTheContract(t *testing.T) {
	t.Parallel()
	var pass *convergedPass
	integrationtest.Run(t, integrationtest.Reconciler{
		New: func(integrationtest.TB) integration.Reconciler {
			pass = &convergedPass{kind: integration.KindGitLab}
			e := &Engine{}
			e.epoch.current.Store(companyFor(t, `
name: Acme
providers:
  llm:
    gateway:
      type: openai
      model: gpt-4o
      api_keys: ["${OPENAI_API_KEY}"]
`))
			return &passConverger{pass: pass, engine: e}
		},
		Mutations: func() int { return pass.writes },
	})
}

// EVERY SURFACE THE LOOP KNOWS HAS AN ANSWER IN THE DOCUMENT TEST.
//
// [config.Company.DeclaresIntegration] is keyed on the surface's own string so
// the config tier does not import integration, which means nothing in either
// package can notice a [integration.Kind] added there and forgotten here. This
// is where both are in scope, so this is where it is checked.
//
// The empty company is what makes it work. An unknown surface deliberately
// answers TRUE — the caller deletes a status row on false, and an older node in
// a rolling upgrade must not erase a newer node's — so a kind that fell through
// to that default would look configured in a company that configures nothing.
// A kind nobody handled therefore fails here rather than quietly keeping a row
// alive for ever.
func TestEverySurfaceHasADocumentTest(t *testing.T) {
	t.Parallel()
	empty := companyFor(t, `
name: Acme
providers:
  llm:
    gateway:
      type: openai
      model: gpt-4o
      api_keys: ["${OPENAI_API_KEY}"]
`)
	for _, kind := range integration.Kinds {
		if empty.Config.DeclaresIntegration(kind.String()) {
			t.Errorf("a company that configures nothing declares %s, so it fell "+
				"through to the unknown-surface default: add it to "+
				"config.Company.DeclaresIntegration", kind)
		}
	}
}
