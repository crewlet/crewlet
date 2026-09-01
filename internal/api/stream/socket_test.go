package stream_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
)

// socketFixture is a running server plus the pieces a test drives it with.
type socketFixture struct {
	server *httptest.Server
	svc    *stream.Service
	url    string
}

func newSocket(t *testing.T, authOpts func(*config.APIAuth), query stream.Query) *socketFixture {
	t.Helper()
	b := config.DefaultBootstrap()
	if authOpts != nil {
		authOpts(&b.API.Auth)
	}
	guard := auth.New(&b)
	svc := stream.NewService(livestate.New(), stream.Options{
		Now: func() time.Time { return clock },
	})
	t.Cleanup(svc.Stop)

	srv := httptest.NewServer(stream.Handler(guard, svc, query))
	t.Cleanup(srv.Close)
	return &socketFixture{
		server: srv, svc: svc,
		url: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream",
	}
}

// dial opens a socket, optionally with a token on the query string.
func (f *socketFixture) dial(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	target := f.url
	if token != "" {
		target += "?token=" + url.QueryEscape(token)
	}
	conn, res, err := websocket.Dial(t.Context(), target, nil)
	if err == nil {
		t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	}
	return conn, res, err
}

// next reads one frame as a decoded map.
func next(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return got
}

func write(t *testing.T, conn *websocket.Conn, frame map[string]any) {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(t.Context(), websocket.MessageText, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// --- the handshake ------------------------------------------------------- //

func TestASocketOpensAndGetsItsSnapshotImmediately(t *testing.T) {
	t.Parallel()
	// Built entirely from the in-memory projection: no database round trip
	// on connect, which is the whole reason the projection exists.
	f := newSocket(t, nil, nil)
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	got := next(t, conn)
	if got["kind"] != stream.KindSnapshot {
		t.Fatalf("first frame = %v, want a snapshot", got["kind"])
	}
	data, _ := got["data"].(map[string]any)
	for _, key := range []string{"health", "agents", "events", "sandboxes", "tokens", "budget"} {
		if _, present := data[key]; !present {
			t.Errorf("snapshot is missing %q", key)
		}
	}
}

func TestABadTokenFailsTheHandshakeRatherThanOpeningAndDying(t *testing.T) {
	t.Parallel()
	// Accepting and then closing makes the browser see a connection that
	// opened and died, which a page cannot tell from an engine that fell
	// over. Refusing the handshake is what lets the dashboard show its
	// token gate instead.
	f := newSocket(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	}, nil)

	conn, res, err := f.dial(t, "wrong")
	if err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("a wrong token opened a socket")
	}
	if res == nil {
		t.Fatalf("no handshake response: %v", err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("handshake status = %d, want 401", res.StatusCode)
	}
}

// TestAPlainGETSeparatesARefusedCredentialFromAnAcceptedOne pins the pairing
// the dashboard's token gate is built on.
//
// A browser cannot learn WHY a handshake failed. A close code needs a close
// frame, and a handshake that never completed has none, so a refusal arrives
// as 1006 — the same code a stopped engine produces — with the status
// deliberately withheld. The dashboard therefore re-asks over plain HTTP,
// where fetch reports the status, and reads exactly these two answers:
//
//	401 → the credential was refused; prompt, and offer to forget it
//	426 → the credential was fine and only the missing Upgrade header stopped it
//
// Anything else it treats as the network. So a change that made this route
// answer an unauthenticated GET 400, or 500, or that let a bad token through
// to the upgrade attempt, would not fail any socket test — it would silently
// strand every reader holding a stale token on a page that says "retrying"
// for ever. That is the bug this asserts against; see _probeRefusal in
// dashboard/src/protocol/socket.ts.
func TestAPlainGETSeparatesARefusedCredentialFromAnAcceptedOne(t *testing.T) {
	t.Parallel()
	f := newSocket(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	}, nil)

	get := func(t *testing.T, token string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(),
			http.MethodGet, f.server.URL+"/ws/stream", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer res.Body.Close()
		return res.StatusCode
	}

	// The reported failure: anonymous reads are open, so this browser
	// would have connected with no credential at all — but it holds a
	// stale one, and a credential that is PRESENT and wrong is refused.
	if got := get(t, "stale-from-last-deployment"); got != http.StatusUnauthorized {
		t.Errorf("a refused credential = %d, want 401 — the dashboard "+
			"cannot tell the reader their token is wrong", got)
	}
	// And the two that must NOT read as a refusal, or every reader gets a
	// token dialog for an engine that is merely restarting.
	if got := get(t, "secret"); got != http.StatusUpgradeRequired {
		t.Errorf("an accepted credential = %d, want 426", got)
	}
	if got := get(t, ""); got != http.StatusUpgradeRequired {
		t.Errorf("no credential under anonymous reads = %d, want 426", got)
	}
}

