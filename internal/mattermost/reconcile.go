package mattermost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// Result is what one reconcile did, for the report.
type Result struct {
	// Decommissioned names the bots this run disabled.
	Decommissioned []string

	Created []string
	// Renamed names the bots whose display name was brought back in line
	// with the company document.
	Renamed []string
	Rotated []string
	// Kept names the seats whose existing token was left alone — the
	// SUCCESSFUL outcome of a re-run, said out loud because a silent
	// report reads as a run that did nothing.
	Kept []string
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

	// Rotate forces a fresh token for every bot, including bots whose
	// current one still works.
	//
	// # Why it is a flag rather than what a run does
	//
	// Mattermost returns an access token's value once, so a provisioner
	// cannot check that what it recorded last time still matches. The
	// tempting answer is to mint every run — and that is an outage: the
	// engine is running with the OLD value, and rotating revokes the
	// credential every bot's websocket is currently authenticated with.
	// An operator adding a tenth seat would take the other nine down,
	// from a command whose whole promise is that it is safe to re-run.
	Rotate bool

	// Decommission disables managed bot accounts whose seats have left the
	// company document, matching the flag GitLab already has.
	//
	// DISABLE, never delete: a deleted bot takes its posts with it, so this
	// would silently rewrite the history of every channel the seat spoke
	// in. A disabled bot keeps what it said and can say nothing more.
	Decommission bool

	// Only narrows the PROVISIONING loop to these handles. Empty does every
	// seat.
	//
	// It deliberately does NOT narrow what Decommission keeps. Filtering the
	// plan itself would be the obvious implementation and it is a footgun:
	// `-handles ceo -decommission` would read every other seat as departed
	// and disable the whole company. The keep-set is always the full plan.
	Only []string
}

// Reconcile runs one pass.
//
// A RE-RUN IS SAFE AND QUIET: a bot that exists is found rather than
// re-created, a channel it is in is left alone, and a token that still
// works is not replaced. See [Options.Rotate] for why the last of those is
// the default rather than the flag.
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

	// THE PREFLIGHT, before a single write.
	//
	// Every failure it catches otherwise surfaces as a 403 on the FIRST bot
	// creation — after the team lookup has succeeded, so the run looks like
	// it was working — and the message names an endpoint rather than the
	// setting an administrator has to change. Checked, not assumed, because
	// all three are invisible from the config.
	preflight := checkPreflight(ctx, opts)

	// THE NOTES ARE COLLECTED AT THE END, not copied here: the run adds
	// its own — a channel that does not exist, a bot that joined nothing
	// — and a snapshot taken now would silently drop every one of them.
	res := &Result{Joined: map[string][]string{}}
	minted := map[string]mintedToken{}

	// ONE bot listing per run, shared by the rename check and the
	// decommission pass. The display name lives on the BOT record and not
	// on its user, so without this the comparison has nothing true to read
	// — and per-seat fetches would be a call each for a value one list
	// already carries.
	managed := map[string]Bot{}
	if bots, err := opts.Client.Bots(ctx); err != nil {
		res.Notes = append(res.Notes, fmt.Sprintf(
			"could not list bots, so display names are not checked this run: %v", err))
	} else {
		for _, bot := range bots {
			managed[strings.ToLower(bot.Username)] = bot
		}
	}

	only := map[string]bool{}
	for _, handle := range opts.Only {
		only[handle] = true
	}
	for _, seat := range opts.Plan.Seats {
		if len(only) > 0 && !only[seat.Handle] {
			continue
		}
		username := BotUsername(opts.Config.Provisioning, seat.Handle)
		user, exists, err := opts.Client.BotByUsername(ctx, username)
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("mattermost: %s: %w", seat.Handle, err))
		}
		if !exists {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			bot, err := opts.Client.CreateBot(ctx, username,
				BotDisplayName(opts.Config.Provisioning, seat.Role))
			if err != nil {
				return nil, rollback(ctx, opts, minted,
					fmt.Errorf("mattermost: %s: create bot: %w", seat.Handle, err))
			}
			user = User{ID: bot.UserID, Username: bot.Username}
			res.Created = append(res.Created, seat.Handle)
		} else if want := BotDisplayName(opts.Config.Provisioning, seat.Role); want != "" &&
			managed[strings.ToLower(username)].DisplayName != want {
			// A RENAMED ROLE REACHES THE BOT. Provisioning is a reconcile,
			// and a create-only display name means the roster in Mattermost
			// drifts from the org chart it mirrors, with no way back but
			// editing every bot by hand.
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := opts.Client.PatchBot(ctx, user.ID, want); err != nil {
				res.Notes = append(res.Notes, fmt.Sprintf(
					"%s: could not update the bot's display name to %q: %v",
					seat.Handle, want, err))
			} else {
				res.Renamed = append(res.Renamed, seat.Handle)
			}
		}

		if err = opts.Client.AddTeamMember(ctx, team.ID, user.ID); err != nil {
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

		created := slices.Contains(res.Created, seat.Handle)
		verdict, held := provision.VerdictRejected, true
		if !created && !opts.Rotate {
			verdict, held, err = credentialFor(ctx, opts, user.ID, seat)
			if err != nil {
				return nil, rollback(ctx, opts, minted,
					fmt.Errorf("mattermost: %s: %w", seat.Handle, err))
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
				"mattermost: %s: %s holds a token that authenticates as a "+
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

		token, err := opts.Client.CreateAccessToken(ctx, user.ID, TokenDescription(seat.Handle))
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("mattermost: %s: mint token: %w", seat.Handle, err))
		}
		minted[seat.Handle] = mintedToken{
			userID: user.ID, tokenID: token.ID, createdBot: created,
		}
		if err = opts.Sink.Record(ctx, seat.TokenVar, token.Value); err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("mattermost: %s: record %s: %w",
					seat.Handle, seat.TokenVar, err))
		}
		res.Rotated = append(res.Rotated, seat.Handle)
		if created {
			continue
		}
		// RETIRED AFTER THE RECORD, and only this tool's own: an
		// administrator may have minted a token on this bot by hand.
		retired, err := retirePrevious(ctx, opts, user.ID, seat, token.ID)
		if err != nil {
			return nil, rollback(ctx, opts, minted,
				fmt.Errorf("mattermost: %s: %w", seat.Handle, err))
		}
		if !held && retired > 0 {
			// THE SURPRISING CASE, and the only one that earns a note:
			// the operator asked for nothing, but the variable was empty
			// on this machine while a live token existed on the account.
			res.Notes = append(res.Notes, fmt.Sprintf(
				"%s: the bot held a working token but %s did not, so a fresh "+
					"one was minted and the old one retired — a running engine "+
					"holding the old value has to be restarted",
				seat.Handle, seat.TokenVar))
		}
	}

	if err := opts.Sink.Flush(ctx); err != nil {
		return nil, rollback(ctx, opts, minted, err)
	}

	// AFTER the flush, deliberately. Decommissioning touches accounts this
	// run did not mint for, so it must not be able to roll back credentials
	// that are already recorded and already in use.
	if opts.Decommission {
		disabled, notes := decommission(ctx, opts, managed)
		res.Decommissioned = disabled
		res.Notes = append(res.Notes, notes...)
	}

	res.Notes = append(notesOf(opts.Plan), append(preflight, res.Notes...)...)
	return res, nil
}

