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

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/livestate"
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

// HealthFunc reports the current health, for the shared tick and the snapshot.
//
// THE WHOLE BODY, which is `api.Health` — returned as `any` for the reason
// [Options.Org] is: its shape is an explicit public type owned by package api,
// which imports this one, so naming it here would be an import cycle, and this
// service carries it to the wire without reading into it. The push used to be a
// three-field type declared HERE, which is how a node refusing every inbound
// webhook for want of a configuration pushed a frame identical to a healthy idle
// one's, and how five screens came to poll a query for the rest of the body.
type HealthFunc func() any

// Service turns the engine's event stream into the pushes a dashboard mirrors.
//
// It is the only thing that applies an event to the projection AND fans out the
// result. A dashboard renders what arrives here; it does not re-derive it from
// the raw event stream. Every tab used to do that — three private copies of the
// projection, each drifting its own way.
type Service struct {
	hub   *Hub
	state *livestate.LiveState

	// health is consulted by the shared tick.
	health HealthFunc

	// handles maps a role to its agent handle, for the per-agent rollup's
	// cross-links. It answers an empty map while no company is active,
	// which leaves them blank.
	handles HandleFunc

	// rosterRows, org, tools and schedules are the config-derived
	// surfaces. See Options.
	rosterRows func() []map[string]any
	org        func(auth.Reach) any
	tools      func() []map[string]any
	schedules  func() any

	// placement reads the seat leases for the seat-state vocabulary's
	// `unplaced`. See Options.Placement.
	placement PlacementFunc

	// placementFailing is whether the last placement read failed, so a
	// lease table that stays unreadable is said once rather than on every
	// tick.
	placementFailing atomic.Bool

	now      func() time.Time
	interval time.Duration

	// tokensDirty means a phase completed since the last rollup went out.
	// Set on the publish path and cleared on the tick — see flushTokens.
	tokensDirty atomic.Bool

	// pushMu orders the `agents` pushes, and pushed is what the last of them
	// said about each seat's live call: which call, and the version of each
	// heavy field it described. See pushAgents.
	pushMu sync.Mutex
	pushed map[string]pushedCall

	mu      sync.Mutex
	ticking bool
	stop    chan struct{}
	done    chan struct{}
}

// HandleFunc answers the role-to-handle map the per-agent rollup links with.
type HandleFunc func() map[string]string

// PlacementFunc reads which of the company's agent seats some node in the fleet
// holds, keyed by handle: every agent seat in the company, true where a node
// holds it. An error is a read that did not happen, never "none are held".
type PlacementFunc func() (map[string]bool, error)

// Options configure a service.
//
// Health, Handles, Roster, Org, Tools and Schedules are REQUIRED, and
// [NewService] refuses a missing one by name. Each is something the engine
// beside the API always answers, so a missing one is a wiring mistake, and
// serving around it would push a confident answer where there is none: a
// health frame reading "ok", an empty catalogue, an organization with no seats.
type Options struct {
	Health HealthFunc

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
	// by package api (the classified org projection), and api imports this
	// package, so naming the type here would be an import cycle. This
	// service only carries the value to the wire and never reads into it.
	// It is asked FOR A REACH and answers what that reach is served of the
	// chart: the projection classifies every field it carries (ADR-0031),
	// and this service sends each socket the variant its own reach is
	// handed — never one built for a richer reach.
	//
	// Roster is the static rows, whatever the reach: what a reach is served
	// of a seat's row is decided here, where the live state is merged onto
	// it ([Service.Roster]).
	Roster    func() []map[string]any
	Org       func(auth.Reach) any
	Tools     func() []map[string]any
	Schedules func() any

	// Placement is which of the company's agent seats some node in the
	// FLEET holds — the seat leases, never this node's own seats — which
	// the seat-state vocabulary needs for `stopped`/`unplaced`, and which
	// no event reports: a seat moves between nodes on a lease, not on
	// anything published. So it is READ, on every snapshot and roster
	// answer and on the shared tick, and the seats whose state it moved are
	// pushed.
	Placement PlacementFunc

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
		{"Handles", opts.Handles == nil},
		{"Roster", opts.Roster == nil},
		{"Org", opts.Org == nil},
		{"Tools", opts.Tools == nil},
		{"Schedules", opts.Schedules == nil},
		{"Placement", opts.Placement == nil},
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
		hub:        NewHub(),
		state:      state,
		health:     opts.Health,
		handles:    opts.Handles,
		rosterRows: opts.Roster,
		org:        opts.Org,
		tools:      opts.Tools,
		schedules:  opts.Schedules,
		placement:  opts.Placement,
		now:        opts.Now,
		interval:   opts.HealthInterval,
		pushed:     map[string]pushedCall{},
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.interval <= 0 {
		s.interval = HealthInterval
	}
	return s, nil
}

