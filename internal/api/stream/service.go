package stream

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
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

// Health is what the shared tick carries and the snapshot opens with: this
// node's WHOLE health body, pushed as it is on every [KindHealth] frame.
//
// AN INTERFACE rather than the struct, because the struct is `api.Health` —
// an explicit public type owned by package api, which imports this one, so
// naming it here would be an import cycle. This service carries the value to
// the wire without reading into it, except for the ONE fact a socket's
// posture is decided from, which is why that is the one method. The push used
// to be a three-field type declared HERE, which is how a node refusing every
// inbound webhook for want of a configuration pushed a frame identical to a
// healthy idle one's, and how five screens came to poll a query for the rest
// of the body.
type Health interface {
	// NodeStatus is the node's own posture word, the `status` /ready is
	// decided from — and so the one [PostureFunc] reads.
	NodeStatus() string
}

// HealthFunc reports the current health, for the shared tick and the snapshot.
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

	// health is consulted by the shared tick.
	health HealthFunc

	// posture derives a socket's delivery mode from that health. See
	// [PostureFunc].
	posture PostureFunc

	// seats is the chart's seats by agent id, for the per-agent rollup's
	// names and cross-links. It answers an empty directory while no
	// company is active, which leaves them blank.
	seats SeatsFunc

	// roster, org and tools are the config-derived surfaces. See Options.
	roster    func() []map[string]any
	org       func() any
	tools     func() []map[string]any
	schedules func() any

	// placement reads the seat leases for the seat-state vocabulary's
	// `unplaced`. See Options.Placement.
	placement PlacementFunc

	// placementFailing is whether the last placement read failed, so a
	// lease table that stays unreadable is said once rather than on every
	// tick.
	placementFailing atomic.Bool

	now      func() time.Time
	interval time.Duration

	// chart decides a `watch` frame. See [Options.Chart].
	chart authz.Chart

	// holders resolves a `watch` frame's login. See [Options.Holders].
	holders iam.Holders

	// seatOf is the published company's word on a seat. See
	// [Options.SeatOf].
	seatOf SeatOfFunc

	// listeners is every open socket, by what it was opened with — see
	// lifetime.go. The service holds it because what ends a socket arrives
	// here: an identity move, a published company.
	listeners listeners

	// rooms decides and feeds artifact rooms; work decides them again off
	// the applier's goroutine, and stopPresence stops hearing the presence
	// hub. See rooms.go.
	rooms        Rooms
	work         *roomWork
	stopPresence func()

	// metrics records the socket's push lag, and judge the change
	// visibility of what a room is pushed; nil records nothing.
	metrics Metrics
	judge   Objectives

	// tokensDirty means a phase completed since the last rollup went out.
	// Set on the publish path and cleared on the tick — see flushTokens.
	tokensDirty atomic.Bool

	// pushMu orders the `agents` pushes, and pushed is what the last of them
	// said about each seat's live call, by agent id: which call, and the version of each
	// heavy field it described. See pushAgents.
	pushMu sync.Mutex
	pushed map[string]pushedCall

	mu      sync.Mutex
	ticking bool
	stop    chan struct{}
	done    chan struct{}
}

// SeatsFunc answers the chart's seats by agent id, which the per-agent rollup
// names and links its rows with. See [tokens.Seats].
type SeatsFunc func() tokens.Seats

// PlacementFunc reads which of the company's agent seats some node in the fleet
// holds, keyed by AGENT ID — the name a seat's lease is held under — every agent
// seat in the company, true where a node holds it. An error is a read that did
// not happen, never "none are held".
type PlacementFunc func() (map[string]bool, error)

