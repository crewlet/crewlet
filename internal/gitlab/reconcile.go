package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/whsec"
)

// Reconcile brings a GitLab instance in line with the company config.
//
// # It is a reconcile, not a setup
//
// Running it twice must be safe and must be quiet: an account that exists is
// found rather than re-created, a membership that holds is left alone. What
// it does do every time is MINT A TOKEN, because a personal access token's
// value is returned once and GitLab will not show it again — so there is no
// "already correct" state to detect, and the honest thing is to rotate and
// say so.
//
// # A run that cannot record what it minted revokes it
//
// Between the vendor creating a token and the sink recording it there is a
// window where the only copy of a live credential is in this process's
// memory. If recording fails, the token exists, nothing can use it, and
// nobody knows to remove it — so the run revokes what it minted and discards
// what it recorded, and reports both.

// Result is what one reconcile did, for the report.
type Result struct {
	// Created names the seats whose accounts this run created.
	Created []string
	// Rotated names the seats whose tokens this run minted.
	Rotated []string
	// Kept names the seats whose existing token was left alone — the
	// SUCCESSFUL outcome of a re-run, said out loud because a silent
	// report reads as a run that did nothing.
	Kept []string
	// Decommissioned names the accounts this run deleted.
	Decommissioned []string
	// Hooked is the webhook target this run registered or re-pointed, or
	// empty when webhooks were not part of it.
	Hooked string
	// HookedOn names WHERE it was registered: the single element "group",
	// or one entry per project. Reported because the two are not
	// interchangeable — a group hook covers projects added later and a set
	// of project hooks does not, and an operator reading "webhook
	// registered" cannot tell which they got.
	HookedOn []string
	// Notes carries the plan's notes plus anything the run itself found.
	Notes []string

	// Recorded counts the values this run wrote to the sink — seat tokens
	// plus a minted or rotated signing secret.
	//
	// A COUNT rather than the names, because the names are the variables
	// holding live credentials and this number's only job is deciding
	// whether the report tells the operator what still has to happen for
	// those values to reach a running engine. Zero means a re-run that
	// changed nothing, and instructing that operator to restart anything
	// would be noise.
	Recorded int
}

// Options are one reconcile's inputs.
type Options struct {
	// Client talks to the instance as an administrator.
	Client *Client

	// Config is the company's gitlab block.
	Config *config.GitLab

	// Plan is what to do, from [PlanFor].
	Plan *provision.Plan

	// Sink records what is minted.
	Sink provision.TokenSink

	// WebhookBase is this deployment's public base URL, or empty to skip
	// webhook registration.
	//
	// SKIPPED RATHER THAN GUESSED. A hook pointing at the wrong host is
	// worse than no hook: the instance reports a healthy integration, and
	// the deliveries go somewhere nobody is looking.
	WebhookBase string

	// SigningSecret is the value the hook is registered with, resolved.
	//
	// Empty means the config's ${VAR} answered nothing, and the run MINTS
	// one — see [mintSigningSecret]. GitLab's signing token is
	// caller-supplied and write-only: it is never returned, so a hook
	// registered with an empty one verifies nothing and there is no way
	// to read back what it should have been.
	SigningSecret string

	// SigningSecretVar is the variable the minted secret is recorded
	// under, empty when the config's signing_secret is not a whole ${VAR}
	// reference. Mirrors the seat tokens' mint-into-${VAR} contract: the
	// config's reference is what says where the value belongs.
	SigningSecretVar string

	// Decommission deletes managed service accounts whose seats have left
	// the config. Off by default: it is the one destructive direction,
	// and a company mid-edit looks exactly like a company that removed a
	// seat.
	Decommission bool

	// Rotate forces a fresh token for every seat, including seats whose
	// current one still works.
	//
	// # Why it is a flag rather than what a run does
	//
	// GitLab returns a personal access token's value once, so a
	// provisioner cannot check that what it recorded last time still
	// matches. The tempting answer is to mint every run — and that is an
	// outage: the engine is running with the OLD value, and rotating
	// revokes the credential every agent is currently authenticating
	// with. An operator adding a tenth seat would take the other nine
	// down, from a command whose whole promise is that it is safe to
	// re-run.
	Rotate bool

	// ExpiryDays is the lifetime minted onto each token, or nil for the
	// instance default. A POINTER because zero is meaningful — it means
	// "send no expiry" — and an int's zero value would silently override
	// every caller that did not set it.
	ExpiryDays *int

	// Now is the clock expiry is computed from. Nil takes the wall clock.
	Now func() time.Time
}

