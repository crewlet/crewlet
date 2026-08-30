package atlassian

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/provision"
)

var log = logging.Get("atlassian.provision")

// Reconcile brings an Atlassian organization in line with the company config.
//
// # What Atlassian lets a provisioner do, and where the old refusal was right
//
// `internal/jira` says Atlassian issues no credential on a provisioner's
// behalf. That is true of a USER account — a Cloud API token is created by
// the person it belongs to, and a Data Center personal access token can only
// be minted for the calling user — and it is exactly false for a SERVICE
// account. With an organization API key created without scopes, the admin API
// creates the account, mints its token, and licences it into a product. So
// this reconcile is the third true minting reconcile in this build, beside
// GitLab's and Mattermost's, and it is shaped like them on purpose.
//
// What it still cannot do is PLACE an agent — add it to a Jira project role
// or a Confluence space permission. That is refused to an API token outright.
// So the run grants the licence and then reads back, as the agent, what
// access it actually ended up with. Reporting a fact it cannot change is a
// better answer than a write that silently does nothing.
//
// # It holds no state, and every question is asked of the vendor
//
// There is no ledger, no local table, nothing remembered between runs. Which
// account belongs to a seat comes from the organization's own listing joined
// by what the recorded credential says it is; whether a seat holds a licence
// comes from whether its credential can call that product. A remembered claim
// is the one thing that can disagree with the vendor, and the four-verdict
// probe exists precisely because that disagreement is the expensive failure.
//
// # A run that cannot record what it minted revokes it
//
// Between Atlassian minting a token and the sink recording it there is a
// window where the only copy of a live credential is in this process's
// memory. If recording fails, the token exists, nothing can use it, and
// nobody knows to remove it — so the run revokes what it minted and discards
// what it recorded, and reports both.

// seatRetryDelays waits out the gap between Atlassian creating an account and
// being able to licence it.
//
// Atlassian answers a grant against a just-created account with a 404 that
// reads exactly like "no such account", and the account becomes grantable a
// short time later — routinely longer than the rest of one seat's
// provisioning takes. Without the wait, every newly created agent would see
// an empty product until somebody ran the command again.
//
// Bounded at three tries totalling 14 seconds: long enough to cover the
// normal case, short enough that a genuinely stuck account costs one seat's
// worth of delay rather than holding the whole run. What it does not cover is
// reported and repaired by the next run, which is why exhausting it is a note
// rather than a failure.
var seatRetryDelays = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

// Options are one reconcile's inputs.
type Options struct {
	// Admin talks to the organization as the operator's own key.
	Admin *AdminClient

	// OrgID is the Atlassian organization, resolved.
	OrgID string

	// Plan is what to do, from [PlanFor].
	Plan *Plan

	// Sink records what is minted.
	Sink provision.TokenSink

	// SiteOf is the cloud id each product's site is reached by, resolved.
	// A product with no entry cannot be licensed and is reported rather
	// than attempted.
	SiteOf map[Product]string

	// Containers are the projects and spaces the org chart names, per
	// product: where an agent's effective access is read back from.
	//
	// The ORG CHART's, not the seat's, because a seat's own project is its
	// filing home rather than the limit of where it works — an agent that
	// files in ENG still comments on PLATFORM's issues, and reporting only
	// its own project would call that access missing that nobody asked for
	// and miss the access it actually needs.
	Containers map[Product][]string

	// SiteURL is the human-readable site base the settings links in a
	// permission report are built from. Empty prints the container without
	// a link, which is honest — the API gateway is not a place a browser
	// can go.
	SiteURL string

	// DisplayNamePrefix is what a provisioned account is called, before the
	// seat's role name. Never empty: it is the whole of how a re-run
	// recognises an account as this company's.
	DisplayNamePrefix string

	// TokenLifetime is how long a minted credential lasts.
	TokenLifetime time.Duration

	// Rotate forces a fresh credential for every seat, including seats
	// whose current one still works.
	//
	// # Why it is a flag rather than what a run does
	//
	// Atlassian returns a token's value once, so a provisioner cannot check
	// that what it recorded last time still matches. The tempting answer is
	// to mint every run — and that is an outage: the engine is running with
	// the OLD value, and rotating revokes the credential every agent is
	// currently authenticating with. An operator adding a tenth seat would
	// take the other nine down, from a command whose whole promise is that
	// it is safe to re-run.
	Rotate bool

	// Decommission deletes managed service accounts whose seats have left
	// the config. Off by default, and it is the one genuinely destructive
	// direction here: Atlassian has no disable verb, so the account that
	// reported an issue stops existing and its history is rewritten.
	Decommission bool

	// Only narrows the run to these handles, empty doing all of them. It
	// narrows PROVISIONING only — see [Reconcile] on why decommissioning
	// reads the whole plan regardless.
	Only []string

	// Now is the clock token expiry and labels are computed from. Nil takes
	// the wall clock.
	Now func() time.Time
}

// AccessReport is what one agent can actually do in one container.
type AccessReport struct {
	Handle    string
	Product   Product
	Container string
	// Missing is contract access the agent did not get: licensed, but
	// unable to do its job. The OPERATOR's to grant.
	Missing []string
	// Excess is forbidden access the tenant's own permission scheme
	// attached. Crewlet did not grant it and cannot revoke it without
	// editing something it does not own, so it says so rather than
	// accepting it in silence.
	Excess []string
	// Pending means the licence was granted in THIS run and Atlassian has
	// not applied it yet. Reported as still starting rather than as broken:
	// the grant is asynchronous and has taken minutes in practice, so a
	// fault on every freshly provisioned agent would be wrong more often
	// than right. The next run reports it as an error if it is one.
	Pending bool
	// Reason explains a check that could not run.
	Reason string
	// SettingsURL is where a person changes this container's access, and
	// SettingsStyle what kind of screen that is — a team-managed project, a
	// shared permission scheme and a space grid are changed in different
	// ways and the advice differs. Resolved only when there is something to
	// fix.
	SettingsURL   string
	SettingsStyle string
}

// OK reports an agent with exactly the access it should have.
func (r AccessReport) OK() bool {
	return r.Reason == "" && !r.Pending && len(r.Missing) == 0 && len(r.Excess) == 0
}

