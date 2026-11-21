package notify

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"
)

// The working indicator: "is thinking…" while an agent reasons. Backends differ
// in one way worth modelling: WHETHER IT CARRIES TEXT; where not, the phrase
// machinery goes INERT and a phase change costs no request.
//
// A raised status expires within minutes, so a session heartbeats at the
// poster's interval. Posting clears it; "gave up" has no backend signal, so a
// session clears explicitly when its last turn ends.
//
// Raises and re-assertions are made by the session's OWN GOROUTINE, never the
// turn: [StatusDriver.Begin] and [StatusSession.Phase] sit on its critical
// path, and a slow chat instance would delay real work. One writer also keeps
// what a backend hears totally ordered.

// # Stopped between posts, never cancelled in the middle of one
//
// CANCELLING A REQUEST ABANDONS IT; IT DOES NOT WITHDRAW IT: a cancelled raise
// is already on the wire, the clear takes a second connection, and the server
// may apply them in either order, leaving the indicator over an ended turn. So
// [StatusSession.End], [StatusDriver.ClearFor] and [StatusDriver.Stop] stop
// the goroutine between posts, let the post in flight finish, WAIT, and only
// then clear. BOUNDED by [statusRequestTimeout]; a post that runs out is the
// one case where the clear can be overtaken. The clear is DETACHED: the ending
// is often the cancellation itself.

// StatusMode is when an agent shows a working status.
type StatusMode string

const (
	// StatusOff disables the indicator.
	StatusOff StatusMode = "off"

	// StatusAddressed is the default: exactly the cases where a person is
	// plausibly waiting on THIS agent — whatever the backend's own
	// [AddressRule] says, which is a direct message, a personal mention,
	// or a thread the seat is in for one of those reasons.
	//
	// A broadcast, a passive top-level channel message and a thread the
	// seat merely spoke in are excluded deliberately. Every bot in the
	// channel wakes on those and the triage prompt tells most of them to
	// stay silent, so lighting up N indicators is noise rather than
	// signal.
	StatusAddressed StatusMode = "addressed"

	// StatusAlways shows it on every chat-triggered turn, which is what a
	// single-agent workspace wants.
	StatusAlways StatusMode = "always"
)

// Valid reports whether m is a mode the engine knows.
func (m StatusMode) Valid() bool {
	switch m {
	case StatusOff, StatusAddressed, StatusAlways:
		return true
	}
	return false
}

// Conversation is the thread a status belongs to.
type Conversation struct {
	Channel string
	Thread  string
}

// ConversationOf resolves the thread a turn's trigger points at, for one
// backend. It reports false for a trigger that came from somewhere else.
//
// The discriminator is the `transport` key every chat transport stamps; it
// survives inbox coalescing and the detached-sandbox round trip, so a resumed
// turn resolves the conversation of its kick-off and the indicator the person
// watches stays the one that clears.
//
// Raising needs a THREAD ANCHOR, and a top-level message's own id is it,
// through [Anchor], the value the chat prompt tells the agent to reply under,
// so the indicator appears where the reply will land.
func ConversationOf(metadata map[string]string, backend string) (Conversation, bool) {
	if len(metadata) == 0 || backend == "" || metadata[TransportField] != backend {
		return Conversation{}, false
	}
	channel, anchor := metadata[ChannelField], Anchor(metadata)
	if channel == "" || anchor == "" {
		return Conversation{}, false
	}
	return Conversation{Channel: channel, Thread: anchor}, true
}

// address is WHY a chat message is addressed to a seat, or that it is not.
//
// [AddressRule.Addressed] is the same answer with the reason dropped, and the
// chat prompt states the reason in place of its triage guidance
// ([ChatPrompt.Build]). BOTH READ ONE RULE, because the delivery check holds
// an addressed turn to an answer: a prompt that judged "addressed" by a rule
// of its own could tell a seat to stay silent on a message the engine then
// refuses to let it ignore.
type address int

const (
	addressNone address = iota
	// addressDirect: a direct conversation, where nobody else is there.
	addressDirect
	// addressMention: the seat was named personally ([FollowMention]).
	addressMention
	// addressThread: a message in a thread the seat follows for a reason its
	// backend's rule says obliges a reply, never merely because it spoke
	// there ([FollowReason.Addresses]).
	addressThread
	// addressRouted: a reason the message itself carries, which its backend's
	// rule counts and which is neither a direct conversation, a mention nor a
	// thread the seat follows — the backend routed the message to the seat to
	// answer. No thread is claimed for it: the seat may be in none.
	addressRouted
)

