package mattermost

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// The transport: everything this backend needs that is not the parser or the
// prompt.
//
// It OWNS THE FLEET, which is what makes a live config apply coherent. A
// seat's token or username can change on an apply, and the socket, the
// authenticated client and the typing indicator all have to change with it —
// so one object holds all three and rebuilds them together. Split across
// three owners, an apply leaves a node acting as a revoked identity through
// a socket nobody rebuilt.

// Config is what the engine hands this transport.
type Config struct {
	// URL is the instance address.
	URL string

	// Team is the team slug agents belong to. Channels are team-scoped,
	// so a seat cannot be placed without it.
	Team string

	// Status is when to show the typing indicator. Mattermost's has a
	// FIXED VOCABULARY, so it conveys only busy — which is why it
	// defaults off here where a text-carrying backend defaults on: a
	// multi-minute turn costs one to two orders of magnitude more
	// requests for strictly less information.
	Status notify.StatusMode

	// Seats are the bots, one per agent handle.
	Seats []SeatConfig
}

// SeatConfig is one agent's bot, as configured.
//
// NO CHANNEL, deliberately. The seat's `integrations.mattermost.channel` is a
// PROVISIONING input — [joinChannels] adds the bot to it, on the reconcile's
// own cadence and off the live org — and this transport has never aimed a
// message anywhere: an agent speaks through the Mattermost MCP server on this
// same token. Carried here it was read by nothing except the engine's rebuild
// fingerprint, so renaming a channel dropped and re-opened every seat's
// websocket, and each one replayed its backfill window, to rebuild a transport
// that could not tell the difference.
type SeatConfig struct {
	Handle string
	Token  string
	// Username defaults to the handle. Set only when the account already
	// exists under another name.
	Username string
}

// Resolve fills the defaults a seat's config leaves open.
func (s SeatConfig) Resolve() SeatConfig {
	if s.Username == "" {
		s.Username = s.Handle
	}
	s.Username = strings.TrimPrefix(strings.TrimSpace(s.Username), "@")
	return s
}

// TransportOptions configure a [Transport].
type TransportOptions struct {
	Config    Config
	Publisher Publisher

	// Follows persists thread-follow state. Nil turns thread routing off,
	// which is the pre-follow behaviour and legitimate for a single-agent
	// workspace where there is no second bot for a reply to belong to.
	Follows notify.FollowStore

	// Registry is where this transport registers its seats' identities, so
	// a message from one agent annotates the way a person's does. Read
	// through a function because an epoch swap builds a new one.
	Registry func() *notify.Registry

	// Connect opens a socket; nil takes the real dialer.
	Connect Connector

	// Backoff and Backfill are the fleet's; zero takes the defaults.
	Backoff  []time.Duration
	Backfill time.Duration

	Now func() time.Time
}

// Transport is this node's whole Mattermost presence.
type Transport struct {
	cfg      Config
	fleet    *Fleet
	registry func() *notify.Registry
	parser   *Parser
	status   *notify.StatusDriver
	now      func() time.Time

	mu sync.Mutex
	// seats is what this node currently runs, keyed by handle. Rebuilt
	// whole on an apply rather than patched, for the same reason an epoch
	// is: a partial update is a state neither config describes.
	seats map[string]runningSeat

	// throttle is the server's own typing cadence, read once at start.
	// The server ENFORCES it — sending faster is rejected, not merely
	// wasteful — so it is read rather than assumed.
	throttle time.Duration
}

type runningSeat struct {
	seat   Seat
	client *Client
}