// Result is what one reconcile did.
type Result struct {
	// Org is the organization this ran against.
	Org string
	// Created names the seats whose service accounts this run created, and
	// Adopted the seats matched to an account the organization already had.
	Created []string
	Adopted []string
	// Rotated names the seats whose credentials this run minted.
	Rotated []string
	// Kept names the seats whose existing credential was left alone — the
	// SUCCESSFUL outcome of a re-run, said out loud because a report that
	// mentioned only what changed reads as a run that did nothing, and the
	// operator's next move is to reach for -rotate.
	Kept []string
	// Licensed names, per seat, the products this run granted a licence
	// for. A steady-state run grants none.
	Licensed map[string][]string
	// Retired counts the superseded credentials this run revoked.
	Retired int
	// Decommissioned names the accounts this run deleted.
	Decommissioned []string
	// Access is what each provisioned agent can actually do, per container.
	Access []AccessReport
	// Recorded counts the values this run wrote to the sink.
	//
	// A COUNT rather than the names, because the names are variables
	// holding live credentials and this number's only job is deciding
	// whether the report tells the operator what still has to happen for
	// those values to reach a running engine.
	Recorded int
	Notes    []string
}

// mintedToken is one credential this run created, as its rollback needs it.
type mintedToken struct {
	atlassianID string
	accountID   string
	tokenID     string
	// label is carried for the one case tokenID is empty: a mint that
	// answered 200 and returned no value. The credential may exist and can
	// only be found by the label the run sent.
	label string
	// mayExist says the mint MIGHT have created a credential this run holds
	// no id for — a 200 with no value, or an answer that never arrived.
	//
	// Without it, a mint Atlassian REFUSED took the same rollback arm: the
	// cleanup hunted a credential that was never created, counted it as
	// revoked, and — when the listing was refused too, which is the
	// correlated case — printed the strongest banner this package has,
	// "these credentials may still be live", naming a token that does not
	// exist.
	mayExist bool
	// createdAccount says the account is this run's, which decides HOW the
	// rollback undoes it: deleting an account nothing else has ever used,
	// versus revoking exactly the one token on an account that already
	// existed and owns issues.
	createdAccount bool
	// licensed are the products this run granted on a PRE-EXISTING account.
	// Atlassian offers no route to withdraw one, so a rollback names them
	// rather than pretending to undo them.
	licensed []Product
}

