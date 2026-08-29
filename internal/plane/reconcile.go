package plane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// Reconcile brings a Plane workspace in line with the company config.
//
// # Nothing is created until everything is known
//
// The order here is deliberate and it is the whole design: PROBE the
// instance, ENUMERATE its members, RESOLVE every project identifier — and
// only then create the first account. Each of those can refuse the run, and
// each of them is a read. A run that discovered a missing capability or a
// typo'd project halfway would leave some accounts made, some tokens live
// and an operator working out which.
//
// # A run that cannot record what it minted undoes it
//
// Between the instance minting a token and the sink recording it, the only
// copy of a live credential is in this process's memory. If recording fails
// the credential exists, nothing can use it, and nobody knows to remove it —
// so the run revokes what it minted, deletes what it created, and says so.

// Result is what one reconcile did, for the report.
type Result struct {
	// Created names the seats whose accounts this run created.
	Created []string
	// Rotated names the seats whose tokens this run minted.
	Rotated []string
	// Joined names the projects seats were added to.
	Joined []string
	// Kept names the seats whose existing credential was left alone —
	// which is the SUCCESSFUL outcome of a re-run, and has to be visible
	// or an operator reads a quiet report as a run that did nothing.
	Kept []string
	// Decommissioned names the accounts this run deleted.
	Decommissioned []string
	// Hooked is the webhook target this run registered, or empty.
	Hooked string
	// Members is the workspace's member table as this run found it.
	//
	// REPORTED because it is the answer to the one thing provisioning
	// cannot do for a founder: a human seat is reached by the user id in
	// `contact.plane_user_id`, that id is a UUID nobody can guess, and
	// Plane's own UI does not show it. Without this the operator has to
	// go and read it out of a URL.
	Members []Account
	// Notes carries the plan's notes plus anything the run itself found.
	Notes []string
}

// Options are one reconcile's inputs.
type Options struct {
	// Client talks to the workspace as an administrator.
	Client *Client

	// Config is the company's plane block, UNRESOLVED: the webhook secret
	// is minted INTO its `${VAR}`, so the reference has to survive.
	Config *config.Plane

	// Plan is what to do, from [PlanFor].
	Plan *provision.Plan

	// Sink records what is minted.
	Sink provision.TokenSink

	// Org is the company's org, for the human-seat check. Optional: the
	// run provisions agent seats either way and the check is a report.
	Org *org.Organization

	// WebhookBase is this deployment's public base URL, or empty to skip
	// webhook registration.
	//
	// SKIPPED RATHER THAN GUESSED, for the reason the GitLab half states:
	// a hook pointing at the wrong host is worse than no hook, because the
	// instance then reports a healthy integration.
	WebhookBase string

	// Rotate forces a fresh credential for every seat, including seats
	// whose current one is fine.
	//
	// # Why it is a flag rather than what a run does
	//
	// A vendor serves a credential once, so a provisioner cannot check
	// that what it recorded last time still matches. The tempting answer
	// is to mint every run — and that is an outage: the engine is running
	// with the OLD value, and rotating revokes the credential every agent
	// is currently authenticating with. An operator adding a tenth seat
	// would take the other nine down, from a command whose whole promise
	// is that it is safe to re-run.
	//
	// So a plain run mints only where there is nothing usable, and
	// rotating a live credential is a thing the operator ASKS for, having
	// planned the restart that follows it.
	Rotate bool

	// Decommission deletes managed accounts whose seats have left the
	// config. Off by default: it is the one destructive direction, and a
	// company mid-edit looks exactly like a company that removed a seat.
	Decommission bool

	// CreateProjects creates configured projects the workspace does not
	// have, instead of refusing the run.
	CreateProjects bool

	// RecreateWebhook deletes and remakes the workspace webhook to mint a
	// fresh secret, for the case where the existing one's secret was
	// never recorded. Destructive: it invalidates the secret every other
	// deployment of this company holds.
	RecreateWebhook bool

	// ExpiryDays overrides integrations.plane.provisioning
	// .token_expiry_days for this run.
	//
	// A POINTER, because zero is a meaningful value here — it means the
	// token never expires — and an int field's zero value would silently
	// override every config to never on any caller that did not set it.
	ExpiryDays *int

	// Now is the clock the token expiry is computed from. Nil takes the
	// wall clock.
	Now func() time.Time
}

// minted is one credential this run created, as its rollback needs it.
type minted struct {
	handle    string
	accountID string
	tokenID   string
	// createdAccount says the account itself is this run's, so undoing it
	// means deleting the account — which cascades the token away — rather
	// than revoking a token on somebody's existing seat.
	createdAccount bool
	// webhookID is set on the one entry that is not a credential at all:
	// a webhook this run created, whose generated secret was recorded and
	// would be discarded with everything else.
	webhookID string
}

