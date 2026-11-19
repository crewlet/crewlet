package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/advisor/duty"
)

// advisorDutyName is the Advisor's lease, `worker:advisor` (ADR-0164), and
// advisorDutyTTL how long a claim lasts unrenewed: three of the duty's ticks.
const (
	advisorDutyName = "advisor"
	advisorDutyTTL  = duty.LeaseTTL
)

// AdvisorClaim claims or renews the Advisor's lease for this node, nil on a
// node with no fleet, which always holds it. A HOLD, gated on the coordination
// store and never on the worker role: the duty runs where the API serves and
// reads through its sources, so a claim on the role would leave a fleet whose
// API nodes run no workers with no Advisor. Renewed every tick and never given
// back, so the duty stays on one node and moves within its TTL when that stops.
func (e *Engine) AdvisorClaim() func(context.Context) (bool, error) {
	hold := e.workerHold(advisorDutyName, advisorDutyTTL)
	if hold == nil {
		return nil
	}
	return func(ctx context.Context) (bool, error) {
		_, held, err := hold(ctx)
		return held, err
	}
}