// NewTransport builds the transport. It connects nothing until Start.
func NewTransport(opts TransportOptions) (*Transport, error) {
	if NormalizeURL(opts.Config.URL) == "" {
		return nil, fmt.Errorf("mattermost: the transport needs an instance url")
	}
	if opts.Publisher == nil {
		return nil, fmt.Errorf("mattermost: the transport needs a publisher")
	}
	t := &Transport{
		cfg:      opts.Config,
		registry: opts.Registry,
		now:      opts.Now,
		seats:    map[string]runningSeat{},
		throttle: DefaultTypingThrottle,
	}
	if t.now == nil {
		t.now = time.Now
	}

	fleet, err := NewFleet(FleetOptions{
		Publisher: opts.Publisher, Connect: opts.Connect,
		Backoff: opts.Backoff, Backfill: opts.Backfill, Now: t.now,
	})
	if err != nil {
		return nil, err
	}
	t.fleet = fleet

	var tracker *notify.ThreadTracker
	if opts.Follows != nil {
		if tracker, err = notify.NewThreadTracker(Grammar, opts.Follows); err != nil {
			return nil, err
		}
	}
	if t.parser, err = NewParser(t.lookup, tracker, t.now); err != nil {
		return nil, err
	}
	t.status = notify.NewStatusDriver(notify.StatusOptions{
		Poster: t, Mode: opts.Config.Status, Now: t.now,
	})
	return t, nil
}

// Parser is this backend's inbound half, for the notification service.
func (t *Transport) Parser() *Parser { return t.parser }

// Prompt is this backend's prompt, for the prompt registry.
func (t *Transport) Prompt() notify.ChatPrompt { return Prompt() }

// Status is the working-indicator driver, for the turn engine.
func (t *Transport) Status() *notify.StatusDriver { return t.status }

// lookup answers the parser's seat question from live state.
func (t *Transport) lookup(handle string) (Seat, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.seats[handle]
	return s.seat, ok
}

// URL is the instance this transport talks to, resolved.
//
// Exported for the same reason [Client.URL] is: a caller diagnosing a chat
// surface needs the address actually in use, which is not the string the
// config holds — that one is usually a ${VAR}.
func (t *Transport) URL() string { return t.cfg.URL }

// Start connects every configured seat, opening the websocket fleet that
// delivers to the seats this node holds.
//
// A SEAT THAT FAILS DOES NOT STOP THE OTHERS. One bot's token being revoked
// is an ordinary state — an operator rotating credentials one at a time —
// and refusing to start the company over it would turn a one-seat problem
// into a whole-company outage. The failure is reported per seat.
func (t *Transport) Start(ctx context.Context) error {
	if len(t.cfg.Seats) == 0 {
		log.InfoContext(ctx, "mattermost_no_seats_configured")
		return nil
	}

	// The server's own typing cadence and Site URL, read ONCE from the
	// first usable seat: they are properties of the instance, not of a
	// bot, and reading them per seat would be N identical calls.
	t.readInstance(ctx)

	// CONCURRENTLY, and that is not an optimisation. Each seat resolves
	// its identity against the server, and a failing call spends the
	// client's whole retry budget — so started in sequence, an instance
	// that is down delays boot by that budget times the number of seats.
	// Started together, it costs one budget however many seats there are,
	// and the fleet's own reconnect loop keeps trying afterwards.
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed []string
	)
	for _, cfg := range t.cfg.Seats {
		wg.Go(func() {
			if err := t.startSeat(ctx, cfg.Resolve()); err != nil {
				mu.Lock()
				failed = append(failed, cfg.Handle)
				mu.Unlock()
				log.ErrorContext(ctx, "mattermost_seat_failed", "handle", cfg.Handle,
					"error", err.Error())
			}
		})
	}
	wg.Wait()
	slices.Sort(failed)
	log.InfoContext(ctx, "mattermost_started", "seats", len(t.cfg.Seats),
		"connected", len(t.cfg.Seats)-len(failed), "failed", failed,
		"typing_status", string(t.status.Mode()))
	if len(failed) == len(t.cfg.Seats) {
		// EVERY seat failing is not N seat problems: it is the
		// instance, the url or the network, and reporting it as such
		// sends an operator somewhere useful.
		return fmt.Errorf("mattermost: no seat could connect to %s", t.cfg.URL)
	}
	return nil
}