// addressOf decides [address] under one backend's [AddressRule], strongest
// reason first.
//
// BUILT FROM THE RULE'S OWN PARTS, [AddressRule.IsDirect] and the follows the
// backend stated, over the same two keys [AddressRule.Addressed] reads, so it
// names a reason for exactly the messages that method calls addressed. A
// mention the backend's rule does not count is no reason here either, or the
// prompt would tell a seat it was named on a message the rule leaves it free
// to pass over.
func addressOf(metadata map[string]string, rule AddressRule) address {
	reason := FollowReason(metadata[FollowReasonField])
	switch {
	case rule.IsDirect(metadata):
		return addressDirect
	case reason == FollowMention && rule.addressing(reason):
		return addressMention
	case rule.addressing(FollowReason(metadata[FollowingField])):
		return addressThread
	case rule.addressing(reason):
		return addressRouted
	}
	return addressNone
}

// StatusPoster is the backend call this driver drives, plus what it can
// express. Implemented by each chat transport, which holds the credentials.
type StatusPoster interface {
	// StatusBackend is the transport name, matched against the
	// `transport` key on a trigger's metadata.
	StatusBackend() string

	// SupportsStatusText reports whether the status string is rendered.
	// False for a fixed-vocabulary typing indicator, which makes the
	// phrase pools inert.
	SupportsStatusText() bool

	// StatusRefresh is the heartbeat interval, sized to this backend's
	// own expiry.
	StatusRefresh() time.Duration

	// AddressRule is this backend's answer to "was this message addressed
	// to this seat?", and it is the SAME VALUE its chat prompt carries —
	// declared once per backend and read here.
	//
	// The indicator says "this agent is working on your message" and the
	// delivery check says "this agent owes your message an answer". Two
	// rules would let one of them be right, so there is one; see
	// [AddressRule].
	AddressRule() AddressRule

	// SetStatus raises or updates the indicator. It never returns an
	// error: a failed call reports false and the status expires on the
	// backend's side, which is a cosmetic loss and not a turn's problem.
	SetStatus(ctx context.Context, post StatusPost) bool

	// ClearStatus takes the indicator down.
	//
	// SEPARATE from SetStatus with an empty string: the two are one operation
	// only on a backend that renders text. A typing indicator has no text to
	// empty and no clear at all (it lapses), so overloading the payload makes
	// "raise" and "clear" indistinguishable.
	//
	// Required, with no generic fallback: SetStatus with "" RAISES the
	// indicator on a poster that ignores its status argument.
	ClearStatus(ctx context.Context, handle, channel, thread string) bool
}

// StatusPost is one raise or re-assertion of an indicator: whose it is, the
// conversation it sits in, the turn holding it, and the words.
type StatusPost struct {
	Handle, Channel, Thread string

	// Turn is the run of the NEWEST turn holding the indicator — the one
	// that most recently began or took back its hold — which a backend that
	// can name a turn links to, so a person can open what the agent is
	// doing. A vendor's indicator names none and reads nothing here.
	Turn string

	// Text is the phrase, empty on a backend that renders none
	// ([StatusPoster.SupportsStatusText]).
	Text string

	// Queued says the work has not started: a person's message in this
	// conversation is waiting behind something the seat is already doing
	// ([StatusDriver.Queue]). A backend that can draw the difference draws
	// it; one that cannot shows Text, which says so in words.
	Queued bool
}

// statusRequestTimeout bounds every request an indicator makes: the raise,
// each re-assertion, and the clear.
//
// It bounds a teardown, which waits for at most the post in flight and then
// the clear (see "Stopped between posts"). Five seconds: long enough for a
// loaded chat instance to answer, short enough that draining a dozen seats
// against one that stopped answering takes seconds, not a vendor client's
// timeout per request. A request that runs out leaves one indicator standing
// until the backend's own expiry (about two minutes where text renders).
const statusRequestTimeout = 5 * time.Second

// statusKey identifies one live indicator.
type statusKey struct{ handle, channel, thread string }

