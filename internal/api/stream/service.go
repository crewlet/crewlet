package stream

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/tokens"
)

// HealthInterval is how often the shared tick broadcasts.
//
// ONE timer for the whole service, not one per connection. What it keeps
// honest — the in-flight pill, the drain state, the live dot — is the same
// answer for every tab, so a timer per client would multiply identical work by
// the number of tabs open and make the load depend on how many people happened
// to be watching.
//
// Five seconds: fast enough that a drain or a stall shows up while an operator
// is still looking at it, slow enough that an idle company is not sending a
// frame a second to every tab. It is also the interval a dashboard's staleness
// rule is sized against — a live dot that has not been refreshed in two ticks
// is a socket that is gone, not a company that is quiet.
const HealthInterval = 5 * time.Second

// Health is what the shared tick carries.
//
// InFlight and ShuttingDown are always present, and a zero is a real zero: the
// API is served beside the engine in every node that serves it, so both are
// always known.
type Health struct {
	Status       string `json:"status"`
	InFlight     int    `json:"in_flight"`
	ShuttingDown bool   `json:"shutting_down"`
}

// HealthFunc reports the current health, for the shared tick.
type HealthFunc func() Health

// PostureFunc maps this node's own health onto the posture its sockets are
// served in.
//
// IT TAKES THE HEALTH IT IS DECIDED FROM rather than reading the node again:
// the health read reaches the coordination plane, the tick already does one
// per interval, and a second read would describe a different instant — so a
// tab could be told "shed" in its health frame while its pushes kept arriving
// as though the node were serving.
//
// The MAPPING lives with the caller (internal/api), not here, because the set
// of node postures that take a node out of service is already declared there
// and read by /ready. Two declarations of one set is how the probe and the
// socket come to disagree about whether this node is serving.
type PostureFunc func(Health) FramePosture

// Service turns the engine's event stream into the pushes a dashboard mirrors.
//
// It is the only thing that applies an event to the projection AND fans out the
// result. A dashboard renders what arrives here; it does not re-derive it from
// the raw event stream. Every tab used to do that — three private copies of the
// projection, each drifting its own way.
type Service struct {
	hub   *Hub
	state *livestate.LiveState

	// budgets is the in-flight query allowance, one per PRINCIPAL rather
	// than one per socket. It lives here because the service outlives any
	// one connection, which is what lets a person's tabs share a budget
	// at all — see budget.go.
	budgets *budgets

	// health is consulted by the shared tick.
	health HealthFunc

	// posture derives a socket's delivery mode from that health. See
	// [PostureFunc].
	posture PostureFunc

	// handles maps a role to its agent handle, for the per-agent rollup's
	// cross-links. It answers an empty map while no company is active,
	// which leaves them blank.
	handles HandleFunc

	// roster, org and tools are the config-derived surfaces. See Options.
	roster    func() []map[string]any
	org       func() any
	tools     func() []map[string]any
	schedules func() any

	now      func() time.Time
	interval time.Duration

	// chart decides a `watch` frame. See [Options.Chart].
	chart authz.Chart

	// revalidateEvery is how often an open socket's credential is checked
	// again. See [Options.RevalidateEvery].
	revalidateEvery time.Duration

	// tokensDirty means a phase completed since the last rollup went out.
	// Set on the publish path and cleared on the tick — see flushTokens.
	tokensDirty atomic.Bool

	mu      sync.Mutex
	ticking bool
	stop    chan struct{}
	done    chan struct{}
}

// HandleFunc answers the role-to-handle map the per-agent rollup links with.
type HandleFunc func() map[string]string

