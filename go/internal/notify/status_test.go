package notify_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/notify"
)

// poster records what a backend was asked to show. Two of them exist in
// spirit — one that renders text and one that cannot — and the difference is
// the only thing this module models per backend, so it is a field.
type poster struct {
	backend  string
	text     bool
	refresh  time.Duration
	dmPrefix string
	fail     bool

	mu      sync.Mutex
	set     []string
	cleared int
}

func newPoster() *poster {
	return &poster{backend: "chat", text: true, refresh: 20 * time.Millisecond}
}

func (p *poster) StatusBackend() string        { return p.backend }
func (p *poster) SupportsStatusText() bool     { return p.text }
func (p *poster) StatusRefresh() time.Duration { return p.refresh }
func (p *poster) DMChannelPrefix() string      { return p.dmPrefix }

func (p *poster) SetStatus(_ context.Context, _, _, _, status string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.set = append(p.set, status)
	return !p.fail
}

func (p *poster) ClearStatus(_ context.Context, _, _, _ string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleared++
	return !p.fail
}

func (p *poster) shown() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.set...)
}

func (p *poster) clears() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cleared
}

func chatMeta(mutate func(map[string]string)) map[string]string {
	m := map[string]string{
		"transport": "chat", "channel": "C1", "ts": "1718.003",
		"channel_type": "im",
	}
	if mutate != nil {
		mutate(m)
	}
	return m
}

func driver(t *testing.T, p *poster, mode notify.StatusMode) *notify.StatusDriver {
	t.Helper()
	d := notify.NewStatusDriver(notify.StatusOptions{Poster: p, Mode: mode})
	t.Cleanup(func() { d.Stop(context.Background()) })
	return d
}

func TestAnIndicatorGoesUpAndComesDown(t *testing.T) {
	p := newPoster()
	d := driver(t, p, notify.StatusAddressed)

	s := d.Begin(t.Context(), "swe", "turn-1", "plan", chatMeta(nil))
	if s == nil {
		t.Fatal("no indicator was raised for a direct message")
	}
	shown := p.shown()
	if len(shown) != 1 || shown[0] == "" {
		t.Fatalf("the indicator showed %q", shown)
	}
	if got := s.Conversation(); got.Channel != "C1" || got.Thread != "1718.003" {
		t.Fatalf("the session sits in %+v", got)
	}
	if live := d.Live(); len(live) != 1 {
		t.Fatalf("Live = %v", live)
	}

	s.End(t.Context(), false)
	if p.clears() != 1 {
		t.Fatalf("the indicator was cleared %d times", p.clears())
	}
	if live := d.Live(); len(live) != 0 {
		t.Fatalf("a finished session is still live: %v", live)
	}
}

// A top-level message has no thread yet, so its OWN id is the anchor — the
// same value the chat prompt tells the agent to reply under, which is what
// makes the indicator appear where the reply will land.
func TestATopLevelMessageAnchorsOnItsOwnID(t *testing.T) {
	p := newPoster()
	d := driver(t, p, notify.StatusAlways)

	s := d.Begin(t.Context(), "swe", "turn-1", "plan",
		chatMeta(func(m map[string]string) { delete(m, "thread_ts") }))
	if s == nil || s.Conversation().Thread != "1718.003" {
		t.Fatalf("the anchor is %+v", s.Conversation())
	}

	// A reply in a thread anchors on the THREAD, not on its own id.
	s2 := d.Begin(t.Context(), "swe", "turn-2", "plan",
		chatMeta(func(m map[string]string) {
			m["thread_ts"], m["ts"] = "1718.001", "1718.009"
		}))
	if s2 == nil || s2.Conversation().Thread != "1718.001" {
		t.Fatalf("a thread reply anchored on %+v", s2.Conversation())
	}
}

