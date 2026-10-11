package atlassian

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/provision"
)

var log = logging.Get("atlassian")

// Options is what one provisioning pass needs.
type Options struct {
	// Client talks to the organization's admin APIs.
	Client *Client

	// OrgID and Key are the organization and its unscoped API key.
	OrgID string
	Key   string

	// Plan names the seats wanting an identity, from [PlanFor].
	Plan *provision.Plan

	// Sink records what is minted. A sink that cannot mint is a DRY RUN:
	// the pass reads what exists and creates nothing, which is what a check
	// is.
	//
	// THE TEST IS [provision.CanMint], NOT A NIL CHECK, and the difference
	// is a whole posture rather than an edge case: nil is the command
	// line's check, while the reconcile loop hands a node with no keyring
	// [provision.ReadOnly], which is not nil and can seal nothing. See
	// [Result.NoKeyring].
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

	// NoKeyring is a pass this node could not run in full because it has
	// nowhere to seal what provisioning creates.
	//
	// A STATE, not an error, and the distinction is what an operator is
	// told: a fault reports the engine working on it and is retried for
	// ever, where this never resolves until somebody sets secrets.keys.
	// See [provision.CanMint], and [Result.Findings] for what it becomes.
	NoKeyring bool
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

	// NotReady is a seat whose account exists and which Atlassian will not
	// grant product access to YET, because it has only just been created.
	//
	// A STATE, NOT AN ERROR, and the distinction is the whole reason this
	// field exists. It used to travel in Err, with a sentence saying exactly
	// what it is — "waiting for Atlassian to make its new account grantable"
	// — and [Result.Findings] turned any Err into an identity_failed, which
	// classifies as DEGRADED and owed by a VENDOR ADMIN. So the card said
	// "Action required" and "you, at the third-party app" about a seat nobody
	// could do anything for, on the brisk vendor-admin cadence, over a
	// condition the very next pass clears on its own.
	//
	// Carried as a value so the classification cannot be lost again: an error
	// string is a sentence a reader has to interpret, and every path that
	// produces one here is a failure except this.
	NotReady bool

	// Granted reports product access this pass gave the account, which is
	// NOT the same as product access that works.
	//
	// A GRANT IS ACCEPTED IMMEDIATELY AND APPLIED OVER THE NEXT MINUTE. The
	// invite POST answers success, the token this pass then mints is
	// accepted by Atlassian's gateway, and Jira itself refuses it with its
	// own 401 ("Client must be authenticated to access this resource") until
	// the grant has propagated. Measured on a live site: about seventy
	// seconds, every time — a reconnect creates a new account, so this
	// window is hit on every reconnect rather than occasionally.
	//
	// Reported as a seat still coming up for the same reason [NotReady] is,
	// and the failure it prevents is the same one: without it this pass
	// declared the seat done, the products said 401, and the card told an
	// operator an ADMIN had to act on an account that was working a minute
	// later. See [Result.Findings].
	Granted bool

	// Err is why this seat could not be provisioned, if it could not.
	Err error

	// AtVendor reports an Err a person settles AT ATLASSIAN — a create, a
	// grant or a mint the organization refused, or a token it minted that
	// this pass could not keep and could not revoke — rather than one this
	// engine's own sealed store refused, which is this deployment's.
	//
	// SET WHERE THE ERROR IS RAISED, as [datadog.SeatResult.AtVendor] is and
	// for its reason: only that frame knows which it was, and telling them
	// apart from the message afterwards would be matching on error text. It
	// decides whose finding the failure is — an Atlassian admin's failed
	// identity, or the company admin's config fault — where every seat
	// failure used to be the first, so a keyring that refused to seal the
	// address Atlassian assigned sent an Atlassian admin looking at an
	// account that was fine.
	AtVendor bool
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
//
// # THE SINK IS COMPLETED WHATEVER THE PASS DID
//
// [provision.TokenSink.Flush] is the point at which what a run sealed STANDS,
// and under the reconcile loop it is load-bearing rather than ceremonial: the
// loop's own sink rebuilds the engine's `${VAR}` snapshot there, and ONLY
// there. This pass reached it on no path at all — it created an account,
// minted the API token that account acts with, sealed the token and the
// address Atlassian assigned it, and returned without ever completing the run.
//
// On this pass that state is PERMANENT rather than self-healing, which is what
// makes it worse than the same omission elsewhere. Everywhere a sealed-but-
// unannounced value is simply re-minted next tick the damage is a rotation on
// a timer; here the next pass reads the value back through
// [provision.TokenSink.Value] (see the mint in [reconcileSeat]), finds it
// held, and therefore never Records again — so the sink never seals again, so
// the snapshot is never rebuilt, for the life of the deployment. Every agent
// then authenticates at Jira and Confluence with the literal text
// "${ATLASSIAN_TOKEN_…}" as its password, every call is refused with a 401
// naming nothing, and this pass reports the seat provisioned on every tick
// because as far as it can see the credential is there.
//
// So the flush happens on EVERY exit path, and it is not conditioned on
// whether this pass minted: a sink that recorded nothing has nothing to make
// durable and says so cheaply, where a "did we mint" flag is one more thing
// that has to stay in step with a mint several call frames away.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	res, err := reconcile(ctx, opts)
	// A NIL SINK IS THE COMMAND LINE'S CHECK and there is nothing to
	// complete. It is not [provision.ReadOnly], which is a sink and whose
	// Flush answers cheaply — see [Options.Sink] for why the two are
	// different facts.
	if opts.Sink == nil {
		return res, err
	}
	// WITHOUT THE CALLER'S CANCELLATION when the pass is already failing,
	// which is the rule this tree applies to every rollback and teardown:
	// the failure being completed is frequently the cancellation itself — a
	// node draining mid-pass, having just sealed an agent's token — and a
	// completion that inherits a dead context does nothing at all, which is
	// the exact state above. A pass that SUCCEEDED keeps its caller's
	// deadline, because there is nothing to rescue and a completion that
	// outlives the request it belongs to is its own problem.
	flushCtx := ctx
	if err != nil {
		flushCtx = context.WithoutCancel(ctx)
	}
	if flushErr := opts.Sink.Flush(flushCtx); flushErr != nil {
		// JOINED, NEVER SUBSTITUTED. The pass's own error is the root cause
		// and callers route on it — [integration.Reject] classifies it, and
		// errors.Is against [integration.ErrCredentialRejected] decides
		// whether an operator is sent to rotate the organization key or told
		// to wait — so replacing it with a completion failure sends them to
		// the wrong place, and dropping the completion failure hides a sink
		// this deployment can no longer seal into. errors.Join keeps both
		// reachable to errors.Is and errors.As.
		return res, errors.Join(err, fmt.Errorf("atlassian: complete the run: %w", flushErr))
	}
	return res, err
}

