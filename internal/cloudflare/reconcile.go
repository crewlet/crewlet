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
	"github.com/crewlet/crewlet/internal/seataccess"
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

	// InUse are the levels a seat that is on holds ([seataccess.LevelsInUse]):
	// a level the token cannot grant is a problem only for one of them.
	InUse []string
}

// Result is what one pass found. It writes nothing at Cloudflare.
type Result struct {
	// Status is the provisioner token as Cloudflare reports it.
	Status TokenStatus

	// Zones is how many zones the token reads in the account.
	Zones int

	// MissingZones are the configured zones the account does not have.
	MissingZones []string

	// OwnUnheld are the groups the provisioner needs for itself and lacks.
	OwnUnheld []string

	// AtlasUnheld are the optional groups the atlas would read more with
	// that the token does not hold: a suggestion, never a gap.
	AtlasUnheld []string

	// Gaps are the levels the account's catalogue or the token cannot grant.
	Gaps []seataccess.LevelGap

	// InUse are the levels a seat that is on holds.
	InUse []string

	// Orphaned are the active tokens named for this company's agents that
	// the ledger does not record: reported, never touched, since a name is
	// no proof the engine made one.
	Orphaned []Token

	// TokensURL is where a person edits the account's tokens.
	TokensURL string

	// Inspection is the read the pass made of the token and the account,
	// for the agents' session of the same pass to take rather than read
	// again ([seataccess.OpenInput.Reads]).
	Inspection *Inspection

	now time.Time
}

// Inspection is one pass's read of the provisioner token and the account,
// as the pass's company half hands it to its agents' session: the account's
// permission groups and zones, and what the token itself holds. One vendor's
// own, which the session checks by its account before it takes it.
type Inspection struct {
	account string
	in      inspection
}

// ReadFor names the vendor, as [seataccess.PassReads] asks.
func (*Inspection) ReadFor() integration.Kind { return integration.KindCloudflare }

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
		OwnUnheld: ownUnheld(in.unheld), Gaps: levelGaps(in.unresolved, in.unheld, opts.Client.TokensURL()),
		AtlasUnheld: slices.Clone(in.atlasUnheld),
		InUse:       opts.InUse,
		Orphaned:    orphanedTokens(tokens, opts.Managed, opts.Company, in.status.ID),
		TokensURL:   opts.Client.TokensURL(), now: now,
		Inspection: &Inspection{account: opts.Client.AccountID(), in: in},
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
	rejected := reject(err)
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
	// subjectAtlas is the suggestion of the atlas's optional permissions,
	// which no field clears: the token at Cloudflare does.
	subjectAtlas = "cloudflare atlas permissions"
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
	out = append(out, seataccess.GapFindings(r.Gaps, r.InUse)...)
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
	if said := seeMore(r.AtlasUnheld); said != "" {
		// AN ADVISORY ON A READY INTEGRATION, never a short grant: nothing
		// Crewlet does for the agents waits on these (ADR-0145).
		out = append(out, integration.Finding{
			Kind: integration.FindingCoveragePartial, Subject: subjectAtlas,
			Title: seeMoreLabel, Detail: strings.TrimSuffix(said, "."),
			Remedy:    "Edit the provisioner token at Cloudflare to add " + them(len(r.AtlasUnheld)) + " over the account, or leave it as it is",
			ActionURL: r.TokensURL, Subjects: dashboardList(r.AtlasUnheld),
		})
	}
	if len(r.OwnUnheld) > 0 {
		out = append(out, integration.Finding{
			Kind: integration.FindingGrantShort, Subject: subjectToken,
			Title: "Provisioner token is missing permissions",
			Detail: "The provisioner token does not hold " + words.And(dashboardNames(r.OwnUnheld)) +
				", which it needs to manage the agents' tokens at all",
			Remedy:    "Edit the provisioner token at Cloudflare to add them over the account and its zones",
			ActionURL: r.TokensURL, Subjects: dashboardList(r.OwnUnheld),
		})
	}
	return out
}
