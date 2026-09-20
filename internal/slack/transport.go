package slack

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// The transport: everything this backend needs that is not the parser or the
// prompt.
//
// # Nothing to connect, and that is the difference from the self-hosted one
//
// The chat backend the engine already served holds one WEBSOCKET per seat,
// because Mattermost has no usable inbound webhook — so its transport has a
// lifecycle, a reconnect policy and a backfill window. Slack pushes: each
// seat's app posts to its own request URL, the API edge verifies it, and the
// parser reads it. So this transport holds no connection at all. What it
// owns is one authenticated client per seat and the identity that client
// resolves — and those clients send no message: an agent speaks through the
// Slack MCP server on this same token, so the only calls made here are
// `auth.test` at start and the working indicator.
//
// That makes Start cheap and failure local: a seat whose token is refused
// costs that seat's identity and nothing else, where the self-hosted
// backend's equivalent is a socket that will not open.

// Config is what the engine hands this transport.
type Config struct {
	// Status is when to show the working indicator. Slack's carries TEXT,
	// which is why it defaults on here where a fixed-vocabulary indicator
	// defaults off: a phase change is something a reader can actually see.
	Status notify.StatusMode

	// Phrases are the per-phase status lines, from config.
	Phrases notify.Phrases

	// Seats are the apps, one per agent handle.
	Seats []SeatConfig
}

// SeatConfig is one agent's Slack app, as configured.
type SeatConfig struct {
	Handle string
	// Token is the app's bot token (xoxb-…).
	Token string
}

// TransportOptions configure a [Transport].
type TransportOptions struct {
	Config Config

	// Follows persists thread-follow state. Nil turns thread routing off,
	// which is the pre-follow behaviour and legitimate for a single-agent
	// workspace where there is no second bot for a reply to belong to.
	Follows notify.FollowStore

	// Registry is where this transport registers its seats' identities, so
	// a message from one agent annotates the way a person's does. Read
	// through a function because an epoch swap builds a new one.
	Registry func() *notify.Registry

	// HTTP is the client every seat's calls go through; nil takes a
	// default with [ClientTimeout].
	HTTP *http.Client

	Now func() time.Time
}

// Transport is this node's whole Slack presence.
type Transport struct {
	cfg      Config
	registry func() *notify.Registry
	parser   *Parser
	status   *notify.StatusDriver
	http     *http.Client

	mu sync.Mutex
	// seats is what this node currently runs, keyed by handle. Rebuilt
	// whole on an apply rather than patched, for the same reason an epoch
	// is: a partial update is a state neither config describes.
	seats map[string]runningSeat
}

type runningSeat struct {
	seat   Seat
	client *Client
}

// NewTransport builds the transport. It resolves nothing until Start.
func NewTransport(opts TransportOptions) (*Transport, error) {
	if len(opts.Config.Seats) == 0 {
		return nil, fmt.Errorf("slack: the transport needs at least one app")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	httpClient := opts.HTTP
	if httpClient == nil {
		httpClient = httpx.Client(ClientTimeout)
	}
	t := &Transport{
		cfg: opts.Config, registry: opts.Registry,
		http: httpClient, seats: map[string]runningSeat{},
	}

	var threads *notify.ThreadTracker
	if opts.Follows != nil {
		var err error
		threads, err = notify.NewThreadTracker(Grammar, opts.Follows)
		if err != nil {
			return nil, fmt.Errorf("slack: %w", err)
		}
	} else {
		log.Warn("slack_thread_routing_off",
			"detail", "no durable follow store, so every message reaches its "+
				"seat and thread replies are not filtered")
	}
	parser, err := NewParser(t.lookup, threads, now)
	if err != nil {
		return nil, err
	}
	t.parser = parser
	t.status = notify.NewStatusDriver(notify.StatusOptions{
		Poster: t, Mode: opts.Config.Status, Phrases: opts.Config.Phrases, Now: now,
	})
	return t, nil
}

// Parser is the inbound half.
func (t *Transport) Parser() *Parser { return t.parser }

// Prompt is what a Slack message asks of the seat it reached.
func (t *Transport) Prompt() notify.ChatPrompt { return Prompt() }

// Status is the working-indicator driver.
func (t *Transport) Status() *notify.StatusDriver { return t.status }

// Handles names the seats this transport runs, sorted.
func (t *Transport) Handles() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := slices.Sorted(maps.Keys(t.seats))
	return out
}