// Options configure a service.
//
// Health, Posture, Handles, Roster, Org, Tools, Schedules and Chart are
// REQUIRED, and [NewService] refuses a missing one by name. Each is something
// the engine beside the API always answers, so a missing one is a wiring
// mistake, and serving around it would push a confident answer where there is
// none: a health frame reading "ok", an empty catalogue, an organization with
// no seats, a lead told they may not watch their own report.
type Options struct {
	Health HealthFunc

	// Posture decides whether a socket is served live or degraded, from
	// the same Health the tick just read. See [PostureFunc].
	Posture PostureFunc

	// Handles supplies the role-to-handle map. An empty map leaves each
	// row's handle blank rather than guessing one: a wrong link is worse
	// than no link.
	Handles HandleFunc

	// Roster, Org and Tools are the three surfaces the dashboard renders
	// from CONFIGURATION rather than from anything that has happened.
	//
	// The projection cannot answer any of them: it holds what a seat is
	// DOING, so it can say a seat is mid-phase and not that the seat
	// exists. Snapshot used to ask it for the agent list anyway, merging
	// the live overlay onto a static roster of nil — an empty list, every
	// connect, for ever on a company whose model was not answering.
	//
	// Functions, not values, for the same reason Handles is one: an apply
	// replaces the company, and a roster captured at boot would keep
	// showing a role a revision deleted.
	//
	// Org answers `any` because its shape is an explicit public type owned
	// by package api (the anonymous org projection), and api imports this
	// package, so naming the type here would be an import cycle. This
	// service only carries the value to the wire and never reads into it.
	Roster    func() []map[string]any
	Org       func() any
	Tools     func() []map[string]any
	Schedules func() any

	// Now is injectable so a test can pin the timestamps envelopes carry.
	Now func() time.Time

	// HealthInterval overrides the shared tick's cadence. Zero takes
	// [HealthInterval], which is the measured production value.
	//
	// Injectable for the same reason Now is: a suite that had to wait out
	// five seconds to see one tick would either be slow or would assert
	// nothing about the tick at all — and the property worth asserting is
	// that there is exactly ONE of them however many times it is started.
	HealthInterval time.Duration

	// RevalidateEvery overrides how often an open socket's credential is
	// checked again. Zero takes [RevalidateEvery], which is the production
	// value and is tied to the stall grace — see revalidate.go.
	//
	// Injectable for HealthInterval's reason: a revocation case that had
	// to wait out a minute per assertion would be a case nobody runs.
	RevalidateEvery time.Duration

	// Chart answers who leads whom, which is what a `watch` frame is
	// decided by — see [watching]. REQUIRED: a watch installs routing for
	// a seat's inbox, and the question it asks is the one the `work_inbox`
	// query asks of the same seat, through the same seam. A service built
	// without one would answer every lead's watch of a report as
	// "undecidable" for the life of the process.
	Chart authz.Chart
}

