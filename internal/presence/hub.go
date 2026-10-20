package presence

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

var log = logging.Get("presence")

// Queue is the broker as the hub uses it: the two ephemeral pairs and nothing
// durable. Publish is deliberately not here, so no path of this package can
// write a presence frame anywhere it would be kept.
type Queue interface {
	Broadcast(ctx context.Context, subject string, payload []byte) error
	Listen(ctx context.Context, pattern string, h queue.ListenFunc) (queue.Unsubscribe, error)
	AskEach(ctx context.Context, subject string, request []byte, each func(reply []byte) bool) error
	Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// Metrics is the recorder the hub reports through.
type Metrics interface {
	Add(name string, n uint64, attrs metrics.Attrs)
	Set(name string, v float64, attrs metrics.Attrs)
}

// Options configure a hub.
type Options struct {
	// Queue is the broker the fleet's frames cross. REQUIRED.
	Queue Queue

	// Wire seals what this node sends and opens what it hears. REQUIRED:
	// a frame names a person, and an unsealed one would let anything on
	// the broker put somebody in a room.
	Wire *Wire

	// Node is this node's id: what its frames name, and what tells its own
	// frames, which the broker echoes, from a peer's. REQUIRED.
	Node string

	// AskBudget is how long a node gaining its first member of a room waits
	// for its peers' rosters: the fleet's read budget, which the engine
	// passes so that every scatter a person waits on is bounded alike.
	// REQUIRED.
	AskBudget time.Duration

	// Now is the clock members are stamped and expired by; nil is the wall
	// clock.
	Now func() time.Time

	// Metrics records frames and rooms; nil records nothing.
	Metrics Metrics

	// Coalesce is the shortest interval between two fan-outs of one room
	// to this node's sockets. Zero takes a second divided by
	// [CoalesceRate], the production value; a suite sets
	// a shorter one so it waits less, never a different rule.
	Coalesce time.Duration

	// Broadcast is the shortest interval between two frames this node
	// sends the fleet about one room. Zero takes a second divided by
	// [BroadcastRate].
	Broadcast time.Duration
}

// Change is what one room's fan-out tells this node's sockets: the members it
// presents that are new or changed, the sessions it no longer presents, and,
// for a room presenting only its most recent members, how many of each kind
// it counts beside them.
//
// NO WIRE SHAPE OF ITS OWN: the socket names each kind's room by that kind's
// own field (an artifact's push says `artifact`, as every other artifact frame
// does), so it builds the frame a client reads from this.
type Change struct {
	Room      Room
	Upsert    []Member
	Leave     []string
	Counts    *Counts
	Truncated bool
}

// Local is who a socket joining a room is, as its node resolved them: the
// seat handle shown beside them, and whether a person or an agent.
type Local struct {
	Seat string
	Kind iam.ActorKind
}

// The hub's refusals.
var (
	// ErrNotJoined is a presence frame for a session this node holds in no
	// room: a socket that left, or one that never joined.
	ErrNotJoined = errors.New("presence: that session is not in the room")

	// ErrStopped is a join on a hub that has stopped.
	ErrStopped = errors.New("presence: the hub has stopped")
)

// Hub is this node's presence: the rooms its sockets and its agents are in,
// what its peers last told it about the same rooms, and the frames between.
//
// ONE LOCK OVER ITS STATE, never held across the broker or a sink: every
// broadcast, listen, withdrawal, ask and fan-out runs after it is released, so
// a handler of the queue's (which takes it) can never wait on a call made
// under it. The one lock held across a sink is [Hub.delivering], taken by a
// fan-out alone and always before the first, which orders what the sinks are
// handed and nothing else.
type Hub struct {
	queue     Queue
	wire      *Wire
	node      string
	askBudget time.Duration
	now       func() time.Time
	metrics   Metrics
	coalesce  time.Duration
	broadcast time.Duration

	mu      sync.Mutex
	rooms   map[Room]*room
	sinks   map[uint64]func(Change)
	sinkSeq uint64
	running bool
	stopped bool

	// delivering serialises the fan-outs this hub hands its sinks: every
	// flush takes it before mu and holds it until its sinks have returned
	// ([Hub.flush]). Two fan-outs of a room are two timers, and the second
	// can fire while the first is still in its sinks — taking its change
	// after the first did and handing it over before, so a member's leave
	// reached a socket ahead of the upsert taken before it and the socket
	// went on showing the member until they moved again.
	delivering sync.Mutex

	// listening is the rooms this node is a listener for:
	// listened and withdrawn by the reconciler alone ([Hub.reconcile]),
	// read and written under mu.
	listening map[Room]queue.Unsubscribe

	// lifetime is the hub's own context, cancelled by Stop; wake asks the
	// reconciler to look again; loops counts the hub's own goroutines.
	lifetime context.Context
	cancel   context.CancelFunc
	wake     chan struct{}
	loops    sync.WaitGroup
	serving  queue.Unsubscribe
}

// New builds a hub, refusing a missing requirement by name.
func New(opts Options) (*Hub, error) {
	switch {
	case opts.Queue == nil:
		return nil, errors.New("presence: a broker is required: rooms cross the fleet on it")
	case opts.Wire == nil:
		return nil, errors.New("presence: a sealed wire is required: a frame names a person")
	case opts.Node == "":
		return nil, errors.New("presence: this node's id is required: it tells this node's " +
			"own frames from a peer's")
	case opts.AskBudget <= 0:
		return nil, errors.New("presence: an ask budget is required: it bounds how long a " +
			"newly opened room waits for its peers")
	}
	h := &Hub{queue: opts.Queue, wire: opts.Wire, node: opts.Node, askBudget: opts.AskBudget,
		now: opts.Now, metrics: opts.Metrics, coalesce: opts.Coalesce, broadcast: opts.Broadcast,
		rooms: map[Room]*room{}, sinks: map[uint64]func(Change){},
		listening: map[Room]queue.Unsubscribe{}, wake: make(chan struct{}, 1)}
	if h.now == nil {
		h.now = func() time.Time { return time.Now().UTC() }
	}
	if h.coalesce <= 0 {
		h.coalesce = time.Second / CoalesceRate
	}
	if h.broadcast <= 0 {
		h.broadcast = time.Second / BroadcastRate
	}
	return h, nil
}

// Domain is the key domain this hub's frames are sealed under, for the gate
// that holds every channel's apart.
func (h *Hub) Domain() string { return h.wire.Domain() }

// OnChange registers what this node does with a room's fan-out, and answers
// the function that unregisters it. fn must not block: it is called after the
// hub's lock is released, on the fan-out's own goroutine, one fan-out at a
// time and in the order the changes were taken.
func (h *Hub) OnChange(fn func(Change)) (stop func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.sinkSeq
	h.sinkSeq++
	h.sinks[id] = fn
	var once sync.Once
	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			delete(h.sinks, id)
		})
	}
}