// reconcile is the pass itself, with no opinion about the sink's completion:
// see [Reconcile], which owns that on every path out of here.
func reconcile(ctx context.Context, opts Options) (*Result, error) {
	// A CANCELLED PASS HAS OBSERVED NOTHING, so it must RAISE rather than
	// answer with a Result.
	//
	// Checked here although a dead context does fail the first request
	// anyway, because of WHAT it fails as: that request is DiscoverSite,
	// and the sentence wrapped around its error names
	// integrations.atlassian.api_key as the thing that could not be
	// verified. A node draining would write that into the fleet's status,
	// sending an operator to rotate a key that is fine — the same false
	// claim [integration.Refusal] exists to stop one level down.
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
	res := &Result{
		Site: *site,
		// A SINK THAT EXISTS AND CANNOT SEAL is a node with no keyring, and
		// it is a different fact from the nil sink of a command-line check.
		// Both create nothing; only one is something an operator has to fix.
		NoKeyring: opts.Sink != nil && !provision.CanMint(opts.Sink),
	}
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
		res.Seats = append(res.Seats, reconcileSeat(ctx, opts, seat, site.CloudID, byHandle))
		// AND CANCELLED PART-WAY THROUGH IS THE SAME ANSWER, which the
		// check at the top cannot give.
		//
		// Every failure inside reconcileSeat lands in [SeatResult.Err] and
		// is reported as a seat whose account could not be created, so a
		// context cancelled between two of Atlassian's calls tells an
		// operator their agents' identities are broken when what actually
		// happened is that this node was draining. That answer is written
		// to the fleet's status and the next node to hold the duty reads
		// it. Checked after each seat rather than only after the loop so a
		// company with fifty seats does not spend forty-nine rounds of
		// failing calls to arrive at it.
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("atlassian: %w", err)
		}
	}
	return res, nil
}

