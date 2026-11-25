package mcpbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A run's note inbox, as its bridge serves it: the steer_take tool a harness's hook
// calls, and the notes route a harness's plugin polls. Every case holds the bridge
// to what it promises the harness — the run's own token and nothing else, an answer
// that is never an error, a poll that never outlives its hold — and to deciding
// nothing about the notes themselves, which the inbox does.

// fakeInbox is a run's inbox with notes a case sets, recording what it is
// asked.
type fakeInbox struct {
	mu      sync.Mutex
	notes   []mcpbridge.SteerNote
	changed chan struct{}
	closed  bool
	takes   []string
	calls   []*sandbox.NoteCall
	cancels []sandbox.NoteCall
	reads   [][]string
	folds   [][]string
	asks    [][]string
	// spentIDs are the notes an ask for a second admission finds at their
	// budget.
	spentIDs []string
	answer   string
	readErr  error
}

func newFakeInbox() *fakeInbox { return &fakeInbox{changed: make(chan struct{})} }

func (i *fakeInbox) Take(_ context.Context, event string, call *sandbox.NoteCall) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.takes = append(i.takes, event)
	i.calls = append(i.calls, call)
	return i.answer
}

func (i *fakeInbox) Cancelled(_ context.Context, call sandbox.NoteCall) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.cancels = append(i.cancels, call)
}

func (i *fakeInbox) Wait(_ context.Context, after int) ([]mcpbridge.SteerNote, <-chan struct{}) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var out []mcpbridge.SteerNote
	for _, n := range i.notes {
		if n.Seq > after {
			out = append(out, n)
		}
	}
	return out, i.changed
}

// Read records the read, or, AS THE CONTRACT SAYS, takes none once the inbox is
// closed and says so.
func (i *fakeInbox) Read(_ context.Context, ids []string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return mcpbridge.ErrNoteUntaken
	}
	i.reads = append(i.reads, ids)
	return i.readErr
}

// Again grants every note asked its id with "again:" before it, but those in
// spentIDs, which it answers spent; it records each ask.
func (i *fakeInbox) Again(_ context.Context, ids []string) (map[string]string, []string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.asks = append(i.asks, ids)
	granted, spent := map[string]string{}, []string(nil)
	for _, id := range ids {
		if slices.Contains(i.spentIDs, id) {
			spent = append(spent, id)
			continue
		}
		granted[id] = "again:" + id
	}
	return granted, spent, nil
}

func (i *fakeInbox) Folded(_ context.Context, ids []string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.folds = append(i.folds, ids)
}

func (i *fakeInbox) Close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.closed {
		i.closed = true
		close(i.changed)
	}
}

// file adds a note and wakes every poll waiting on the inbox.
func (i *fakeInbox) file(n mcpbridge.SteerNote) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.notes = append(i.notes, n)
	close(i.changed)
	i.changed = make(chan struct{})
}

func (i *fakeInbox) taken() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.takes)
}

// readIDs is the notes of every read the inbox was asked to record, a read a
// slice, in the order they came.
func (i *fakeInbox) readIDs() [][]string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.reads)
}

// steerFixture is a bridge serving one run whose session holds an inbox, on
// both of the bridge's routes.
type steerFixture struct {
	*fixture
	inbox *fakeInbox
	url   string
	// inflight is how many requests the server is answering now.
	inflight *atomic.Int32
}

func newSteerFixture(t *testing.T, hold time.Duration) *steerFixture {
	t.Helper()
	return newSteerFixtureWith(t, hold, nil)
}

// newSteerFixtureWith is [newSteerFixture] with the session edited before it
// is opened, for a case about a session opened some other way.
func newSteerFixtureWith(t *testing.T, hold time.Duration, edit func(*mcpbridge.Session)) *steerFixture {
	t.Helper()
	f := newFixture(t)
	inbox := newFakeInbox()
	f.session.Steer = inbox
	if edit != nil {
		edit(f.session)
	}
	f.bridge = bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: f.server.URL,
	})
	mcpbridge.HoldPollsFor(f.bridge, hold)
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", f.bridge.Handler())
	mux.Handle(mcpbridge.ServerPattern, f.bridge.Handler())
	mux.Handle(mcpbridge.SteerPattern, f.bridge.SteerHandler())
	inflight := &atomic.Int32{}
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		defer inflight.Add(-1)
		mux.ServeHTTP(w, r)
	})
	url := f.bridge.Open(t.Context(), f.session)
	if url == "" {
		t.Fatal("no endpoint was minted")
	}
	return &steerFixture{fixture: f, inbox: inbox, url: url, inflight: inflight}
}

// poll is one GET of the notes route, and how long it was held.
func (f *steerFixture) poll(ctx context.Context, t *testing.T, url string) (*http.Response, []mcpbridge.SteerNote, time.Duration) {
	t.Helper()
	got := f.answer(ctx, t, url)
	return got.res, got.notes, got.took
}

// polled is one poll's answer: the response, the notes of a 200, the error
// code of a refusal, and how long the poll was held.
type polled struct {
	res     *http.Response
	notes   []mcpbridge.SteerNote
	refusal string
	took    time.Duration
}

// status is the answer's status, or 0 where the call failed on the wire.
func (p polled) status() int {
	if p.res == nil {
		return 0
	}
	return p.res.StatusCode
}

// answer is one GET of the notes route, read whole.
func (f *steerFixture) answer(ctx context.Context, t *testing.T, url string) polled {
	t.Helper()
	return pollWith(ctx, t, f.server.Client(), url)
}