// startSeat resolves one bot's identity and attaches its socket.
func (t *Transport) startSeat(ctx context.Context, cfg SeatConfig) error {
	if cfg.Token == "" {
		return fmt.Errorf("seat %q has no bot token", cfg.Handle)
	}
	c, err := NewClient(ClientOptions{URL: t.cfg.URL, Token: cfg.Token, Now: t.now})
	if err != nil {
		return err
	}
	// THE IDENTITY IS RESOLVED, never assumed from config. An id the
	// engine guessed disables own-message suppression when it is wrong,
	// and an agent that cannot recognise its own posts answers itself
	// for ever at one turn each.
	me, err := c.Me(ctx)
	if err != nil {
		return fmt.Errorf("resolving identity: %w", err)
	}
	seat := Seat{Handle: cfg.Handle, Username: me.Username, UserID: me.ID}
	if seat.Username == "" {
		// The server knows better, but if it said nothing the
		// configured name is the only one anybody can address.
		seat.Username = cfg.Username
	}

	t.mu.Lock()
	t.seats[cfg.Handle] = runningSeat{seat: seat, client: c}
	t.mu.Unlock()

	t.register(seat)
	return t.fleet.Add(ctx, seat, c)
}

// register puts this seat's identities into the party registry.
//
// BOTH NAMESPACES, because the two halves of the system see different
// identifiers: an inbound payload names a poster by user id, while a person
// typing a mention and an MCP tool addressing a colleague both use the
// username. Registering one only means a fellow agent's message resolves to
// nobody and gets annotated as a stranger, while a human's identical message
// is annotated as a colleague.
func (t *Transport) register(seat Seat) {
	t.mu.Lock()
	lookup := t.registry
	t.mu.Unlock()
	if lookup == nil {
		return
	}
	reg := lookup()
	if reg == nil {
		return
	}
	if seat.UserID != "" {
		if err := reg.Register(notify.BotNamespace(Backend), seat.UserID, seat.Handle); err != nil {
			log.Warn("mattermost_bot_id_not_registered", "handle", seat.Handle,
				"user_id", seat.UserID, "error", err.Error())
		}
	}
	if seat.Username != "" {
		if err := reg.Register(Backend, seat.Username, seat.Handle); err != nil {
			log.Warn("mattermost_username_not_registered", "handle", seat.Handle,
				"username", seat.Username, "error", err.Error())
		}
	}
}

// Reregister puts every running seat's identities into a NEW registry.
//
// An apply builds a fresh registry from the new org, and these identities
// are facts about the SERVER rather than about the config — resolved once at
// connect, against a live instance. Losing them on an apply would make every
// agent's own message annotate as a stranger until something reconnected,
// which is a thing nothing here would ever do on its own.
func (t *Transport) Reregister(reg *notify.Registry) {
	if reg == nil {
		return
	}
	t.mu.Lock()
	seats := make([]Seat, 0, len(t.seats))
	for _, s := range t.seats {
		seats = append(seats, s.seat)
	}
	previous := t.registry
	t.registry = func() *notify.Registry { return reg }
	t.mu.Unlock()
	_ = previous

	for _, seat := range seats {
		t.register(seat)
	}
}

// readInstance reads the facts that belong to the server rather than to a
// bot: the typing cadence it enforces, and the Site URL it believes it is
// served at.
func (t *Transport) readInstance(ctx context.Context) {
	for _, cfg := range t.cfg.Seats {
		if cfg.Token == "" {
			continue
		}
		c, err := NewClient(ClientOptions{URL: t.cfg.URL, Token: cfg.Token, Now: t.now})
		if err != nil {
			continue
		}
		conf, err := c.ClientConfig(ctx)
		if err != nil {
			continue
		}
		t.mu.Lock()
		t.throttle = TypingThrottle(conf)
		t.mu.Unlock()

		// THE SILENT FAILURE THIS EXISTS FOR: Mattermost accepts a
		// websocket only from a browser whose Origin matches its Site
		// URL, so a mismatch blinds every HUMAN in the workspace while
		// the agents — which send no Origin the server checks the same
		// way — keep working perfectly. Nothing else reports it.
		if reported := SiteURL(conf); reported != "" && !OriginMatches(t.cfg.URL, reported) {
			log.WarnContext(ctx, "mattermost_site_url_mismatch",
				"configured", t.cfg.URL, "reported", reported,
				"detail", "the server accepts a websocket only from an origin "+
					"matching its own SiteURL, so browsers will fail to connect "+
					"while agents keep working")
		}
		return
	}
}

