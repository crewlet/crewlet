package mattermost

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
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
		record, recordErr := c.BotRecord(ctx, user.ID)
		if recordErr != nil {
			return BotAccount{}, true, fmt.Errorf(
				"mattermost: read whether this engine deactivated %s: %w", user.Username, recordErr)
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

// RevokeEngineTokens revokes every token this engine minted on a bot for a
// seat, and reports how many. Only this engine's own: an administrator may
// have minted one by hand, and a deactivated bot's tokens authenticate as
// nothing anyway.
func (c *Client) RevokeEngineTokens(ctx context.Context, bot BotAccount, handle string) (int, error) {
	revoked, err := c.RevokeMinted(ctx, bot.ID, TokenDescription(handle), "")
	if err != nil {
		return revoked, fmt.Errorf("revoke the bot's tokens: %w", err)
	}
	return revoked, nil
}

// Deactivate marks a bot as this engine's off and deactivates it.
//
// THE MARKER BEFORE THE DEACTIVATION, so an interrupted run leaves a marked,
// live bot rather than a deactivated one with no provenance (D3). Marked
// whatever state the bot is in, so one already deactivated by hand is still
// recorded as this engine's off once the seat's document says off too.
func (c *Client) Deactivate(ctx context.Context, bot BotAccount) error {
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

// TurnOff revokes the tokens this engine minted on a bot, then marks and
// deactivates it.
//
// THE TOKENS FIRST: a disabled bot keeps its tokens, so one deactivated with
// a live token is an agent that works again the moment anybody re-enables
// it. One rule for a seat turned off, a departed seat and a disconnect.
func (c *Client) TurnOff(ctx context.Context, bot BotAccount, handle string) error {
	if _, err := c.RevokeEngineTokens(ctx, bot, handle); err != nil {
		return err
	}
	return c.Deactivate(ctx, bot)
}

// TurnOn reactivates a bot and clears the marker.
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

// RetireTokens revokes the tokens this engine minted on a bot other than keep,
// which is a rotation's second half: keep was minted and sealed by the step
// before, so the tokens it replaces hold values nothing reads.
//
// IT REFUSES RATHER THAN GUESSES. Mattermost lists no creation time for a
// token, so the one it keeps is named; a keep the bot does not hold means the
// token just minted is gone, and revoking the others would leave the seat
// with nothing.
func (c *Client) RetireTokens(ctx context.Context, bot BotAccount, handle, keep string) (int, error) {
	held := bot.MintedTokens(handle)
	if keep == "" || !slices.ContainsFunc(held, func(t Token) bool { return t.ID == keep }) {
		return 0, errors.New("mattermost: the token this rotation minted is not on the bot, " +
			"so no older token was revoked; rotate again")
	}
	return c.RevokeMinted(ctx, bot.ID, TokenDescription(handle), keep)
}
