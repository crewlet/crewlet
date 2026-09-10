package mattermost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// The Mattermost half of provisioning: one bot account per agent seat, in
// the team, in the channels, with an access token minted into the variable
// the config points at.
//
// # No manifest, no OAuth, no ledger
//
// This is the GitLab shape rather than Slack's. An admin token
// mints a bot's access token directly, so there is no app to install, no
// browser round trip to broker, and nothing to remember between runs — the
// server IS the ledger, and a reconcile reads it.
//
// # A bot only hears what it has joined
//
// Mattermost delivers a channel's messages to its members. So joining the
// channels is not a convenience here: a bot that exists, authenticates and
// is in no channel is an agent that never wakes, and the failure is silent
// on both sides.

// PlanFor walks the org for seats that need a Mattermost bot.
func PlanFor(o *org.Organization, cfg *config.Mattermost) (*provision.Plan, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, errors.New("mattermost: the company config does not enable mattermost")
	}
	if strings.TrimSpace(cfg.Team) == "" {
		return nil, errors.New(
			"mattermost: integrations.mattermost.team is unset — channels are " +
				"team-scoped, so a bot cannot be placed without it")
	}
	plan := &provision.Plan{}
	for seat := range o.AllRoles() {
		if !seat.IsAgent() || seat.Mattermost.IsZero() {
			continue
		}
		handle := seat.Handle()
		name, ok := provision.SoleVar(seat.Mattermost.BotToken)
		if !ok {
			plan.Note("%s: mattermost.bot_token is %s rather than a whole "+
				"${VAR} reference, so there is nowhere to mint a token — "+
				"point it at a variable, or manage this bot by hand",
				handle, provision.Shape(seat.Mattermost.BotToken))
			continue
		}
		plan.Add(provision.Seat{
			Handle: handle, Role: seat.Name, TokenVar: name,
			Email: BotEmail(cfg.Provisioning, handle),
		})
	}
	return plan, nil
}

// BotUsername is a seat's bot username.
//
// LOWERCASED, because Mattermost usernames are and a mixed-case handle would
// be created as one thing and looked up as another on the next run — which
// reads as "the bot is missing" and creates a second.
func BotUsername(p *config.MattermostProvisioning, handle string) string {
	prefix := ""
	if p != nil {
		prefix = strings.TrimSpace(p.UsernamePrefix)
	}
	return strings.ToLower(prefix + handle)
}

// BotEmail is the address a bot account is created with.
func BotEmail(p *config.MattermostProvisioning, handle string) string {
	return BotUsername(p, handle) + "@noreply.crewlet.invalid"
}

// BotDisplayName is what a person sees beside the bot's posts.
func BotDisplayName(p *config.MattermostProvisioning, role string) string {
	suffix := ""
	if p != nil {
		suffix = p.DisplayNameSuffix
	}
	return role + suffix
}

// ---- the admin API ---------------------------------------------------- //

// Bot is a Mattermost bot account.
type Bot struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	// DisplayName is the bot's own, which lives on the BOT record rather
	// than on its user: the user's nickname is a different field the bots
	// API does not set, so comparing against it would report drift on every
	// run and rewrite a name that was already correct.
	DisplayName string `json:"display_name"`
}

// BotByUsername finds a bot's account, reporting whether it exists.
func (c *Client) BotByUsername(ctx context.Context, username string) (User, bool, error) {
	user, err := c.UserByUsername(ctx, username)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return User{}, false, nil
		}
		return User{}, false, err
	}
	return user, true, nil
}

// CreateBot creates a bot account.
//
// A BOT, not a user: it cannot sign in, holds no password, and is owned by
// the account that created it — which is what makes it removable when a seat
// is decommissioned without touching a person's account.
func (c *Client) CreateBot(ctx context.Context, username, displayName string) (Bot, error) {
	var out Bot
	_, err := c.request(ctx, http.MethodPost, "/bots", map[string]string{
		"username": username, "display_name": displayName,
	}, &out, false)
	return out, err
}

// CreateAccessToken mints a personal access token for an account.
//
// Its value is returned once and never again, which is why the sink is
// written through: between here and the record there is a window where the
// only copy of a live credential is in this process's memory.
func (c *Client) CreateAccessToken(ctx context.Context, userID, description string) (Token, error) {
	var out Token
	_, err := c.request(ctx, http.MethodPost, "/users/"+userID+"/tokens",
		map[string]string{"description": description}, &out, false)
	if err != nil {
		return Token{}, err
	}
	if out.Value == "" {
		return Token{}, fmt.Errorf("mattermost: the server minted a token for %s and "+
			"returned no value; it exists and cannot be recovered — revoke it "+
			"in the account's settings", userID)
	}
	return out, nil
}

// Token is a personal access token as the list endpoint serves it.
//
// Never the value: the server returns that from the mint call alone. There
// is no expiry field — Mattermost personal access tokens do not expire, so
// a token that exists and is not revoked is a token that works.
type Token struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	// Value is the plaintext, present on a mint response only.
	Value string `json:"token"`
}

