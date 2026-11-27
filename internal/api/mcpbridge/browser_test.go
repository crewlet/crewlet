package mcpbridge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/screen"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// A job's browser screen, as its bridge serves it to the relay in its box: the
// run's own token and nothing else, the opening's end turning a call away to
// ask again, a poll held no longer than its hold and let go at a shutdown, and
// every refusal of the screen's answered in its own code. What the screen takes
// is internal/browser/screen's, whose hub these cases run where they can.

// browserJob is the launch every case's session serves.
const browserJob = "job-1"

// recordedHub is a real hub as its bridge serves it, saying when it holds a
// poll and counting its closes.
type recordedHub struct {
	*screen.Hub
	held   chan struct{}
	closes atomic.Int32
}

// newRecordedHub is a hub of the fixture's job on limits of its own, serving
// where serving.
func newRecordedHub(t *testing.T, serving bool) *recordedHub {
	t.Helper()
	hub, err := screen.NewHub(screen.HubOptions{Launch: browserJob, Turn: "run-1", Seat: "dev", Serving: serving,
		Limits: screen.NewLimits(nil)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hub.Close)
	return &recordedHub{Hub: hub, held: make(chan struct{}, 16)}
}

func (h *recordedHub) Poll(ctx context.Context, q browser.PollQuery) (browser.PollAnswer, <-chan struct{}, error) {
	answer, moved, err := h.Hub.Poll(ctx, q)
	if moved != nil {
		h.held <- struct{}{}
	}
	return answer, moved, err
}

func (h *recordedHub) Close() {
	h.closes.Add(1)
	h.Hub.Close()
}

// fakeScreen is a serving screen that answers every post and poll with what a
// case sets, and takes no post it is not handed. As the hub does, it answers
// a post it refuses as too fast, or once closed, before reading its body, and
// reads every other's, answering the read's own error where it fails.
type fakeScreen struct {
	mu      sync.Mutex
	answer  error
	handed  int
	answers browser.PollAnswer
}

func (f *fakeScreen) Serving() bool { return true }

func (f *fakeScreen) take() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handed++
	return f.answer
}

// post is a post's answer, its body read as the hub reads one.
func (f *fakeScreen) post(read func() ([]byte, error)) error {
	answer := f.take()
	if _, fast := errors.AsType[*browser.TooFast](answer); fast || errors.Is(answer, browser.ErrScreenClosed) {
		return answer
	}
	if _, err := read(); err != nil {
		return err
	}
	return answer
}

func (f *fakeScreen) Frame(_ context.Context, _ string, read func() ([]byte, error)) error {
	return f.post(read)
}

func (f *fakeScreen) State(_ context.Context, read func() ([]byte, error)) error {
	return f.post(read)
}

func (f *fakeScreen) Checkin(_ context.Context, read func() ([]byte, error)) error {
	return f.post(read)
}

func (f *fakeScreen) Poll(context.Context, browser.PollQuery) (browser.PollAnswer, <-chan struct{}, error) {
	return f.answers, nil, f.take()
}

func (f *fakeScreen) Answer() (browser.PollAnswer, error) { return f.answers, f.take() }
func (f *fakeScreen) Close()                              {}

// browserFixture is a bridge serving one run whose session holds a browser
// screen, on every route of the bridge's.
type browserFixture struct {
	*fixture
	url string
}

// newBrowserFixture opens the fixture's session with screen, after edit.
func newBrowserFixture(t *testing.T, screen mcpbridge.Browser, edit func(*mcpbridge.Session)) *browserFixture {
	t.Helper()
	f := newFixture(t)
	f.session.Launch, f.session.Browser = browserJob, screen
	if edit != nil {
		edit(f.session)
	}
	f.bridge = bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"), BaseURL: f.server.URL})
	mux := http.NewServeMux()
	mountAll(mux, f.bridge)
	f.server.Config.Handler = mux
	url := f.bridge.Open(t.Context(), f.session)
	if url == "" {
		t.Fatal("no endpoint was minted")
	}
	return &browserFixture{fixture: f, url: url}
}