// Reconcile runs one pass.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if opts.Client == nil {
		return nil, errors.New("gitlab: no client")
	}
	if opts.Sink == nil {
		return nil, provision.ErrNoSink
	}
	if opts.Plan == nil || opts.Plan.Empty() {
		return &Result{Notes: notesOf(opts.Plan)}, nil
	}
	p := opts.Config.Provisioning
	group, found, err := opts.Client.GroupByPath(ctx, p.Group)
	if err != nil {
		return nil, fmt.Errorf("gitlab: resolve group %q: %w", p.Group, err)
	}
	if !found {
		return nil, fmt.Errorf(
			"gitlab: no group %q on this instance — service accounts are "+
				"owned by a group, so it has to exist before seats can be "+
				"provisioned", p.Group)
	}

	res := &Result{Notes: notesOf(opts.Plan)}

	// PROJECTS ARE RESOLVED BEFORE ANYTHING IS MUTATED, and a missing one
	// is dropped rather than fatal — see [resolveProjects].
	projects, notes, err := resolveProjects(ctx, opts.Client, p.Projects)
	if err != nil {
		return nil, err
	}
	res.Notes = append(res.Notes, notes...)

	// MINTED IDS ARE TRACKED so a failure can revoke them. Held here
	// rather than looked up on the way out, because the account whose
	// token needs revoking may be one this run just created.
	minted := map[string]mintedToken{}

	for _, seat := range opts.Plan.Seats {
		user, created, err := ensureAccount(ctx, opts.Client, group.ID, p, seat)
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("gitlab: %s: %w", seat.Handle, err))
		}
		if created {
			res.Created = append(res.Created, seat.Handle)
		}
		level := accessLevel(p, seat.Handle)
		if err := opts.Client.AddGroupMember(ctx, group.ID, user.ID, level); err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("gitlab: %s: group membership: %w", seat.Handle, err))
		}
		for _, project := range projects {
			if err := opts.Client.AddProjectMember(ctx, project, user.ID, level); err != nil {
				return nil, rollback(ctx, opts, minted,
					fmt.Errorf("gitlab: %s: membership of %s: %w",
						seat.Handle, project, err))
			}
		}

		verdict, held := provision.VerdictRejected, true
		if !created && !opts.Rotate {
			verdict, held, err = credentialFor(ctx, opts, user.ID, seat)
			if err != nil {
				return nil, rollback(ctx, opts, minted,
					fmt.Errorf("gitlab: %s: %w", seat.Handle, err))
			}
		}
		switch verdict {
		case provision.VerdictSelf:
			res.Kept = append(res.Kept, seat.Handle)
			continue
		case provision.VerdictOther:
			// A COPY-PASTED VARIABLE. Minting over it hands this seat a
			// second identity while the other keeps authenticating as
			// one account from two places, and nothing reports it.
			return nil, rollback(ctx, opts, minted, fmt.Errorf(
				"gitlab: %s: %s holds a token that authenticates as a "+
					"different account — give this seat its own variable",
				seat.Handle, seat.TokenVar))
		case provision.VerdictUnknown:
			// LEFT EXACTLY AS IT WAS. Re-minting on "cannot tell"
			// destroys a token that works; the recovery for one that
			// does not is a -rotate away.
			res.Kept = append(res.Kept, seat.Handle)
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: could not check whether the token in %s still works, so "+
					"it was left alone — re-run with -rotate if this seat is "+
					"failing to authenticate", seat.Handle, seat.TokenVar))
			continue
		}

		token, err := opts.Client.CreateToken(ctx, user.ID,
			TokenName(seat.Handle), tokenScopes(p), expiry(opts))
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("gitlab: %s: mint token: %w", seat.Handle, err))
		}
		minted[seat.Handle] = mintedToken{
			userID: user.ID, tokenID: token.ID, createdAccount: created,
		}
		// RECORDED IMMEDIATELY. The value above is the only copy there
		// will ever be.
		if err := opts.Sink.Record(ctx, seat.TokenVar, token.Value); err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("gitlab: %s: record %s: %w", seat.Handle, seat.TokenVar, err))
		}
		res.Rotated = append(res.Rotated, seat.Handle)
		res.Recorded++
		if created {
			continue
		}
		// RETIRED AFTER THE RECORD, and only this tool's own: an
		// administrator may have minted a token on this account by hand,
		// and revoking it would break whatever is using it — silently,
		// since nothing here knows what that is.
		retired, err := retirePrevious(ctx, opts, user.ID, seat, token.ID)
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("gitlab: %s: %w", seat.Handle, err))
		}
		if !held && retired > 0 {
			// THE SURPRISING CASE, and the only one that earns a note:
			// the operator asked for nothing, but the variable was empty
			// on this machine while a live token existed on the account
			// — so a rotation happened anyway, and whatever is running
			// with the old value is now failing to authenticate.
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: the account held a working token but %s did not, so a "+
					"fresh one was minted and the old one retired — a running "+
					"engine holding the old value has to be restarted",
				seat.Handle, seat.TokenVar))
		}
	}

	if opts.Decommission {
		removed, notes, err := decommission(ctx, opts, group.ID)
		if err != nil {
			return nil, rollback(ctx, opts, minted, err)
		}
		res.Decommissioned = removed
		res.Notes = append(res.Notes, notes...)
	}

	if target := webhookTarget(opts.WebhookBase); target != "" {
		secret, note, recorded, err := signingSecret(ctx, opts)
		if err != nil {
			return nil, rollback(ctx, opts, minted, err)
		}
		if note != "" {
			res.Notes = append(res.Notes, note)
		}
		if recorded {
			res.Recorded++
		}
		opts.SigningSecret = secret
		hooked, notes, err := ensureHooks(ctx, opts, group.ID, projects, target)
		if err != nil {
			return nil, rollback(ctx, opts, minted, err)
		}
		res.Hooked, res.HookedOn = target, hooked
		res.Notes = append(res.Notes, notes...)
	} else {
		res.Notes = append(res.Notes,
			"no webhook was registered: pass the deployment's public base URL "+
				"to register one, or add it by hand — without it the instance "+
				"delivers nothing and the integration looks idle rather than "+
				"unconfigured")
	}

	if err := opts.Sink.Flush(ctx); err != nil {
		return nil, rollback(ctx, opts, minted, err)
	}
	return res, nil
}

