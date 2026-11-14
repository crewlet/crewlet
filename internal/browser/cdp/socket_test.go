package cdp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// wsURL is srv's address as a websocket's, path and query appended.
func wsURL(srv *httptest.Server, rest string) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + rest
}

// THE SOCKET REFUSES A DIAL WITHOUT AN HTTP CLIENT, before anything is
// dialled: the websocket library would otherwise fall back to the
// process-global client, which no pool guard sees.
//
// Mutation: hand the nil client to the library, and the server sees the
// upgrade the default client sent — red.
func TestTheSocketRefusesADialWithoutAnHTTPClient(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	if _, err := Dial(bounded(t), wsURL(srv, "/devtools/browser/x"), DialOptions{}); !errors.Is(err, ErrNoHTTPClient) {
		t.Fatalf("a dial without a client answered %v, want ErrNoHTTPClient", err)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("the server saw %d requests from a dial that had no client", n)
	}
}

// THE SOCKET NEVER FOLLOWS A REDIRECT: an upgrade answered with a 307 to a
// second server fails with ErrRedirected and the second server sees nothing —
// a followed redirect would carry the pool's credentials wherever it pointed —
// while the caller's own client keeps its own redirect policy. The error
// names the address without its query, where a vendor's token may sit.
//
// Mutation: drop the copy's CheckRedirect, and the client with no policy of
// its own follows the redirect to the second server — red.
func TestTheSocketNeverFollowsARedirect(t *testing.T) {
	t.Parallel()
	var followed atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		if ws, err := websocket.Accept(w, r, nil); err == nil {
			_ = ws.CloseNow()
		}
	}))
	t.Cleanup(second.Close)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/devtools/browser/elsewhere", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(first.Close)

	// Each its own client on the server's transport: Client() hands back
	// one shared client, whose policy one case would set for both.
	ownPolicy := errors.New("the caller's own redirect policy")
	follows := &http.Client{Transport: first.Client().Transport}
	withPolicy := &http.Client{
		Transport:     first.Client().Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return ownPolicy },
	}
	for _, tc := range []struct {
		name   string
		client *http.Client
	}{
		{"a client that would follow", follows},
		{"a client with a policy of its own", withPolicy},
	} {
		_, err := Dial(bounded(t), wsURL(first, "/devtools/browser/x?token=pool-secret"), DialOptions{
			HTTPClient: tc.client,
			Header:     http.Header{"Authorization": {"Bearer pool-secret"}},
		})
		if !errors.Is(err, ErrRedirected) {
			t.Fatalf("%s: the redirected dial answered %v, want ErrRedirected", tc.name, err)
		}
		if strings.Contains(err.Error(), "pool-secret") {
			t.Fatalf("%s: the error names the address's query: %v", tc.name, err)
		}
	}
	if n := followed.Load(); n != 0 {
		t.Fatalf("the second server saw %d requests: a redirect was followed", n)
	}
	if err := withPolicy.CheckRedirect(nil, nil); !errors.Is(err, ownPolicy) {
		t.Fatalf("the caller's client lost its own redirect policy: %v", err)
	}
}

// A FAILED DIAL NAMES NEITHER ITS QUERY NOR ITS CREDENTIALS: a vendor's
// address may carry a key in either, and a dial that fails — here at a port
// that refuses it, as one would at a DNS or a TLS failure — answers an error
// naming the address by its scheme, host and path alone.
//
// Mutations: return the transport's error whole, the address the HTTP client
// embeds in it included, and the error names the query — red; keep the user
// information in the address the error names, and it names the password —
// red.
func TestAFailedDialNamesNeitherItsQueryNorItsCredentials(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusing := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	addr := "ws://user:pw-secret@" + refusing + "/devtools/browser/x?token=pool-secret"
	_, err = Dial(bounded(t), addr, DialOptions{HTTPClient: httpxtest.Pool(t)})
	if err == nil {
		t.Fatalf("a dial of %s, which refuses it, succeeded", refusing)
	}
	for _, secret := range []string{"pw-secret", "pool-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the failed dial's error names %s: %v", secret, err)
		}
	}
	if !strings.Contains(err.Error(), "ws://"+refusing+"/devtools/browser/x") {
		t.Errorf("the failed dial's error does not name the address it dialled: %v", err)
	}
}

// A CONNECTION OUTLIVES THE CONTEXT IT WAS DIALLED UNDER: the context bounds
// the dial alone, and once Dial has returned its end ends nothing, so a caller
// that dials under a bounded opening step keeps its browser after it — two
// calls are answered after the cancel, and the connection is open.
//
// Mutation: hand the dial's own context to the connection's reader and
// writer, and the cancel ends the connection — red.
func TestAConnectionOutlivesTheContextItWasDialledUnder(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	dialling, cancel := context.WithCancel(ctx)
	c, p := socketPairDialled(dialling, t, nil)
	cancel()
	// Two calls, so a reader the cancel ended cannot pass by answering the
	// first before it saw the cancel.
	for i := range 2 {
		calls := make(chan error, 1)
		go func() {
			_, err := Call(ctx, c.Browser(), testCall, testParams{})
			calls <- err
		}()
		r, err := p.recv()
		if err == nil {
			err = p.answer(r, testResult{})
		}
		if err != nil {
			t.Fatalf("call %d after the dial's context ended never reached the peer: %v (the connection ended with %v)", i, err, c.Err())
		}
		if err := <-calls; err != nil {
			t.Fatalf("call %d after the dial's context ended: %v", i, err)
		}
	}
	if err := c.Err(); err != nil {
		t.Fatalf("the connection ended with its dial's context: %v", err)
	}
}

// THE SOCKET SENDS THE CALLER'S HEADER AND HOST on the upgrade: a pool's
// credentials, and the Host a plain Chromium admits where the caller reached
// it by a name.
//
// Mutation: drop DialOptions.Host, and the upgrade carries the dialled
// address as its Host — red.
func TestTheSocketSendsTheCallersHeaderAndHost(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var gotAuth, gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth, gotHost = r.Header.Get("Authorization"), r.Host
		mu.Unlock()
		if ws, err := websocket.Accept(w, r, nil); err == nil {
			_ = ws.CloseNow()
		}
	}))
	t.Cleanup(srv.Close)
	c, err := Dial(bounded(t), wsURL(srv, "/devtools/browser/x"), DialOptions{
		HTTPClient: srv.Client(),
		Header:     http.Header{"Authorization": {"Bearer pool-credential"}},
		Host:       "127.0.0.1:9222",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer pool-credential" || gotHost != "127.0.0.1:9222" {
		t.Fatalf("the upgrade carried Authorization %q and Host %q", gotAuth, gotHost)
	}
}
