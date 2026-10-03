package stream_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// socketFixture is a running server plus the pieces a test drives it with.
type socketFixture struct {
	server *httptest.Server
	svc    *stream.Service
	url    string
}

func newSocket(t *testing.T, authOpts func(*config.APIAuth), query stream.Query) *socketFixture {
	t.Helper()
	return newSocketWith(t, authOpts, query, stream.Options{})
}

// newSocketWith is newSocket for a case whose subject is the SERVICE — its
// posture, its tick — rather than the handshake.
func newSocketWith(t *testing.T, authOpts func(*config.APIAuth), query stream.Query,
	opts stream.Options,
) *socketFixture {
	t.Helper()
	b := config.DefaultBootstrap()
	// EVERY FIXTURE IS CREDENTIALLED, because every socket is: there is no
	// anonymous-read posture any more, so a dial with nothing to present
	// is refused before the handshake and every case below would be
	// asserting the refusal rather than its own subject.
	b.API.Auth.Tokens = []config.APIToken{{ID: "founder", Token: fixtureToken}}
	if authOpts != nil {
		authOpts(&b.API.Auth)
	}
	// AND EVERY FIXTURE CREDENTIAL CAN READ what the socket carries, for
	// the same reason: the socket refuses a caller without `state:read`,
	// and the grant ceiling is intersected on every request. A case about
	// what a NARROWER credential receives states its own grants.
	if b.API.Auth.MaxGrants == nil {
		b.API.Auth.MaxGrants = iam.AllGrants
	}
	for i := range b.API.Auth.Tokens {
		if b.API.Auth.Tokens[i].Grants == nil {
			b.API.Auth.Tokens[i].Grants = []iam.Grant{iam.GrantStateRead,
				iam.GrantAuditRead}
		}
	}
	// THE DEPLOYMENT HAS AN ADDRESS, as every served one must, so a case
	// about the handshake's Origin has something to be judged against. A
	// dial from Go sends no Origin at all, which is how a non-browser
	// client reaches this path and what every other case here does.
	b.API.ExternalURL = fixtureOrigin
	guard := auth.New(&b)
	svc := buildService(t, opts)

	srv := httptest.NewServer(stream.Handler(guard, auth.NewCSRF(&b), svc, query))
	t.Cleanup(srv.Close)
	return &socketFixture{
		server: srv, svc: svc,
		url: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/stream",
	}
}

// fixtureOrigin is where every fixture's deployment says a browser reaches it:
// NOT the test server's own address, which is the point — a deployment behind a
// proxy is reached at a public address its listener never sees as `Host`.
const fixtureOrigin = "https://crewlet.example.com"

// fixtureToken is what every fixture's default credential is, and what an
// empty argument to [socketFixture.dial] presents.
const fixtureToken = "fixture-token-long-enough-to-pass"

// dial opens a socket. An empty token presents the fixture's own credential,
// which is what an ordinary case wants; [socketFixture.dialAnonymous] is how a
// case asks for the unauthenticated arm on purpose.
func (f *socketFixture) dial(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	if token == "" {
		token = fixtureToken
	}
	return f.dialWith(t, token)
}

// bearer is a handshake header presenting token, the one place a client
// other than a browser puts its credential: nothing is read off the URL.
func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

// dialAnonymous opens a socket presenting NOTHING.
func (f *socketFixture) dialAnonymous(t *testing.T) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	return f.dialWith(t, "")
}

