package queries

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/integration"
)

// advisor answers `advisor`: every finding about the fleet's company at the
// revision it is, the revision each fix is sent under. One read of the fleet's
// pointer gives both, and the document is redacted before anything is read
// off it, so no change a fix carries can hold a value redaction hides.
//
// AN EMPTY LIST, not an error, when nothing is active: a deployment before
// its first import has nothing to advise about.
func (s Sources) advisor(ctx context.Context, _ Params) (any, error) {
	company, revision, err := s.Config.FleetCompany(ctx)
	if errors.Is(err, configapi.ErrNoActiveRevision) {
		return advisor.Answer{Findings: []advisor.Finding{}, Unread: []string{}}, nil
	}
	if err != nil {
		return nil, revisionRefusal("advisor", err)
	}
	in := advisor.Inputs{Company: company.Redact(), Revision: revision}
	// THE INTEGRATIONS' LAST RECONCILE, from the read the integrations answer
	// makes; nil where this node cannot say, which the answer names as unread.
	if states, known := s.reconcileStates(ctx); known {
		in.Integrations = slices.SortedFunc(maps.Values(states), func(a, b integration.State) int {
			return cmp.Compare(a.Kind, b.Kind)
		})
	}
	return advisor.Read(in), nil
}
