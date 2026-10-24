package mattermost

import (
	"context"
	"fmt"
	"maps"
	"net/http"
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

	// People are the human seats' usernames, which the transport resolves
	// to the user ids their posts carry (see people.go). An apply replaces
	// them through [Transport.Reregister] rather than through a rebuild:
	// they are not what a socket is built from, so naming a new person
	// reconnects nobody.
	People []Person
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

	// Claims is the fleet-wide registry each post is claimed in before it
	// is published — see [FleetOptions.Claims]. Required.
	Claims Claims

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

	// PeopleRefresh is how often the people's usernames are asked about
	// again; zero takes [PeopleRefresh].
	PeopleRefresh time.Duration

	// OnPeople is called when an answer changes which people the server
	// holds no account for — what [Transport.PeopleFindings] reports — so
	// the integration status can be read again rather than waiting out its
	// cadence. Nil is valid.
	OnPeople func()

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

	// people is what is known about the human seats; see people.go.
	people people

	// registering serialises [Transport.publishPeople] whole, from the
	// read of what is known to the last write into the registry. An apply
	// and an answer both register, and each reads before it writes: let
	// one read, the other read newer facts and write them, and the first
	// then write what it read, and the registry ends on the older answer.
	// Held across the writes, the later caller always reads last and
	// writes last.
	registering sync.Mutex

	// peopleEvery, peoplePoke, peopleStop and peopleDone are the people's
	// refresh loop: its cadence, the ask an apply makes for an answer now,
	// and its lifetime. onPeople is [TransportOptions.OnPeople].
	peopleEvery time.Duration
	peoplePoke  chan struct{}
	peopleStop  context.CancelFunc
	peopleDone  chan struct{}
	onPeople    func()
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
		people: people{
			want: slices.Clone(opts.Config.People), held: map[string]User{},
			missing: map[string]bool{}, agents: map[string]string{},
			anchor: map[string]string{},
		},
		peopleEvery: opts.PeopleRefresh,
		peoplePoke:  make(chan struct{}, 1),
		onPeople:    opts.OnPeople,
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.peopleEvery <= 0 {
		t.peopleEvery = PeopleRefresh
	}

	fleet, err := NewFleet(FleetOptions{
		Publisher: opts.Publisher, Claims: opts.Claims, Connect: opts.Connect,
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

// Start connects every configured seat — every one, on every node, whatever
// seats this node holds. The fleet claims each post before it publishes it, so
// exactly one node delivers a post however many read it (see the "Every node
// listens" section of fleet.go): a node restarting or losing a seat leaves it
// heard by the rest, with no gap and no handover.
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

	// THE INSTANCE AND EVERY SEAT'S IDENTITY ARE READ TOGETHER, and that
	// is not an optimisation. Each is a call the client retries for its
	// whole budget against a server that is down, so read in sequence an
	// unreachable instance delays boot by that budget times every call:
	// the instance read once per seat's token, then the identities. Read
	// together they cost one budget however many seats there are, and the
	// fleet's own reconnect loop keeps trying afterwards.
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failed   []string
		resolved []identifiedSeat
	)
	fail := func(handle string, err error) {
		mu.Lock()
		failed = append(failed, handle)
		mu.Unlock()
		log.ErrorContext(ctx, "mattermost_seat_failed", "handle", handle,
			"error", err.Error())
	}
	// The server's own typing cadence and Site URL, read ONCE from the
	// first seat whose token the server takes: they are properties of the
	// instance, not of a bot, and reading them per seat would be N
	// identical calls.
	wg.Go(func() { t.readInstance(ctx) })
	for _, cfg := range t.cfg.Seats {
		wg.Go(func() {
			seat, err := t.identify(ctx, cfg.Resolve())
			if err != nil {
				fail(cfg.Handle, err)
				return
			}
			mu.Lock()
			resolved = append(resolved, seat)
			mu.Unlock()
		})
	}
	wg.Wait()
	// THE PEOPLE ARE ASKED ABOUT BEFORE ANY SOCKET ATTACHES, for the
	// reason the instance is read first: the first post a socket hears may
	// be a person's, and asked afterwards it would reach its seat from a
	// stranger. One request, on a token the server has just accepted.
	// None when every seat failed, which the end of this function reports
	// as the one problem it is — asked anyway, it would add a second line
	// blaming the people for it.
	//
	// AND ONLY ONCE EVERY SEAT'S ACCOUNT IS KNOWN, because a person's name
	// that holds one of them is refused rather than registered (people.go),
	// and asked before, it would be judged against no agent at all.
	if len(resolved) > 0 {
		clients := make([]*Client, 0, len(resolved))
		t.mu.Lock()
		for _, seat := range slices.SortedFunc(slices.Values(resolved), func(a, b identifiedSeat) int {
			return strings.Compare(a.seat.Handle, b.seat.Handle)
		}) {
			clients = append(clients, seat.client)
			if seat.seat.UserID != "" {
				t.people.agents[seat.seat.UserID] = seat.seat.Handle
			}
		}
		t.mu.Unlock()
		t.resolvePeople(ctx, clients)
	}
	// THE SOCKETS ATTACH ONLY ONCE THE INSTANCE IS READ, because the first
	// post a socket hears can raise a typing indicator, and that indicator
	// must run at the cadence the server enforces ([Transport.StatusRefresh])
	// — sent faster than the server allows, it is rejected and never shows.
	// Concurrently for the identities' reason.
	for _, seat := range resolved {
		wg.Go(func() {
			if err := t.attach(ctx, seat); err != nil {
				fail(seat.seat.Handle, err)
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
	// AND ONLY A TRANSPORT THAT RUNS KEEPS ASKING. One whose every seat
	// failed holds no token to ask with, and is dropped by its caller
	// without a Stop — a loop started for it would outlive it.
	t.startPeople(ctx)
	return nil
}

// identifiedSeat is a seat whose bot the server has named, with the client
// that asked, before its socket is attached.
type identifiedSeat struct {
	seat   Seat
	client *Client
}

// identify resolves one bot's identity.
func (t *Transport) identify(ctx context.Context, cfg SeatConfig) (identifiedSeat, error) {
	if cfg.Token == "" {
		return identifiedSeat{}, fmt.Errorf("seat %q has no bot token", cfg.Handle)
	}
	c, err := NewClient(ClientOptions{URL: t.cfg.URL, Token: cfg.Token, Now: t.now})
	if err != nil {
		return identifiedSeat{}, err
	}
	// THE IDENTITY IS RESOLVED, never assumed from config. An id the
	// engine guessed disables own-message suppression when it is wrong,
	// and an agent that cannot recognise its own posts answers itself
	// for ever at one turn each.
	me, err := c.Me(ctx)
	if err != nil {
		return identifiedSeat{}, fmt.Errorf("resolving identity: %w", err)
	}
	seat := Seat{Handle: cfg.Handle, Username: me.Username, UserID: me.ID}
	if seat.Username == "" {
		// The server knows better, but if it said nothing the
		// configured name is the only one anybody can address.
		seat.Username = cfg.Username
	}
	return identifiedSeat{seat: seat, client: c}, nil
}

// attach registers an identified seat and attaches its socket.
func (t *Transport) attach(ctx context.Context, s identifiedSeat) error {
	t.mu.Lock()
	t.seats[s.seat.Handle] = runningSeat(s)
	t.mu.Unlock()

	t.register(s.seat)
	return t.fleet.Add(ctx, s.seat, s.client)
}

// register puts this seat's bot into the party registry.
//
// BY USER ID, IN THE BOT NAMESPACE, because the user id is what every payload
// names a poster by — a post's author, a mention list — and the bot namespace
// is where a fellow agent's id lives beside the people's ([notify.BotNamespace]),
// so a fellow agent's message is annotated exactly the way a person's is.
//
// NOT THE USERNAME. It was registered in the transport's own namespace too,
// on the reasoning that a person typing a mention uses it — but no lookup in
// the engine is ever handed a username: a mention reaches a seat through the
// server's own `mentions` list, which carries user ids, and an agent finds a
// colleague through the org. What that namespace holds now is the PEOPLE's
// user ids (see people.go), and a username beside them would be a second kind
// of key in one map, one that Mattermost's own rules let a bot be named so as
// to collide with: a 26-character lowercase username is a valid one.
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
}

// Reregister puts every running seat's identities, and the new org's people,
// into a NEW registry.
//
// An apply builds a fresh registry from the new org, and these identities
// are facts about the SERVER rather than about the config — resolved once at
// connect, against a live instance. Losing them on an apply would make every
// agent's own message annotate as a stranger until something reconnected,
// which is a thing nothing here would ever do on its own.
//
// THE PEOPLE ARE THE ORG'S, and what the server last said about their names
// fills the new registry at once, with no request — so a person recognised a
// moment ago is not a stranger while the names are asked about again, which
// the refresh loop is told to do now: an apply is what names a new person or
// corrects a name. A registry the identity directory rebuilds for the same
// org — a suspension, a reinstatement — comes through here too, and what it
// withholds gets no account (people.go).
//
// AND AN APPLY STATES WHO THEY ARE, which a directory rebuild does not:
// restated says which this is. On an apply each name is anchored to the
// account it holds now, so a name reported as moved stops being reported: the
// company has been stated again with it, which is the operator's answer to
// the report (people.go, "The name is the authority, and a move is said"). A
// rebuild for the same company keeps the anchors, or suspending anybody would
// withdraw a report about somebody else that nobody answered.
func (t *Transport) Reregister(reg *notify.Registry, people []Person, restated bool) {
	if reg == nil {
		return
	}
	t.mu.Lock()
	seats := make([]Seat, 0, len(t.seats))
	for _, s := range t.seats {
		seats = append(seats, s.seat)
	}
	t.registry = func() *notify.Registry { return reg }
	t.people.want = slices.Clone(people)
	if restated {
		for _, s := range t.people.standings(reg) {
			if s.id != "" {
				t.people.anchor[s.Username] = s.id
			}
		}
	}
	t.mu.Unlock()

	for _, seat := range seats {
		t.register(seat)
	}
	t.publishPeople()
	t.pokePeople()
}

// readInstance reads the facts that belong to the server rather than to a
// bot: the typing cadence it enforces, and the Site URL it believes it is
// served at — judged by [JudgeSiteURL].
//
// THE NEXT SEAT'S TOKEN IS TRIED ONLY WHEN THE SERVER REFUSED THIS ONE'S
// ([refusesToken]), because only that is a fact about a token. Every other
// failure — nothing listening, a timeout, a server error — is a fact about
// the SERVER, which the next token would only meet again after the client
// had spent its whole retry budget once more: a seven-seat company booting
// against an instance that was down spent seven budgets, a minute, here
// before a seat had even begun to connect. What is read is best effort
// either way, and the defaults stand without it.
func (t *Transport) readInstance(ctx context.Context) {
	for _, cfg := range t.cfg.Seats {
		if cfg.Token == "" {
			continue
		}
		c, err := NewClient(ClientOptions{URL: t.cfg.URL, Token: cfg.Token, Now: t.now})
		if err != nil {
			// The instance's URL, which every token shares.
			return
		}
		conf, err := c.ClientConfig(ctx)
		if refusesToken(err) {
			continue
		}
		if err != nil {
			log.WarnContext(ctx, "mattermost_instance_unread", "error", err.Error(),
				"detail", "the server's typing cadence and Site URL could not be "+
					"read, so indicators run at Mattermost's default cadence and a "+
					"Site URL the server cannot use goes unreported until the next "+
					"start")
			return
		}
		t.mu.Lock()
		t.throttle = TypingThrottle(conf)
		t.mu.Unlock()

		// WHAT THE SERVER ITSELF DOES WITH ITS SITE URL, judged by the
		// one function the doctor prints from ([JudgeSiteURL]) — never by
		// comparing it with the address this engine dials. People browse
		// the Site URL, not the engine's address, and the engine's own
		// sockets carry no Origin, so the origin check never applies to
		// them: an in-cluster Service dialled while people browse a
		// public name is the ordinary deployment, and a warning that it
		// blinds browsers fired on every one of them.
		reported := SiteURL(conf)
		if fault, ok := JudgeSiteURL(reported); ok {
			log.WarnContext(ctx, fault.Event,
				"url", t.cfg.URL, "reported", reported, "detail", fault.Detail)
		}
		return
	}
}

// refusesToken reports a failure that is the server refusing one bot's token
// rather than the server failing: an unauthenticated or forbidden answer.
func refusesToken(err error) bool {
	switch Status(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return false
}

// Stop disconnects everything.
func (t *Transport) Stop(ctx context.Context) {
	t.stopPeople()
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
	var rootSeen bool
	for _, post := range posts {
		// THE ROOT IS KEPT WHATEVER IT SAYS, recognised by its own id
		// rather than by its position. [notify.ThreadReader] promises the
		// root FIRST, and dropping it for want of a body hands the
		// renderer a transcript whose first line is the oldest surviving
		// REPLY — which every bound then protects and the prompt then
		// calls the thread's opening.
		//
		// It travels with an EMPTY body rather than an invented one: what
		// to say in its place is the renderer's decision, and this
		// transport rendering prose would put one backend's wording in
		// front of a seat and the other's beside it.
		isRoot := post.ID != "" && post.ID == root
		bookkeeping := post.Bookkeeping() != ""
		if bookkeeping && !isRoot {
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
		body := ""
		if !bookkeeping {
			body = post.Body()
		}
		if body == "" && !isRoot {
			continue
		}
		rootSeen = rootSeen || isRoot
		out.Messages = append(out.Messages, notify.Message{
			SenderID: post.UserID,
			// THE POST ID, which is what the trigger's `ts` is on this
			// backend — see [notify.Message.ID].
			ID: post.ID,
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
	if len(out.Messages) > 0 && !rootSeen {
		// A DELETED ROOT, which is the ordinary way to get here: the
		// thread endpoint leaves a deleted post out of its answer while
		// every reply to it stays, so the read comes back with no root at
		// all. Its slot is kept empty rather than closed up, because
		// "somebody deleted the opening" and "this thread opens with the
		// oldest line below" are different threads to a seat.
		out.Messages = append([]notify.Message{{}}, out.Messages...)
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

// AddressRule implements [notify.StatusPoster].
//
// THE PROMPT'S OWN RULE, not a second one: the indicator raises on exactly
// the messages the prompt tells the agent it owes an answer to. It carries
// no channel-id prefix, because Mattermost ids are opaque 26-character
// alphanumerics and a prefix test would mark arbitrary public channels as
// direct messages — raising an indicator, and demanding a reply, for traffic
// nobody addressed to this agent.
func (t *Transport) AddressRule() notify.AddressRule { return AddressRule() }

// SetStatus implements [notify.StatusPoster] by raising the typing indicator.
//
// The status text is ignored, and that is what SupportsStatusText declares:
// there is nothing here to render it with.
//
// REACHED ONCE PER TURN AND THEN EVERY FEW SECONDS. The engine raises the
// indicator at the start of every chat-triggered turn and the session's
// heartbeat re-asserts it at the interval this instance itself declares (see
// [Transport.StatusRefresh]), so on this backend it is the most frequent call
// the transport makes — which is what `typing_status: addressed` exists to
// bound, and why a failure here is a DEBUG line rather than anything louder.
func (t *Transport) SetStatus(ctx context.Context, post notify.StatusPost) bool {
	t.mu.Lock()
	s, ok := t.seats[post.Handle]
	t.mu.Unlock()
	if !ok {
		return false
	}
	if err := s.client.Typing(ctx, s.seat.UserID, post.Channel, post.Thread); err != nil {
		log.DebugContext(ctx, "mattermost_typing_failed", "handle", post.Handle,
			"error", err.Error())
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

// seatToken is the bot token a seat's transport authenticates with — and
// therefore the one [SeatTokens] checks. Empty when the seat names a variable
// that is unset.
func seatToken(role *org.Role, lookup org.EnvLookup) string {
	return envref.Resolve(role.Mattermost.BotToken, lookup)
}

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
		token := seatToken(role, lookup)
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