// Start makes this node an answerer of its peers' asks and starts the
// rebroadcast, the expiry and the reconciler of its listens. The hub's
// goroutines are its own, ended by [Hub.Stop], whatever ctx does after.
func (h *Hub) Start(ctx context.Context) error {
	h.mu.Lock()
	if h.running || h.stopped {
		h.mu.Unlock()
		return nil
	}
	h.running = true
	h.lifetime, h.cancel = context.WithCancel(context.WithoutCancel(ctx))
	lifetime := h.lifetime
	h.mu.Unlock()

	stop, err := h.queue.Serve(ctx, topics.PresenceAsk, h.answer)
	if err != nil {
		h.mu.Lock()
		h.running = false
		h.cancel()
		h.mu.Unlock()
		return fmt.Errorf("presence: answer peers' asks: %w", err)
	}
	h.serving = stop
	h.loops.Add(2)
	go h.reconciler(lifetime) //nolint:contextcheck // the hub's own lifetime, ended by Stop, never a caller's; see Start
	go h.ticker(lifetime)     //nolint:contextcheck // the hub's own lifetime, ended by Stop, never a caller's; see Start
	return nil
}

// Stop tells the fleet this node's members have left, withdraws every listen
// and the answerer, and ends the hub's goroutines, bounded by ctx. A hub
// stopped takes no further join.
func (h *Hub) Stop(ctx context.Context) {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	h.stopped = true
	running := h.running
	var farewell []Frame
	for id, r := range h.rooms {
		if sessions := slices.Sorted(maps.Keys(r.local)); len(sessions) > 0 {
			farewell = append(farewell, h.frameLocked(id, FrameState, nil, sessions, false))
		}
		r.stopTimer()
	}
	h.rooms = map[Room]*room{}
	h.mu.Unlock()

	// THE LEAVES FIRST, while the broker is still there: a peer told now
	// drops these members at once rather than showing them for the whole
	// expiry after this node went away.
	for _, f := range farewell {
		h.send(ctx, f)
	}
	if !running {
		return
	}
	h.cancel()
	h.loops.Wait()
	h.mu.Lock()
	listening := h.listening
	h.listening = map[Room]queue.Unsubscribe{}
	h.mu.Unlock()
	for id, unsub := range listening {
		if err := unsub(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "presence_listen_not_withdrawn", "room", id.String(),
				"error", err.Error())
		}
	}
	if h.serving != nil {
		if err := h.serving(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "presence_answerer_not_withdrawn", "error", err.Error())
		}
	}
	h.setRooms(0)
}

