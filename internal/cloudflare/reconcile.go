package cloudflare

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/words"
)

// Options is what one pass over the company's half of Cloudflare needs: the
// provisioner token and the account. Each agent's own token is the per-seat
// access sync's ([accessVendor]), which a setup pass runs beside this one.
type Options struct {
	// Client carries the provisioner token for the company's account.
	Client *Client

	// Config is the company's cloudflare block.
	Config *config.Cloudflare

	// Now is the clock the expiry warning is judged by; nil is the wall
	// clock.
	Now func() time.Time

	// Company is the company's name, which every agent token's name carries
	// ([TokenName]).
	Company string

	// Managed are the token IDs the access ledger records. The orphan
	// advisory never reports them.
	Managed []string
}

// Result is what one pass found. It writes nothing at Cloudflare.
type Result struct {
	// Status is the provisioner token as Cloudflare reports it.
	Status TokenStatus

	// Zones is how many zones the token reads in the account.
	Zones int

	// MissingZones are the configured zones the account does not have.
	MissingZones []string

	// Unresolved is, by level, the permission group names the account's
	// catalogue does not offer.
	Unresolved map[string][]string

	// Unheld are the provisioner groups the token does not hold itself.
	Unheld []string

	// Orphaned are the active tokens named for this company's agents that
	// the ledger does not record: reported, never touched, since a name is
	// no proof the engine made one.
	Orphaned []Token

	// TokensURL is where a person edits the account's tokens.
	TokensURL string

	now time.Time
}

// Reconcile reads the provisioner token and the account and reports what an
// operator has to act on. A refused or inactive token is an error wrapping
// [integration.ErrCredentialRejected]: nothing else a pass reports could be
// trusted over it.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Client == nil {
		return nil, errors.New("cloudflare: no client")
	}
	if opts.Config == nil || opts.Config.Provisioning == nil {
		return nil, integration.ErrNotConfigured
	}
	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now()
	}
	in, err := inspect(ctx, opts.Client, opts.Config.Provisioning.Zones, nil)
	if err != nil {
		return nil, reconcileError(err)
	}
	tokens, err := opts.Client.Tokens(ctx)
	if err != nil {
		return nil, reconcileError(&stageError{stage: stageTokens, err: err})
	}
	return &Result{
		Status: in.status, Zones: len(in.zones), MissingZones: in.cat.missingZones,
		Unresolved: in.unresolved, Unheld: in.unheld,
		Orphaned:  orphanedTokens(tokens, opts.Managed, opts.Company, in.status.ID),
		TokensURL: opts.Client.TokensURL(), now: now,
	}, nil
}

// orphanedTokens is every active token named for this company's agents that
// is neither one the ledger records nor the provisioner itself. A disabled or
// expired one is already inert, which is where the advisory asks it to get.
func orphanedTokens(tokens []Token, managed []string, company, provisionerID string) []Token {
	prefix := TokenName(company, "")
	var out []Token
	for _, tok := range tokens {
		if tok.Status != StatusActive || tok.ID == provisionerID || slices.Contains(managed, tok.ID) ||
			!strings.HasPrefix(tok.Name, prefix) {
			continue
		}
		out = append(out, tok)
	}
	slices.SortFunc(out, func(a, b Token) int { return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID)) })
	return out
}

// reconcileError is a failed read as the loop reads it: a refusal or an
// inactive token is the credential's, anything else a fault it retries.
func reconcileError(err error) error {
	where := "the token"
	needs := ""
	if st, ok := errors.AsType[*stageError](err); ok {
		where, needs = st.stage.name, st.stage.needs
	}
	if _, ok := errors.AsType[*inactiveError](err); ok {
		return fmt.Errorf("cloudflare: integrations.cloudflare.provisioning.api_token: %w: %w",
			integration.ErrCredentialRejected, err)
	}
	rejected := integration.Reject(err, Status(err))
	if errors.Is(rejected, integration.ErrCredentialRejected) && needs != "" {
		return fmt.Errorf("cloudflare: the provisioner token in integrations.cloudflare."+
			"provisioning.api_token was refused reading %s, which needs %s: %w", where, needs, rejected)
	}
	return fmt.Errorf("cloudflare: the provisioner token in integrations.cloudflare."+
		"provisioning.api_token %s reading %s: %w", integration.Refusal(rejected), where, rejected)
}