// mintedToken is one credential this run created, as its rollback needs it.
type mintedToken struct {
	userID  int
	tokenID int
	// createdAccount says the account is this run's, which decides HOW
	// the rollback undoes it: everything on an account nothing else ever
	// minted on, versus exactly the one token on an account that already
	// existed. Sweeping the second would take an administrator's own
	// token with no way to tell that it had.
	createdAccount bool
}

// TokenName is the name this tool mints under.
//
// It is the ONLY thing distinguishing a token this tool owns from one an
// administrator created by hand, and both the keep-or-mint decision and the
// retire step key on it — so it is a named function rather than a format
// string repeated at three call sites, where the three would eventually
// differ and rotation would quietly stop retiring anything.
func TokenName(handle string) string { return "crewlet-" + handle }

// credentialFor decides whether this seat already has a working token.
//
// # It PROVES it, rather than inferring it
//
// The weaker test — "the variable has a value and the account has some
// token" — reads as provisioned in exactly the case that matters: an
// operator who restored an older env file has a stale value sitting beside
// a live token that is not it, and the seat then authenticates with
// nothing, on every run, forever. So the run takes the value the variable
// actually holds and asks the instance who it is.
func credentialFor(ctx context.Context, opts Options, userID int, seat provision.Seat) (provision.Verdict, bool, error) {
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
	return opts.Client.verify(ctx, value, userID), true, nil
}

// verify asks the instance who a token authenticates as.
func (c *Client) verify(ctx context.Context, value string, wantID int) provision.Verdict {
	probe, err := NewClient(ClientOptions{URL: c.base, Token: value, HTTP: c.http})
	if err != nil {
		return provision.VerdictRejected
	}
	var who User
	err = probe.get(ctx, "/user", nil, &who)
	switch {
	case err == nil && who.ID == wantID:
		return provision.VerdictSelf
	case err == nil:
		return provision.VerdictOther
	case status(err) == http.StatusUnauthorized, status(err) == http.StatusForbidden:
		return provision.VerdictRejected
	default:
		// A 5xx or a dropped connection. NOT a rejection: re-minting on
		// "cannot tell" destroys a token that works.
		return provision.VerdictUnknown
	}
}

// status reads the HTTP status off an error, or 0 where there is none.
func status(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}

// retirePrevious revokes this tool's earlier tokens on an existing account.
func retirePrevious(ctx context.Context, opts Options, userID int, seat provision.Seat, keep int) (int, error) {
	tokens, err := opts.Client.Tokens(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("list tokens: %w", err)
	}
	retired := 0
	for _, token := range tokens {
		// ALREADY-REVOKED ROWS ARE SKIPPED: revoking one again changes
		// nothing and every rotation leaves another behind, so a run
		// without this issues one more pointless request than the run
		// before it, for ever.
		if token.ID == keep || token.Revoked || token.Name != TokenName(seat.Handle) {
			continue
		}
		if err := opts.Client.RevokeToken(ctx, token.ID); err != nil {
			return retired, fmt.Errorf("revoke the previous token: %w", err)
		}
		retired++
	}
	return retired, nil
}