// Stop disconnects everything.
func (t *Transport) Stop(ctx context.Context) {
	t.status.Stop(ctx)
	t.fleet.Stop()
	t.mu.Lock()
	t.seats = map[string]runningSeat{}
	t.mu.Unlock()
	log.InfoContext(ctx, "mattermost_stopped")
}

// Handles lists the seats this node is running, sorted.
func (t *Transport) Handles() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := slices.Sorted(maps.Keys(t.seats))
	return out
}

// Client exposes a seat's authenticated client, keyed by handle.
//
// ONE PER SEAT and never shared, which is the whole point of a bot per
// agent: every call carries that seat's own token, so the instance
// attributes it to the agent rather than to one company-wide account.
// Nothing here creates a post — an agent speaks through the Mattermost MCP
// server, on this same token — so what this client does is read: the
// reconnect backfill, and the thread a turn is handed. It can also raise the
// seat's typing indicator, which nothing running asks it to; see
// [Transport.SetStatus].
func (t *Transport) Client(handle string) (*Client, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.seats[handle]
	return s.client, ok
}

// ---------------------------------------------------------------- //
// notify.ThreadReader
// ---------------------------------------------------------------- //

// ThreadBackend implements [notify.ThreadReader].
func (t *Transport) ThreadBackend() string { return Backend }

// ReadThread implements [notify.ThreadReader].
//
// It reads as the SEAT, on the seat's own bot token, which is what makes the
// block match what that agent can actually see: a channel the bot is not in
// answers an error and the turn gets the unreadable hint rather than somebody
// else's conversation.
//
// A seat this node has no client for reports false rather than dereferencing
// one. That is not defensive: a bot whose token was refused at boot is left
// out of the map deliberately (see [Transport.startSeat]), and a node running
// in maintenance mode starts no chat transport at all — both are ordinary
// states, and both must render "the thread could not be read" rather than
// panicking a turn.
func (t *Transport) ReadThread(ctx context.Context, handle, channel, root string) (notify.Transcript, bool) {
	t.mu.Lock()
	s, ok := t.seats[handle]
	t.mu.Unlock()
	if !ok || s.client == nil {
		return notify.Transcript{}, false
	}
	posts, err := s.client.Thread(ctx, root)
	if err != nil {
		log.DebugContext(ctx, "mattermost_thread_unreadable", "handle", handle,
			"channel", channel, "root", root, "error", err.Error())
		return notify.Transcript{}, false
	}
	// NOTHING STOPS SHORT HERE, and the zero values say so rather than
	// being guessed at: GET /api/v4/posts/{root}/thread answers the WHOLE
	// thread in one response, with no cursor and no page size, so this
	// backend has no bound of its own to drop an older message or to stop
	// before the newest one. Slack's paged read does, which is why
	// [notify.Transcript] carries the two fields at all.
	out := notify.Transcript{Messages: make([]notify.Message, 0, len(posts))}
	for _, post := range posts {
		if post.Bookkeeping() != "" {
			continue
		}
		// SCOPED TO THE CHANNEL THE TRIGGER NAMED. The endpoint is
		// addressed by post id alone, so nothing in the request says
		// which channel this thread is meant to be in — and a root id
		// that names a post somewhere else would render another
		// conversation into this seat's prompt under the trigger's own
		// heading. Checked rather than trusted, because it is the one
		// thing the request itself cannot assert.
		if post.ChannelID != "" && post.ChannelID != channel {
			continue
		}
		body := post.Body()
		if body == "" {
			continue
		}
		out.Messages = append(out.Messages, notify.Message{
			SenderID: post.UserID,
			// NO NAME. A Mattermost post carries a user id and nothing
			// else, so the party registry is the only thing that can
			// turn one into a colleague — and a miss renders the raw
			// id, which is what a person reading the channel sees a
			// stranger as too.
			Text: body,
			// THE RESOLVED IDENTITY, never the configured one: an id
			// the engine guessed would mark none of the seat's posts
			// as its own, and an agent that cannot recognise its own
			// replies reads them as a colleague's and answers itself.
			Own: s.seat.UserID != "" && post.UserID == s.seat.UserID,
		})
	}
	return out, true
}