func TestAClosedPostureRefusesAnUnauthenticatedSocket(t *testing.T) {
	t.Parallel()
	// The socket carries full LLM transcripts, so it is guarded exactly as
	// the equivalent HTTP read is.
	f := newSocket(t, func(a *config.APIAuth) {
		a.AllowAnonymousRead = false
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	}, nil)

	if conn, _, err := f.dial(t, ""); err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Error("a closed posture opened an unauthenticated socket")
	}
	// And the counterfactual: the right token still gets in.
	conn, _, err := f.dial(t, "secret")
	if err != nil {
		t.Fatalf("a valid token was refused: %v", err)
	}
	if got := next(t, conn); got["kind"] != stream.KindSnapshot {
		t.Errorf("first frame = %v", got["kind"])
	}
}

func TestAnAnonymousReadPostureOpensWithoutACredential(t *testing.T) {
	t.Parallel()
	f := newSocket(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	}, nil)
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := next(t, conn); got["kind"] != stream.KindSnapshot {
		t.Errorf("first frame = %v", got["kind"])
	}
}

// --- the live channel ----------------------------------------------------- //

func TestAnIngestedEventReachesTheSocket(t *testing.T) {
	t.Parallel()
	f := newSocket(t, nil, nil)
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := next(t, conn); got["kind"] != stream.KindSnapshot {
		t.Fatalf("first frame = %v", got["kind"])
	}

	f.svc.Ingest(livestate.Envelope{
		ID: "e1", Type: "task_started", Timestamp: "2026-06-14T12:00:00Z",
		Category: "system", Payload: map[string]any{"role": "Lead", "task_id": "t-1"},
	})

	kinds := map[string]bool{}
	for range 2 {
		kinds[next(t, conn)["kind"].(string)] = true
	}
	if !kinds[stream.KindEvent] || !kinds[stream.KindAgents] {
		t.Errorf("kinds = %v, want the event and the derived overlay", kinds)
	}
}

func TestAPingIsAnswered(t *testing.T) {
	t.Parallel()
	f := newSocket(t, nil, nil)
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn) // snapshot

	write(t, conn, map[string]any{"kind": "ping"})
	if got := next(t, conn); got["kind"] != stream.KindPong {
		t.Errorf("frame = %v, want a pong", got)
	}
}

func TestAnUnknownFrameKindIsIgnored(t *testing.T) {
	t.Parallel()
	// Unknown kinds are ignored on both ends, which is what makes new ones
	// additive rather than a coordinated release.
	f := newSocket(t, nil, nil)
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)

	write(t, conn, map[string]any{"kind": "from-the-future"})
	write(t, conn, map[string]any{"kind": "ping"})
	if got := next(t, conn); got["kind"] != stream.KindPong {
		t.Errorf("an unknown kind broke the socket: %v", got)
	}
}