// mountAll mounts every route of the bridge's on mux, as internal/api does.
func mountAll(mux *http.ServeMux, b *mcpbridge.Bridge) {
	mux.Handle(mcpbridge.PathPrefix+"{token}", b.Handler())
	mux.Handle(mcpbridge.ServerPattern, b.Handler())
	mux.Handle(mcpbridge.SteerPattern, b.SteerHandler())
	mux.Handle(mcpbridge.SkillPattern, b.SkillHandler())
	mux.Handle(mcpbridge.BrowserPattern, b.BrowserHandler())
}

// frameShot is an 8 by 8 JPEG, the size frameHead names.
var frameShot = sync.OnceValue(func() []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		panic(err)
	}
	return buf.Bytes()
})

// frameHead is the metadata of frameShot numbered seq.
func frameHead(seq int64) string {
	raw, err := json.Marshal(browser.FrameMeta{V: 1, Boot: "00000000000000a1", Seq: seq, Tab: "t1",
		Capture: browser.CaptureScreencast, AtMS: 1, Width: 8, Height: 8, DeviceWidth: 8, DeviceHeight: 8,
		PageScale: 1, Bytes: len(frameShot())})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// stateBody is a state numbered rev, one tab in front.
func stateBody(rev int64) string {
	return `{"v":1,"boot":"00000000000000a1","rev":` + strconv.FormatInt(rev, 10) +
		`,"running":"running","tabs":[{"id":"t1","title":"a page","url":"https://example.com/","front":true}]}`
}

// checkinBody is the fixture job's check-in.
const checkinBody = `{"v":1,"job":"` + browserJob + `","connected":true,"since_ms":1,"supervisor":"answering"}`

// browserCall is one call of a browser route.
type browserCall struct {
	method, verb, query, contentType, meta string
	body                                   []byte
	host                                   string
}

// The four verbs, each as a relay makes it, the poll naming no hub yet.
func frameCall(seq int64) browserCall {
	return browserCall{method: http.MethodPost, verb: "frame", contentType: browser.FrameType, meta: frameHead(seq),
		body: frameShot()}
}
func stateCall(rev int64) browserCall {
	return browserCall{method: http.MethodPost, verb: "state", contentType: "application/json",
		body: []byte(stateBody(rev))}
}
func checkinCall() browserCall {
	return browserCall{method: http.MethodPost, verb: "checkin", contentType: "application/json",
		body: []byte(checkinBody)}
}
func pollCall(hub string, level int) browserCall {
	return browserCall{method: http.MethodGet, verb: "poll", query: "hub=" + hub + "&level=" + strconv.Itoa(level)}
}

// everyVerb is one call of each verb.
func everyVerb() map[string]browserCall {
	return map[string]browserCall{"a frame": frameCall(1), "a state": stateCall(1), "a check-in": checkinCall(),
		"a poll": pollCall("", 0)}
}

// browserAnswer is a browser route's answer: its status, its refusal's code and
// why, its Retry-After, and a poll's answer.
type browserAnswer struct {
	status     int
	code, why  string
	retryAfter string
	poll       browser.PollAnswer
	raw        string
}

// call makes c on the browser routes of endpoint through client.
func (c browserCall) to(ctx context.Context, t *testing.T, client *http.Client, endpoint string) browserAnswer {
	t.Helper()
	url := mcpbridge.BrowserURL(endpoint) + "/" + c.verb
	if c.query != "" {
		url += "?" + c.query
	}
	req, err := http.NewRequestWithContext(ctx, c.method, url, bytes.NewReader(c.body))
	if err != nil {
		t.Fatal(err)
	}
	if c.contentType != "" {
		req.Header.Set("Content-Type", c.contentType)
	}
	if c.meta != "" {
		req.Header.Set(browser.FrameHeader, c.meta)
	}
	if c.host != "" {
		req.Host = c.host
	}
	res, err := client.Do(req)
	if err != nil {
		return browserAnswer{}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	got := browserAnswer{status: res.StatusCode, retryAfter: res.Header.Get("Retry-After"), raw: string(raw)}
	var refusal struct {
		Error string `json:"error"`
		Why   string `json:"why"`
	}
	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &got.poll); err != nil {
			t.Errorf("decode a poll's answer %q: %v", raw, err)
		}
	} else if len(raw) > 0 && json.Unmarshal(raw, &refusal) == nil {
		got.code, got.why = refusal.Error, refusal.Why
	}
	return got
}

