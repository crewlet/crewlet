package datadog

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
)

// Options is what one provisioning pass needs.
type Options struct {
	// Client talks to the company's region.
	Client *Client

	// Config is the company's datadog block.
	Config *config.Datadog

	// Plan names the seats wanting an identity, from [PlanFor].
	Plan *provision.Plan

	// Creds is the organization credential pair, resolved.
	Creds Credentials

	// Sink records what is minted. Nil is a DRY RUN: the pass reads what
	// exists and creates nothing, which is what a check is.
	Sink provision.TokenSink

	// WebhookBase is this deployment's public base URL, or empty to skip
	// registering the inbound webhook at Datadog.
	WebhookBase string

	// WebhookToken is the resolved shared token the definition carries and
	// the route checks. Empty skips registration for the same reason an
	// empty base does: a definition that cannot deliver is worse than none.
	WebhookToken string
}

// Result is what one pass found.
type Result struct {
	// Org is the organization the credentials authenticate as, which is
	// the one thing an operator can check to see they connected the
	// account they meant to.
	Org Org

	// Seats is one entry per seat the plan named.
	Seats []SeatResult

	// Webhook is what this pass did to the inbound definition at Datadog,
	// nil where the pass never reached it.
	Webhook *WebhookResult

	// Notes are the caveats: a seat whose key is a literal, a role the
	// organization does not have.
	Notes []string
}

// SeatResult is what happened to one seat.
type SeatResult struct {
	Handle string
	// AccountID is the Datadog user, empty when none exists yet.
	AccountID string
	// Created reports an account this pass made.
	Created bool
	// KeyMinted reports an application key this pass minted.
	KeyMinted bool
	// Err is why this seat could not be provisioned, if it could not.
	Err error
}

// Reconcile brings this company's Datadog identities in line with its org.
//
// # What it will and will not do
//
// It CREATES an account for a seat that has none, holding the configured
// role, and mints that account one application key written into the seat's
// own ${VAR}. It does not delete, rename, or re-role an account that already
// exists: a seat that left the config looks exactly like a company mid-edit,
// and the difference is not visible from here. Removal is [Teardown], which
// is only ever reached by somebody pressing Disconnect.
//
// A pass with NO SINK reads and reports without writing, which is what a
// read-only check is. It still lists accounts, so an operator sees which
// seats are missing one without the pass creating any.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if opts.Client == nil {
		return nil, errors.New("datadog: no client")
	}
	if opts.Config == nil || opts.Config.Provisioning == nil {
		return nil, integration.ErrNotConfigured
	}
	if opts.Creds.APIKey == "" || opts.Creds.AppKey == "" {
		// A FINDING'S WORTH OF FACT, reported as an error because the
		// pass observed nothing: both keys are needed together, and
		// Datadog's refusal for a missing one names neither.
		return nil, errors.New(
			"datadog: both the API key and the application key are needed; " +
				"Datadog refuses a write carrying only one and its message " +
				"names neither")
	}

	org, err := opts.Client.VerifyCredentials(ctx, opts.Creds)
	if err != nil {
		// THE VERB FOLLOWS THE CLASSIFICATION. Saying refused whatever
		// happened turns this build's own mistake into an accusation
		// against the operator's key: see [integration.Refusal].
		rejected := integration.Reject(err, Status(err))
		return nil, fmt.Errorf(
			"datadog: the organization credential pair in "+
				"integrations.datadog.provisioning %s, so nothing "+
				"else this run reports would be trustworthy: %w",
			integration.Refusal(rejected), rejected)
	}
	res := &Result{Org: org}

	// THE INBOUND HALF FIRST, and before the early return below. A company
	// with no seats to give identities to still wants its alerts to
	// arrive, and the webhook is the only thing that makes them.
	hook := ensureWebhook(ctx, opts)
	res.Webhook = &hook
	if hook.Note != "" {
		res.Notes = append(res.Notes, hook.Note)
	}

	if opts.Plan == nil {
		return res, nil
	}
	res.Notes = append(res.Notes, opts.Plan.Notes...)

	// THE ROLE IS RESOLVED ONCE, not per seat: it is the same role for
	// every account, and asking Datadog for it per seat spends a rate
	// limit on an answer that cannot differ.
	roleName := RoleName(opts.Config.Provisioning)
	roleID, err := roleIDOf(ctx, opts, roleName)
	if err != nil {
		return nil, err
	}

	existing, err := opts.Client.ListServiceAccounts(ctx, opts.Creds, emailDomainOf(opts.Config))
	if err != nil {
		return nil, fmt.Errorf("datadog: list the service accounts: %w",
			integration.Reject(err, Status(err)))
	}
	byEmail := make(map[string]User, len(existing))
	for _, user := range existing {
		byEmail[user.Email] = user
	}

	for _, seat := range opts.Plan.Seats {
		res.Seats = append(res.Seats, provisionSeat(ctx, opts, seat, byEmail, roleID))
	}
	if opts.Sink != nil {
		if err := opts.Sink.Flush(ctx); err != nil {
			return res, fmt.Errorf("datadog: record the minted keys: %w", err)
		}
	}
	return res, nil
}