// The discriminator is the transport key. A trigger from somewhere else must
// not raise this backend's indicator.
func TestATriggerFromAnotherBackendRaisesNothing(t *testing.T) {
	p := newPoster()
	d := driver(t, p, notify.StatusAlways)

	for _, meta := range []map[string]string{
		chatMeta(func(m map[string]string) { m["transport"] = "elsewhere" }),
		chatMeta(func(m map[string]string) { delete(m, "transport") }),
		chatMeta(func(m map[string]string) { delete(m, "channel") }),
		chatMeta(func(m map[string]string) { delete(m, "ts") }),
		nil,
	} {
		if s := d.Begin(t.Context(), "swe", "turn-1", "plan", meta); s != nil {
			t.Fatalf("an indicator was raised for %v", meta)
		}
	}
	if len(p.shown()) != 0 {
		t.Fatalf("the backend was asked to show %v", p.shown())
	}
}

// `addressed` covers exactly the cases where a person is plausibly waiting
// on THIS agent. A broadcast and a passive channel message wake every bot in
// the room, and lighting up N indicators is noise rather than signal.
func TestAddressedShowsOnlyWhereSomebodyIsWaiting(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
		want bool
	}{
		{"direct message", chatMeta(nil), true},
		{"group direct message", chatMeta(func(m map[string]string) {
			m["channel_type"] = "mpim"
		}), true},
		{"personal mention", chatMeta(func(m map[string]string) {
			m["channel_type"] = "channel"
			m["thread_follow_reason"] = string(notify.FollowMention)
		}), true},
		{"a thread it already follows", chatMeta(func(m map[string]string) {
			m["channel_type"] = "channel"
			m["thread_following"] = "yes"
		}), true},
		{"a broadcast", chatMeta(func(m map[string]string) {
			m["channel_type"] = "channel"
			m["thread_follow_reason"] = string(notify.FollowCollective)
		}), false},
		{"a passive channel message", chatMeta(func(m map[string]string) {
			m["channel_type"] = "channel"
		}), false},
	}
	for _, c := range cases {
		p := newPoster()
		d := driver(t, p, notify.StatusAddressed)
		got := d.Begin(t.Context(), "swe", "turn-1", "plan", c.meta) != nil
		if got != c.want {
			t.Errorf("%s: raised = %v, want %v", c.name, got, c.want)
		}
	}
}

// `always` is what a single-agent workspace wants; `off` disables it, with
// no branch at the call site.
func TestTheOtherModes(t *testing.T) {
	passive := chatMeta(func(m map[string]string) { m["channel_type"] = "channel" })

	p := newPoster()
	if d := driver(t, p, notify.StatusAlways); d.Begin(t.Context(), "swe", "t", "plan", passive) == nil {
		t.Fatal("always did not raise on a passive message")
	}
	p = newPoster()
	d := driver(t, p, notify.StatusOff)
	if s := d.Begin(t.Context(), "swe", "t", "plan", chatMeta(nil)); s != nil {
		t.Fatal("off raised an indicator")
	}
	// A nil session's methods are no-ops, so a caller never branches.
	var none *notify.StatusSession
	none.Phase(t.Context(), "execute")
	none.End(t.Context(), false)
	if got := none.Conversation(); got != (notify.Conversation{}) {
		t.Fatalf("a nil session has a conversation: %+v", got)
	}
	// A driver with no poster at all is off, not a nil dereference.
	bare := notify.NewStatusDriver(notify.StatusOptions{Mode: notify.StatusAlways})
	if bare.Mode() != notify.StatusOff || bare.Backend() != "" {
		t.Fatalf("a poster-less driver is %q on %q", bare.Mode(), bare.Backend())
	}
	if s := bare.Begin(t.Context(), "swe", "t", "plan", chatMeta(nil)); s != nil {
		t.Fatal("a poster-less driver raised an indicator")
	}
	bare.Stop(t.Context())
}