// Reconcile runs one pass.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if opts.Admin == nil {
		return nil, errors.New("atlassian: no admin client")
	}
	if opts.Sink == nil {
		return nil, provision.ErrNoSink
	}
	if strings.TrimSpace(opts.OrgID) == "" {
		return nil, errors.New("atlassian: no organization id")
	}
	if opts.Plan == nil {
		return nil, errors.New("atlassian: no plan")
	}
	res := &Result{Org: opts.OrgID, Licensed: map[string][]string{}}
	res.Notes = append(res.Notes, opts.Plan.Notes...)
	if opts.Plan.Empty() && !opts.Decommission {
		return res, nil
	}

	// THE ORGANIZATION IS LISTED ONCE, up front, and every seat is joined
	// against that one answer. Listing per seat would be a request per seat
	// to answer a question that cannot change mid-run, and it is also what
	// makes adoption deterministic: two seats whose display names collide
	// must not both adopt the same account.
	upstream, err := opts.Admin.ServiceAccounts(ctx, opts.OrgID)
	if err != nil {
		return nil, fmt.Errorf("atlassian: list the organization's service accounts: %w", err)
	}

	minted := map[string]mintedToken{}
	claimed := map[string]bool{}
	only := handleSet(opts.Only)
	// quota carries the organization's refusal past the loop, so the run
	// finishes what it started, reports it, and fails afterwards.
	var quota error

	for _, seat := range opts.Plan.Seats {
		if len(only) > 0 && !only[seat.Handle] {
			continue
		}
		account, created, err := ensureAccount(ctx, opts, upstream, claimed, seat)
		if errors.Is(err, ErrQuotaExceeded) {
			// A WALL, NOT A FAULT — and the difference is everything the
			// rollback would otherwise destroy. The organization has no
			// room for another account; the seats already provisioned are
			// finished, their credentials are recorded, and nothing about
			// them is wrong. Rolling those back would delete N billable
			// identities to report a limit, and every re-run would churn
			// the same create-then-delete. So enrolment stops here, the
			// sink is still flushed, and the report prints what did land
			// beside the error.
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: %v. The seats above are provisioned and recorded; this one "+
					"and any after it are not. Free a service account or a licence, "+
					"raise the allowance, or narrow the run with -handles",
				seat.Handle, err))
			quota = err
			break
		}
		if created && account != nil && account.ID != "" {
			// LEDGERED BEFORE THE ERROR IS CHECKED, for the same reason the
			// mint below is: a create that failed after Atlassian had
			// already made the account leaves a billable identity nothing
			// else will ever clean up, and the rollback is the only thing
			// that still knows it exists.
			minted[seat.Handle] = mintedToken{
				atlassianID:    account.AtlassianID,
				accountID:      account.ID,
				createdAccount: true,
			}
		}
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("atlassian: %s: %w", seat.Handle, err))
		}
		claimed[account.ID] = true
		switch {
		case created:
			res.Created = append(res.Created, seat.Handle)
			upstream = append(upstream, *account)
		default:
			res.Adopted = append(res.Adopted, seat.Handle)
		}

		// THE PROBE COMES FIRST, and it answers four questions: whether
		// this seat already has a working credential, which of its
		// products need a licence, whether its credential still reaches
		// only the products it is enabled for, and whether its variables
		// authenticate as somebody else.
		//
		// IT RUNS FOR A CREATED ACCOUNT TOO, and on a genuinely new seat
		// that costs nothing — recordedCredential answers not-held and
		// the probe returns without one product call. What it buys is the
		// case creation cannot see. A seat whose ROLE NAME was edited no
		// longer matches its account by display name, so this run makes a
		// SECOND billable identity and mints into variables that still
		// authenticate as the first — which keeps every issue, watcher
		// and history entry it had, plus a live credential nothing will
		// ever revoke, until a later -decommission deletes it for not
		// being in the keep-set. Atlassian has no restore. VerdictOther
		// is exactly that fact, and it stops the run before the second
		// identity is minted into.
		verdict, owed, note, err := probe(ctx, opts, account, seat)
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("atlassian: %s: %w", seat.Handle, err))
		}
		switch {
		case created && verdict == provision.VerdictOther:
			// Left alone: the refusal below is the whole point.
		case created:
			// Nothing recorded can authenticate as an account minted
			// seconds ago, so Self is unreachable here and Unknown is not
			// a credential worth keeping — the account owes every product
			// by construction. The note still travels: an operator whose
			// check could not run should know the rename guard did not
			// get to answer.
			verdict, owed = provision.VerdictRejected, seat.Products
			if note != "" {
				res.Notes = append(res.Notes, note)
				note = ""
			}
		case opts.Rotate && verdict != provision.VerdictOther:
			// -ROTATE MINTS REGARDLESS — but the probe still RAN, and
			// that is the difference between "mint for every seat" and
			// "skip every check". Two of its answers survive the flag:
			// which licences are owed, so a rotation does not re-buy
			// every one and report every unreachable container as still
			// starting; and VerdictOther, because minting over a variable
			// that authenticates as a different account hands one account
			// two seats' identities however the run was invoked.
			verdict, note = provision.VerdictRejected, ""
		}

		// A LICENCE IS GRANTED ONLY WHERE ONE IS OWED. It is billable and
		// the grant is asynchronous, so re-sending it every run would both
		// spend a write per product per seat to change nothing AND make
		// every container the agent cannot see look like a licence still
		// propagating — which is how the one permanent failure the access
		// report exists to surface becomes unprintable.
		granted, notes := ensureLicences(ctx, opts, account, seat, owed, created)
		res.Notes = append(res.Notes, notes...)
		if len(granted) > 0 {
			res.Licensed[seat.Handle] = productNames(granted)
		}
		switch verdict {
		case provision.VerdictSelf:
			res.Kept = append(res.Kept, seat.Handle)
		case provision.VerdictOther:
			// A COPY-PASTED VARIABLE, or a variable this company shares
			// with another one. Minting over it hands this seat a second
			// identity while whoever else holds the value keeps
			// authenticating as one account from two places, and nothing
			// anywhere reports it.
			return nil, rollback(ctx, opts, minted, fmt.Errorf(
				"atlassian: %s: %s holds a credential that authenticates as a "+
					"different account — give this seat its own variables",
				seat.Handle, strings.Join(seat.TokenVars, ", ")))
		case provision.VerdictUnknown:
			// LEFT EXACTLY AS IT WAS. Re-minting on "cannot tell" destroys
			// a credential that works; the recovery for one that does not
			// is a -rotate away.
			res.Kept = append(res.Kept, seat.Handle)
			res.Notes = append(res.Notes, note)
		default:
			if note != "" {
				res.Notes = append(res.Notes, note)
			}
			written, entry, mintErr := mintAndRetire(ctx, opts, account, seat, created)
			if !created {
				// Only a PRE-EXISTING account's licences are carried into
				// the rollback: an account this run created is deleted
				// whole, which takes its licences with it, and naming them
				// as un-withdrawable would send an operator to a console
				// page for an account that no longer exists.
				entry.licensed = granted
			}
			// THE LEDGER IS UPDATED BEFORE THE ERROR IS CHECKED. Whatever
			// the mint managed to do — an account created, a credential
			// minted and not recorded — is what the rollback has to undo,
			// and a ledger written only on success would leave exactly
			// those live.
			minted[seat.Handle] = entry
			res.Recorded += written.recorded
			res.Retired += written.retired
			res.Notes = append(res.Notes, written.notes...)
			if mintErr != nil {
				return nil, rollback(ctx, opts, minted,
					fmt.Errorf("atlassian: %s: %w", seat.Handle, mintErr))
			}
			res.Rotated = append(res.Rotated, seat.Handle)
		}

		res.Access = append(res.Access,
			verifyAccess(ctx, opts, account, seat, granted)...)
	}

	if err := opts.Sink.Flush(ctx); err != nil {
		return nil, rollback(ctx, opts, minted, fmt.Errorf("atlassian: %w", err))
	}

	if opts.Decommission {
		// AFTER THE FLUSH, deliberately. Deleting an account is the one
		// step no rollback can undo — Atlassian has no restore — so it
		// happens only once every credential this run minted is durable.
		removed, notes := decommission(ctx, opts, upstream)
		res.Decommissioned = removed
		res.Notes = append(res.Notes, notes...)
	}
	// THE RESULT COMES BACK WITH THE ERROR. Everything above it happened
	// and is durable; a caller handed only the error would print no report
	// at all, and the operator would not know which seats are done.
	if quota != nil {
		return res, fmt.Errorf("atlassian: %w", quota)
	}
	return res, nil
}