// Join puts one socket in a room and answers the session it is known by there
// and the room as it presents now, the socket's own member included. The
// caller has decided the socket may be in the room (for an artifact's, that it
// may read the artifact); the hub decides nothing about who may be in a room.
func (h *Hub) Join(id Room, who Local) (string, Roster, error) {
	member := Member{Session: newSession(), Seat: who.Seat, Kind: who.Kind,
		Activity: Viewing}
	if err := id.check(); err != nil {
		return "", Roster{}, err
	}
	if err := checkMember(id.Kind, member); err != nil {
		return "", Roster{}, err
	}
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return "", Roster{}, ErrStopped
	}
	now := h.now()
	member.At = now
	r, created := h.roomLocked(id)
	r.local[member.Session] = &localMember{member: member, socket: true, touched: now}
	r.changed(member.Session)
	roster := r.roster(r.present())
	h.scheduleLocked(r)
	h.scheduleSendLocked(r)
	rooms := len(h.rooms)
	h.mu.Unlock()

	if created {
		h.setRooms(rooms)
	}
	h.poke()
	return member.Session, roster, nil
}

// Update is a socket's presence frame: what it is doing in the room and where
// it is looking. It moves the member's recency, which is what a room
// presenting only its most recent members orders by.
func (h *Hub) Update(id Room, session string, activity Activity, focus Focus) error {
	if !activity.Valid() {
		return fmt.Errorf("presence: %q is not an activity (viewing, editing or idle)", activity)
	}
	if err := CheckFocus(id.Kind, focus); err != nil {
		return err
	}
	h.mu.Lock()
	r := h.rooms[id]
	var m *localMember
	if r != nil {
		m = r.local[session]
	}
	if m == nil || !m.socket {
		h.mu.Unlock()
		return ErrNotJoined
	}
	now := h.now()
	m.touched = now
	if m.member.Activity == activity && m.member.Focus == focus {
		// NOTHING MOVED: a frame saying what the room already shows
		// changes no member's recency and fans nothing out.
		h.mu.Unlock()
		h.count("client")
		return nil
	}
	m.member.Activity, m.member.Focus, m.member.At = activity, focus, now
	r.changed(session)
	h.scheduleLocked(r)
	h.scheduleSendLocked(r)
	h.mu.Unlock()
	h.count("client")
	return nil
}

