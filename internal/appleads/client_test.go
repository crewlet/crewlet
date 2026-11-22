package appleads_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/appleads"
	"github.com/crewlet/crewlet/internal/es256"
)

// window is a fake API answering with a rate window, a 429 first where set.
type window struct {
	mu         sync.Mutex
	remaining  int
	throttle   int
	retryAfter string
	tokens     int
	calls      []string
}

func (w *window) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r.URL.Path == "/token" {
		w.tokens++
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"access_token": "tok-` + strconv.Itoa(w.tokens) + `", "expires_in": 3600}`))
		return
	}
	w.calls = append(w.calls, r.Method+" "+r.Header.Get("Authorization"))
	rw.Header().Set("RateLimit-Limit", "100")
	rw.Header().Set("RateLimit-Remaining", strconv.Itoa(w.remaining))
	rw.Header().Set("RateLimit-Reset", "60")
	if r.Header.Get("Authorization") == "Bearer tok-1" && w.tokens == 1 && r.URL.Path == "/v1/expired" {
		rw.WriteHeader(http.StatusUnauthorized)
		return
	}
	if w.throttle > 0 {
		w.throttle--
		rw.Header().Set("Retry-After", w.retryAfter)
		rw.WriteHeader(http.StatusTooManyRequests)
		return
	}
	_, _ = rw.Write([]byte(`{"result": {}}`))
}

func client(t *testing.T, w *window) *appleads.Client {
	t.Helper()
	srv := httptest.NewServer(w)
	t.Cleanup(srv.Close)
	pemKey, _ := sec1(t)
	key, err := es256.ParseKey(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	return appleads.NewClient(srv.URL, srv.URL+"/token", appleads.Credentials{ClientID: "SEARCHADS.c",
		TeamID: "SEARCHADS.t", KeyID: "k", Key: key}, srv.Client())
}

// A tenth of Apple's window is kept for approved acts and the pass: an
// agent's read at the reserve is refused before Apple is asked, a reserved
// read and a write go on.
//
// Mutation: check the reserve on every call, or never, and an approved act
// fails for agents' reads, or agents spend the window.
func TestAgentsReadsStopAtTheReserve(t *testing.T) {
	t.Parallel()
	w := &window{remaining: 10}
	c := client(t, w)
	ctx := context.Background()
	if _, err := c.Do(ctx, appleads.Request{Method: "GET", Path: "/v1/me", Read: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(ctx, appleads.Request{Method: "GET", Path: "/v1/me", Read: true}); !errors.Is(err, appleads.ErrReserve) {
		t.Errorf("a read at the reserve answered %v", err)
	}
	if _, err := c.Do(ctx, appleads.Request{Method: "GET", Path: "/v1/me", Read: true, Reserved: true}); err != nil {
		t.Errorf("a reserved read answered %v", err)
	}
	if _, err := c.Do(ctx, appleads.Request{Method: "PUT", Path: "/v1/keywords/1", Body: map[string]any{}}); err != nil {
		t.Errorf("a write answered %v", err)
	}
	if limit, remaining, resets := c.Remaining(); limit != 100 || remaining != 10 || resets.IsZero() {
		t.Errorf("the window reads %d, %d, %v", limit, remaining, resets)
	}
}

// A read waits out a short 429 and is retried; a write never is, and a wait
// past thirty seconds is Apple's quota, answered at once.
//
// Mutation: retry writes, or wait out any Retry-After, and a budget raise is
// sent twice, or a read sleeps through a quota window.
func TestOnlyReadsRetryAfterAShort429(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reads := &window{remaining: 90, throttle: 1, retryAfter: "0"}
	if _, err := client(t, reads).Do(ctx, appleads.Request{Method: "POST", Path: "/v1/campaigns/query",
		Body: map[string]any{}, Read: true}); err != nil || len(reads.calls) != 2 {
		t.Errorf("a throttled read answered %v after %d calls", err, len(reads.calls))
	}
	writes := &window{remaining: 90, throttle: 1, retryAfter: "0"}
	if _, err := client(t, writes).Do(ctx, appleads.Request{Method: "PUT", Path: "/v1/campaigns/1",
		Body: map[string]any{}}); err == nil || len(writes.calls) != 1 {
		t.Errorf("a throttled write answered %v after %d calls", err, len(writes.calls))
	}
	quota := &window{remaining: 90, throttle: 1, retryAfter: "120"}
	_, err := client(t, quota).Do(ctx, appleads.Request{Method: "GET", Path: "/v1/me", Read: true})
	if !errors.Is(err, appleads.ErrQuota) || len(quota.calls) != 1 {
		t.Errorf("a long Retry-After answered %v after %d calls", err, len(quota.calls))
	}
}

// A cached token Apple no longer takes is replaced once and the call made
// again; the token is exchanged once while it lives.
//
// Mutation: drop the replacement, and every call fails for the token's last
// minutes; mint per call, and the token endpoint is hammered.
func TestARefusedCachedTokenIsReplacedOnce(t *testing.T) {
	t.Parallel()
	w := &window{remaining: 90}
	c := client(t, w)
	ctx := context.Background()
	if _, err := c.Do(ctx, appleads.Request{Method: "GET", Path: "/v1/me", Read: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Do(ctx, appleads.Request{Method: "GET", Path: "/v1/expired", Read: true}); err != nil || w.tokens != 2 {
		t.Errorf("a refused cached token answered %v after %d tokens", err, w.tokens)
	}
	if _, err := c.Do(ctx, appleads.Request{Method: "GET", Path: "/v1/me", Read: true}); err != nil || w.tokens != 2 {
		t.Errorf("a live token was exchanged again: %d tokens (%v)", w.tokens, err)
	}
}