// Apps is the app each running seat authenticates as, by handle.
//
// LEARNED, not configured. Nothing in the org model names an agent's Slack
// app: the token does, and `auth.test` is what turns one into the other at
// wire time. An operator looking at a roster of agents wants to know WHICH of
// their apps each one is, which is the id every Slack settings page is keyed
// on, and this is the only place in the process that knows it.
//
// Only the seats that came up. A seat whose token was refused has no app to
// name, and inventing one from the config would name an app that may not
// exist.
func (t *Transport) Apps() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]string, len(t.seats))
	for handle, s := range t.seats {
		if s.seat.AppID != "" {
			out[handle] = s.seat.AppID
		}
	}
	return out
}

// running is the ONE per-seat lookup: the identity this node resolved at
// start and the authenticated client that resolved it, together.
//
// TOGETHER RATHER THAN SEPARATELY, because every caller here needs both and
// the map is REPLACED WHOLE on an apply and on [Transport.Stop] — so two
// lookups around one operation can straddle that swap and pair one epoch's
// client with another's identity, or with no identity at all. That is not
// cosmetic: [Transport.ReadThread] marks the seat's own replies from
// [Seat.Owns], and an identity lost between the two lookups turns every one
// of the agent's own posts into a colleague's, which is the confusion this
// whole read exists to prevent.
//
// ONE CLIENT PER SEAT and never shared: a Slack app has one bot user and one
// token, so every call made through it is made AS that agent and the
// workspace attributes it to them. A seat whose token was refused at boot is
// not in the map at all; see [Transport.Start] for why it is dropped rather
// than run half-configured.
func (t *Transport) running(handle string) (runningSeat, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.seats[handle]
	return s, ok
}

// lookup implements [Seats].
func (t *Transport) lookup(handle string) (Seat, bool) {
	s, ok := t.running(handle)
	if !ok {
		return Seat{}, false
	}
	return s.seat, true
}

// Start resolves every seat's identity.
//
// CONCURRENTLY, and a seat whose token is refused is left out rather than
// failing the start: the other seats work, and the one that does not is
// reported by name. Its consequence is precise and worth stating — without
// an identity it cannot recognise its own messages, so it would answer
// itself, which is why it is dropped rather than run half-configured.
func (t *Transport) Start(ctx context.Context) error {
	type resolved struct {
		seat   Seat
		client *Client
		err    error
	}
	found := make([]resolved, len(t.cfg.Seats))

	var wg sync.WaitGroup
	for i, cfg := range t.cfg.Seats {
		wg.Go(func() {
			client, err := NewClient(cfg.Token, t.http)
			if err != nil {
				found[i].err = err
				return
			}
			identity, err := client.AuthTest(ctx)
			if err != nil {
				found[i].err = err
				return
			}
			found[i] = resolved{
				seat: Seat{
					Handle: cfg.Handle, BotUserID: identity.UserID,
					AppID: identity.AppID,
				},
				client: client,
			}
		})
	}
	wg.Wait()

	seats := make(map[string]runningSeat, len(found))
	for i, r := range found {
		if r.err != nil {
			log.ErrorContext(ctx, "slack_seat_unavailable", "handle", t.cfg.Seats[i].Handle,
				"error", r.err.Error(),
				"detail", "this seat receives nothing and shows no working "+
					"indicator until its app token is fixed; leaving it out is "+
					"what stops it answering its own messages")
			continue
		}
		seats[r.seat.Handle] = runningSeat{seat: r.seat, client: r.client}
	}
	t.mu.Lock()
	t.seats = seats
	t.mu.Unlock()

	for _, s := range seats {
		t.register(s.seat)
	}
	if len(seats) == 0 {
		return fmt.Errorf("slack: no app token resolved, so no seat can send or receive")
	}
	log.InfoContext(ctx, "slack_wired", "seats", len(seats), "status", string(t.cfg.Status))
	return nil
}

// Stop releases the working indicators.
//
// There is no connection to close — the inbound half is the API edge — so
// this exists for the one thing that outlives the process visibly: a raised
// status stays up until Slack expires it, and a seat that vanished mid-turn
// would look like it was still thinking.
func (t *Transport) Stop(ctx context.Context) {
	t.status.Stop(ctx)
	t.mu.Lock()
	t.seats = map[string]runningSeat{}
	t.mu.Unlock()
}