// Options configure a service.
//
// Health, Posture, Seats, Roster, Org, Tools, Schedules, Placement, Chart,
// Holders and SeatOf are REQUIRED, and [NewService] refuses a missing one by name. Each is
// something the engine beside the API always answers, so a missing one is a
// wiring mistake, and serving around it would push a confident answer where
// there is none: a health frame reading "ok", an empty catalogue, an
// organization with no seats, a lead told they may not watch their own report.
type Options struct {
	Health HealthFunc

	// Posture decides whether a socket is served live or degraded, from
	// the same Health the tick just read. See [PostureFunc].
	Posture PostureFunc

	// Seats supplies the chart's seats by agent id. A seat absent from it
	// leaves its row's handle blank rather than guessing one: a wrong link
	// is worse than no link.
	Seats SeatsFunc

	// Roster, Org and Tools are the three surfaces the dashboard renders
	// from CONFIGURATION rather than from anything that has happened.
	//
	// The projection cannot answer any of them: it holds what a seat is
	// DOING, so it can say a seat is mid-phase and not that the seat
	// exists. Snapshot used to ask it for the agent list anyway, merging
	// the live overlay onto a static roster of nil — an empty list, every
	// connect, for ever on a company whose model was not answering.
	//
	// Functions, not values, for the same reason Seats is one: an apply
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

	// Chart answers who leads whom, which is what a `watch` frame is
	// decided by — see [watching]. REQUIRED: a watch installs routing for
	// a seat's inbox, and the question it asks is the one the `work_inbox`
	// query asks of the same seat, through the same seam. A service built
	// without one would answer every lead's watch of a report as
	// "undecidable" for the life of the process.
	Chart authz.Chart

	// Holders says whose record somebody else's LOGIN names — the identity
	// directory's half of [iam.OwnerOf] — which is what a `watch` frame
	// naming a login is resolved through before it is decided and
	// installed. REQUIRED for [Options.Chart]'s reason: `inbox_changed` is
	// pushed to the name a person's notices are kept under, their SEAT
	// when the directory binds them to one, so a watch installed on the
	// login as sent is one no frame ever reaches — and a service built
	// without it would refuse every such watch as undecidable for the life
	// of the process.
	Holders iam.Holders

	// SeatOf answers what the company this node has published says about a
	// seat, which is what an open socket acting as a seat is decided by
	// when a company is published — in memory, with no identity read (see
	// lifetime.go). REQUIRED for [Options.Chart]'s reason: a service built
	// without it could never say a seat is gone.
	SeatOf SeatOfFunc

	// Rooms decides and feeds artifact rooms. Optional, and every half or
	// none: the zero value serves no room, which is a node with no
	// artifacts domain, and a join there is `unknown_query`. A partial one
	// is refused by name, because a room that could be joined and never
	// told who is present is a room that lies.
	Rooms Rooms

	// Metrics records the socket's push lag; nil records nothing.
	Metrics Metrics

	// Objectives records the change visibility of every committed change
	// this service pushes — an inbox movement, and what the artifact rooms
	// are pushed ([Objectives]); nil records nothing.
	Objectives Objectives
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
		{"Seats", opts.Seats == nil},
		{"Roster", opts.Roster == nil},
		{"Org", opts.Org == nil},
		{"Tools", opts.Tools == nil},
		{"Schedules", opts.Schedules == nil},
		{"Chart", opts.Chart == nil},
		{"Holders", opts.Holders == nil},
		{"SeatOf", opts.SeatOf == nil},
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
	if r := opts.Rooms; (r.Access != nil || r.Stores != nil || r.Presence != nil) && !r.serves() {
		return nil, errors.New("stream: Options.Rooms needs Access, Stores and Presence " +
			"together: a room that could be joined and never told who is present, or " +
			"where its datastore stands, is a room that lies")
	}
	s := &Service{
		hub:       NewHub(),
		state:     state,
		health:    opts.Health,
		posture:   opts.Posture,
		seats:     opts.Seats,
		roster:    opts.Roster,
		org:       opts.Org,
		tools:     opts.Tools,
		schedules: opts.Schedules,
		chart:     opts.Chart,
		holders:   opts.Holders,
		seatOf:    opts.SeatOf,
		placement: opts.Placement,
		now:       opts.Now,
		interval:  opts.HealthInterval,
		pushed:    map[string]pushedCall{},
		rooms:     opts.Rooms,
		metrics:   opts.Metrics,
		judge:     opts.Objectives,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.interval <= 0 {
		s.interval = HealthInterval
	}
	if s.rooms.serves() {
		s.startRooms()
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
	s.pushAgents(change, now)
	if change.Sandboxes {
		s.hub.Broadcast(Push(KindSandboxes, SandboxRows(s.state.ActiveSandboxes()), now))
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
		s.hub.Broadcast(Push(KindSandboxes, SandboxRows(s.state.ActiveSandboxes()), now))
	}
	s.pushAgents(change, now)
}

// pushAgents sends the rows of the seats a change moved, named by agent id.
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
// LEFT OUT ONCE, THEN MASKED PER AUDIENCE. What a push leaves out is decided
// once per broadcast and shared by every reader, because the bookkeeping it
// is decided against is per SEAT; the rows then travel as [AgentRows], which
// the hub masks for each audience variant ([livestate.MaskRow]) — a reader
// the seat's trail is not open to receives its status and no body, whichever
// heavy fields this push carried. Masked first, each variant would need its
// own bookkeeping, and two variants would each leave out a field the other
// was the one to carry.
func (s *Service) pushAgents(change livestate.Change, now time.Time) {
	if len(change.Agents) == 0 {
		return
	}
	// Sorted, so a push carrying two seats is byte-stable across runs — Go
	// map iteration is randomised, and a frame whose row order changes for
	// no reason makes a diff of two captures unreadable.
	seats := slices.Sorted(maps.Keys(change.Agents))
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	rows := s.state.OverlayRows(seats)
	if len(rows) == 0 {
		return
	}
	for _, row := range rows {
		s.leaveOutWhatIsHeld(row)
	}
	s.hub.Broadcast(Push(KindAgents, AgentRows(rows), now))
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
	// KEYED ON THE SEAT'S AGENT ID, which is what names a row: a role is
	// prose two seats may share, and two seats' calls held under one key
	// would each leave out what the other's push carried.
	seat, _ := row["agent_id"].(string)
	call, _ := row["live_call"].(*livestate.LiveCall)
	if call == nil {
		delete(s.pushed, seat)
		return
	}
	last, ok := s.pushed[seat]
	s.pushed[seat] = pushedCall{key: call.Key(), versions: call.Versions}
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

// Snapshot is the state a client receives the instant it connects.
//
// Built entirely from the in-memory projection — no database round trip on
// connect. That is the whole reason the projection exists: a dashboard that
// rebuilt agent history from the store on every reconnect would take a
// thirty-day scan per tab, and would lose any call mid-flight while it did.
//
// It is the snapshot FOR ONE AUDIENCE, and carries only the keys whose push
// that audience may receive (see [needs]), each SHAPED for it exactly as its
// push is ([Shaped]): the seats' live calls masked to their status where the
// audience may not read their trail, the coding runs likewise, and the org
// tree in the variant the audience reads.
//
// The one read it makes is the seat leases ([Service.RefreshPlacement]),
// because no event says which seats a node holds and a connecting tab must not
// be handed a placement up to a tick old.
func (s *Service) Snapshot(audience Audience) map[string]any {
	s.RefreshPlacement()
	full := map[string]any{
		"health": s.currentHealth(),
		// THE STATIC ROSTER FIRST, with the live overlay merged onto it.
		// MergeAgents walks what it is GIVEN, so passing nil here — which
		// it did — produced an empty list whatever the projection held.
		"agents": AgentRows(s.state.MergeAgents(s.currentRoster())),
		"org":    s.currentOrg(),
		"tools":  s.currentTools(),
		// THE CONFIGURED ROWS, not the dispatch ledger. The screen renders
		// its table from this slice and fetches the ledger itself, so
		// without it the table stayed on its skeleton for ever — the
		// client reads null as "not here yet", which was permanently
		// true.
		"schedules": s.currentSchedules(),
		"events":    s.state.RecentEvents(livestate.EventFeedLimit),
		"sandboxes": SandboxRows(s.state.ActiveSandboxes()),
		"tokens":    SpendRollup(s.TokenRollup()),
		"budget":    s.state.Budget(),
	}
	// EACH KEY IS WHAT ITS PUSH WOULD CARRY, so a reader receives in the
	// handshake exactly what it would receive afterwards: an audience that
	// is not an admin gets no `events` key rather than an empty one, which
	// the client reads as "nothing here yet" rather than as "the company
	// has done nothing" — and every key it does get in the shape its push
	// hands it.
	out := make(map[string]any, len(full))
	for key, value := range full {
		if audience.Receives(snapshotKinds[key]) {
			_, out[key] = shapeFor(value, audience)
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
var snapshotKinds = map[string]Kind{
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

// Roster is the company's seat list as the `seats` push carries it: [Shaped],
// so each reader is handed it masked to their reach over each seat — through
// the hub on a push, through [ShapeFor] on its REST mirror.
//
// Exported because a config apply has to re-send it: the client's own doc
// says a merge cannot express a deletion, so a revision that removed a role
// would leave its card on screen until someone reloaded the page.
func (s *Service) Roster() AgentRows {
	s.RefreshPlacement()
	return AgentRows(s.state.MergeAgents(s.currentRoster()))
}

// AudienceFor is what principal p may receive on this node: [NewAudience] over
// the agent seats whose trail p may read, each decided by the authority
// table's own `audit.seat.read` ([authz.ActionSeatTrailRead]) — its lead, or
// an admin — against the chart this node runs.
//
// THE TABLE'S ROW AND NOT A SECOND COPY OF IT, so who reads a seat's body over
// the socket is exactly who reads its turns, its memory and its conversations
// through the questions beside it, and moving the rule moves both.
//
// AN UNDECIDABLE SEAT IS NOT BODY: a chart this node cannot read withholds a
// transcript rather than serving one, and the reader still sees the seat's
// status. An admin reads every body and is decided without the walk.
func (s *Service) AudienceFor(ctx context.Context, p iam.Principal) Audience {
	if p.Can(iam.GrantAdmin) {
		return NewAudience(p, nil)
	}
	now := s.now()
	var body []string
	undecided := 0
	for key, seat := range s.seats() {
		// THE AGENT SEATS, by their agent id: a human seat's key is
		// [tokens.PersonKey] of its handle, and a person has no trail
		// here.
		if seat.Handle == "" || key == tokens.PersonKey(seat.Handle) {
			continue
		}
		d := authz.Decide(ctx, p, authz.ActionSeatTrailRead, authz.Object{
			Kind: authz.KindPerson, Owner: seat.Handle}, s.chart, now)
		switch {
		case d.Allowed:
			body = append(body, key)
		case d.Unknown():
			undecided++
		}
	}
	if undecided > 0 {
		log.InfoContext(ctx, "stream_audience_undecided", "login", p.Login,
			"seats", undecided,
			"hint", "the chart could not say whether this reader leads those "+
				"seats; they are shown their status and not their trail until "+
				"the socket is opened again")
	}
	return NewAudience(p, body)
}

// Org is the company's role and unit tree, [Shaped] — what the `org` push
// carries, and what its REST mirror hands each reader through [ShapeFor].
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
// ON EVERY PUBLISHED COMPANY, at boot as well as on every apply
// ([engine.Engine.SetOnCompanyPublished]).
//
// ONE METHOD NAMING ITS OWN KINDS, where there could be an exported
// Broadcast(kind, data) and a caller in another package spelling each kind:
// the payload each kind carries is this service's to build, and a caller that
// paired a kind with the wrong payload would compile cleanly.
//
// AND EVERY OPEN SOCKET IS DECIDED AGAIN, after the re-sends, in memory
// against the company just published: the seat its principal acts as may be
// gone, and a lead may have moved off the team whose inbox the socket watches.
// See lifetime.go.
func (s *Service) CompanyPublished() {
	now := s.now()
	for _, push := range []struct {
		kind Kind
		data any
	}{
		{KindSeats, s.Roster()},
		// UNSHAPED: the hub hands each client the shape its audience may
		// read, so one push carries every variant ([Shaped]).
		{KindOrg, s.currentOrg()},
		{KindTools, s.Tools()},
		{KindSchedules, s.Schedules()},
	} {
		s.hub.Broadcast(Push(push.kind, push.data, now))
	}
	s.listeners.each(func(l *listener) { signal(l.published) })
}

// ToolsChanged re-sends the tool catalogue alone, to every client whose
// audience receives it.
//
// FOR A CATALOGUE THAT MOVED WITHOUT A PUBLISHED COMPANY. The catalogue is the
// current epoch's registry, and an epoch is published whole but for its shared
// MCP servers' tools, which the engine refiles when a server comes back with a
// different list or a retried start brings one up
// ([engine.Engine.SetOnCatalogueChanged]). No event corrects this push either,
// so without a re-send an open dashboard listed the tools the server had when
// the company was published until somebody reloaded.
//
// THE CATALOGUE ALONE: nothing else [Service.CompanyPublished] re-sends has
// moved, and deciding every socket again against an unchanged company is work
// with no new answer.
func (s *Service) ToolsChanged() {
	s.hub.Broadcast(Push(KindTools, s.Tools(), s.now()))
}

// InboxChange is the payload of an `inbox_changed` frame: which domain's half
// of whose inbox moved, by how many notices, and the subject and reason of the
// newest — the engine's inbox.Movement, field for field.
//
// IDENTIFIERS AND A COUNT, NEVER CONTENT. The frame reaches whoever watches
// the seat, and what it is for is telling that screen to re-read the inbox
// through the inbox question — the same question, decided by the same
// authority, as the poll it makes immediate. An excerpt here would be the
// inbox's content read around that authority. The count is a HINT: the rows
// are the truth, and a screen re-reads them rather than adding these up.
//
// THE DASHBOARD READS IT AS DECLARED IN ITS CONTRACT (`contract/wire.ts`'s
// `InboxChange`), field for field: a key this sends that the screen does not
// declare is one nothing on it can tell apart, and a key it declares that
// this never sends is one it reads as undefined.
type InboxChange struct {
	// Source is the domain whose half of the inbox moved — the state log's
	// name, `tracker` — on a movement and on a re-read alike: a gap is one
	// domain's movements switching where they come from.
	Source      string `json:"source"`
	Handle      string `json:"handle"`
	UnreadDelta int    `json:"unread_delta"`
	Subject     string `json:"subject"`
	Reason      string `json:"reason"`

	// Stored is the broker's instant for the oldest record that moved the
	// inbox (internal/worklet's InboxMovement.At): what the push is timed from
	// (change visibility, [Objectives]). NEVER ON THE FRAME — the screen
	// re-reads the inbox, and the frame names what moved and nothing about
	// when.
	Stored time.Time `json:"-"`
}

// InboxChanged pushes one seat's inbox movement to the clients watching that
// seat and to nobody else — see [KindInboxChanged].
//
// NEVER BLOCKS, for [Hub.Broadcast]'s reason, and its caller is why that
// matters: this is called from an applier's post-commit half, on the apply
// loop's own goroutine, with the next batch waiting behind it.
//
// TIMED as every committed change this service pushes is ([visible]): from
// the broker's instant for the records that moved the inbox to the push, and
// only where some socket watching the seat was handed it.
func (s *Service) InboxChanged(change InboxChange) {
	if change.Handle == "" {
		// NO SEAT, NO AUDIENCE: a seat-routed frame with no address is
		// refused by the hub, loudly. A movement for nobody is not one.
		return
	}
	visible(s.judge, s.hub.Broadcast(PushSeat(KindInboxChanged, change.Handle, change,
		s.now())), change.Stored)
}

// InboxReread tells every watched seat's sockets to read their inbox's source
// half again: one `inbox_changed` frame per watched seat, naming the source,
// counting nothing and naming no subject — see [KindInboxChanged].
//
// FOR A GAP IN WHAT THIS NODE HEARS of one source, never a movement: the
// engine switched where that domain's movements come from (its own applier, a
// peer's relayed), and what moved in between is not replayed. A frame is
// exactly what a screen needs to cover that — it re-reads the inbox through
// the inbox question on every one, whatever it counts — where closing every
// socket would have rebuilt every tab's whole snapshot to refresh one list.
//
// NEVER BLOCKS, for [Service.InboxChanged]'s reason.
func (s *Service) InboxReread(source string) {
	now := s.now()
	for _, seat := range s.hub.WatchedSeats() {
		s.hub.Broadcast(PushSeat(KindInboxChanged, seat,
			InboxChange{Source: source, Handle: seat}, now))
	}
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
func (s *Service) flushTokens() {
	if s.state.ExpireSpend() {
		s.tokensDirty.Store(true)
	}
	if !s.tokensDirty.Load() {
		return
	}
	rollup := s.TokenRollup()
	s.tokensDirty.Store(false)
	s.hub.Broadcast(Push(KindTokens, SpendRollup(rollup), s.now()))
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
		Seats: s.seats(),
		Since: window.Since,
		Until: window.Until,
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
	s.stopRooms()
	s.hub.Close()
}