// Leave takes one socket out of a room. Leaving a room it is not in is not an
// error: a socket closing leaves every room it may have been in.
func (h *Hub) Leave(id Room, session string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[id]
	if r == nil {
		return
	}
	if _, held := r.local[session]; !held {
		return
	}
	delete(r.local, session)
	r.left(session)
	h.scheduleLocked(r)
	h.scheduleSendLocked(r)
	h.pokeLocked()
}

// Touch is an agent's tool session reading (viewing) or writing (editing) in a
// room, in the turn whose run is a.Turn: the seat appears in the room as an
// agent, naming that turn, and stays there for [agentLinger] after its last
// touch. One member per seat per room on this node.
func (h *Hub) Touch(id Room, a Agent) {
	seat, turn, activity, focus := a.Seat, a.Turn, a.Activity, a.Focus
	if id.check() != nil || seat == "" || len(turn) > maxTurnBytes || !activity.Valid() ||
		CheckFocus(id.Kind, focus) != nil {
		// A WIRING FAULT in the tool that touched, never a person's
		// input: logged and dropped, because presence is a display and a
		// tool call must not fail over it.
		log.Warn("presence_touch_refused", "room", id.String(), "seat", seat,
			"activity", string(activity))
		return
	}
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return
	}
	now := h.now()
	r, created := h.roomLocked(id)
	m := r.agentMember(seat)
	if m == nil {
		m = &localMember{member: Member{Session: newSession(), Seat: seat,
			Kind: iam.ActorAgent}}
		r.local[m.member.Session] = m
	}
	if m.member.Activity != activity || m.member.Focus != focus || m.member.Turn != turn ||
		m.member.At.IsZero() {
		m.member.Activity, m.member.Focus, m.member.Turn, m.member.At = activity, focus, turn, now
		r.changed(m.member.Session)
		h.scheduleLocked(r)
		h.scheduleSendLocked(r)
	}
	m.touched = now
	rooms := len(h.rooms)
	h.mu.Unlock()
	if created {
		h.setRooms(rooms)
	}
}

// agentLinger is how long an agent stays in a room after its last touch:
// [Expiry], three missed heartbeats, because an agent's touches are its
// heartbeats and a turn between two calls is still working in the room.
const agentLinger = Expiry

// newSession is a socket's or an agent's session id in one room: random, so it
// says nothing about the credential behind it and two tabs of one person are
// two members.
func newSession() string { return rand.Text() }

// roomLocked is id's room, made if this node held none. Caller holds mu.
func (h *Hub) roomLocked(id Room) (*room, bool) {
	if r, ok := h.rooms[id]; ok {
		return r, false
	}
	r := newRoom(id)
	h.rooms[id] = r
	return r, true
}

// scheduleLocked arranges r's next fan-out to this node's sockets: at once
// when the last one is at least [Options.Coalesce] old, and otherwise when it
// will be, so a room fans out at most [CoalesceRate] times a
// second however fast its members move. Caller holds mu.
func (h *Hub) scheduleLocked(r *room) {
	if r.timer != nil || h.stopped {
		return
	}
	delay := max(0, r.flushed.Add(h.coalesce).Sub(h.now()))
	id := r.id
	r.timer = time.AfterFunc(delay, func() { h.flush(id) })
}

// scheduleSendLocked arranges r's next frame to the fleet, when this node's
// own members changed since the last: at once when the last frame is at least
// [Options.Broadcast] old, and otherwise when it will be. Apart from the
// sockets' fan-out, so a member's move leaves its node within one broadcast
// window whatever the viewer's node does. Caller holds mu.
func (h *Hub) scheduleSendLocked(r *room) {
	if r.sendTimer != nil || h.stopped || len(r.outUpsert)+len(r.outLeave) == 0 {
		return
	}
	delay := max(0, r.sent.Add(h.broadcast).Sub(h.now()))
	id := r.id
	r.sendTimer = time.AfterFunc(delay, func() { h.flushSend(id) })
}

