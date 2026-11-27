package screen

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("browser.screen")

// WatchLapse is how long a viewer node's watch lease lives past its last
// renewal: thirty seconds, the live feed's idle (internal/sandbox's liveIdle),
// three of the renewals a viewer's node sends, so a renewal or two lost stop
// no screen and a viewer gone stops it within half a minute.
const WatchLapse = 30 * time.Second

// MaxWatchNodes bounds the viewer nodes a hub keeps a lease for: far past the
// nodes a fleet runs, so only a node renewing for nobody meets it.
const MaxWatchNodes = 64

// replacedBoots is how many boots a hub remembers having replaced: more
// supervisor restarts than one job sees, each one ensure's.
const replacedBoots = 8

// ErrTooManyWatchers is a watch lease past [MaxWatchNodes].
var ErrTooManyWatchers = errors.New("screen: the job's screen is watched from as many nodes as it takes")

// ErrNoLaunch is a hub asked for no job.
var ErrNoLaunch = errors.New("screen: a hub serves one job, and none was named")

// ErrNoLimits is a hub asked for without its node's limits ([NewLimits]).
var ErrNoLimits = errors.New("screen: a hub takes posts only under its node's limits, and none were handed")

// HubOptions are what a hub serves.
type HubOptions struct {
	// Launch is the job the hub serves, the launch its check-ins must name.
	// Required.
	Launch string
	// Turn and Seat name the job's run and seat for the operator's log.
	Turn, Seat string
	// Serving is a hub that serves from the first: a job opened again from a
	// record whose browser was ready. False waits for [Hub.Serve].
	Serving bool
	// Limits are the node's ([NewLimits]). Required.
	Limits Limits
}

// Hub is one job's browser screen on the node holding its seat, for one
// bridge opening (the package doc). Safe for concurrent use.
type Hub struct {
	id                 string
	launch, turn, seat string
	limits             Limits
	heard              sync.Once

	mu      sync.Mutex
	closed  bool
	serving bool
	// boot is the boot the hub takes posts under, "" before the first, and
	// replaced the ones it replaced, oldest first.
	boot     string
	replaced []string
	// seq and rev are the newest frame's and state's numbers under boot.
	seq, rev int64
	frame    *frame
	state    *browser.State
	checkin  *checkin
	// level rises with every change of whether anybody watches, and held is
	// the held poll's channel, closed when it moves.
	level  int
	held   chan struct{}
	leases map[string]time.Time
	lapse  *time.Timer
}

// frame is a frame a hub keeps: its metadata and its JPEG.
type frame struct {
	meta browser.FrameMeta
	jpeg []byte
}

// checkin is the newest check-in a hub keeps: whether the job's model
// connection holds the box's browser, whether a supervisor answers the relay,
// and since when by the HUB'S clock, never the box's, which the engine never
// compares with its own. sinceMS is the box's own word on since when, read
// only to tell a change the hub did not see between two check-ins.
type checkin struct {
	connected  bool
	supervisor browser.SupervisorPresence
	since      time.Time
	sinceMS    int64
}

// NewHub builds a job's hub, waiting unless opts.Serving, under an id of its
// own: 32 lower-case hex digits from crypto/rand, which its poll's answer
// names.
func NewHub(opts HubOptions) (*Hub, error) {
	switch {
	case opts.Launch == "":
		return nil, ErrNoLaunch
	case !opts.Limits.complete():
		return nil, ErrNoLimits
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	return &Hub{id: hex.EncodeToString(raw[:]), launch: opts.Launch, turn: opts.Turn, seat: opts.Seat,
		limits: opts.Limits, serving: opts.Serving, leases: map[string]time.Time{}}, nil
}

// ID is the hub's id, which every answer to its relay's poll names.
func (h *Hub) ID() string { return h.id }

// Serving reports whether the hub serves its job's browser: false while its
// launch's ensure has not answered ready at or above the floor.
func (h *Hub) Serving() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.serving
}

// Serve makes a waiting hub serve: its job's ensure answered ready at or
// above the floor, before the job starts, so the relay finds it serving.
func (h *Hub) Serve() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.serving = true
}