// Hub exposes the client registry, for a transport to join and leave.
func (s *Service) Hub() *Hub { return s.hub }

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
		s.hub.Broadcast(KindEvent, Same(env), now)
	}
	s.pushAgents(change, now)
	if change.Sandboxes {
		s.pushSandboxes(now)
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
		s.hub.Broadcast(KindBudget, func(reach auth.Reach) (any, bool) {
			return s.budget(reach), true
		}, now)
	}
}

// pushSandboxes sends the run set, built only if a client is sent it.
func (s *Service) pushSandboxes(now time.Time) {
	s.hub.Broadcast(KindSandboxes, Shared(func() any { return s.state.ActiveSandboxes() }), now)
}

// budget is the live meter as reach is served it: an admin's meters, figures
// and all, or a member's [livestate.BudgetStatus] — which windows are refusing
// or near and when each turns over, and no figure, because what a window spent
// is what the machine processed (ADR-0031).
func (s *Service) budget(reach auth.Reach) any {
	if reach.Covers(auth.ReachAdmin) {
		return s.state.Budget()
	}
	return s.state.BudgetStatus()
}

// ReconcileSandboxes lands a read of the durable run record on the projection
// and pushes the sandbox set when it moved — which is what corrects every open
// panel when an event that would have was lost.
//
// AND THE SEATS WHOSE STATE IT MOVED: a seat's runs are read from this record,
// so a question found here makes its seat need somebody on every open screen at
// the same moment the panel shows it.
func (s *Service) ReconcileSandboxes(records []livestate.SandboxRecord, asOf time.Time) {
	change := s.state.ReconcileSandboxes(records, asOf)
	now := s.now()
	if change.Sandboxes {
		s.pushSandboxes(now)
	}
	s.pushAgents(change, now)
}

// pushAgents sends the rows of the seats a change moved.
//
// A LIVE CALL'S HEAVY FIELDS GO ONLY WHEN THEY MOVED. A running phase moves
// its seat's row five times a second while a round streams, and the prompt,
// the committed rounds' narration, the tool calls and the rounds' timing move
// once a round or once a call at most — so each push carries those only when
// their version moved since the last push for the same call, and names every
// version always (see livestate's detail.go for why a version and not a
// delta: the hub drops a slow client's oldest envelope, and a tab must know
// from the next push that it missed one).
//
// ONE PUSH AT A TIME, under pushMu, from reading the rows to queueing the
// frame: what is left out is decided against what the previous push said,
// so two pushes built side by side could each leave out a field the other
// was the one to carry. Broadcast never blocks, so nothing waits long here.
//
// TWO VARIANTS, and the heavy-field bookkeeping is the admin one's alone. A
// member is pushed each seat's [livestate.AgentStatus] — its state, the item it
// is on and its step, and nothing of the call — so there is nothing to leave
// out. The admin rows are built only while an admin's socket is connected, and
// a gap in which none was leaves what the last admin push said behind: the
// next one then sends more than it needed to, never less, because a version
// only moves forward and an admin who connected in the gap holds every field
// at the version its snapshot was taken at.
func (s *Service) pushAgents(change livestate.Change, now time.Time) {
	if len(change.Agents) == 0 {
		return
	}
	// Sorted, so a push carrying two seats is byte-stable across runs — Go
	// map iteration is randomised, and a frame whose row order changes for
	// no reason makes a diff of two captures unreadable.
	handles := slices.Sorted(maps.Keys(change.Agents))
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	s.hub.Broadcast(KindAgents, func(reach auth.Reach) (any, bool) {
		if !reach.Covers(auth.ReachAdmin) {
			statuses := s.state.Statuses(handles)
			return statuses, len(statuses) > 0
		}
		rows := s.state.OverlayRows(handles)
		for _, row := range rows {
			s.leaveOutWhatIsHeld(row)
		}
		return rows, len(rows) > 0
	}, now)
}

// pushedCall is what an `agents` push said about one seat's live call.
type pushedCall struct {
	key      livestate.CallKey
	versions livestate.CallVersions
}