// reconcileSeat brings one seat's identity into line.
func reconcileSeat(
	ctx context.Context, opts Options, seat provision.Seat, site string,
	byHandle map[string]ServiceAccount,
) SeatResult {
	out := SeatResult{Handle: seat.Handle}
	account, found := byHandle[seat.Handle]

	// WHETHER THIS PASS MAY WRITE AT ALL, asked once and by
	// [provision.CanMint] rather than by a nil check.
	//
	// The reconcile loop hands a node with no keyring [provision.ReadOnly],
	// which is not nil. On the nil check this was, such a node created an
	// account at Atlassian, granted it, and then failed at the first Record
	// with [provision.ErrNoSink] — reporting EVERY seat as a failure over a
	// perfectly converged company, on a timer, and leaving behind an
	// identity nothing had recorded a credential for. See
	// [Result.NoKeyring] for what it reports instead.
	writing := provision.CanMint(opts.Sink)

	switch {
	case found:
		out.AccountID = account.ID
	case !writing:
		// A DRY RUN CREATES NOTHING. The seat is reported without an
		// account, which is the true answer to what a check asks.
		return out
	default:
		created, err := opts.Client.CreateServiceAccount(ctx, opts.Key, opts.OrgID,
			AccountName(seat.Role, seat.Handle), "")
		if err != nil {
			out.Err = fmt.Errorf("atlassian: create the account for %s: %w", seat.Handle, err)
			out.AtVendor = true
			return out
		}
		account, out.AccountID, out.Created = *created, created.ID, true
	}

	// A DRY RUN WRITES NOTHING PAST THIS POINT, which is what a check is:
	// everything below either mutates Atlassian or seals a value.
	if !writing {
		return out
	}

	// WHAT THE ACCOUNT ALREADY HOLDS, read once and spent twice — the grant
	// below turns on it, and so does the mint at the end.
	tokens := heldTokens(ctx, opts, out)

	// GRANTED WHERE THERE IS NO EVIDENCE IT ALREADY WAS.
	//
	// An account with no product access is refused by the product REST API
	// with a 401 that reads exactly like a bad credential: the token is fine
	// and there is nothing it may open. So a grant that failed on the pass
	// that created the account MUST still be retried — Atlassian refuses to
	// grant one it has only just made — and this used to be done by granting
	// on EVERY pass. Granting is idempotent, so that was harmless at
	// Atlassian and wrong here: it is a write, once per seat, on a company
	// that needs nothing, from a loop whose whole promise is that it is safe
	// to leave switched on.
	//
	// WHY THE EVIDENCE IS A TOKEN COUNT RATHER THAN A READ OF THE GRANT.
	// Atlassian exposes no read of a service account's product access.
	// account-management answers no GET for a single account, the documented
	// service-account listing carries identity and metadata only, and the
	// routes that DO return product access are directory-user routes — which
	// a service account is not, as [invitePath] records from what a group-add
	// answered. What this pass has instead is a fact about its own order:
	// the grant strictly precedes the mint below and every step between them
	// returns on failure, so an account holding at least one API token was
	// necessarily granted by an earlier pass. That is weaker than a real
	// read and the weakness is named: an account granted but never minted is
	// granted a second time, which costs one idempotent request.
	if !tokens.granted() {
		switch err := opts.Client.Grant(
			ctx, opts.Key, opts.OrgID, out.AccountID, GrantsFor(site),
		); {
		case err == nil:
			// ACCEPTED, NOT YET IN FORCE. See [SeatResult.Granted].
			out.Granted = true
		case errors.Is(err, ErrAccountNotReady):
			// The next pass grants it. Reported as a seat still coming up
			// rather than a failure, because that is what it is — and now
			// carried as [SeatResult.NotReady] rather than as an Err, which
			// is what made that sentence come out as "Action required".
			out.NotReady = true
			return out
		default:
			out.Err = fmt.Errorf("atlassian: grant %s product access: %w", seat.Handle, err)
			out.AtVendor = true
			return out
		}
	}

	// THE ADDRESS FIRST, and for any account this pass has — not only one it
	// just made. Atlassian's product APIs authenticate as Basic
	// base64(address:token), so a seat holding the token alone is refused
	// with a 403 that reads as a broken credential. Atlassian invents the
	// address, so this is the only place it can come from, and a seat can
	// hold a token with no address recorded: its address slot was added or
	// renamed after the token was minted (the plan's own note asks for
	// exactly that), or Atlassian reported no address, or a different one,
	// on the pass that minted it.
	//
	// AND ONLY ON A DIFFERENCE. The sealed store is the company's, shared by
	// the whole fleet, and a Record is a real write with an author and a
	// timestamp on it — so re-sealing the same address once per seat per
	// reconcile interval, for ever, is exactly the kind of write this loop
	// promises not to make. It is not an HTTP call to Atlassian, and that
	// makes no difference to the person who has to explain the audit trail.
	if seat.EmailVar != "" && account.Email != "" &&
		!recorded(ctx, opts, seat.EmailVar, account.Email) {
		if err := opts.Sink.Record(ctx, seat.EmailVar, account.Email); err != nil {
			out.Err = fmt.Errorf("atlassian: record %s: %w", seat.EmailVar, err)
			return out
		}
	}

	// THE TOKEN IS MINTED ONLY WHERE THERE IS NOWHERE TO READ ONE FROM. A
	// mint is not idempotent — Atlassian issues a new credential every time
	// and shows it once — so minting on every pass would rotate the token
	// every agent is authenticating with, on the reconcile loop's timer.
	// THREE-VALUED, and the middle answer is the one that matters: held,
	// definitively not held, and "the store could not say". Minting on the
	// third would issue a new credential over one that already exists and
	// that every running seat is authenticating with.
	_, held, err := opts.Sink.Value(ctx, seat.TokenVar)
	if err != nil {
		out.Err = fmt.Errorf("atlassian: read %s: %w", seat.TokenVar, err)
		return out
	}
	if held && !orphaned(tokens, seat.Handle) {
		return out
	}
	// A SEAT THAT REACHES THE MINT HOLDS NOTHING THAT WORKS: nothing is
	// sealed for it, or what is sealed has no token behind it because the
	// account holds none. So whatever this engine minted on the account is
	// held by nobody, and it goes before another is minted beside it. No
	// `held` test guards this, because the second case has nothing to
	// revoke; see [revokeUnheld].
	if err = revokeUnheld(ctx, opts, out.AccountID, seat.Handle, tokens); err != nil {
		out.Err, out.AtVendor = err, true
		return out
	}
	label := TokenLabel(seat.Handle, opts.now())
	token, err := opts.Client.MintToken(ctx, opts.Key, out.AccountID, label, opts.now())
	switch {
	case errors.Is(err, ErrTokenNotReturned):
		// A TOKEN THAT MAY EXIST WITH NO VALUE ANYBODY HOLDS, which is the
		// failure below reached from the other side: taken back the same way,
		// and Atlassian's either way, since it was Atlassian's answer that
		// came back empty.
		out.AtVendor, out.Err = revoke(ctx, opts, seat, out.AccountID, label, true, err)
		return out
	case err != nil:
		out.Err = fmt.Errorf("atlassian: mint a token for %s: %w", seat.Handle, err)
		out.AtVendor = true
		return out
	}
	if err := opts.Sink.Record(ctx, seat.TokenVar, token.Token); err != nil {
		// THE TOKEN EXISTS AND ITS VALUE IS NOW LOST, so this engine revokes
		// it rather than leaving it live.
		//
		// [provision.TokenSink]'s contract says why in its own words: "a
		// credential that exists and is not recorded is one nobody can use
		// and nobody will remember to remove". Atlassian shows a token once,
		// so nothing can ever read this one back, and every pass that met a
		// sealed store refusing writes left one more of them on the account,
		// valid for [TokenLifetime].
		//
		// AND A RECORD THAT FAILED AFTER IT LANDED COSTS NOTHING HERE. A
		// write reported failed may still be durable — a timeout after the
		// commit — and then the sealed value names a token this revokes. The
		// next pass finds it held over an account holding none, which is
		// [orphaned]'s case, and mints over it; the alternative direction is
		// a live credential on the account for the better part of a year.
		out.AtVendor, out.Err = revoke(ctx, opts, seat, out.AccountID, label, false,
			fmt.Errorf("record %s: %w", seat.TokenVar, err))
		return out
	}
	out.TokenMinted = true
	return out
}

