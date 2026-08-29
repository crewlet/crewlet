package mattermost

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The endpoints the engine actually uses, each named for the question it
// answers rather than for its path.

// User is a Mattermost account — a person or a bot.
type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Nickname string `json:"nickname,omitempty"`
	IsBot    bool   `json:"is_bot,omitempty"`
	Email    string `json:"email,omitempty"`

	// Roles is the space-separated role list Mattermost assigns. Read for
	// ONE question: does the provisioning credential actually hold
	// system_admin? Every write this provisioner makes needs it, and a
	// token without it fails on the first bot creation with a 403 that
	// names the endpoint rather than the missing role.
	Roles string `json:"roles,omitempty"`
}

// SystemAdmin reports whether this account may provision.
func (u User) SystemAdmin() bool {
	return slices.Contains(strings.Fields(u.Roles), "system_admin")
}

// Post is one message.
type Post struct {
	ID        string         `json:"id"`
	ChannelID string         `json:"channel_id"`
	UserID    string         `json:"user_id"`
	RootID    string         `json:"root_id,omitempty"`
	Message   string         `json:"message"`
	Type      string         `json:"type,omitempty"`
	CreateAt  int64          `json:"create_at,omitempty"`
	DeleteAt  int64          `json:"delete_at,omitempty"`
	FileIDs   []string       `json:"file_ids,omitempty"`
	Props     map[string]any `json:"props,omitempty"`
}