// wantRefused fails the case unless got is status with code.
func wantRefused(t *testing.T, what string, got browserAnswer, status int, code string) {
	t.Helper()
	if got.status != status || got.code != code {
		t.Errorf("%s was answered %d %q (%s), want %d %q", what, got.status, got.code, got.raw, status, code)
	}
}

// wantAskedAgain fails the case unless got turns the call away to ask again: a
// 503 with code and a Retry-After of whole seconds, at least one.
func wantAskedAgain(t *testing.T, what string, got browserAnswer, code string) {
	t.Helper()
	wantRefused(t, what, got, http.StatusServiceUnavailable, code)
	if after, err := strconv.Atoi(got.retryAfter); err != nil || after < 1 {
		t.Errorf("%s carries Retry-After %q, want whole seconds, at least one", what, got.retryAfter)
	}
}

// THE BROWSER ROUTES REACH A JOB'S SCREEN ONLY THROUGH ITS TOKEN. A forged
// token and a finished run's are the bridge's own 401; a session with no
// browser, and one whose browser waits for its ensure, answer every verb
// 404 `not_found`, which ends the relay; a verb this build does not serve is
// 404 `no_route`, which the relay asks again; a known verb on another method
// is 405; and a token that resolves is served whatever Host it names.
//
// Mutation: answer a verb this build does not serve `not_found`, or serve a
// waiting screen, and a case goes red.
func TestTheBrowserRoutesReachTheHubOnlyThroughTheirToken(t *testing.T) {
	t.Parallel()
	hub := newRecordedHub(t, false)
	f := newBrowserFixture(t, hub, nil)
	client := f.server.Client()
	ctx := t.Context()

	forged := f.server.URL + mcpbridge.PathPrefix + "v1.run-1.99999999999.x"
	wantRefused(t, "a forged token's state", stateCall(1).to(ctx, t, client, forged), http.StatusUnauthorized,
		"invalid_token")
	for name, c := range everyVerb() {
		wantRefused(t, name+" of a waiting screen", c.to(ctx, t, client, f.url), http.StatusNotFound, "not_found")
	}
	bare := &mcpbridge.Session{RunID: "run-2", Launch: "job-2", Handle: "dev", Role: "Engineer",
		Surface: f.surface, Ledger: f.ledger}
	none := f.bridge.Open(ctx, bare)
	for name, c := range everyVerb() {
		wantRefused(t, name+" of a session with no browser", c.to(ctx, t, client, none), http.StatusNotFound,
			"not_found")
	}

	hub.Serve()
	if got := stateCall(1).to(ctx, t, client, f.url); got.status != http.StatusNoContent {
		t.Errorf("a state of a serving screen was answered %d (%s), want 204", got.status, got.raw)
	}
	public := pollCall("", 0)
	public.host = "crewlet.example.com"
	if got := public.to(ctx, t, client, f.url); got.status != http.StatusOK || got.poll.Hub != hub.ID() {
		t.Errorf("a poll naming a public Host was answered %d %+v (%s), want the hub's answer", got.status,
			got.poll, got.raw)
	}
	outcome := browserCall{method: http.MethodPost, verb: "outcome", contentType: "application/json",
		body: []byte(`{"v":1}`)}
	wantRefused(t, "a verb this build does not serve", outcome.to(ctx, t, client, f.url), http.StatusNotFound,
		"no_route")
	for name, c := range map[string]browserCall{
		"a state fetched": {method: http.MethodGet, verb: "state"},
		"a poll posted":   {method: http.MethodPost, verb: "poll", query: "hub=&level=0"},
	} {
		wantRefused(t, name, c.to(ctx, t, client, f.url), http.StatusMethodNotAllowed, "method_not_allowed")
	}

	f.bridge.Close("run-1")
	wantRefused(t, "a finished run's state", stateCall(2).to(ctx, t, client, f.url), http.StatusUnauthorized,
		"invalid_token")
}

