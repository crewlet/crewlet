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
// half for free. The "agent gave up" half — the planner decided the message
// was not for it, the turn failed, the budget ran out — has no backend-side
// signal at all, so a session always clears explicitly when its last turn
// ends.

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
	if len(metadata) == 0 || backend == "" || metadata["transport"] != backend {
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

	cancel context.CancelFunc
	done   chan struct{}
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
// turn id, so two turns for one agent in one thread — a suspend/resume pair,
// or a queued follow-up — share a heartbeat and the indicator clears only
// when the last of them finishes. Clearing on the first would take the
// indicator down while somebody is still waiting.
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
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel, s.done = cancel, make(chan struct{})
	d.sessions[key] = s
	text := d.phrases.Pick(phase, s.seed, 0)
	d.mu.Unlock()

	d.post(ctx, key, text)
	go d.heartbeat(loopCtx, s)
	return &StatusSession{driver: d, key: key, turn: turnID}
}

// Phase moves the indicator to a new phase's wording.
//
// A no-op on a backend that cannot render text: the session still runs and
// the heartbeat still keeps the indicator alive, but a phase change stops
// costing a request, because there is nothing about it a reader could see.
func (s *StatusSession) Phase(ctx context.Context, phase string) {
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
	text := d.phrases.Pick(phase, sess.seed, sess.rotation)
	d.mu.Unlock()
	d.post(ctx, s.key, text)
}

// End releases this turn's hold.
//
// The indicator comes down only when the LAST holder ends. keepAlive holds
// it up regardless — the suspend/resume case, where the same turn id resumes
// once a detached coding run completes and the person is still waiting.
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

	sess.cancel()
	<-sess.done
	d.clear(ctx, s.key)
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

	for _, s := range live {
		s.cancel()
		<-s.done
		d.clear(ctx, s.key)
	}
}

// heartbeat re-asserts a status inside the backend's expiry window.
func (d *StatusDriver) heartbeat(ctx context.Context, s *session) {
	defer close(s.done)
	interval := d.poster.StatusRefresh()
	if interval <= 0 {
		// A poster that declares no interval gets no heartbeat rather
		// than a spin: its indicator lapses, which is a cosmetic loss,
		// where a zero-interval ticker is a hot loop against a vendor's
		// rate limiter.
		log.WarnContext(ctx, "status_poster_declares_no_refresh", "backend", d.poster.StatusBackend())
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// No liveness re-check, and the ORDERING is why it is
			// safe: End and Stop both cancel this context and wait
			// for this goroutine to exit BEFORE they clear the
			// indicator. So a re-assertion can never follow a clear
			// — the worst case is one extra post that the clear
			// immediately takes down. A check here would read as
			// though that ordering were in doubt.
			d.mu.Lock()
			text := d.phrases.Pick(s.phase, s.seed, s.rotation)
			d.mu.Unlock()
			d.post(ctx, s.key, text)
		}
	}
}

func (d *StatusDriver) post(ctx context.Context, key statusKey, text string) {
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

func (d *StatusDriver) clear(ctx context.Context, key statusKey) {
	if !d.poster.ClearStatus(ctx, key.handle, key.channel, key.thread) {
		log.DebugContext(ctx, "working_status_not_cleared", "backend", d.poster.StatusBackend(),
			"handle", key.handle, "channel", key.channel)
	}
}

// String renders a conversation for a log line.
func (c Conversation) String() string {
	return fmt.Sprintf("%s:%s", c.Channel, c.Thread)
}
