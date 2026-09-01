package slack

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/envref"
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
// owns is the OUTBOUND half — one client per seat — and the identities those
// clients resolve.
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
	// Channel is where this seat posts when nothing else names a target.
	Channel string
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
	now      func() time.Time

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
		httpClient = &http.Client{Timeout: ClientTimeout}
	}
	t := &Transport{
		cfg: opts.Config, registry: opts.Registry,
		http: httpClient, now: now, seats: map[string]runningSeat{},
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
	out := make([]string, 0, len(t.seats))
	for handle := range t.seats {
		out = append(out, handle)
	}
	slices.Sort(out)
	return out
}

// lookup implements [Seats].
func (t *Transport) lookup(handle string) (Seat, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.seats[handle]
	if !ok {
		return Seat{}, false
	}
	return s.seat, true
}

// Client is one seat's authenticated app, for a caller that needs to post.
func (t *Transport) Client(handle string) (*Client, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.seats[handle]
	if !ok {
		return nil, false
	}
	return s.client, true
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
		wg.Add(1)
		go func() {
			defer wg.Done()
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
					AppID: identity.AppID, Channel: cfg.Channel,
				},
				client: client,
			}
		}()
	}
	wg.Wait()

	seats := make(map[string]runningSeat, len(found))
	for i, r := range found {
		if r.err != nil {
			log.ErrorContext(ctx, "slack_seat_unavailable", "handle", t.cfg.Seats[i].Handle,
				"error", r.err.Error(),
				"detail", "this seat sends and receives nothing until its app "+
					"token is fixed; leaving it out is what stops it answering "+
					"its own messages")
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

// Send posts as one seat, into a thread where one is named.
//
// It records PARTICIPATION on a threaded reply, which is how every chat
// client treats replying: the seat now follows the thread and hears what
// comes back, without having to be named again.
func (t *Transport) Send(ctx context.Context, handle, channel, thread, text string) (string, error) {
	t.mu.Lock()
	s, ok := t.seats[handle]
	t.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("slack: no app for seat %q", handle)
	}
	if channel == "" {
		channel = s.seat.Channel
	}
	if channel == "" {
		return "", fmt.Errorf("slack: %s: no channel to post in", handle)
	}
	ts, err := s.client.PostMessage(ctx, channel, thread, text)
	if err != nil {
		return "", err
	}
	if thread != "" && t.parser.threads != nil {
		if err := t.parser.threads.Participated(ctx, handle, channel, thread, t.now()); err != nil {
			log.WarnContext(ctx, "slack_participation_not_recorded", "handle", handle,
				"thread", thread, "error", err.Error())
		}
	}
	return ts, nil
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
	t.mu.Lock()
	s, ok := t.seats[handle]
	t.mu.Unlock()
	if !ok {
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
		out = append(out, SeatConfig{
			Handle:  role.Handle(),
			Token:   token,
			Channel: envref.Resolve(role.Slack.Channel, lookup),
		})
	}
	return out
}