// Tokens lists an account's access tokens.
func (c *Client) Tokens(ctx context.Context, userID string) ([]Token, error) {
	var tokens []Token
	_, err := c.request(ctx, http.MethodGet,
		"/users/"+userID+"/tokens", nil, &tokens, true)
	return tokens, err
}

// RevokeToken removes one access token.
func (c *Client) RevokeToken(ctx context.Context, tokenID string) error {
	_, err := c.request(ctx, http.MethodPost, "/users/tokens/revoke",
		map[string]string{"token_id": tokenID}, nil, false)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// RevokeTokens removes every access token on an account.
//
// THE ROLLBACK PATH FOR A BOT THIS RUN CREATED, and only that: nothing else
// has ever minted on it, so taking everything takes exactly what this run
// caused. On a bot that already existed the rollback revokes by id instead —
// sweeping it would take an administrator's own token with no way to tell.
func (c *Client) RevokeTokens(ctx context.Context, userID string) error {
	tokens, err := c.Tokens(ctx, userID)
	if err != nil {
		return err
	}
	var stuck []string
	for _, t := range tokens {
		if err := c.RevokeToken(ctx, t.ID); err != nil {
			stuck = append(stuck, t.ID)
		}
	}
	if len(stuck) > 0 {
		return fmt.Errorf("mattermost: these tokens on %s could not be revoked "+
			"and are still live — remove them by hand: %s",
			userID, strings.Join(stuck, ", "))
	}
	return nil
}

// RevokeMinted removes every token this tool minted on an account under one
// description, keeping the id in keep (empty keeps nothing), and reports how
// many it took.
//
// THE ONE WALK THREE CALLERS SHARE: a rotation retiring the token it just
// superseded, a decommission removing a departed seat's, and a disconnect
// removing a seat's before its bot is disabled. Written three times, the
// three would have disagreed about the one thing that matters here, which is
// what "this tool's own" means, and the caller that got it wrong would revoke
// an administrator's own token or leave a live one behind, silently either
// way.
//
// FOLDED, because the description is built from a seat handle a person wrote
// (see [TokenDescription]) and a decommission recovers that handle from the
// LOWERCASED username Mattermost stores. Comparing exactly there would leave
// a mixed-case seat's credential live on an account this engine had just
// disabled.
func (c *Client) RevokeMinted(ctx context.Context, userID, description, keep string) (int, error) {
	tokens, err := c.Tokens(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("mattermost: list the tokens on %s: %w", userID, err)
	}
	revoked := 0
	var failures []error
	for _, token := range tokens {
		if token.ID == keep || !strings.EqualFold(token.Description, description) {
			continue
		}
		// EVERY ONE ATTEMPTED before any is reported: the caller either
		// rolls the run back or fails the disconnect, and both are
		// better off having taken what they could than stopping at the
		// first token an instance would not give up.
		if err := c.RevokeToken(ctx, token.ID); err != nil {
			failures = append(failures, fmt.Errorf(
				"mattermost: revoke token %s on %s: %w", token.ID, userID, err))
			continue
		}
		revoked++
	}
	return revoked, errors.Join(failures...)
}

// TeamByName resolves a team by its slug.
func (c *Client) TeamByName(ctx context.Context, name string) (Team, bool, error) {
	var out Team
	_, err := c.request(ctx, http.MethodGet, "/teams/name/"+name, nil, &out, true)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return Team{}, false, nil
		}
		return Team{}, false, err
	}
	return out, true, nil
}