// flush fans one room out to this node's sockets: what changed of its
// presentation, at most [FrameMembers] members re-presented,
// and another fan-out arranged for the rest.
//
// IN THE ORDER THE CHANGES WERE TAKEN, under [Hub.delivering]: each change is
// the difference from the one before it, so a sink handed two out of order
// applies the older last and shows what the newer had already undone.
func (h *Hub) flush(id Room) {
	h.delivering.Lock()
	defer h.delivering.Unlock()
	h.mu.Lock()
	r := h.rooms[id]
	if r == nil || h.stopped {
		h.mu.Unlock()
		return
	}
	r.timer = nil
	r.flushed = h.now()
	change, pushed, deferred := r.diff(r.present(), r.flushed)
	if deferred {
		h.scheduleLocked(r)
	}
	sinks := slices.Collect(maps.Values(h.sinks))
	h.mu.Unlock()

	if pushed {
		h.count("push")
		for _, fn := range sinks {
			fn(change)
		}
	}
}

// flushSend tells the fleet what changed of this node's own members in one
// room, and forgets the room once the last of them left.
func (h *Hub) flushSend(id Room) {
	h.mu.Lock()
	r := h.rooms[id]
	if r == nil || h.stopped {
		h.mu.Unlock()
		return
	}
	r.sendTimer = nil
	r.sent = h.now()
	var out *Frame
	if len(r.outUpsert)+len(r.outLeave) > 0 {
		upsert := make([]Member, 0, len(r.outUpsert))
		for _, s := range slices.Sorted(maps.Keys(r.outUpsert)) {
			if m, ok := r.local[s]; ok {
				upsert = append(upsert, m.member)
			}
		}
		f := h.frameLocked(id, FrameState, upsert, slices.Sorted(maps.Keys(r.outLeave)), false)
		out = &f
		clear(r.outUpsert)
		clear(r.outLeave)
	}
	removed := false
	if len(r.local) == 0 {
		// THE LAST OF THIS NODE'S MEMBERS LEFT: what its peers said is
		// heard by nobody here, and the room goes with them once the
		// leave above is on its way.
		r.stopTimer()
		delete(h.rooms, id)
		removed = true
	}
	rooms := len(h.rooms)
	h.mu.Unlock()

	if out != nil {
		h.send(h.context(), *out)
	}
	if removed {
		h.setRooms(rooms)
		h.poke()
	}
}

// frameLocked is a frame from this node about room id. Caller holds mu.
func (h *Hub) frameLocked(id Room, kind FrameKind, upsert []Member,
	leave []string, full bool) Frame {

	return Frame{V: frameVersion, Kind: kind, Node: h.node, Room: id, Upsert: upsert,
		Leave: leave, Full: full, Until: h.now().Add(FrameValidity)}
}

// sendBudget bounds one broadcast. A core publish is a write into the
// client's buffer and returns at once on a connected broker, so a second is
// generous; a broker that is not answering is refused at once rather than
// waited on.
const sendBudget = time.Second