// Channel is a room or a private conversation.
type Channel struct {
	ID          string `json:"id"`
	TeamID      string `json:"team_id,omitempty"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	// Type is Mattermost's single letter: O(pen), P(rivate), D(irect),
	// G(roup DM).
	Type string `json:"type"`
}

// Direct reports whether this channel is a private conversation.
func (c Channel) Direct() bool { return isDirect(c.Type) }

// Team is a Mattermost team; channels live inside one.
type Team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Me is the account this client's token authenticates as.
//
// The identity every seat needs before it can do anything: it is how the
// agent recognises its own posts, and an unresolved one disables
// own-message suppression entirely.
func (c *Client) Me(ctx context.Context) (User, error) {
	var out User
	_, err := c.request(ctx, http.MethodGet, "/users/me", nil, &out, false)
	return out, err
}

// Ping asks whether the server is there, UNAUTHENTICATED.
//
// The first question the doctor asks, and it must not carry a credential: a
// bad token would otherwise make a healthy server look dead, and the two
// have completely different remedies.
func (c *Client) Ping(ctx context.Context) error {
	var out map[string]any
	_, err := c.request(ctx, http.MethodGet, "/system/ping", nil, &out, false)
	return err
}

// UserByUsername resolves a name to an account.
func (c *Client) UserByUsername(ctx context.Context, username string) (User, error) {
	var out User
	name := strings.TrimPrefix(strings.TrimSpace(username), "@")
	if name == "" {
		return out, fmt.Errorf("mattermost: no username")
	}
	_, err := c.request(ctx, http.MethodGet, "/users/username/"+url.PathEscape(name), nil, &out, false)
	return out, err
}

// PostRequest is a message to send.
type PostRequest struct {
	ChannelID string `json:"channel_id"`
	Message   string `json:"message"`
	// RootID replies in a thread. Empty posts at top level, which starts
	// one — so a reply that loses this becomes a new conversation in the
	// channel rather than an answer.
	RootID string `json:"root_id,omitempty"`
}

// CreatePost sends a message.
//
// NOT repeatable: every call creates a post, so a retry on anything but a
// proven-rejected failure would double-post into a channel people read.
func (c *Client) CreatePost(ctx context.Context, req PostRequest) (Post, error) {
	var out Post
	if req.ChannelID == "" {
		return out, fmt.Errorf("mattermost: a post needs a channel")
	}
	_, err := c.request(ctx, http.MethodPost, "/posts", req, &out, false)
	return out, err
}

// postList is the wire shape of every list-of-posts endpoint: a map plus an
// ordering, because JSON objects have none.
type postList struct {
	Order []string        `json:"order"`
	Posts map[string]Post `json:"posts"`
}

// ordered returns the posts oldest-first.
//
// Mattermost's `order` is NEWEST FIRST, and every consumer here wants a
// conversation in the order it happened — a backfill replayed newest-first
// would hand a seat the answer before the question.
func (l postList) ordered() []Post {
	out := make([]Post, 0, len(l.Order))
	for i := len(l.Order) - 1; i >= 0; i-- {
		if p, ok := l.Posts[l.Order[i]]; ok {
			out = append(out, p)
		}
	}
	return out
}

// PostsSince reads a channel's posts created after a server timestamp,
// oldest first.
//
// THE RECONNECT BACKFILL. Mattermost replays nothing on a websocket
// reconnect, so a seat that dropped its socket for thirty seconds has simply
// not heard whatever was said — this is the only way back.
//
// `since` is a MILLISECOND stamp from the SERVER's clock, which is why
// [Client.ServerTime] exists: the engine's own clock skewed even slightly
// early re-reads messages the seat already answered, and skewed late silently
// loses the ones it never saw.
func (c *Client) PostsSince(ctx context.Context, channelID string, since time.Time) ([]Post, error) {
	if channelID == "" {
		return nil, fmt.Errorf("mattermost: no channel")
	}
	path := "/channels/" + url.PathEscape(channelID) + "/posts?since=" +
		strconv.FormatInt(since.UnixMilli(), 10)
	var out postList
	if _, err := c.request(ctx, http.MethodGet, path, nil, &out, false); err != nil {
		return nil, err
	}
	return out.ordered(), nil
}

// Thread reads a whole conversation, oldest first.
func (c *Client) Thread(ctx context.Context, rootID string) ([]Post, error) {
	if rootID == "" {
		return nil, fmt.Errorf("mattermost: no thread")
	}
	var out postList
	path := "/posts/" + url.PathEscape(rootID) + "/thread"
	if _, err := c.request(ctx, http.MethodGet, path, nil, &out, false); err != nil {
		return nil, err
	}
	return out.ordered(), nil
}

// Channels lists the channels a user belongs to on a team.
//
// The fleet's work list: a seat re-reads exactly these over a reconnect gap,
// because they are the only ones it could have been spoken to in.
func (c *Client) Channels(ctx context.Context, userID, teamID string) ([]Channel, error) {
	if userID == "" || teamID == "" {
		return nil, fmt.Errorf("mattermost: channels need a user and a team")
	}
	var out []Channel
	path := "/users/" + url.PathEscape(userID) + "/teams/" + url.PathEscape(teamID) + "/channels"
	_, err := c.request(ctx, http.MethodGet, path, nil, &out, false)
	return out, err
}

// Teams lists the teams a user belongs to.
func (c *Client) Teams(ctx context.Context, userID string) ([]Team, error) {
	if userID == "" {
		userID = "me"
	}
	var out []Team
	_, err := c.request(ctx, http.MethodGet, "/users/"+url.PathEscape(userID)+"/teams", nil, &out, false)
	return out, err
}

// ChannelByName resolves a team-scoped channel name to a channel.
func (c *Client) ChannelByName(ctx context.Context, teamID, name string) (Channel, error) {
	var out Channel
	if teamID == "" || name == "" {
		return out, fmt.Errorf("mattermost: a channel lookup needs a team and a name")
	}
	path := "/teams/" + url.PathEscape(teamID) + "/channels/name/" + url.PathEscape(name)
	_, err := c.request(ctx, http.MethodGet, path, nil, &out, false)
	return out, err
}

// DirectChannel opens (or finds) the private conversation between two users.
//
// REPEATABLE: Mattermost returns the existing channel rather than making a
// second one, so a retry after a lost answer is safe — and the alternative
// is a seat that cannot reach a person because one response went missing.
func (c *Client) DirectChannel(ctx context.Context, userA, userB string) (Channel, error) {
	var out Channel
	if userA == "" || userB == "" {
		return out, fmt.Errorf("mattermost: a direct channel needs two users")
	}
	_, err := c.request(ctx, http.MethodPost, "/channels/direct", []string{userA, userB}, &out, true)
	return out, err
}

// Typing raises the composer typing indicator.
//
// REPEATABLE, and it must be: this is re-asserted on a heartbeat for the
// whole of a turn, so a lost answer costing the indicator would leave a
// person watching nothing while an agent works for minutes.
//
// parentID scopes it to a thread, which is where the reply will land.
func (c *Client) Typing(ctx context.Context, userID, channelID, parentID string) error {
	if userID == "" || channelID == "" {
		return fmt.Errorf("mattermost: typing needs a user and a channel")
	}
	body := map[string]string{"channel_id": channelID}
	if parentID != "" {
		body["parent_id"] = parentID
	}
	_, err := c.request(ctx, http.MethodPost,
		"/users/"+url.PathEscape(userID)+"/typing", body, nil, true)
	return err
}

// ClientConfig is the server's public configuration.
//
// Read once at connect for two facts that are otherwise invisible: the Site
// URL, which decides whether a browser's websocket is accepted at all, and
// the typing throttle, which the server ENFORCES — sending faster is
// rejected, not merely wasteful.
func (c *Client) ClientConfig(ctx context.Context) (map[string]string, error) {
	var out map[string]string
	_, err := c.request(ctx, http.MethodGet, "/config/client?format=old", nil, &out, false)
	return out, err
}

// SiteURL is the address the server believes it is served at.
func SiteURL(clientConfig map[string]string) string {
	return NormalizeURL(clientConfig["SiteURL"])
}

// TypingThrottle is how often the server permits a typing indicator.
//
// Split from the read so a caller already holding the client config — the
// transport, which reads it once at connect for this and the Site URL check
// — does not fetch it twice.
func TypingThrottle(clientConfig map[string]string) time.Duration {
	ms, err := strconv.Atoi(clientConfig["TimeBetweenUserTypingUpdatesMilliseconds"])
	if err != nil || ms <= 0 {
		return DefaultTypingThrottle
	}
	return time.Duration(ms) * time.Millisecond
}

// ServerTime is the instance's own clock, from the Date header.
//
// NOT a convenience. A reconnect window compares SERVER-stamped post
// timestamps, so "now" cannot come from the engine's clock: skewed even
// slightly early it re-reads messages the seat already answered, and skewed
// late it silently loses the ones it never saw. Every clock in a fleet is a
// different clock, and this is the one that matters.
//
// It rides on a cheap authenticated call rather than having an endpoint of
// its own, because every response carries the header.
func (c *Client) ServerTime(ctx context.Context) (time.Time, error) {
	header, err := c.request(ctx, http.MethodGet, "/users/me", nil, nil, false)
	if err != nil {
		return time.Time{}, err
	}
	stamped, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		return time.Time{}, fmt.Errorf("mattermost: server sent no usable Date: %w", err)
	}
	return stamped.UTC(), nil
}
