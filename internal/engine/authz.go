package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
)

// ChartAuthority answers [authz.Chart] against the epoch this node is running.
//
// No company (booting, before the first revision, or installing one) is an
// ERROR, never "you do not lead them": [authz.Decide] turns it into an UNKNOWN
// decision, which a surface answers 503 rather than 403, so a lagging node does
// not lock a lead out of their own project. The admin path is checked before
// the chart, so an admin is never told "I cannot tell". The chart is read PER
// CALL, because the writer outlives a revision; the relations are
// internal/authz/orgchart's.
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

// MemberOf reports whether actor's seat sits in the unit key names or any unit
// below it, in the running chart.
func (c ChartAuthority) MemberOf(ctx context.Context, actor, unitKey string) (bool, error) {
	chart, err := c.chart()
	if err != nil {
		return false, err
	}
	return chart.MemberOf(ctx, actor, unitKey)
}

// SeatGranted reports whether actor's seat's role carries grant in the running
// chart.
func (c ChartAuthority) SeatGranted(ctx context.Context, actor string, grant authz.SeatGrant) (bool, error) {
	chart, err := c.chart()
	if err != nil {
		return false, err
	}
	return chart.SeatGranted(ctx, actor, grant)
}

// chart is the running company's chart, or why this node cannot answer: in one
// place, so no method can collapse "no chart" into `false`.
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