// The subjects a company finding names: the fields that clear it.
const (
	subjectToken = "integrations.cloudflare.provisioning.api_token"
	subjectZones = "integrations.cloudflare.provisioning.zones"
)

// Findings is what the pass saw, in the shared vocabulary. Each agent's own
// token is reported per seat by the access sync.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	out := []integration.Finding{}
	if integration.ExpiresSoon(r.Status.ExpiresOn, r.now) {
		out = append(out, integration.Finding{
			Kind: integration.FindingCredentialExpiring, Subject: subjectToken,
			Title: "Provisioner token expires soon",
			Detail: "The provisioner token expires on " + r.Status.ExpiresOn.Format(time.DateOnly) +
				". From then on no pass can mint, roll or narrow an agent's token",
			Remedy:    "Create a new account API token with the same permissions and replace it on the Cloudflare connection",
			ActionURL: r.TokensURL, ExpiresAt: r.Status.ExpiresOn,
		})
	}
	if len(r.MissingZones) > 0 {
		out = append(out, integration.Finding{
			Kind: integration.FindingUnknownTier, Subject: subjectZones,
			Title: "Zones not found",
			Detail: "The account has no " + integration.Count(len(r.MissingZones), "zone") +
				" the company names, so agents' tokens leave them out",
			Remedy: "Correct integrations.cloudflare.provisioning.zones, or give the provisioner " +
				"token access to the zone at Cloudflare",
			ActionURL: r.TokensURL, Subjects: r.MissingZones,
		})
	}
	for _, level := range Levels() {
		missing := r.Unresolved[level]
		if len(missing) == 0 {
			continue
		}
		out = append(out, integration.Finding{
			Kind: integration.FindingUnknownTier, Subject: "cloudflare level " + level,
			Title: "Permission groups not found",
			Detail: fmt.Sprintf("Cloudflare lists no permission group a token can be granted named %s, "+
				"which the %s level grants, so no agent can be given that level", words.And(missing), LevelName(level)),
			Remedy:   "Move the agents at this level to another one on the Agents tab until Cloudflare lists the group again",
			Subjects: missing,
		})
	}
	if len(r.Orphaned) > 0 {
		names := make([]string, 0, len(r.Orphaned))
		for _, tok := range r.Orphaned {
			names = append(names, tok.Name+" ("+tok.ID+")")
		}
		out = append(out, integration.Finding{
			Kind: integration.FindingRegistrationOrphaned, Subject: "cloudflare agent tokens",
			Title: "Agent tokens with no seat",
			Detail: "Cloudflare holds " + integration.Count(len(names), "active account token") +
				" named for this company's agents that Crewlet has no record of making",
			Remedy: "Delete the ones nobody uses at Cloudflare. Crewlet never deletes a token " +
				"it has no record of",
			ActionURL: r.TokensURL, Subjects: names,
		})
	}
	if len(r.Unheld) > 0 {
		out = append(out, integration.Finding{
			Kind: integration.FindingGrantShort, Subject: subjectToken,
			Title: "Provisioner token is missing permissions",
			Detail: "The provisioner token does not hold " + integration.Count(len(r.Unheld), "permission group") +
				" an agent level grants, and Crewlet grants an agent nothing the token does not hold itself",
			Remedy:    "Edit the provisioner token at Cloudflare to add them over the account and its zones",
			ActionURL: r.TokensURL, Subjects: r.Unheld,
		})
	}
	return out
}