// A BROWSER CALL THAT REACHES ITS OPENING AS IT ENDS IS TURNED AWAY TO ASK
// AGAIN, every verb alike, and a poll held as its opening ends: a 503 with a
// Retry-After, whether the run ended, the seat left this node or a recovery
// opened the job again; never the 404 of a session with no browser, which ends
// the relay for the rest of its job. After a recovery the box's next call is
// served by the opening that serves it now, with a screen of its own.
//
// Mutations, each red: answer an opening whose end took its screen as a
// session with no browser (every case but the held poll); answer a closed
// screen as one (the held poll).
func TestABrowserCallReachingItsOpeningAsItEndsIsTurnedAwayToAskAgain(t *testing.T) {
	t.Parallel()
	ends := []struct {
		name     string
		end      func(t *testing.T, f *browserFixture) *recordedHub
		recovers bool
	}{
		{name: "the run ends", end: func(_ *testing.T, f *browserFixture) *recordedHub {
			f.bridge.Close("run-1")
			return nil
		}},
		{name: "the seat leaves this node", end: func(_ *testing.T, f *browserFixture) *recordedHub {
			f.bridge.ReleaseSeat(seat())
			return nil
		}},
		{name: "a recovery opens the job again", recovers: true,
			end: func(t *testing.T, f *browserFixture) *recordedHub {
				next := newRecordedHub(t, true)
				f.session.Browser = next
				if f.bridge.Open(t.Context(), f.session) == "" {
					t.Fatal("the recovery minted no endpoint")
				}
				return next
			}},
	}
	for _, end := range ends {
		for name, call := range everyVerb() {
			t.Run(end.name+", "+name, func(t *testing.T) {
				t.Parallel()
				parked := newParkedFence()
				f := newBrowserFixture(t, newRecordedHub(t, true), func(s *mcpbridge.Session) { s.Fence = parked.fence })
				t.Cleanup(parked.letGo)
				parked.arm()
				answered := make(chan browserAnswer, 1)
				go func() { answered <- call.to(t.Context(), t, f.server.Client(), f.url) }()
				// THE ROUTING FOUND THE CALL'S OPENING, and the call waits in the
				// session's fence, short of the route that serves it.
				<-parked.entered
				next := end.end(t, f)
				parked.letGo()
				wantAskedAgain(t, name+" reaching its opening as it ended", <-answered, "unavailable")
				if end.recovers {
					got := call.to(t.Context(), t, f.server.Client(), f.url)
					if got.status != http.StatusNoContent && got.status != http.StatusOK {
						t.Errorf("%s after the recovery was answered %d (%s), want it served", name, got.status,
							got.raw)
					}
					if got.status == http.StatusOK && got.poll.Hub != next.ID() {
						t.Errorf("the poll after the recovery names hub %q, want the new opening's", got.poll.Hub)
					}
				}
			})
		}
	}
	t.Run("a poll held as its opening ends", func(t *testing.T) {
		t.Parallel()
		hub := newRecordedHub(t, true)
		f := newBrowserFixture(t, hub, nil)
		answered := make(chan browserAnswer, 1)
		go func() { answered <- pollCall(hub.ID(), 0).to(t.Context(), t, f.server.Client(), f.url) }()
		<-hub.held
		f.bridge.Close("run-1")
		wantAskedAgain(t, "a poll held as its opening ended", <-answered, "unavailable")
	})
}

// A BROWSER ROUTE IS THE BOX'S WORD ON THE SESSION IT STARTED, as every route
// under the prefix is: a relay's first request on the job's other token tells
// the session the box started one of its own, and the token the launch chose
// is refused from then on, so a relay started on the wrong session's token
// locks its harness out rather than serving two sessions.
//
// Mutation: serve the browser routes without holding the opening to the
// session the box started, and the launch's token is served after it.
func TestABrowserRouteIsTheBoxsWordOnTheSessionItStarted(t *testing.T) {
	t.Parallel()
	heard := &told{}
	var calls *ledger
	f := newBrowserFixture(t, newRecordedHub(t, true), func(s *mcpbridge.Session) {
		calls = &ledger{}
		s.Ledger = calls
		s.Started = heard.hook(calls)
	})
	fresh := f.bridge.FreshEndpoint(f.session)
	if fresh == "" || fresh == f.url {
		t.Fatalf("the job's other token is %q beside %q", fresh, f.url)
	}
	if got := stateCall(1).to(t.Context(), t, f.server.Client(), fresh); got.status != http.StatusNoContent {
		t.Fatalf("the relay's first state on the other token was answered %d (%s), want 204", got.status, got.raw)
	}
	if said, _ := heard.words(); len(said) != 1 || !said[0] {
		t.Fatalf("the session was told %v, want once that the box started a session of its own", said)
	}
	if status := postPingTo(t, f.server.Client(), f.url); status != http.StatusUnauthorized {
		t.Errorf("the launch's token answered %d once the relay had said, want 401", status)
	}
	if got := checkinCall().to(t.Context(), t, f.server.Client(), f.url); got.status != http.StatusUnauthorized {
		t.Errorf("a check-in on the launch's token answered %d, want 401", got.status)
	}
}