// session is one live indicator, shared by every turn holding it open.
type session struct {
	key statusKey

	// holds are the turns holding the indicator open, in the order each
	// last began or took back its hold, so the newest is the last: the turn
	// a post names ([StatusPost.Turn]). Guarded by the driver's mutex.
	holds []string

	// waits are the person's messages in this conversation that are
	// waiting for the seat to finish what it is doing ([StatusDriver.Queue]),
	// each with the order it was queued in. They hold the indicator open
	// too: with no turn holding it, it says the message is queued. Guarded
	// by the driver's mutex.
	waits []wait

	// seed fixes which lines this conversation draws, and rotation counts
	// the phase changes it has been through — together they are what make
	// the text stable while a phase holds and different when it moves on.
	seed     string
	phase    string
	rotation int

	// poke asks this session's goroutine to re-assert the status NOW, which
	// is how a phase change reaches the backend without the turn waiting on
	// the request.
	//
	// Capacity one and a NON-BLOCKING send, because the goroutine posts
	// whatever the state says when it wakes rather than a queued value: a
	// second request landing while the first is in flight asks for the same
	// thing, and a phase superseded before its post reached the backend is
	// a phase the reader is better off never seeing.
	poke chan struct{}

	// stop asks the goroutine to exit at its next boundary between posts,
	// and done is closed once it has. Closed exactly once, by the teardown
	// that took the session out of the driver's map — which only one can do.
	//
	// A CHANNEL RATHER THAN A CANCELLED CONTEXT, because cancelling would
	// abandon the post in flight rather than let it finish, and an abandoned
	// raise can land after the clear. See "Stopped between posts" above.
	stop chan struct{}
	done chan struct{}
}

// wait is one person's message queued behind the seat's current work: the
// wake that carries it, the order it was queued in on this driver, and when.
type wait struct {
	wake string
	seq  uint64
	at   time.Time
}

// maxWaitAge is the longest a queued line stands for one message, the
// backstop under every way a wait ends (the picking-up turn, the dispatch
// that settles it, [StatusDriver.ClearFor], [StatusDriver.Stop]): one of those
// missed would be re-asserted on every heartbeat for the life of the process.
//
// ONE HOUR, a judgment rather than a measurement: nothing bounds how long a
// seat may legitimately be busy (an executor run has no time limit), so any
// bound can end a line still true. An hour is twice the ack window (jetstream
// DefaultAckWait), so only a seat held by an executor outlasts it, and past it
// the seat's own state says more. Ending the line never touches the message.
const maxWaitAge = time.Hour

// queued reports whether this indicator says "queued": somebody's message is
// waiting and no turn is working here yet. Caller holds the driver's mutex.
func (s *session) queued() bool { return len(s.holds) == 0 && len(s.waits) > 0 }

// firstWait is the order of this conversation's earliest waiting message.
// Caller holds the driver's mutex and has checked there is one.
func (s *session) firstWait() uint64 {
	first := s.waits[0].seq
	for _, w := range s.waits[1:] {
		first = min(first, w.seq)
	}
	return first
}

// hold records turn as the newest holder: appended, or moved to the end if it
// already holds. Caller holds the driver's mutex.
func (s *session) hold(turn string) {
	s.holds = append(slices.DeleteFunc(s.holds, func(t string) bool { return t == turn }), turn)
}

// newest is the turn a post names. Caller holds the driver's mutex.
func (s *session) newest() string {
	if len(s.holds) == 0 {
		return ""
	}
	return s.holds[len(s.holds)-1]
}