// Reconcile runs one pass.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if opts.Client == nil {
		return nil, errors.New("plane: no client")
	}
	if opts.Sink == nil {
		return nil, provision.ErrNoSink
	}
	if opts.Config == nil {
		return nil, errors.New("plane: no plane config")
	}
	if opts.Plan == nil || opts.Plan.Empty() {
		return &Result{Notes: notesOf(opts.Plan)}, nil
	}

	caps, err := opts.Client.Probe(ctx)
	if err != nil {
		return nil, err
	}
	if fatal := caps.Fatal(); len(fatal) > 0 {
		return nil, fmt.Errorf("plane: this instance cannot be provisioned:\n  %s",
			strings.Join(fatal, "\n  "))
	}

	res := &Result{}
	res.Notes = append(res.Notes, caps.Degraded()...)

	members, err := opts.Client.Members(ctx)
	if err != nil {
		return nil, fmt.Errorf("plane: list workspace members: %w", err)
	}
	accounts, err := index(members)
	if err != nil {
		return nil, err
	}
	res.Members = members
	res.Notes = append(res.Notes, humanSeatNotes(opts.Org, members)...)
	projects, err := resolveProjects(ctx, opts)
	if err != nil {
		return nil, err
	}

	var made []minted
	for _, seat := range opts.Plan.Seats {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		account, created, drift, err := ensureAccount(ctx, opts, accounts, seat)
		if err != nil {
			return nil, rollback(ctx, opts, made, fmt.Errorf("plane: %s: %w", seat.Handle, err))
		}
		res.Notes = append(res.Notes, drift...)
		if created {
			// TRACKED THE INSTANT IT EXISTS, before the membership calls
			// and before the record: the create response already handed
			// out a live credential, and an account missing from this
			// list is one the rollback walks straight past.
			made = append(made, minted{handle: seat.Handle,
				accountID: account.ID, createdAccount: true})
		}
		role := AccountRole(opts.Config.Provisioning, seat.Handle)
		for _, project := range projects {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := opts.Client.AddProjectMember(ctx, project.ID, account.ID, role); err != nil {
				return nil, rollback(ctx, opts, made,
					fmt.Errorf("plane: %s: membership of %s: %w",
						seat.Handle, project.Identifier, err))
			}
			// A DUPLICATE IS NOT ALWAYS A MEMBERSHIP. "Remove from project"
			// in the Plane UI keeps the row and flips is_active, so the add
			// above answers "already a member" for a seat that cannot see
			// the project at all — and the run reports it joined. This is
			// the one case where the vendor's success is not the answer.
			if note := inactiveNote(ctx, opts, project, account, seat); note != "" {
				res.Notes = append(res.Notes, note)
			}
		}

		if created {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := seedCredential(ctx, opts, caps, account, seat); err != nil {
				return nil, rollback(ctx, opts, made,
					fmt.Errorf("plane: %s: %w", seat.Handle, err))
			}
			res.Created = append(res.Created, seat.Handle)
			res.Rotated = append(res.Rotated, seat.Handle)
			continue
		}

		if !caps.TokenLifecycle {
			// THE DEGRADED CASE COMES FIRST, before anything asks the
			// token routes a question: they are not there, and reading
			// their 404 as "this seat has no credential" would report a
			// working company as broken.
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: the account exists and this instance cannot mint a "+
					"second token for it, so %s still needs whatever "+
					"credential it already had — delete the account to have "+
					"the next run create it afresh",
				seat.Handle, seat.TokenVar))
			continue
		}
		verdict, held := provision.VerdictRejected, true
		if !opts.Rotate {
			verdict, held, err = credentialFor(ctx, opts, account, seat)
			if err != nil {
				return nil, rollback(ctx, opts, made,
					fmt.Errorf("plane: %s: %w", seat.Handle, err))
			}
		}
		switch verdict {
		case provision.VerdictSelf:
			res.Kept = append(res.Kept, seat.Handle)
			if note := expiryNote(ctx, opts, account, seat); note != "" {
				res.Notes = append(res.Notes, note)
			}
			continue
		case provision.VerdictOther:
			// A COPY-PASTED VARIABLE. Minting over it hands this seat a
			// second identity while the other keeps authenticating as
			// one account from two places, and nothing anywhere reports
			// it.
			return nil, rollback(ctx, opts, made, fmt.Errorf(
				"plane: %s: %s holds a credential that authenticates as a "+
					"different account — give this seat its own variable",
				seat.Handle, seat.TokenVar))
		case provision.VerdictUnknown:
			// LEFT EXACTLY AS IT WAS. Re-minting on "cannot tell"
			// destroys a credential that works; the recovery for one
			// that does not is a -rotate away.
			res.Kept = append(res.Kept, seat.Handle)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: could not check whether the credential in %s still "+
					"works, so it was left alone — re-run with -rotate if "+
					"this seat is failing to authenticate",
				seat.Handle, seat.TokenVar))
			continue
		}

		token, retired, err := rotate(ctx, opts, account, seat)
		if token != "" {
			// rotate RETURNS WHAT IT MINTED even when it then failed,
			// which is the only reason it returns an id at all —
			// dropping it on the error path would leave a live
			// credential nothing recorded and nothing revokes.
			made = append(made, minted{handle: seat.Handle,
				accountID: account.ID, tokenID: token})
		}
		if err != nil {
			return nil, rollback(ctx, opts, made,
				fmt.Errorf("plane: %s: %w", seat.Handle, err))
		}
		res.Rotated = append(res.Rotated, seat.Handle)
		if !held && retired > 0 {
			// THE SURPRISING CASE, and the only one that earns a note: the
			// operator asked for nothing, but the variable was empty on
			// this machine while a live token existed on the account — so
			// a rotation happened anyway, and whatever is running with the
			// old value is now failing to authenticate.
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: the account held a working token but %s did not, so a "+
					"fresh one was minted and the old one retired — a running "+
					"engine holding the old value has to be restarted",
				seat.Handle, seat.TokenVar))
		}
	}
	for _, project := range projects {
		res.Joined = append(res.Joined, project.Identifier)
	}

	if opts.Decommission {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		removed, notes, err := decommission(ctx, opts, accounts)
		if err != nil {
			return nil, rollback(ctx, opts, made, err)
		}
		res.Decommissioned = removed
		res.Notes = append(res.Notes, notes...)
	}

	hooked, notes, err := ensureWebhook(ctx, opts, &made)
	if err != nil {
		return nil, rollback(ctx, opts, made, err)
	}
	res.Hooked = hooked
	res.Notes = append(res.Notes, notes...)

	if err := opts.Sink.Flush(ctx); err != nil {
		return nil, rollback(ctx, opts, made, err)
	}
	// THE PLAN'S NOTES COME FIRST and the run's own after, and both are
	// assembled at the END: a note appended to a slice captured up front
	// would be dropped, which is exactly how the Mattermost half lost the
	// notes its own run produced.
	res.Notes = append(notesOf(opts.Plan), res.Notes...)
	return res, nil
}

