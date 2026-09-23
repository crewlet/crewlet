package notify

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// The working indicator: "is thinking…" while an agent reasons.
//
// An agent turn takes minutes. Without a signal the person who posted sees
// nothing until the reply lands, and cannot tell "the bot is working" from
// "the bot is dead". Every chat backend offers some way to close that gap,
// and they differ in exactly one way worth modelling: WHETHER THE INDICATOR
// CARRIES TEXT.
//
// One backend renders free text ("Agent SWE is crewleting…"). Another offers
// only the composer typing indicator, whose wording its client fixes — the
// engine can raise it but cannot say anything with it. A poster declares
// which it is, and where text is unsupported the phrase machinery goes
// INERT: the session still runs, keeping the indicator alive, but a phase
// change stops costing a request, because there is nothing about it a reader
// could see.
//
// # Two properties of every backend's mechanism shape the lifecycle
//
// A RAISED STATUS EXPIRES — in a couple of minutes at most — so a session
// keeps a heartbeat that re-asserts well inside that window, at the poster's
// own interval.
//
// POSTING INTO THE CONVERSATION CLEARS IT, which covers the "agent replied"
// half for free. The "agent gave up" half — the agent decided the message
// was not for it, the turn failed, the budget ran out — has no backend-side
// signal at all, so a session always clears explicitly when its last turn
// ends.
//
// # And one property of the CALLER shapes where the requests are made
//
// EVERY RAISE AND EVERY RE-ASSERTION IS MADE BY THE SESSION'S OWN GOROUTINE,
// never by the turn that asked for it. Both calls sit on a turn's critical
// path — [StatusDriver.Begin] runs before the turn assembles its context, and
// [StatusSession.Phase] runs immediately before a phase's first provider call
// — so a chat instance that takes its client timeout to answer would delay
// the agent's actual work by that long, for a cosmetic surface whose every
// other failure is swallowed. So those two only write the state and wake the
// goroutine, which is also what makes the ORDER of what a backend hears total:
// one writer, so a re-assertion can never overtake the raise it belongs to.
//
// The teardown is the deliberate exception. [StatusSession.End],
// [StatusDriver.ClearFor] and [StatusDriver.Stop] STOP the goroutine at its
// next boundary between posts, WAIT for it, and only then clear — because the
// clear has to be the last thing the backend hears, and a process that exits
// before it lands leaves an indicator claiming an agent is working for the
// whole of the backend's expiry.
//
// # Stopped between posts, never cancelled in the middle of one
//
// CANCELLING A REQUEST ABANDONS IT; IT DOES NOT WITHDRAW IT. The teardown used
// to cancel the goroutine's context, which made an in-flight raise or
// re-assertion return at once — while the request itself was already on the
// wire, or already in the server's hands. The clear then went out on a second
// connection, and the server was free to apply the two in either order: when
// the abandoned raise landed second, the indicator stayed up over a turn that
// had ended, with nothing left to take it down but the backend's own expiry.
// So a teardown asks the loop to stop and lets the post in flight finish:
// once its response is back, the server has applied it, and the clear that
// follows is ordered after it for real. A backend with no clear at all (the
// typing indicator) gets the same guarantee from the same rule — nothing it
// was sent can arrive after the turn that raised it is over.
//
// BOUNDED, because the wait is now for a real request: every request an
// indicator makes runs under [statusRequestTimeout], the clear included, so a
// teardown waits at most one post and one clear however slow the backend is.
// A post that runs out of that time is abandoned after all, and its fate is
// the one thing no client can know; that is the only case left in which the
// clear can be overtaken, and the indicator then lapses on the backend's own
// expiry.
//
// DETACHED, the clear especially: the ending being reported is often the
// cancellation itself — a shed seat, a drained node, a turn that ran out of
// time — and a clear made on a dead context does nothing at all.

// StatusMode is when an agent shows a working status.
type StatusMode string