// wake asks the session's goroutine to re-assert, and never blocks. See
// [session.poke].
func (s *session) wake() {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// StatusOptions configure a [StatusDriver].
type StatusOptions struct {
	Poster  StatusPoster
	Mode    StatusMode
	Phrases Phrases

	// Now is the clock: how long a settled message is remembered
	// ([StatusDriver.Dequeue]) and how old a queued line is ([maxWaitAge]).
	// Nil is the wall clock, so a test need not supply one.
	Now func() time.Time
}

// StatusDriver owns every live working indicator on one backend.
//
// Sessions are keyed by (handle, channel, thread) and REFERENCE-COUNTED by
// turn id: two DIFFERENT turns for one agent in one thread (a queued
// follow-up, a colleague's ask) share a heartbeat, and the indicator clears
// when the last finishes, not while somebody is still waiting.
//
// A suspend/resume pair is ONE turn id, so its hold is one, taken back by
// [StatusDriver.Rejoin] rather than counted twice; ending a resumed turn then
// takes the indicator down instead of decrementing a count that never reached
// two.
type StatusDriver struct {
	poster  StatusPoster
	mode    StatusMode
	phrases Phrases
	now     func() time.Time

	mu       sync.Mutex
	sessions map[statusKey]*session
	stopped  bool

	// queuedSeq orders the waits this driver has queued, across every
	// conversation, so a message can say how many tasks are ahead of it.
	queuedSeq uint64

	// settled remembers the wakes [StatusDriver.Dequeue] was told about,
	// for [settledFor], so an announcement that arrives AFTER the turn that
	// picked its message up (the two travel separately) queues nothing: a
	// wait added then would hold the line up as "queued" once that turn
	// ends, for a message already answered.
	settled map[string]time.Time
}

// settledFor is how long a settled wake is remembered: far longer than an
// announcement takes to arrive, and short enough that the map stays small.
const settledFor = 5 * time.Minute

// NewStatusDriver builds the driver for one backend.
//
// A nil poster or a mode of off yields a driver whose Begin always reports
// no session — the feature disabled, with no branch at the call site. The
// turn engine should not have to ask whether indicators exist before saying
// what phase it is in.
func NewStatusDriver(opts StatusOptions) *StatusDriver {
	mode := opts.Mode
	if !mode.Valid() {
		mode = StatusAddressed
	}
	if opts.Poster == nil {
		mode = StatusOff
	}
	phrases := opts.Phrases
	if phrases.pools == nil {
		phrases = NewPhrases(nil)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &StatusDriver{
		poster:   opts.Poster,
		mode:     mode,
		phrases:  phrases,
		now:      now,
		sessions: map[statusKey]*session{},
	}
}

// Backend names the transport this driver posts through. Empty when off.
func (d *StatusDriver) Backend() string {
	if d.poster == nil {
		return ""
	}
	return d.poster.StatusBackend()
}

// Mode is the configured visibility.
func (d *StatusDriver) Mode() StatusMode { return d.mode }

// Shows reports whether a trigger with this metadata warrants an indicator.
//
// The mode decision, kept separate from Begin so a caller can ask before it
// has a turn id — and so the rule reads in one place rather than as an early
// return inside the lifecycle.
func (d *StatusDriver) Shows(metadata map[string]string) bool {
	switch d.mode {
	case StatusOff:
		return false
	case StatusAlways:
		return true
	default:
		return d.poster.AddressRule().Addressed(metadata)
	}
}

// StatusSession is one turn's hold on an indicator.
//
// A nil session is valid and every method on it is a no-op, so a caller
// never has to check: the feature being off, the trigger not being a chat
// message, and the turn not being addressed are all "no indicator", and a
// caller that had to distinguish them would grow three branches to say
// nothing.
type StatusSession struct {
	driver *StatusDriver
	key    statusKey
	turn   string
}

// Begin opens or joins a session for a turn.
//
// Reports nil when there is no indicator to raise — the feature is off, the
// trigger did not come from this backend, or nobody is waiting on this agent.
func (d *StatusDriver) Begin(ctx context.Context, handle, turnID, phase string, metadata map[string]string) *StatusSession {
	if d.mode == StatusOff || handle == "" || turnID == "" {
		return nil
	}
	conv, ok := ConversationOf(metadata, d.poster.StatusBackend())
	if !ok || !d.Shows(metadata) {
		return nil
	}

	key := statusKey{handle: handle, channel: conv.Channel, thread: conv.Thread}
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return nil
	}
	if s, live := d.sessions[key]; live {
		// JOIN, do not re-raise. The indicator is already up and its
		// heartbeat is running; a second raise would reset the text a
		// reader is watching to this turn's line for no reason.
		//
		// UNLESS IT SAID "QUEUED": the work it was waiting for has just
		// started, so the same indicator moves on to this turn's phase in
		// place, rather than coming down and going up again under the
		// person watching it.
		started := s.queued()
		s.hold(turnID)
		if started {
			s.phase = phase
			s.rotation++
		}
		d.mu.Unlock()
		if started {
			s.wake()
		}
		return &StatusSession{driver: d, key: key, turn: turnID}
	}

	s := &session{
		key: key, holds: []string{turnID},
		// Seeded on the TURN, not the thread: two turns in one thread
		// should not read as one long thought, and the seed is what
		// makes their lines differ.
		seed: turnID, phase: phase,
		poke: make(chan struct{}, 1),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	// DETACHED FROM THE CALLER'S CONTEXT, because the indicator outlives the
	// call that raised it in the one case it matters most: a turn that
	// suspends on a detached executor ends its own context and keeps the
	// indicator up (see [StatusSession.End]). Its values stay — the trace a
	// debug line is filed under — and nothing cancels it: the goroutine ends
	// on [session.stop], and each request on its own deadline.
	loopCtx := context.WithoutCancel(ctx)
	d.sessions[key] = s
	d.mu.Unlock()

	// THE RAISE IS THE GOROUTINE'S FIRST ACT, not this function's last (see
	// the file's opening comment): the turn never waits on a chat backend.
	go d.run(loopCtx, s)
	return &StatusSession{driver: d, key: key, turn: turnID}
}

// Rejoin takes back the hold a turn already has on a live indicator.
//
// A RESUMED turn calls it: one that suspended with keepAlive left its
// indicator up and needs the hold back to end it for real. It CANNOT call
// Begin, since the parked run's row carries no chat metadata.
//
// Addressed by (handle, turn id) for the same reason; it never raises, posts
// or creates. A session not held here answers nil, the honest answer where
// the seat moved or the process restarted (the indicator lapses on the
// backend's expiry; a fresh raise would claim a conversation it cannot prove
// it is in). An empty handle or turn id needs no guard: nothing is keyed on
// one.
func (d *StatusDriver) Rejoin(handle, turnID string) *StatusSession {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, s := range d.sessions {
		if key.handle == handle && slices.Contains(s.holds, turnID) {
			// THE RESUMED TURN IS THE NEWEST: it is the one working now.
			s.hold(turnID)
			return &StatusSession{driver: d, key: key, turn: turnID}
		}
	}
	return nil
}

// ClearFor takes down every indicator this driver holds for one seat.
//
// Called when this node stops running the seat. A kept-alive session is the
// reason it has to exist: a turn that suspended on a detached executor
// leaves its indicator up deliberately, and the resume lands on whichever node
// holds the seat WHEN THE BOX FINISHES. Left alone, a node that has handed the
// seat on would re-assert "is thinking…" every refresh interval, for a turn it
// is no longer running, for as long as the process lives — and on the backend
// whose indicator expires in seconds that is a request every couple of seconds
// for ever.
func (d *StatusDriver) ClearFor(ctx context.Context, handle string) {
	d.mu.Lock()
	var live []*session
	for key, s := range d.sessions {
		if key.handle != handle {
			continue
		}
		live = append(live, s)
		delete(d.sessions, key)
	}
	d.mu.Unlock()

	d.retire(ctx, live)
}

// Phase moves the indicator to a new phase's wording.
//
// NO CONTEXT, deliberately: this writes the state and wakes the session's own
// goroutine, which makes the request. The caller is a turn about to make its
// first provider call of a new phase, and a context here would be a context
// the request does not run under — see the third property in this file's
// opening comment.
//
// A no-op on a backend that cannot render text: the session still runs and
// the heartbeat still keeps the indicator alive, but a phase change stops
// costing a request, because there is nothing about it a reader could see.
func (s *StatusSession) Phase(phase string) {
	if s == nil || s.driver == nil {
		return
	}
	d := s.driver
	if !d.poster.SupportsStatusText() {
		return
	}
	d.mu.Lock()
	sess, live := d.sessions[s.key]
	if !live || sess.phase == phase {
		d.mu.Unlock()
		return
	}
	sess.phase = phase
	// Rotation advances on every phase change, so a turn that revisits a
	// phase — Execute, Review, Execute again after a self-iterate — does
	// not repeat the same line and read as though nothing moved.
	sess.rotation++
	d.mu.Unlock()
	sess.wake()
}

// End releases this turn's hold.
//
// The indicator comes down only when the LAST holder ends. keepAlive holds it
// up regardless — the suspend case, where this same turn resumes once a
// detached executor completes and the person is still waiting. The resumed
// half takes this hold back with [StatusDriver.Rejoin], and ends it without
// keepAlive; a node that never sees that resume takes it down when it stops
// running the seat ([StatusDriver.ClearFor]).
//
// Taking it down waits for the post in flight to finish and then clears, on a
// context detached from ctx and bounded — so ctx may be the turn's own,
// cancelled or not. See "Stopped between posts" at the top of this file.
func (s *StatusSession) End(ctx context.Context, keepAlive bool) {
	if s == nil || s.driver == nil || keepAlive {
		return
	}
	d := s.driver
	d.mu.Lock()
	sess, live := d.sessions[s.key]
	if !live {
		d.mu.Unlock()
		return
	}
	sess.holds = slices.DeleteFunc(sess.holds, func(t string) bool { return t == s.turn })
	if len(sess.holds) > 0 {
		d.mu.Unlock()
		return
	}
	if len(sess.waits) > 0 {
		// A MESSAGE STILL WAITING here keeps the indicator up, now
		// saying it is queued: the turn that ended was not the one it
		// waits for.
		d.mu.Unlock()
		sess.wake()
		return
	}
	delete(d.sessions, s.key)
	d.mu.Unlock()

	d.retire(ctx, []*session{sess})
}

// Queue raises the indicator for a person's message that is waiting behind
// something the seat is already doing, saying it is queued and how many tasks
// are ahead of it; it reports whether this driver took it.
//
// THE SAME INDICATOR A TURN RAISES, keyed on the same conversation: the turn
// that picks the message up joins it ([StatusDriver.Begin]) and moves it to
// its phase in place, and [StatusDriver.Dequeue] takes the wait away. The
// lifecycle is otherwise unchanged ([StatusDriver.ClearFor] included).
//
// ONLY WHERE WORDS ARE DRAWN: a composer typing line says "writing" whatever
// it is told, and a queued message is not being written, so there this raises
// nothing.
func (d *StatusDriver) Queue(ctx context.Context, handle, wake string, metadata map[string]string) bool {
	if d.mode == StatusOff || handle == "" || wake == "" || !d.poster.SupportsStatusText() {
		return false
	}
	conv, ok := ConversationOf(metadata, d.poster.StatusBackend())
	if !ok || !d.Shows(metadata) {
		return false
	}
	key := statusKey{handle: handle, channel: conv.Channel, thread: conv.Thread}
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return false
	}
	if _, done := d.settled[handle+"\x00"+wake]; done {
		d.mu.Unlock()
		return false
	}
	d.queuedSeq++
	now := d.now()
	if s, live := d.sessions[key]; live {
		if !slices.ContainsFunc(s.waits, func(w wait) bool { return w.wake == wake }) {
			s.waits = append(s.waits, wait{wake: wake, seq: d.queuedSeq, at: now})
		}
		queued := s.queued()
		d.mu.Unlock()
		if queued {
			s.wake()
		}
		return true
	}
	s := &session{
		key: key, waits: []wait{{wake: wake, seq: d.queuedSeq, at: now}},
		seed: wake,
		poke: make(chan struct{}, 1),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	loopCtx := context.WithoutCancel(ctx)
	d.sessions[key] = s
	d.mu.Unlock()
	go d.run(loopCtx, s)
	return true
}

// Dequeue takes away the waits of the given wakes for one seat: each message
// has been picked up by a turn or settled without one. An indicator held by
// nothing else comes down; one a turn holds carries on; one still holding
// another waiting message stays queued, and every queued indicator of the
// seat is told again how many tasks are ahead of it.
func (d *StatusDriver) Dequeue(ctx context.Context, handle string, wakes []string) {
	if handle == "" || len(wakes) == 0 {
		return
	}
	d.mu.Lock()
	d.rememberSettled(handle, wakes)
	var gone, again []*session
	removed := false
	for key, s := range d.sessions {
		if key.handle != handle {
			continue
		}
		before := len(s.waits)
		s.waits = slices.DeleteFunc(s.waits, func(w wait) bool { return slices.Contains(wakes, w.wake) })
		if len(s.waits) == before {
			continue
		}
		removed = true
		if len(s.holds) == 0 && len(s.waits) == 0 {
			delete(d.sessions, key)
			gone = append(gone, s)
		}
	}
	if removed {
		for key, s := range d.sessions {
			if key.handle == handle && s.queued() {
				again = append(again, s)
			}
		}
	}
	d.mu.Unlock()
	for _, s := range again {
		s.wake()
	}
	d.retire(ctx, gone)
}

// rememberSettled records wakes a dispatch has finished with, and forgets the
// ones older than [settledFor]. Caller holds the driver's mutex.
func (d *StatusDriver) rememberSettled(handle string, wakes []string) {
	now := d.now()
	if d.settled == nil {
		d.settled = map[string]time.Time{}
	}
	for key, at := range d.settled {
		if now.Sub(at) > settledFor {
			delete(d.settled, key)
		}
	}
	for _, wake := range wakes {
		d.settled[handle+"\x00"+wake] = now
	}
}

// queuedAhead is how many tasks are ahead of a queued indicator's earliest
// message: the one the seat is doing now, and every message of the same seat
// queued before it on this driver. Caller holds the driver's mutex.
func (d *StatusDriver) queuedAhead(s *session) int {
	first := s.firstWait()
	ahead := 1
	for key, other := range d.sessions {
		if key.handle != s.key.handle || other == s {
			continue
		}
		for _, w := range other.waits {
			if w.seq < first {
				ahead++
			}
		}
	}
	return ahead
}

// queuedLine is what a queued indicator says beside the seat's name.
func queuedLine(ahead int) string {
	if ahead <= 1 {
		return "is queued behind its current task"
	}
	return "is queued behind " + strconv.Itoa(ahead) + " tasks"
}

// Conversation is the thread this session's indicator sits in.
func (s *StatusSession) Conversation() Conversation {
	if s == nil {
		return Conversation{}
	}
	return Conversation{Channel: s.key.channel, Thread: s.key.thread}
}

// Live lists the conversations currently showing an indicator, for the
// operator surface that answers "what does this node think it is doing".
func (d *StatusDriver) Live() []Conversation {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Conversation, 0, len(d.sessions))
	for key := range d.sessions {
		out = append(out, Conversation{Channel: key.channel, Thread: key.thread})
	}
	slices.SortFunc(out, func(a, b Conversation) int {
		return cmp.Or(cmp.Compare(a.Channel, b.Channel), cmp.Compare(a.Thread, b.Thread))
	})
	return out
}

// Stop tears down every live indicator.
//
// It CLEARS them rather than letting them lapse. A node shutting down leaves
// indicators up for the whole of a backend's expiry otherwise, telling
// everyone watching that an agent is working on something this process is
// about to stop doing.
func (d *StatusDriver) Stop(ctx context.Context) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	d.stopped = true
	live := make([]*session, 0, len(d.sessions))
	for key, s := range d.sessions {
		live = append(live, s)
		delete(d.sessions, key)
	}
	d.mu.Unlock()

	d.retire(ctx, live)
}

