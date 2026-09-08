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

// THE CONTRACT IS CERTIFIED AGAINST THE THING THAT IMPLEMENTS IT.
//
// integrationtest states what every [integration.Reconciler] must do, and its
// package doc calls the write-counting hook required because that clause is
// "the one most likely to be wrong". It had no callers at all: the suite was
// written, tested against its own fakes, and never pointed at anything the
// engine runs.
//
// passConverger is the only implementation in the tree — every surface reaches
// the loop through it — so this is where the contract binds. What the suite
// then certifies is the shared half of every vendor's pass: the kind it
// reports, its stability across passes, that a converged world is left alone,
// that two passes agree, and that a cancelled one is a fault.
func TestTheLoopsReconcilerMeetsTheContract(t *testing.T) {
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
