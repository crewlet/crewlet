package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/iam/credential"
)

// MachineToken is one presented machine token and its owner, as the request
// guard's third credential arm reads them — a personal access token or a
// service account's, minted through `/iam/credentials`.
//
// # Three answers, and this node's third is an error
//
// What the rows MEAN is internal/iam/credential's table, over the value this
// returns: the row, the owner as this node holds them now, and how far the node
// can vouch for both. An ERROR is the unknown arm, always — a node running no
// identity domain answers [errNoIdentityDomain] rather than an empty row,
// because an empty copy of the estate read as "no such token" is a 401 to a
// pipeline whose credential is fine, on exactly the nodes that cannot see it.
//
// BOUNDED BY [requestReadBudget], for [Engine.BoundSeat]'s reason: it is a
// keyed read of this node's own replicated rows in front of every request a
// pipeline makes, so running out is a 503 rather than a request held behind a
// sick file handle.
func (e *Engine) MachineToken(ctx context.Context, id string) (
	credential.TokenRow, error) {

	if e == nil {
		return credential.TokenRow{}, errNoIdentityDomain
	}
	reader := e.IAM()
	if reader == nil {
		return credential.TokenRow{}, errNoIdentityDomain
	}
	ctx, cancel := context.WithTimeout(ctx, requestReadBudget)
	defer cancel()
	return reader.MachineToken(ctx, id)
}