// ensureAccount finds, adopts or creates a seat's service account.
//
// # Adoption matches on the DISPLAY NAME, because it is the only field both
// sides control
//
// Atlassian assigns the account's id and its address, so neither can be
// derived from the org chart the way a GitLab username is. The display name
// is what an operator reads in the assignee picker and what this tool wrote
// when it created the account, so it is the join — matched case- and
// space-insensitively, which is what somebody comparing them by eye would do.
//
// An account another seat in this run already took is skipped rather than
// shared: two seats whose role names normalise alike would otherwise both
// adopt one identity, and each would then mint over the other's credential.
func ensureAccount(ctx context.Context, opts Options, upstream []ServiceAccount, claimed map[string]bool, seat SeatPlan) (*ServiceAccount, bool, error) {
	want := NormalizeName(DisplayName(opts.DisplayNamePrefix, seat))
	var matches []*ServiceAccount
	for i := range upstream {
		account := &upstream[i]
		if claimed[account.ID] || NormalizeName(account.DisplayName) != want {
			continue
		}
		matches = append(matches, account)
	}
	if len(matches) > 1 {
		// TWO ACCOUNTS UPSTREAM WEAR ONE NAME, so there is no answer to
		// which of them is this seat. Taking the first is taking whichever
		// Atlassian happened to list first, which is not stable across
		// runs: the seat's identity would flip between two account ids,
		// each holding issues the other does not, and -decommission can
		// sweep neither because the name is in the keep-set. The plan
		// already refuses the version of this the CHART can cause; this is
		// the half only the organization can.
		return nil, false, fmt.Errorf(
			"the organization has %d service accounts named %q (%s), so there "+
				"is no telling which one is this seat — delete or rename the "+
				"duplicates in admin.atlassian.com",
			len(matches), matches[0].DisplayName, strings.Join(accountIDs(matches), ", "))
	}
	if len(matches) == 1 {
		account := matches[0]
		if account.AtlassianID == "" {
			return nil, false, fmt.Errorf(
				"the organization has a service account named %q with no "+
					"atlassianId, so it cannot be given a credential — rename "+
					"or remove it in admin.atlassian.com", account.DisplayName)
		}
		return account, false, nil
	}
	account, err := opts.Admin.CreateServiceAccount(ctx, opts.OrgID,
		DisplayName(opts.DisplayNamePrefix, seat), Description(seat))
	if err != nil {
		// THE ACCOUNT COMES BACK WITH THE ERROR when there is one to hand
		// back — Atlassian made it and then answered with something
		// unusable, most often no atlassianId. Returning only the error
		// would leave a billable identity upstream that this run created,
		// nothing recorded, and no rollback able to reach it; worse, every
		// later run would adopt it by display name and fail the same seat
		// the same way for ever. The boolean still says it is this run's,
		// which is what decides how the caller undoes it.
		return account, account != nil, fmt.Errorf("create the service account: %w", err)
	}
	return account, true, nil
}

// ensureLicences gives an agent product access, once per product it needs.
//
// A service account is created with NONE, so without this an agent
// authenticates perfectly and then finds an empty Jira. Licences are per
// product because they are billable, and because a Confluence-only agent must
// not be able to act in Jira.
//
// # It is sent only where the probe found one owed
//
// A grant is idempotent, so sending one per product per run looks free. It is
// not: it is a write per product per seat forever, and it makes a licence
// look freshly granted on every run — which turns a genuinely inaccessible
// container into "still starting" for ever. A licence revoked by hand is
// still repaired, because the agent's own credential is then refused on that
// product and the probe reports it owed. The evidence is the check, not a
// memory.
func ensureLicences(ctx context.Context, opts Options, account *ServiceAccount, seat SeatPlan, owed []Product, created bool) ([]Product, []string) {
	var granted []Product
	var notes []string
	for _, product := range owed {
		cloudID := opts.SiteOf[product]
		if cloudID == "" {
			notes = append(notes, fmt.Sprintf(
				"%s: no cloud_id for %s, so no licence was granted — set "+
					"integrations.%s.cloud_id, or drop %s from this seat's "+
					"integrations.atlassian.products",
				seat.Handle, product.Label(), product, product))
			continue
		}
		err := grantWhenReady(ctx, opts, account.AtlassianID, cloudID, product, created)
		switch {
		case err == nil:
			granted = append(granted, product)
		case errors.Is(err, ErrAccountNotReady):
			// Outlived the wait. Not a fault: the account exists and is
			// correct, and the next run grants the licence.
			notes = append(notes, fmt.Sprintf(
				"%s: Atlassian does not see this account in the directory yet, "+
					"so its %s licence was not granted — run the command again "+
					"in a few minutes", seat.Handle, product.Label()))
		default:
			// Not fatal: the account and its credential still work, and an
			// operator can grant the licence in the console. Failing the
			// run here would roll back a credential over a billing step.
			notes = append(notes, fmt.Sprintf(
				"%s: could not grant the %s licence, so this agent will "+
					"authenticate and see nothing: %v",
				seat.Handle, product.Label(), err))
			log.WarnContext(ctx, "atlassian_licence_failed", "seat", seat.Handle,
				"product", string(product), "error", err.Error())
		}
	}
	return granted, notes
}

// grantWhenReady waits out a just-created account's invisibility.
//
// The wait happens ONLY for an account this run created. An account that
// already existed is already in the directory, so a not-ready answer from one
// is a real fault and sleeping fourteen seconds over it would just make the
// report slower to arrive.
func grantWhenReady(ctx context.Context, opts Options, atlassianID, cloudID string, product Product, created bool) error {
	err := opts.Admin.GrantLicence(ctx, opts.OrgID, cloudID, atlassianID, product)
	if !created {
		return err
	}
	for _, delay := range seatRetryDelays {
		if !errors.Is(err, ErrAccountNotReady) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		err = opts.Admin.GrantLicence(ctx, opts.OrgID, cloudID, atlassianID, product)
	}
	return err
}

