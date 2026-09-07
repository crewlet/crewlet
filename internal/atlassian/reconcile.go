package atlassian

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
)

// Options is what one provisioning pass needs.
type Options struct {
	// Client talks to the organization's admin APIs.
	Client *Client

	// OrgID and Key are the organization and its unscoped API key.
	OrgID string
	Key   string

	// Plan names the seats wanting an identity, from [PlanFor].
	Plan *provision.Plan

	// Sink records what is minted. Nil is a DRY RUN: the pass reads what
	// exists and creates nothing, which is what a check is.
	Sink provision.TokenSink

	// Now is injectable so a test can pin a token's label and expiry.
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now == nil {
		return time.Now().UTC()
	}
	return o.Now().UTC()
}

// Result is what one pass found.
type Result struct {
	// Site is the Atlassian site the organization's agents work in,
	// discovered rather than asked for.
	Site Site

	// Seats is one entry per seat the plan named.
	Seats []SeatResult

	// Notes are the caveats: a seat whose token slot is a literal.
	Notes []string
}

// SeatResult is what happened to one seat.
type SeatResult struct {
	Handle string
	// AccountID is the Atlassian service account, empty when none exists.
	AccountID string
	// Created reports an account this pass made.
	Created bool
	// TokenMinted reports a credential this pass issued.
	TokenMinted bool
	// Err is why this seat could not be provisioned, if it could not.
	Err error
}

// Reconcile brings this company's Atlassian identities in line with its org.
//
// CREATE AND MINT, NEVER DELETE. A seat that has left the org chart keeps its
// account until somebody disconnects the integration, which is the explicit
// act on the other end: a reconcile loop that removed an identity because a
// config was mid-edit would destroy an account and everything attributable to
// it, on a timer.
//
// It is also idempotent in the way that matters: an account is recognised by
// the description this engine wrote on it, so a second pass over the same org
// finds what the first made rather than creating it twice.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
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
	res := &Result{Site: *site}
	if opts.Plan == nil {
		return res, nil
	}
	res.Notes = append(res.Notes, opts.Plan.Notes...)

	existing, err := opts.Client.ListServiceAccounts(ctx, opts.Key, opts.OrgID)
	if err != nil {
		return nil, fmt.Errorf("atlassian: list service accounts: %w", err)
	}
	// BY THE MARKER IN THE DISPLAY NAME, which is the only field of this
	// engine's own that Atlassian keeps: it accepts a description, answers
	// 200 and stores nothing. See [AccountName].
	byHandle := map[string]ServiceAccount{}
	for _, account := range existing {
		if handle := HandleFrom(account.DisplayName); handle != "" {
			byHandle[handle] = account
		}
	}

	for _, seat := range opts.Plan.Seats {
		res.Seats = append(res.Seats, reconcileSeat(ctx, opts, seat, byHandle))
	}
	return res, nil
}

// reconcileSeat brings one seat's identity into line.
func reconcileSeat(
	ctx context.Context, opts Options, seat provision.Seat, byHandle map[string]ServiceAccount,
) SeatResult {
	out := SeatResult{Handle: seat.Handle}
	account, found := byHandle[seat.Handle]

	switch {
	case found:
		out.AccountID = account.ID
	case opts.Sink == nil:
		// A DRY RUN CREATES NOTHING. The seat is reported without an
		// account, which is the true answer to what a check asks.
		return out
	default:
		created, err := opts.Client.CreateServiceAccount(ctx, opts.Key, opts.OrgID,
			AccountName(seat.Role, seat.Handle), "")
		if err != nil {
			out.Err = fmt.Errorf("atlassian: create the account for %s: %w", seat.Handle, err)
			return out
		}
		out.AccountID, out.Created = created.ID, true
	}

	// THE TOKEN IS MINTED ONLY WHERE THERE IS NOWHERE TO READ ONE FROM. A
	// mint is not idempotent — Atlassian issues a new credential every time
	// and shows it once — so minting on every pass would rotate the token
	// every agent is authenticating with, on the reconcile loop's timer.
	if opts.Sink == nil {
		return out
	}
	// THREE-VALUED, and the middle answer is the one that matters: held,
	// definitively not held, and "the store could not say". Minting on the
	// third would issue a new credential over one that already exists and
	// that every running seat is authenticating with.
	_, held, err := opts.Sink.Value(ctx, seat.TokenVar)
	if err != nil {
		out.Err = fmt.Errorf("atlassian: read %s: %w", seat.TokenVar, err)
		return out
	}
	if held {
		return out
	}
	token, err := opts.Client.MintToken(ctx, opts.Key, out.AccountID, seat.Handle, opts.now())
	if err != nil {
		out.Err = fmt.Errorf("atlassian: mint a token for %s: %w", seat.Handle, err)
		return out
	}
	if err := opts.Sink.Record(ctx, seat.TokenVar, token.Token); err != nil {
		out.Err = fmt.Errorf("atlassian: record %s: %w", seat.TokenVar, err)
		return out
	}
	out.TokenMinted = true
	return out
}

// Findings is what this pass has to report to the reconcile status.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	out := []integration.Finding{}
	for _, seat := range r.Seats {
		switch {
		case seat.Err != nil:
			out = append(out, integration.Finding{
				Kind: integration.FindingIdentityFailed, Subject: seat.Handle,
				Detail: seat.Err.Error(),
			})
		case seat.AccountID == "":
			out = append(out, integration.Finding{
				Kind: integration.FindingIdentityMissing, Subject: seat.Handle,
				Detail: seat.Handle + " has no Atlassian account, so nothing it does " +
					"in Jira or Confluence is its own",
			})
		}
	}
	for _, note := range r.Notes {
		out = append(out, integration.Finding{
			Kind: integration.FindingGrantShort, Detail: note,
		})
	}
	return out
}
