package googlecloud

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/words"
)

// Options is what one pass over the company's half of Google Cloud needs: the
// provisioner's client and the agents' accounts the access ledger records.
// Each agent's own account is the per-seat access sync's ([accessVendor]),
// which a setup pass runs beside this one.
type Options struct {
	Client *Client

	// Company is the company's name, which every agent account's display
	// name carries ([displayName]).
	Company string

	// Seats are the agents' accounts the access ledger records, by seat
	// handle to unique ID.
	Seats map[string]string

	// Growth are the seats' growth identities the core records: Crewlet's
	// accounts too, by unique ID, or by email while only the intent to make
	// one is recorded.
	Growth []coord.GoogleIdentity

	// InUse are the levels a seat that is on holds ([seataccess.LevelsInUse]):
	// a level Google's roles cannot be granted at is a problem only for one.
	InUse []string
}

// Result is what one pass found. It writes nothing at Google.
type Result struct {
	gaps  []seataccess.LevelGap
	inUse []string

	// identity is the identity project, foreign its accounts that are
	// neither the provisioner nor Crewlet's, and orphaned the accounts named
	// for this company's agents that the ledger does not record.
	identity string
	foreign  []string
	orphaned []string
}

// Reconcile reaches Google as the provisioner, reads the level roles and the
// identity project's accounts, and reports what an operator has to act on. A
// refused federation or impersonation is an error wrapping
// [integration.ErrCredentialRejected]: nothing else a pass reports could be
// trusted over it.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Client == nil {
		return nil, errors.New("googlecloud: no client")
	}
	c := opts.Client
	if _, err := c.auth.FederatedToken(ctx, c.Provisioner()); err != nil {
		return nil, fmt.Errorf("googlecloud: the provisioner in integrations.google_cloud.provisioning %s: %w",
			integration.Refusal(googleauth.Reject(err)), googleauth.Reject(err))
	}
	gaps, err := levelGaps(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("googlecloud: %w", googleauth.Reject(err))
	}
	accounts, err := c.ServiceAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("googlecloud: read the accounts of %s: %w", c.IdentityProject(), googleauth.Reject(err))
	}
	res := &Result{gaps: gaps, inUse: opts.InUse, identity: c.IdentityProject()}
	recorded := map[string]bool{}
	for _, uid := range opts.Seats {
		recorded[uid] = true
	}
	intended := map[string]bool{}
	for _, rec := range opts.Growth {
		if rec.Made() {
			recorded[rec.UniqueID] = true
		} else {
			intended[rec.Email] = true
		}
	}
	ours := displayName(opts.Company, "")
	for _, a := range accounts {
		switch {
		case a.Email == c.Provisioner() || recorded[a.UniqueID] || intended[a.Email]:
		case strings.HasPrefix(a.Email, accountPrefix) && strings.HasPrefix(a.DisplayName, ours):
			res.orphaned = append(res.orphaned, a.Email)
		case !crewletsOwn(a.Email):
			res.foreign = append(res.foreign, a.Email)
		}
	}
	slices.Sort(res.foreign)
	slices.Sort(res.orphaned)
	return res, nil
}

// subjectIdentity is the field that clears a finding about the identity
// project or the provisioner in it.
const subjectIdentity = "integrations.google_cloud.provisioning.identity_project"

// Findings is what the pass saw, in the shared vocabulary. Each agent's own
// account is reported per seat by the access sync.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	out := seataccess.GapFindings(r.gaps, r.inUse)
	if len(r.foreign) > 0 {
		out = append(out, integration.Finding{
			Kind: integration.FindingGrantExcess, Subject: subjectIdentity,
			Title: "Accounts the provisioner can act as",
			Detail: "The identity project " + r.identity + " holds " + integration.Count(len(r.foreign), "service account") +
				" besides Crewlet's, and the provisioner's Service Account Admin there lets it grant itself any of them",
			Remedy: "Move " + pronoun(len(r.foreign)) + " to another project, so the identity project holds the " +
				"provisioner and Crewlet's accounts alone",
			ActionURL: projectURL(ServiceAccountsURL, r.identity), Subjects: slices.Clone(r.foreign),
		})
	}
	if len(r.orphaned) > 0 {
		out = append(out, integration.Finding{
			Kind: integration.FindingRegistrationOrphaned, Subject: "google_cloud agent service accounts",
			Title: "Agent service accounts with no seat",
			Detail: "The identity project " + r.identity + " holds " + integration.Count(len(r.orphaned), "service account") +
				" named for this company's agents that Crewlet has no record of making: " + words.And(r.orphaned),
			Remedy:    "Delete the ones nobody uses. Crewlet never deletes an account it has no record of",
			ActionURL: projectURL(ServiceAccountsURL, r.identity), Subjects: slices.Clone(r.orphaned),
		})
	}
	return out
}