func TestAMalformedFrameDoesNotDropTheSocket(t *testing.T) {
	t.Parallel()
	// Unparseable input from a client is not a reason to drop a socket
	// that is otherwise working.
	f := newSocket(t, nil, nil)
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)

	if err := conn.Write(t.Context(), websocket.MessageText, []byte("not json")); err != nil {
		t.Fatal(err)
	}
	write(t, conn, map[string]any{"kind": "ping"})
	if got := next(t, conn); got["kind"] != stream.KindPong {
		t.Errorf("a malformed frame broke the socket: %v", got)
	}
}

// --- queries -------------------------------------------------------------- //

func TestAQueryIsAnsweredWithItsCorrelationID(t *testing.T) {
	t.Parallel()
	f := newSocket(t, nil, func(_ context.Context, what string, params map[string]any, _ string) (any, error) {
		return map[string]any{"what": what, "role": params["role"]}, nil
	})
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)

	write(t, conn, map[string]any{
		"kind": "query", "id": 7, "what": "agent",
		"params": map[string]any{"role": "Lead"},
	})
	got := next(t, conn)
	if got["kind"] != stream.KindResult || got["id"] != float64(7) || got["what"] != "agent" {
		t.Fatalf("answer = %v", got)
	}
	data, _ := got["data"].(map[string]any)
	if data["role"] != "Lead" {
		t.Errorf("params did not reach the query: %v", data)
	}
}

func TestEachQueryFailureCarriesItsOwnCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{stream.ErrUnknownQuery, stream.CodeUnknownQuery},
		{stream.ErrUnauthorized, stream.CodeUnauthorized},
		{errors.New("the store fell over at /var/lib/crewlet/crewlet.db"), stream.CodeQueryFailed},
	} {
		f := newSocket(t, nil, func(context.Context, string, map[string]any, string) (any, error) {
			return nil, tc.err
		})
		conn, _, err := f.dial(t, "")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		next(t, conn)

		write(t, conn, map[string]any{"kind": "query", "id": 1, "what": "config"})
		got := next(t, conn)
		if got["kind"] != stream.KindError || got["error"] != tc.want {
			t.Errorf("%v: answer = %v, want %q", tc.err, got, tc.want)
		}
		// The reason reaches the log, not the client: a failure can
		// carry a database path, and the socket is the one surface an
		// unauthenticated reader may be holding.
		if raw, _ := json.Marshal(got); strings.Contains(string(raw), "/var/lib") {
			t.Errorf("the failure leaked its detail to the client: %s", raw)
		}
	}
}

func TestAQueryWithNoSurfaceIsAnUnknownQuery(t *testing.T) {
	t.Parallel()
	f := newSocket(t, nil, nil)
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)

	write(t, conn, map[string]any{"kind": "query", "id": 3, "what": "agent"})
	if got := next(t, conn); got["error"] != stream.CodeUnknownQuery {
		t.Errorf("answer = %v", got)
	}
}

func TestTheSocketsOperatorReachesTheQuery(t *testing.T) {
	t.Parallel()
	seen := make(chan string, 1)
	f := newSocket(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	}, func(_ context.Context, _ string, _ map[string]any, operatorID string) (any, error) {
		seen <- operatorID
		return nil, nil
	})
	conn, _, err := f.dial(t, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)

	write(t, conn, map[string]any{"kind": "query", "id": 1, "what": "config"})
	select {
	case got := <-seen:
		if got != "founder" {
			t.Errorf("operator = %q, want founder", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the query never ran")
	}
}

func TestAFrameTokenUpgradesOneQueryOnly(t *testing.T) {
	t.Parallel()
	// How a socket opened for anonymous reads asks one operator-only
	// question without reconnecting. A browser cannot set a header on a
	// WebSocket constructor.
	seen := make(chan string, 2)
	f := newSocket(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	}, func(_ context.Context, _ string, _ map[string]any, operatorID string) (any, error) {
		seen <- operatorID
		return nil, nil
	})
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)

	write(t, conn, map[string]any{"kind": "query", "id": 1, "what": "config", "token": "secret"})
	if got := <-seen; got != "founder" {
		t.Errorf("credentialled query ran as %q, want founder", got)
	}
	// And the NEXT query, with no token, is anonymous again.
	write(t, conn, map[string]any{"kind": "query", "id": 2, "what": "events"})
	if got := <-seen; got != "" {
		t.Errorf("the upgrade outlived its own query: %q", got)
	}
}