// ---------------------------------------------------------------- //
// notify.StatusPoster
// ---------------------------------------------------------------- //

// StatusBackend implements [notify.StatusPoster].
func (t *Transport) StatusBackend() string { return Backend }

// SupportsStatusText implements [notify.StatusPoster]: NO.
//
// Mattermost offers only the composer typing indicator, whose wording its
// client fixes. The engine can raise it; it cannot say anything with it — so
// the phrase pools go inert and a phase change stops costing a request,
// because there is nothing about it a reader could see.
func (t *Transport) SupportsStatusText() bool { return false }

// StatusRefresh implements [notify.StatusPoster].
//
// The server's own cadence, read at start. It ENFORCES it: sending faster is
// rejected rather than merely wasteful, so a guessed value that is too eager
// produces an indicator that never appears at all.
func (t *Transport) StatusRefresh() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.throttle
}

// DMChannelPrefix implements [notify.StatusPoster]: NONE.
//
// Mattermost ids are opaque 26-character alphanumerics, so a prefix test
// would mark arbitrary public channels as direct messages and raise
// indicators for traffic nobody addressed to this agent.
func (t *Transport) DMChannelPrefix() string { return "" }

// SetStatus implements [notify.StatusPoster] by raising the typing indicator.
//
// The status text is ignored, and that is what SupportsStatusText declares:
// there is nothing here to render it with.
//
// NOTHING IN PRODUCTION REACHES IT. The only way in is notify.Statuses.Begin,
// which has no caller outside tests — on either chat backend — so no agent has
// ever raised an indicator on a running company. Said here, at the
// declaration, because the absence is invisible from this side: the method is
// complete and tested, and every doc comment that cited the indicator as
// something a seat's client DOES was describing wiring rather than behaviour.
// What is missing is the call at a turn's own start, which is a product
// decision rather than a gap to close on the way past.
func (t *Transport) SetStatus(ctx context.Context, handle, channel, thread, _ string) bool {
	t.mu.Lock()
	s, ok := t.seats[handle]
	t.mu.Unlock()
	if !ok {
		return false
	}
	if err := s.client.Typing(ctx, s.seat.UserID, channel, thread); err != nil {
		log.DebugContext(ctx, "mattermost_typing_failed", "handle", handle, "error", err.Error())
		return false
	}
	return true
}

// ClearStatus implements [notify.StatusPoster].
//
// There is nothing to clear: a typing indicator has no text to empty and no
// clear operation — it LAPSES on its own a few seconds after the last
// heartbeat, which is exactly what stopping the heartbeat achieves.
//
// It reports true because the indicator does come down. Reporting false
// would log a failure on every turn end of every seat, for the one backend
// where taking the indicator down is the one thing that cannot fail.
func (t *Transport) ClearStatus(context.Context, string, string, string) bool { return true }

// SeatsFrom builds the transport's seat list from an org.
//
// The USERNAME defaults to the handle, and both are needed downstream — so
// resolving that default here, once, keeps every consumer from having to
// know the rule.
func SeatsFrom(o *org.Organization, lookup org.EnvLookup) []SeatConfig {
	if o == nil {
		return nil
	}
	var out []SeatConfig
	for role := range o.AllRoles() {
		if role.IsHuman() || role.Mattermost.IsZero() {
			continue
		}
		token := envref.Resolve(role.Mattermost.BotToken, lookup)
		if token == "" {
			// A seat whose ${VAR} did not resolve is skipped rather
			// than started with an empty token, which would fail at
			// connect with a less useful message.
			log.Warn("mattermost_seat_token_unresolved", "handle", role.Handle())
			continue
		}
		out = append(out, SeatConfig{
			Handle:   role.Handle(),
			Token:    token,
			Username: envref.Resolve(role.Mattermost.Username, lookup),
		}.Resolve())
	}
	return out
}
