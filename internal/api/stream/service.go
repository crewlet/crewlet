package stream

import (
	"context"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

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

// Health is what the shared tick carries.
//
// InFlight and ShuttingDown are pointers because ABSENT and ZERO are different
// answers: a merged node knows how many turns are running, and a standalone API
// does not — and a dashboard that drew "0 in flight" for "cannot see the
// engine" would report an idle company during an outage.
type Health struct {
	Status       string `json:"status"`
	InFlight     *int   `json:"in_flight,omitempty"`
	ShuttingDown *bool  `json:"shutting_down,omitempty"`
}

// HealthFunc reports the current health, for the shared tick.
type HealthFunc func() Health

// Service turns the engine's event stream into the pushes a dashboard mirrors.
//
// It is the only thing that applies an event to the projection AND fans out the
// result. A dashboard renders what arrives here; it does not re-derive it from
// the raw event stream. Every tab used to do that — three private copies of the
// projection, each drifting its own way.
type Service struct {
	hub   *Hub
	state *livestate.LiveState

	// health is consulted by the shared tick. Nil answers a bare "ok",
	// which is the standalone API's honest answer: it has no engine to ask
	// and must not invent one.
	health HealthFunc

	// handles maps a role to its agent handle, for the per-agent rollup's
	// cross-links. Nil leaves them blank, which is what a standalone API
	// with no org honestly has.
	handles HandleFunc

	// roster, org and tools are the config-derived surfaces. See Options.
	roster func() []map[string]any
	org    func() map[string]any
	tools  func() []map[string]any

	now      func() time.Time
	interval time.Duration

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
type Options struct {
	Health HealthFunc

	// Handles supplies the role-to-handle map. Nil leaves each row's
	// handle blank rather than guessing one — a wrong link is worse than
	// no link.
	Handles HandleFunc

	// Roster, Org and Tools are the three surfaces the dashboard renders
	// from CONFIGURATION rather than from anything that has happened.
	//
	// The projection cannot answer any of them: it holds what a seat is
	// DOING, so it can say a seat is mid-Plan and not that the seat
	// exists. Snapshot used to ask it for the agent list anyway, merging
	// the live overlay onto a static roster of nil — an empty list, every
	// connect, for ever on a company whose model was not answering.
	//
	// Functions, not values, for the same reason Handles is one: an apply
	// replaces the company, and a roster captured at boot would keep
	// showing a role a revision deleted.
	//
	// Nil is an empty surface rather than a fault: a standalone API has no
	// engine to ask for tools, and that screen says so.
	Roster func() []map[string]any
	Org    func() map[string]any
	Tools  func() []map[string]any

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

// NewService builds the fan-out over a projection.
func NewService(state *livestate.LiveState, opts Options) *Service {
	s := &Service{
		hub:      NewHub(),
		state:    state,
		health:   opts.Health,
		handles:  opts.Handles,
		roster:   opts.Roster,
		org:      opts.Org,
		tools:    opts.Tools,
		now:      opts.Now,
		interval: opts.HealthInterval,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.interval <= 0 {
		s.interval = HealthInterval
	}
	return s
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
	change := s.state.Apply(env)
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
		// caller's publish — which on a merged node is the engine's own
		// goroutine, mid-turn, between a model's answer and its tools.
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
func (s *Service) Snapshot() map[string]any {
	return map[string]any{
		"health": s.currentHealth(),
		// THE STATIC ROSTER FIRST, with the live overlay merged onto it.
		// MergeAgents walks what it is GIVEN, so passing nil here — which
		// it did — produced an empty list whatever the projection held.
		"agents":    s.state.MergeAgents(s.currentRoster()),
		"org":       s.currentOrg(),
		"tools":     s.currentTools(),
		"events":    s.state.RecentEvents(livestate.EventFeedLimit),
		"sandboxes": s.state.ActiveSandboxes(),
		"tokens":    s.TokenRollup(),
		"budget":    s.state.Budget(),
	}
}

// Roster is the company's seat list as the `seats` push carries it.
//
// Exported because a config apply has to re-send it: the client's own doc
// says a merge cannot express a deletion, so a revision that removed a role
// would leave its card on screen until someone reloaded the page.
func (s *Service) Roster() []map[string]any { return s.state.MergeAgents(s.currentRoster()) }

// Org is the company's role and unit tree, for the same re-send.
func (s *Service) Org() map[string]any { return s.currentOrg() }

// Tools is this node's catalogue, for the same re-send. It changes on an
// apply too — a revision that adds an MCP server adds its tools.
func (s *Service) Tools() []map[string]any { return s.currentTools() }

func (s *Service) currentRoster() []map[string]any {
	if s.roster == nil {
		return nil
	}
	return s.roster()
}

func (s *Service) currentOrg() map[string]any {
	if s.org == nil {
		return map[string]any{}
	}
	return s.org()
}

func (s *Service) currentTools() []map[string]any {
	if s.tools == nil {
		return nil
	}
	return s.tools()
}

// Broadcast pushes an envelope to every client, for the surfaces that own their
// own data — the roster, the org tree, the tool catalogue, the schedules.
func (s *Service) Broadcast(kind string, data any) {
	s.hub.Broadcast(Push(kind, data, s.now()))
}

func (s *Service) currentHealth() Health {
	if s.health == nil {
		// No engine to ask. "ok" and nothing else is the honest answer: a
		// standalone API is serving, and it cannot see whether anything
		// is in flight.
		return Health{Status: "ok"}
	}
	return s.health()
}

// StartHealthTicks runs the shared tick until the context is cancelled or
// [Service.Stop] is called.
//
// Idempotent: a second call while one is running is a no-op rather than a
// second timer, because the merged topology can reach this from either half.
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
				s.hub.Broadcast(Push(KindHealth, s.currentHealth(), s.now()))
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
	handles := map[string]string{}
	if s.handles != nil {
		handles = s.handles()
	}
	return tokens.Aggregate(s.state.SpendRecords(), tokens.Options{
		Handles: handles,
		// The window this rollup actually covers, reported rather than
		// assumed: the client prints it beside the numbers, and a figure
		// labelled with the wrong window is worse than an unlabelled one.
		SinceDays: livestate.LiveSpendWindowDays(),
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