func TestAWrongFrameTokenDoesNotUpgrade(t *testing.T) {
	t.Parallel()
	seen := make(chan string, 1)
	f := newSocket(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	}, func(_ context.Context, _ string, _ map[string]any, operatorID string) (any, error) {
		seen <- operatorID
		return nil, nil
	})
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)

	write(t, conn, map[string]any{"kind": "query", "id": 1, "what": "config", "token": "wrong"})
	if got := <-seen; got != "" {
		t.Errorf("a wrong frame token authenticated as %q", got)
	}
}

func TestABadFrameTokenDoesNotDowngradeAnAuthenticatedSocket(t *testing.T) {
	t.Parallel()
	// The frame token UPGRADES one query; it must never demote the socket
	// that is already authenticated. A garbled or expired token on one
	// frame would otherwise silently answer an operator's question as
	// anonymous — and an operator-only query would come back unauthorized
	// on a socket that had every right to ask it.
	seen := make(chan string, 1)
	f := newSocket(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	}, func(_ context.Context, _ string, _ map[string]any, operatorID string) (any, error) {
		seen <- operatorID
		return nil, nil
	})
	conn, _, err := f.dial(t, "secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)

	write(t, conn, map[string]any{"kind": "query", "id": 1, "what": "config", "token": "garbled"})
	select {
	case got := <-seen:
		if got != "founder" {
			t.Errorf("query ran as %q, want the socket's own operator: a bad "+
				"frame token demoted an authenticated socket", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the query never ran")
	}
}

func TestQueriesRunConcurrentlyUpToTheBound(t *testing.T) {
	t.Parallel()
	// Concurrent so a store scan cannot stall the live feed, and bounded
	// so an unbounded fan-out from one tab cannot starve the engine's own
	// writes.
	entered := make(chan struct{}, stream.MaxInFlightQueries*4)
	release := make(chan struct{})
	f := newSocket(t, nil, func(ctx context.Context, _ string, _ map[string]any, _ string) (any, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	})
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)
	defer close(release)

	for i := range stream.MaxInFlightQueries * 3 {
		write(t, conn, map[string]any{"kind": "query", "id": i, "what": "events"})
	}
	// Exactly the bound get in; the rest queue behind them.
	for range stream.MaxInFlightQueries {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("queries did not run concurrently")
		}
	}
	select {
	case <-entered:
		t.Errorf("more than %d queries ran at once", stream.MaxInFlightQueries)
	case <-time.After(250 * time.Millisecond):
	}
}

func TestASlowQueryDoesNotStallTheLiveFeed(t *testing.T) {
	t.Parallel()
	// The reason queries run off the read loop at all.
	release := make(chan struct{})
	f := newSocket(t, nil, func(ctx context.Context, _ string, _ map[string]any, _ string) (any, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	})
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)
	defer close(release)

	write(t, conn, map[string]any{"kind": "query", "id": 1, "what": "events"})
	f.svc.Ingest(livestate.Envelope{
		ID: "e1", Type: "task_started", Timestamp: "2026-06-14T12:00:00Z",
		Category: "system", Payload: map[string]any{"role": "Lead", "task_id": "t-1"},
	})
	if got := next(t, conn); got["kind"] != stream.KindEvent {
		t.Errorf("frame = %v, want the live event through a blocked query", got["kind"])
	}
}

func TestADisconnectedClientLeavesTheHub(t *testing.T) {
	t.Parallel()
	f := newSocket(t, nil, nil)
	conn, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, conn)
	if got := f.svc.Hub().Clients(); got != 1 {
		t.Fatalf("clients = %d, want 1", got)
	}

	_ = conn.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f.svc.Hub().Clients() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("clients = %d after a disconnect, want 0", f.svc.Hub().Clients())
}
