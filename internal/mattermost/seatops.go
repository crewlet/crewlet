package mattermost

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/provision"
)

// The per-seat operations the access pass is built from (design 5.2).
//
// Each is one thing done at Mattermost to one bot, with the rule that keeps it
// safe written beside it, so the access pass and the teardown that reuses them
// cannot disagree about what turning a bot off means.

// BotAccount is what a pass reads back about one seat's bot, every pass.
type BotAccount struct {
	// ID is the bot's user id, which the ledger records.
	ID       string
	Username string
	// Deactivated is the account's own state at Mattermost.
	Deactivated bool
	// DeactivatedHere reports that THIS engine deactivated it: the bot's
	// description is [DisconnectedDescription]. Only such a bot is ever
	// turned back on automatically (D3).
	DeactivatedHere bool
	// Roles are the system roles it holds, normalized.
	Roles []string
	// Channels are the team's channels it is a member of, by name, sorted.
	// Private conversations are not channels in this sense and are left
	// out: a direct message is a person's, never a scope this engine sets.
	Channels []string
	// Tokens are its personal access tokens, without their values.
	Tokens []Token

	// channelIDs maps a channel name to its id, so leaving one needs no
	// second lookup.
	channelIDs map[string]string
}

// ObserveBot reads one bot, its roles, its channels in the team and its
// tokens.
//
// THE BOT ABSENT IS AN ANSWER (found false, no error): the ledger row is
// closed and a seat that is on is given a fresh bot (design 5.4).
func (c *Client) ObserveBot(ctx context.Context, userID, teamID string) (BotAccount, bool, error) {
	user, found, err := c.UserByID(ctx, userID)
	if err != nil || !found {
		return BotAccount{}, found, err
	}
	out := BotAccount{
		ID: user.ID, Username: user.Username, Deactivated: user.DeleteAt != 0,
		Roles: RolesOf(user), channelIDs: map[string]string{},
	}
	if out.Deactivated {
		// ASKED ONLY OF A DEACTIVATED BOT: the description is a second
		// request, and the question it answers has no meaning for a bot
		// that is running.
		record, err := c.BotRecord(ctx, user.ID)
		if err != nil {
			return BotAccount{}, true, fmt.Errorf(
				"mattermost: read whether this engine deactivated %s: %w", user.Username, err)
		}
		out.DeactivatedHere = strings.TrimSpace(record.Description) == DisconnectedDescription
	}
	channels, err := c.Channels(ctx, user.ID, teamID)
	if err != nil {
		return BotAccount{}, true, fmt.Errorf(
			"mattermost: read the channels %s is in: %w", user.Username, err)
	}
	for _, channel := range channels {
		if channel.Direct() || channel.Name == "" {
			continue
		}
		out.channelIDs[channel.Name] = channel.ID
	}
	out.Channels = slices.Sorted(maps.Keys(out.channelIDs))
	if out.Tokens, err = c.Tokens(ctx, user.ID); err != nil {
		return BotAccount{}, true, fmt.Errorf(
			"mattermost: read the tokens on %s: %w", user.Username, err)
	}
	return out, true, nil
}

// MintedTokens are the tokens on the bot this engine minted for a seat, by
// the description every mint here writes.
func (b BotAccount) MintedTokens(handle string) []Token {
	var out []Token
	for _, token := range b.Tokens {
		if strings.EqualFold(token.Description, TokenDescription(handle)) {
			out = append(out, token)
		}
	}
	return out
}

// SetRoles brings a bot's system roles to the seat's, reporting whether it
// wrote. A bot already holding them is left alone: a converged pass writes
// nothing to somebody's instance.
func (c *Client) SetRoles(ctx context.Context, bot BotAccount, want []string) (bool, error) {
	if SameRoles(bot.Roles, want) {
		return false, nil
	}
	if err := c.UpdateRoles(ctx, bot.ID, want); err != nil {
		return false, err
	}
	return true, nil
}

// ChannelChange is what [Client.SetChannels] did.
type ChannelChange struct {
	Joined, Left []string
	// Missing are channels the seat names that the team does not have,
	// which is a note rather than a failure: the usual cause is a typo an
	// operator fixes in seconds.
	Missing []string
}