// revoke takes back a token this pass minted and could not keep.
//
// It returns the ORIGINAL failure with the revocation's outcome around it,
// never in place of it — the rule [datadog]'s own revoke states: the reason
// the pass stopped is what an operator has to fix, and a cleanup error
// replacing it would hide the cause behind its consequence. Only when the
// revocation ALSO fails is anybody asked to delete a token by hand.
//
// DETACHED, because the failure is frequently the cancellation itself — a node
// draining as it sealed — and a revocation inheriting a dead context does
// nothing at all, which is exactly when the credential most needs taking back.
//
// The bool is [SeatResult.AtVendor]: the cause's own owner when the token is
// gone, and an Atlassian admin's whenever it may not be, because a credential
// live at Atlassian and held by nobody is the one thing in this failure that
// only somebody there can remove.
func revoke(
	ctx context.Context, opts Options, seat provision.Seat, accountID, label string,
	causeAtVendor bool, cause error,
) (bool, error) {
	if err := revokeLabelled(context.WithoutCancel(ctx), opts, accountID, label); err != nil {
		// THE INSTRUCTION FIRST and both causes after it, because a
		// finding's sentence is cut at [integration.MaxDetailLength] and an
		// Atlassian refusal quoted in the middle would push the one thing a
		// person can act on past the cut.
		return true, fmt.Errorf(
			"atlassian: the API token labelled %q on %s's service account may be "+
				"live at Atlassian and held by nobody — delete it there; the next "+
				"pass tries again too. It was minted and could not be kept (%w), and "+
				"revoking it failed (%w)",
			label, seat.Handle, cause, err)
	}
	return causeAtVendor, fmt.Errorf(
		"atlassian: minted a token for %s and could not keep it, so it was revoked "+
			"again — nothing is left at Atlassian and the next pass mints another: %w",
		seat.Handle, cause)
}