const (
	// StatusOff disables the indicator.
	StatusOff StatusMode = "off"

	// StatusAddressed is the default: exactly the cases where a person is
	// plausibly waiting on THIS agent — a direct message, a personal
	// mention, or a thread it already follows.
	//
	// A broadcast and a passive top-level channel message are excluded
	// deliberately. Every bot in the channel wakes on those and the triage
	// prompt tells most of them to stay silent, so lighting up N
	// indicators is noise rather than signal.
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

// DirectChannelTypes are every chat backend's spelling of "this is a private
// conversation, not a room".
//
// A normalised vocabulary each transport maps its own onto, so the decision
// below never has to know a backend's id conventions.
var DirectChannelTypes = []string{"im", "mpim", "D", "G"}

// Conversation is the thread a status belongs to.
type Conversation struct {
	Channel string
	Thread  string
}

// ConversationOf resolves the thread a turn's trigger points at, for one
// backend. It reports false for a trigger that came from somewhere else.
//
// The discriminator is the `transport` key every chat transport stamps on
// what it parses. That key survives inbox coalescing — the merged event
// mirrors the latest constituent's metadata — and the detached-sandbox round
// trip, so a resumed turn resolves the same conversation as its kick-off and
// the indicator the person is watching stays the one that clears.
//
// Raising a status needs a THREAD ANCHOR, and a top-level message has no
// thread yet — so its own id is the anchor. That is the same value the chat
// prompt tells the agent to reply under, which is what makes the indicator
// appear where the reply will land rather than somewhere else in the channel.
func ConversationOf(metadata map[string]string, backend string) (Conversation, bool) {
	if len(metadata) == 0 || backend == "" || metadata[TransportField] != backend {
		return Conversation{}, false
	}
	channel := metadata["channel"]
	anchor := metadata["thread_ts"]
	if anchor == "" {
		anchor = metadata["ts"]
	}
	if channel == "" || anchor == "" {
		return Conversation{}, false
	}
	return Conversation{Channel: channel, Thread: anchor}, true
}

// Addressed reports whether a person is plausibly waiting on THIS agent.
//
// dmPrefix is the belt-and-braces fallback for the one backend with a
// meaningful one, where a channel id beginning with a known letter is always
// a direct message — so the answer stays right even if metadata arrives
// without a channel type at all.
//
// It is OPT-IN PER BACKEND and must stay empty where channel ids are opaque.
// A backend whose ids are arbitrary alphanumerics would mark random public
// channels as direct messages, and raise an indicator on every seat for
// traffic nobody addressed to any of them.
func Addressed(metadata map[string]string, dmPrefix string) bool {
	if slices.Contains(DirectChannelTypes, metadata["channel_type"]) {
		return true
	}
	if dmPrefix != "" && strings.HasPrefix(metadata["channel"], dmPrefix) {
		return true
	}
	if metadata["thread_follow_reason"] == string(FollowMention) {
		return true
	}
	return metadata["thread_following"] != ""
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

	// DMChannelPrefix marks a direct message unambiguously on this
	// backend, or is empty where its ids are opaque. See [Addressed].
	DMChannelPrefix() string

	// SetStatus raises or updates the indicator. It never returns an
	// error: a failed call reports false and the status expires on the
	// backend's side, which is a cosmetic loss and not a turn's problem.
	SetStatus(ctx context.Context, handle, channel, thread, status string) bool

	// ClearStatus takes the indicator down.
	//
	// SEPARATE from SetStatus with an empty string, because the two are
	// only the same operation on a backend that renders text. One clears
	// by setting an empty status; a fixed-vocabulary typing indicator has
	// no text to empty and no clear operation at all — it lapses on its
	// own — so overloading the payload makes "raise" and "clear"
	// indistinguishable, and such a backend has to refuse both.
	//
	// Required, and deliberately with no generic fallback: the only
	// candidate — SetStatus with "" — RAISES the indicator on a poster
	// that ignores its status argument, which is the entire class of
	// backend this method exists for.
	ClearStatus(ctx context.Context, handle, channel, thread string) bool
}

// statusRequestTimeout bounds every request an indicator makes: the raise,
// each re-assertion, and the clear.
//
// ONE REQUEST, ONE BUDGET, and it is what bounds a teardown, which waits for at
// most the post in flight and then makes the clear (see the opening comment).
// Five seconds: long enough for a chat instance under load to answer, short
// enough that a drain of a dozen seats against one that has stopped answering
// finishes in seconds rather than waiting out a vendor client's timeout per
// request, on the one surface whose every failure is swallowed as cosmetic.
// What a request that runs out loses is one indicator left standing until the
// backend's own expiry lapses it — about two minutes on the surface that
// renders text, seconds on the one that does not.
const statusRequestTimeout = 5 * time.Second

// statusKey identifies one live indicator.
type statusKey struct{ handle, channel, thread string }

// session is one live indicator, shared by every turn holding it open.
type session struct {
	key   statusKey
	turns map[string]bool

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

	// Now is the clock; only used for logging cadence, so a test need not
	// supply one.
	Now func() time.Time
}

// StatusDriver owns every live working indicator on one backend.
//
// Sessions are keyed by (handle, channel, thread) and REFERENCE-COUNTED by
// turn id, so two DIFFERENT turns for one agent in one thread — a queued
// follow-up, or a colleague's ask arriving mid-conversation — share a
// heartbeat and the indicator clears only when the last of them finishes.
// Clearing on the first would take the indicator down while somebody is still
// waiting.
//
// A suspend/resume pair is NOT that case and must not be confused with it: it
// is ONE turn id, so its hold is one, taken back by [StatusDriver.Rejoin]
// rather than counted twice — which is what makes ending a resumed turn take
// the indicator down instead of decrementing a count that never reached two.
type StatusDriver struct {
	poster  StatusPoster
	mode    StatusMode
	phrases Phrases

	mu       sync.Mutex
	sessions map[statusKey]*session
	stopped  bool
}

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
	return &StatusDriver{
		poster:   opts.Poster,
		mode:     mode,
		phrases:  phrases,
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
		return Addressed(metadata, d.poster.DMChannelPrefix())
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
		s.turns[turnID] = true
		d.mu.Unlock()
		return &StatusSession{driver: d, key: key, turn: turnID}
	}

	s := &session{
		key: key, turns: map[string]bool{turnID: true},
		// Seeded on the TURN, not the thread: two turns in one thread
		// should not read as one long thought, and the seed is what
		// makes their lines differ.
		seed: turnID, phase: phase,
		poke: make(chan struct{}, 1),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	// DETACHED FROM THE CALLER'S CONTEXT, because the indicator outlives the
	// call that raised it in the one case it matters most: a turn that
	// suspends into a detached coding run ends its own context and keeps the
	// indicator up (see [StatusSession.End]). Its values stay — the trace a
	// debug line is filed under — and nothing cancels it: the goroutine ends
	// on [session.stop], and each request on its own deadline.
	loopCtx := context.WithoutCancel(ctx)
	d.sessions[key] = s
	d.mu.Unlock()

	// THE RAISE IS THE GOROUTINE'S FIRST ACT, not this function's last: see
	// the third property in this file's opening comment. Begin returns as
	// soon as the session exists, so the turn it belongs to never waits on a
	// chat backend to start working.
	go d.run(loopCtx, s)
	return &StatusSession{driver: d, key: key, turn: turnID}
}

// Rejoin takes back the hold a turn already has on a live indicator.
//
// It is what a RESUMED turn calls. A turn that suspended into a detached
// coding run ended with keepAlive, so its indicator is still up and still
// heartbeating; the turn that re-enters that conversation minutes later needs
// the hold back so it can end it for real — and it CANNOT call Begin, because
// a resume has no trigger to raise from: the parked run's own row deliberately
// carries no chat metadata (the message that woke the suspended turn may be
// days gone, and a coalescing key is a partition rather than an address).
//
// Addressed by (handle, turn id) rather than by conversation for the same
// reason, and it never raises, never posts and never creates: a session this
// driver does not hold answers nil, which is the honest answer where the seat
// moved node or the process restarted — the indicator then lapses on the
// backend's own expiry, and a resumed turn that raised a fresh one would be
// claiming a conversation it cannot prove it is in.
//
// No guard on an empty handle or turn id, because the match below already is
// one: no live session is keyed on an empty handle ([StatusDriver.Begin]
// refuses one) and no hold is recorded under an empty turn id, so both answer
// nil by the same rule everything else does.
func (d *StatusDriver) Rejoin(handle, turnID string) *StatusSession {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, s := range d.sessions {
		if key.handle == handle && s.turns[turnID] {
			return &StatusSession{driver: d, key: key, turn: turnID}
		}
	}
	return nil
}

// ClearFor takes down every indicator this driver holds for one seat.
//
// Called when this node stops running the seat. A kept-alive session is the
// reason it has to exist: a turn that suspended into a detached coding run
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
// detached coding run completes and the person is still waiting. The resumed
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
	delete(sess.turns, s.turn)
	if len(sess.turns) > 0 {
		d.mu.Unlock()
		return
	}
	delete(d.sessions, s.key)
	d.mu.Unlock()

	d.retire(ctx, []*session{sess})
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
// map, each in the one order that keeps its clear last: stop its goroutine at
// the next boundary between posts, wait for the post in flight to FINISH, then
// clear. Never cancel that post — see "Stopped between posts" in the opening
// comment.
//
// ALL AT ONCE rather than one after another, so the call is bounded by one
// post and one clear however many indicators it takes down: every stop is
// asked for before any wait, and each clear goes out as soon as its own
// session's goroutine is gone. One after another, a node stopping with a
// dozen live indicators against a chat instance that had stopped answering
// would wait out a dozen budgets in turn.
//
// ctx may be dead already — the ending being reported is often the
// cancellation itself — because each clear detaches from it and carries its
// own deadline ([StatusDriver.clear]); the waits take nothing from it at all.
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
		d.repost(ctx, s)
	}
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
	text := d.phrases.Pick(s.phase, s.seed, s.rotation)
	d.mu.Unlock()
	d.post(ctx, s.key, text)
}

func (d *StatusDriver) post(ctx context.Context, key statusKey, text string) {
	ctx, cancel := context.WithTimeout(ctx, statusRequestTimeout)
	defer cancel()
	if !d.poster.SupportsStatusText() {
		// The indicator still goes up; the words are the part this
		// backend cannot render.
		text = ""
	}
	if !d.poster.SetStatus(ctx, key.handle, key.channel, key.thread, text) {
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
