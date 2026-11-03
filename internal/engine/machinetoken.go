package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam/credential"
)

// MachineToken is the row one presented machine token is checked against, as
// the request guard's third credential arm reads it — a personal access token
// or a service account's, minted through `/iam/credentials`.
//
// # Three answers, and this node's third is an error
//
// What the rows MEAN is internal/iam/credential's table, over the value this
// returns: the row, the owner as the directory holds them now, and how far the
// node that read them can vouch for both. An ERROR is the unknown arm, always
// — an engine holding no identity directory answers [errNoDirectory] rather
// than an empty row, because an absent estate read as "no such token" is a 401
// to a pipeline whose credential is fine.
//
// # This node's own rows, or a data node's
//
// A data node reads its own replicated rows, BOUNDED BY [requestReadBudget],
// for [Engine.BoundSeat]'s reason: it is a keyed read of this node's own file
// in front of every request a pipeline makes, so running out is a 503 rather
// than a request held behind a sick file handle.
//
// A node without `data` holds no rows, so it asks a data node through the
// estate's router — floored on the identity log, bounded by the router per
// data node it asks and by the caller as a whole, as [Engine.HolderRecord]'s
// routed read is: a two-second ceiling over a failover would end it at the
// first silent node and answer unknown where the next would have answered.
// What crosses back is NARROWED (internal/estate's TokenFacts): the row with
// its verifier withheld and a MAC over it in its place, and the verifier on
// the row returned here is the one the presented token implies, set only
// where that MAC confirms it is the stored one — so the table decides on
// either node exactly what it decides on a data node, and neither the secret
// nor the verifier ever reaches the broker. That is why this takes the token
// whole rather than its id: the secret it carries is checked here, against
// the MAC, and goes nowhere else.
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
	// A NODE WITHOUT `data`, and only one: a data node whose core is not up
	// yet holds a directory it cannot read, which is "cannot say" — never a
	// peer's answer standing in for rows it is about to apply.
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