// provisionSeat brings one seat's identity into line.
func provisionSeat(
	ctx context.Context, opts Options, seat provision.Seat,
	byEmail map[string]User, roleID string,
) SeatResult {
	out := SeatResult{Handle: seat.Handle}
	account, found := byEmail[seat.Email]

	switch {
	case found:
		out.AccountID = account.ID
	case opts.Sink == nil:
		// A CHECK CREATES NOTHING. The seat is reported without an
		// account, which is the fact the check exists to surface.
		return out
	default:
		created, err := opts.Client.CreateServiceAccount(
			ctx, opts.Creds, seat.Email, AccountName(seat.Role, seat.Handle), roleID)
		if err != nil {
			out.Err = fmt.Errorf("create the account for %s: %w", seat.Handle,
				integration.Reject(err, Status(err)))
			return out
		}
		out.AccountID, out.Created = created.ID, true
		account = created
	}

	// A KEY IS MINTED ONLY WHEN THE SEAT HAS NONE, because Datadog returns
	// a key's value exactly once: minting a second on every pass would
	// leave a trail of keys nobody holds, and rotating one revokes what
	// the agent is currently authenticating with.
	if opts.Sink == nil {
		return out
	}
	// THREE-VALUED, and the unknown case matters: a sink that could not be
	// read is not a seat with no key. Minting on unknown would revoke what
	// the agent is authenticating with, so an unreadable sink stops this
	// seat and says so rather than acting on a guess.
	_, held, err := opts.Sink.Value(ctx, seat.TokenVar)
	switch {
	case err != nil:
		out.Err = fmt.Errorf(
			"could not read whether %s already holds a key, so none was "+
				"minted: %w", seat.TokenVar, err)
		return out
	case held:
		return out
	}
	keys, err := opts.Client.ListAppKeys(ctx, opts.Creds, account.ID)
	if err != nil {
		out.Err = fmt.Errorf("read %s's keys: %w", seat.Handle,
			integration.Reject(err, Status(err)))
		return out
	}
	if len(keys) > 0 {
		// AN ACCOUNT WITH A KEY THIS ENGINE CANNOT READ. Datadog shows a
		// value once, so a key that exists with nothing stored for it is
		// unrecoverable: it is reported rather than replaced, because
		// replacing it silently revokes whatever is using it.
		out.Err = fmt.Errorf(
			"%s already has an application key and %s holds no value for it — "+
				"Datadog shows a key once, so delete the key at Datadog and "+
				"run this again, or set %s by hand",
			seat.Handle, seat.TokenVar, seat.TokenVar)
		return out
	}
	minted, err := opts.Client.CreateAppKey(ctx, opts.Creds, account.ID, "crewlet")
	if err != nil {
		out.Err = fmt.Errorf("mint a key for %s: %w", seat.Handle,
			integration.Reject(err, Status(err)))
		return out
	}
	if err := opts.Sink.Record(ctx, seat.TokenVar, minted.Key); err != nil {
		// THE KEY EXISTS AND ITS VALUE IS NOW LOST, so this engine revokes
		// it rather than leaving it live.
		//
		// [provision.TokenSink]'s contract says why in its own words: "a
		// credential that exists and is not recorded is one nobody can
		// use and nobody will remember to remove". Left behind, it also
		// wedges this seat for good — the next pass finds a key it cannot
		// read a value for and reports the seat stuck, a state this
		// engine created and could not leave without somebody logging
		// into Datadog.
		out.Err = revoke(ctx, opts, seat, account.ID, minted, err)
		return out
	}
	out.KeyMinted = true
	return out
}

