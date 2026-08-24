package mattermost

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// Result is what one reconcile did, for the report.
type Result struct {
	Created []string
	Rotated []string
	// Joined maps a seat to the channels it was added to, which is the
	// part an operator most needs to see: a bot hears only what it has
	// joined, so a seat with an empty list is one that will never wake.
	Joined map[string][]string
	Notes  []string
}

// Options are one reconcile's inputs.
type Options struct {
	// Client talks to the instance as a system administrator.
	Client *Client

	// Config is the company's mattermost block.
	Config *config.Mattermost

	// Org is the company, for the per-seat channel each role names.
	Org *org.Organization

	Plan *provision.Plan
	Sink provision.TokenSink
}

// Reconcile runs one pass.
//
// Like every provisioner here it MINTS EVERY TIME: an access token's value
// is returned once, so there is no already-correct state to detect, and
// pretending otherwise would leave an operator believing a token they cannot
// read is still the one in their config.
func Reconcile(ctx context.Context, opts Options) (*Result, error) {
	if opts.Client == nil {
		return nil, errors.New("mattermost: no client")
	}
	if opts.Sink == nil {
		return nil, provision.ErrNoSink
	}
	if opts.Plan == nil || opts.Plan.Empty() {
		return &Result{Notes: notesOf(opts.Plan)}, nil
	}

	team, found, err := opts.Client.TeamByName(ctx, opts.Config.Team)
	if err != nil {
		return nil, fmt.Errorf("mattermost: resolve team %q: %w", opts.Config.Team, err)
	}
	if !found {
		return nil, fmt.Errorf(
			"mattermost: no team %q on this instance — channels are "+
				"team-scoped, so the bots have nowhere to be placed",
			opts.Config.Team)
	}

	// THE NOTES ARE COLLECTED AT THE END, not copied here: the run adds
	// its own — a channel that does not exist, a bot that joined nothing
	// — and a snapshot taken now would silently drop every one of them.
	res := &Result{Joined: map[string][]string{}}
	minted := map[string]string{}

	for _, seat := range opts.Plan.Seats {
		username := BotUsername(opts.Config.Provisioning, seat.Handle)
		user, exists, err := opts.Client.BotByUsername(ctx, username)
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("mattermost: %s: %w", seat.Handle, err))
		}
		if !exists {
			bot, err := opts.Client.CreateBot(ctx, username,
				BotDisplayName(opts.Config.Provisioning, seat.Role))
			if err != nil {
				return nil, rollback(ctx, opts, minted,
					fmt.Errorf("mattermost: %s: create bot: %w", seat.Handle, err))
			}
			user = User{ID: bot.UserID, Username: bot.Username}
			res.Created = append(res.Created, seat.Handle)
		}

		if err := opts.Client.AddTeamMember(ctx, team.ID, user.ID); err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("mattermost: %s: join team: %w", seat.Handle, err))
		}
		joined, err := joinChannels(ctx, opts, team.ID, user.ID, seat.Handle)
		if err != nil {
			return nil, rollback(ctx, opts, minted, err)
		}
		res.Joined[seat.Handle] = joined
		if len(joined) == 0 {
			// SAID OUT LOUD. A bot receives only what its channels
			// deliver, so one in no channel is an agent that never wakes
			// — and nothing about the account looks wrong.
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s joined no channel, so it will never receive a message: "+
					"name channels under integrations.mattermost.provisioning "+
					"or on the seat itself", seat.Handle))
		}

		token, err := opts.Client.CreateAccessToken(ctx, user.ID, "crewlet-"+seat.Handle)
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("mattermost: %s: mint token: %w", seat.Handle, err))
		}
		minted[seat.Handle] = user.ID
		if err := opts.Sink.Record(ctx, seat.TokenVar, token); err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("mattermost: %s: record %s: %w",
					seat.Handle, seat.TokenVar, err))
		}
		res.Rotated = append(res.Rotated, seat.Handle)
	}

	if err := opts.Sink.Flush(ctx); err != nil {
		return nil, rollback(ctx, opts, minted, err)
	}
	res.Notes = append(notesOf(opts.Plan), res.Notes...)
	return res, nil
}

// joinChannels adds a bot to the company-wide channels plus its own.
//
// A CHANNEL THAT DOES NOT EXIST IS A NOTE, NOT A FAILURE. Half a fleet of
// bots joined and the run aborted is a worse state than every bot joined to
// the channels that do exist and a line saying which did not — especially
// since the usual cause is a typo an operator fixes in seconds.
func joinChannels(ctx context.Context, opts Options, teamID, userID, handle string) ([]string, error) {
	wanted := map[string]struct{}{}
	if p := opts.Config.Provisioning; p != nil {
		for _, name := range p.Channels {
			wanted[strings.TrimSpace(name)] = struct{}{}
		}
	}
	if seat := opts.Org.AgentSeatByHandle(handle); seat != nil {
		if own := strings.TrimSpace(seat.Mattermost.Channel); own != "" {
			wanted[own] = struct{}{}
		}
	}
	delete(wanted, "")

	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	sort.Strings(names)

	var joined []string
	for _, name := range names {
		channel, err := opts.Client.ChannelByName(ctx, teamID, name)
		if err != nil {
			if isStatus(err, 404) {
				opts.noteMissingChannel(handle, name)
				continue
			}
			return nil, fmt.Errorf("mattermost: %s: resolve channel %q: %w",
				handle, name, err)
		}
		if err := opts.Client.AddChannelMember(ctx, channel.ID, userID); err != nil {
			return nil, fmt.Errorf("mattermost: %s: join %q: %w", handle, name, err)
		}
		joined = append(joined, name)
	}
	return joined, nil
}

// noteMissingChannel records a channel that is not there.
//
// On the PLAN rather than the result, because the plan is what both the dry
// run and the real run print — so the same missing channel is reported the
// same way whether or not anything was touched.
func (o Options) noteMissingChannel(handle, name string) {
	o.Plan.Note("%s: no channel %q in team %q, so it was not joined — check "+
		"the slug (the URL name, not the display name)", handle, name, o.Config.Team)
}

// rollback revokes what this run minted and clears what it recorded.
func rollback(ctx context.Context, opts Options, minted map[string]string, cause error) error {
	if len(minted) == 0 {
		return cause
	}
	// DETACHED: the failure is often a cancelled context, and a rollback
	// that inherited it would do nothing while every minted token stays
	// live.
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

func notesOf(p *provision.Plan) []string {
	if p == nil {
		return nil
	}
	return p.Notes
}