// checkPreflight reports what would make this run fail on its first write.
//
// NOTES, not errors, and the distinction is deliberate: the two settings are
// read from a config endpoint whose exact key set varies by server version,
// so an absent key means "this server did not say", not "it is off". Refusing
// on silence would make the provisioner unusable against a version this
// engine has not seen; warning loudly on an explicit false is the honest
// half.
//
// The admin role IS checked hard enough to be worth naming, because a token
// without it fails every single write.
func checkPreflight(ctx context.Context, opts Options) []string {
	var notes []string
	if me, err := opts.Client.Me(ctx); err != nil {
		notes = append(notes, fmt.Sprintf(
			"could not confirm the provisioning account: %v", err))
	} else if !me.SystemAdmin() {
		notes = append(notes, fmt.Sprintf(
			"%s is not a system administrator (roles: %s) — creating a bot, "+
				"minting an access token and adding a team member all require "+
				"it, so this run will fail on its first write",
			me.Username, orNone(me.Roles)))
	}

	cfg, err := opts.Client.ClientConfig(ctx)
	if err != nil {
		// Not worth a note of its own: the run is about to make real calls
		// that will report the same reachability problem with more context.
		return notes
	}
	for _, setting := range []struct{ key, why string }{
		{"EnableBotAccountCreation", "no bot account can be created"},
		{"EnableUserAccessTokens", "no bot can be given a token, so none of " +
			"them can connect"},
	} {
		if cfg[setting.key] == "false" {
			notes = append(notes, fmt.Sprintf(
				"ServiceSettings.%s is false on this instance, so %s — "+
					"an administrator has to enable it in the System Console",
				setting.key, setting.why))
		}
	}
	return notes
}