func (f *socketFixture) dialWith(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	var opts *websocket.DialOptions
	if token != "" {
		opts = &websocket.DialOptions{HTTPHeader: bearer(token)}
	}
	conn, res, err := websocket.Dial(t.Context(), f.url, opts)
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

// waitFor polls until cond holds, or fails with why.
//
// The engine's own goroutine acts on a frame this test wrote, so there is no
// moment the test can synchronise on other than the effect itself.
func waitFor(t *testing.T, cond func() bool, why string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(why)
}

// kindOf is a decoded frame's kind, typed. Compared as they come out of the
// decode, the JSON string "snapshot" is never equal to [stream.KindSnapshot]:
// an `any` holding a string and one holding a Kind differ in dynamic type, so
// `kindOf(frame) != stream.KindSnapshot` is true of every frame there is.
func kindOf(frame map[string]any) stream.Kind {
	kind, _ := frame["kind"].(string)
	return stream.Kind(kind)
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
	if kindOf(got) != stream.KindSnapshot {
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
// for ever. That is the bug this asserts against; see probeRefusal in
// dashboard/src/protocol/socket.ts.
func TestAPlainGETSeparatesARefusedCredentialFromAnAcceptedOne(t *testing.T) {
	t.Parallel()
	f := newSocket(t, nil, nil)

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

	// The reported failure: a browser holding a stale credential, which is
	// PRESENT and wrong and therefore refused.
	if got := get(t, "stale-from-last-deployment"); got != http.StatusUnauthorized {
		t.Errorf("a refused credential = %d, want 401 — the dashboard "+
			"cannot tell the reader their token is wrong", got)
	}
	// NO CREDENTIAL IS NOW THE SAME ANSWER, which it was not: it used to
	// be 426, because an anonymous socket would have opened. The pairing
	// the dashboard reads is unchanged — 401 means "your credential is the
	// problem" and 426 means "the engine is fine, you used the wrong
	// protocol" — and what moved is which side of it an empty credential
	// falls on.
	if got := get(t, ""); got != http.StatusUnauthorized {
		t.Errorf("no credential = %d, want 401", got)
	}
	// And the one that must NOT read as a refusal, or every reader gets a
	// token dialog for an engine that is merely restarting.
	if got := get(t, fixtureToken); got != http.StatusUpgradeRequired {
		t.Errorf("an accepted credential = %d, want 426", got)
	}
}

func TestAnUnauthenticatedSocketIsRefused(t *testing.T) {
	t.Parallel()
	// The socket carries full LLM transcripts, so it is guarded exactly as
	// the equivalent HTTP read is — which used to mean "unless
	// allow_anonymous_read is on", and now means always.
	f := newSocket(t, nil, nil)

	if conn, _, err := f.dialAnonymous(t); err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Error("a socket opened with no credential at all")
	}
	// And the counterfactual: the right token still gets in, or the
	// assertion above would pass on a fixture that refuses everything.
	conn, _, err := f.dial(t, fixtureToken)
	if err != nil {
		t.Fatalf("a valid token was refused: %v", err)
	}
	if got := next(t, conn); kindOf(got) != stream.KindSnapshot {
		t.Errorf("first frame = %v", got["kind"])
	}
}

// dialFrom opens a socket the way a BROWSER does: carrying the page's Origin.
func (f *socketFixture) dialFrom(t *testing.T, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	header := bearer(fixtureToken)
	header.Set("Origin", origin)
	conn, res, err := websocket.Dial(t.Context(), f.url,
		&websocket.DialOptions{HTTPHeader: header})
	if err == nil {
		t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	}
	return conn, res, err
}

// THE HANDSHAKE'S ORIGIN IS JUDGED BY THE WRITES' RULE.
//
// The socket used to leave it to its WebSocket library, which compares
// `Origin` with the request's own `Host`. The fixture's deployment is reached
// at an address that is not the test server's — exactly a deployment behind a
// proxy that rewrites `Host` to its upstream's — and its second hostname is
// named in `api.auth.allowed_origins`: both were refused at the handshake
// while every write from them was served.
func TestTheHandshakeAdmitsEveryAddressTheDeploymentIsReachedAt(t *testing.T) {
	t.Parallel()
	f := newSocket(t, func(a *config.APIAuth) {
		a.AllowedOrigins = []string{"https://ops.example.com"}
	}, nil)
	for _, origin := range []string{fixtureOrigin, "https://ops.example.com"} {
		conn, _, err := f.dialFrom(t, origin)
		if err != nil {
			t.Errorf("a handshake from %s was refused: %v", origin, err)
			continue
		}
		if got := next(t, conn); kindOf(got) != stream.KindSnapshot {
			t.Errorf("from %s: first frame = %v, want the snapshot", origin, got["kind"])
		}
	}
}

// AND A PAGE ON ANOTHER SITE IS STILL REFUSED, which is the control and the
// reason the check exists: a browser attaches the session cookie to a
// cross-site page's handshake exactly as it does to its form post, and the
// socket streams the company's state to whoever opened it.
func TestAHandshakeFromAnotherSiteIsRefused(t *testing.T) {
	t.Parallel()
	f := newSocket(t, nil, nil)
	conn, res, err := f.dialFrom(t, "https://evil.example.com")
	if err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("a handshake from another site opened a socket")
	}
	if res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %v, want 403 csrf_origin", res)
	}
}

