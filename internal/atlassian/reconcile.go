package atlassian

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
)

// Options is what one company-level pass needs.
type Options struct {
	// Client talks to the organization's admin APIs.
	Client *Client

	// OrgID and Key are the organization and its unscoped API key.
	OrgID string
	Key   string

	// Plan and Sink are IGNORED. They are kept only so the engine's current
	// call sites compile until they are retired: seats are per-seat access
	// (internal/atlassian/agentaccess), and this pass seals nothing.
	Plan *provision.Plan
	Sink provision.TokenSink
}

// Result is what one pass found.
type Result struct {
	// Site is the Atlassian site the organization's agents work in,
	// discovered rather than asked for.
	Site Site
}

// Findings is what this pass reports beyond a failure, which is nothing: a
// key that does not work or a site that cannot be found is the error
// [Reconcile] returns. A seat's findings come from per-seat access.
func (r *Result) Findings() []integration.Finding { return nil }

// TeardownOptions is what removing the organization's integration needs.
type TeardownOptions struct {
	Client *Client
	OrgID  string
	Key    string

	// Plan is IGNORED, kept only so the engine's current call site compiles
	// until it is retired: removing an agent's account is per-seat access,
	// through the ledger.
	Plan *provision.Plan
}

// Teardown has nothing of the organization's to undo. Atlassian keeps no
// webhook this engine registered (Cloud events arrive through the Forge
// relay), and every agent's account is per-seat access, removed through the
// ledger rather than from the document's current seats. It answers an empty
// [provision.Removed].
func Teardown(_ context.Context, opts TeardownOptions) (provision.Removed, error) {
	if opts.Client == nil {
		return provision.Removed{}, errors.New("atlassian: no client")
	}
	return provision.Removed{}, nil
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
// # Why a window rather than a read
//
// Atlassian exposes no read of a service account's product access at all:
// account-management answers no GET for a single account, the service-account
// listing carries identity and metadata only, and the routes that do return
// product access are directory-user routes, which a service account is not.
// So nothing can ask whether a grant is in force. What CAN be observed is the
// product refusing the account's credential, and that observation means two
// different things depending only on how long ago the credential was sealed:
// inside this window it is the grant still landing, and outside it, it is
// something a person has to look at.
//
// MEASURED, not chosen: on a live Cloud site the gateway accepted a
// seconds-old token and Jira answered its own 401 ("Client must be
// authenticated to access this resource") for about seventy seconds, twice in
// a row, then worked. Five minutes is a four-times margin over that, which is
// the honest shape for a provider's internal propagation with no published
// bound — and it is short enough that a grant which is NOT landing becomes
// somebody's work inside one settled interval rather than waiting for ever
// under "the provider is working on it".
const GrantPropagation = 5 * time.Minute