// decommission disables managed bots whose seats have left the config.
//
// Matched by the SAME username prefix the run mints under, so an account a
// person created by hand is never touched — the prefix is the only thing that
// makes a bot "managed", and widening the match would let this disable
// somebody's own integration.
func decommission(ctx context.Context, opts Options, managed map[string]Bot) ([]string, []string) {
	prefix := strings.ToLower(BotUsername(opts.Config.Provisioning, ""))
	if prefix == "" {
		// Without a prefix every bot on the instance is a candidate, which
		// is precisely the run an operator cannot undo.
		return nil, []string{"decommission skipped: this config has no bot " +
			"username prefix, so there is nothing that safely identifies a " +
			"managed account"}
	}
	keep := make(map[string]bool, len(opts.Plan.Seats))
	for _, seat := range opts.Plan.Seats {
		keep[strings.ToLower(BotUsername(opts.Config.Provisioning, seat.Handle))] = true
	}
	var disabled, notes []string
	for _, bot := range managed {
		username := strings.ToLower(bot.Username)
		if !strings.HasPrefix(username, prefix) || keep[username] {
			continue
		}
		if err := opts.Client.DisableBot(ctx, bot.UserID); err != nil {
			notes = append(notes, fmt.Sprintf(
				"%s matches the managed prefix and could not be disabled: %v",
				bot.Username, err))
			continue
		}
		disabled = append(disabled, bot.Username)
	}
	return disabled, notes
}

// orNone renders an empty role list readably.
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// mintedToken is one credential this run created, as its rollback needs it.
type mintedToken struct {
	userID  string
	tokenID string
	// createdBot says the bot is this run's, which decides HOW the
	// rollback undoes it: everything on an account nothing else ever
	// minted on, versus exactly the one token on one that already
	// existed.
	createdBot bool
}

// TokenDescription is the description this tool mints under.
//
// It is the ONLY thing distinguishing a token this tool owns from one an
// administrator created by hand, and both the keep-or-mint decision and the
// retire step key on it.
func TokenDescription(handle string) string { return "crewlet-" + handle }

// credentialFor decides whether this bot already has a working token.
//
// # It PROVES it, rather than inferring it
//
// The weaker test — "the variable has a value and the account has some
// token" — reads as provisioned in exactly the case that matters: an
// operator who restored an older env file has a stale value sitting beside
// a live token that is not it, and the bot then authenticates with nothing,
// on every run, forever. So the run takes the value the variable actually
// holds and asks the server who it is.
func credentialFor(ctx context.Context, opts Options, userID string, seat provision.Seat) (provision.Verdict, bool, error) {
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

// verify asks the server who a token authenticates as.
func (c *Client) verify(ctx context.Context, value, wantID string) provision.Verdict {
	probe, err := NewClient(ClientOptions{URL: c.base, Token: value, HTTP: c.http})
	if err != nil {
		return provision.VerdictRejected
	}
	var who User
	_, err = probe.request(ctx, http.MethodGet, "/users/me", nil, &who, true)
	switch {
	case err == nil && who.ID == wantID:
		return provision.VerdictSelf
	case err == nil:
		return provision.VerdictOther
	case isStatus(err, http.StatusUnauthorized), isStatus(err, http.StatusForbidden):
		return provision.VerdictRejected
	default:
		// A 5xx or a dropped connection. NOT a rejection: re-minting on
		// "cannot tell" destroys a token that works.
		return provision.VerdictUnknown
	}
}

// retirePrevious revokes this tool's earlier tokens on an existing bot.
func retirePrevious(ctx context.Context, opts Options, userID string, seat provision.Seat, keep string) (int, error) {
	tokens, err := opts.Client.Tokens(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("list tokens: %w", err)
	}
	retired := 0
	for _, token := range tokens {
		if token.ID == keep || token.Description != TokenDescription(seat.Handle) {
			continue
		}
		if err := opts.Client.RevokeToken(ctx, token.ID); err != nil {
			return retired, fmt.Errorf("revoke the previous token: %w", err)
		}
		retired++
	}
	return retired, nil
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
	slices.Sort(names)

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
func rollback(ctx context.Context, opts Options, minted map[string]mintedToken, cause error) error {
	if len(minted) == 0 {
		return cause
	}
	// DETACHED: the failure is often a cancelled context, and a rollback
	// that inherited it would do nothing while every minted token stays
	// live.
	ctx = context.WithoutCancel(ctx)
	var problems []string
	for handle, m := range minted {
		var err error
		if m.createdBot {
			// Nothing else has ever minted on a bot this run made, so
			// taking everything takes exactly what this run caused.
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

func notesOf(p *provision.Plan) []string {
	if p == nil {
		return nil
	}
	return p.Notes
}