// revokeLabelled revokes the one token on an account carrying this label.
//
// BY THE LABEL, through the documented listing — see [TokenLabel] for why
// that and not an id off the mint's answer.
//
// A LABEL THE LISTING DOES NOT CARRY IS NOT A REVOKED TOKEN. The token was
// minted moments ago, or its mint answered with no value, and either way the
// two readings of its absence are a listing that has not caught up and a
// token that was never made. Only the second is safe to report as nothing
// left at Atlassian, and nothing here can tell them apart — so it is refused
// as a revocation that did not happen, the direction that asks a person to
// look rather than the one that tells them a live credential is gone.
func revokeLabelled(ctx context.Context, opts Options, accountID, label string) error {
	tokens, err := opts.Client.ListTokens(ctx, opts.Key, accountID)
	if err != nil {
		return fmt.Errorf("list the account's API tokens: %w", err)
	}
	for _, token := range tokens {
		if token.Label != label {
			continue
		}
		return revokeListed(ctx, opts, accountID, token)
	}
	return fmt.Errorf("the account's API token listing carries no token labelled %q", label)
}

// revokeListed revokes one token as the listing described it.
//
// AN ID IS REQUIRED, not assumed: the route is the token's id under the
// account, and a listing that carried none would send the DELETE to the
// collection itself.
func revokeListed(ctx context.Context, opts Options, accountID string, token APIToken) error {
	if token.ID == "" {
		return fmt.Errorf("the account's API token listing carries the token labelled "+
			"%q with no id to revoke it by", token.Label)
	}
	return opts.Client.RevokeToken(ctx, opts.Key, accountID, token.ID)
}