// seedCredential gives a newly created account the credential every later
// run will recognise.
//
// # THE INVARIANT: a managed account holds exactly one active token, under
// this tool's own label
//
// It is what makes a re-run able to tell "this seat is fine" from "this
// seat needs minting" — and the account-create endpoint cannot establish
// it, because the token it returns is labelled by the instance and takes no
// label from the caller. So a capable instance gets a labelled token minted
// and the create-response one retired, in that order: the recorded value is
// always a live credential, never a window where the seat has none.
//
// A DEGRADED instance keeps the create-response token, because it is the
// only one that will ever exist there. Its re-runs cannot recognise it —
// which is precisely what the degraded note tells the operator.
func seedCredential(ctx context.Context, opts Options, caps Capabilities, account Account, seat provision.Seat) error {
	if !caps.TokenLifecycle {
		// THE CREATE RESPONSE CARRIES THE FIRST TOKEN, and no read ever
		// will again — so it is recorded rather than replaced, which
		// would leave the account holding a credential nothing wrote
		// down.
		if err := opts.Sink.Record(ctx, seat.TokenVar, account.Token); err != nil {
			return fmt.Errorf("record %s: %w", seat.TokenVar, err)
		}
		return nil
	}
	initial, err := opts.Client.Tokens(ctx, account.ID)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	token, err := opts.Client.MintToken(ctx, account.ID, TokenLabel(seat.Handle), expiry(opts))
	if err != nil {
		return fmt.Errorf("mint token: %w", err)
	}
	if err := opts.Sink.Record(ctx, seat.TokenVar, token.Value); err != nil {
		return fmt.Errorf("record %s: %w", seat.TokenVar, err)
	}
	// AFTER the record: the create-response token is a live credential
	// until this line, so retiring it first would leave the seat with
	// nothing if the record then failed.
	//
	// EVERY ROW, with no active check: the account was created moments
	// ago, so all of its tokens are ones this run caused.
	for _, old := range initial {
		if err := opts.Client.RevokeToken(ctx, account.ID, old.ID); err != nil {
			return fmt.Errorf("retire the account's initial token: %w", err)
		}
	}
	return nil
}

// credentialFor decides whether this seat already has a working credential.
//
// # It PROVES it, rather than inferring it
//
// The weaker test — "the variable has a value and the account has some
// token" — reads as provisioned in exactly the case that matters: an
// operator who restored an older env file has a stale value sitting beside
// a live token that is not it, and the seat then authenticates with
// nothing, on every run, forever. So the run takes the value the variable
// actually holds and asks the instance who it is.
func credentialFor(ctx context.Context, opts Options, account Account, seat provision.Seat) (provision.Verdict, bool, error) {
	value, held, err := opts.Sink.Value(ctx, seat.TokenVar)
	if err != nil {
		// UNREADABLE IS NOT ABSENT. Treating it as absent would rotate
		// every live credential in the company because a store blinked.
		return provision.VerdictUnknown, false, fmt.Errorf(
			"cannot read %s, and guessing would either rotate a live token or "+
				"leave a seat with none: %w", seat.TokenVar, err)
	}
	if !held {
		return provision.VerdictRejected, false, nil
	}
	return opts.Client.verify(ctx, value, account.ID), true, nil
}

// ExpiryWarning is how far ahead a kept credential's death is announced.
//
// NOTHING IN CREWLET RENEWS A TOKEN. `expiry` says it outright: an expiry
// nobody renews is an outage with a date on it, and the only thing that mints
// a replacement is an operator running this command again. So the window is
// sized to the human loop rather than to the machine — 30 days spans a
// monthly ops cadence with room to schedule the re-run, where a week would
// land inside a single holiday and a quarter would be noise on every run for
// three months.
const ExpiryWarning = 30 * 24 * time.Hour