// register binds a seat's Slack identity in the party registry, so a message
// from one agent annotates the way a person's does.
//
// The BOT NAMESPACE, because a Slack payload names a bot by the same U…/B…
// id shape it names a person by — and the seat's own id must not shadow the
// human id space the org's contacts are registered in.
func (t *Transport) register(seat Seat) {
	t.mu.Lock()
	lookup := t.registry
	t.mu.Unlock()
	if lookup == nil {
		return
	}
	reg := lookup()
	if reg == nil || seat.BotUserID == "" {
		return
	}
	if err := reg.Register(notify.BotNamespace(Backend), seat.BotUserID, seat.Handle); err != nil {
		log.Warn("slack_bot_id_not_registered", "handle", seat.Handle,
			"user_id", seat.BotUserID, "error", err.Error())
	}
}

// Reregister puts every running seat's identity into a NEW registry.
//
// An apply builds a fresh registry from the new org, and these identities
// are facts about SLACK rather than about the config — resolved once at
// start, against the live workspace. Losing them on an apply would make
// every agent's own message annotate as a stranger until something
// re-resolved them, which nothing here does on its own.
func (t *Transport) Reregister(reg *notify.Registry) {
	if reg == nil {
		return
	}
	t.mu.Lock()
	seats := make([]Seat, 0, len(t.seats))
	for _, s := range t.seats {
		seats = append(seats, s.seat)
	}
	t.registry = func() *notify.Registry { return reg }
	t.mu.Unlock()

	for _, seat := range seats {
		t.register(seat)
	}
}

// ThreadBackend implements [notify.ThreadReader].
func (t *Transport) ThreadBackend() string { return Backend }

// ReadThread implements [notify.ThreadReader].
//
// It reads as the SEAT, on that app's own bot token and its own history
// scopes, so the block matches what this agent can actually see: a channel
// the app is not in answers `not_in_channel` and the turn gets the unreadable
// hint rather than somebody else's conversation.
//
// A seat this node has no client for reports false rather than dereferencing
// one. A token refused at boot leaves the seat out of the map deliberately,
// and a maintenance-mode node runs no chat transport at all — both ordinary,
// and both must render "the thread could not be read" rather than panicking a
// turn.
func (t *Transport) ReadThread(ctx context.Context, handle, channel, root string) (notify.Transcript, bool) {
	s, ok := t.running(handle)
	if !ok || s.client == nil {
		return notify.Transcript{}, false
	}
	read, err := s.client.Replies(ctx, channel, root)
	if err != nil {
		// DEBUG, like the indicator's: a refusal here costs the block and
		// nothing else, the turn runs on the unreadable hint, and a
		// workspace missing a history scope would otherwise log a warning
		// on every chat turn of every seat.
		log.DebugContext(ctx, "slack_thread_unreadable", "handle", handle,
			"channel", channel, "thread", root, "error", err.Error())
		return notify.Transcript{}, false
	}
	// BOTH BOUNDS TRAVEL WITH THE MESSAGES. conversations.replies pages
	// from the oldest end, so what a bounded walk cannot reach is the
	// NEWEST message — the one that woke this turn — and a renderer handed
	// the messages alone would present the start of a conversation as the
	// whole of it.
	out := notify.Transcript{
		Messages:     make([]notify.Message, 0, len(read.Messages)),
		Older:        read.Older,
		StoppedShort: read.StoppedShort,
	}
	var rootSeen bool
	for _, reply := range read.Messages {
		// THE ROOT IS KEPT WHATEVER IT SAYS, and it is recognised by its
		// own ts rather than by its position: [notify.ThreadReader]
		// promises the root FIRST, and dropping it for want of a body
		// hands the renderer a transcript whose first line is the oldest
		// surviving REPLY — which every bound then protects and the
		// prompt then calls the thread's opening. An alert bot posting
		// its payload in attachments or blocks alone carries no `text`
		// and no `files`, so a thread rooted on one is not an edge case,
		// it is the shape of an alert channel.
		//
		// It travels with an EMPTY body rather than an invented one: what
		// to say in its place is the renderer's decision, and this
		// transport rendering prose would put one backend's wording in
		// front of a seat and the other's beside it.
		isRoot := reply.TS != "" && reply.TS == root
		body := ""
		if reply.Skip() == "" {
			body = reply.Body()
		}
		if body == "" && !isRoot {
			continue
		}
		rootSeen = rootSeen || isRoot
		out.Messages = append(out.Messages, notify.Message{
			// THE SAME FALLBACK CHAIN [Sender] applies, split
			// across the two fields: a human and a bot USER carry
			// `user`, while a legacy bot_message — an incoming
			// webhook, a workflow bot — carries `username` and
			// `bot_id` instead. Either way the sender is never
			// blank, and an unattributed line in a thread reads as
			// the previous speaker continuing.
			SenderID:   firstOf(reply.User, reply.BotID),
			SenderName: reply.Username,
			Text:       body,
			// BOTH IDS. A bot_message echo of this seat's own post
			// carries the app id and no user id at all, so a check
			// on the user id alone would present the agent's own
			// replies back to it as a colleague's.
			Own: s.seat.Owns(reply.User, reply.AppID),
		})
	}
	if len(out.Messages) > 0 && !rootSeen {
		// A READ THAT REACHED REPLIES BUT NOT THE POST THEY HANG OFF.
		// conversations.replies answers with the parent first, so this is
		// the workspace behaving unexpectedly rather than a known shape —
		// and the honest transcript is one whose first entry is still the
		// root, empty, rather than one that silently starts at a reply.
		out.Messages = append([]notify.Message{{}}, out.Messages...)
	}
	return out, true
}

