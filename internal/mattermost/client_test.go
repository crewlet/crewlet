package mattermost_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/mattermost"
)

// server is a Mattermost stand-in that records what it was asked.
type server struct {
	*httptest.Server
	mu    sync.Mutex
	calls []string
	// respond is consulted per call; nil serves 200 with an empty object.
	// Set it through [server.responds], never by assignment: the handler
	// reads it on the HTTP server's own goroutines, so a bare write from
	// the test races whatever is still in flight — and the transport's
	// broadcast and the fleet reconcile both put several requests on this
	// stand-in at once.
	respond func(w http.ResponseWriter, r *http.Request) bool
}

// responds installs what the stand-in answers with next.
func (s *server) responds(fn func(w http.ResponseWriter, r *http.Request) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = fn
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, r.Method+" "+r.URL.RequestURI())
		respond := s.respond
		s.mu.Unlock()
		if respond != nil && respond(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func client(t *testing.T, s *server) *mattermost.Client {
	t.Helper()
	c, err := mattermost.NewClient(mattermost.ClientOptions{URL: s.URL, Token: "tok"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestAClientNeedsARealInstanceURL(t *testing.T) {
	for _, raw := range []string{"", "   ", "not a url", "/just/a/path"} {
		if _, err := mattermost.NewClient(mattermost.ClientOptions{URL: raw}); err == nil {
			t.Errorf("NewClient accepted %q", raw)
		}
	}
	c, err := mattermost.NewClient(mattermost.ClientOptions{URL: "https://chat.example.com/"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.URL() != "https://chat.example.com" {
		t.Fatalf("URL = %q", c.URL())
	}
	if c.WebsocketURL() != "wss://chat.example.com/api/v4/websocket" {
		t.Fatalf("WebsocketURL = %q", c.WebsocketURL())
	}
}

func TestTheSessionRidesOnEveryCall(t *testing.T) {
	s := newServer(t)
	var auth atomic.Value
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		auth.Store(r.Header.Get("Authorization"))
		return false
	})

	if _, err := client(t, s).Me(t.Context()); err != nil {
		t.Fatalf("Me: %v", err)
	}
	if got, _ := auth.Load().(string); got != "Bearer tok" {
		t.Fatalf("Authorization = %q", got)
	}
}

// The server's own error text is written for a person: surfacing it turns
// "500 on /users/me" into "Invalid session".
func TestAFailureCarriesTheServersOwnWords(t *testing.T) {
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"id":"api.context.session_expired","message":"Invalid or expired session"}`))
		return true
	})

	_, err := client(t, s).Me(t.Context())
	if err == nil {
		t.Fatal("a 401 was not reported")
	}
	if mattermost.Status(err) != http.StatusUnauthorized {
		t.Fatalf("Status = %d", mattermost.Status(err))
	}
	for _, want := range []string{"/users/me", "401", "Invalid or expired session"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
	}
	// Status is 0 for anything that was not an API failure, so a caller
	// branching on it cannot mistake a transport error for a 200.
	if got := mattermost.Status(context.Canceled); got != 0 {
		t.Fatalf("Status of a non-API error = %d", got)
	}
}

// A 401 is the caller's problem and will be a 401 again: retrying burns the
// budget and tells an operator nothing new.
func TestAnAuthFailureIsNotRetried(t *testing.T) {
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusUnauthorized)
		return true
	})

	if _, err := client(t, s).Me(t.Context()); err == nil {
		t.Fatal("a 401 was not reported")
	}
	if got := len(s.seen()); got != 1 {
		t.Fatalf("a 401 was attempted %d times", got)
	}
}

// A 503 is a server mid-restart, which is the ordinary state of a container
// that is up but still migrating.
func TestARestartingServerIsWaitedOut(t *testing.T) {
	s := newServer(t)
	var n atomic.Int32
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		if n.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		w.Write([]byte(`{"id":"u1","username":"agent-swe"}`))
		return true
	})

	me, err := client(t, s).Me(t.Context())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if me.Username != "agent-swe" {
		t.Fatalf("Me = %+v", me)
	}
	if got := len(s.seen()); got != 3 {
		t.Fatalf("the call was attempted %d times, want 3", got)
	}
}

// THE RULE THAT MATTERS: a POST is repeated only when the caller has
// established that repeating it cannot repeat an effect. A 502 leaves it
// unknowable whether the request was applied and only the answer lost —
// which is the case that mints a SECOND access token. Its value is served
// once and never again, so the duplicate is a live credential with no record
// anywhere that it exists.
func TestAPostIsNotRepeatedOnAnUnknowableFailure(t *testing.T) {
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusBadGateway)
		return true
	})

	_, err := client(t, s).CreateAccessToken(t.Context(), "u1", "crewlet")
	if err == nil {
		t.Fatal("a 502 was not reported")
	}
	if got := len(s.seen()); got != 1 {
		t.Fatalf("the mint was sent %d times after a 502", got)
	}
}

// A rate-limited request never reached the handler, so repeating it cannot
// repeat a side effect — which makes it safe even for a write that must not
// happen twice.
func TestARateLimitIsRepeatedForAnyMethod(t *testing.T) {
	s := newServer(t)
	var n atomic.Int32
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		w.Write([]byte(`{"id":"t1","token":"tok-1"}`))
		return true
	})

	got, err := client(t, s).CreateAccessToken(t.Context(), "u1", "crewlet")
	if err != nil {
		t.Fatalf("CreateAccessToken: %v", err)
	}
	if got.ID != "t1" || got.Value != "tok-1" {
		t.Fatalf("CreateAccessToken = %+v", got)
	}
	if n.Load() != 2 {
		t.Fatalf("the mint was attempted %d times", n.Load())
	}
}

// An idempotent method is repeated on any retryable failure: repeating one
// cannot create a second anything.
func TestAnIdempotentMethodIsRepeatedOnA502(t *testing.T) {
	s := newServer(t)
	var n atomic.Int32
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		w.Write([]byte(`{"id":"u1"}`))
		return true
	})

	if _, err := client(t, s).Me(t.Context()); err != nil {
		t.Fatalf("Me: %v", err)
	}
	if n.Load() != 2 {
		t.Fatalf("a GET was attempted %d times", n.Load())
	}
}

// A caller that has established repeatability gets it: opening a direct
// channel returns the existing one rather than making a second, and the
// alternative is a seat that cannot reach a person because one answer went
// missing.
func TestARepeatableCallIsRepeated(t *testing.T) {
	s := newServer(t)
	var n atomic.Int32
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		w.Write([]byte(`{"id":"D1","type":"D"}`))
		return true
	})

	ch, err := client(t, s).DirectChannel(t.Context(), "u1", "u2")
	if err != nil {
		t.Fatalf("DirectChannel: %v", err)
	}
	if ch.ID != "D1" || !ch.Direct() {
		t.Fatalf("DirectChannel = %+v", ch)
	}
	if n.Load() != 2 {
		t.Fatalf("a repeatable POST was attempted %d times", n.Load())
	}
}

// A cancelled context ends the call rather than being waited out — the
// retry budget is for a slow server, not for a caller that has gone away.
func TestACancelledCallStopsRetrying(t *testing.T) {
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := client(t, s).Me(ctx)
	if err == nil {
		t.Fatal("a cancelled call succeeded")
	}
	if got := len(s.seen()); got > 1 {
		t.Fatalf("a cancelled call was attempted %d times", got)
	}
	// And it reports WHICH call was cancelled. Falling into the retry
	// path and returning the context's own error instead loses the method
	// and path — leaving a caller debugging a cancelled turn with a bare
	// "context canceled" and no idea which of a dozen calls it was.
	if !strings.Contains(err.Error(), "/users/me") {
		t.Fatalf("a cancelled call reported %v, naming no endpoint", err)
	}
}

// A conversation must arrive in the order it happened: Mattermost's own
// ordering is NEWEST FIRST, and a backfill replayed that way hands a seat
// the answer before the question.
func TestPostListsComeBackOldestFirst(t *testing.T) {
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		json.NewEncoder(w).Encode(map[string]any{
			"order": []string{"p3", "p2", "p1"},
			"posts": map[string]any{
				"p1": map[string]any{"id": "p1", "message": "first"},
				"p2": map[string]any{"id": "p2", "message": "second"},
				"p3": map[string]any{"id": "p3", "message": "third"},
			},
		})
		return true
	})

	got, err := client(t, s).PostsSince(t.Context(), "C1", time.UnixMilli(1718003000))
	if err != nil {
		t.Fatalf("PostsSince: %v", err)
	}
	var ids []string
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	if strings.Join(ids, ",") != "p1,p2,p3" {
		t.Fatalf("posts arrived as %v", ids)
	}
	// The cursor is a MILLISECOND stamp: seconds would re-read a whole
	// second of conversation on every reconnect.
	if !strings.Contains(s.seen()[0], "since=1718003000") {
		t.Fatalf("the backfill asked for %q", s.seen()[0])
	}
	// An entry in the ordering with no post is skipped rather than
	// producing a blank message — a deleted post leaves its id in `order`,
	// and a zero-valued Post replayed from it is an empty message delivered
	// to a seat as though somebody had sent it.
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		json.NewEncoder(w).Encode(map[string]any{
			"order": []string{"p9", "p1"},
			"posts": map[string]any{"p1": map[string]any{"id": "p1"}},
		})
		return true
	})
	got, err = client(t, s).PostsSince(t.Context(), "C1", time.UnixMilli(1718003000))
	if err != nil {
		t.Fatalf("PostsSince: %v", err)
	}
	if len(got) != 1 || got[0].ID != "p1" {
		t.Fatalf("a dangling ordering entry produced %+v", got)
	}
}

// The reconnect window compares SERVER-stamped timestamps, so "now" cannot
// come from the engine's clock: skewed early it re-reads messages the seat
// already answered, skewed late it silently loses the ones it never saw.
func TestServerTimeComesFromTheServer(t *testing.T) {
	s := newServer(t)
	when := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Date", when.Format(http.TimeFormat))
		w.Write([]byte(`{"id":"u1"}`))
		return true
	})

	got, err := client(t, s).ServerTime(t.Context())
	if err != nil {
		t.Fatalf("ServerTime: %v", err)
	}
	if !got.Equal(when) {
		t.Fatalf("ServerTime = %s, want %s", got, when)
	}

	// A server that sends no usable Date is reported rather than silently
	// falling back to this process's clock, which is the thing this
	// exists to avoid.
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Date", "not a date")
		w.Write([]byte(`{}`))
		return true
	})
	if _, err := client(t, s).ServerTime(t.Context()); err == nil {
		t.Fatal("an unusable Date was accepted")
	}
}

// The server ENFORCES the typing throttle: sending faster is rejected, not
// merely wasteful, so the live value is read rather than assumed.
func TestTheTypingThrottleComesFromTheServer(t *testing.T) {
	if got := mattermost.TypingThrottle(map[string]string{
		"TimeBetweenUserTypingUpdatesMilliseconds": "2000",
	}); got != 2*time.Second {
		t.Fatalf("TypingThrottle = %v", got)
	}
	// An absent, unparseable or nonsensical value takes the server's own
	// default rather than zero, which would be a hot loop against a rate
	// limiter.
	for _, raw := range []string{"", "lots", "0", "-1"} {
		got := mattermost.TypingThrottle(map[string]string{
			"TimeBetweenUserTypingUpdatesMilliseconds": raw,
		})
		if got != mattermost.DefaultTypingThrottle {
			t.Errorf("TypingThrottle(%q) = %v", raw, got)
		}
	}
	if got := mattermost.SiteURL(map[string]string{"SiteURL": "https://chat.example.com/"}); got != "https://chat.example.com" {
		t.Fatalf("SiteURL = %q", got)
	}
}

func TestTheEndpointsAskForWhatTheyName(t *testing.T) {
	s := newServer(t)
	c := client(t, s)
	ctx := t.Context()

	c.Me(ctx)
	c.UserByUsername(ctx, "@agent-swe")
	c.Channels(ctx, "u1", "t1")
	c.Teams(ctx, "")
	c.ChannelByName(ctx, "t1", "eng")
	c.Typing(ctx, "u1", "C1", "root-1")
	c.ClientConfig(ctx)
	c.Thread(ctx, "root-1")

	want := []string{
		"GET /api/v4/users/me",
		"GET /api/v4/users/username/agent-swe",
		"GET /api/v4/users/u1/teams/t1/channels",
		"GET /api/v4/users/me/teams",
		"GET /api/v4/teams/t1/channels/name/eng",
		"POST /api/v4/users/u1/typing",
		"GET /api/v4/config/client?format=old",
		"GET /api/v4/posts/root-1/thread?skipFetchThreads=true",
	}
	got := s.seen()
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Errorf("call %d was %q, want %q", i, got[i], w)
		}
	}
}

// An incomplete call is refused before it reaches the network, where it
// would become a confusing 404 rather than a caller's mistake.
func TestIncompleteCallsAreRefusedLocally(t *testing.T) {
	s := newServer(t)
	c := client(t, s)
	ctx := t.Context()

	for name, err := range map[string]error{
		"no username": errOf(func() error { _, e := c.UserByUsername(ctx, " @ "); return e }),
		"no channel to backfill": errOf(func() error {
			_, e := c.PostsSince(ctx, "", time.Now())
			return e
		}),
		"no team": errOf(func() error { _, e := c.Channels(ctx, "u1", ""); return e }),
		"no thread root": errOf(func() error {
			_, e := c.Thread(ctx, "")
			return e
		}),
		"one dm party":  errOf(func() error { _, e := c.DirectChannel(ctx, "u1", ""); return e }),
		"typing nobody": c.Typing(ctx, "", "C1", ""),
	} {
		if err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if got := len(s.seen()); got != 0 {
		t.Fatalf("%d incomplete calls reached the network", got)
	}
}

func errOf(f func() error) error { return f() }

// A THREAD ARRIVES IN THE ORDER IT WAS WRITTEN, and the ordering is derived
// from the posts' own timestamps rather than from the server's `order`.
//
// `order` is newest-first for the CHANNEL endpoint, which is a claim tested
// for [Client.PostsSince] and for nothing else. /posts/{id}/thread is a
// different endpoint, and reusing that reversal on it would hand a seat the
// answer before the question — the exact failure the reversal exists to
// prevent. A create_at sort is right whatever either endpoint answers.
func TestAThreadIsOrderedByWhenItWasWritten(t *testing.T) {
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		// `order` deliberately disagrees with the timestamps, in BOTH
		// directions at once: a reader that trusted it either way lands
		// on a different sequence from this one.
		json.NewEncoder(w).Encode(map[string]any{
			"order": []string{"p2", "root", "p1"},
			"posts": map[string]any{
				"root": map[string]any{"id": "root", "message": "the question", "create_at": 1000},
				"p1":   map[string]any{"id": "p1", "message": "a first reply", "create_at": 2000},
				"p2":   map[string]any{"id": "p2", "message": "the answer", "create_at": 3000},
			},
		})
		return true
	})

	got, err := client(t, s).Thread(t.Context(), "root")
	if err != nil {
		t.Fatalf("Thread: %v", err)
	}
	var ids []string
	for _, p := range got {
		ids = append(ids, p.ID)
	}
	if strings.Join(ids, ",") != "root,p1,p2" {
		t.Fatalf("the thread arrived as %v, want the root first and the answer last", ids)
	}
}

// TWO POSTS IN THE SAME MILLISECOND still order the same way on every node.
//
// Without the tie-break the sequence is Go's map iteration order, so two
// nodes rendering one thread into one seat's prompt would disagree — and a
// system prompt that is not byte-stable costs the provider's prefix cache the
// whole prompt on every round.
func TestPostsWrittenAtOnceOrderStably(t *testing.T) {
	s := newServer(t)
	s.responds(func(w http.ResponseWriter, r *http.Request) bool {
		json.NewEncoder(w).Encode(map[string]any{
			"order": []string{"b", "a", "c"},
			"posts": map[string]any{
				"a": map[string]any{"id": "a", "message": "one", "create_at": 1000},
				"b": map[string]any{"id": "b", "message": "two", "create_at": 1000},
				"c": map[string]any{"id": "c", "message": "three", "create_at": 1000},
			},
		})
		return true
	})
	c := client(t, s)
	for range 8 {
		got, err := c.Thread(t.Context(), "a")
		if err != nil {
			t.Fatalf("Thread: %v", err)
		}
		var ids []string
		for _, p := range got {
			ids = append(ids, p.ID)
		}
		if strings.Join(ids, ",") != "a,b,c" {
			t.Fatalf("the thread arrived as %v", ids)
		}
	}
}

// BOOKKEEPING AND CONTENT ARE ONE RULE ACROSS BOTH DECODED SHAPES.
//
// The websocket delivers a post as an untyped map and the thread read decodes
// it into a [mattermost.Post]. A deleted post that wakes nobody but still
// renders into a seat's thread block is the kind of divergence nothing would
// report, because the block degrades silently by design.
func TestOnePostRulesBothWays(t *testing.T) {
	for name, tc := range map[string]struct {
		post mattermost.Post
		raw  map[string]any
		skip bool
		body string
	}{
		"an ordinary post": {
			post: mattermost.Post{ID: "p1", Message: "hello"},
			raw:  map[string]any{"id": "p1", "message": "hello"},
			body: "hello",
		},
		"a system join line": {
			post: mattermost.Post{ID: "p2", Type: "system_join_channel", Message: "x joined"},
			raw:  map[string]any{"id": "p2", "type": "system_join_channel", "message": "x joined"},
			skip: true, body: "x joined",
		},
		"a deleted post": {
			post: mattermost.Post{ID: "p3", Message: "oops", DeleteAt: 1700},
			raw:  map[string]any{"id": "p3", "message": "oops", "delete_at": float64(1700)},
			skip: true, body: "oops",
		},
		"a file with no comment": {
			post: mattermost.Post{ID: "p4", FileIDs: []string{"f1", "f2"}},
			raw:  map[string]any{"id": "p4", "file_ids": []any{"f1", "f2"}},
			body: "(shared 2 files)",
		},
		"nothing at all": {
			post: mattermost.Post{ID: "p5"},
			raw:  map[string]any{"id": "p5"},
			body: "",
		},
	} {
		if got := tc.post.Bookkeeping() != ""; got != tc.skip {
			t.Errorf("%s: Post.Bookkeeping says skip=%v", name, got)
		}
		if got := mattermost.SkipReason(tc.raw) != ""; got != tc.skip {
			t.Errorf("%s: SkipReason says skip=%v", name, got)
		}
		if got := tc.post.Body(); got != tc.body {
			t.Errorf("%s: Post.Body = %q, want %q", name, got, tc.body)
		}
		if got := mattermost.Text(tc.raw); got != tc.body {
			t.Errorf("%s: Text = %q, want %q", name, got, tc.body)
		}
	}
}