// leaveOutWhatIsHeld replaces a row's live call with the copy a push carries:
// without the heavy fields the last push for the same call already carried at
// the same version. A call the last push did not describe — a new phase, a
// seat's first — goes whole.
//
// Called under pushMu.
func (s *Service) leaveOutWhatIsHeld(row map[string]any) {
	handle, _ := row["handle"].(string)
	call, _ := row["live_call"].(*livestate.LiveCall)
	if call == nil {
		delete(s.pushed, handle)
		return
	}
	last, ok := s.pushed[handle]
	s.pushed[handle] = pushedCall{key: call.Key(), versions: call.Versions}
	if ok && last.key == call.Key() {
		row["live_call"] = call.Without(last.versions)
	}
}

// RefreshPlacement reads the seat leases onto the projection and pushes the
// seats whose state that moved.
//
// A READ THAT FAILED CHANGES NOTHING: the projection keeps the placement it
// last read rather than reading an unreachable lease table as "no node holds
// anything", which would stop every seat on every screen over a store blip.
func (s *Service) RefreshPlacement() {
	placed, err := s.placement()
	if err != nil {
		if !s.placementFailing.Swap(true) {
			log.Warn("stream_placement_unread", "error", err,
				"hint", "which seats no node holds is read from the seat leases; "+
					"each seat keeps the placement last read until a read succeeds")
		}
		return
	}
	s.placementFailing.Store(false)
	s.pushAgents(s.state.SetPlacement(placed), s.now())
}

// section is one key of the handshake snapshot: the push kind that keeps it
// current, whose reach decides who is handed it, and its content for a reach.
type section struct {
	key   string
	kind  Kind
	build func(reach auth.Reach) any
}

// sections is the snapshot, key by key. Each key is gated by the push that
// keeps it current, so a tab is never handed on connect what it would not be
// pushed a moment later — and the reach is stated once, in [kinds].
func (s *Service) sections() []section {
	return []section{
		{"health", KindHealth, func(auth.Reach) any { return s.currentHealth() }},
		// THE STATIC ROSTER FIRST, with the live state merged onto it.
		// The merge walks what it is GIVEN, so passing nil here — which it
		// did — produced an empty list whatever the projection held.
		{"agents", KindSeats, s.roster},
		{"org", KindOrg, s.currentOrg},
		{"tools", KindTools, func(auth.Reach) any { return s.currentTools() }},
		// THE CONFIGURED ROWS, not the dispatch ledger. The screen renders
		// its table from this slice and fetches the ledger itself, so
		// without it the table stayed on its skeleton for ever — the
		// client reads null as "not here yet", which was permanently true.
		{"schedules", KindSchedules, func(auth.Reach) any { return s.currentSchedules() }},
		{"events", KindEvent, func(auth.Reach) any { return s.state.RecentEvents(livestate.EventFeedLimit) }},
		{"sandboxes", KindSandboxes, func(auth.Reach) any { return s.state.ActiveSandboxes() }},
		{"tokens", KindTokens, func(auth.Reach) any { return s.TokenRollup() }},
		{"budget", KindBudget, s.budget},
	}
}

// Snapshot is the state a client holding reach receives the instant it
// connects: every section whose push reaches it, each in its reach's variant,
// and no key at all for one that does not — absent, never null, because a
// client reads null as "not here yet" and would wait for a push that is never
// coming.
//
// Built entirely from the in-memory projection — no database round trip on
// connect. That is the whole reason the projection exists: a dashboard that
// rebuilt agent history from the store on every reconnect would take a
// thirty-day scan per tab, and would lose any call mid-flight while it did.
//
// The one read it makes is the seat leases ([Service.RefreshPlacement]),
// because no event says which seats a node holds and a connecting tab must not
// be handed a placement up to a tick old — and only for a reach that is served
// the roster, so a socket anybody may open costs no lease read.
func (s *Service) Snapshot(reach auth.Reach) map[string]any {
	if need, _ := KindReach(KindSeats); reach.Covers(need) {
		s.RefreshPlacement()
	}
	out := map[string]any{}
	for _, sec := range s.sections() {
		if need, _ := KindReach(sec.kind); reach.Covers(need) {
			out[sec.key] = sec.build(reach)
		}
	}
	return out
}

// Roster is the company's seat list as reach is served it — the rows the
// `seats` push carries and GET /agents answers.
//
// AN ADMIN'S ROW is the static row with the live overlay merged on, the call
// in flight included; A MEMBER'S is the seat's public identity and its
// [livestate.AgentStatus]. Mounted at member and gated by the `seats` push's
// reach, so nothing below member is handed either.
func (s *Service) Roster(reach auth.Reach) any {
	s.RefreshPlacement()
	return s.roster(reach)
}