// retire takes down the indicators of sessions already out of the driver's
// map, in the one order that keeps each clear last: stop its goroutine, wait
// for the post in flight to FINISH, then clear (see "Stopped between posts").
//
// ALL AT ONCE, so the call is bounded by one post and one clear however many
// indicators it takes down: every stop is asked for before any wait, and one
// after another a dozen indicators against an unanswering chat instance would
// wait out a dozen budgets in turn.
//
// ctx may be dead already (the ending is often the cancellation itself): each
// clear detaches from it and carries its own deadline ([StatusDriver.clear]).
func (d *StatusDriver) retire(ctx context.Context, sessions []*session) {
	for _, s := range sessions {
		close(s.stop)
	}
	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Go(func() {
			<-s.done
			d.clear(ctx, s.key)
		})
	}
	wg.Wait()
}

// run is the session's own goroutine, and the ONLY writer of its indicator:
// the opening raise, every phase change and every re-assertion inside the
// backend's expiry window are all this loop.
//
// It exits only at a boundary BETWEEN posts, and the ORDERING is why nothing
// else needs checking: End, ClearFor and Stop all ask it to stop and wait for
// it to exit BEFORE they clear the indicator, and the post it was making when
// they asked is allowed to finish. So a post can never follow a clear — the
// worst case is one extra post that the clear immediately takes down.
func (d *StatusDriver) run(ctx context.Context, s *session) {
	defer close(s.done)
	// THE RAISE. Skipped only where the session is already over, which is a
	// turn that ended before its own indicator went up: posting then would
	// raise an indicator for nobody, and the clear that follows would be
	// taking down something the reader never saw.
	if s.stopped() {
		return
	}
	d.repost(ctx, s)

	// A poster that declares no interval gets NO ticker rather than a spin:
	// its indicator lapses, which is a cosmetic loss, where a zero-interval
	// ticker is a hot loop against a third-party app's rate limiter. It
	// still serves phase changes — a nil channel blocks in the select below
	// for ever, which is exactly "no heartbeat".
	var tick <-chan time.Time
	if interval := d.poster.StatusRefresh(); interval > 0 {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		tick = ticker.C
	} else {
		log.WarnContext(ctx, "status_poster_declares_no_refresh", "backend", d.poster.StatusBackend())
	}
	for {
		select {
		case <-s.stop:
			return
		case <-s.poke:
		case <-tick:
		}
		// A STOP THAT ARRIVED WITH THE WAKE WINS: select picks among
		// ready cases at random, and a re-assertion made after the
		// teardown asked for none is one more request for it to wait out.
		if s.stopped() {
			return
		}
		// AND A WAIT PAST ITS AGE ENDS HERE ([maxWaitAge]). An indicator
		// it was the last thing holding comes down: this goroutine took it
		// out of the map, so no teardown will, and it is between posts, so
		// its clear cannot be overtaken by one.
		if d.expire(s) {
			d.clear(ctx, s.key)
			return
		}
		d.repost(ctx, s)
	}
}

