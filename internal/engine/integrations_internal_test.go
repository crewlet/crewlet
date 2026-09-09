package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
)

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

// AND THE KIND IT REPORTS IS THE PASS'S OWN, which is what keys every status
// row the loop writes.
func TestTheConvergerReportsThePassesKind(t *testing.T) {
	t.Parallel()
	c := &passConverger{pass: &countingPass{kind: integration.KindDatadog}, engine: &Engine{}}
	if got := c.Kind(); got != integration.KindDatadog {
		t.Errorf("Kind() = %q, want %q", got, integration.KindDatadog)
	}
}