// expiry is the date a minted token dies, or the zero time for the
// instance default.
//
// NO EXPIRY BY DEFAULT, deliberately: nothing in Crewlet renews a
// credential on a schedule, so a lifetime nobody renews is an outage with a
// date on it. GitLab.com caps personal access tokens at a year regardless,
// which is the instance enforcing its own policy rather than this tool
// choosing one — and a company whose policy needs a shorter window sets
// -token-expiry-days and owns the re-run.
func expiry(opts Options) time.Time {
	if opts.ExpiryDays == nil || *opts.ExpiryDays <= 0 {
		return time.Time{}
	}
	return clock(opts).AddDate(0, 0, *opts.ExpiryDays)
}

func clock(opts Options) time.Time {
	if opts.Now != nil {
		return opts.Now()
	}
	return time.Now()
}

// ensureAccount finds or creates a seat's service account.
func ensureAccount(ctx context.Context, c *Client, groupID int,
	p *config.GitLabProvisioning, seat provision.Seat,
) (User, bool, error) {
	username := Username(p, seat.Handle)
	user, found, err := c.UserByUsername(ctx, username)
	if err != nil {
		return User{}, false, err
	}
	if found {
		return user, false, nil
	}
	user, err = c.CreateServiceAccount(ctx, groupID, seat.Role, username, seat.Email)
	if err != nil {
		return User{}, false, err
	}
	return user, true, nil
}

// decommission deletes the managed accounts whose seats have left.
//
// # The prefix and the group are both the safety property
//
// "Managed" means an account whose username starts with the configured
// prefix AND which is a member of the group this company provisions into.
// Either alone is too broad: a prefix matches whatever an operator chose to
// call things elsewhere on the instance, and a group holds people. The
// prefix can never be empty — [Username] defaults one — and that default is
// what stops an unscoped sweep.
//
// A group member the instance refuses to delete because it is not a service
// account is left alone and reported: that refusal is GitLab catching what
// this scan should not have proposed, so it is a signal about the prefix
// rather than an error to abort on.
func decommission(ctx context.Context, opts Options, groupID int) ([]string, []string, error) {
	p := opts.Config.Provisioning
	prefix := strings.ToLower(Username(p, ""))
	members, err := opts.Client.GroupMembers(ctx, groupID)
	if err != nil {
		return nil, nil, fmt.Errorf("gitlab: list group members: %w", err)
	}
	keep := make(map[string]bool, len(opts.Plan.Seats))
	for _, seat := range opts.Plan.Seats {
		keep[strings.ToLower(Username(p, seat.Handle))] = true
	}
	var removed, notes []string
	for _, member := range members {
		username := strings.ToLower(member.Username)
		if !strings.HasPrefix(username, prefix) || keep[username] {
			continue
		}
		if err := opts.Client.DeleteServiceAccount(ctx, groupID, member.ID); err != nil {
			var api *APIError
			if errors.As(err, &api) && api.Status == http.StatusBadRequest {
				notes = append(notes, fmt.Sprintf(
					"%s matches the managed prefix but the instance refuses "+
						"to delete it, which is what it does for an account "+
						"that is not a service account — check that "+
						"provisioning.username_prefix is not catching people",
					member.Username))
				continue
			}
			return removed, notes, fmt.Errorf("gitlab: decommission %s: %w",
				member.Username, err)
		}
		removed = append(removed, member.Username)
	}
	return removed, notes, nil
}

// signingSecret is what the hook is registered with, minting one when the
// config's reference answered nothing.
//
// # Why an empty one cannot just be registered
//
// GitLab's signing token is CALLER-SUPPLIED and write-only: the instance
// never returns it. A hook registered with an empty one is accepted, shows
// as healthy in the settings page, and signs every delivery with nothing —
// which the engine then refuses. Measured against a real instance: the
// issue was created, the hook fired, and the only trace was one
// `webhook_signature_invalid` line in a log nobody was watching.
//
// So an empty resolution is not passed through. It is either minted — into
// the variable the config's ${VAR} names, the same mint-into-${VAR} contract
// the seat tokens follow — or refused, because a literal has nowhere to
// record it and half-configuring is the failure above.
func signingSecret(ctx context.Context, opts Options) (string, string, bool, error) {
	plan := PlanSigningSecret(opts.SigningSecret, opts.SigningSecretVar, opts.Rotate, true)
	switch plan.Action {
	case SigningReuse:
		return strings.TrimSpace(opts.SigningSecret), plan.Note, false, nil
	case SigningBlocked:
		return "", "", false, errors.New("gitlab: " + plan.Note)
	}
	secret, err := whsec.Mint()
	if err != nil {
		return "", "", false, err
	}
	if err := opts.Sink.Record(ctx, plan.Var, secret); err != nil {
		return "", "", false, fmt.Errorf("gitlab: record %s: %w", plan.Var, err)
	}
	return secret, plan.Note + " — " + opts.Sink.NextStep(), true, nil
}