// A PLAIN GET IS NOT A HANDSHAKE, so its Origin is not judged: the dashboard
// re-asks this path over fetch to read a refused handshake's status, a
// same-origin GET sends no Origin, and it carries the cookie. Judged, the
// cookie-with-no-Origin arm would answer 403 and the page would report a
// withdrawn surface to somebody whose engine was merely restarting.
func TestThePlainReAskIsNotJudgedForItsOrigin(t *testing.T) {
	t.Parallel()
	f := newSocket(t, nil, nil)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		f.server.URL+"/ws/stream", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+fixtureToken)
	req.Header.Set("Origin", "https://evil.example.com")
	res, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUpgradeRequired {
		t.Errorf("status = %d, want 426: a GET that asks for no upgrade "+
			"opens nothing, so there is nothing for its Origin to decide", res.StatusCode)
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
	if got := next(t, conn); kindOf(got) != stream.KindSnapshot {
		t.Fatalf("first frame = %v", got["kind"])
	}

	f.svc.Ingest(livestate.Envelope{
		ID: "e1", Type: "agent_phase_started", Timestamp: "2026-06-14T12:00:00Z",
		Category: "system", Payload: map[string]any{"agent_id": "a-lead", "role": "Lead", "task_id": "t-1"},
	})

	kinds := map[stream.Kind]bool{}
	for range 2 {
		kinds[kindOf(next(t, conn))] = true
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
	if got := next(t, conn); kindOf(got) != stream.KindPong {
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
	if got := next(t, conn); kindOf(got) != stream.KindPong {
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
	if got := next(t, conn); kindOf(got) != stream.KindPong {
		t.Errorf("a malformed frame broke the socket: %v", got)
	}
}

// --- queries -------------------------------------------------------------- //

func TestAQueryIsAnsweredWithItsCorrelationID(t *testing.T) {
	t.Parallel()
	f := newSocket(t, nil, func(_ context.Context, what string, params map[string]any) (any, error) {
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
	if kindOf(got) != stream.KindResult || got["id"] != float64(7) || got["what"] != "agent" {
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
		{stream.ErrNotFound, stream.CodeNotFound},
		// A REFUSED REQUEST IS NOT A FAILED ONE. Left to the default it
		// reached the client as `query_failed` — retried on every poll
		// of a screen that could never succeed — and was logged by this
		// node as though its own health were in question.
		{stream.ErrBadParams, stream.CodeBadParams},
		{stream.ErrUnavailable, stream.CodeUnavailable},
		{errors.New("the store fell over at /var/lib/crewlet/crewlet.db"), stream.CodeQueryFailed},
	} {
		f := newSocket(t, nil, func(context.Context, string, map[string]any) (any, error) {
			return nil, tc.err
		})
		conn, _, err := f.dial(t, "")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		next(t, conn)

		write(t, conn, map[string]any{"kind": "query", "id": 1, "what": "config"})
		got := next(t, conn)
		if kindOf(got) != stream.KindError || got["error"] != tc.want {
			t.Errorf("%v: answer = %v, want %q", tc.err, got, tc.want)
		}
		// The reason reaches the log, not the client: a failure can
		// carry a database path, and holding the grant a question needs
		// does not make a reader somebody that path is meant for.
		if raw, _ := json.Marshal(got); strings.Contains(string(raw), "/var/lib") {
			t.Errorf("the failure leaked its detail to the client: %s", raw)
		}
	}
}

// A REFUSAL CARRIES ITS SENTENCE, AND NOTHING ELSE DOES.
//
// `bad_params` is the one failure of a question whose text is written for the
// caller — it names the parameter to change — and it reached the debug log and
// nothing else, so a person who asked for a window past the spend history read
// "the engine refused this request" with no word of which field or why. An
// `unavailable` answer carries its STATE LOG's words, under the same key, and
// never the error behind them. Every other code stays bare: its text can carry
// a path, and a detail on it would be the leak the loop above guards against.
//
// Mutation: build the `unavailable` frame by setting its Unavailable alone,
// and the state log's words never reach the frame.
func TestARefusalCarriesItsSentenceAndNothingElseDoes(t *testing.T) {
	t.Parallel()
	const sentence = "days is 91, and a spend window is 1 to 90 company days — ask for at most 90"
	const remedy = "raise the stream's byte ceiling"
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&stream.BadParamsError{What: "tokens", Detail: sentence}, sentence},
		{stream.ErrBadParams, ""},
		{fmt.Errorf("%w: tokens: %w", stream.ErrUnavailable,
			errors.New("open /var/lib/crewlet/crewlet.db: busy")), ""},
		{fmt.Errorf("%w: tokens: %w", stream.ErrUnavailable,
			&statelog.Refused{Code: statelog.RefuseLogFull, Detail: remedy}), remedy},
		{errors.New("open /var/lib/crewlet/crewlet.db: " + sentence), ""},
	} {
		f := newSocket(t, nil, func(context.Context, string, map[string]any) (any, error) {
			return nil, tc.err
		})
		conn, _, err := f.dial(t, "")
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		next(t, conn)

		write(t, conn, map[string]any{"kind": "query", "id": 1, "what": "tokens"})
		got := next(t, conn)
		detail, present := got["detail"]
		switch {
		case tc.want == "" && present:
			t.Errorf("%v: the frame carried a detail it has no business carrying: %v", tc.err, got)
		case tc.want != "" && detail != tc.want:
			t.Errorf("%v: detail = %v, want the refusal's own sentence %q", tc.err, detail, tc.want)
		}
		if tc.want == sentence && got["error"] != stream.CodeBadParams {
			t.Errorf("%v: code = %v, want bad_params", tc.err, got["error"])
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

// THE CALLER REACHES THE QUERY, and it reaches it in the CONTEXT.
//
// The socket used to hand an operator id along as a fourth argument, converted
// from the very principal the guard had already resolved into that context —
// so a question asking who was calling read a string derived from an answer it
// could have read directly, and the two were one edit from disagreeing.
func TestTheSocketsCallerReachesTheQuery(t *testing.T) {
	t.Parallel()
	seen := make(chan string, 1)
	f := newSocket(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	}, func(ctx context.Context, _ string, _ map[string]any) (any, error) {
		// BOTH VALUES, because the resolution is the point: a socket
		// this node could not check must not reach a query looking
		// like an anonymous one.
		principal, how := iam.From(ctx)
		if how != iam.Resolved {
			seen <- "unresolved: " + string(how)
			return nil, nil
		}
		seen <- iam.ActorFor(principal).OperatorID
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
		if got != "token:founder" {
			t.Errorf("operator = %q, want token:founder", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the query never ran")
	}
}

// TestAWatchNeedsAnOperator pins that a subscription is a WRITE.
//
// A watch installs a row in this node's routing index on behalf of a caller
// and leaves it there for the life of the socket, so a client nobody can name
// that could install one would be writing server state anonymously.
//
// THE UNAUTHENTICATED ARM IS NOW THE HANDSHAKE'S, which is what this asserts:
// there is no anonymous socket to send a watch from any more, so the refusal
// happens before a frame is ever read. The guard inside `watch` stays for the
// one case that would resurrect it — the socket path being declared exempt —
// and that case cannot be reached from here.
func TestAWatchNeedsAnOperator(t *testing.T) {
	t.Parallel()
	// THE ADMIN GRANT, so the counterfactual below is about the credential
	// and not about whose seat "lead" is — which is watch_test.go's.
	f := newSocket(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{{ID: "founder", Token: fixtureToken,
			Grants: []iam.Grant{iam.GrantStateRead, iam.GrantFleetOperate}}}
	}, nil)

	if conn, _, err := f.dialAnonymous(t); err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Error("a socket with no credential opened, so a watch could reach the index")
	}
	if got := f.svc.Hub().Watchers("lead"); got != 0 {
		t.Errorf("watchers = %d: an unauthenticated client wrote the index", got)
	}

	// The counterfactual: an operator's watch takes, and clearing it
	// releases the bucket.
	held, _, err := f.dial(t, fixtureToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, held)
	write(t, held, map[string]any{"kind": "watch", "seat": "lead"})
	waitFor(t, func() bool { return f.svc.Hub().Watchers("lead") == 1 },
		"an operator's watch never reached the index")
	write(t, held, map[string]any{"kind": "watch", "seat": ""})
	waitFor(t, func() bool { return f.svc.Hub().Watchers("lead") == 0 },
		"clearing a watch left the client in the index")
}

func TestABadFrameTokenDoesNotDowngradeAnAuthenticatedSocket(t *testing.T) {
	t.Parallel()
	// A FRAME CARRIES NO CREDENTIAL any more (see the note above
	// [stream.Handler]), and a client built when it did still sends one.
	// Every question is asked as the principal the handshake resolved, so
	// a garbled token on one frame must change nothing: read, it would
	// answer an operator's question as somebody else, or refuse one the
	// socket had every right to ask.
	seen := make(chan string, 1)
	f := newSocket(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{{ID: "founder", Token: "secret"}}
	}, func(ctx context.Context, _ string, _ map[string]any) (any, error) {
		// BOTH VALUES, because the resolution is the point: a socket
		// this node could not check must not reach a query looking
		// like an anonymous one.
		principal, how := iam.From(ctx)
		if how != iam.Resolved {
			seen <- "unresolved: " + string(how)
			return nil, nil
		}
		seen <- iam.ActorFor(principal).OperatorID
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
		if got != "token:founder" {
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
	// so an unbounded fan-out cannot starve the engine's own writes. What
	// the bound is PER is the case below this one.
	entered := make(chan struct{}, stream.MaxInFlightQueries*4)
	release := make(chan struct{})
	f := newSocket(t, nil, func(ctx context.Context, _ string, _ map[string]any) (any, error) {
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

// EACH SOCKET HAS ITS OWN IN-FLIGHT BUDGET, and one tab's burst does not hold
// back another's — the same person's included — while the node has room.
//
// The bound is what one tab needs running at once, so it is counted per
// socket. It used to be shared by every socket one principal held, which made
// a person's second tab wait on their first while bounding the wrong thing:
// what keeps sockets from starving the engine's own reads is the node's
// ceiling on all of them together, the next case. Two sockets presenting ONE
// credential is the case, on a store wide enough that the ceiling is not met:
// the first takes its whole budget and holds it, and the second still runs its
// own four.
func TestEachSocketHasItsOwnInFlightBudget(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, stream.MaxInFlightQueries*8)
	release := make(chan struct{})
	f := newSocket(t, nil, func(ctx context.Context, _ string, _ map[string]any) (any, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	})
	defer close(release)

	first, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, first)
	for i := range stream.MaxInFlightQueries * 2 {
		write(t, first, map[string]any{"kind": "query", "id": i, "what": "events"})
	}
	for range stream.MaxInFlightQueries {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the first socket's queries did not run")
		}
	}

	// THE SAME CREDENTIAL, a second tab, with the first's still blocked.
	second, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial a second tab: %v", err)
	}
	next(t, second)
	for i := range stream.MaxInFlightQueries {
		write(t, second, map[string]any{"kind": "query", "id": 500 + i, "what": "events"})
	}
	for range stream.MaxInFlightQueries {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("a second tab queued behind the first tab's burst")
		}
	}
	// AND STILL BOUNDED: the first socket's queued half did not get in
	// beside them.
	select {
	case <-entered:
		t.Errorf("more than %d queries ran on one socket", stream.MaxInFlightQueries)
	case <-time.After(250 * time.Millisecond):
	}
}

// EVERY SOCKET ON A NODE SHARES ONE CEILING, at half the store's readers, and
// a socket waiting on it still answers its keepalive.
//
// Nothing bounds how many sockets a caller holding `state:read` opens, so
// without a node-wide ceiling N sockets at four queries each take every reader
// the store has and the engine's own reads — a seat's tool lookups, the
// coverage probes, the /health body — queue behind them. Here the store has
// two readers, so every socket together may run one query: a second socket's
// query waits while the first runs, runs the moment it finishes, and the
// waiting socket's ping is answered meanwhile because the wait is not on its
// read loop.
//
// Mutation: drop the node's ceiling and both queries run at once.
func TestEverySocketOnANodeSharesOneQueryCeiling(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	f := newSocketWith(t, nil, func(ctx context.Context, _ string, _ map[string]any) (any, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, nil
	}, stream.Options{Readers: 2})
	defer close(release)

	first, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	next(t, first)
	second, _, err := f.dial(t, "")
	if err != nil {
		t.Fatalf("dial a second tab: %v", err)
	}
	next(t, second)

	write(t, first, map[string]any{"kind": "query", "id": 1, "what": "events"})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first socket's query never ran")
	}
	write(t, second, map[string]any{"kind": "query", "id": 2, "what": "events"})
	select {
	case <-entered:
		t.Fatal("a second socket's query ran beside the first on a node whose " +
			"ceiling is one")
	case <-time.After(250 * time.Millisecond):
	}
	// THE WAITING SOCKET STILL ANSWERS ITS KEEPALIVE, whatever pushes
	// arrive first.
	write(t, second, map[string]any{"kind": "ping"})
	for kindOf(next(t, second)) != stream.KindPong {
		// another push, read past
	}

	release <- struct{}{}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the second socket's query never ran once the first finished")
	}
}

func TestASlowQueryDoesNotStallTheLiveFeed(t *testing.T) {
	t.Parallel()
	// The reason queries run off the read loop at all.
	release := make(chan struct{})
	f := newSocket(t, nil, func(ctx context.Context, _ string, _ map[string]any) (any, error) {
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
		ID: "e1", Type: "agent_phase_started", Timestamp: "2026-06-14T12:00:00Z",
		Category: "system", Payload: map[string]any{"agent_id": "a-lead", "role": "Lead", "task_id": "t-1"},
	})
	if got := next(t, conn); kindOf(got) != stream.KindEvent {
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

// A RESOLVED CALLER WHO MAY READ NONE OF WHAT THE SOCKET CARRIES IS REFUSED
// BEFORE THE UPGRADE, and the page's plain-HTTP re-ask sees why.
//
// Every push is a `state:read` question's answer or an `audit:read` one's, so
// the socket used to accept anybody resolved and hand them the whole company's
// state. 403 rather than 401: the credential is fine, and signing in again
// reaches the same grants.
func TestASocketWithoutStateReadIsRefusedBeforeTheUpgrade(t *testing.T) {
	t.Parallel()
	const auditOnly = "a-token-carrying-audit-read-alone"
	f := newSocket(t, func(a *config.APIAuth) {
		a.Tokens = []config.APIToken{
			{ID: "founder", Token: fixtureToken},
			{ID: "auditor", Token: auditOnly, Grants: []iam.Grant{iam.GrantAuditRead}},
		}
	}, nil)
	conn, res, err := f.dial(t, auditOnly)
	if err == nil {
		_ = conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("a caller without state:read opened the socket")
	}
	if res == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("the handshake answered %v, want 403: %v", res, err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		f.server.URL+"/ws/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+auditOnly)
	plain, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Body.Close()
	if plain.StatusCode != http.StatusForbidden {
		t.Fatalf("the plain re-ask answered %d, want 403", plain.StatusCode)
	}
}