// Two turns in one thread — a suspend/resume pair, or a queued follow-up —
// share a heartbeat, and the indicator clears only when the LAST finishes.
// Clearing on the first takes it down while somebody is still waiting.
func TestTheIndicatorSurvivesUntilTheLastTurnEnds(t *testing.T) {
	p := newPoster()
	d := driver(t, p, notify.StatusAddressed)

	first := d.Begin(t.Context(), "swe", "turn-1", "plan", chatMeta(nil))
	second := d.Begin(t.Context(), "swe", "turn-2", "plan", chatMeta(nil))
	if first == nil || second == nil {
		t.Fatal("a second turn did not join the session")
	}
	// JOINING does not re-raise: the indicator is already up and its text
	// is being watched.
	if len(p.shown()) != 1 {
		t.Fatalf("joining re-raised the indicator: %v", p.shown())
	}

	first.End(t.Context(), false)
	if p.clears() != 0 {
		t.Fatal("the indicator came down while a turn was still running")
	}
	second.End(t.Context(), false)
	if p.clears() != 1 {
		t.Fatalf("the indicator was cleared %d times", p.clears())
	}
}

// A turn that SUSPENDS for a detached coding run holds its session open: the
// same turn resumes when the job completes, and the person is still waiting.
func TestASuspendedTurnKeepsItsIndicator(t *testing.T) {
	p := newPoster()
	d := driver(t, p, notify.StatusAddressed)

	s := d.Begin(t.Context(), "swe", "turn-1", "execute", chatMeta(nil))
	s.End(t.Context(), true)
	if p.clears() != 0 {
		t.Fatal("a suspended turn took its indicator down")
	}
	if len(d.Live()) != 1 {
		t.Fatal("the suspended session is not live")
	}
	// And the resumed turn ends it for real.
	s.End(t.Context(), false)
	if p.clears() != 1 {
		t.Fatalf("the resumed turn cleared %d times", p.clears())
	}
}

func TestAPhaseChangeChangesTheWords(t *testing.T) {
	p := newPoster()
	d := driver(t, p, notify.StatusAddressed)

	s := d.Begin(t.Context(), "swe", "turn-1", "plan", chatMeta(nil))
	s.Phase(t.Context(), "execute")
	s.Phase(t.Context(), "review")
	shown := p.shown()
	if len(shown) != 3 {
		t.Fatalf("the backend was asked to show %v", shown)
	}
	if shown[0] == shown[1] || shown[1] == shown[2] {
		t.Fatalf("the words did not move between phases: %v", shown)
	}
	// Re-asserting the SAME phase costs nothing: the heartbeat already
	// keeps it alive, and a re-post would reset the text a reader is on.
	s.Phase(t.Context(), "review")
	if len(p.shown()) != 3 {
		t.Fatalf("a repeated phase cost a request: %v", p.shown())
	}

	// A REVISITED phase must not repeat its own earlier line — Execute,
	// Review, then Execute again after a self-iterate is an ordinary turn,
	// and repeating reads as though nothing moved. That is what the
	// rotation is for, and it is invisible in a plan/execute/review walk
	// because those draw from different pools anyway.
	s.Phase(t.Context(), "execute")
	shown = p.shown()
	if shown[len(shown)-1] == shown[1] {
		t.Fatalf("a revisited phase repeated its earlier line: %v", shown)
	}
}

// On a backend that cannot render text the phrase machinery goes INERT: the
// session still runs and the indicator stays alive, but a phase change stops
// costing a request, because there is nothing about it a reader could see.
func TestPhrasesAreInertWhereTextIsNotRendered(t *testing.T) {
	p := newPoster()
	p.text = false
	d := driver(t, p, notify.StatusAddressed)

	s := d.Begin(t.Context(), "swe", "turn-1", "plan", chatMeta(nil))
	if shown := p.shown(); len(shown) != 1 || shown[0] != "" {
		t.Fatalf("a text-less backend was sent %q", shown)
	}
	s.Phase(t.Context(), "execute")
	s.Phase(t.Context(), "review")
	if len(p.shown()) != 1 {
		t.Fatalf("phase changes cost %d requests on a text-less backend", len(p.shown()))
	}
	// It still comes down explicitly: "the agent gave up" has no
	// backend-side signal at all.
	s.End(t.Context(), false)
	if p.clears() != 1 {
		t.Fatal("a text-less indicator was left to lapse")
	}
}