// browserFleet is a fleet of two, the second holding the seat, every member
// serving every route, with a session whose screen is hub opened on the
// holder; its endpoint is the fleet's address, the first member's.
func browserFleet(t *testing.T, hub mcpbridge.Browser, steer mcpbridge.SteerInbox) (*fleetOf, string) {
	t.Helper()
	fx := newFixture(t)
	f := newFleet(t, 2, 10*time.Second)
	for _, m := range f.members {
		mux := m.server.Config.Handler.(*http.ServeMux)
		mux.Handle(mcpbridge.SteerPattern, m.bridge.SteerHandler())
		mux.Handle(mcpbridge.BrowserPattern, m.bridge.BrowserHandler())
	}
	session := &mcpbridge.Session{RunID: "run-1", Launch: browserJob, Handle: "dev", Role: "Engineer",
		Surface: fx.surface, Ledger: fx.ledger, Browser: hub, Steer: steer}
	return f, f.takeOver(t, f.members[1], session)
}

// A BROWSER POST IS RELAYED TO THE SEAT'S HOLDER, like every bridged call: a
// relay's state and poll land on the fleet's address, the node there reads the
// seat's lease, and the holder's screen takes and answers them.
//
// Mutation: leave the browser routes out of the mux the holder serves a
// relayed call on, and the relayed post is never served.
func TestABrowserPostIsRelayedToTheSeatsHolder(t *testing.T) {
	t.Parallel()
	hub := newRecordedHub(t, true)
	f, url := browserFleet(t, hub, nil)
	client := f.members[0].server.Client()
	if got := stateCall(3).to(t.Context(), t, client, url); got.status != http.StatusNoContent {
		t.Fatalf("a state through the fleet's address was answered %d (%s), want 204", got.status, got.raw)
	}
	if got := pollCall("", 0).to(t.Context(), t, client, url); got.status != http.StatusOK || got.poll.Hub != hub.ID() {
		t.Errorf("a poll through the fleet's address was answered %d %+v, want the holder's hub", got.status, got.poll)
	}
	// THE HOLDER'S SCREEN TOOK THE STATE: it drops the one it holds as stale.
	if got := stateCall(3).to(t.Context(), t, client, url); got.status != http.StatusNoContent {
		t.Errorf("the state again was answered %d, want 204 as a stale one", got.status)
	}
}

// A RELAYED BROWSER POLL IS LET GO WHEN THE NODE RELAYING IT STOPS, as a notes
// poll is: a poll held at the seat's holder is answered at the relaying
// node's shutdown with a 503 `draining` and a Retry-After, at once, never left
// to keep the stopping node's listener busy; and a poll after the shutdown is
// turned away the same way.
//
// Mutation: hand the browser routes no held poll, and the relayed poll waits
// out the holder's hold.
func TestShutdownLetsGoOfABrowserPollItRelays(t *testing.T) {
	t.Parallel()
	hub := newRecordedHub(t, true)
	f, url := browserFleet(t, hub, nil)
	entry, holder := f.members[0], f.members[1]
	mcpbridge.HoldPollsFor(holder.bridge, time.Minute)
	answered := make(chan browserAnswer, 1)
	go func() { answered <- pollCall(hub.ID(), 0).to(t.Context(), t, entry.server.Client(), url) }()
	<-hub.held
	entry.bridge.Shutdown()
	wantAskedAgain(t, "the relayed poll at its node's shutdown", <-answered, "draining")
	wantAskedAgain(t, "a poll after the shutdown",
		pollCall(hub.ID(), 0).to(t.Context(), t, entry.server.Client(), url), "draining")
}

