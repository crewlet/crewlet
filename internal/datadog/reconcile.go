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
	//
	// A sink that exists and cannot mint ([provision.ReadOnly], which the
	// loop hands a node with no keyring) is a THIRD answer rather than
	// either of those — see [sealsNothing].
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

	// NoKeyring reports a node that was asked to provision identities and
	// cannot seal what it would mint, so it created nothing. See
	// [sealsNothing].
	NoKeyring bool

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
	// Enabled reports an account this pass turned back on, having
	// established that a previous disconnect of this engine's is what
	// disabled it. See [DisconnectedTitle].
	Enabled bool
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
	// A CANCELLED PASS OBSERVED NOTHING, AND IT SAYS SO HERE RATHER THAN BY
	// HAPPENING TO MAKE A NETWORK CALL FIRST.
	//
	// [integration.Reconciler]'s contract — and the clause
	// integrationtest drives for it — is that a cancelled pass raises
	// rather than answering with findings, because an error is a fault the
	// loop retries and an empty findings list is a statement that
	// everything is fine. This pass satisfied that only INCIDENTALLY: the
	// credential probe below is a network call, so a dead context failed
	// it. That left the clause resting on two things it should not rest
	// on. The first is the transport honouring cancellation, which is a
	// property of whatever [Client] was built with rather than of this
	// package. The second is worse and is reachable today: the arm above
	// the probe returns [integration.ErrNotConfigured], which the loop
	// reads as "forget this surface's status row" — so a node draining
	// during shutdown, against a company mid-edit, would delete the
	// fleet's Datadog status on its way out.
	//
	// And when the probe DID answer for it, it answered wrongly. The
	// sentence it produces names the organization credential pair and says
	// it "could not be verified", so a pass cancelled by a node shutting
	// down sent an operator to look at a key that was never asked about.
	// [integration.Refusal]'s own doc is about exactly that class of
	// mistake.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	if hook.Blocked != nil {
		res.Notes = append(res.Notes, hook.Blocked.Detail)
	}

	if opts.Plan == nil {
		return res, nil
	}
	res.Notes = append(res.Notes, opts.Plan.Notes...)

	// A NODE THAT CANNOT SEAL DOES NOT CREATE AN IDENTITY.
	//
	// [provision.CanMint]'s own doc names this shape as the hazard it
	// exists for: a pass that creates an ACCOUNT and then mints a key on
	// it has no rollback that can undo the first half, so reaching the
	// first Record on a sink that cannot record leaves a service account
	// at Datadog nobody asked for and nothing can ever authenticate as.
	// This pass did exactly that — once per seat, because the account it
	// made was then found by every later pass — and then reported each
	// seat as "could not read whether <VAR> already holds a key", a
	// sentence whose only instruction came from [provision.ErrNoSink] and
	// told an operator to name where minted credentials should go. None of
	// them named secrets.keys, which is the only thing that fixes it.
	//
	// AFTER THE WEBHOOK, which is what makes this different from the same
	// check in gitlab, jira and mattermost. The inbound half needs no
	// keyring at all — the definition is written with the organization
	// credentials the company document already carries — and it is the
	// half that carries the alerts, so a node that cannot provision
	// identities still keeps this company's monitors reaching a seat.
	if sealsNothing(opts.Sink) {
		res.NoKeyring = true
		return res, nil
	}

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
	// FLUSHED AFTER THE WHOLE PLAN, AND ON EVERY PATH THAT MINTED.
	//
	// [provision.TokenSink] hands nothing to the fleet until Flush, so a
	// key minted at Datadog and sealed but never flushed is the state the
	// sink's own contract legislates against: it exists, nobody holds it,
	// and the next pass finds a seat with a key it cannot read a value for
	// and mints another. The shape that produces it is a mid-pass `return
	// res, err` placed after the first mint, which is why nothing in the
	// loop above returns early — a seat that fails carries its failure in
	// its own [SeatResult] and the plan runs to the end.
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
	case found && account.Disabled && account.Title == DisconnectedTitle:
		// THIS ENGINE DISABLED IT, and connecting is the operator asking
		// for it back.
		//
		// Re-enabling used to be refused outright, on the reasoning that
		// undoing a decommission is somebody's decision and a pass that
		// quietly reversed it would fight that gesture on every tick. The
		// reasoning holds for an account a PERSON disabled at Datadog and
		// not for one this engine's own teardown turned off — and with
		// nothing recording which was which, both were the same
		// ambiguous bit and both were refused.
		//
		// What that cost: a disconnect-then-reconnect cycle could not
		// complete. The pass found the account it had disabled, reported
		// "re-enable it at Datadog: this pass will not", and the operator
		// either did it by hand or left another dead account behind.
		// About thirty-seven accumulated in one deployment.
		//
		// See [DisconnectedTitle] for why the provenance lives on the
		// account rather than on the surface's status row.
		if opts.Sink == nil {
			// A CHECK CHANGES NOTHING, which is the whole of what
			// distinguishes it from a pass.
			out.AccountID = account.ID
			return out
		}
		if err := opts.Client.EnableUser(ctx, opts.Creds, account.ID); err != nil {
			out.AccountID = account.ID
			out.Err = fmt.Errorf(
				"re-enable %s's Datadog service account (%s), which a previous "+
					"disconnect disabled: %w",
				seat.Handle, seat.Email, integration.Reject(err, Status(err)))
			return out
		}
		out.AccountID, out.Enabled = account.ID, true
	case found && account.Disabled:
		// A DISABLED ACCOUNT AUTHENTICATES AS NOTHING, and reading it as
		// an account is how this seat went silently dead.
		//
		// SOMEBODY ELSE DISABLED THIS ONE — it carries no marker of this
		// engine's — so it is reported rather than re-enabled, on the same
		// terms as an account that left the config: that was a deliberate
		// act at Datadog, and a pass that reversed it would fight the
		// gesture on every tick.
		out.AccountID = account.ID
		out.Err = fmt.Errorf(
			"%s's Datadog service account (%s) is disabled and was not disabled "+
				"by this engine, so everything it authenticates is refused — "+
				"re-enable it at Datadog if that was not deliberate",
			seat.Handle, seat.Email)
		return out
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
	if err != nil {
		out.Err = fmt.Errorf(
			"could not read whether %s already holds a key, so none was "+
				"minted: %w", seat.TokenVar, err)
		return out
	}

	// AND THE ACCOUNT IS ASKED EVEN WHEN A VALUE IS HELD, which is the
	// whole of this block's correctness.
	//
	// "A value is stored" used to return here, and it is not the same fact
	// as "the agent can authenticate". A teardown that KEEPS the account
	// still revokes the key this engine minted (see [Teardown]), and the
	// sealed value survives that by design — an operator's own decision,
	// which a plain disconnect lists rather than deletes. So a reconnect
	// found a value, minted nothing, and reported the seat ready over a
	// credential Datadog answers 403 for. Measured over five cycles
	// against a real organization: Connected in five seconds, seat
	// satisfied, zero findings, nought application keys on the account.
	// The same held for a key an administrator deleted by hand, for ever.
	//
	// Datadog is the last of the four provisioning surfaces to ask. Every
	// other one already does, and each had to be taught the same lesson:
	// atlassian counts the account's tokens ([orphaned]), gitlab and
	// mattermost ask the instance who the credential is (Client.verify).
	keys, listErr := opts.Client.ListAppKeys(ctx, opts.Creds, account.ID)
	switch {
	case listErr != nil && held:
		// CANNOT TELL, AND SOMETHING IS HELD, so nothing changes. This is
		// the same asymmetry [orphaned] draws and for the same reason: a
		// Datadog blip that read as "no keys" would rotate every agent's
		// credential on the loop's timer, which is an outage this engine
		// caused. The seat keeps what it has and the next pass asks again.
		return out
	case listErr != nil:
		out.Err = fmt.Errorf("read %s's keys: %w", seat.Handle,
			integration.Reject(listErr, Status(listErr)))
		return out
	case held && len(keys) > 0:
		// CONVERGED, as far as anything can establish. Datadog shows a
		// key's value once, so nothing can prove the stored string is one
		// of these — exactly the limit [atlassian.CountTokens] names. What
		// it CAN prove is the negative below.
		return out
	case held:
		// THE ACCOUNT HOLDS NO KEY AT ALL, so whatever is sealed for this
		// seat is not one of its keys and cannot be: it was revoked at
		// Datadog, or the account was recreated under it. Minted over,
		// which is the repair, and said out loud because a credential
		// changing underneath a running agent is worth a line.
		log.InfoContext(ctx, "datadog_seat_key_replaced", "seat", seat.Handle,
			"detail", "this seat's Datadog account holds no application key, "+
				"so the credential sealed for it cannot authenticate — either "+
				"the key was deleted at Datadog, or a disconnect revoked it. "+
				"Minting a replacement; nothing has to be done by hand")
	case len(keys) > 0:
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

// sealsNothing reports a sink that EXISTS and cannot record.
//
// DISTINCT FROM A NIL SINK, and collapsing the two would lose the more useful
// of them. Nil is a dry run, which is a posture the caller chose: it reads and
// reports without writing, and telling an operator which seats have no account
// is the whole point of one. This is a node that was ASKED to provision and
// has nowhere to put a credential, which is a fact about the deployment — the
// only thing to report is the bootstrap field that fixes it, and reading on
// would spend two calls per tick on an answer nothing can act on.
func sealsNothing(sink provision.TokenSink) bool {
	return sink != nil && !provision.CanMint(sink)
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
	if r.NoKeyring {
		// THE OPERATOR'S WORK, not a fault the engine is retrying. No
		// pass will ever provision a seat until somebody sets
		// secrets.keys, so reporting it as a wait leaves them watching a
		// retry that cannot succeed. The same finding gitlab, jira and
		// mattermost report, on the same subject, because it is one
		// bootstrap field rather than seven third-party app problems.
		out = append(out, integration.Finding{
			Kind:    integration.FindingCredentialMissing,
			Subject: "secrets.keys",
			Detail: "this node has no keyring, so an application key minted " +
				"for a seat could not be sealed and no service account was " +
				"created — set secrets.keys in the bootstrap configuration",
		})
	}
	if hook := r.Webhook; hook != nil {
		switch {
		case hook.Err != nil:
			// INGRESS, NOT IDENTITY. A definition that could not be
			// written is every alert this company has going nowhere,
			// which is a different thing from one agent lacking an
			// account and is owned by whoever holds the Datadog keys.
			out = append(out, integration.Finding{
				Kind:    integration.FindingIngressBlocked,
				Subject: hook.Name,
				Detail:  hook.Err.Error(),
			})
		case hook.Blocked != nil:
			// AND A DEFINITION THAT WAS NEVER ATTEMPTED IS THE SAME
			// OUTAGE, reached through this deployment's own
			// configuration rather than through Datadog refusing a
			// write. It was a note nothing read, so a company with no
			// public base URL or an unresolved webhook token had its
			// Datadog surface classified Ready over an inbound path
			// that did not exist — see [Unregistered].
			out = append(out, integration.Finding{
				Kind:    hook.Blocked.Kind,
				Subject: hook.Blocked.Subject,
				Detail:  hook.Blocked.Detail,
			})
		}
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