// expiryNote warns that the credential this run kept has a death date.
//
// Only ever a NOTE. The token still authenticates — that is what put the run
// on this branch — and re-minting a working credential early would break
// whatever is holding the old value for no reason other than a calendar.
func expiryNote(ctx context.Context, opts Options, account Account, seat provision.Seat) string {
	tokens, err := opts.Client.Tokens(ctx, account.ID)
	if err != nil {
		// The seat is fine: its credential was just verified against the
		// instance. This is a second opinion about how long that lasts.
		return ""
	}
	label := TokenLabel(seat.Handle)
	deadline := now(opts).Add(ExpiryWarning)
	for _, token := range tokens {
		if !token.Active || !strings.EqualFold(token.Label, label) {
			continue
		}
		if token.ExpiresAt.IsZero() || token.ExpiresAt.After(deadline) {
			continue
		}
		return fmt.Sprintf(
			"%s: the token in %s expires %s and NOTHING renews it — re-run "+
				"this command with -rotate before then, or the seat stops "+
				"authenticating with no other warning",
			seat.Handle, seat.TokenVar,
			token.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return ""
}

// verify asks the instance who a credential authenticates as.
func (c *Client) verify(ctx context.Context, value, wantID string) provision.Verdict {
	probe, err := NewClient(ClientOptions{
		URL: c.base, Workspace: c.workspace, APIKey: value, HTTP: c.http,
	})
	if err != nil {
		return provision.VerdictRejected
	}
	who, err := probe.Me(ctx)
	switch {
	case err == nil && strings.EqualFold(who.ID, wantID):
		return provision.VerdictSelf
	case err == nil:
		return provision.VerdictOther
	case Status(err) == http.StatusUnauthorized, Status(err) == http.StatusForbidden:
		return provision.VerdictRejected
	default:
		// A 5xx or a dropped connection. NOT a rejection: re-minting on
		// "cannot tell" destroys a credential that works.
		return provision.VerdictUnknown
	}
}

// now is the run's clock.
func now(opts Options) time.Time {
	if opts.Now != nil {
		return opts.Now()
	}
	return time.Now()
}

// index enumerates the workspace's members, keyed the way a seat is matched.
//
// # The username field is a hard requirement, checked here
//
// A seat's account is found by the username derived from its handle, because
// that is the only identifier a run can compute before the account exists.
// An instance whose member rows carry no username cannot be matched at all —
// so every re-run would create another account for every seat, mint it a
// token, and write that token over the live one. Refusing here costs an
// operator a message; not refusing costs them a workspace of duplicates and
// a company of seats authenticating with credentials nothing holds.
func index(members []Account) (map[string]Account, error) {
	out := make(map[string]Account, len(members))
	for _, m := range members {
		if name := strings.ToLower(strings.TrimSpace(m.Username)); name != "" {
			out[name] = m
		}
	}
	if len(members) > 0 && len(out) == 0 {
		return nil, errors.New(
			"plane: this workspace's member rows carry no username, so an " +
				"account created for a seat could never be found again — " +
				"every run would create another one. The username field is " +
				"part of the same service-account support the preflight " +
				"probes for; an instance that has one and not the other " +
				"cannot be provisioned safely")
	}
	return out, nil
}

// humanSeatNotes reports the human seats Plane cannot reach.
//
// # Validated, never created
//
// A human seat is a person who already has an account; provisioning one
// would be creating a second identity for somebody who has one. What CAN go
// wrong is the id: `contact.plane_user_id` is a UUID an operator copies by
// hand, and a wrong one silently addresses nobody — the assignment lands,
// the mention renders as raw markup, and no notification is ever delivered.
// So a declared id is checked against the member table, and a human seat
// with no id at all is named beside it, because the table printed with this
// report is exactly what fills it in.
func humanSeatNotes(o *org.Organization, members []Account) []string {
	if o == nil {
		return nil
	}
	known := make(map[string]bool, len(members))
	for _, m := range members {
		known[strings.ToLower(strings.TrimSpace(m.ID))] = true
	}
	var (
		notes      []string
		undeclared []string
	)
	for seat := range o.AllRoles() {
		if seat.IsAgent() {
			continue
		}
		id := ""
		if seat.Contact != nil {
			id = strings.TrimSpace(seat.Contact.PlaneUserID)
		}
		switch {
		case id == "":
			undeclared = append(undeclared, seat.Handle())
		case len(provision.ReferencedVars(id)) > 0:
			// A ${VAR} resolves at run time against an environment this
			// command does not have, so checking it here would report a
			// working config as broken.
		case !known[strings.ToLower(id)]:
			notes = append(notes, fmt.Sprintf(
				"%s: contact.plane_user_id names a user this workspace does "+
					"not have — assignments and mentions for this seat will "+
					"address nobody, silently. The member table below has the "+
					"ids", seat.Handle()))
		}
	}
	if len(undeclared) > 0 {
		sort.Strings(undeclared)
		notes = append(notes, fmt.Sprintf(
			"no contact.plane_user_id for %s, so nothing in Plane can be "+
				"assigned to or mention them; the member table below has the ids",
			strings.Join(undeclared, ", ")))
	}
	return notes
}

// inactiveNote reports a membership Plane is keeping deactivated.
//
// A NOTE rather than a repair: reactivating is a workspace-admin decision
// about a person's deliberate act, and a provisioner that silently undid
// "remove from project" every run would be fighting the operator. Saying it
// out loud is the honest half — the alternative is a seat that looks
// provisioned and reads nothing.
func inactiveNote(ctx context.Context, opts Options, project Project,
	account Account, seat provision.Seat,
) string {
	members, err := opts.Client.ProjectMembers(ctx, project.ID)
	if err != nil {
		// Not worth failing the run: the add succeeded, and this is a
		// second opinion about it.
		return ""
	}
	for _, member := range members {
		if member.Member != account.ID || member.Active {
			continue
		}
		return fmt.Sprintf(
			"%s is a member of %s but the membership is DEACTIVATED — Plane "+
				"keeps the row when somebody removes a member in the UI, so "+
				"the add reported success and the seat still cannot see the "+
				"project. Re-activate it in the project's member list",
			seat.Handle, project.Identifier)
	}
	return ""
}

// ensureAccount finds or creates a seat's service account.
func ensureAccount(ctx context.Context, opts Options, accounts map[string]Account,
	seat provision.Seat,
) (Account, bool, []string, error) {
	p := opts.Config.Provisioning
	username := AccountUsername(p, seat.Handle)
	if existing, ok := accounts[strings.ToLower(username)]; ok {
		if !existing.IsBot {
			// A HUMAN WHOSE NAME COLLIDES. Minting into their account
			// would hand an agent a person's identity, and every action
			// the agent took would be attributed to them.
			return Account{}, false, nil, fmt.Errorf(
				"the workspace member %q is not a service account — refusing "+
					"to provision a seat onto a person's identity; change "+
					"integrations.plane.provisioning.username_prefix", username)
		}
		// DRIFT IS REPORTED, NOT REPAIRED. The account exists and works;
		// what has moved is how it presents itself, and a provisioner that
		// silently rewrote a display name an admin set by hand would be
		// undoing somebody's deliberate edit on every run. Naming it is what
		// lets them decide.
		return existing, false, accountDrift(opts, existing, seat), nil
	}

	account, err := opts.Client.CreateAccount(ctx, username, seat.Role,
		AccountRole(p, seat.Handle))
	if err != nil {
		return Account{}, false, nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(account.Username), username) {
		// THE INSTANCE GENERATED ITS OWN NAME. The account exists and is
		// unfindable by the only identifier this run can derive, so the
		// next run would create another — which is the duplicate-account
		// failure, arriving one run later. Undo it and stop.
		if err := opts.Client.DeleteAccount(context.WithoutCancel(ctx), account.ID); err != nil {
			return Account{}, false, nil, fmt.Errorf(
				"this instance ignored the requested username (it created %q "+
					"instead of %q), and the account it made could not be "+
					"removed: %w — delete it by hand before re-running",
				account.Username, username, err)
		}
		return Account{}, false, nil, fmt.Errorf(
			"this instance ignored the requested username and created %q "+
				"instead of %q, so no later run could find the account "+
				"again. The account was deleted and nothing was minted",
			account.Username, username)
	}
	if strings.TrimSpace(account.Token) == "" {
		return Account{}, false, nil, errors.New(
			"the account was created but the response carried no token, and " +
				"a service account's first token is served exactly once")
	}
	return account, true, nil, nil
}

// accountDrift names the ways an existing service account has moved away
// from what the company config would create today.
//
// REPORTED, NEVER REPAIRED. Both fields are ones a workspace admin can set
// by hand in the Plane UI, and a provisioner that rewrote them on every run
// would silently undo a deliberate edit — and would do it without ever
// saying so, because a converged run prints nothing. The workspace role in
// particular is a privilege decision: quietly demoting an account somebody
// promoted for a reason is the failure mode with teeth, and quietly
// promoting one is worse.
//
// # An absent field is unknown, not drift
//
// Both comparisons skip a zero value, because the member listing is the only
// place they are read from and which fields a given Plane build serves there
// has moved between versions. An account this tool created always has both,
// so the cost of the guard is one missed note on a workspace where somebody
// blanked a display name by hand — and the cost of not having it is the same
// two notes on every seat, on every run, on an instance that simply does not
// serve the column.
func accountDrift(opts Options, existing Account, seat provision.Seat) []string {
	var notes []string
	if have, want := strings.TrimSpace(existing.DisplayName), strings.TrimSpace(seat.Role); have != "" &&
		want != "" && !strings.EqualFold(have, want) {
		notes = append(notes, fmt.Sprintf(
			"%s: the account's display name in Plane is %q, but the company "+
				"config calls the seat %q — every issue it touches is "+
				"attributed under the Plane name. Rename it in the workspace "+
				"member list, or change the role's title",
			seat.Handle, have, want))
	}
	if want := AccountRole(opts.Config.Provisioning, seat.Handle); existing.Role != 0 &&
		existing.Role != want {
		notes = append(notes, fmt.Sprintf(
			"%s: the account holds the %s workspace role and this config "+
				"would create it as %s — the provisioner does not change the "+
				"role of an account that already exists, so change it in "+
				"Plane or set integrations.plane.provisioning.roles.%s",
			seat.Handle, roleName(existing.Role), roleName(want), seat.Handle))
	}
	return notes
}

// rotate mints a fresh token for an existing account and retires the old
// ones this tool minted.
//
// # Record before revoke
//
// The new token is written down BEFORE the old ones are retired, so a sink
// failure leaves the seat authenticating with the credential it already had.
// The other order leaves it with none.
func rotate(ctx context.Context, opts Options, account Account, seat provision.Seat) (string, int, error) {
	label := TokenLabel(seat.Handle)
	existing, err := opts.Client.Tokens(ctx, account.ID)
	if err != nil {
		return "", 0, fmt.Errorf("list tokens: %w", err)
	}
	token, err := opts.Client.MintToken(ctx, account.ID, label, expiry(opts))
	if err != nil {
		return "", 0, fmt.Errorf("mint token: %w", err)
	}
	if err := opts.Sink.Record(ctx, seat.TokenVar, token.Value); err != nil {
		// The caller rolls back, which revokes the token just minted.
		return token.ID, 0, fmt.Errorf("record %s: %w", seat.TokenVar, err)
	}
	retired := 0
	// `existing` was read BEFORE the mint, so it cannot contain the token
	// just minted — which is what makes revoking from it safe with no
	// self-exclusion check.
	for _, old := range existing {
		// ONLY THIS TOOL'S OWN LABEL: a workspace admin may have minted a
		// token for this account by hand, and revoking it would break
		// whatever is using it — silently, since nothing here knows what
		// that is.
		//
		// ONLY ACTIVE ROWS, which costs nothing at the vendor (a revoke is
		// idempotent) and bounds the requests: every rotation leaves
		// another retired row behind, so a run without this issues one
		// more pointless DELETE than the run before it, for ever.
		if !old.Active || old.Label != label {
			continue
		}
		if err := opts.Client.RevokeToken(ctx, account.ID, old.ID); err != nil {
			return token.ID, retired, fmt.Errorf("revoke the previous token: %w", err)
		}
		retired++
	}
	return token.ID, retired, nil
}

// TokenLabel is the label this tool mints under.
//
// It is the ONLY thing that distinguishes a credential this tool owns from
// one an administrator created by hand, and rotation revokes by it — so it
// is a named constant rather than a format string repeated at two call
// sites, where the two would eventually differ and rotation would quietly
// stop retiring anything.
func TokenLabel(handle string) string { return "crewlet-" + handle }

// expiry is the instant a minted token dies, or the zero time for never.
func expiry(opts Options) time.Time {
	days := 0
	if p := opts.Config.Provisioning; p != nil {
		days = p.TokenExpiryDays
	}
	if opts.ExpiryDays != nil {
		days = *opts.ExpiryDays
	}
	if days == 0 {
		// ZERO MEANS NEVER — the documented meaning of the config field
		// and the endpoint's own semantics for an omitted expiry. It is
		// also the DEFAULT, deliberately: nothing in Crewlet renews a
		// credential on a schedule, so an expiry nobody renews is an
		// outage with a date on it. A company whose policy requires one
		// sets the field and owns the re-run.
		return time.Time{}
	}
	return now(opts).AddDate(0, 0, days)
}

// resolveProjects turns the configured identifiers into project ids.
//
// BEFORE ANY MUTATION and ALL OR NOTHING: a typo'd identifier means seats
// that never get access to the project they were meant to work in, which
// looks exactly like agents ignoring their work. Failing here is free —
// nothing has been created yet — and the message can name what does exist.
func resolveProjects(ctx context.Context, opts Options) ([]Project, error) {
	p := opts.Config.Provisioning
	if p == nil || len(p.Projects) == 0 {
		return nil, nil
	}
	c := opts.Client
	all, err := c.Projects(ctx)
	if err != nil {
		return nil, fmt.Errorf("plane: list projects: %w", err)
	}
	byIdentifier := make(map[string]Project, len(all))
	known := make([]string, 0, len(all))
	for _, project := range all {
		byIdentifier[strings.ToUpper(strings.TrimSpace(project.Identifier))] = project
		known = append(known, project.Identifier)
	}
	var (
		out     []Project
		unknown []string
		seen    = map[string]bool{}
	)
	for _, want := range p.Projects {
		key := strings.ToUpper(strings.TrimSpace(want))
		project, ok := byIdentifier[key]
		switch {
		case !ok:
			unknown = append(unknown, want)
		case !seen[project.ID]:
			seen[project.ID] = true
			out = append(out, project)
		}
	}
	if len(unknown) == 0 {
		return out, nil
	}
	if !opts.CreateProjects {
		sort.Strings(known)
		return nil, fmt.Errorf(
			"plane: integrations.plane.provisioning.projects names %s, which "+
				"this workspace does not have. It has: %s. Pass -create-projects "+
				"to have this run make them",
			strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	for _, identifier := range unknown {
		// NAMED AFTER THE IDENTIFIER, which an operator renames in the UI
		// the moment they see it. Guessing a prettier name from the
		// config would be a name nobody chose that nobody can search for.
		project, err := c.CreateProject(ctx, identifier, identifier)
		if err != nil {
			return nil, fmt.Errorf("plane: create project %s: %w", identifier, err)
		}
		out = append(out, project)
	}
	return out, nil
}

// decommission deletes the managed accounts whose seats have left.
//
// # The prefix is the whole safety property
//
// "Managed" means: a service account whose username starts with the
// configured prefix and whose handle no seat in the config claims. Both
// halves matter — without the prefix a workspace's own bots match, and
// without the bot check a person whose name happens to start with
// `crewlet-` matches. A wrong delete here cascades: the instance removes
// every token and membership and deactivates the user.
//
// # The prefix can never be empty, and that is load-bearing
//
// An empty prefix would make every service account in the workspace a
// candidate, which is the one mistake here with no undo. It cannot happen
// because [AccountUsername] defaults one — a company that clears
// `username_prefix` gets `crewlet-`, not "". That default is the safety
// property; do not make it optional.
func decommission(ctx context.Context, opts Options, accounts map[string]Account) ([]string, []string, error) {
	p := opts.Config.Provisioning
	prefix := strings.ToLower(AccountUsername(p, ""))
	keep := make(map[string]bool, len(opts.Plan.Seats))
	for _, seat := range opts.Plan.Seats {
		keep[strings.ToLower(AccountUsername(p, seat.Handle))] = true
	}
	var (
		removed []string
		notes   []string
	)
	for _, username := range sortedKeys(accounts) {
		account := accounts[username]
		switch {
		case !strings.HasPrefix(username, prefix), keep[username]:
			continue
		case !account.IsBot:
			// A PERSON WHOSE NAME MATCHES THE PREFIX is never deleted,
			// and is reported: it is either a naming collision the
			// operator has to resolve or a prefix that is too broad, and
			// both are things they need to know before the next run.
			notes = append(notes, fmt.Sprintf(
				"%s matches the managed prefix but is not a service account, "+
					"so it was left alone — check that "+
					"provisioning.username_prefix is not catching people",
				username))
			continue
		}
		if err := opts.Client.DeleteAccount(ctx, account.ID); err != nil {
			return removed, notes, fmt.Errorf("plane: decommission %s: %w", username, err)
		}
		removed = append(removed, username)
	}
	return removed, notes, nil
}

// sortedKeys is the account index in a stable order, so a report and a
// sequence of deletions read the same way on every run.
func sortedKeys(accounts map[string]Account) []string {
	out := make([]string, 0, len(accounts))
	for name := range accounts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ensureWebhook registers the workspace webhook, or converges the one that
// is already there.
//
// # The secret is the asymmetry
//
// Plane generates the signing secret and serves it in the create response
// and nowhere else — not the list, not the retrieve, not the update. So a
// webhook this run CREATES yields a secret to capture, and one that already
// exists yields none: its toggles converge and its secret stays whatever it
// was. An operator who lost that value rotates it by deleting the webhook,
// which is what the note says.
func ensureWebhook(ctx context.Context, opts Options, made *[]minted) (string, []string, error) {
	target := webhookTarget(opts.WebhookBase)
	if target == "" {
		return "", []string{
			"no webhook was registered: pass the deployment's public base URL " +
				"to register one, or add it by hand — without it the workspace " +
				"delivers nothing and the integration looks idle rather than " +
				"unconfigured"}, nil
	}
	secretVar, ok := provision.SoleVar(opts.Config.WebhookSecret)
	if !ok {
		return "", nil, fmt.Errorf(
			"plane: integrations.plane.webhook_secret is not a whole ${VAR} "+
				"reference, so there is nowhere to record the secret Plane "+
				"generates — and it is served once, at creation. Point it at "+
				"a variable, or drop -public-url and register %s by hand", target)
	}

	hooks, err := opts.Client.Webhooks(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("plane: list webhooks: %w", err)
	}
	var nearby []string
	for _, hook := range hooks {
		if hook.URL != target {
			// MATCHED BYTE-EXACT, because that is what identifies "our"
			// hook: a workspace may carry hooks somebody else
			// registered, and a run that reconfigured the first one it
			// found would take down an unrelated integration.
			//
			// Plane's own duplicate-URL constraint is byte-exact too,
			// so a hook that differs only in a trailing slash or an
			// explicit :443 is a SECOND hook that fires on the same
			// events — every delivery arriving twice. It is reported and
			// never touched: a foreign hook is not this run's to remove,
			// and the operator is the only one who knows whether it was
			// deliberate.
			if sameEndpoint(hook.URL, target) {
				nearby = append(nearby, hook.URL)
			}
			continue
		}
		if opts.RecreateWebhook {
			// DESTRUCTIVE AND ASKED FOR: remaking the hook mints a fresh
			// secret and invalidates the one every other deployment of
			// this company holds. It is the only recovery for a secret
			// that was never recorded, because the value cannot be read
			// back — which is exactly why it is a flag.
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := opts.Client.DeleteWebhook(ctx, hook.ID); err != nil {
				return "", nil, fmt.Errorf("plane: replace webhook: %w", err)
			}
			break
		}
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		updated, err := opts.Client.UpdateWebhook(ctx, hook.ID, target)
		if err != nil {
			return "", nil, fmt.Errorf("plane: update webhook: %w", err)
		}
		var notes []string
		_, held, err := opts.Sink.Value(ctx, secretVar)
		if err != nil || !held {
			// SAID ONLY WHEN IT MATTERS. A sink that already holds the
			// secret needs no advice, and printing it every run trains
			// an operator to skip the notes.
			notes = append(notes, fmt.Sprintf(
				"the webhook for %s already existed; its entity toggles were "+
					"brought in line but its secret was NOT re-read — Plane "+
					"serves that once, at creation, and %s does not hold it. "+
					"Pass -recreate-webhook to mint a fresh one, which "+
					"invalidates the secret every other deployment holds",
				target, secretVar))
		}
		notes = append(notes, pageToggleNote(updated)...)
		return target, append(notes, duplicateHookNote(nearby, target)...), nil
	}

	created, err := opts.Client.CreateWebhook(ctx, target)
	if created.ID != "" {
		// TRACKED BEFORE THE ERROR IS JUDGED, exactly as a minted token
		// is: the hook exists on the workspace the moment the call
		// returns, and one this run cannot use is one it has to remove.
		*made = append(*made, minted{handle: "webhook " + target, webhookID: created.ID})
	}
	if err != nil {
		return "", nil, fmt.Errorf("plane: create webhook: %w", err)
	}
	if err := opts.Sink.Record(ctx, secretVar, created.SecretKey); err != nil {
		return "", nil, fmt.Errorf("plane: record %s: %w", secretVar, err)
	}
	return target, append(pageToggleNote(created),
		duplicateHookNote(nearby, target)...), nil
}

// sameEndpoint reports two URLs that address the same place while being
// different strings.
//
// Only the differences Plane's byte-exact uniqueness lets through, and only
// ones that are unambiguously the same endpoint: a trailing slash, case in
// the scheme or host, and the default port written out. Anything else is a
// different URL and none of this run's business.
func sameEndpoint(a, b string) bool {
	return a != b && canonicalURL(a) == canonicalURL(b)
}

func canonicalURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return raw
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Host)
	switch {
	case parsed.Scheme == "https" && strings.HasSuffix(host, ":443"):
		host = strings.TrimSuffix(host, ":443")
	case parsed.Scheme == "http" && strings.HasSuffix(host, ":80"):
		host = strings.TrimSuffix(host, ":80")
	}
	parsed.Host = host
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String()
}

// duplicateHookNote reports hooks that would double-deliver.
func duplicateHookNote(nearby []string, target string) []string {
	if len(nearby) == 0 {
		return nil
	}
	sort.Strings(nearby)
	return []string{fmt.Sprintf(
		"this workspace also carries %s, which addresses the same endpoint as "+
			"%s while being a different string — Plane's duplicate check is "+
			"byte-exact, so both fire and every event arrives twice. Nothing "+
			"was deleted: remove whichever is not yours",
		strings.Join(nearby, ", "), target)}
}

// pageToggleNote reports an instance that dropped the page entity.
//
// SILENTLY DROPPED rather than refused — an unknown field is ignored by the
// serializer — so the only evidence is the echo. Without page deliveries the
// tool-skill sync never learns that a skill page changed, and it degrades to
// whatever it read at boot: no error anywhere, just skills that stop
// updating.
func pageToggleNote(hook Webhook) []string {
	if hook.Page {
		return nil
	}
	return []string{
		"this instance dropped the `page` webhook entity, so page edits will " +
			"not be delivered: the tool-skill sync will keep serving what it " +
			"read at boot and no error will be raised"}
}

// webhookTarget is the endpoint Plane delivers to.
func webhookTarget(base string) string {
	if base = strings.TrimRight(strings.TrimSpace(base), "/"); base == "" {
		return ""
	}
	return base + "/webhooks/plane"
}

// rollback undoes what this run created and clears what it recorded.
//
// It returns the ORIGINAL failure with the cleanup's own problems appended,
// never in place of it: the reason the run stopped is what an operator has
// to fix, and a cleanup error that replaced it would hide the cause behind
// its consequence.
func rollback(ctx context.Context, opts Options, made []minted, cause error) error {
	if len(made) == 0 {
		if err := opts.Sink.Discard(ctx); err != nil {
			return fmt.Errorf("%w\n\nAND THE SINK COULD NOT BE CLEARED: %w", cause, err)
		}
		return cause
	}
	// DETACHED, because the failure may BE a cancelled context — and a
	// rollback that inherited it would do nothing at all, leaving every
	// minted credential live.
	ctx = context.WithoutCancel(ctx)
	var problems []string
	for _, m := range made {
		var err error
		switch {
		case m.webhookID != "":
			// A webhook whose secret was recorded and then discarded
			// delivers signed requests the engine cannot verify, which
			// reads as a broken integration rather than an absent one.
			err = opts.Client.DeleteWebhook(ctx, m.webhookID)
		case m.createdAccount:
			// Deleting the account cascades its tokens away, which is
			// why a created account needs no separate revoke.
			err = opts.Client.DeleteAccount(ctx, m.accountID)
		case m.tokenID != "":
			err = opts.Client.RevokeToken(ctx, m.accountID, m.tokenID)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", m.handle, err))
		}
	}
	if err := opts.Sink.Discard(ctx); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) == 0 {
		return fmt.Errorf("%w (the %d credential(s) this run created were undone)",
			cause, len(made))
	}
	return fmt.Errorf("%w\n\nAND THE CLEANUP DID NOT FINISH — these may still be "+
		"live and must be removed by hand:\n  %s",
		cause, strings.Join(problems, "\n  "))
}

func notesOf(p *provision.Plan) []string {
	if p == nil {
		return nil
	}
	return p.Notes
}