// TeamMember reports whether an account is already in a team.
//
// THE READ THAT MAKES THE JOIN CONDITIONAL, and it exists because
// [Client.AddTeamMember] tolerating a duplicate made an unconditional POST
// look free. It is not free: this pass is what the reconcile loop runs every
// few minutes for the life of the deployment, so an unconditional join is a
// membership write to somebody's instance for ever, on a company that needs
// nothing. The tolerance below stays exactly where it belongs — the backstop
// between this read and that write, for the window between them and for two
// writers racing.
//
// A SINGLE OBJECT rather than [Client.Teams]: Mattermost answers 404 when the
// account is not a member, so the answer is one row whatever the account's
// team count, where the listing grows with it.
func (c *Client) TeamMember(ctx context.Context, teamID, userID string) (bool, error) {
	if teamID == "" || userID == "" {
		return false, fmt.Errorf(
			"mattermost: a team membership lookup needs a team and a user")
	}
	var out struct {
		TeamID string `json:"team_id"`
		UserID string `json:"user_id"`
	}
	_, err := c.request(ctx, http.MethodGet, "/teams/"+url.PathEscape(teamID)+
		"/members/"+url.PathEscape(userID), nil, &out, true)
	if isStatus(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AddTeamMember adds an account to a team.
func (c *Client) AddTeamMember(ctx context.Context, teamID, userID string) error {
	_, err := c.request(ctx, http.MethodPost, "/teams/"+teamID+"/members",
		map[string]string{"team_id": teamID, "user_id": userID}, nil, false)
	if isConflictStatus(err) {
		// ALREADY A MEMBER is success: this is a reconcile, and a second
		// run must not fail on what the first one did.
		return nil
	}
	return err
}

// AddChannelMember adds an account to a channel.
func (c *Client) AddChannelMember(ctx context.Context, channelID, userID string) error {
	_, err := c.request(ctx, http.MethodPost, "/channels/"+channelID+"/members",
		map[string]string{"user_id": userID}, nil, false)
	if isConflictStatus(err) {
		return nil
	}
	return err
}

// isStatus reports an error carrying a particular HTTP status.
func isStatus(err error, status int) bool { return Status(err) == status }

// isConflictStatus reports "this already exists" in the shapes Mattermost
// uses for it.
//
// TWO STATUSES, because Mattermost is not consistent: a duplicate team
// membership comes back 400 with a message saying so, while a duplicate
// channel membership is a 409. A reconcile that only knew one would fail on
// its second run against whichever endpoint used the other — which is the
// bug that makes a provisioner "work once".
func isConflictStatus(err error) bool {
	switch Status(err) {
	case http.StatusConflict:
		return true
	case http.StatusBadRequest:
		var e *Error
		return errors.As(err, &e) && strings.Contains(
			strings.ToLower(e.Message), "already")
	}
	return false
}

// PatchBot updates a bot's display name.
//
// Provisioning is a RECONCILE, not a create-once: a role renamed in the
// company document has to reach the bot account, or the roster in Mattermost
// drifts from the org chart it is supposed to mirror and the only way back is
// to edit every bot by hand. The previous engine kept this current and the
// port only ever set the name on the create branch.
func (c *Client) PatchBot(ctx context.Context, userID, displayName string) error {
	_, err := c.request(ctx, http.MethodPut, "/bots/"+userID,
		map[string]string{"display_name": displayName}, nil, false)
	return err
}

// DisableBot deactivates a bot account without deleting it.
//
// DISABLE rather than delete, and the difference is the point: a deleted bot
// takes its posts with it, so a decommission would silently rewrite the
// history of every channel the seat ever spoke in. A disabled bot keeps what
// it said and can say nothing more, which is what decommissioning a colleague
// actually means.
func (c *Client) DisableBot(ctx context.Context, userID string) error {
	_, err := c.request(ctx, http.MethodPost, "/bots/"+userID+"/disable", nil, nil, false)
	return err
}

// EnableBot reactivates a bot account this engine disabled.
//
// THE UNDO OF A DISCONNECT, and without it a reconnect is a card that reports
// itself ready over an agent that cannot sign in. A teardown disables rather
// than deletes, deliberately, so the account survives with its history and
// its username; reconnecting then FINDS that account, joins it to the team,
// mints it a fresh token and reports success, while every socket it opens is
// refused because the account behind the token is deactivated.
//
// Mattermost's own route, which is the mirror of the disable above rather
// than the generic user-activation call: a bot is enabled through /bots.
func (c *Client) EnableBot(ctx context.Context, userID string) error {
	_, err := c.request(ctx, http.MethodPost, "/bots/"+userID+"/enable", nil, nil, false)
	return err
}

// botPageSize is Mattermost's documented per_page maximum. A larger value is
// clamped server-side, which would make a partial walk look like a complete
// one — the page size and the stop condition have to agree.
const botPageSize = 200

// botWalkCeiling stops a walk that is not converging. An instance with more
// managed bots than this is not one a single company config describes.
//
// Compared with > and not >=, for the reason internal/github's hookWalkCeiling
// carries: at >= an instance holding EXACTLY this many bots is refused by an
// error saying it has "more than" this many, though its next page is empty and
// the walk had in fact converged.
const botWalkCeiling = 5000

// Bots lists the bot accounts the instance has, including disabled ones.
//
// Including disabled: a decommission that could not see them would try to
// disable the same account on every run and report it as work done each time.
//
// PAGED TO EXHAUSTION, the same correction internal/gitlab's GroupMembers
// carries and for the same reason. It asked for one page of 200 and took
// whatever came back, which is silent truncation on the listing a DESTRUCTIVE
// decision is made from: on an instance with more bots than that, every
// managed account past the first page was invisible to a decommission sweep
// and stayed live for ever, with the run reporting success.
func (c *Client) Bots(ctx context.Context) ([]Bot, error) {
	var out []Bot
	for page := 0; ; page++ {
		var batch []Bot
		if _, err := c.request(ctx, http.MethodGet, fmt.Sprintf(
			"/bots?include_deleted=true&per_page=%d&page=%d", botPageSize, page),
			nil, &batch, true); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < botPageSize {
			return out, nil
		}
		if len(out) > botWalkCeiling {
			return nil, fmt.Errorf(
				"mattermost: the instance has more than %d bots, which is not one "+
					"Crewlet provisions into", botWalkCeiling)
		}
	}
}