// A raised status expires, so the session re-asserts it well inside that
// window. Without the heartbeat the indicator vanishes mid-turn and the
// reader concludes the bot died.
func TestTheHeartbeatKeepsTheIndicatorAlive(t *testing.T) {
	p := newPoster()
	p.refresh = 5 * time.Millisecond
	d := driver(t, p, notify.StatusAddressed)

	s := d.Begin(t.Context(), "swe", "turn-1", "plan", chatMeta(nil))
	deadline := time.Now().Add(2 * time.Second)
	for len(p.shown()) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	shown := p.shown()
	if len(shown) < 3 {
		t.Fatalf("the heartbeat re-asserted %d times in two seconds", len(shown))
	}
	// The TEXT does not drift under the reader between re-assertions.
	for _, got := range shown {
		if got != shown[0] {
			t.Fatalf("the heartbeat changed the words: %v", shown)
		}
	}

	// End waits for the heartbeat to exit BEFORE it clears, so the clear
	// is the last thing the backend hears. A re-assertion landing after it
	// would leave an indicator up with no session left to take it down.
	s.End(t.Context(), false)
	settled := len(p.shown())
	time.Sleep(40 * time.Millisecond)
	if len(p.shown()) != settled {
		t.Fatal("the heartbeat outlived its session")
	}
	if p.clears() != 1 {
		t.Fatalf("the indicator was cleared %d times", p.clears())
	}
}

// A poster declaring no interval gets NO heartbeat rather than a spin: its
// indicator lapses, which is cosmetic, where a zero-interval ticker is a hot
// loop against a vendor's rate limiter.
// gatedPoster blocks inside SetStatus so a heartbeat post is provably in
// flight when a test calls End, and records the ORDER of what the backend
// was asked to do.
type gatedPoster struct {
	poster
	entered chan struct{}
	release chan struct{}
	armed   bool

	omu sync.Mutex
	ops []string
}

func (g *gatedPoster) SetStatus(ctx context.Context, h, c, th, status string) bool {
	g.omu.Lock()
	armed := g.armed
	g.omu.Unlock()
	if armed {
		g.omu.Lock()
		g.armed = false
		g.omu.Unlock()
		close(g.entered)
		<-g.release
	}
	g.record("set")
	return g.poster.SetStatus(ctx, h, c, th, status)
}

func (g *gatedPoster) ClearStatus(ctx context.Context, h, c, th string) bool {
	g.record("clear")
	return g.poster.ClearStatus(ctx, h, c, th)
}

func (g *gatedPoster) record(op string) {
	g.omu.Lock()
	defer g.omu.Unlock()
	g.ops = append(g.ops, op)
}

func (g *gatedPoster) trace() []string {
	g.omu.Lock()
	defer g.omu.Unlock()
	return append([]string(nil), g.ops...)
}