// probe decides whether this seat already has a working credential.
//
// # It PROVES it, rather than inferring it
//
// The weaker test — "the variable has a value and the account has some token"
// — reads as provisioned in exactly the case that matters: an operator who
// restored an older env file has a stale value sitting beside a live token
// that is not it, and the seat then authenticates with nothing, on every run,
// for ever. So the run takes the value the variables actually hold and asks
// Atlassian who it is.
//
// # It asks once per PRODUCT, and that is what detects a stale scope set
//
// A token's scopes cannot be read back from Atlassian at all — the listing
// returns an id and a label and nothing else. So a seat that has gained
// Confluence since its credential was minted holds a Jira-only token that
// looks perfectly healthy, and only its first real Confluence call fails.
// Exercising the credential against every product the seat is enabled for
// turns that into a refusal here, which re-mints with the current scopes.
// Narrowing matters the same way, or a seat that dropped a product keeps a
// credential that can still act in it.
//
// # It also returns WHICH products refused it, and that is what makes a
// steady-state run free
//
// A licence grant is idempotent, so the tempting shape is to send one per
// product per run and call it a repair. It costs a write per product per seat
// forever, and — worse — it makes every container the agent cannot see look
// like a licence that has not propagated yet, on every run, so the one
// permanent failure the access report exists to surface can never print. A
// product the credential reached needs no grant; a product it was refused on
// is the evidence that one is owed.
func probe(ctx context.Context, opts Options, account *ServiceAccount, seat SeatPlan) (provision.Verdict, []Product, string, error) {
	cred, held, err := recordedCredential(ctx, opts, seat)
	if err != nil {
		return provision.VerdictUnknown, nil, "", err
	}
	if !held {
		// Nothing to test with, so nothing is known about any product's
		// licence either: every one is treated as unproven, which is what
		// makes the grant below a repair rather than a guess.
		return provision.VerdictRejected, seat.Products, "", nil
	}
	var refused []Product
	for _, product := range seat.Products {
		cloudID := opts.SiteOf[product]
		if cloudID == "" {
			// Nothing to check against, and nothing will be licensed
			// either. Skipping is right: refusing here would re-mint a
			// working credential because a cloud id is missing from the
			// config.
			continue
		}
		client, err := NewProductClient(opts.Admin.Gateway(), product, cloudID, cred, opts.Admin.HTTP())
		if err != nil {
			return provision.VerdictUnknown, nil, "", err
		}
		who, err := client.Me(ctx)
		switch {
		case err == nil && who == account.AtlassianID:
			continue
		case err == nil:
			return provision.VerdictOther, nil, "", nil
		case errors.Is(err, ErrCredentialRefused):
			// Refused on ONE product is refused: either the credential is
			// wrong, or its scopes no longer cover what this seat does, or
			// the licence was revoked by hand. All three are repaired by
			// granting the licence again and minting a credential for the
			// products the seat has now — and which of them it was cannot
			// be told apart, because Atlassian answers all three with the
			// same flat 401.
			refused = append(refused, product)
		case StatusOf(err) >= 500, StatusOf(err) == 0:
			// A 5xx or a dropped connection. NOT a rejection: re-minting
			// on "cannot tell" destroys a credential that works.
			return provision.VerdictUnknown, nil, fmt.Sprintf(
				"%s: could not check whether its credential still works "+
					"against %s, so it was left alone — re-run with -rotate if "+
					"this seat is failing to authenticate (%v)",
				seat.Handle, product.Label(), err), nil
		default:
			return provision.VerdictUnknown, nil, fmt.Sprintf(
				"%s: %s answered its identity check unexpectedly, so the "+
					"credential was left alone: %v", seat.Handle, product.Label(), err), nil
		}
	}
	if len(refused) > 0 {
		return provision.VerdictRejected, refused, "", nil
	}
	// NARROWING, which is the half this comment used to promise and the
	// code did not do. The loop above walks the seat's CURRENT products, so
	// a product removed from the seat is by definition not in it: its
	// credential was never exercised there, never refused, and never
	// re-minted, and the agent kept a live write scope on a product its
	// author had taken away — for the whole token lifetime. Atlassian will
	// not tell anyone a token's scopes, so the only way to ask is to use it.
	for _, product := range Products {
		cloudID := opts.SiteOf[product]
		if cloudID == "" || slices.Contains(seat.Products, product) {
			continue
		}
		client, err := NewProductClient(opts.Admin.Gateway(), product, cloudID, cred, opts.Admin.HTTP())
		if err != nil {
			continue
		}
		who, err := client.Me(ctx)
		if err != nil || who != account.AtlassianID {
			// Refused is the HEALTHY answer here, and an unreachable
			// product tells us nothing — this check only ever adds a
			// reason to re-mint, never a reason to keep.
			continue
		}
		// NO LICENCE IS OWED: every product the seat still holds answered
		// above. The repair is the credential alone, minted with the
		// scopes the seat has now. The licence itself stays — Atlassian
		// offers no route to give one back — so the report says so.
		return provision.VerdictRejected, nil, fmt.Sprintf(
			"%s: its credential still reached %s, which this seat's "+
				"integrations.atlassian.products no longer names, so it was "+
				"re-minted with narrower scopes. The %s LICENCE is still held "+
				"and still billable — Atlassian has no route to withdraw one, "+
				"so remove it in admin.atlassian.com if it is not coming back",
			seat.Handle, product.Label(), product.Label()), nil
	}
	return provision.VerdictSelf, nil, "", nil
}

// recordedCredential reads back what the sink holds for this seat.
//
// EVERY variable has to agree. A seat whose four keys point at four variables
// is one credential written four times, and a sink holding two different
// values for it is an operator half way through an edit — whichever half the
// engine reads, the other seat's tools authenticate with something else. That
// is a re-mint, not a "close enough".
func recordedCredential(ctx context.Context, opts Options, seat SeatPlan) (Credential, bool, error) {
	read := func(names []string) (string, bool, error) {
		var agreed string
		for _, name := range names {
			value, held, err := opts.Sink.Value(ctx, name)
			if err != nil {
				// UNREADABLE IS NOT ABSENT. Treating it as absent would
				// rotate every live credential in the company because a
				// store blinked.
				return "", false, fmt.Errorf(
					"cannot read %s, and guessing would either rotate a live "+
						"credential or leave a seat with none: %w", name, err)
			}
			if !held {
				return "", false, nil
			}
			if agreed != "" && agreed != value {
				return "", false, nil
			}
			agreed = value
		}
		return agreed, agreed != "", nil
	}
	token, held, err := read(seat.TokenVars)
	if err != nil || !held {
		return Credential{}, false, err
	}
	email, held, err := read(seat.EmailVars)
	if err != nil || !held {
		return Credential{}, false, err
	}
	return Credential{Token: token, Email: email}, true, nil
}

// written counts what one seat's mint changed, for the report.
type written struct {
	// recorded is the values this seat's mint wrote to the sink.
	recorded int
	// retired is the superseded credentials it revoked.
	retired int
	// notes are the credentials it could not retire. Reported rather than
	// logged: this engine keeps no state, so the next run's probe answers
	// VerdictSelf and never comes back for them — the report is the only
	// place a leftover credential is ever named.
	notes []string
}

