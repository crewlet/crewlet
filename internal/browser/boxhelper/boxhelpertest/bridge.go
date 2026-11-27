package boxhelpertest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
)

// bridgeWithin bounds one wait on what a relay sends its bridge: a relay
// posts as the supervisor tells it and polls at once, so a minute is a hang,
// never a slow machine.
const bridgeWithin = time.Minute

// Request is one request a relay made of a [FakeBridge], as it came: its
// verb, method and query, the frame metadata it carried, its body, and the
// order it came in.
type Request struct {
	N      int
	Verb   string
	Method string
	Query  string
	Token  string
	Frame  string
	Type   string
	Body   []byte
}

// refusal is how a [FakeBridge] answers a verb a case refuses.
type refusal struct {
	status     int
	code       string
	retryAfter int
}

// spoiling is how a [FakeBridge] spoils the answers to a verb a case names.
type spoiling string

const (
	spoilLong spoiling = "long"
	spoilCut  spoiling = "cut"
)

// FakeBridge plays a run's bridge for the box helper's own suite: the browser
// routes under one token, recording every request, holding each poll until
// the case answers it ([FakeBridge.Watch]) or the request ends, never on a
// timer, and answering as a case says ([FakeBridge.Refuse],
// [FakeBridge.Hold], [FakeBridge.Overlong], [FakeBridge.BreakOff]). It is no
// hub: the engine's suite proves the real one.
type FakeBridge struct {
	t     testing.TB
	srv   *httptest.Server
	token string

	mu       sync.Mutex
	requests []Request
	arrived  chan struct{}
	hub      string
	level    int
	watch    browser.Watch
	changed  chan struct{}
	refuse   map[string]refusal
	spoil    map[string]spoiling
	held     map[string]chan struct{}
	polls    int
	maxPolls int
}

// NewFakeBridge starts a fake bridge for t's life.
func NewFakeBridge(t testing.TB) *FakeBridge {
	t.Helper()
	b := &FakeBridge{t: t, token: "crewlet_rt1." + randomHex(t, 24), hub: randomHex(t, 16),
		arrived: make(chan struct{}, 1), changed: make(chan struct{}), refuse: map[string]refusal{},
		spoil: map[string]spoiling{}, held: map[string]chan struct{}{}, watch: browser.Watch{FPS: browser.WatchFPS}}
	b.srv = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(func() {
		b.mu.Lock()
		for _, ch := range b.held {
			close(ch)
		}
		b.held = map[string]chan struct{}{}
		b.mu.Unlock()
		b.srv.CloseClientConnections()
		b.srv.Close()
	})
	return b
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// URL is the route a wiring names: the bridge's browser routes under its
// token.
func (b *FakeBridge) URL() string { return b.srv.URL + "/mcp/" + b.token + "/browser" }

// Token is the token its route holds.
func (b *FakeBridge) Token() string { return b.token }

// Hub is the hub id the bridge answers polls under.
func (b *FakeBridge) Hub() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hub
}

func (b *FakeBridge) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, "/mcp/")
	token, verb, found := strings.Cut(rest, "/browser/")
	if !ok || !found || strings.Contains(verb, "/") {
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, int64(browser.MaxFrameBytes)+1))
	b.mu.Lock()
	req := Request{N: len(b.requests) + 1, Verb: verb, Method: r.Method, Query: r.URL.RawQuery, Token: token,
		Frame: r.Header.Get(browser.FrameHeader), Type: r.Header.Get("Content-Type"), Body: body}
	b.requests = append(b.requests, req)
	refused, isRefused := b.refuse[verb]
	spoiled := b.spoil[verb]
	held := b.held[verb]
	b.mu.Unlock()
	select {
	case b.arrived <- struct{}{}:
	default:
	}
	if held != nil {
		select {
		case <-held:
		case <-r.Context().Done():
			return
		}
	}
	if spoiled != "" {
		spoil(w, spoiled)
		return
	}
	if isRefused {
		if refused.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(refused.retryAfter))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(refused.status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": refused.code})
		return
	}
	if verb != "poll" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	b.poll(w, r)
}