// SigningAction is what a run will do about the webhook signing secret.
type SigningAction string

// The outcomes, named so the plan and the run cannot describe them
// differently.
const (
	// SigningUntouched: no hook is being registered, so the secret is not
	// this run's business.
	SigningUntouched SigningAction = "untouched"
	// SigningReuse: a usable secret already resolved.
	SigningReuse SigningAction = "reuse"
	// SigningMint: none resolved, and the config names a ${VAR} to put one in.
	SigningMint SigningAction = "mint"
	// SigningRotate: one resolved and -rotate was passed, so it is replaced.
	SigningRotate SigningAction = "rotate"
	// SigningBlocked: none resolved and nowhere to record one.
	SigningBlocked SigningAction = "blocked"
)

// SigningPlan is what a run intends to do about the webhook signing secret,
// decided BEFORE anything is touched so `-dry-run` can state it.
//
// The plan and the run read the same function, which is the rule the seat
// plan already follows: a dry run that re-derived this separately would be a
// second implementation, free to disagree with the real one about the most
// consequential thing a run can do — replacing the key a working hook signs
// with, which fails every delivery in flight until the new value reaches the
// engine.
type SigningPlan struct {
	Action SigningAction
	// Var is the ${VAR} a minted or rotated secret is recorded into.
	Var string
	// Note is the caveat this outcome carries, or empty.
	Note string
}

// PlanSigningSecret decides what a run will do about the signing secret.
//
// secret is the RESOLVED value of integrations.gitlab.signing_secret, varName
// the whole ${VAR} it references (empty when it is a literal), and
// registeringHooks whether this run has a public URL to point a hook at.
func PlanSigningSecret(secret, varName string, rotate, registeringHooks bool) SigningPlan {
	if !registeringHooks {
		return SigningPlan{Action: SigningUntouched}
	}
	// -rotate REACHES THE SIGNING SECRET, not just the seat tokens.
	//
	// It has to. Until the provisioning fix, the minted key went into
	// GitLab's plaintext `token` attribute, so the instance echoed it back
	// in cleartext on every delivery — into request logs, into any proxy in
	// front of the engine, and into the stored delivery headers. Every key
	// installed by an older Crewlet is therefore compromised, and a
	// provisioner that could not replace one would leave the operator
	// editing environment variables by hand to recover.
	//
	// Deliberately gated on the flag rather than done on every run: minting
	// a signing secret every time would re-point the hook at a key the
	// RUNNING engine does not have yet, refusing every delivery until the
	// new value is sourced and the process restarted — the same outage the
	// seat tokens' Rotate doc explains at length.
	rotating := rotate && varName != ""
	if trimmed := strings.TrimSpace(secret); trimmed != "" && !rotating {
		if rotate {
			// REPORTED, NOT REFUSED. -rotate is about the seat tokens, and
			// failing the whole run would stop an operator rotating those
			// because of a signing secret they manage by hand. They do
			// need to hear it, though — so it is a note on a run that
			// otherwise succeeded rather than silence.
			return SigningPlan{Action: SigningReuse, Note: "the webhook signing " +
				"secret was left alone: integrations.gitlab.signing_secret is a " +
				"literal, so there is nowhere to record a new one. Replace it by " +
				"hand and re-run — a key installed before the signing_token fix " +
				"was echoed back in cleartext on every delivery, so it is " +
				"compromised"}
		}
		return SigningPlan{Action: SigningReuse}
	}
	if varName == "" {
		return SigningPlan{Action: SigningBlocked, Note: "integrations.gitlab." +
			"signing_secret resolved to nothing and is not a whole ${VAR} " +
			"reference, so there is nowhere to record a minted one. Point it at " +
			"a variable, export a whsec_ value yourself, or drop -public-url and " +
			"register the hook by hand"}
	}
	action, what := SigningMint, "minted"
	if rotating {
		action, what = SigningRotate, "rotated"
	}
	return SigningPlan{Action: action, Var: varName, Note: fmt.Sprintf(
		"%s a webhook signing secret into %s", what, varName)}
}