// revokeUnheld revokes every token this engine minted on a seat's account,
// for a seat holding none of them.
//
// # Why a token nothing holds is revoked rather than minted beside
//
// Atlassian shows a token's value once, so a token on this seat's own account
// with nothing sealed for the seat is a value NOBODY IN THIS COMPANY HOLDS: the
// seat authenticates from its ${VAR}, and the ${VAR} is empty. The pass mints
// a working credential next whatever happens here, and used to leave each one
// of these where it was — so every token a failed record, an empty mint
// answer, a refused revocation or a deleted secret stranded stayed live on the
// account for the [TokenLifetime] it was minted with, and the instruction to
// delete one by hand was undone by the very next pass, which minted again and
// reported the seat ready. This is what makes that instruction optional: a
// stranded token is gone on the pass after the one that stranded it.
//
// ITS OWN TOKENS ONLY, by the label every mint here stamps ([MintedFor]). A
// token somebody made on the account under a name of their own is not this
// engine's to take away, and the seat is fixed regardless: minting is
// additive.
//
// A FAILED REVOCATION STOPS THE SEAT short of the mint, as [datadog]'s repair
// does: the next pass tries again, and minting first would bury the stray for
// good — every later pass would find a token held and the account holding
// tokens, which is converged as far as anything can establish, and never look
// at it again.
//
// "CANNOT TELL" REVOKES NOTHING, by construction rather than by a check: a
// listing that failed is a [tokenList] carrying no tokens, which is evidence
// of none, and the mint goes ahead — safe, because nothing is held for this
// seat to be rotated away.
func revokeUnheld(
	ctx context.Context, opts Options, accountID, handle string, tokens tokenList,
) error {
	for _, token := range tokens.tokens {
		if !MintedFor(token.Label, handle) {
			continue
		}
		if err := revokeListed(ctx, opts, accountID, token); err != nil {
			return fmt.Errorf(
				"atlassian: revoke %s's API token labelled %q, which nothing in this "+
					"company holds, before minting the one that replaces it: %w",
				handle, token.Label, err)
		}
		log.Info("atlassian_unheld_token_revoked", "seat", handle, "label", token.Label,
			"detail", "this seat's Atlassian account held an API token this engine "+
				"minted and nothing in this company holds — Atlassian shows a token's "+
				"value once, and the seat's credential is not sealed. Revoked, and a "+
				"replacement is minted; nothing has to be done by hand")
	}
	return nil
}

// GrantPropagation is how long Atlassian takes to make a new service
// account's product access actually work.
//
// # Why a window rather than a read
//
// Atlassian exposes no read of a service account's product access at all —
// see the grant block in [reconcileSeat] for the routes that were tried — so
// nothing can ask whether a grant is in force. What CAN be observed is the
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

// tokenList is the API tokens one account holds, or the fact that Atlassian
// could not say.
//
// # Three-valued, and the two decisions it settles go opposite ways on the
// third
//
// One read answers both of a seat's writes, and "the vendor could not say"
// means something different to each. Each direction is the safe one for its
// own write, and the asymmetry is the whole reason this is a type rather
// than a slice, whose nil would read as an account holding nothing:
//
//   - THE GRANT. Cannot-tell GRANTS. Granting is idempotent, so a redundant
//     one costs a single request, while a missing one leaves an account that
//     can reach nothing and reports it as a 401 that reads like a bad
//     credential.
//   - THE MINT. Cannot-tell LEAVES IT ALONE. Minting is not idempotent —
//     Atlassian issues a new credential every time and shows it once — so
//     minting on a failed read rotates the token every running seat is
//     authenticating with, every time Atlassian is briefly unreachable, on
//     the loop's timer.
//
// Collapsing either into a bool is the mistake this engine's coordination
// layer exists to avoid: held, definitively not held, and "the store could
// not say" are three different facts, and folding the last into the second
// is how a healthy company gets torn down over a two-second blip.
//
// And the revocation of what a seat holding nothing left on its account
// ([revokeUnheld]) takes the mint's direction, for the mint's reason: a
// revocation is not idempotent either, so cannot-tell revokes nothing.
type tokenList struct {
	tokens []APIToken
	known  bool
}

// granted reports an account an earlier pass certainly finished with, which
// is the only evidence available that its product access was ever applied.
// See the grant in [reconcileSeat] for why the evidence is this and not a
// read of the grant itself.
func (c tokenList) granted() bool { return c.known && len(c.tokens) > 0 }

// none reports an account that definitely holds no API token at all.
func (c tokenList) none() bool { return c.known && len(c.tokens) == 0 }