// mintAndRetire gives a seat a fresh credential and revokes the ones it
// replaced, in that order.
//
// THE ORDER IS THE WHOLE POINT. Recording comes after the mint because the
// value exists only in that response; retiring comes after the record
// because a failed record on a seat whose old credential was already revoked
// leaves that seat with nothing at all. And a seat whose account this run
// CREATED has nothing to retire — nothing has ever minted there.
func mintAndRetire(ctx context.Context, opts Options, account *ServiceAccount, seat SeatPlan, created bool) (written, mintedToken, error) {
	var done written
	recorded, entry, err := mint(ctx, opts, account, seat, created)
	done.recorded = recorded
	if err != nil {
		return done, entry, err
	}
	if created {
		return done, entry, nil
	}
	// Only this tool's own credentials, recognised by the label it stamps:
	// an administrator may have minted one on this account by hand, and
	// revoking it would break whatever is using it — silently, since
	// nothing here knows what that is.
	//
	// RETIREMENT NEVER FAILS THE SEAT. It is hygiene that runs AFTER the
	// real work: the fresh credential exists and is recorded, so a
	// transient 5xx on the listing or on one DELETE would otherwise roll
	// back a run whose every meaningful step succeeded — revoking the new
	// credential of every seat before it, whose old ones this same
	// function has already killed. What a failed revoke costs is one live
	// credential too many, and that is a note.
	retired, notes := retirePrevious(ctx, opts, account, seat, entry.tokenID)
	done.retired, done.notes = retired, notes
	return done, entry, nil
}

// mint gives a seat a credential of its own, and records it everywhere the
// config points.
//
// The ADDRESS is recorded too, and it is not decoration: Atlassian assigns
// the service account's address at creation and Cloud authenticates Basic
// base64(email:token), so a seat holding the token and not the address
// authenticates as nobody. Crewlet never chooses that address — inventing one
// would be a value the vendor overwrites.
func mint(ctx context.Context, opts Options, account *ServiceAccount, seat SeatPlan, created bool) (int, mintedToken, error) {
	now := clock(opts)
	entry := mintedToken{
		atlassianID:    account.AtlassianID,
		accountID:      account.ID,
		createdAccount: created,
		// THE LABEL IS BUILT HERE, ONCE, and it carries the run's clock.
		//
		// The STAMP is what makes one generation of a seat's credential
		// distinguishable from the next. [retirePrevious] revokes the
		// labels under this seat's prefix and keeps the one it was told to,
		// so an unstamped label — which IS the prefix, not a name under it
		// — is never a candidate: every rotation would leave its
		// predecessor live until expiry, on an account nobody is auditing,
		// and the report would say it retired nothing without that being a
		// clue.
		//
		// And it is built by the CALLER rather than by the mint, because
		// [revokeByLabel] finds a credential whose id was never returned by
		// comparing this exact string: a label the mint stamped for itself
		// would differ from the one the run remembers, and the cleanup
		// would match nothing while reporting that it had revoked
		// everything.
		label: fmt.Sprintf("%s-%d", TokenLabel(seat.Handle), now.Unix()),
	}
	token, err := opts.Admin.MintToken(ctx, account.AtlassianID, entry.label,
		Scopes(seat.Products), opts.TokenLifetime, now)
	if token != nil {
		entry.tokenID = token.ID
	}
	if err != nil {
		// A REFUSAL IS PROOF NOTHING WAS CREATED; silence and a valueless
		// 200 are not. Only the second kind leaves something to clean up.
		var lost *TransportError
		entry.mayExist = errors.As(err, &lost) || errors.Is(err, ErrTokenNotReturned) ||
			errors.Is(err, ErrUnexpected)
		return 0, entry, fmt.Errorf("mint a credential: %w", err)
	}
	// Past this point the credential certainly exists.
	entry.mayExist = true

	// THE ADDRESS IS HALF THE CREDENTIAL, so an account without one is
	// refused rather than recorded. Cloud authenticates Basic
	// base64(email:token); with an empty address [AuthHeader] falls back to
	// a bearer token, which Cloud rejects — so a seat provisioned this way
	// reports as successfully rotated and cannot authenticate at all.
	if strings.TrimSpace(account.Email) == "" {
		return 0, entry, fmt.Errorf(
			"the service account %s (%s) has no email address, so this seat "+
				"would hold a token it cannot present — Atlassian assigns one at "+
				"creation, and an account without it has to be repaired in "+
				"admin.atlassian.com", account.DisplayName, account.AtlassianID)
	}

	// RECORDED IMMEDIATELY, and the address first: a run that died between
	// the two would leave a seat holding a token it cannot present, which
	// looks like a bad credential rather than an incomplete one.
	var recorded int
	for _, name := range seat.EmailVars {
		if err := opts.Sink.Record(ctx, name, account.Email); err != nil {
			return recorded, entry, fmt.Errorf("record %s: %w", name, err)
		}
		recorded++
	}
	for _, name := range seat.TokenVars {
		if err := opts.Sink.Record(ctx, name, token.Token); err != nil {
			return recorded, entry, fmt.Errorf("record %s: %w", name, err)
		}
		recorded++
	}
	return recorded, entry, nil
}

// retirePrevious revokes this tool's earlier credentials on an account.
//
// A credential Crewlet has stopped using does not stop working: it stays
// valid until its expiry, and an account re-provisioned or re-scoped a few
// times collects a drawer of live credentials nobody can account for. Only
// tokens this tool minted for THIS seat are touched, recognised by the label
// prefix it stamps, so an administrator's own is left alone.
func retirePrevious(ctx context.Context, opts Options, account *ServiceAccount, seat SeatPlan, keep string) (int, []string) {
	tokens, err := opts.Admin.Tokens(ctx, account.AtlassianID)
	if err != nil {
		return 0, []string{fmt.Sprintf(
			"%s: its earlier credentials could not be listed, so any this tool "+
				"minted before are still live until they expire — re-run to "+
				"retire them: %v", seat.Handle, err)}
	}
	prefix := TokenLabel(seat.Handle) + "-"
	var retired int
	var notes []string
	for _, token := range tokens {
		if token.ID == keep || !strings.HasPrefix(token.Label, prefix) {
			continue
		}
		// EVERY ONE IS ATTEMPTED. Stopping at the first failure left the
		// rest live AND unnamed, which is the worst of both: the operator
		// is told about one credential and inherits several.
		if err := opts.Admin.RevokeToken(ctx, account.AtlassianID, token.ID); err != nil {
			notes = append(notes, fmt.Sprintf(
				"%s: the superseded credential labelled %q is still live and "+
					"could not be revoked — remove it in admin.atlassian.com or "+
					"re-run: %v", seat.Handle, token.Label, err))
			continue
		}
		retired++
	}
	return retired, notes
}