// ONLY A POLL OF ITS OWN ROUTE IS HELD. A node that relays a held poll lets go
// of it at its shutdown, so a stopping node turns a held poll away at once;
// every other call it relays to the holder, who answers it. A GET of the
// browser routes' notes, or of the notes route's poll, is no held poll: each
// is relayed and answered by the holder as a verb its route does not serve.
//
// Mutation: hold a poll by its verb across routes, and the stopping node turns
// the browser routes' notes away as a held poll.
func TestOnlyAPollOfItsOwnRouteIsHeld(t *testing.T) {
	t.Parallel()
	hub := newRecordedHub(t, true)
	f, url := browserFleet(t, hub, newFakeInbox())
	entry := f.members[0]
	entry.bridge.Shutdown()
	client := entry.server.Client()
	wantAskedAgain(t, "the browser's poll", pollCall(hub.ID(), 0).to(t.Context(), t, client, url), "draining")
	if got := pollWith(t.Context(), t, client, mcpbridge.SteerURL(url)+"/notes"); got.status() !=
		http.StatusServiceUnavailable || got.refusal != "draining" {
		t.Errorf("the notes poll was answered %d %q, want 503 draining", got.status(), got.refusal)
	}
	wantRefused(t, "a GET of the browser routes' notes",
		browserCall{method: http.MethodGet, verb: "notes"}.to(t.Context(), t, client, url), http.StatusNotFound,
		"no_route")
	if got := pollWith(t.Context(), t, client, mcpbridge.SteerURL(url)+"/poll"); got.status() !=
		http.StatusNotFound || got.refusal != "not_found" {
		t.Errorf("a GET of the notes route's poll was answered %d %q, want the holder's 404", got.status(),
			got.refusal)
	}
}

// EVERY END OF AN OPENING CLOSES ITS SCREEN, once: the run's end, a relaunch's
// replacement, a recovery's, and the seat leaving this node alike, unlike the
// note inbox a recovery keeps. An opening of the job again gets a screen of
// its own, under a new hub id the relay reads as its cue to restate what it
// holds, and the new one stays open.
//
// Mutation: keep the screen on a recovery, as the inbox is kept, and the
// recovery's and the seat's leaving go red.
func TestEveryEndOfAnOpeningClosesItsBrowser(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(t *testing.T, f *browserFixture) *recordedHub{
		"the run ends": func(_ *testing.T, f *browserFixture) *recordedHub {
			f.bridge.Close("run-1")
			return nil
		},
		"a relaunch replaces it": func(t *testing.T, f *browserFixture) *recordedHub {
			next := newRecordedHub(t, false)
			relaunch := &mcpbridge.Session{RunID: "run-1", Launch: "job-2", Handle: "dev", Role: "Engineer",
				Surface: f.surface, Ledger: f.ledger, Browser: next}
			if f.bridge.Open(t.Context(), relaunch) == "" {
				t.Fatal("the relaunch minted no endpoint")
			}
			return next
		},
		"a recovery replaces it": func(t *testing.T, f *browserFixture) *recordedHub {
			next := newRecordedHub(t, true)
			f.session.Browser = next
			if f.bridge.Open(t.Context(), f.session) == "" {
				t.Fatal("the recovery minted no endpoint")
			}
			return next
		},
		"the seat leaves": func(_ *testing.T, f *browserFixture) *recordedHub {
			f.bridge.ReleaseSeat(seat())
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hub := newRecordedHub(t, true)
			f := newBrowserFixture(t, hub, nil)
			next := end(t, f)
			if closes := hub.closes.Load(); closes != 1 {
				t.Errorf("the ended opening's screen was closed %d times, want once", closes)
			}
			if next != nil && next.closes.Load() != 0 {
				t.Error("the opening that replaced it closed its own screen")
			}
		})
	}
}