// roster is [Service.Roster] without the lease read, for a caller that has
// just made one.
func (s *Service) roster(reach auth.Reach) any {
	static := s.currentRoster()
	if reach.Covers(auth.ReachAdmin) {
		return s.state.MergeAgents(static)
	}
	return s.state.MergeStatuses(static)
}

// Org is the company's role and unit tree as reach is served it.
func (s *Service) Org(reach auth.Reach) any { return s.currentOrg(reach) }

// Tools is this node's catalogue, which only an admin is served: it is how
// the engine is wired, every tool every server serves with its schema.
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

func (s *Service) currentRoster() []map[string]any { return s.rosterRows() }

func (s *Service) currentOrg(reach auth.Reach) any { return s.org(reach) }

func (s *Service) currentSchedules() any { return s.schedules() }

func (s *Service) currentTools() []map[string]any { return s.tools() }

// ResendConfigured pushes every surface the company document decides — the
// roster, the org tree, the tool catalogue and the schedules — each to every
// socket in its own reach's variant.
//
// A config apply calls it: no event will ever correct any of them, since a
// revision that adds, renames or removes a seat produces nothing a projection
// could learn from, and the client's merge cannot express a deletion — so
// without it an open dashboard renders the company it connected to until
// somebody reloads.
func (s *Service) ResendConfigured() {
	s.RefreshPlacement()
	now := s.now()
	s.hub.Broadcast(KindSeats, func(reach auth.Reach) (any, bool) { return s.roster(reach), true }, now)
	s.hub.Broadcast(KindOrg, func(reach auth.Reach) (any, bool) { return s.currentOrg(reach), true }, now)
	s.hub.Broadcast(KindTools, Shared(func() any { return s.currentTools() }), now)
	s.hub.Broadcast(KindSchedules, Shared(func() any { return s.Schedules() }), now)
}

func (s *Service) currentHealth() any { return s.health() }

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
				s.hub.Broadcast(KindHealth, Shared(s.currentHealth), s.now())
				s.flushTokens()
				// PLACEMENT ON THE SAME TICK: a seat moves between
				// nodes on a lease, which publishes nothing, so an open
				// screen learns a seat went unplaced — or was taken up
				// by a peer — only from a read. The seat host sweeps on
				// five seconds too (seat.SweepInterval), so a faster
				// read would find nothing newer.
				s.RefreshPlacement()
			}
		}
	}()
}

// flushTokens sends the spend rollup if it moved since the last one: a spend
// record arrived, or one aged out of the live window.
//
// AGED AS WELL AS ARRIVED. A record leaving the window publishes nothing, so a
// rollup pushed only on arrivals kept every open screen on a figure the window
// no longer held until the next phase completed — on a quiet company, a day's
// spend under the heading of a day that had none.
//
// The flag is cleared only AFTER the fold, so an aggregation that panicked
// would not consume the burst it failed on and leave the rollup stale until
// the next phase completed.
//
// FOLDED ONLY WHILE AN ADMIN IS CONNECTED to be pushed it: the rollup is spend,
// which no other reach is sent, and a fold nobody is sent is a fold wasted. The
// flag stays set until one is, so the first tick after an admin connects pushes
// the window as it then stands.
func (s *Service) flushTokens() {
	if s.state.ExpireSpend() {
		s.tokensDirty.Store(true)
	}
	if !s.tokensDirty.Load() {
		return
	}
	s.hub.Broadcast(KindTokens, Shared(func() any {
		rollup := s.TokenRollup()
		s.tokensDirty.Store(false)
		return rollup
	}), s.now())
}

// TokenRollup folds the live window into the breakdown the `tokens` push and
// the snapshot carry, which no screen of the bundled dashboard draws: its spend
// screens read named windows.
//
// Exported because the snapshot needs the same answer: a client that connected
// mid-window and one that has been receiving pushes must hold the same rollup,
// and two constructions of it is how they come to differ.
func (s *Service) TokenRollup() tokens.Rollup {
	// The window this rollup actually covers, reported rather than assumed:
	// a reader prints it beside the numbers, and a figure labelled with
	// the wrong window is worse than an unlabelled one. The projection ages
	// its window on its own clock and names the two instants it cut it at.
	window := s.state.Spend()
	return tokens.Aggregate(window.Records, tokens.Options{
		Handles: s.handles(),
		Since:   window.Since,
		Until:   window.Until,
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