// SetChannels brings a bot's channel membership to the seat's scope.
//
// WITHIN THE COMPANY'S LIST AND NOWHERE ELSE. A channel the seat names is
// joined; a channel the bot is in that the company's list names and the seat
// does not is left. A channel outside the company's list was never this
// engine's to manage (a person invited the bot there, or it is a private
// conversation) and is not touched, so narrowing a seat can never remove a
// bot from a room nobody asked Crewlet to manage.
func (c *Client) SetChannels(
	ctx context.Context, teamID string, bot BotAccount, want, managed []string,
) (ChannelChange, error) {
	var out ChannelChange
	wanted := map[string]bool{}
	for _, name := range want {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
		}
	}
	for _, name := range slices.Sorted(maps.Keys(wanted)) {
		if _, in := bot.channelIDs[name]; in {
			continue
		}
		channel, err := c.ChannelByName(ctx, teamID, name)
		if isStatus(err, http.StatusNotFound) {
			out.Missing = append(out.Missing, name)
			continue
		}
		if err != nil {
			return out, fmt.Errorf("resolve channel %q: %w", name, err)
		}
		if err := c.AddChannelMember(ctx, channel.ID, bot.ID); err != nil {
			return out, fmt.Errorf("join %q: %w", name, err)
		}
		out.Joined = append(out.Joined, name)
	}
	for _, name := range slices.Sorted(slices.Values(managed)) {
		id, in := bot.channelIDs[strings.TrimSpace(name)]
		if !in || wanted[strings.TrimSpace(name)] {
			continue
		}
		if err := c.RemoveChannelMember(ctx, id, bot.ID); err != nil {
			return out, fmt.Errorf("leave %q: %w", name, err)
		}
		out.Left = append(out.Left, strings.TrimSpace(name))
	}
	return out, nil
}

// TurnOff revokes the tokens this engine minted on a bot and deactivates it.
//
// THE TOKENS FIRST, THEN THE MARKER, THEN THE DEACTIVATION. A disabled bot
// keeps its tokens, so one deactivated with a live token is an agent that
// works again the moment anybody re-enables it. The marker is written before
// the deactivation, so an interrupted run leaves a marked, live bot rather
// than a deactivated one with no provenance (D3). Only this engine's own
// tokens are revoked: an administrator may have minted one by hand.
func (c *Client) TurnOff(ctx context.Context, bot BotAccount, handle string) error {
	if _, err := c.RevokeMinted(ctx, bot.ID, TokenDescription(handle), ""); err != nil {
		return fmt.Errorf("revoke the bot's tokens: %w", err)
	}
	if err := c.MarkDisconnected(ctx, bot.ID); err != nil {
		return fmt.Errorf("record that this engine is turning the bot off: %w", err)
	}
	if bot.Deactivated {
		return nil
	}
	if err := c.DisableBot(ctx, bot.ID); err != nil {
		return fmt.Errorf("deactivate the bot: %w", err)
	}
	return nil
}

// TurnOn reactivates a bot this engine deactivated and clears the marker.
//
// THE MARKER GOES WITH IT, or a live bot still described as disconnected
// would read as this engine's off on the next pass.
func (c *Client) TurnOn(ctx context.Context, bot BotAccount) error {
	if err := c.EnableBot(ctx, bot.ID); err != nil {
		return fmt.Errorf("reactivate the bot: %w", err)
	}
	if err := c.ClearDisconnected(ctx, bot.ID); err != nil {
		return fmt.Errorf("clear the bot's %q description: %w", DisconnectedDescription, err)
	}
	return nil
}

// MintInto creates one access token on a bot and records it in the seat's
// sink.
//
// A FAILED RECORD REVOKES THE TOKEN (design 5.2): Mattermost serves a token's
// value once, so one minted and not recorded is a live credential held by
// nobody. The returned error is the record's failure with the revocation's
// outcome appended, never replaced by it.
func (c *Client) MintInto(
	ctx context.Context, sink provision.TokenSink, userID, handle, sinkVar string,
) (Token, error) {
	if sink == nil {
		return Token{}, provision.ErrNoSink
	}
	token, err := c.CreateAccessToken(ctx, userID, TokenDescription(handle))
	if err != nil {
		return Token{}, err
	}
	if err := sink.Record(ctx, sinkVar, token.Value); err != nil {
		// DETACHED: the failure is often the cancellation itself, and a
		// revocation that inherited it would do nothing.
		if revokeErr := c.RevokeToken(context.WithoutCancel(ctx), token.ID); revokeErr != nil {
			return Token{}, fmt.Errorf(
				"minted a token for %s and could not record it (%w), and could not "+
					"revoke it either (%v): it is live on the bot and held by nobody, "+
					"so revoke the token %s in the bot's settings",
				handle, err, revokeErr, token.ID)
		}
		return Token{}, fmt.Errorf(
			"minted a token for %s and could not record it, so it was revoked "+
				"again; the next pass mints another: %w", handle, err)
	}
	return Token{ID: token.ID, Description: token.Description}, nil
}

// Rotate mints a new token, records it, and then revokes the tokens this
// engine minted before it.
//
// IN THAT ORDER: the old token keeps the bot's websocket authenticated until
// the new value is recorded, so a rotation that fails part way leaves the
// seat with a working credential.
func (c *Client) Rotate(
	ctx context.Context, sink provision.TokenSink, bot BotAccount, handle, sinkVar string,
) (Token, error) {
	minted, err := c.MintInto(ctx, sink, bot.ID, handle, sinkVar)
	if err != nil {
		return Token{}, err
	}
	if _, err := c.RevokeMinted(ctx, bot.ID, TokenDescription(handle), minted.ID); err != nil {
		return minted, fmt.Errorf("revoke the replaced tokens: %w", err)
	}
	return minted, nil
}