// EACH REFUSAL OF A SCREEN'S IS ANSWERED IN ITS CODE: a post past its rate
// is 429 `too_fast` with the refusing limit's own wait as its Retry-After,
// rounded up and never under a second; one the screen refuses whole is 400
// `invalid_body` saying why, and a poll's query `invalid_query`; a closed
// screen is 503 to ask again; and a body the screen reads past its bound is
// 413. Before the screen: a frame not declared a JPEG is 415, never handed on.
//
// Mutation: answer every Retry-After with a second, or a post past its rate
// with a 503, and a case goes red.
func TestEachHubRefusalIsAnsweredInItsCode(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name       string
		answer     error
		call       browserCall
		status     int
		code, why  string
		retryAfter string
		handed     bool
	}{
		{"a frame past its rate", &browser.TooFast{RetryAfter: 2500 * time.Millisecond}, frameCall(1),
			http.StatusTooManyRequests, "too_fast", "", "3", true},
		{"a state past its rate", &browser.TooFast{RetryAfter: time.Millisecond}, stateCall(1),
			http.StatusTooManyRequests, "too_fast", "", "1", true},
		{"a frame of other dimensions", &browser.Invalid{Why: browser.WhyDimensions}, frameCall(1),
			http.StatusBadRequest, "invalid_body", "dimensions", "", true},
		{"a check-in of another job", &browser.Invalid{Why: browser.WhyCheckin}, checkinCall(),
			http.StatusBadRequest, "invalid_body", "checkin", "", true},
		{"a state of a closed screen", browser.ErrScreenClosed, stateCall(1),
			http.StatusServiceUnavailable, "unavailable", "", "2", true},
		{"a poll of a closed screen", browser.ErrScreenClosed, pollCall("", 0),
			http.StatusServiceUnavailable, "unavailable", "", "2", true},
		{"a poll asking after a cursor", nil, browserCall{method: http.MethodGet, verb: "poll",
			query: "hub=&level=0&after=3"}, http.StatusBadRequest, "invalid_query", "query", "", false},
		{"a state past its bound", nil, browserCall{method: http.MethodPost, verb: "state",
			body: bytes.Repeat([]byte(" "), browser.MaxStateBytes+1)}, http.StatusRequestEntityTooLarge,
			"body_too_large", "", "", true},
		{"a check-in past its bound", nil, browserCall{method: http.MethodPost, verb: "checkin",
			body: bytes.Repeat([]byte(" "), browser.MaxCheckinBytes+1)}, http.StatusRequestEntityTooLarge,
			"body_too_large", "", "", true},
		{"a frame past its bound", nil, browserCall{method: http.MethodPost, verb: "frame",
			contentType: browser.FrameType, meta: frameHead(1),
			body: bytes.Repeat([]byte{0xff}, browser.MaxFrameBytes+1)}, http.StatusRequestEntityTooLarge,
			"body_too_large", "", "", true},
		{"a frame declared a PNG", nil, browserCall{method: http.MethodPost, verb: "frame",
			contentType: "image/png", meta: frameHead(1), body: frameShot()}, http.StatusUnsupportedMediaType,
			"unsupported_media_type", "", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeScreen{answer: c.answer}
			f := newBrowserFixture(t, fake, nil)
			got := c.call.to(t.Context(), t, f.server.Client(), f.url)
			if got.status != c.status || got.code != c.code || got.why != c.why || got.retryAfter != c.retryAfter {
				t.Errorf("answered %d %q why %q, Retry-After %q (%s); want %d %q why %q, Retry-After %q",
					got.status, got.code, got.why, got.retryAfter, got.raw, c.status, c.code, c.why, c.retryAfter)
			}
			fake.mu.Lock()
			handed := fake.handed > 0
			fake.mu.Unlock()
			if handed != c.handed {
				t.Errorf("the call was handed to the screen: %v, want %v", handed, c.handed)
			}
		})
	}
	t.Run("a frame whose metadata passes its bound, by the hub", func(t *testing.T) {
		t.Parallel()
		f := newBrowserFixture(t, newRecordedHub(t, true), nil)
		call := frameCall(1)
		call.meta = strings.Replace(call.meta, `"tab":"t1"`, `"tab":"t1","pad":"`+
			strings.Repeat("x", browser.MaxFrameMetaBytes)+`"`, 1)
		got := call.to(t.Context(), t, f.server.Client(), f.url)
		if got.status != http.StatusBadRequest || got.code != "invalid_body" || got.why != "metadata" {
			t.Errorf("answered %d %q why %q (%s), want 400 invalid_body why metadata", got.status, got.code, got.why,
				got.raw)
		}
	})
}