// pollWith is one GET of the notes route through client, read whole.
func pollWith(ctx context.Context, t *testing.T, client *http.Client, url string) polled {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	res, err := client.Do(req)
	if err != nil {
		return polled{took: time.Since(began)}
	}
	defer res.Body.Close()
	var body struct {
		Notes []mcpbridge.SteerNote `json:"notes"`
		Error string                `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil && res.StatusCode == http.StatusOK {
		t.Errorf("decode the poll's answer: %v", err)
	}
	return polled{res: res, notes: body.Notes, refusal: body.Error, took: time.Since(began)}
}

// A TOKEN THE ROUTE CANNOT RESOLVE IS REFUSED, AND SAYS NOTHING MORE: a forged
// one, the token of a job a relaunch replaced, and a finished run's are three
// facts, and telling a caller which one it was tells anybody the same — so each
// is the bridge's own 401, naming no reason, and the inbox is never asked.
func TestTheNotesRouteRefusesATokenItCannotResolve(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, time.Minute)
	superseded := f.url
	relaunch := &mcpbridge.Session{RunID: "run-1", Launch: "job-2", Handle: "dev", Role: "Engineer",
		Surface: f.surface, Ledger: f.ledger, Steer: newFakeInbox()}
	current := f.bridge.Open(t.Context(), relaunch)
	finished := newSteerFixture(t, time.Minute)
	finished.bridge.Close("run-1")
	for name, url := range map[string]string{
		"forged":     f.server.URL + mcpbridge.PathPrefix + "v1.run-1.99999999999.x/steer/notes",
		"superseded": mcpbridge.SteerURL(superseded) + "/notes",
		"finished":   mcpbridge.SteerURL(finished.url) + "/notes",
	} {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := f.server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if res.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", res.StatusCode)
			}
			for _, word := range []string{"forged", "expired", "relaunch", "ended", "job"} {
				if strings.Contains(strings.ToLower(string(body)), word) {
					t.Errorf("the refusal says why (%q): %s", word, body)
				}
			}
		})
	}
	if current == "" {
		t.Fatal("the relaunch minted no endpoint")
	}
}

// A SESSION WITH NO INBOX HAS NO NOTES ROUTE: it answers 404, never an empty
// list a poller would hold open for ever, and its steer_take answers nothing.
func TestTheNotesRouteIsAbsentForASessionWithNoInbox(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, time.Minute)
	bare := &mcpbridge.Session{RunID: "run-2", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: f.surface, Ledger: f.ledger}
	url := f.bridge.Open(t.Context(), bare)
	res, _, _ := f.poll(t.Context(), t, mcpbridge.SteerURL(url)+"/notes")
	if res == nil || res.StatusCode != http.StatusNotFound {
		t.Fatalf("a session with no inbox answered %v, want 404", res)
	}
	sess := dial(t, url)
	if got := callTool(t, sess, mcpbridge.SteerTool, map[string]any{"event": "Stop"}); got.IsError ||
		text(got) != "" {
		t.Errorf("steer_take with no inbox answered %q (error %v), want nothing", text(got), got.IsError)
	}
}

// A HELD POLL RETURNS AT ONCE ON A CHANGE, A CLOSE OR A SHUTDOWN — the note that
// arrived, the run that ended, the node that is stopping — and never waits out
// its hold for any of them. A POLL THE OPENING CAN NO LONGER HOLD IS TURNED AWAY
// AS A FAILURE: a 503 with a Retry-After, the held one and every one that
// arrives after it, never a 200 with nothing — which a poller whose next poll is
// the answer to its last reads as "ask again now", and asked again at once for
// as long as the stopping node served its route.
//
// Mutation: answer the shutdown or the ended opening with an empty 200, as the
// route did, and the cases for them go red.
func TestAHeldPollReturnsOnAChangeACloseOrShutdown(t *testing.T) {
	t.Parallel()
	const hold = time.Minute
	for name, tc := range map[string]struct {
		wake   func(f *steerFixture)
		status int
		code   string
	}{
		"a note arrives": {
			wake: func(f *steerFixture) {
				f.inbox.file(mcpbridge.SteerNote{Seq: 1, ID: "n-1", Inbox: "msg_1", Message: "use staging"})
			},
			status: http.StatusOK,
		},
		"the run ends": {
			wake:   func(f *steerFixture) { f.bridge.Close("run-1") },
			status: http.StatusServiceUnavailable, code: "unavailable",
		},
		"the node stops": {
			wake:   func(f *steerFixture) { f.bridge.Shutdown() },
			status: http.StatusServiceUnavailable, code: "draining",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newSteerFixture(t, hold)
			done := make(chan polled, 1)
			go func() { done <- f.answer(t.Context(), t, mcpbridge.SteerURL(f.url)+"/notes") }()
			time.Sleep(100 * time.Millisecond)
			tc.wake(f)
			select {
			case got := <-done:
				if got.res == nil || got.res.StatusCode != tc.status {
					t.Fatalf("the poll answered %v, want %d", got.res, tc.status)
				}
				if tc.status == http.StatusOK {
					if len(got.notes) != 1 || got.notes[0].ID != "n-1" {
						t.Errorf("the poll answered %+v, want the note that arrived", got.notes)
					}
					return
				}
				wantTurnedAway(t, got, tc.code)
			case <-time.After(hold / 2):
				t.Fatal("the poll waited out its hold")
			}
		})
	}
	t.Run("a poll after the shutdown", func(t *testing.T) {
		t.Parallel()
		f := newSteerFixture(t, hold)
		f.inbox.file(mcpbridge.SteerNote{Seq: 1, ID: "n-1", Inbox: "msg_1", Message: "use staging"})
		f.bridge.Shutdown()
		f.bridge.Shutdown()
		got := f.answer(t.Context(), t, mcpbridge.SteerURL(f.url)+"/notes")
		if got.res == nil || got.res.StatusCode != http.StatusServiceUnavailable || got.took > hold/2 {
			t.Fatalf("a poll after the shutdown answered %v after %s, want a 503 at once", got.res, got.took)
		}
		if len(got.notes) != 0 {
			t.Errorf("a stopping node handed a poll %+v", got.notes)
		}
		wantTurnedAway(t, got, "draining")
	})
}

// wantTurnedAway fails the case unless a poll's answer is a refusal a poller
// pauses on: code, and a Retry-After of whole seconds, at least one.
func wantTurnedAway(t *testing.T, got polled, code string) {
	t.Helper()
	if got.refusal != code {
		t.Errorf("the refusal's code is %q, want %q", got.refusal, code)
	}
	if after, err := strconv.Atoi(got.res.Header.Get("Retry-After")); err != nil || after < 1 {
		t.Errorf("the refusal carries Retry-After %q, want whole seconds, at least one",
			got.res.Header.Get("Retry-After"))
	}
}

// A NOTES CALL THAT REACHES ITS OPENING AS IT ENDS IS TURNED AWAY TO ASK
// AGAIN, a poll and a read alike, as a poll the opening can no longer hold
// is: a 503 with a Retry-After, which the poller pauses on before it asks
// again, whether the run ended, the seat left this node or a recovery opened
// the job again; never the 404 of a session that never had an inbox (package
// doc, "A note inbox"). The read is recorded nowhere, and after a recovery
// the box's next call is served by the opening that serves it now.
//
// Mutation: answer an opening whose end took its inbox as a session that never
// had one (the 404 for every opening without an inbox), and every case goes
// red; answer a session that never had an inbox as an ended opening, and
// TestTheNotesRouteIsAbsentForASessionWithNoInbox goes red.
func TestANotesCallReachingItsOpeningAsItEndsIsTurnedAwayToAskAgain(t *testing.T) {
	t.Parallel()
	for _, end := range []struct {
		name string
		end  func(t *testing.T, f *steerFixture)
		// recovers is an end after which this node serves the box's next
		// call itself, from the opening that replaced the ended one.
		recovers bool
	}{
		{name: "the run ends", end: func(_ *testing.T, f *steerFixture) { f.bridge.Close("run-1") }},
		{name: "the seat leaves this node",
			end: func(_ *testing.T, f *steerFixture) { f.bridge.ReleaseSeat(seat()) }},
		{name: "a recovery opens the job again", recovers: true, end: func(t *testing.T, f *steerFixture) {
			if f.bridge.Open(t.Context(), f.session) == "" {
				t.Fatal("the recovery minted no endpoint")
			}
		}},
	} {
		for _, call := range notesCalls() {
			t.Run(end.name+", "+call.name, func(t *testing.T) {
				t.Parallel()
				parked := newParkedFence()
				f := newSteerFixtureWith(t, time.Minute, func(s *mcpbridge.Session) { s.Fence = parked.fence })
				t.Cleanup(parked.letGo)
				f.inbox.file(mcpbridge.SteerNote{Seq: 1, ID: "n-1", Inbox: "msg_1", Message: "use staging"})

				parked.arm()
				answered := make(chan polled, 1)
				go func() { answered <- call.make(t.Context(), t, f) }()
				// THE ROUTING FOUND THE CALL'S OPENING, and the call waits in
				// the session's fence, short of the route that serves it.
				<-parked.entered
				end.end(t, f)
				parked.letGo()
				got := <-answered
				if got.res == nil {
					t.Fatalf("%s reaching its opening as it ended failed on the wire", call.name)
				}
				if got.res.StatusCode != http.StatusServiceUnavailable {
					t.Fatalf("%s reaching its opening as it ended was answered %d %q, want a 503 to ask again",
						call.name, got.res.StatusCode, got.refusal)
				}
				wantTurnedAway(t, got, "unavailable")
				if reads := f.inbox.readIDs(); len(reads) != 0 {
					t.Errorf("the inbox recorded the reads %q of a call its opening's end turned away", reads)
				}
				if end.recovers {
					call.served(t, f, call.make(t.Context(), t, f))
				}
			})
		}
	}
}

// notesCall is one call a harness's plugin makes on the notes route.
type notesCall struct {
	name string
	// make makes the call on the fixture's job, naming the note n-1 where it
	// names one.
	make func(ctx context.Context, t *testing.T, f *steerFixture) polled
	// served fails the case unless got is the answer of an opening that
	// serves the call, with n-1 waiting on the fixture's inbox.
	served func(t *testing.T, f *steerFixture, got polled)
}

// notesCalls is a poll and a read.
func notesCalls() []notesCall {
	return []notesCall{
		{
			name: "a poll",
			make: func(ctx context.Context, t *testing.T, f *steerFixture) polled {
				return f.answer(ctx, t, mcpbridge.SteerURL(f.url)+"/notes")
			},
			served: func(t *testing.T, _ *steerFixture, got polled) {
				t.Helper()
				if got.res == nil || got.res.StatusCode != http.StatusOK || len(got.notes) != 1 ||
					got.notes[0].ID != "n-1" {
					t.Errorf("the box's next poll was answered %v %+v, want the note waiting", got.res, got.notes)
				}
			},
		},
		{
			name: "a read",
			make: func(ctx context.Context, t *testing.T, f *steerFixture) polled {
				return readWith(ctx, t, f.server.Client(), mcpbridge.SteerURL(f.url)+"/read", "n-1")
			},
			served: func(t *testing.T, f *steerFixture, got polled) {
				t.Helper()
				if got.res == nil || got.res.StatusCode != http.StatusNoContent {
					t.Errorf("the box's next read was answered %v, want 204", got.res)
				}
				if reads := f.inbox.readIDs(); len(reads) != 1 || !slices.Equal(reads[0], []string{"n-1"}) {
					t.Errorf("the inbox recorded the reads %q, want the read of n-1 alone", reads)
				}
			},
		},
	}
}

// readWith is one read of the notes route through client, naming ids, read
// whole: the response, and the error code of a refusal.
func readWith(ctx context.Context, t *testing.T, client *http.Client, url string, ids ...string) polled {
	t.Helper()
	body, err := json.Marshal(map[string][]string{"ids": ids})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return polled{}
	}
	defer res.Body.Close()
	var refusal struct {
		Error string `json:"error"`
	}
	if res.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(res.Body).Decode(&refusal); err != nil {
			t.Errorf("decode the read's answer: %v", err)
		}
	}
	return polled{res: res, refusal: refusal.Error}
}

// A READ ITS OPENING'S END OVERTOOK ON ITS WAY IN IS TURNED AWAY TO ASK AGAIN,
// never answered done with nothing recorded (package doc, "A note inbox"): a
// closed inbox says it took nothing ([mcpbridge.ErrNoteUntaken]), and a run
// whose tools are closed turns its read away whether or not the seat's release
// has ended the opening yet, each with a 503 and the Retry-After a poll the
// opening cannot hold is given, never as a record that could not be written.
// After a recovery the box's read made again is recorded by the opening that
// serves it now.
//
// Mutation: answer a read its inbox did not take done, or as a record that
// could not be written, and the run's end and the recovery go red; answer a
// fenced run's read done, and the seat's leaving goes red, before and after.
func TestAReadItsOpeningsEndOvertookIsTurnedAwayToAskAgain(t *testing.T) {
	t.Parallel()
	for _, end := range []struct {
		name string
		// end ends the opening, closing the seat's fence first where the
		// seat leaves, or closes the fence alone where the seat's release is
		// still to come, and answers the inbox the box's next read reaches,
		// nil where none on this node serves it.
		end func(t *testing.T, f *steerFixture, fenced *atomic.Bool) *fakeInbox
	}{
		{"the run ends", func(_ *testing.T, f *steerFixture, _ *atomic.Bool) *fakeInbox {
			f.bridge.Close("run-1")
			return nil
		}},
		{"the seat leaves this node", func(_ *testing.T, f *steerFixture, fenced *atomic.Bool) *fakeInbox {
			fenced.Store(true)
			f.bridge.ReleaseSeat(seat())
			return nil
		}},
		{"the seat's grants drop, its release still to come", func(_ *testing.T, _ *steerFixture,
			fenced *atomic.Bool) *fakeInbox {
			fenced.Store(true)
			return nil
		}},
		{"a recovery opens the job again", func(t *testing.T, f *steerFixture, _ *atomic.Bool) *fakeInbox {
			inbox := newFakeInbox()
			if f.bridge.Open(t.Context(), &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "dev",
				Role: "Engineer", Surface: f.surface, Ledger: f.ledger, Steer: inbox}) == "" {
				t.Fatal("the recovery minted no endpoint")
			}
			return inbox
		}},
	} {
		t.Run(end.name, func(t *testing.T) {
			t.Parallel()
			parked := newParkedFence()
			var asks atomic.Int32
			var fenced atomic.Bool
			// THE READ'S OWN ASK OF THE FENCE IS THE SECOND: the routing asks
			// first, and the read asks once its route found the inbox serving
			// and read its body, as the last step before the inbox.
			f := newSteerFixtureWith(t, time.Minute, func(s *mcpbridge.Session) {
				s.Fence = func() error {
					if asks.Add(1) == 2 {
						_ = parked.fence()
					}
					if fenced.Load() {
						return errors.New("the seat left this node")
					}
					return nil
				}
			})
			t.Cleanup(parked.letGo)
			parked.arm()
			url := mcpbridge.SteerURL(f.url) + "/read"
			answered := make(chan polled, 1)
			go func() { answered <- readWith(t.Context(), t, f.server.Client(), url, "n-1") }()
			<-parked.entered
			next := end.end(t, f, &fenced)
			parked.letGo()
			got := <-answered
			if got.res == nil {
				t.Fatal("a read its opening's end overtook failed on the wire")
			}
			if got.res.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("a read its opening's end overtook was answered %d %q, want a 503 to ask again",
					got.res.StatusCode, got.refusal)
			}
			wantTurnedAway(t, got, "unavailable")
			if after := got.res.Header.Get("Retry-After"); after != strconv.Itoa(int(mcpbridge.AskAgainRetry/time.Second)) {
				t.Errorf("the read is told to ask again after %q, want %s, as a call its opening's end "+
					"overtook is", after, mcpbridge.AskAgainRetry)
			}
			if reads := f.inbox.readIDs(); len(reads) != 0 {
				t.Errorf("the ended opening's inbox recorded the reads %q", reads)
			}
			if next == nil {
				return
			}
			if again := readWith(t.Context(), t, f.server.Client(), url, "n-1"); again.status() != http.StatusNoContent {
				t.Errorf("the box's read made again was answered %d %q, want 204", again.status(), again.refusal)
			}
			if reads := next.readIDs(); len(reads) != 1 || !slices.Equal(reads[0], []string{"n-1"}) {
				t.Errorf("the recovery's inbox recorded the reads %q, want the read of n-1 alone", reads)
			}
		})
	}
}

// parkedFence is a session's fence that holds every request asking it once it
// is armed, until it is let go: on the notes route, a call whose opening the
// routing found and the route has not yet served ([mcpbridge.Bridge.SteerHandler]),
// which is where the opening's end can come between the two.
type parkedFence struct {
	armed   chan struct{}
	arm     func()
	entered chan struct{}
	enter   sync.Once
	release chan struct{}
	letGo   func()
}

func newParkedFence() *parkedFence {
	p := &parkedFence{armed: make(chan struct{}), entered: make(chan struct{}), release: make(chan struct{})}
	p.arm = sync.OnceFunc(func() { close(p.armed) })
	p.letGo = sync.OnceFunc(func() { close(p.release) })
	return p
}

// fence is the session's fence: open, and once armed, open only once it is let
// go, entered closed by the first request it holds.
func (p *parkedFence) fence() error {
	select {
	case <-p.armed:
		p.enter.Do(func() { close(p.entered) })
		<-p.release
	default:
	}
	return nil
}

// A HELD POLL ENDS AT ITS HOLD, with nothing: a gateway between a box and the
// bridge cuts a request held longer than its own timeout, so the bridge answers
// first and the poller asks again.
func TestAHeldPollEndsAtItsHold(t *testing.T) {
	t.Parallel()
	if mcpbridge.PollHold >= 15*time.Second {
		t.Errorf("a poll is held %s, past the 15 s an Envoy route allows by default", mcpbridge.PollHold)
	}
	const hold = 300 * time.Millisecond
	f := newSteerFixture(t, hold)
	res, notes, took := f.poll(t.Context(), t, mcpbridge.SteerURL(f.url)+"/notes?after=0")
	if res == nil || res.StatusCode != http.StatusOK || len(notes) != 0 {
		t.Fatalf("an empty poll answered %v %+v", res, notes)
	}
	if took < hold || took > 10*hold {
		t.Errorf("an empty poll was held %s, want its hold of %s", took, hold)
	}
	// AND A POLL PAST THE CURSOR ANSWERS ONLY WHAT CAME AFTER IT, at once.
	f.inbox.file(mcpbridge.SteerNote{Seq: 1, ID: "n-1"})
	f.inbox.file(mcpbridge.SteerNote{Seq: 2, ID: "n-2"})
	_, notes, took = f.poll(t.Context(), t, mcpbridge.SteerURL(f.url)+"/notes?after=1")
	if len(notes) != 1 || notes[0].ID != "n-2" || took >= hold {
		t.Errorf("a poll after seq 1 answered %+v after %s", notes, took)
	}
	if res, _, _ := f.poll(t.Context(), t, mcpbridge.SteerURL(f.url)+"/notes?after=-1"); res == nil ||
		res.StatusCode != http.StatusBadRequest {
		t.Errorf("a cursor that is no place answered %v", res)
	}
}

// A CANCELLED POLL FREES ITS WAITER: a poller that goes away mid-hold leaves
// nothing of its poll held on the bridge — the handler returns rather than
// waiting out the hold for a poller that will never read the answer — and the
// next poll is answered as the first would have been.
func TestACancelledPollFreesItsWaiter(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = f.poll(ctx, t, mcpbridge.SteerURL(f.url)+"/notes")
	}()
	deadline := time.Now().Add(10 * time.Second)
	for f.inflight.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the poll never reached the bridge")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	for f.inflight.Load() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the cancelled poll's handler is still held")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.inbox.file(mcpbridge.SteerNote{Seq: 1, ID: "n-1"})
	if _, notes, _ := f.poll(t.Context(), t, mcpbridge.SteerURL(f.url)+"/notes"); len(notes) != 1 {
		t.Errorf("the poll after a cancelled one answered %+v", notes)
	}
}

// A READ'S BODY IS CAPPED: a read names what one job can be owed — a couple
// of kilobytes — and a body far past that is refused before the inbox is
// asked, as one naming no note is.
func TestAReadBodyIsCapped(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, time.Minute)
	url := mcpbridge.SteerURL(f.url) + "/read"
	post := func(body string) int {
		res, err := f.server.Client().Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	huge := `{"ids":["` + strings.Repeat("a", 70<<10) + `"]}`
	if got := post(huge); got != http.StatusRequestEntityTooLarge {
		t.Errorf("a 70 KiB read answered %d, want 413", got)
	}
	if got := post(`{"ids":[]}`); got != http.StatusBadRequest {
		t.Errorf("a read naming nothing answered %d, want 400", got)
	}
	if got := post(`{"ids":["n-1"]}`); got != http.StatusNoContent {
		t.Errorf("a read answered %d, want 204", got)
	}
	f.inbox.mu.Lock()
	reads := slices.Clone(f.inbox.reads)
	f.inbox.mu.Unlock()
	if len(reads) != 1 || !slices.Equal(reads[0], []string{"n-1"}) {
		t.Errorf("the inbox was asked to read %v, want the one well-formed read", reads)
	}
	if res, err := f.server.Client().Get(url); err == nil {
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("a GET of read answered %d, want 405", res.StatusCode)
		}
		_ = res.Body.Close()
	}
}

// A FOLD IS GATED LIKE A READ AND CARRIES IDS ALONE: behind the run's own
// token (a forged or finished one is the bridge's 401 and the inbox is never
// asked), its body bounded as a read's, and only the ids reach the inbox, so
// no note's text can ride it into the engine's log, whatever the box sends.
//
// Mutation: answer `folded` before serveSteer's token routing, or skip its
// body's bounds, and this goes red.
func TestAFoldIsGatedLikeAReadAndCarriesIDsAlone(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, time.Minute)
	finished := newSteerFixture(t, time.Minute)
	finished.bridge.Close("run-1")
	post := func(url, body string) int {
		res, err := f.server.Client().Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	const withText = `{"ids":["n-1"],"message":"the note's own words"}`
	for name, url := range map[string]string{
		"forged":   f.server.URL + mcpbridge.PathPrefix + "v1.run-1.99999999999.x/steer/folded",
		"finished": mcpbridge.SteerURL(finished.url) + "/folded",
	} {
		if got := post(url, withText); got != http.StatusUnauthorized {
			t.Errorf("a fold under a %s token answered %d, want 401", name, got)
		}
	}
	url := mcpbridge.SteerURL(f.url) + "/folded"
	if got := post(url, `{"ids":[]}`); got != http.StatusBadRequest {
		t.Errorf("a fold naming nothing answered %d, want 400", got)
	}
	if got := post(url, `{"ids":["`+strings.Repeat("a", 70<<10)+`"]}`); got != http.StatusRequestEntityTooLarge {
		t.Errorf("a 70 KiB fold answered %d, want 413", got)
	}
	if got := post(url, withText); got != http.StatusNoContent {
		t.Errorf("a fold answered %d, want 204", got)
	}
	f.inbox.mu.Lock()
	folds, reads := slices.Clone(f.inbox.folds), slices.Clone(f.inbox.reads)
	f.inbox.mu.Unlock()
	finished.inbox.mu.Lock()
	finishedFolds := len(finished.inbox.folds)
	finished.inbox.mu.Unlock()
	if len(folds) != 1 || !slices.Equal(folds[0], []string{"n-1"}) || finishedFolds != 0 {
		t.Errorf("the inboxes were handed the folds %v and %d, want the one well-formed fold's id alone",
			folds, finishedFolds)
	}
	if len(reads) != 0 {
		t.Errorf("a fold recorded the reads %v", reads)
	}
	if res, err := f.server.Client().Get(url); err == nil {
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("a GET of folded answered %d, want 405", res.StatusCode)
		}
		_ = res.Body.Close()
	}
}

// AN ASK FOR A SECOND ADMISSION IS GATED LIKE A READ AND ANSWERS IDS ALONE:
// behind the run's own token (a forged or finished one is the bridge's 401 and
// the inbox is never asked), its body bounded as a read's, only the ids reach
// the inbox, and the answer is the id each granted note is admitted under and
// the ids at their budget, which the plugin gives up: never a note's words.
//
// Mutation: answer `again` before serveSteer's token routing, skip its body's
// bounds, or drop the spent ids from the answer, and this goes red.
func TestAnAskForASecondAdmissionIsGatedLikeAReadAndAnswersIDsAlone(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, time.Minute)
	f.inbox.spentIDs = []string{"n-2"}
	finished := newSteerFixture(t, time.Minute)
	finished.bridge.Close("run-1")
	post := func(url, body string) (int, string) {
		res, err := f.server.Client().Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(raw)
	}
	const withText = `{"ids":["n-1","n-2"],"message":"the note's own words"}`
	for name, url := range map[string]string{
		"forged":   f.server.URL + mcpbridge.PathPrefix + "v1.run-1.99999999999.x/steer/again",
		"finished": mcpbridge.SteerURL(finished.url) + "/again",
	} {
		if got, _ := post(url, withText); got != http.StatusUnauthorized {
			t.Errorf("an ask under a %s token answered %d, want 401", name, got)
		}
	}
	url := mcpbridge.SteerURL(f.url) + "/again"
	if got, _ := post(url, `{"ids":[]}`); got != http.StatusBadRequest {
		t.Errorf("an ask naming nothing answered %d, want 400", got)
	}
	if got, _ := post(url, `{"ids":["`+strings.Repeat("a", 70<<10)+`"]}`); got != http.StatusRequestEntityTooLarge {
		t.Errorf("a 70 KiB ask answered %d, want 413", got)
	}
	status, body := post(url, withText)
	if status != http.StatusOK || strings.TrimSpace(body) != `{"again":{"n-1":"again:n-1"},"spent":["n-2"]}` {
		t.Errorf("an ask answered %d %s, want n-1 granted and n-2 spent, ids alone", status, body)
	}
	f.inbox.mu.Lock()
	asks := slices.Clone(f.inbox.asks)
	f.inbox.mu.Unlock()
	finished.inbox.mu.Lock()
	finishedAsks := len(finished.inbox.asks)
	finished.inbox.mu.Unlock()
	if len(asks) != 1 || !slices.Equal(asks[0], []string{"n-1", "n-2"}) || finishedAsks != 0 {
		t.Errorf("the inboxes were asked %v and %d, want the one well-formed ask's ids alone", asks,
			finishedAsks)
	}
	if res, err := f.server.Client().Get(url); err == nil {
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("a GET of again answered %d, want 405", res.StatusCode)
		}
		_ = res.Body.Close()
	}
}

// A READ NAMING EVERYTHING ONE JOB CAN BE OWED IS TAKEN: a turn's notes and
// the job's own items, its late calls and its wakes. OpenCode's plugin
// confirms every note it owes in one read and sends that read again on every
// retry, so a read refused for its count would confirm nothing more for the
// rest of the run. One more is refused before the inbox is asked.
func TestAReadNamingEveryNoteAndItemOneJobCanOweIsTaken(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, time.Minute)
	url := mcpbridge.SteerURL(f.url) + "/read"
	post := func(ids []string) int {
		body, err := json.Marshal(map[string][]string{"ids": ids})
		if err != nil {
			t.Fatal(err)
		}
		res, err := f.server.Client().Post(url, "application/json", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	owed := steer.MaxTurnNotes + sandbox.MaxJobItems + sandbox.MaxJobWakes
	ids := make([]string, owed+1)
	for i := range ids {
		ids[i] = "n-" + strconv.Itoa(i+1)
	}
	if got := post(ids); got != http.StatusBadRequest {
		t.Errorf("a read naming %d notes, one past what a job can be owed, answered %d, want 400", len(ids), got)
	}
	if got := post(ids[:owed]); got != http.StatusNoContent {
		t.Errorf("a read naming the %d notes and items a job can be owed answered %d, want 204", owed, got)
	}
	if reads := f.inbox.readIDs(); len(reads) != 1 || !slices.Equal(reads[0], ids[:owed]) {
		t.Errorf("the inbox was asked to read %v, want the one read of %d", reads, owed)
	}
}

// STEER_TAKE IS SERVED UNLISTED AND NEVER RECORDED: no harness offers it to its
// model, it never reaches the run's surface or its ledger — it is the harness
// asking, not the model — and its answer is always text, never an error. Only a
// boundary of the main agent's loop reaches the inbox: a subagent's batch, an
// agent id the harness did not fill in and any other event take nothing.
func TestSteerTakeIsServedUnlistedAndNeverRecorded(t *testing.T) {
	t.Parallel()
	f := newSteerFixture(t, time.Minute)
	f.inbox.answer = `{"hookSpecificOutput":{"hookEventName":"PostToolBatch","additionalContext":"x"}}`
	sess := dial(t, f.url)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	listed, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if slices.ContainsFunc(listed.Tools, func(tl *mcp.Tool) bool { return tl.Name == mcpbridge.SteerTool }) {
		t.Fatal("steer_take is listed, so a harness would offer it to its model")
	}
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"event": "PostToolBatch", "agent": ""}, f.inbox.answer},
		{map[string]any{"event": "Stop"}, f.inbox.answer},
		{map[string]any{"event": "PostToolBatch", "agent": "a1"}, ""},
		{map[string]any{"event": "PostToolBatch", "agent": "${agent_id}"}, ""},
		{map[string]any{"event": "PreToolUse"}, ""},
		{map[string]any{"event": 7}, ""},
	} {
		res := callTool(t, sess, mcpbridge.SteerTool, tc.args)
		if res.IsError || text(res) != tc.want {
			t.Errorf("steer_take %v answered %q (error %v), want %q", tc.args, text(res), res.IsError, tc.want)
		}
	}
	if got := f.inbox.taken(); !slices.Equal(got, []string{"PostToolBatch", "Stop"}) {
		t.Errorf("the inbox was asked at %v, want the main agent's two boundaries", got)
	}
	if rows := f.ledger.rows(); len(rows) != 0 {
		t.Errorf("steer_take was recorded on the run's ledger: %+v", rows)
	}
}

// A RUN WHOSE TOOLS ARE CLOSED TAKES NOTHING MORE FROM ITS NOTES, its seat moved
// or a person having stopped it, on either way in: its steer_take answers
// nothing, its poll holds as though nothing waited, and its read records
// nothing. The notes wait for whatever reads the turn next. AND ITS READ IS
// TURNED AWAY TO ASK AGAIN, never answered done: the plugin would never confirm
// the notes again, though OpenCode delivered them, and the read's next try
// reaches the seat's next holder, which records it, or the run's end, which
// refuses it.
//
// Mutation: answer a fenced run's read done, and the read goes red.
func TestAFencedRunTakesNothingMoreFromItsNotes(t *testing.T) {
	t.Parallel()
	const hold = 300 * time.Millisecond
	f := newSteerFixture(t, hold)
	inbox := newFakeInbox()
	inbox.answer = `{"hookSpecificOutput":{"hookEventName":"Stop","additionalContext":"x"}}`
	inbox.notes = []mcpbridge.SteerNote{{Seq: 1, ID: "n-1", Inbox: "msg_1", Message: "use staging"}}
	fenced := &mcpbridge.Session{RunID: "run-2", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: f.surface, Ledger: f.ledger, Steer: inbox,
		Fence: func() error { return errors.New("the seat was stopped") }}
	url := f.bridge.Open(t.Context(), fenced)
	if url == "" {
		t.Fatal("no endpoint was minted")
	}
	sess := dial(t, url)
	if res := callTool(t, sess, mcpbridge.SteerTool, map[string]any{"event": "Stop"}); text(res) != "" {
		t.Errorf("a fenced run's steer_take answered %q", text(res))
	}
	res, notes, took := f.poll(t.Context(), t, mcpbridge.SteerURL(url)+"/notes")
	if res == nil || res.StatusCode != http.StatusOK || len(notes) != 0 || took < hold {
		t.Errorf("a fenced run's poll answered %v %+v after %s, want nothing at its hold", res, notes, took)
	}
	read := readWith(t.Context(), t, f.server.Client(), mcpbridge.SteerURL(url)+"/read", "n-1")
	inbox.mu.Lock()
	reads, takes := len(inbox.reads), len(inbox.takes)
	inbox.mu.Unlock()
	if read.status() != http.StatusServiceUnavailable || reads != 0 || takes != 0 {
		t.Fatalf("a fenced run's read answered %d %q; the inbox was read %d and taken %d times, "+
			"want a 503 to ask again and neither", read.status(), read.refusal, reads, takes)
	}
	wantTurnedAway(t, read, "unavailable")
}

// THE NOTES ROUTE REACHES THE SESSION'S HOLDER THROUGH THE RELAY, like every
// bridged call: a box's poll lands on the fleet's address, the node there reads
// the seat's lease, and the holder answers it from its inbox — the token parsed
// as the first segment of a path beneath the bridge's prefix.
func TestTheNotesRouteReachesTheSessionsHolderThroughTheRelay(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	f := newFleet(t, 2, 10*time.Second)
	entry, holder := f.members[0], f.members[1]
	for _, m := range f.members {
		mux := m.server.Config.Handler.(*http.ServeMux)
		mux.Handle(mcpbridge.SteerPattern, m.bridge.SteerHandler())
	}
	inbox := newFakeInbox()
	inbox.notes = []mcpbridge.SteerNote{{Seq: 1, ID: "n-1", Inbox: "msg_1", Message: "use staging"}}
	session := &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: fx.surface, Ledger: fx.ledger, Steer: inbox}
	url := f.takeOver(t, holder, session)
	if !strings.HasPrefix(url, entry.server.URL) {
		t.Fatalf("the endpoint %q is not the fleet's address", url)
	}
	res, err := entry.server.Client().Get(mcpbridge.SteerURL(url) + "/notes")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body struct {
		Notes []mcpbridge.SteerNote `json:"notes"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&body) != nil ||
		len(body.Notes) != 1 || body.Notes[0].ID != "n-1" {
		t.Fatalf("a poll through the fleet's address answered %d %+v", res.StatusCode, body.Notes)
	}
	read, err := entry.server.Client().Post(mcpbridge.SteerURL(url)+"/read", "application/json",
		strings.NewReader(`{"ids":["n-1"]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = read.Body.Close()
	inbox.mu.Lock()
	reads := slices.Clone(inbox.reads)
	inbox.mu.Unlock()
	if read.StatusCode != http.StatusNoContent || len(reads) != 1 {
		t.Errorf("a read through the fleet's address answered %d, the holder's inbox read %v",
			read.StatusCode, reads)
	}
}

// A RELAYED POLL IS LET GO WHEN THE NODE RELAYING IT STOPS. A poll that reaches
// a node holding no opening of its job is relayed to the session's holder, and
// waits there for a note — and the node relaying it waits too. A stopping node
// relays its polls the longest (its seats are handed on, and a direct box goes
// on dialling its loopback), so its Shutdown lets go of each, answering it as a
// poll held there is answered: a 503 `draining` with a Retry-After, at once —
// never left to keep the listener busy for its whole grace and then be cut,
// which a poller reads as a network error rather than the answer to pause on.
// A poll that arrives after the shutdown is turned away the same way.
//
// Mutation: forward the poll under the request's own context, as every call
// was, and the poll waits out the holder's hold.
func TestShutdownLetsGoOfAPollItRelays(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	f := newFleet(t, 2, 10*time.Second)
	entry, holder := f.members[0], f.members[1]
	for _, m := range f.members {
		mux := m.server.Config.Handler.(*http.ServeMux)
		mux.Handle(mcpbridge.SteerPattern, m.bridge.SteerHandler())
	}
	const hold = time.Minute
	mcpbridge.HoldPollsFor(holder.bridge, hold)
	session := &mcpbridge.Session{RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer",
		Surface: fx.surface, Ledger: fx.ledger, Steer: newFakeInbox()}
	route := mcpbridge.SteerURL(f.takeOver(t, holder, session)) + "/notes"
	poll := func() polled {
		t.Helper()
		return pollWith(t.Context(), t, entry.server.Client(), route)
	}
	done := make(chan polled, 1)
	go func() { done <- poll() }()
	// THE POLL IS HELD AT THE HOLDER, relayed by the entry.
	time.Sleep(300 * time.Millisecond)
	select {
	case got := <-done:
		t.Fatalf("the relayed poll answered %v before anything happened", got.res)
	default:
	}
	stopped := time.Now()
	entry.bridge.Shutdown()
	select {
	case got := <-done:
		if got.res == nil || got.res.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("the relayed poll answered %v when its node stopped, want a 503", got.res)
		}
		wantTurnedAway(t, got, "draining")
		if took := time.Since(stopped); took > 2*time.Second {
			t.Errorf("the relayed poll was let go %s after its node stopped, want at once", took)
		}
	case <-time.After(hold / 2):
		t.Fatal("the relayed poll waited out the holder's hold after its node stopped")
	}
	after := poll()
	if after.res == nil || after.res.StatusCode != http.StatusServiceUnavailable || after.took > 2*time.Second {
		t.Fatalf("a poll after the shutdown answered %v after %s, want a 503 at once", after.res, after.took)
	}
	wantTurnedAway(t, after, "draining")
}

// THE NOTES ROUTE JUDGES A CALL'S HOST AS THE MCP ENDPOINT DOES. A seat's poll
// is answered its notes under whatever Host it arrives with (a reverse proxy
// or tunnel passing the public name on included), on the node holding the
// opening and on the holder a poll is relayed to alike: both share the routing
// that judges it. Over real listeners on 127.0.0.1, so every poll arrives on
// loopback.
//
// Mutation: judge a seat's Host on the notes route (refuse a name that is not
// loopback), and the public names are refused on both paths.
func TestTheNotesRouteJudgesAHostAsTheMCPEndpointDoes(t *testing.T) {
	t.Parallel()
	note := mcpbridge.SteerNote{Seq: 1, ID: "n-1", Inbox: "msg_1", Message: "use staging"}
	direct := newSteerFixture(t, time.Second)
	direct.inbox.file(note)

	fx := newFixture(t)
	f := newFleet(t, 2, 10*time.Second)
	entry, holder := f.members[0], f.members[1]
	for _, m := range f.members {
		mux := m.server.Config.Handler.(*http.ServeMux)
		mux.Handle(mcpbridge.SteerPattern, m.bridge.SteerHandler())
	}
	inbox := newFakeInbox()
	inbox.notes = []mcpbridge.SteerNote{note}
	relayed := f.takeOver(t, holder, &mcpbridge.Session{RunID: "run-1", Launch: "job-1",
		Handle: "dev", Role: "Engineer", Surface: fx.surface, Ledger: fx.ledger, Steer: inbox})

	for path, route := range map[string]struct {
		url    string
		client *http.Client
	}{
		"a seat's, held here": {mcpbridge.SteerURL(direct.url) + "/notes", direct.server.Client()},
		"a seat's, relayed":   {mcpbridge.SteerURL(relayed) + "/notes", entry.server.Client()},
	} {
		parsed, err := url.Parse(route.url)
		if err != nil {
			t.Fatal(err)
		}
		port := parsed.Port()
		for _, host := range []string{
			"127.0.0.1:" + port, "localhost:" + port, "host.docker.internal:" + port,
			"host.containers.internal:" + port, "crewlet.example.com", "rebound.example.com:" + port,
			"host.docker.internal.rebound.example.com:" + port,
		} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, route.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = host
			res, err := route.client.Do(req)
			if err != nil {
				t.Fatalf("%s: poll as %s: %v", path, host, err)
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Errorf("%s: a poll naming Host %s on loopback answered %d, want %d",
					path, host, res.StatusCode, http.StatusOK)
			}
		}
	}
}

// syncBuffer is a log sink the logger may write to from any goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A HOOK WHOSE AGENT THE HARNESS DID NOT FILL IN TAKES NOTHING, AND IS SAID ONCE.
// Whose loop fired such a hook cannot be told, so it is answered as a
// subagent's would be (the note waits for a boundary that can be told apart)
// and the operator is told, once per session, with the run and the seat:
// else a run whose harness never substitutes the variable takes no note
// between its steps and nothing says why. A subagent's own batch says nothing.
// NOT PARALLEL: it installs the process's log sink.
//
// Mutation: drop the unsubstituted case, or say it on every call, and this goes
// red.
func TestAnUnsubstitutedAgentIsSaidOncePerSession(t *testing.T) {
	logs := &syncBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatText, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	f := newSteerFixture(t, time.Minute)
	f.inbox.answer = `{"hookSpecificOutput":{"hookEventName":"PostToolBatch","additionalContext":"x"}}`
	sess := dial(t, f.url)
	for _, agent := range []string{"${agent_id}", "${agent_id}", "a1"} {
		res := callTool(t, sess, mcpbridge.SteerTool, map[string]any{"event": "PostToolBatch", "agent": agent})
		if res.IsError || text(res) != "" {
			t.Errorf("steer_take for agent %q answered %q (error %v), want nothing", agent, text(res), res.IsError)
		}
	}
	if taken := f.inbox.taken(); len(taken) != 0 {
		t.Errorf("the inbox was asked at %v, want never", taken)
	}
	var said []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, "mcp_bridge_steer_unsubstituted") {
			said = append(said, line)
		}
	}
	if len(said) != 1 {
		t.Fatalf("mcp_bridge_steer_unsubstituted was logged %d times, want once:\n%s", len(said), logs.String())
	}
	for _, field := range []string{"run_id=run-1", "seat=dev", "level=WARN"} {
		if !strings.Contains(said[0], field) {
			t.Errorf("the warning does not say %s: %s", field, said[0])
		}
	}
}