// Describe renders the plan as the line a run prints before it acts.
func (p SigningPlan) Describe() string {
	switch p.Action {
	case SigningUntouched:
		return "webhook signing secret: untouched (no -public-url, so no hook is registered)"
	case SigningReuse:
		return "webhook signing secret: reused — the configured one already resolves"
	case SigningMint:
		return "webhook signing secret: WILL BE MINTED into " + p.Var
	case SigningRotate:
		return "webhook signing secret: WILL BE ROTATED into " + p.Var +
			" — the hook stops verifying against the old key the moment this runs"
	case SigningBlocked:
		return "webhook signing secret: THIS RUN WILL FAIL — " + p.Note
	}
	return ""
}

// SigningSecretPrefix is what GitLab's Standard-Webhooks implementation
// expects a signing token to start with.
const SigningSecretPrefix = whsec.Prefix

// MintSigningSecret generates a Standard-Webhooks signing secret.
//
// The FORMAT lives in [whsec], with the three readers that must agree on it.
// This is the vendor-facing name for it, because a caller here is asking
// GitLab a question and should not have to know which spec answers it.
func MintSigningSecret() (string, error) { return whsec.Mint() }

// ensureHooks registers this deployment's webhook, at ONE level.
//
// # One level, never both
//
// A group hook already fires for every issue, merge request, note and
// pipeline event in every project of the group and its subgroups. GitLab is
// explicit that a group hook and a project hook subscribed to the same
// events BOTH fire for an in-project event — double delivery, which the
// engine's ledger deduplicates and its inbox does not. So this picks a
// level and registers there.
//
// # Why there is a choice at all
//
// THE GROUP HOOKS API IS NOT EVERYWHERE. It is a Premium feature on
// gitlab.com and it does not exist in Community Edition at all, and GitLab
// hides an unavailable endpoint as a 404 rather than a 402 — so "not found"
// is what an instance says about a feature it does not serve. Registering
// only at the group level therefore failed the whole reconcile there, and
// failed it AFTER minting, so the rollback revoked every credential the run
// had just created.
//
// MEASURED, because the obvious guess is wrong in both directions: the
// unlicensed `gitlab-ee` image this repository's compose stack runs (19.3.0,
// no license) DOES serve `GET /groups/:id/hooks`, so the local loop takes
// the group path and never exercises the fallback. Reach for `false` to
// exercise it, and do not assume "unlicensed" means "no group hooks".
//
// Modes come from provisioning.group_webhook — auto (default) tries the
// group and falls back, true demands the group, false goes straight to the
// projects.
func ensureHooks(ctx context.Context, opts Options, groupID int, projects []string, target string) ([]string, []string, error) {
	mode := config.GroupWebhookAuto
	if pv := opts.Config.Provisioning; pv != nil && pv.GroupWebhook != "" {
		mode = pv.GroupWebhook
	}
	secret := opts.SigningSecret

	if mode != config.GroupWebhookNever {
		err := ensureGroupHook(ctx, opts.Client, groupID, target, secret)
		switch {
		case err == nil:
			return []string{"group"}, nil, nil
		case mode == config.GroupWebhookRequire:
			return nil, nil, fmt.Errorf(
				"%w\n\ngroup_webhook is \"true\", so no per-project fallback was "+
					"tried. Group webhooks are a GitLab Premium feature: on Free "+
					"the endpoint answers 404. Set group_webhook: false (or auto) "+
					"to register one hook per provisioning.projects entry", err)
		case !gatedByTier(err):
			return nil, nil, err
		}
		// The tier gate, on auto. Fall through to the projects, and say
		// so — an operator who expected one group hook and got four
		// project hooks should learn it here rather than from the
		// instance's settings pages.
		hooked, err := ensureProjectHooks(ctx, opts.Client, projects, target, secret)
		if err != nil {
			return nil, nil, err
		}
		return hooked, []string{
			"this instance does not serve the group webhooks API (it is a " +
				"GitLab Premium feature), so one hook was registered per " +
				"project instead of one for the group; a project added to the " +
				"group later will NOT be covered until this runs again",
		}, nil
	}

	hooked, err := ensureProjectHooks(ctx, opts.Client, projects, target, secret)
	return hooked, nil, err
}