// NewService builds the fan-out over a projection, or refuses a missing
// required function by name. See [Options].
func NewService(state *livestate.LiveState, opts Options) (*Service, error) {
	var missing []string
	for _, field := range []struct {
		name   string
		absent bool
	}{
		{"Health", opts.Health == nil},
		{"Posture", opts.Posture == nil},
		{"Handles", opts.Handles == nil},
		{"Roster", opts.Roster == nil},
		{"Org", opts.Org == nil},
		{"Tools", opts.Tools == nil},
		{"Schedules", opts.Schedules == nil},
		{"Chart", opts.Chart == nil},
	} {
		if field.absent {
			missing = append(missing, "Options."+field.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("stream: %s required: the live channel is served "+
			"beside the engine, which answers every one of them",
			strings.Join(missing, ", "))
	}
	s := &Service{
		hub:       NewHub(),
		budgets:   newBudgets(),
		state:     state,
		health:    opts.Health,
		posture:   opts.Posture,
		handles:   opts.Handles,
		roster:    opts.Roster,
		org:       opts.Org,
		tools:     opts.Tools,
		schedules: opts.Schedules,
		chart:     opts.Chart,
		now:       opts.Now,
		interval:  opts.HealthInterval,

		revalidateEvery: opts.RevalidateEvery,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.interval <= 0 {
		s.interval = HealthInterval
	}
	if s.revalidateEvery <= 0 {
		s.revalidateEvery = RevalidateEvery
	}
	return s, nil
}

// Hub exposes the client registry, for a transport to join and leave.
func (s *Service) Hub() *Hub { return s.hub }

// Join registers a client at the posture this node is serving RIGHT NOW.
//
// The tick refreshes the hub's posture every interval, which is what moves
// every open socket when a node sheds. That leaves one window a tick cannot
// cover: a tab that connects between two ticks, to a node that changed posture
// since the last one. So a connect reads the posture once, for itself — a
// coordination round trip per connect, which is what the health body already
// costs on every probe, and orders of magnitude rarer than a push.
func (s *Service) Join(c *Client) {
	s.hub.SetPosture(s.posture(s.currentHealth()))
	s.hub.Register(c)
}

// State exposes the projection, for the snapshot and the query surface.
func (s *Service) State() *livestate.LiveState { return s.state }

// Ingest applies one event and pushes what it moved.
//
// THE RESULT, NOT THE EVENT — for the derived kinds. The raw event still goes
// out as `event`, because the activity feed is a list of events and nothing
// derives it; everything else a dashboard shows is a projection, and shipping
// the projection is what stops each tab keeping its own.
func (s *Service) Ingest(env livestate.Envelope) {
	// A pointer, so the `failed` mark Apply derives is on the frame that
	// goes out below. Without it the live row and the snapshot's own row
	// disagreed about the same event: a turn that failed while somebody was
	// watching rendered exactly like one that succeeded, and only grew its
	// failure mark on the next reload.
	change := s.state.Apply(&env)
	now := s.now()

	// The event first. A client dedupes by event id, so a feed row that
	// also appears in a snapshot is harmless — but a derived push arriving
	// before the event that caused it would briefly show a consequence
	// with no cause in the feed beside it.
	if change.Events {
		s.hub.Broadcast(Push(KindEvent, env, now))
	}
	if len(change.Agents) > 0 {
		// Sorted, so a push carrying two seats is byte-stable across
		// runs — Go map iteration is randomised, and a frame whose row
		// order changes for no reason makes a diff of two captures
		// unreadable.
		roles := slices.Sorted(maps.Keys(change.Agents))
		if rows := s.state.OverlayRows(roles); len(rows) > 0 {
			s.hub.Broadcast(Push(KindAgents, rows, now))
		}
	}
	if change.Sandboxes {
		s.hub.Broadcast(Push(KindSandboxes, s.state.ActiveSandboxes(), now))
	}
	if change.Tokens {
		// MARKED, NOT SENT. Aggregating here would run inside the
		// caller's publish, which is the engine's own goroutine, mid-turn,
		// between a model's answer and its tools.
		// The shared tick owns the fold, so a busy company's rollup costs
		// one aggregation every five seconds rather than one per phase.
		s.tokensDirty.Store(true)
	}
	if change.Budget {
		s.hub.Broadcast(Push(KindBudget, s.state.Budget(), now))
	}
}

// Snapshot is the state a client receives the instant it connects.
//
// Built entirely from the in-memory projection — no database round trip on
// connect. That is the whole reason the projection exists: a dashboard that
// rebuilt agent history from the store on every reconnect would take a
// thirty-day scan per tab, and would lose any call mid-flight while it did.
//
// It is the snapshot FOR ONE AUDIENCE, and carries only the keys whose push
// that audience may receive (see [needs]).
func (s *Service) Snapshot(audience Audience) map[string]any {
	full := map[string]any{
		"health": s.currentHealth(),
		// THE STATIC ROSTER FIRST, with the live overlay merged onto it.
		// MergeAgents walks what it is GIVEN, so passing nil here — which
		// it did — produced an empty list whatever the projection held.
		"agents": s.state.MergeAgents(s.currentRoster()),
		"org":    s.currentOrg(),
		"tools":  s.currentTools(),
		// THE CONFIGURED ROWS, not the dispatch ledger. The screen renders
		// its table from this slice and fetches the ledger itself, so
		// without it the table stayed on its skeleton for ever — the
		// client reads null as "not here yet", which was permanently
		// true.
		"schedules": s.currentSchedules(),
		"events":    s.state.RecentEvents(livestate.EventFeedLimit),
		"sandboxes": s.state.ActiveSandboxes(),
		"tokens":    s.TokenRollup(),
		"budget":    s.state.Budget(),
	}
	// EACH KEY IS WHAT ITS PUSH WOULD CARRY, so a reader receives in the
	// handshake exactly what it would receive afterwards: an audience
	// without `audit:read` gets no `events` key rather than an empty one,
	// which the client reads as "nothing here yet" rather than as "the
	// company has done nothing".
	out := make(map[string]any, len(full))
	for key, value := range full {
		if audience.Receives(snapshotKinds[key]) {
			out[key] = value
		}
	}
	return out
}

// snapshotKinds is the push kind each snapshot key stands in for.
//
// Total over the keys [Service.Snapshot] builds, which
// TestEverySnapshotKeyIsAPushKind holds: a key with no kind here is received
// by nobody, which is the closed end, and would read as a screen that never
// loads.
var snapshotKinds = map[string]string{
	"health":    KindHealth,
	"agents":    KindAgents,
	"org":       KindOrg,
	"tools":     KindTools,
	"schedules": KindSchedules,
	"events":    KindEvent,
	"sandboxes": KindSandboxes,
	"tokens":    KindTokens,
	"budget":    KindBudget,
}

// Roster is the company's seat list as the `seats` push carries it.
//
// Exported because a config apply has to re-send it: the client's own doc
// says a merge cannot express a deletion, so a revision that removed a role
// would leave its card on screen until someone reloaded the page.
func (s *Service) Roster() []map[string]any { return s.state.MergeAgents(s.currentRoster()) }

// Org is the company's role and unit tree, for the same re-send.
func (s *Service) Org() any { return s.currentOrg() }

// Tools is this node's catalogue, for the same re-send. It changes on an
// apply too — a revision that adds an MCP server adds its tools.
func (s *Service) Tools() []map[string]any { return s.currentTools() }

// Schedules is the configured rows as the `schedules` push carries them.
//
// Wrapped in the push's own object shape rather than sent bare: the client
// assigns each key only when present, so omitting one LEAVES what it already
// holds rather than blanking it. The RUN LEDGER is deliberately not one of
// them — "how it last went" is answered by the `schedules` query, which the
// screen polls, and sending a second, staler copy of it here would give one
// screen two sources for one fact.
func (s *Service) Schedules() map[string]any {
	return map[string]any{"schedules": s.currentSchedules()}
}

func (s *Service) currentRoster() []map[string]any { return s.roster() }

func (s *Service) currentOrg() any { return s.org() }

func (s *Service) currentSchedules() any { return s.schedules() }

func (s *Service) currentTools() []map[string]any { return s.tools() }

// CompanyPublished re-sends every push a published company changes — the
// roster, the org tree, the tool catalogue and the schedules — to every client
// whose audience receives it.
//
// WHOLE PAYLOADS, each replacing its predecessor: none of these is ever
// corrected by an event, and an overlay merge cannot express a deletion, so a
// screen that got a delta and lost it to backpressure would render a removed
// seat until it reloaded.
//
// ONE METHOD NAMING ITS OWN KINDS, where there used to be a Broadcast(kind,
// data) whose one caller spelled all four kinds as string literals in another
// package: a constant renamed here would have compiled cleanly there and sent a
// kind the dashboard's dispatch has no case for.
func (s *Service) CompanyPublished() {
	now := s.now()
	for _, push := range []struct {
		kind string
		data any
	}{
		{KindSeats, s.Roster()},
		{KindOrg, s.Org()},
		{KindTools, s.Tools()},
		{KindSchedules, s.Schedules()},
	} {
		s.hub.Broadcast(Push(push.kind, push.data, now))
	}
}

// InboxChange is the payload of an `inbox_changed` frame: whose inbox moved,
// by how many notices, and the subject and reason of the newest.
//
// IDENTIFIERS AND A COUNT, NEVER CONTENT. The frame reaches whoever watches
// the seat, and what it is for is telling that screen to re-read the inbox
// through the `work_inbox` question — the same question, decided by the same
// authority, as the poll it makes immediate. An excerpt here would be the
// inbox's content read around that authority. The count is a HINT: the rows
// are the truth, and a screen re-reads them rather than adding these up.
type InboxChange struct {
	Handle      string `json:"handle"`
	UnreadDelta int    `json:"unread_delta"`
	Subject     string `json:"subject"`
	Reason      string `json:"reason"`
}

// InboxChanged pushes one seat's inbox movement to the clients watching that
// seat and to nobody else — see [KindInboxChanged].
//
// NEVER BLOCKS, for [Hub.Broadcast]'s reason, and its caller is why that
// matters: this is called from the tracker applier's post-commit half, on the
// apply loop's own goroutine, with the next batch waiting behind it.
func (s *Service) InboxChanged(change InboxChange) {
	if change.Handle == "" {
		// NO SEAT, NO AUDIENCE: a seat-routed frame with no address is
		// refused by the hub, loudly. A movement for nobody is not one.
		return
	}
	s.hub.Broadcast(PushSeat(KindInboxChanged, change.Handle, change, s.now()))
}

func (s *Service) currentHealth() Health { return s.health() }

// StartHealthTicks runs the shared tick until the context is cancelled or
// [Service.Stop] is called.
//
// Idempotent: a second call while one is running is a no-op rather than a
// second timer, which would push every health frame twice and leave a goroutine
// that [Service.Stop] never joins.
func (s *Service) StartHealthTicks(ctx context.Context) {
	s.mu.Lock()
	if s.ticking {
		s.mu.Unlock()
		return
	}
	s.ticking = true
	stop, done := make(chan struct{}), make(chan struct{})
	s.stop, s.done = stop, done
	s.mu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				// ONE HEALTH READ PER TICK, feeding both the frame
				// and the posture derived from it. Read twice, a tab
				// could be told "shed" in the frame it is handed
				// while the posture that decided whether to hand it
				// anything came from a different instant.
				health := s.currentHealth()
				s.hub.SetPosture(s.posture(health))
				s.hub.Broadcast(Push(KindHealth, health, s.now()))
				s.flushTokens()
			}
		}
	}()
}