// verifyAccess reports what an agent can actually do where the org chart says
// it works.
//
// Read AS THE AGENT, because both products report the caller's own access and
// that is the only reading that accounts for every scheme, role and group
// grant at once. Crewlet did not grant this access and cannot: the report is
// the deliverable.
//
// A failure here never fails the run. The credential is minted and recorded;
// what is in doubt is whether the tenant's permissions let the agent use it,
// and that is a finding rather than a fault in the provisioning.
func verifyAccess(ctx context.Context, opts Options, account *ServiceAccount, seat SeatPlan, granted []Product) []AccessReport {
	cred, held, err := recordedCredential(ctx, opts, seat)
	if err != nil || !held {
		return nil
	}
	var out []AccessReport
	for _, product := range seat.Products {
		cloudID := opts.SiteOf[product]
		containers := opts.Containers[product]
		if cloudID == "" || len(containers) == 0 {
			continue
		}
		client, err := NewProductClient(opts.Admin.Gateway(), product, cloudID, cred, opts.Admin.HTTP())
		if err != nil {
			continue
		}
		fresh := slices.Contains(granted, product)
		for _, container := range containers {
			out = append(out, verifyOne(ctx, opts, client, account, seat, product, container, fresh))
		}
	}
	return out
}

// verifyOne reads back one agent's access to one container.
func verifyOne(ctx context.Context, opts Options, client *ProductClient, account *ServiceAccount, seat SeatPlan, product Product, container string, fresh bool) AccessReport {
	report := AccessReport{Handle: seat.Handle, Product: product, Container: container}
	held, err := client.PermissionsIn(ctx, container, account.AtlassianID)
	if err != nil {
		denied := errors.Is(err, ErrCredentialRefused) || errors.Is(err, ErrContainerNotVisible)
		if denied && fresh {
			// A licence granted moments ago has very likely not landed
			// yet — Atlassian applies it asynchronously and has taken
			// minutes. Saying otherwise would put a fault on every
			// freshly provisioned agent, once, every time.
			report.Pending = true
			return report
		}
		report.Reason = describeAccessFailure(err)
		return report
	}
	report.Missing, report.Excess = Classify(product, held)
	if !report.OK() {
		// Where somebody goes to fix it, looked up ONLY when there is
		// something to fix. A clean container has nobody to send anywhere,
		// and asking anyway would spend a request per container per run to
		// build a link nothing prints.
		settings, err := client.SettingsFor(ctx, opts.SiteURL, container)
		switch {
		case err == nil:
			report.SettingsURL, report.SettingsStyle = settings.URL, settings.Style
		default:
			// The report keeps its verdict and silently loses the "change
			// it at …" link and the team-managed/company-managed label —
			// on the one line the operator has to act on. The realistic
			// cause is a project type this build has no settings route
			// for, which is a finding of its own.
			log.WarnContext(ctx, "atlassian_settings_lookup_failed", "seat", seat.Handle,
				"product", string(product), "container", container,
				"error", err.Error())
		}
	}
	if len(report.Excess) > 0 {
		log.WarnContext(ctx, "atlassian_excess_permissions", "seat", seat.Handle,
			"product", string(product), "container", container,
			"excess", strings.Join(report.Excess, ","))
	}
	return report
}

// describeAccessFailure renders a failed check for an operator. It completes
// a sentence the report has already started with the product and container,
// so it names the reason and nothing else.
func describeAccessFailure(err error) string {
	switch {
	case errors.Is(err, ErrContainerNotVisible):
		return "it does not exist, or this agent cannot see it"
	case errors.Is(err, ErrCredentialRefused):
		return "the agent's credential was refused"
	default:
		return err.Error()
	}
}

// decommission deletes the managed accounts whose seats have left.
//
// # The prefix is the whole safety property
//
// "Managed" means an account whose display name starts with this company's
// display-name prefix. It can never be empty — [config.Atlassian.Prefix]
// defaults it — and that default is what stops an unscoped sweep over an
// organization that also holds people.
//
// # The keep-set is every agent seat the CHART has, not the seats this run
// provisioned
//
// Those are different sets, and the difference is irreversible. A seat that
// opted out of every product, one whose credential is managed by hand, one
// whose mcp_env names no address variable — none of them is in the plan, and
// every one of them may hold an account an earlier run created. Sweeping on
// the plan would delete an identity that owns issues because somebody edited
// a products list, and Atlassian has no restore.
//
// -handles is not read here either, for the same shape of reason: it says
// which seats to PROVISION, and reading it as a keep-set would make
// `-handles a -decommission` delete every other seat's account.
//
// Failures are notes rather than errors: the credentials are already durable
// by the time this runs, and aborting would leave an operator believing the
// whole run failed.
func decommission(ctx context.Context, opts Options, upstream []ServiceAccount) ([]string, []string) {
	prefix := NormalizeName(opts.DisplayNamePrefix)
	if prefix == "" {
		return nil, []string{
			"-decommission was skipped: with no display-name prefix there is " +
				"nothing that marks an account as this company's, and the sweep " +
				"would propose deleting every service account in the organization"}
	}
	keep := make(map[string]bool, len(opts.Plan.Managed))
	for _, name := range opts.Plan.Managed {
		keep[name] = true
	}
	var removed, notes []string
	for _, account := range upstream {
		name := NormalizeName(account.DisplayName)
		if !strings.HasPrefix(name, prefix+" ") || keep[name] {
			continue
		}
		if err := opts.Admin.DeleteServiceAccount(ctx, opts.OrgID, account.ID); err != nil {
			if StatusOf(err) == http.StatusNotFound {
				// Already gone upstream is the outcome we wanted.
				removed = append(removed, account.DisplayName)
				continue
			}
			notes = append(notes, fmt.Sprintf(
				"%q matches this company's prefix and its seat has left, but "+
					"Atlassian refused to delete it: %v", account.DisplayName, err))
			continue
		}
		removed = append(removed, account.DisplayName)
	}
	return removed, notes
}

