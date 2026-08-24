package webhooks_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/webhooks"
)

// The paths that only open when something else is already broken. Each is a
// decision about what a partial failure costs, and none of them is reachable
// from a well-behaved request — which is exactly why they need reaching.

// brokenBody fails part way through, the way a client that hung up mid-upload
// or a socket that broke does.
type brokenBody struct{ read int }

func (b *brokenBody) Read(p []byte) (int, error) {
	if b.read > 0 {
		return 0, errors.New("connection reset")
	}
	b.read++
	p[0] = '{'
	return 1, nil
}

func (b *brokenBody) Close() error { return nil }

func TestABodyThatStopsMidWayIsNotHalfAccepted(t *testing.T) {
	t.Parallel()
	// There is nothing to verify and nobody left to tell — but the status
	// still has to be written, or the handler falls off the end and Go
	// answers 200 to a delivery it never read.
	e := newEdge(t)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", &brokenBody{})
	req.Header.Set("X-GitHub-Event", "issues")
	res := httptest.NewRecorder()
	e.mux.ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", res.Code)
	}
	if e.published.count() != 0 || len(e.rows(t)) != 0 {
		t.Error("a truncated body reached the queue or the store")
	}
}

func TestAFailedStoreWriteDoesNotFailTheDelivery(t *testing.T) {
	t.Parallel()
	// The store row is observability and the republish is the wake. A
	// receiver that answered 503 because it could not write a feed row
	// would make the provider retry work that has ALREADY been queued —
	// so the seat does it twice, and the dashboard is the reason.
	e := newEdge(t)
	e.closeStore(t)

	res := e.post(t, "/webhooks/github", issueBody, githubDelivery(issueBody, "gh-secret"))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", res.Code)
	}
	if e.published.count() != 1 {
		t.Error("the delivery did not reach the queue")
	}
	// And it still reached the dashboard live, which is the surface that
	// does not depend on the store at all.
	if e.stream.count() != 1 {
		t.Error("the live push was skipped along with the row")
	}
}

func TestAReceiverWiredToNothingRefusesRatherThanPanics(t *testing.T) {
	t.Parallel()
	// A zero Options is what an embedder writes first. Every field it
	// leaves out has a defined absence — no secrets is "cannot verify",
	// no clock is the wall clock, no configured flag is "serving" — and
	// none of them may be a nil dereference on the request path.
	mux := http.NewServeMux()
	webhooks.New(webhooks.Options{}).Routes(mux)

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github",
		strings.NewReader(`{"action":"opened"}`))
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 — a receiver with no secrets can verify nothing", res.Code)
	}
}

func TestTheOAuthLandingIsStillReachableWithNothingWired(t *testing.T) {
	t.Parallel()
	// It holds no secret and touches no dependency, so it is the one route
	// that must work on a receiver wired to nothing: an operator reaches it
	// mid-install, before anything else is configured.
	mux := http.NewServeMux()
	webhooks.New(webhooks.Options{}).Routes(mux)
	req := httptest.NewRequest(http.MethodGet, "/webhooks/slack-oauth?code=abc", nil)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("got %d", res.Code)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "abc") {
		t.Error("the page did not show the code")
	}
}

func TestAMalformedJWKEntryIsSkippedNotTrusted(t *testing.T) {
	t.Parallel()
	// One unusable entry must not discard the rest — a key set carries the
	// outgoing key alongside the incoming one through a rotation — and an
	// entry whose exponent will not fit an int must be skipped rather than
	// WRAPPED into a different key, which produces a public key that is
	// wrong without being detectably so.
	cases := []struct {
		name string
		doc  string
		want bool
	}{
		{"a good entry beside a broken one",
			`{"keys":[{"kid":"bad","kty":"RSA","n":"!!!","e":"AQAB"},` +
				`{"kid":"k0","kty":"RSA","n":"` + modulusOf(t) + `","e":"AQAB"}]}`, true},
		{"an exponent too large for an int",
			`{"keys":[{"kid":"k0","kty":"RSA","n":"` + modulusOf(t) +
				`","e":"AQABAQABAQABAQAB"}]}`, false},
		{"an exponent of zero",
			`{"keys":[{"kid":"k0","kty":"RSA","n":"` + modulusOf(t) + `","e":"AA"}]}`, false},
		{"an empty modulus",
			`{"keys":[{"kid":"k0","kty":"RSA","n":"","e":"AQAB"}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.doc))
			}))
			t.Cleanup(server.Close)
			_, err := webhooks.NewJWKS(server.URL, nil, nil).Key(t.Context(), "k0")
			if tc.want && err != nil {
				t.Errorf("the usable key was discarded with the broken one: %v", err)
			}
			if !tc.want && err == nil {
				t.Error("a key that cannot be rebuilt correctly was accepted")
			}
		})
	}
}