// heldTokens asks Atlassian which API tokens one account holds.
//
// AN ACCOUNT THIS PASS JUST CREATED IS NOT ASKED. Atlassian made it seconds
// ago, so it holds none and the answer is known without a request — which
// also keeps the create-then-grant-then-mint path exactly one call per step.
//
// An account with no id is UNKNOWN rather than empty: Atlassian answered a
// create or a listing without one, so there is nothing to read and nothing to
// conclude, and the two writes downstream take their cannot-tell directions.
func heldTokens(ctx context.Context, opts Options, seat SeatResult) tokenList {
	if seat.AccountID == "" {
		return tokenList{}
	}
	if seat.Created {
		return tokenList{known: true}
	}
	tokens, err := opts.Client.ListTokens(ctx, opts.Key, seat.AccountID)
	if err != nil {
		log.Debug("atlassian_token_check_failed", "seat", seat.Handle, "error", err.Error(),
			"detail", "the held credential is left alone, no token is revoked and "+
				"the account's product access is granted again; a failed read is "+
				"evidence of none of them")
		return tokenList{}
	}
	return tokenList{tokens: tokens, known: true}
}

// recorded reports a sink that already holds exactly this value.
//
// "CANNOT TELL" WRITES HERE, which is the opposite of what the mint does with
// the same answer, and the asymmetry is deliberate. Re-recording the address
// Atlassian assigned overwrites a value with itself; re-minting a token
// issues a new credential and revokes the one every running seat is
// authenticating with. So an unreadable store costs one pointless write here
// and would cost an outage there.
func recorded(ctx context.Context, opts Options, name, value string) bool {
	current, held, err := opts.Sink.Value(ctx, name)
	return err == nil && held && current == value
}

// orphaned reports a held credential with no live token behind it, so the
// pass mints over it.
//
// # Why a held credential is not necessarily a working one
//
// TWO THINGS PUT A SEAT HERE and the observable is identical, which is why
// this says neither of them and the old message said the wrong one.
//
// One is a reconnect. Disconnecting with "remove accounts" deletes them at
// Atlassian and leaves the minted tokens in the sealed store: the store is
// the company's, and a teardown that emptied it would take values an operator
// may have put there by hand. A later reconnect then creates a NEW account
// and finds a credential already held for the seat, so it mints nothing, and
// every call the seat makes is refused with a 401 naming nothing. The
// dashboard reports "has no Jira account" while the account plainly exists,
// and the only cure was deleting the secret by hand.
//
// The other is somebody revoking the token at Atlassian — the ordinary
// administrative gesture this engine's settled cadence exists to notice. The
// ACCOUNT is untouched there, and the log line said it "no longer exists",
// which sent an operator who had deleted one token looking for a deleted
// account. Measured: exactly that, on a live deployment.
//
// # It asks the vendor, not the credential
//
// Atlassian shows a token's value once, so nothing can check that the stored
// string is one of the account's. What it can check is that the account has
// NO tokens at all, which is true of an account this engine has just created
// and false of every one it has finished with. That is the whole test — and
// it is the whole of what the message may claim.
//
// # "Cannot tell" leaves it alone
//
// A failed list is not evidence of anything, and minting on it would rotate a
// working credential every time Atlassian was briefly unreachable, on a
// timer. The three-valued rule this engine applies to ownership applies here
// for the same reason: only a definite answer acts.
func orphaned(tokens tokenList, handle string) bool {
	if !tokens.none() {
		return false
	}
	log.Info("atlassian_seat_token_replaced", "seat", handle,
		"detail", "this seat's Atlassian account holds no API token, so the "+
			"credential sealed for it cannot authenticate — either the token "+
			"was revoked at Atlassian, or the account was recreated after a "+
			"disconnect and the sealed value belongs to the old one. Minting a "+
			"replacement; nothing has to be done by hand")
	return true
}