// A POST IS CHARGED BEFORE ITS BODY IS READ: frames past their bound, posted
// back to back to a real hub whose limits' clock stands still, are each
// refused 413 for the job's burst of four, each spending the job's rate as a
// taken frame does, and the fifth is refused 429 `too_fast` with its body
// unread. So a box posting bodies past their bound in a loop meets its rate,
// and a node reads none past it.
//
// Mutation: read the body before charging, and every one is refused 413.
func TestAPostIsChargedBeforeItsBodyIsRead(t *testing.T) {
	t.Parallel()
	still := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	hub, err := screen.NewHub(screen.HubOptions{Launch: browserJob, Turn: "run-1", Seat: "dev", Serving: true,
		Limits: screen.NewLimits(func() time.Time { return still })})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(hub.Close)
	f := newBrowserFixture(t, hub, nil)
	call := browserCall{method: http.MethodPost, verb: "frame", contentType: browser.FrameType, meta: frameHead(1),
		body: bytes.Repeat([]byte{0xff}, browser.MaxFrameBytes+1)}
	var got []string
	for range 5 {
		answer := call.to(t.Context(), t, f.server.Client(), f.url)
		got = append(got, strconv.Itoa(answer.status)+" "+answer.code)
	}
	want := []string{"413 body_too_large", "413 body_too_large", "413 body_too_large", "413 body_too_large",
		"429 too_fast"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("five frames past their bound were answered %q, want %q", got, want)
	}
}

// A HELD BROWSER POLL IS ANSWERED WHEN IT MOVES, AND AT ITS HOLD. A poll naming
// the hub at its level is held: the first watch lease moves it, and it is
// answered at once with the answer as it then stands; one nothing moves is
// answered at its hold with the answer it waited on. Every field is sent, the
// watch's false and the level's 0 among them.
//
// Mutation: answer a moved poll with the answer it was held on, and it says
// the level it already had.
func TestAHeldBrowserPollIsAnsweredWhenItMovesAndAtItsHold(t *testing.T) {
	t.Parallel()
	hub := newRecordedHub(t, true)
	f := newBrowserFixture(t, hub, nil)
	mcpbridge.HoldPollsFor(f.bridge, time.Minute)
	first := pollCall("", 0).to(t.Context(), t, f.server.Client(), f.url)
	if first.status != http.StatusOK || first.poll.Hub != hub.ID() {
		t.Fatalf("the first poll was answered %d %+v, want the hub's answer at once", first.status, first.poll)
	}
	for _, field := range []string{`"v":1`, `"level":0`, `"on":false`, `"fps":2`} {
		if !strings.Contains(first.raw, field) {
			t.Errorf("the first answer %s does not say %s", first.raw, field)
		}
	}
	answered := make(chan browserAnswer, 1)
	go func() { answered <- pollCall(hub.ID(), 0).to(t.Context(), t, f.server.Client(), f.url) }()
	<-hub.held
	if err := hub.Watch("node-a"); err != nil {
		t.Fatal(err)
	}
	if got := <-answered; got.status != http.StatusOK || got.poll.Level != 1 || !got.poll.Watch.On {
		t.Errorf("the moved poll was answered %d %+v, want level 1, watched", got.status, got.poll)
	}
	quiet := newRecordedHub(t, true)
	short := newBrowserFixture(t, quiet, nil)
	mcpbridge.HoldPollsFor(short.bridge, time.Millisecond)
	if got := pollCall(quiet.ID(), 0).to(t.Context(), t, short.server.Client(), short.url); got.status !=
		http.StatusOK || got.poll != (browser.PollAnswer{V: 1, Hub: quiet.ID(), Watch: browser.Watch{FPS: 2}}) {
		t.Errorf("a poll nothing moved was answered %d %+v at its hold, want the answer it waited on",
			got.status, got.poll)
	}
}