// expire drops the session's waits older than [maxWaitAge], and reports
// whether that left it holding nothing, in which case it has also been taken
// out of the driver's map. A session a teardown already took out is the
// teardown's to clear, and is left to it.
func (d *StatusDriver) expire(s *session) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(s.waits) == 0 {
		return false
	}
	now := d.now()
	s.waits = slices.DeleteFunc(s.waits, func(w wait) bool { return now.Sub(w.at) > maxWaitAge })
	if len(s.holds) > 0 || len(s.waits) > 0 || d.sessions[s.key] != s {
		return false
	}
	delete(d.sessions, s.key)
	return true
}

// stopped reports whether a teardown has asked this session's goroutine to
// exit.
func (s *session) stopped() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

// repost asserts whatever the session's state says NOW.
//
// The state rather than a value handed in, so a phase change that landed while
// the previous request was in flight is picked up by this one instead of being
// posted after it — which is what makes a coalesced [session.wake] correct
// rather than lossy.
func (d *StatusDriver) repost(ctx context.Context, s *session) {
	d.mu.Lock()
	queued := s.queued()
	var text string
	if queued {
		text = queuedLine(d.queuedAhead(s))
	} else {
		text = d.phrases.Pick(s.phase, s.seed, s.rotation)
	}
	turn := s.newest()
	d.mu.Unlock()
	d.post(ctx, s.key, turn, text, queued)
}