// send seals and broadcasts one frame. A frame that does not go out is logged
// and otherwise forgotten: the next rebroadcast says the same and more.
func (h *Hub) send(ctx context.Context, f Frame) {
	sealed, err := h.wire.Seal(f)
	if err != nil {
		log.ErrorContext(ctx, "presence_frame_not_sealed", "room", f.Room.String(),
			"error", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(ctx, sendBudget)
	defer cancel()
	if err := h.queue.Broadcast(ctx, f.Room.subject(), sealed); err != nil {
		log.DebugContext(ctx, "presence_frame_not_sent", "room", f.Room.String(),
			"error", err.Error(),
			"detail", "the next rebroadcast carries this node's members again")
	}
}

// heard is the listener every room this node listens on reports to: a peer's
// frame, opened and applied.
func (h *Hub) heard(_ string, payload []byte) {
	f, ok := h.open(payload)
	if !ok || f.Node == h.node || f.Kind != FrameState {
		// THIS NODE'S OWN FRAMES COME BACK: the broker echoes a publish
		// to every subscription that matches, the sender's included.
		return
	}
	h.count("peer")
	h.apply(f)
}

// open verifies and checks one frame, reporting false for one that must not
// be believed. A frame the fleet did not seal shows nobody.
func (h *Hub) open(payload []byte) (Frame, bool) {
	f, d, err := h.wire.Open(payload, h.now())
	switch d {
	case signedask.Admitted:
	case signedask.Tampered, signedask.KeyUnknown:
		log.Warn("presence_frame_refused", "disposition", string(d), "error", err.Error())
		return Frame{}, false
	default:
		log.Debug("presence_frame_refused", "disposition", string(d), "error", err.Error())
		return Frame{}, false
	}
	if err := f.check(); err != nil {
		log.Warn("presence_frame_refused", "disposition", "malformed", "error", err.Error())
		return Frame{}, false
	}
	return f, true
}

// apply lands one peer's frame on the room it names.
func (h *Hub) apply(f Frame) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[f.Room]
	if r == nil || !r.hasSockets() {
		// A ROOM NOBODY HERE IS WATCHING: nothing to show, and an
		// agent-only room never listens anyway.
		return
	}
	if r.hear(f, h.now()) {
		h.scheduleLocked(r)
	}
}

// answer is this node's reply to a peer that gained its first member of a
// room: every member this node holds there, sealed. A node holding none
// answers nothing, which the asker reads as a node with nobody in the room.
func (h *Hub) answer(_ context.Context, request []byte) ([]byte, error) {
	f, ok := h.open(request)
	if !ok || f.Kind != FrameAsk {
		return nil, errors.New("presence: not an ask this node can believe")
	}
	if f.Node == h.node {
		return nil, errors.New("presence: this node's own ask")
	}
	h.mu.Lock()
	r := h.rooms[f.Room]
	if r == nil || len(r.local) == 0 {
		h.mu.Unlock()
		return nil, errors.New("presence: nobody of this node's is in that room")
	}
	members := r.localMembers()
	out := h.frameLocked(f.Room, FrameState, members, nil, true)
	h.mu.Unlock()
	return h.wire.Seal(out)
}

// ask fills a room this node just started listening on from its peers, once,
// rather than leaving a newcomer to wait a rebroadcast interval.
func (h *Hub) ask(ctx context.Context, id Room) {
	h.mu.Lock()
	req := h.frameLocked(id, FrameAsk, nil, nil, false)
	h.mu.Unlock()
	sealed, err := h.wire.Seal(req)
	if err != nil {
		log.ErrorContext(ctx, "presence_ask_not_sealed", "error", err.Error())
		return
	}
	asked, cancel := context.WithTimeout(ctx, h.askBudget)
	defer cancel()
	// EACH ANSWER IS APPLIED AS IT ARRIVES. A scatter has no roster, so the
	// ask runs out its budget to hear whoever answers; applying the
	// answers only then showed a join everywhere after the whole budget
	// (two seconds, spike S6), past the second presence allows for it,
	// where a peer on the same broker answers in milliseconds.
	err = h.queue.AskEach(asked, topics.PresenceAsk, sealed, func(reply []byte) bool {
		f, ok := h.open(reply)
		if ok && f.Node != h.node && f.Kind == FrameState && f.Room == id {
			h.count("peer")
			h.apply(f)
		}
		return true
	})
	if err != nil {
		log.DebugContext(ctx, "presence_ask_failed", "room", id.String(),
			"error", err.Error(),
			"detail", "the room fills from the peers' next rebroadcast instead")
	}
}

// ticker runs the rebroadcast and the expiry every [Rebroadcast]
// until the hub stops.
func (h *Hub) ticker(ctx context.Context) {
	defer h.loops.Done()
	t := time.NewTicker(Rebroadcast)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.tick(ctx, h.now())
			// AND THE RECONCILER, which retries a listen that failed on
			// this cadence rather than on a timer of its own.
			h.poke()
		}
	}
}

