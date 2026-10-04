package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
)

// ChartAuthority answers [authz.Chart] against the epoch this node is running.
//
// # Why it is three-valued, and what the two-valued one cost
//
// This replaces `LeadsProjectOf`, whose seam was `func(ctx, actor, project)
// bool` and whose body opened with:
//
//	c := e.Company()
//	if c == nil || c.Org == nil { return false }
//
// A node holds no company while it is booting, before its first revision is
// applied, and while it is installing one. Every one of those moments
// answered "you do not lead this project" about a person who leads it
// — so a lead was locked out of their own project's policy by a node that was
// merely lagging, the refusal named the project rather than the lag, and the
// node reported itself healthy throughout. The two facts are opposite and were
// wearing one value.
//
// So absent is an ERROR here. [authz.Decide] turns it into an UNKNOWN
// decision, which a surface answers 503 to rather than 403, and the admin
// path is checked before the chart is asked at all — an operator holding the
// deployment's own grant is never told "I cannot tell".
//
// PER CALL, which is the one thing the old seam got right: the writer outlives
// a revision, and a chart captured once would answer for a company that has
// since moved. The relations themselves are internal/authz/orgchart's, over
// whichever tree this node is running at the call.
type ChartAuthority struct{ engine *Engine }

var _ authz.Chart = ChartAuthority{}

// ChartAuthorityOf is the seam over one engine.
func ChartAuthorityOf(e *Engine) ChartAuthority { return ChartAuthority{engine: e} }

// Leads reports whether actor is above subject in the running chart.
func (c ChartAuthority) Leads(ctx context.Context, actor, subject string) (bool, error) {
	chart, err := c.chart()
	if err != nil {
		return false, err
	}
	return chart.Leads(ctx, actor, subject)
}

// LeadsAnyone reports whether actor leads any seat in the running chart.
func (c ChartAuthority) LeadsAnyone(ctx context.Context, actor string) (bool, error) {
	chart, err := c.chart()
	if err != nil {
		return false, err
	}
	return chart.LeadsAnyone(ctx, actor)
}

// LeadsProject reports whether actor leads the unit that owns a project, or is
// the seat whose own project it is, in the running chart.
func (c ChartAuthority) LeadsProject(ctx context.Context, actor, project string) (bool, error) {
	chart, err := c.chart()
	if err != nil {
		return false, err
	}
	return chart.LeadsProject(ctx, actor, project)
}

// LeadsContainer reports whether actor leads the unit that owns a page
// container, or is the seat whose own container it is, in the running chart.
func (c ChartAuthority) LeadsContainer(ctx context.Context, actor, container string) (bool, error) {
	chart, err := c.chart()
	if err != nil {
		return false, err
	}
	return chart.LeadsContainer(ctx, actor, container)
}

// LeadsUnit reports whether actor leads the unit key names, directly or from
// anywhere above it, in the running chart.
func (c ChartAuthority) LeadsUnit(ctx context.Context, actor, unitKey string) (bool, error) {
	chart, err := c.chart()
	if err != nil {
		return false, err
	}
	return chart.LeadsUnit(ctx, actor, unitKey)
}

// chart is the running company's chart, or why this node cannot answer.
//
// ONE PLACE, because every method needs it and the whole point of this type is
// that "no chart" is never a `false` — written once per method, one of them
// eventually returns the zero value and the collapse is back.
func (c ChartAuthority) chart() (authz.Chart, error) {
	if c.engine == nil {
		return nil, fmt.Errorf("engine: %w", authz.ErrNoChart)
	}
	company := c.engine.Company()
	if company == nil || company.Org == nil {
		return nil, fmt.Errorf("engine: this node is not running a company yet, "+
			"so it cannot say who leads whom: %w", authz.ErrNoChart)
	}
	return orgchart.Of(company.Org), nil
}
