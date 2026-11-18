package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam/credential"
)

// MachineToken is the row a presented machine token (a personal access or
// service account token) is checked against by the request guard's third
// credential arm, as internal/iam/credential's table reads it. An error is the
// unknown arm: no directory answers [errNoDirectory], never a "no such token"
// empty row, a 401 to a pipeline whose token is fine.
//
// A data node reads its own rows within [requestReadBudget] ([Engine.BoundSeat]'s
// reason); one without `data` asks one through the estate's router, floored on the
// identity log and bounded as [Engine.HolderRecord] is, never by that budget, which
// would end a failover at the first silent node. The answer is NARROWED
// (internal/estate's TokenFacts): a MAC checks the whole token's verifier there,
// so the table decides as on a data node and no secret or verifier crosses the broker.
func (e *Engine) MachineToken(ctx context.Context, presented credential.Token) (
	credential.TokenRow, error) {

	if e == nil {
		return credential.TokenRow{}, errNoDirectory
	}
	if reader := e.IAM(); reader != nil {
		bounded, cancel := context.WithTimeout(ctx, requestReadBudget)
		defer cancel()
		return reader.MachineToken(bounded, presented.ID)
	}
	// A NODE WITHOUT `data` only: a data node whose core is not up yet cannot
	// say, and a peer's answer must not stand in for rows it is about to apply.
	if e.router == nil || e.local != nil {
		return credential.TokenRow{}, errNoDirectory
	}
	row, err := e.router.Identity().MachineToken(ctx, presented)
	if err != nil {
		return credential.TokenRow{}, fmt.Errorf("engine: ask a data node for the "+
			"rows machine token %s is checked against: %w", presented.ID, err)
	}
	return row, nil
}