// tick is one rebroadcast: every room's local members to the fleet, in full,
// after dropping the peers' members not heard for [Expiry]
// and the agents not touched for [agentLinger].
func (h *Hub) tick(ctx context.Context, now time.Time) {
	h.mu.Lock()
	var frames []Frame
	for id, r := range h.rooms {
		if r.expire(now) {
			h.scheduleLocked(r)     //nolint:contextcheck // the hub's own lifetime, ended by Stop, never a caller's; see Start
			h.scheduleSendLocked(r) //nolint:contextcheck // the hub's own lifetime, ended by Stop, never a caller's; see Start
		}
		if members := r.localMembers(); len(members) > 0 {
			frames = append(frames, h.frameLocked(id, FrameState, members, nil, true))
		}
	}
	h.mu.Unlock()
	for _, f := range frames {
		h.send(ctx, f)
	}
}

// poke asks the reconciler to look again; a request already pending covers
// this one.
func (h *Hub) poke() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// pokeLocked is [Hub.poke] under mu, which it does not need: named apart so a
// reader sees it is safe there.
func (h *Hub) pokeLocked() { h.poke() }

// reconciler keeps this node a listener on exactly the rooms one of its
// sockets is in, until the hub stops. ONE GOROUTINE, the only one that
// listens or withdraws, so the two never race for one room.
func (h *Hub) reconciler(ctx context.Context) {
	defer h.loops.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
		}
		h.reconcile(ctx)
	}
}

// reconcile listens on every room that wants it and withdraws from every room
// that does not. THE CALLS RUN OUTSIDE THE LOCK, because a withdrawal waits
// for the listener's handler, which takes it; the map of listens is read and
// written under it, so a reader elsewhere sees a listen only once it is held.
func (h *Hub) reconcile(ctx context.Context) {
	h.mu.Lock()
	var drop []Room
	var unsubs []queue.Unsubscribe
	for id, unsub := range h.listening {
		if r := h.rooms[id]; r == nil || !r.hasSockets() {
			drop = append(drop, id)
			unsubs = append(unsubs, unsub)
			delete(h.listening, id)
		}
	}
	var add []Room
	for id, r := range h.rooms {
		if _, held := h.listening[id]; !held && r.hasSockets() {
			add = append(add, id)
		}
	}
	h.mu.Unlock()

	for i, id := range drop {
		if err := unsubs[i](ctx); err != nil {
			log.WarnContext(ctx, "presence_listen_not_withdrawn", "room", id.String(),
				"error", err.Error())
		}
	}
	for _, id := range add {
		unsub, err := h.queue.Listen(ctx, id.subject(), h.heard)
		if err != nil {
			// RETRIED ON THE NEXT POKE, the rebroadcast's at the latest:
			// until then the room shows this node's own members only.
			log.InfoContext(ctx, "presence_listen_failed", "room", id.String(),
				"error", err.Error())
			continue
		}
		h.mu.Lock()
		h.listening[id] = unsub
		h.mu.Unlock()
		// ITS OWN GOROUTINE, so one room's ask never holds another's
		// listen, and counted with the hub's, so a stop waits for it: the
		// lifetime it runs under ends the ask at once.
		h.loops.Go(func() { h.ask(ctx, id) })
	}
	// A ROOM THAT EMPTIED WHILE ITS LISTEN WAS BEING MADE is withdrawn on
	// the next pass, which this asks for.
	if len(add) > 0 {
		h.poke()
	}
}

// context is the hub's own lifetime, or a background one before Start.
func (h *Hub) context() context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lifetime != nil {
		return h.lifetime
	}
	return context.Background()
}

func (h *Hub) count(direction string) {
	if h.metrics != nil {
		h.metrics.Add(metrics.PresenceFrames, 1, metrics.Attrs{"direction": direction})
	}
}

func (h *Hub) setRooms(n int) {
	if h.metrics != nil {
		h.metrics.Set(metrics.PresenceRooms, float64(n), nil)
	}
}