// Findings is what this pass has to report to the reconcile status.
func (r *Result) Findings() []integration.Finding {
	if r == nil {
		return nil
	}
	out := []integration.Finding{}
	// THE PASS COULD NOT DO ITS WORK AT ALL, said as the operator's own task
	// rather than as a fault the engine is retrying. Provisioning here
	// creates an ACCOUNT and then mints a token on it, so a node with
	// nowhere to seal the token must not create the account either — and no
	// pass on this node will ever get further until somebody sets
	// secrets.keys, which a retry cannot bring about. See
	// [provision.CanMint].
	//
	// A CONFIG FAULT — the bootstrap file's keyring — not the missing
	// credential it was filed as, which is a ${VAR} pointing at nothing,
	// reads as Failed, and offered the organization key this deployment
	// already holds as the field that clears it.
	if r.NoKeyring {
		out = append(out, integration.Finding{
			Kind:    integration.FindingConfigFault,
			Subject: "secrets.keys",
			Detail: "this node has no keyring, so an Atlassian API token minted " +
				"for a seat could not be sealed and no service account was " +
				"created — set secrets.keys in the bootstrap configuration",
		})
	}
	for _, seat := range r.Seats {
		switch {
		case seat.Err != nil:
			// A FAILURE OUTRANKS EVERY WAIT, and it has to be FIRST.
			//
			// [reconcileSeat] sets Granted and then keeps going — the
			// address Record, the token-slot read, the mint and the token
			// Record all follow it, and each sets Err without clearing
			// Granted. With the Granted arm ahead of this one, all four of
			// those failures were reported as [integration.FindingGrantPending]
			// — PhaseActivating / ActorProvider, the one classification that
			// tells an operator NOBODY has to act — under the sentence
			// "Nothing has to be done; the next pass checks again". A seat
			// whose mint is being refused therefore sat there for ever, with
			// its error string dropped, while the card said the provider was
			// working on it.
			//
			// NotReady is safe either way because it returns immediately,
			// and AccountID == "" is the create that failed, whose Err is
			// the better sentence. So the ordering rule is simply: what
			// went wrong beats what is being waited for.
			//
			// AND WHOSE IT IS FOLLOWS WHERE IT WENT WRONG: a create, grant
			// or mint Atlassian refused is an Atlassian admin's failed
			// identity; a value this engine's own sealed store would not
			// read or keep is this deployment's config fault. See
			// [SeatResult.AtVendor].
			kind := integration.FindingIdentityFailed
			if !seat.AtVendor {
				kind = integration.FindingConfigFault
			}
			out = append(out, integration.Finding{
				Kind: kind, Subject: seat.Handle,
				Detail: seat.Err.Error(),
			})
		case seat.Granted:
			// THE GRANT LANDED AND ATLASSIAN HAS NOT APPLIED IT YET, which
			// is a provider's wait and not a finished seat.
			//
			// This pass used to report nothing here, so a seat whose
			// account it had created seconds earlier was reported done —
			// and the products it had just been granted refused its brand
			// new credential for the next minute or so. Jira said the seat
			// had no account, this card said the surface was ready, and the
			// roster said the seat was ready: three answers, one transient
			// cause, none of them anything anybody could act on.
			out = append(out, integration.Finding{
				Kind: integration.FindingGrantPending, Subject: seat.Handle,
				Detail: "Atlassian has accepted " + seat.Handle + "'s product " +
					"access and is still applying it, so Jira and Confluence " +
					"refuse this account's credential for about a minute. " +
					"Nothing has to be done; the next pass checks again",
			})
		case seat.NotReady:
			// NOBODY HAS TO ACT. Atlassian has the account and has not
			// finished making it grantable; the next pass grants it. This
			// is [integration.FindingGrantPending]'s exact case, and until
			// now that kind had no producer anywhere in the tree while this
			// condition was reported as a failed identity.
			out = append(out, integration.Finding{
				Kind: integration.FindingGrantPending, Subject: seat.Handle,
				Detail: seat.Handle + " has its Atlassian account and is waiting " +
					"for Atlassian to make it grantable, which the next pass does",
			})
		case seat.AccountID == "":
			out = append(out, integration.Finding{
				Kind: integration.FindingIdentityMissing, Subject: seat.Handle,
				Detail: seat.Handle + " has no Atlassian account, so nothing it does " +
					"in Jira or Confluence is its own",
			})
		}
	}
	// THE PLAN'S CAVEATS ARE THE COMPANY DOCUMENT'S: a seat whose mcp_env
	// slot is not a whole ${VAR}, so a minted token or the address Atlassian
	// assigns has nowhere to be written. Every one of them is fixed by an
	// edit here, so a config fault, the company admin's. They were short
	// grants — a seat holding less than its tier, which only a person AT
	// Atlassian can widen — and sent an Atlassian admin to look for access
	// that was never the problem.
	for _, note := range r.Notes {
		out = append(out, integration.Finding{
			Kind: integration.FindingConfigFault, Detail: note,
		})
	}
	return out
}