// flushTokens sends the spend rollup if a phase completed since the last one.
//
// The flag is cleared only AFTER the fold, so an aggregation that panicked
// would not consume the burst it failed on and leave the rollup stale until
// the next phase completed.
func (s *Service) flushTokens() {
	if !s.tokensDirty.Load() {
		return
	}
	rollup := s.TokenRollup()
	s.tokensDirty.Store(false)
	s.hub.Broadcast(Push(KindTokens, rollup, s.now()))
}

// TokenRollup folds the live window into the breakdown the dashboard renders.
//
// Exported because the snapshot needs the same answer: a client that connected
// mid-window and one that has been receiving pushes must hold the same rollup,
// and two constructions of it is how they come to differ.
func (s *Service) TokenRollup() tokens.Rollup {
	// The window this rollup actually covers, reported rather than assumed:
	// the client prints it beside the numbers, and a figure labelled with
	// the wrong window is worse than an unlabelled one. The projection
	// evicts on a rolling window, so its top edge is this instant.
	now := time.Now()
	return tokens.Aggregate(s.state.SpendRecords(), tokens.Options{
		Handles: s.handles(),
		Since:   now.Add(-livestate.LiveSpendWindow),
		Until:   now,
	})
}

// Stop ends the health tick and disconnects every client.
func (s *Service) Stop() {
	s.mu.Lock()
	stop, done, ticking := s.stop, s.done, s.ticking
	s.ticking = false
	s.stop, s.done = nil, nil
	s.mu.Unlock()

	if ticking {
		close(stop)
		<-done
	}
	s.hub.Close()
}