// Frame takes a frame of its relay's: meta, its [browser.FrameHeader], and its
// JPEG, which read reads within its bound once the post is charged and the
// hub keeps rather than copies. Charged first ([Hub.admit]), then read whole
// (browser.ParseFrameMeta and browser.CheckJPEG), then held to the boot rule
// (the package doc's "The newest only"), and kept only while a watch lease
// lives. nil for a frame taken or dropped as stale alike.
func (h *Hub) Frame(ctx context.Context, meta string, read func() ([]byte, error)) error {
	jpeg, err := h.admit(ctx, "frame", h.limits.Frames, read)
	if err != nil {
		return err
	}
	m, err := browser.ParseFrameMeta(meta, len(jpeg))
	if err == nil {
		err = browser.CheckJPEG(jpeg, m)
	}
	if err != nil {
		return h.refused(ctx, "frame", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return browser.ErrScreenClosed
	}
	if !h.takesLocked(m.Boot) || (m.Boot == h.boot && m.Seq <= h.seq) {
		return nil
	}
	h.seq = m.Seq
	if len(h.leases) > 0 {
		h.frame = &frame{meta: m, jpeg: jpeg}
	}
	return nil
}

// State takes a state of its relay's, charged first ([Hub.admit]), then
// read whole (browser.ParseState) and held to the boot rule by its rev. nil
// for a state taken or dropped as stale alike.
func (h *Hub) State(ctx context.Context, read func() ([]byte, error)) error {
	body, err := h.admit(ctx, "state", h.limits.States, read)
	if err != nil {
		return err
	}
	s, err := browser.ParseState(body)
	if err != nil {
		return h.refused(ctx, "state", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return browser.ErrScreenClosed
	}
	if !h.takesLocked(s.Boot) || (s.Boot == h.boot && s.Rev <= h.rev) {
		return nil
	}
	h.rev, h.state = s.Rev, &s
	return nil
}

// Checkin takes a check-in of its relay's, charged first ([Hub.admit]),
// then read whole for the hub's own job (browser.ParseCheckin). The newest is
// kept, dated by the hub's clock when it says another connection than the
// last did.
func (h *Hub) Checkin(ctx context.Context, read func() ([]byte, error)) error {
	body, err := h.admit(ctx, "checkin", h.limits.Checkins, read)
	if err != nil {
		return err
	}
	c, err := browser.ParseCheckin(body, h.launch)
	if err != nil {
		return h.refused(ctx, "checkin", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return browser.ErrScreenClosed
	}
	since := time.Now().UTC()
	if last := h.checkin; last != nil && last.connected == c.Connected && last.sinceMS == c.SinceMS {
		since = last.since
	}
	h.checkin = &checkin{connected: c.Connected, supervisor: c.Supervisor, since: since, sinceMS: c.SinceMS}
	return nil
}

// admit is the first steps of a post of verb, in their order: the hub has
// heard its relay, it is open, the post is paid for on limiter, and only then
// is read called for its body. So a post past its rate is refused before a
// byte of it is read, and one whose body is refused, or past its bound, has
// paid as a taken one does. An error read answers is answered as it is.
func (h *Hub) admit(ctx context.Context, verb string, limiter *costlimit.Limiter,
	read func() ([]byte, error)) ([]byte, error) {
	h.hear(ctx)
	if err := h.open(); err != nil {
		return nil, err
	}
	if err := charge(limiter, h.launch); err != nil {
		return nil, h.refused(ctx, verb, err)
	}
	return read()
}

// takesLocked reports whether a post of boot may be taken at all, replacing
// the current boot with it where it is one the hub has not seen; under mu.
// A post of the current boot is then held to its number by the caller.
func (h *Hub) takesLocked(boot string) bool {
	switch {
	case boot == h.boot:
		return true
	case h.wasReplacedLocked(boot):
		return false
	}
	if h.boot != "" {
		h.replaced = append(h.replaced, h.boot)
		if over := len(h.replaced) - replacedBoots; over > 0 {
			h.replaced = h.replaced[over:]
		}
	}
	h.boot, h.seq, h.rev, h.frame, h.state = boot, 0, 0, nil, nil
	return true
}

// wasReplacedLocked reports whether boot is one the hub replaced and still
// remembers; under mu. Boots are compared for equality alone: they have no
// order.
func (h *Hub) wasReplacedLocked(boot string) bool {
	for _, gone := range h.replaced {
		if gone == boot {
			return true
		}
	}
	return false
}

// Poll answers its relay's poll q: the answer as it stands, and nil where it
// is answered now, q naming another hub or a level below the hub's. Else the
// poll is held, and the channel closes when it moves: the level moved, a
// newer poll took its place, or the hub closed; [Hub.Answer] is then its
// answer. Every poll answers the one held before it at once, so one poll a
// job is held. [browser.ErrScreenClosed] once the hub has closed.
func (h *Hub) Poll(ctx context.Context, q browser.PollQuery) (browser.PollAnswer, <-chan struct{}, error) {
	h.hear(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return browser.PollAnswer{}, nil, browser.ErrScreenClosed
	}
	if h.held != nil {
		close(h.held)
		h.held = nil
	}
	if q.Hub != h.id || q.Level < h.level {
		return h.answerLocked(), nil, nil
	}
	h.held = make(chan struct{})
	return h.answerLocked(), h.held, nil
}

// Answer is the hub's answer to a poll as it stands now, for one whose hold
// ended or moved; [browser.ErrScreenClosed] once the hub has closed.
func (h *Hub) Answer() (browser.PollAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return browser.PollAnswer{}, browser.ErrScreenClosed
	}
	return h.answerLocked(), nil
}

// answerLocked is a poll's answer as the hub stands, every field set; under
// mu. The rate is the one a watched box sends at whether or not anybody
// watches, since it says how fast, never whether.
func (h *Hub) answerLocked() browser.PollAnswer {
	return browser.PollAnswer{V: browser.WireVersion, Hub: h.id, Level: h.level,
		Watch: browser.Watch{On: len(h.leases) > 0, FPS: browser.WatchFPS}}
}

// moveLocked raises the level and answers the held poll; under mu.
func (h *Hub) moveLocked() {
	h.level++
	if h.held != nil {
		close(h.held)
		h.held = nil
	}
}

// Watch sets node's watch lease to [WatchLapse] from now, the first turning
// the box's screen on: a renewal of a live lease changes nothing a poll reads.
// [ErrTooManyWatchers] for a node past [MaxWatchNodes];
// [browser.ErrScreenClosed] once the hub has closed.
func (h *Hub) Watch(node string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return browser.ErrScreenClosed
	}
	if _, renewed := h.leases[node]; !renewed && len(h.leases) >= MaxWatchNodes {
		return ErrTooManyWatchers
	}
	if len(h.leases) == 0 {
		h.moveLocked()
	}
	h.leases[node] = time.Now().Add(WatchLapse)
	h.armLocked()
	return nil
}

// Unwatch drops node's watch lease, the last turning the box's screen off.
func (h *Hub) Unwatch(node string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.leases[node]; !ok || h.closed {
		return
	}
	delete(h.leases, node)
	h.unwatchedLocked()
	h.armLocked()
}

// unwatchedLocked turns the screen off where no lease is left: the level
// moves, and the frame nothing reads any more goes; under mu.
func (h *Hub) unwatchedLocked() {
	if len(h.leases) == 0 {
		h.moveLocked()
		h.frame = nil
	}
}

// armLocked sets the hub's one timer to the earliest lease's lapse, or stops
// it where none is left; under mu.
func (h *Hub) armLocked() {
	if h.lapse != nil {
		h.lapse.Stop()
		h.lapse = nil
	}
	var earliest time.Time
	for _, until := range h.leases {
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	if !earliest.IsZero() {
		h.lapse = time.AfterFunc(time.Until(earliest), h.lapsed)
	}
}

// lapsed drops every lease whose time has come, by the hub's clock.
func (h *Hub) lapsed() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	now := time.Now()
	gone := false
	for node, until := range h.leases {
		if !until.After(now) {
			delete(h.leases, node)
			gone = true
		}
	}
	if gone {
		h.unwatchedLocked()
	}
	h.armLocked()
}

// Close ends the hub with its opening: the held poll is answered, the timer
// stopped, the leases and everything kept dropped, and every later call
// answers [browser.ErrScreenClosed]. Safe to call more than once.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	if h.held != nil {
		close(h.held)
		h.held = nil
	}
	if h.lapse != nil {
		h.lapse.Stop()
		h.lapse = nil
	}
	h.leases, h.frame, h.state, h.checkin = nil, nil, nil, nil
}

