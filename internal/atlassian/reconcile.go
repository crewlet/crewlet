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
		case errors.Is(err, ErrAccountNotReady):
			// The next pass grants it. Reported as a seat still coming up
			// rather than a failure, because that is what it is.
			out.Err = fmt.Errorf(
				"%s is waiting for Atlassian to make its new account grantable", seat.Handle)
			return out
		default:
			out.Err = fmt.Errorf("atlassian: grant %s product access: %w", seat.Handle, err)
			return out
		}
	}

	// THE ADDRESS FIRST, and for any account this pass has — not only one it
	// just made. Atlassian's product APIs authenticate as Basic
	// base64(address:token), so a seat holding the token alone is refused
	// with a 403 that reads as a broken credential. Atlassian invents the
	// address, so this is the only place it can come from, and a seat whose
	// token was minted by an earlier build has none recorded.
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

// tokenCount is how many API tokens one account holds, or the fact that
// Atlassian could not say.
//
// # Three-valued, and the two decisions it settles go opposite ways on the
// third
//
// One read answers both of a seat's writes, and "the vendor could not say"
// means something different to each. Each direction is the safe one for its
// own write, and the asymmetry is the whole reason this is a type rather
// than an int:
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
type tokenCount struct {
	n     int
	known bool
}

// granted reports an account an earlier pass certainly finished with, which
// is the only evidence available that its product access was ever applied.
// See the grant in [reconcileSeat] for why the evidence is this and not a
// read of the grant itself.
func (c tokenCount) granted() bool { return c.known && c.n > 0 }

// none reports an account that definitely holds no API token at all.
func (c tokenCount) none() bool { return c.known && c.n == 0 }

// heldTokens asks Atlassian how many API tokens one account holds.
//
// AN ACCOUNT THIS PASS JUST CREATED IS NOT ASKED. Atlassian made it seconds
// ago, so it holds none and the answer is known without a request — which
// also keeps the create-then-grant-then-mint path exactly one call per step.
//
// An account with no id is UNKNOWN rather than empty: Atlassian answered a
// create or a listing without one, so there is nothing to read and nothing to
// conclude, and the two writes downstream take their cannot-tell directions.
func heldTokens(ctx context.Context, opts Options, seat SeatResult) tokenCount {
	if seat.AccountID == "" {
		return tokenCount{}
	}
	if seat.Created {
		return tokenCount{known: true}
	}
	count, err := opts.Client.CountTokens(ctx, opts.Key, seat.AccountID)
	if err != nil {
		log.Debug("atlassian_token_check_failed", "seat", seat.Handle, "error", err.Error(),
			"detail", "the held credential is left alone and the account's product "+
				"access is granted again; a failed read is evidence of neither")
		return tokenCount{}
	}
	return tokenCount{n: count, known: true}
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

// orphaned reports a held credential that cannot belong to this seat's
// account, so the pass mints over it.
//
// # Why a held credential is not necessarily a working one
//
// Disconnecting with "remove accounts" deletes them at Atlassian and leaves
// the minted tokens in the sealed store: the store is the company's, and a
// teardown that emptied it would take values an operator may have put there
// by hand. A later reconnect then creates a NEW account and finds a
// credential already held for the seat, so it mints nothing, and every call
// the seat makes is refused with a 401 naming nothing. The dashboard reports
// "has no Jira account" while the account plainly exists, and the only cure
// was deleting the secret by hand.
//
// # It asks the vendor, not the credential
//
// Atlassian shows a token's value once, so nothing can check that the stored
// string is one of the account's. What it can check is that the account has
// NO tokens at all, which is true of an account this engine has just created
// and false of every one it has finished with. That is the whole test.
//
// # "Cannot tell" leaves it alone
//
// A failed list is not evidence of anything, and minting on it would rotate a
// working credential every time Atlassian was briefly unreachable, on a
// timer. The three-valued rule this engine applies to ownership applies here
// for the same reason: only a definite answer acts.
func orphaned(tokens tokenCount, handle string) bool {
	if !tokens.none() {
		return false
	}
	log.Info("atlassian_token_orphaned", "seat", handle,
		"detail", "the account holds no API token, so the credential in the "+
			"store belongs to an account that no longer exists; minting a new one")
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
	if r.NoKeyring {
		out = append(out, integration.Finding{
			Kind:    integration.FindingCredentialMissing,
			Subject: "secrets.keys",
			Detail: "this node has no keyring, so an Atlassian API token minted " +
				"for a seat could not be sealed and no service account was " +
				"created — set secrets.keys in the bootstrap configuration",
		})
	}
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