// resolveProjects keeps the declared projects this instance actually has.
//
// # A missing one is dropped, not fatal
//
// `provisioning.projects` names a company's real repositories, and one of
// them being renamed, moved or not created yet is an ordinary state of a
// config — not a reason to refuse to provision the other nine. Aborting on
// the first 404 did exactly that, and it aborted MID-LOOP, after minting, so
// the rollback then revoked the credentials the run had already created.
//
// It runs BEFORE any mutation for the same reason the group is resolved up
// front: a check that happens halfway through leaves half a reconcile
// behind, and the operator's fix — create the project, re-run — is what the
// note tells them to do.
//
// It is the opposite call from the Plane importer, deliberately. There a
// missing project aborts before a single page is written, because half an
// import looks like a complete knowledge base with holes in it. Memberships
// and hooks are additive and independent: the seats that could be added
// were added, and re-running adds the rest.
func resolveProjects(ctx context.Context, c *Client, declared []string) ([]string, []string, error) {
	kept := make([]string, 0, len(declared))
	var missing []string
	for _, project := range declared {
		exists, err := c.ProjectExists(ctx, project)
		if err != nil {
			return nil, nil, fmt.Errorf("gitlab: check project %s: %w", project, err)
		}
		if !exists {
			missing = append(missing, project)
			continue
		}
		kept = append(kept, project)
	}
	if len(missing) == 0 {
		return kept, nil, nil
	}
	return kept, []string{fmt.Sprintf(
		"these provisioning.projects are not on this instance and were "+
			"skipped: %s — create them (or drop them from the config) and "+
			"re-run; everything else reconciled",
		strings.Join(missing, ", "))}, nil
}

// ensureGroupHook registers the one group webhook, or re-points it.
//
// MATCHED ON THE URL, because that is what identifies "our" hook: an
// instance may carry hooks somebody else registered, and a run that replaced
// the first one it found would take down an unrelated integration.
func ensureGroupHook(ctx context.Context, c *Client, groupID int, target, secret string) error {
	hooks, err := c.GroupHooks(ctx, groupID)
	if err != nil {
		return fmt.Errorf("gitlab: list group hooks: %w", err)
	}
	for _, hook := range hooks {
		if hook.URL == target {
			// UPDATED RATHER THAN SKIPPED: the signing secret may have
			// rotated, and a hook still carrying the old one delivers
			// events the engine then refuses.
			if err := c.UpdateGroupHook(ctx, groupID, hook.ID, target, secret); err != nil {
				return fmt.Errorf("gitlab: update group hook: %w", err)
			}
			return confirmSigned(ctx, "group hook", func() ([]Hook, error) {
				return c.GroupHooks(ctx, groupID)
			}, target)
		}
	}
	if _, err := c.CreateGroupHook(ctx, groupID, target, secret); err != nil {
		return fmt.Errorf("gitlab: create group hook: %w", err)
	}
	return confirmSigned(ctx, "group hook", func() ([]Hook, error) {
		return c.GroupHooks(ctx, groupID)
	}, target)
}

// confirmSigned reads the hook back and refuses to call the run a success
// unless GitLab says it now holds a signing token.
//
// THE WRITE SUCCEEDING PROVES NOTHING. `signing_token` arrived in GitLab
// 19.0 and went generally available in 19.1; an older instance takes the
// attribute, ignores it, and answers 200. The hook then exists, GitLab's
// settings page calls it healthy, and it delivers unsigned to an engine
// whose verification is mandatory — so every delivery is refused, and the
// only place that could have said why is this function.
//
// `signing_token_present` is the only thing GitLab will say about it: the
// token itself is never returned. That is enough, because what is being
// confirmed is that a signing token EXISTS, not which one.
func confirmSigned(ctx context.Context, what string, list func() ([]Hook, error), target string) error {
	hooks, err := list()
	if err != nil {
		return fmt.Errorf("gitlab: re-read the %s to confirm it can sign: %w", what, err)
	}
	for _, hook := range hooks {
		if hook.URL != target {
			continue
		}
		if !hook.SigningTokenPresent {
			return fmt.Errorf(
				"gitlab: the %s at %s reports no signing token, so this "+
					"instance would deliver unsigned and the engine refuses "+
					"unsigned deliveries. Signing tokens need GitLab 19.1 or "+
					"newer (19.0 behind the webhook_signing_token flag)",
				what, target)
		}
		return nil
	}
	return fmt.Errorf("gitlab: the %s at %s is not there after writing it", what, target)
}

// ensureProjectHooks registers one hook per declared project.
//
// It refuses an empty list rather than registering nothing: a run that
// quietly hooked no project leaves the instance reporting a healthy
// integration that delivers to nobody, which is the exact failure the
// skipped-rather-than-guessed rule above exists to prevent.
func ensureProjectHooks(ctx context.Context, c *Client, projects []string, target, secret string) ([]string, error) {
	if len(projects) == 0 {
		return nil, errors.New(
			"gitlab: per-project webhooks need provisioning.projects, and none " +
				"are declared — either list the projects to hook, or use a " +
				"Premium instance where one group hook covers them all")
	}
	hooked := make([]string, 0, len(projects))
	for _, project := range projects {
		if err := ensureProjectHook(ctx, c, project, target, secret); err != nil {
			return nil, err
		}
		hooked = append(hooked, project)
	}
	return hooked, nil
}