// rollback undoes what this run minted, and says what it could not.
//
// # It detaches from the caller's context
//
// The failure being undone is often the cancellation itself, so a cleanup
// that inherited a dead context would do nothing at all — and every
// credential this run minted would stay live, recorded nowhere.
func rollback(ctx context.Context, opts Options, minted map[string]mintedToken, cause error) error {
	ctx = context.WithoutCancel(ctx)
	// TWO LISTS, because they ask the operator for two different things. A
	// credential the cleanup could not revoke is live and dangerous and has
	// to be hunted down; a licence it granted is billable and simply cannot
	// be given back through any API. Printed together under one "these may
	// still be live" banner, the licences read as loose credentials and send
	// somebody looking for a token that does not exist.
	var problems, licences []string
	for handle, entry := range minted {
		switch {
		case entry.createdAccount:
			// The account is this run's: deleting it takes its
			// credentials with it and frees the billable licence, and it
			// owns nothing because nothing has used it yet.
			if err := opts.Admin.DeleteServiceAccount(ctx, opts.OrgID, entry.accountID); err != nil {
				problems = append(problems, fmt.Sprintf(
					"%s: the service account this run created (%s) could not be "+
						"deleted and still holds a live credential: %v",
					handle, entry.accountID, err))
			}
		case entry.tokenID != "":
			// An adopted account is NEVER deleted: it owns issues, is a
			// watcher, and appears in history. Exactly the one credential
			// this run minted is revoked.
			if err := opts.Admin.RevokeToken(ctx, entry.atlassianID, entry.tokenID); err != nil {
				problems = append(problems, fmt.Sprintf(
					"%s: the credential this run minted could not be revoked: %v",
					handle, err))
			}
		case entry.mayExist:
			// A mint that answered without a value, or never answered. The
			// credential may exist and has no id here, so it is found by
			// the label the run sent.
			problems = append(problems, revokeByLabel(ctx, opts, handle, entry)...)
		}
		for _, product := range entry.licensed {
			licences = append(licences, fmt.Sprintf(
				"%s: the %s licence this run granted. Remove it in "+
					"admin.atlassian.com if the seat is not coming back",
				handle, product.Label()))
		}
	}
	if err := opts.Sink.Discard(ctx); err != nil {
		problems = append(problems, err.Error())
	}
	// SORTED, because both lists are built by ranging a map: an operator
	// comparing two runs of the same failure would otherwise get the same
	// lines in a different order every time.
	slices.Sort(problems)
	slices.Sort(licences)

	err := cause
	if len(problems) > 0 {
		err = fmt.Errorf("%w\n\nAND THE CLEANUP DID NOT FINISH — these credentials "+
			"may still be live and must be revoked by hand:\n  - %s",
			err, strings.Join(problems, "\n  - "))
	} else {
		err = fmt.Errorf("%w (%s)", err, describeRollback(minted))
	}
	if len(licences) > 0 {
		err = fmt.Errorf("%w\n\nAND THESE LICENCES COULD NOT BE WITHDRAWN — Atlassian "+
			"offers no route to give one back, and they stay billable:\n  - %s",
			err, strings.Join(licences, "\n  - "))
	}
	return err
}

// describeRollback says what the cleanup actually undid.
//
// It counts the two outcomes apart because they are not the same undo and the
// difference is what an operator checks: an account this run created is
// DELETED, which takes its credential and its licence with it, while an
// adopted account keeps everything it had and gives back exactly the one token
// this run minted on it. A single "N credentials were revoked" was also wrong
// on the run that fails before it has minted anything, where it read "the 0
// credential(s) this run minted were revoked".
func describeRollback(minted map[string]mintedToken) string {
	var accounts, credentials int
	for _, entry := range minted {
		switch {
		case entry.createdAccount:
			accounts++
		case entry.mayExist:
			credentials++
		}
		// An entry that is neither is a seat whose mint Atlassian refused
		// on an account it did not create: nothing was made, so counting
		// it would report a credential that never existed as revoked.
	}
	var parts []string
	if accounts > 0 {
		parts = append(parts, fmt.Sprintf(
			"the %d service account(s) this run created were deleted", accounts))
	}
	if credentials > 0 {
		parts = append(parts, fmt.Sprintf(
			"the %d credential(s) this run minted were revoked", credentials))
	}
	if len(parts) == 0 {
		return "nothing had been created yet"
	}
	return strings.Join(parts, "; ")
}

// revokeByLabel finds and revokes a credential whose id was never returned.
func revokeByLabel(ctx context.Context, opts Options, handle string, entry mintedToken) []string {
	tokens, err := opts.Admin.Tokens(ctx, entry.atlassianID)
	if err != nil {
		return []string{fmt.Sprintf(
			"%s: Atlassian accepted a credential and returned no value, and "+
				"the account's credentials could not be listed to find it — "+
				"revoke the one labelled %q by hand: %v", handle, entry.label, err)}
	}
	for _, token := range tokens {
		if token.Label != entry.label {
			continue
		}
		if err := opts.Admin.RevokeToken(ctx, entry.atlassianID, token.ID); err != nil {
			return []string{fmt.Sprintf(
				"%s: the credential labelled %q could not be revoked: %v",
				handle, entry.label, err)}
		}
		return nil
	}
	return nil
}

// accountIDs names a set of accounts for an error an operator has to act on.
func accountIDs(accounts []*ServiceAccount) []string {
	out := make([]string, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, account.ID)
	}
	return out
}

// handleSet turns a -handles narrowing into a lookup, empty meaning all.
func handleSet(only []string) map[string]bool {
	if len(only) == 0 {
		return nil
	}
	set := make(map[string]bool, len(only))
	for _, handle := range only {
		if handle = strings.TrimSpace(handle); handle != "" {
			set[handle] = true
		}
	}
	return set
}

func productNames(products []Product) []string {
	out := make([]string, 0, len(products))
	for _, p := range products {
		out = append(out, p.Label())
	}
	return out
}

// clock is the run's own time, so a test can pin the credential label and
// the expiry it sends.
func clock(opts Options) time.Time {
	if opts.Now != nil {
		return opts.Now().UTC()
	}
	return time.Now().UTC()
}