// THE ORDERING INVARIANT the heartbeat rests on: End cancels and WAITS for
// the heartbeat to exit before it clears, so the clear is the last thing the
// backend hears. Clearing first leaves the in-flight re-assertion to land
// after it — an indicator up for ever with no session left to take it down.
func TestTheClearIsTheLastThingTheBackendHears(t *testing.T) {
	g := &gatedPoster{
		poster:  poster{backend: "chat", text: true, refresh: 5 * time.Millisecond},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	d := notify.NewStatusDriver(notify.StatusOptions{Poster: g, Mode: notify.StatusAlways})
	defer d.Stop(context.Background())

	s := d.Begin(t.Context(), "swe", "turn-1", "plan", chatMeta(nil))
	if s == nil {
		t.Fatal("no session")
	}
	// Arm AFTER the opening post, so it is a HEARTBEAT that blocks.
	g.omu.Lock()
	g.armed = true
	g.omu.Unlock()

	select {
	case <-g.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no heartbeat arrived to block")
	}

	ended := make(chan struct{})
	go func() { s.End(t.Context(), false); close(ended) }()

	// End must be BLOCKED on the in-flight post rather than racing past
	// it to clear.
	select {
	case <-ended:
		t.Fatal("End returned while a re-assertion was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(g.release)
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("End never returned")
	}

	ops := g.trace()
	if len(ops) == 0 || ops[len(ops)-1] != "clear" {
		t.Fatalf("the backend last heard %v, want the clear", ops)
	}
}

// Stop takes the same care, for the same reason: a node shutting down must
// not leave an indicator up because a re-assertion landed after its clear.
func TestStopAlsoClearsLast(t *testing.T) {
	g := &gatedPoster{
		poster:  poster{backend: "chat", text: true, refresh: 5 * time.Millisecond},
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	d := notify.NewStatusDriver(notify.StatusOptions{Poster: g, Mode: notify.StatusAlways})

	if s := d.Begin(t.Context(), "swe", "turn-1", "plan", chatMeta(nil)); s == nil {
		t.Fatal("no session")
	}
	g.omu.Lock()
	g.armed = true
	g.omu.Unlock()

	select {
	case <-g.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("no heartbeat arrived to block")
	}

	stopped := make(chan struct{})
	go func() { d.Stop(context.Background()); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a re-assertion was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(g.release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned")
	}

	ops := g.trace()
	if len(ops) == 0 || ops[len(ops)-1] != "clear" {
		t.Fatalf("the backend last heard %v, want the clear", ops)
	}
}

func TestAPosterWithNoRefreshDoesNotSpin(t *testing.T) {
	p := newPoster()
	p.refresh = 0
	d := driver(t, p, notify.StatusAddressed)

	s := d.Begin(t.Context(), "swe", "turn-1", "plan", chatMeta(nil))
	time.Sleep(30 * time.Millisecond)
	if got := len(p.shown()); got != 1 {
		t.Fatalf("a refresh-less poster was called %d times", got)
	}
	s.End(t.Context(), false)
}

// A failed call is a cosmetic loss, not a turn's problem: the status simply
// expires on the backend's side.
func TestAFailedPostDoesNotBreakTheSession(t *testing.T) {
	p := newPoster()
	p.fail = true
	d := driver(t, p, notify.StatusAddressed)

	s := d.Begin(t.Context(), "swe", "turn-1", "plan", chatMeta(nil))
	if s == nil {
		t.Fatal("a failing backend refused the session")
	}
	s.Phase(t.Context(), "execute")
	s.End(t.Context(), false)
	if p.clears() != 1 {
		t.Fatal("the clear was not attempted")
	}
}

// A node shutting down CLEARS its indicators rather than letting them lapse:
// otherwise everyone watching is told an agent is working on something this
// process is about to stop doing.
func TestStopClearsEveryLiveIndicator(t *testing.T) {
	p := newPoster()
	d := notify.NewStatusDriver(notify.StatusOptions{Poster: p, Mode: notify.StatusAlways})

	for _, ch := range []string{"C1", "C2", "C3"} {
		d.Begin(t.Context(), "swe", "turn-"+ch, "plan",
			chatMeta(func(m map[string]string) { m["channel"] = ch }))
	}
	if len(d.Live()) != 3 {
		t.Fatalf("Live = %v", d.Live())
	}

	d.Stop(t.Context())
	if p.clears() != 3 {
		t.Fatalf("Stop cleared %d of 3 indicators", p.clears())
	}
	if len(d.Live()) != 0 {
		t.Fatalf("indicators survived Stop: %v", d.Live())
	}
	// A stopped driver raises nothing more, and stopping twice is safe.
	if s := d.Begin(t.Context(), "swe", "late", "plan", chatMeta(nil)); s != nil {
		t.Fatal("a stopped driver raised an indicator")
	}
	d.Stop(t.Context())
}

func TestConcurrentSessionsAreSafe(t *testing.T) {
	p := newPoster()
	p.refresh = time.Millisecond
	d := driver(t, p, notify.StatusAlways)

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := "C" + string(rune('a'+i%4))
			s := d.Begin(t.Context(), "swe", "turn-"+string(rune('a'+i)), "plan",
				chatMeta(func(m map[string]string) { m["channel"] = ch }))
			s.Phase(t.Context(), "execute")
			_ = d.Live()
			s.End(t.Context(), false)
		}()
	}
	wg.Wait()
	if live := d.Live(); len(live) != 0 {
		t.Fatalf("sessions leaked: %v", live)
	}
}

// ---------------------------------------------------------------- //
// Phrase pools
// ---------------------------------------------------------------- //

// The heartbeat re-asserts the same status every interval, so a line that
// moved between re-assertions would flicker under a reader watching it.
func TestAPhraseIsStableForAGivenTurnAndPhase(t *testing.T) {
	p := notify.NewPhrases(nil)
	first := p.Pick("plan", "turn-1", 0)
	for range 20 {
		if got := p.Pick("plan", "turn-1", 0); got != first {
			t.Fatalf("the line moved: %q then %q", first, got)
		}
	}
	// Different turns in one thread start from different points.
	var distinct bool
	for _, turn := range []string{"turn-2", "turn-3", "turn-4", "turn-5"} {
		if p.Pick("plan", turn, 0) != first {
			distinct = true
		}
	}
	if !distinct {
		t.Fatal("every turn drew the same line")
	}
	// And a rotation moves it, so a revisited phase does not read as
	// though nothing happened.
	if p.Pick("plan", "turn-1", 1) == first {
		t.Fatal("a rotation did not move the line")
	}
}

func TestAnUnknownPhaseFallsBackToTheDefaultPool(t *testing.T) {
	p := notify.NewPhrases(nil)
	got := p.Pick("some-new-phase", "turn-1", 0)
	if !slicesContains(notify.PhasePhrases[notify.DefaultPhase], got) {
		t.Fatalf("an unknown phase drew %q, which is in no default pool", got)
	}
}

// "Override nothing" and "override with nothing" are the same request: an
// empty status is a CLEARED indicator, which is the opposite of what an
// operator writing a phrase list is asking for.
func TestAnEmptyOverrideKeepsTheBuiltInPool(t *testing.T) {
	builtin := notify.NewPhrases(nil).Pick("plan", "turn-1", 0)
	for _, override := range []map[string][]string{
		nil, {}, {"plan": nil}, {"plan": {}}, {"plan": {"", "   "}},
	} {
		if got := notify.NewPhrases(override).Pick("plan", "turn-1", 0); got != builtin {
			t.Fatalf("override %v changed the line to %q", override, got)
		}
	}
}

func TestAnOverrideReplacesOnlyItsOwnPhase(t *testing.T) {
	p := notify.NewPhrases(map[string][]string{"plan": {" is pondering... "}})

	if got := p.Pick("plan", "turn-1", 0); got != "is pondering..." {
		t.Fatalf("the override rendered %q, untrimmed", got)
	}
	builtin := notify.NewPhrases(nil).Pick("execute", "turn-1", 0)
	if got := p.Pick("execute", "turn-1", 0); got != builtin {
		t.Fatalf("overriding plan changed execute to %q", got)
	}
	// Pools hands back a copy, so an operator surface reading it cannot
	// reach into the driver's own state.
	pools := p.Pools()
	pools["plan"] = []string{"injected"}
	if got := p.Pick("plan", "turn-1", 0); got == "injected" {
		t.Fatal("mutating the returned pools reached the driver")
	}
}

func TestEveryShippedPhraseSaysOnlyThatTheAgentIsBusy(t *testing.T) {
	for phase, pool := range notify.PhasePhrases {
		if len(pool) == 0 {
			t.Errorf("%s ships an empty pool, which renders a cleared indicator", phase)
		}
		for _, phrase := range pool {
			// Rendered suffixed to the agent's name — "Agent SWE is
			// crewleting…" — so the leading "is" is load-bearing.
			if !strings.HasPrefix(phrase, "is ") {
				t.Errorf("%s: %q does not read after an agent's name", phase, phrase)
			}
			if strings.TrimSpace(phrase) != phrase {
				t.Errorf("%s: %q is padded", phase, phrase)
			}
		}
	}
}

func slicesContains(pool []string, want string) bool {
	for _, p := range pool {
		if p == want {
			return true
		}
	}
	return false
}
