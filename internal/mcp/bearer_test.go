package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// scriptedBearer is a grant whose token the test sets: it renews to next on a
// refusal, counting each renewal, and answers err from Token where set. A
// token it renewed to is not renewed again on a refusal, as the engine's
// grant does not renew one it has just been issued ([BearerSource.Rejected]).
type scriptedBearer struct {
	mu       sync.Mutex
	token    string
	next     []string
	renewals int
	renewed  bool
	err      error
}

func (b *scriptedBearer) Token(context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.token, b.err
}

func (b *scriptedBearer) Rejected(_ context.Context, refused string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if refused != b.token || b.renewed {
		return b.token, nil
	}
	if len(b.next) == 0 {
		return "", fmt.Errorf("%w: the vendor refused the renewal", ErrSignInNeeded)
	}
	b.renewals++
	b.token, b.next, b.renewed = b.next[0], b.next[1:], true
	return b.token, nil
}

// acceptingOnly is a remote MCP server that serves a request carrying the
// bearer token it accepts and challenges every other, recording what each
// request carried.
func acceptingOnly(t *testing.T, accept *atomic.Pointer[string]) (*httptest.Server, *[]string, *sync.Mutex) {
	t.Helper()
	h := &httpMCPServer{tools: `[{"name":"search","inputSchema":{"type":"object"}}]`}
	var mu sync.Mutex
	var carried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		carried = append(carried, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+*accept.Load() {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_token","echo":"`+r.Header.Get("Authorization")+`"}`)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &carried, &mu
}

// A GRANT'S TOKEN IS SENT AT EACH REQUEST, and a refusal is ONE renewal and ONE
// retry: a token the vendor revoked before its stated life ran out costs the
// call nothing, and a token renewal that the server still refuses is the
// server's answer, never a loop.
func TestARefusedTokenIsRenewedOnceAndTheRequestRetriedOnce(t *testing.T) {
	t.Parallel()
	var accept atomic.Pointer[string]
	fresh := "at-fresh-0123456789"
	accept.Store(&fresh)
	srv, carried, mu := acceptingOnly(t, &accept)

	bearer := &scriptedBearer{token: "at-stale-0123456789", next: []string{fresh}}
	spec := httpSpec("vendor", srv.URL)
	spec.Bearer = bearer
	c, err := connect(t.Context(), spec, discardLogger())
	if err != nil {
		t.Fatalf("a server whose token was renewed on its refusal did not connect: %v", err)
	}
	_, _ = c.close(context.Background())
	if bearer.renewals != 1 {
		t.Fatalf("the grant was renewed %d times, want once", bearer.renewals)
	}
	mu.Lock()
	first := (*carried)[0]
	rest := append([]string(nil), (*carried)[1:]...)
	mu.Unlock()
	if first != "Bearer at-stale-0123456789" {
		t.Fatalf("the first request carried %q, want the stale token", first)
	}
	for _, got := range rest {
		if got != "Bearer "+fresh {
			t.Fatalf("a request after the renewal carried %q, want the renewed token", got)
		}
	}

	// A RENEWED TOKEN THE SERVER STILL REFUSES is one renewal and the
	// refusal, classed as a credential the server rejected.
	never := "at-nobody-holds-this"
	accept.Store(&never)
	again := &scriptedBearer{token: "at-one-0123456789", next: []string{"at-two-0123456789",
		"at-three-0123456789"}}
	spec.Bearer = again
	c, err = connect(t.Context(), spec, discardLogger())
	if err == nil {
		_, _ = c.close(context.Background())
		t.Fatal("a server refusing every token connected")
	}
	if again.renewals != 1 {
		t.Fatalf("a refusal of the renewed token renewed again: %d renewals", again.renewals)
	}
	if ClassOf(err) != FailCredentialRejected {
		t.Fatalf("class = %q, want credential_rejected", ClassOf(err))
	}
}

// A GRANT NOBODY HOLDS SENDS NOTHING: the server is classed as needing a
// sign-in and is never sent a request without the token it wants.
func TestAGrantWithNoTokenSendsNothingAndNeedsASignIn(t *testing.T) {
	t.Parallel()
	var accept atomic.Pointer[string]
	held := "at-held"
	accept.Store(&held)
	srv, carried, mu := acceptingOnly(t, &accept)
	spec := httpSpec("vendor", srv.URL)
	spec.Bearer = &scriptedBearer{err: fmt.Errorf("%w: nobody has signed in", ErrSignInNeeded)}
	c, err := connect(t.Context(), spec, discardLogger())
	if err == nil {
		_, _ = c.close(context.Background())
		t.Fatal("a server with no grant connected")
	}
	if ClassOf(err) != FailNeedsSignIn {
		t.Fatalf("class = %q (%v), want needs_sign_in", ClassOf(err), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(*carried) != 0 {
		t.Fatalf("the server was sent %d requests with no token to carry", len(*carried))
	}
}

// THE TOKEN GOES TO THE CONFIGURED ORIGIN ALONE, as the configured headers do,
// and an answer that echoes it is logged with it withheld.
func TestATokenGoesToTheServersOriginAloneAndIsNeverLogged(t *testing.T) {
	t.Parallel()
	var got atomic.Pointer[http.Header]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		got.Store(&h)
	}))
	t.Cleanup(srv.Close)
	spec := httpSpec("vendor", "https://mcp.example.com/mcp")
	spec.Bearer = &scriptedBearer{token: "at-origin-0123456789"}
	_, ident := newHTTPTransport(spec, discardLogger(), dialing{})
	ident.base = srv.Client().Transport
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/elsewhere", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ident.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if h := got.Load(); h == nil || h.Get("Authorization") != "" {
		t.Fatalf("a request to another origin carried the token: %v", h)
	}

	var accept atomic.Pointer[string]
	nobody := "at-nobody"
	accept.Store(&nobody)
	echo, _, _ := acceptingOnly(t, &accept)
	var logged strings.Builder
	var logMu sync.Mutex
	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logged.Write(p)
	}), nil))
	spec = httpSpec("vendor", echo.URL)
	spec.Bearer = &scriptedBearer{token: "at-echoed-0123456789"}
	if c, err := connect(t.Context(), spec, logger); err == nil {
		_, _ = c.close(context.Background())
		t.Fatal("a refused server connected")
	}
	logMu.Lock()
	defer logMu.Unlock()
	if !strings.Contains(logged.String(), "http_error") {
		t.Fatalf("the refusal was not logged: %s", logged.String())
	}
	if strings.Contains(logged.String(), "at-echoed-0123456789") {
		t.Fatalf("the log carries the token the server echoed: %s", logged.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A NEW GRANT IS A NEW SERVER, and a token renewed under one is not: the
// bridge restarts a server whose source changed and leaves one alone whose
// source only renewed.
func TestASpecsBearerIsComparedAsTheGrantNotItsToken(t *testing.T) {
	t.Parallel()
	grant := &scriptedBearer{token: "at-1"}
	a := httpSpec("vendor", "https://mcp.example.com/mcp")
	a.Bearer = grant
	b := a
	if !a.equal(b) || !a.SameProcess(b) {
		t.Fatal("one grant's spec is not equal to itself")
	}
	grant.token = "at-2"
	if !a.equal(b) {
		t.Fatal("a renewal made the spec unequal, which would restart the server")
	}
	c := a
	c.Bearer = &scriptedBearer{token: "at-2"}
	if a.equal(c) || a.SameProcess(c) {
		t.Fatal("a new grant's spec is equal to the old one's, so the server keeps the old grant")
	}
	if !errors.Is(fmt.Errorf("x: %w", ErrSignInNeeded), ErrSignInNeeded) ||
		ClassOf(fmt.Errorf("x: %w", ErrSignInNeeded)) != FailNeedsSignIn {
		t.Fatal("a grant nobody holds is not classed needs_sign_in")
	}
}
