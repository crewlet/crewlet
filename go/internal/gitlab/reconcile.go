package gitlab

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
	minted := map[string]int{}

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

		token, err := opts.Client.CreateToken(ctx, user.ID,
			"crewlet-"+seat.Handle, tokenScopes(p))
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("gitlab: %s: mint token: %w", seat.Handle, err))
		}
		minted[seat.Handle] = user.ID
		// RECORDED IMMEDIATELY. The value above is the only copy there
		// will ever be.
		if err := opts.Sink.Record(ctx, seat.TokenVar, token); err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("gitlab: %s: record %s: %w", seat.Handle, seat.TokenVar, err))
		}
		res.Rotated = append(res.Rotated, seat.Handle)
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
func rollback(ctx context.Context, opts Options, minted map[string]int, cause error) error {
	if len(minted) == 0 {
		return cause
	}
	// DETACHED, because the failure may BE a cancelled context — and a
	// rollback that inherits it does nothing at all, leaving every minted
	// credential live.
	ctx = context.WithoutCancel(ctx)
	var problems []string
	for handle, userID := range minted {
		if err := opts.Client.RevokeTokens(ctx, userID); err != nil {
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