func ensureProjectHook(ctx context.Context, c *Client, project, target, secret string) error {
	hooks, err := c.ProjectHooks(ctx, project)
	if err != nil {
		return fmt.Errorf("gitlab: list hooks on %s: %w", project, err)
	}
	for _, hook := range hooks {
		if hook.URL == target {
			if err := c.UpdateProjectHook(ctx, project, hook.ID, target, secret); err != nil {
				return fmt.Errorf("gitlab: update hook on %s: %w", project, err)
			}
			return confirmSigned(ctx, "hook on "+project, func() ([]Hook, error) {
				return c.ProjectHooks(ctx, project)
			}, target)
		}
	}
	if _, err := c.CreateProjectHook(ctx, project, target, secret); err != nil {
		return fmt.Errorf("gitlab: create hook on %s: %w", project, err)
	}
	return confirmSigned(ctx, "hook on "+project, func() ([]Hook, error) {
		return c.ProjectHooks(ctx, project)
	}, target)
}

// gatedByTier reports whether a failure is GitLab withholding a licensed
// endpoint rather than refusing this request.
//
// 404 is the one that matters and the one that reads wrong: GitLab hides an
// unavailable endpoint rather than answering 402, so "not found" is what an
// instance says about a feature its tier does not serve. 403 is the same
// answer from one that surfaces the endpoint and refuses the call. Anything
// else —
// a 401, a 5xx, a transport failure — is a real problem, and falling back on
// it would paper over a broken credential with four project hooks.
func gatedByTier(err error) bool {
	switch Status(err) {
	case http.StatusNotFound, http.StatusForbidden:
		return true
	}
	return false
}

// rollback revokes what this run minted and clears what it recorded.
//
// It returns the ORIGINAL failure with the cleanup's own problems appended,
// never in place of it: the reason the run stopped is what an operator has
// to fix, and a cleanup error that replaced it would hide the cause behind
// its consequence.
func rollback(ctx context.Context, opts Options, minted map[string]mintedToken, cause error) error {
	if len(minted) == 0 {
		return cause
	}
	// DETACHED, because the failure may BE a cancelled context — and a
	// rollback that inherits it does nothing at all, leaving every minted
	// credential live.
	ctx = context.WithoutCancel(ctx)
	var problems []string
	for handle, m := range minted {
		var err error
		if m.createdAccount {
			// Nothing else has ever minted on an account this run made,
			// so taking everything takes exactly what this run caused.
			err = opts.Client.RevokeTokens(ctx, m.userID)
		} else {
			err = opts.Client.RevokeToken(ctx, m.tokenID)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", handle, err))
		}
	}
	if err := opts.Sink.Discard(ctx); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) == 0 {
		return fmt.Errorf("%w (the %d token(s) this run minted were revoked)",
			cause, len(minted))
	}
	return fmt.Errorf("%w\n\nAND THE CLEANUP DID NOT FINISH — these credentials "+
		"may still be live and must be revoked by hand:\n  %s",
		cause, strings.Join(problems, "\n  "))
}

// webhookTarget is the endpoint GitLab delivers to.
func webhookTarget(base string) string {
	if base = strings.TrimRight(strings.TrimSpace(base), "/"); base == "" {
		return ""
	}
	return base + "/webhooks/gitlab"
}

// accessLevel is a seat's membership level, as GitLab's numbers.
func accessLevel(p *config.GitLabProvisioning, handle string) int {
	level := p.AccessLevel
	if override, ok := p.AccessLevels[handle]; ok {
		level = override
	}
	if level == config.GitLabMaintainer {
		return gitlabMaintainer
	}
	return gitlabDeveloper
}

// GitLab's access levels, which the API takes as integers.
const (
	gitlabDeveloper  = 30
	gitlabMaintainer = 40
)

// tokenScopes are the scopes minted on a seat's token.
func tokenScopes(p *config.GitLabProvisioning) []string {
	if len(p.TokenScopes) > 0 {
		return p.TokenScopes
	}
	// `api` is what an agent needs to comment, review and push through the
	// MCP surface. It is broad, which is why it is a config field: a
	// company that runs read-only agents narrows it there, and one that
	// says nothing gets the scope its agents will actually use rather than
	// a run that succeeds and produces tokens nothing can act with.
	return []string{"api"}
}

func notesOf(p *provision.Plan) []string {
	if p == nil {
		return nil
	}
	return p.Notes
}