func (d *StatusDriver) post(ctx context.Context, key statusKey, turn, text string, queued bool) {
	ctx, cancel := context.WithTimeout(ctx, statusRequestTimeout)
	defer cancel()
	if !d.poster.SupportsStatusText() {
		// The indicator still goes up; the words are the part this
		// backend cannot render.
		text = ""
	}
	if !d.poster.SetStatus(ctx, StatusPost{Handle: key.handle, Channel: key.channel,
		Thread: key.thread, Turn: turn, Text: text, Queued: queued}) {
		log.DebugContext(ctx, "working_status_not_raised", "backend", d.poster.StatusBackend(),
			"handle", key.handle, "channel", key.channel)
	}
}

// clear takes one indicator down, on a context of its own: detached, because
// the ending being reported is often the caller's cancellation, and bounded,
// because detaching also dropped the caller's deadline.
func (d *StatusDriver) clear(ctx context.Context, key statusKey) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusRequestTimeout)
	defer cancel()
	if !d.poster.ClearStatus(ctx, key.handle, key.channel, key.thread) {
		log.DebugContext(ctx, "working_status_not_cleared", "backend", d.poster.StatusBackend(),
			"handle", key.handle, "channel", key.channel)
	}
}

// String renders a conversation for a log line.
func (c Conversation) String() string {
	return fmt.Sprintf("%s:%s", c.Channel, c.Thread)
}
