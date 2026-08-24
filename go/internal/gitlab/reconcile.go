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
	// Notes carries the plan's notes plus anything the run itself found.
	Notes []string
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

	// SigningSecret is the value the hook is registered with. It is read
	// from the resolved config rather than minted, because the engine has
	// to verify with the same one.
	SigningSecret string

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
		for _, project := range p.Projects {
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
		if err := ensureHook(ctx, opts.Client, group.ID, target, opts.SigningSecret); err != nil {
			return nil, rollback(ctx, opts, minted, err)
		}
		res.Hooked = target
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

// ensureHook registers the group webhook, or re-points the existing one.
//
// MATCHED ON THE URL, because that is what identifies "our" hook: an
// instance may carry hooks somebody else registered, and a run that replaced
// the first one it found would take down an unrelated integration.
func ensureHook(ctx context.Context, c *Client, groupID int, target, secret string) error {
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
			return nil
		}
	}
	if _, err := c.CreateGroupHook(ctx, groupID, target, secret); err != nil {
		return fmt.Errorf("gitlab: create group hook: %w", err)
	}
	return nil
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
