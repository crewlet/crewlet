package atlassian

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
)

// Options is what one company-level pass needs.
type Options struct {
	// Client talks to the organization's admin APIs.
	Client *Client

	// OrgID and Key are the organization and its unscoped API key.
	OrgID string
	Key   string
}

// Result is what one pass found.
type Result struct {
	// Site is the Atlassian site the organization's agents work in,
	// discovered rather than asked for.
	Site Site
}

// Reconcile is the organization's company-level pass: it proves the
// organization key and discovers the site the agents work in.
//
// # Seats are not this pass's
//
// Each agent's service account, product access, roles and API token are
// per-seat access (internal/atlassian/agentaccess): a seat's own Jira and
// Confluence blocks in the document, read back every pass and applied by the
// shared sync. This pass creates, grants, mints and deletes nothing.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	// A CANCELLED PASS HAS OBSERVED NOTHING, so it must RAISE rather than
	// answer with a Result. Checked here although a dead context fails the
	// first request anyway, because of WHAT it fails as: the sentence wrapped
	// around that failure names integrations.atlassian.api_key as the thing
	// that could not be verified, which would send an operator to rotate a
	// key that is fine.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("atlassian: %w", err)
	}
	if opts.Client == nil {
		return nil, errors.New("atlassian: no client")
	}
	if opts.OrgID == "" || opts.Key == "" {
		return nil, errors.New(
			"atlassian: the organization id and its API key are both needed; " +
				"the admin APIs take the organization as the subject and the key " +
				"as the authority, and neither stands in for the other")
	}
	site, err := opts.Client.DiscoverSite(ctx, opts.Key, opts.OrgID)
	if err != nil {
		rejected := integration.Reject(err, Status(err))
		return nil, fmt.Errorf(
			"atlassian: the organization credential in "+
				"integrations.atlassian.api_key %s, so nothing else this run "+
				"reports would be trustworthy: %w",
			integration.Refusal(rejected), rejected)
	}
	return &Result{Site: *site}, nil
}

// GrantPropagation is how long Atlassian takes to make a new service
// account's product access actually work.
//
// Atlassian exposes no read of a service account's product access, so nothing
// can ask whether a grant is in force; the product refusing the credential is
// the grant still landing inside this window and needs a person outside it.
//
// MEASURED: Jira answered 401 to a seconds-old token for about seventy
// seconds, twice, then worked. Five minutes is a four-times margin on an
// unpublished bound, short enough that a grant that is NOT landing becomes
// somebody's work within one settled interval.
const GrantPropagation = 5 * time.Minute