// spoil answers 200 with a body past what a relay reads, or with one cut off
// partway: its length declared, a part of it sent, and the connection closed.
func spoil(w http.ResponseWriter, how spoiling) {
	w.Header().Set("Content-Type", "application/json")
	if how == spoilLong {
		_, _ = w.Write(bytes.Repeat([]byte(" "), browser.MaxPollAnswerBytes+1))
		return
	}
	w.Header().Set("Content-Length", "64")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"v":1,`))
	_ = http.NewResponseController(w).Flush()
	panic(http.ErrAbortHandler)
}

// poll answers a poll as a hub does: at once where it names another hub or
// a lower level, and otherwise once the watch changes or the request ends.
func (b *FakeBridge) poll(w http.ResponseWriter, r *http.Request) {
	q, err := browser.ParsePollQuery(r.URL.Query())
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_query"})
		return
	}
	b.mu.Lock()
	b.polls++
	b.maxPolls = max(b.maxPolls, b.polls)
	defer func() {
		b.mu.Lock()
		b.polls--
		b.mu.Unlock()
	}()
	for q.Hub == b.hub && q.Level >= b.level {
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
		b.mu.Lock()
	}
	answer := browser.PollAnswer{V: 1, Hub: b.hub, Level: b.level, Watch: b.watch}
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

// Watch answers the held poll, and every later one, with a watch of on at
// fps, at a level above the last.
func (b *FakeBridge) Watch(on bool, fps int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.watch = browser.Watch{On: on, FPS: fps}
	b.level++
	close(b.changed)
	b.changed = make(chan struct{})
}

// NewHub answers polls under a new hub id from now, as a hub that replaced
// the last does: the held poll is answered at once.
func (b *FakeBridge) NewHub() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.hub = randomHex(b.t, 16)
	b.level = 0
	close(b.changed)
	b.changed = make(chan struct{})
}

// Refuse answers every request of verb with status, code its body's error,
// and retryAfter seconds where above zero, until [FakeBridge.Accept].
func (b *FakeBridge) Refuse(verb string, status int, code string, retryAfter int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refuse[verb] = refusal{status: status, code: code, retryAfter: retryAfter}
}

// Overlong answers every request of verb with a 200 whose body runs past
// what a relay reads, MaxPollAnswerBytes, until [FakeBridge.Accept].
func (b *FakeBridge) Overlong(verb string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spoil[verb] = spoilLong
}

// BreakOff answers every request of verb with a 200 cut off partway, its
// connection closed, until [FakeBridge.Accept].
func (b *FakeBridge) BreakOff(verb string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spoil[verb] = spoilCut
}

// Accept answers verb as a bridge that takes it again.
func (b *FakeBridge) Accept(verb string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.refuse, verb)
	delete(b.spoil, verb)
}

// Hold holds every request of verb from now until the function it answers is
// called or the request ends, never on a timer.
func (b *FakeBridge) Hold(verb string) (release func()) {
	ch := make(chan struct{})
	b.mu.Lock()
	b.held[verb] = ch
	b.mu.Unlock()
	return sync.OnceFunc(func() {
		b.mu.Lock()
		if b.held[verb] == ch {
			delete(b.held, verb)
		}
		b.mu.Unlock()
		close(ch)
	})
}

// Requests is every request so far, in the order they came.
func (b *FakeBridge) Requests() []Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Request(nil), b.requests...)
}

// MaxPolls is the most polls the bridge held at once.
func (b *FakeBridge) MaxPolls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxPolls
}

// Await is the first request after the after-th that match holds, waiting
// for it within bridgeWithin: the case fails on none.
func (b *FakeBridge) Await(what string, after int, match func(Request) bool) Request {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(b.t.Context(), bridgeWithin)
	defer cancel()
	for {
		seen := b.Requests()
		for _, req := range seen[min(after, len(seen)):] {
			if match(req) {
				return req
			}
		}
		select {
		case <-b.arrived:
		case <-ctx.Done():
			b.t.Fatalf("the bridge saw no %s within %s", what, bridgeWithin)
		}
	}
}

// Verb matches a request of verb.
func Verb(verb string) func(Request) bool {
	return func(r Request) bool { return r.Verb == verb }
}