// open is [browser.ErrScreenClosed] once the hub has closed, else nil.
func (h *Hub) open() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return browser.ErrScreenClosed
	}
	return nil
}

// hear says once, the first time the job's relay reaches its hub, that it
// did: the operator's sign that a box's relay reaches its seat's holder.
func (h *Hub) hear(ctx context.Context) {
	h.heard.Do(func() {
		log.InfoContext(ctx, "executor_browser_relay_heard", "turn_id", h.turn, "launch_id", h.launch,
			"seat", h.seat, "hub", h.id)
	})
}

// refused logs a post the hub refused, at debug since the box can cause one
// at will, naming the verb and why, never the box's words, and answers err.
func (h *Hub) refused(ctx context.Context, verb string, err error) error {
	attrs := []any{"turn_id", h.turn, "launch_id", h.launch, "seat", h.seat, "verb", verb}
	if invalid, ok := errors.AsType[*browser.Invalid](err); ok {
		attrs = append(attrs, "why", string(invalid.Why), "rule", invalid.Rule)
	} else if fast, ok := errors.AsType[*browser.TooFast](err); ok {
		attrs = append(attrs, "why", "too_fast", "retry_after_ms", fast.RetryAfter.Milliseconds())
	}
	log.DebugContext(ctx, "executor_browser_relay_refused", attrs...)
	return err
}