// StatusBackend implements [notify.StatusPoster].
func (t *Transport) StatusBackend() string { return Backend }

// SupportsStatusText implements [notify.StatusPoster]: YES.
//
// assistant.threads.setStatus renders the string under the thread's
// composer, so a phase change is something the person waiting can read —
// which is what makes the phrase pools worth having here and inert on a
// backend with a fixed indicator.
func (t *Transport) SupportsStatusText() bool { return true }

// StatusRefresh implements [notify.StatusPoster].
//
// Slack expires a raised status after about two minutes, so the heartbeat
// sits well inside that window: 45 seconds re-asserts twice before expiry,
// which survives one lost call without the indicator flickering, and costs
// roughly one request a minute per live turn.
func (t *Transport) StatusRefresh() time.Duration { return 45 * time.Second }

// DMChannelPrefix implements [notify.StatusPoster].
//
// Slack's channel ids carry their kind in the first letter, so this is exact
// rather than a heuristic — and it is what answers for an app_mention, whose
// payload omits `channel_type` entirely.
func (t *Transport) DMChannelPrefix() string { return DMPrefix }

// SetStatus implements [notify.StatusPoster].
func (t *Transport) SetStatus(ctx context.Context, handle, channel, thread, status string) bool {
	return t.setStatus(ctx, handle, channel, thread, status)
}

// ClearStatus implements [notify.StatusPoster].
//
// Slack's own clear IS an empty status, so this is [Transport.SetStatus]
// with no text — spelled out rather than left to the caller, because the
// contract does not overload the payload for backends whose indicator has no
// text to empty.
func (t *Transport) ClearStatus(ctx context.Context, handle, channel, thread string) bool {
	return t.setStatus(ctx, handle, channel, thread, "")
}

func (t *Transport) setStatus(ctx context.Context, handle, channel, thread, status string) bool {
	s, ok := t.running(handle)
	if !ok || s.client == nil {
		return false
	}
	if err := s.client.SetStatus(ctx, channel, thread, status); err != nil {
		// DEBUG, not warn: the indicator is a cosmetic side-channel and
		// a failed call costs nothing but its own absence — Slack
		// expires whatever is raised on its own. A busy workspace would
		// otherwise fill the log with them.
		log.DebugContext(ctx, "slack_set_status_failed", "handle", handle, "error", err.Error())
		return false
	}
	return true
}

// SeatsFrom builds the transport's app list from an org.
func SeatsFrom(o *org.Organization, lookup org.EnvLookup) []SeatConfig {
	if o == nil {
		return nil
	}
	var out []SeatConfig
	for role := range o.AllRoles() {
		if role.IsHuman() || role.Slack.IsZero() {
			continue
		}
		token := envref.Resolve(role.Slack.BotToken, lookup)
		if token == "" {
			// A seat whose ${VAR} did not resolve is skipped rather
			// than started with an empty token, which would fail at
			// auth.test with a less useful message.
			log.Warn("slack_seat_token_unresolved", "handle", role.Handle())
			continue
		}
		out = append(out, SeatConfig{Handle: role.Handle(), Token: token})
	}
	return out
}