// revoke takes back a key this run minted and could not record.
//
// It returns the ORIGINAL failure with the revocation's outcome appended,
// never in place of it — the same rule [gitlab.rollback] states: the reason
// the run stopped is what an operator has to fix, and a cleanup error
// replacing it would hide the cause behind its consequence. Only when the
// revocation ALSO fails is anybody asked to delete a key by hand.
//
// DETACHED, because the failure is frequently the cancellation itself, and a
// revocation inheriting a dead context does nothing at all — which is exactly
// when the credential most needs taking back.
func revoke(
	ctx context.Context, opts Options, seat provision.Seat, accountID string,
	key AppKey, cause error,
) error {
	if err := opts.Client.DeleteAppKey(
		context.WithoutCancel(ctx), opts.Creds, accountID, key.ID,
	); err != nil {
		return fmt.Errorf(
			"minted a key for %s and could not record it (%w) AND could not "+
				"revoke it — it is live at Datadog, held by nobody: delete "+
				"the key named %q on %s's service account and run this again",
			seat.Handle, cause, key.Name, seat.Handle)
	}
	return fmt.Errorf(
		"minted a key for %s and could not record it, so it was revoked "+
			"again — nothing is left at Datadog and the next pass will mint "+
			"another: %w", seat.Handle, cause)
}

// roleIDOf resolves the configured role, refusing rather than guessing.
func roleIDOf(ctx context.Context, opts Options, name string) (string, error) {
	roles, err := opts.Client.ListRoles(ctx, opts.Creds, name)
	if err != nil {
		return "", fmt.Errorf("datadog: read the roles: %w",
			integration.Reject(err, Status(err)))
	}
	for _, role := range roles {
		if role.Name == name {
			return role.ID, nil
		}
	}
	// REFUSED, not defaulted. Creating accounts under whatever role
	// happened to match would grant an agent access nobody asked for, and
	// the wrong direction is the dangerous one.
	return "", fmt.Errorf(
		"datadog: this organization has no role named %q, so no account can "+
			"be created holding it — name a role the organization has in "+
			"integrations.datadog.provisioning.role", name)
}

func emailDomainOf(cfg *config.Datadog) string {
	if cfg != nil && cfg.Provisioning != nil && cfg.Provisioning.EmailDomain != "" {
		return cfg.Provisioning.EmailDomain
	}
	return DefaultEmailDomain
}

// Findings is what a pass observed, in the shared vocabulary.
//
// The pass reports what it SAW rather than what it did: a seat with an
// account and a key is nothing to say, and every entry here is a thing an
// operator can act on.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	out := []integration.Finding{}
	if hook := r.Webhook; hook != nil && hook.Err != nil {
		// INGRESS, NOT IDENTITY. A definition that could not be written is
		// every alert this company has going nowhere, which is a different
		// thing from one agent lacking an account and is owned by whoever
		// holds the Datadog keys.
		out = append(out, integration.Finding{
			Kind:    integration.FindingIngressBlocked,
			Subject: hook.Name,
			Detail:  hook.Err.Error(),
		})
	}
	for _, seat := range r.Seats {
		switch {
		case seat.Err != nil:
			out = append(out, integration.Finding{
				Kind:    integration.FindingIdentityFailed,
				Subject: seat.Handle,
				Detail:  seat.Err.Error(),
			})
		case seat.AccountID == "":
			out = append(out, integration.Finding{
				Kind:    integration.FindingIdentityMissing,
				Subject: seat.Handle,
				Detail: seat.Handle + " has no Datadog account yet, so nothing " +
					"authenticates as it there",
			})
		}
	}
	return out
}
