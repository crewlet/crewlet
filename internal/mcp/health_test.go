package mcp

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/httpx"
)

// EVERY FAILURE CLASS IS NAMED ONCE, and only the classes listed are valid:
// the dashboard's copy of the union is held to this list, so a class added
// here and not there is a word the page cannot draw.
func TestTheFailureClassesAreTheFourteenATestTellsApart(t *testing.T) {
	t.Parallel()
	seen := map[FailureClass]bool{}
	for _, c := range FailureClasses {
		if !c.Valid() || seen[c] {
			t.Errorf("class %q is invalid or listed twice", c)
		}
		seen[c] = true
	}
	if len(seen) != 14 {
		t.Errorf("%d classes, want the 14 a person tells apart by what fixes them", len(seen))
	}
	if FailureClass("").Valid() || FailureClass("broken").Valid() {
		t.Error("a class this build does not name reads as valid")
	}
}

// A CLASS IS READ OFF AN ERROR'S OWN TYPES where no layer decided it, and a
// class a layer decided wins over them; an error that fits none has none,
// rather than a guess.
func TestAnErrorIsClassedByWhatItIs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"nothing", nil, ""},
		{"an address the guard refuses", fmt.Errorf("dial: %w", httpx.ErrNotPublic), FailBlockedAddress},
		{"a command not on PATH", &exec.Error{Name: "npx", Err: exec.ErrNotFound}, FailCommandNotFound},
		{"a host not found", &net.DNSError{Err: "no such host", Name: "mcp.example.invalid", IsNotFound: true}, FailDNS},
		{"a certificate nobody signed", x509.UnknownAuthorityError{}, FailTLS},
		{"a refused connection", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, FailUnreachable},
		{"a dial that failed otherwise", &net.OpError{Op: "dial", Err: errors.New("no route")}, FailUnreachable},
		{"the startup timeout", fmt.Errorf("connect: %w", context.DeadlineExceeded), FailUnreachable},
		{"no protocol version in common", &jsonrpc.Error{Code: sdk.CodeUnsupportedProtocolVersion}, FailOldProtocol},
		{"the SDK's own refusal", errors.New(`calling "initialize": unsupported protocol version: "2024-11-05"`), FailOldProtocol},
		{"anything else", errors.New("the server sent a list it could not page"), ""},
		{"a decided class over the cause's", classify(FailCredentialRejected, fmt.Errorf("x: %w", syscall.ECONNRESET)), FailCredentialRejected},
		{"a decided class wrapped further", fmt.Errorf("engine: %w", classify(FailExited, errors.New("EOF"))), FailExited},
	}
	for _, tc := range cases {
		if got := ClassOf(tc.err); got != tc.want {
			t.Errorf("%s: ClassOf = %q, want %q", tc.name, got, tc.want)
		}
	}
	// A CLASSED ERROR SAYS WHAT IT SAID: the wrapper changes no sentence.
	inner := errors.New("mcp: server \"x\" failed to connect: 401 Unauthorized")
	if got := classify(FailCredentialRejected, inner).Error(); got != inner.Error() {
		t.Errorf("a classed error reads %q, want %q", got, inner.Error())
	}
}

// A REMOTE SERVER'S START IS CLASSED BY WHAT IT ANSWERED, read off the answer
// rather than out of the SDK's sentence: the status, whether a 401 challenged
// for a Bearer token the request did not carry, a page of HTML, a 400 about
// the protocol's version.
func TestARemoteStartIsClassedByWhatTheServerAnswered(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		status  int
		header  map[string]string
		body    string
		headers map[string]string // the spec's
		want    FailureClass
	}{
		{name: "a sign-in it asks for", status: 401,
			header: map[string]string{"WWW-Authenticate": `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`},
			want:   FailNeedsSignIn},
		{name: "a token it refuses", status: 401,
			header:  map[string]string{"WWW-Authenticate": `Bearer error="invalid_token"`},
			headers: map[string]string{"Authorization": "Bearer not-a-real-token"}, want: FailCredentialRejected},
		{name: "a 401 with no challenge", status: 401, want: FailCredentialRejected},
		{name: "a scope missing", status: 403, want: FailMissingPermission},
		{name: "no endpoint", status: 404, want: FailNoEndpoint},
		{name: "a retired SSE address", status: 405, want: FailNoEndpoint},
		{name: "slow down", status: 429, want: FailRateLimited},
		{name: "a gateway whose server is down", status: 503, want: FailUnreachable},
		{name: "an old server", status: 400, body: "Bad Request: Unsupported protocol version (supported versions: 2024-11-05)", want: FailOldProtocol},
		{name: "a 400 about something else", status: 400, body: "Bad Request: missing body", want: FailNotMCP},
		{name: "a login page", status: 200, header: map[string]string{"Content-Type": "text/html; charset=utf-8"}, body: "<html>Sign in</html>", want: FailNotMCP},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			spec := httpSpec("vendor", srv.URL)
			spec.Headers = tc.headers
			c, err := connect(t.Context(), spec, discardLogger())
			if err == nil {
				_, _ = c.close(context.Background())
				t.Fatal("a server that answered no handshake was connected")
			}
			if got := ClassOf(err); got != tc.want {
				t.Errorf("ClassOf(%v) = %q, want %q", err, got, tc.want)
			}
		})
	}
}

// AN ADDRESS NOTHING ANSWERS AT, AND A CERTIFICATE NOBODY SIGNED, are classed
// from the dial: no answer was given to read.
func TestARemoteStartNothingAnsweredIsClassedFromTheDial(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + listener.Addr().String() + "/mcp"
	_ = listener.Close()
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(tlsServer.Close)
	for _, tc := range []struct {
		name, url string
		want      FailureClass
	}{
		{"a port nothing listens on", closed, FailUnreachable},
		{"a certificate nobody signed", tlsServer.URL + "/mcp", FailTLS},
	} {
		c, err := connect(t.Context(), httpSpec("vendor", tc.url), discardLogger())
		if err == nil {
			_, _ = c.close(context.Background())
			t.Fatalf("%s: connected", tc.name)
		}
		if got := ClassOf(err); got != tc.want {
			t.Errorf("%s: ClassOf(%v) = %q, want %q", tc.name, err, got, tc.want)
		}
	}
}

// A LOCAL SERVER'S START IS CLASSED BY WHAT BECAME OF ITS PROCESS: a command
// that could not be started, a process that exited before it answered, and
// one that never answered in time, which the engine ended itself and so is no
// exit of the server's own.
func TestALocalStartIsClassedByWhatBecameOfItsProcess(t *testing.T) {
	t.Parallel()
	missing := Spec{Name: "absent", Command: "crewlet-test-no-such-command", StartupTimeout: 5 * time.Second}
	nowhere := Spec{Name: "nowhere", Command: "/nonexistent/crewlet/server", StartupTimeout: 5 * time.Second}
	exits := helperSpec(t, "exits", "serve", map[string]string{helperExitEnv: "3"})
	exits.StartupTimeout = 10 * time.Second
	mute := helperSpec(t, "mute", "mute", nil)
	mute.StartupTimeout = 300 * time.Millisecond
	for _, tc := range []struct {
		name string
		spec Spec
		want FailureClass
	}{
		{"a command not on PATH", missing, FailCommandNotFound},
		{"a path to nothing", nowhere, FailCommandNotFound},
		{"a process that exits before it answers", exits, FailExited},
		{"a process that never answers", mute, FailUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := connect(t.Context(), tc.spec, discardLogger())
			if err == nil {
				_, _ = c.close(context.Background())
				t.Fatal("connected")
			}
			if got := ClassOf(err); got != tc.want {
				t.Errorf("ClassOf(%v) = %q, want %q", err, got, tc.want)
			}
		})
	}
}

// A SERVER'S HEALTH KEEPS ITS LAST FAILURE AFTER IT IS BACK, counts the
// restart, and says when the server last answered: "it is up now" and "it went
// down a minute ago" are both true, and a person sees a server that flaps
// only from the second.
func TestAServersHealthKeepsItsLastFailureAndCountsItsRestarts(t *testing.T) {
	t.Parallel()
	b, _ := supervisedBridge(t, Supervision{})
	spec, starts := countedSpec(t, "crashy", "serve", map[string]string{helperExitAfterEnv: "1"})
	change, err := b.Add(t.Context(), spec)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h, _ := b.Health("crashy")
	if !h.Serving || h.Restarts != 0 || h.LastFailure != nil || h.LastAnswer.IsZero() || h.Since.IsZero() {
		t.Fatalf("a server just started reads %+v: serving since its start, answered by its "+
			"listing, never restarted and never failed", h)
	}
	if _, err := change.Added[0].Call(t.Context(), map[string]any{"q": "x"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the restarted child", func() bool { return startsIn(starts) == 2 && serving(b, "crashy") })
	h, _ = b.Health("crashy")
	if h.Restarts != 1 {
		t.Errorf("restarts = %d after one death, want 1", h.Restarts)
	}
	if h.LastFailure == nil || h.LastFailure.Class != FailExited ||
		!strings.Contains(h.LastFailure.Said, "exit status") || h.LastFailure.At.IsZero() {
		t.Errorf("last failure = %+v, want the death, classed as an exit, kept while it serves", h.LastFailure)
	}
	if h.Class != "" || h.Down != "" {
		t.Errorf("a serving server reads down: %+v", h)
	}
	if !h.Since.After(h.LastFailure.At) && !h.Since.Equal(h.LastFailure.At) {
		t.Errorf("serving since %s, before the death at %s it came back from", h.Since, h.LastFailure.At)
	}
}

// A RESTART ASKED FOR BEFORE A SESSION BEGAN RESTARTS IT, AT ONCE AND ONCE.
// The request is an instant: a session that began after it is already what
// was asked for, so the same request read twice, or late, restarts nothing
// more. And a restart somebody asked for is nobody's failure.
func TestARestartAskedBeforeASessionBeganRestartsItOnce(t *testing.T) {
	t.Parallel()
	b, rec := supervisedBridge(t, Supervision{})
	before := time.Now().Add(-time.Second)
	spec, starts := countedSpec(t, "steady", "serve", nil)
	if _, err := b.Add(t.Context(), spec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := b.RestartBefore("steady", before, "asked"); got != RestartCurrent {
		t.Fatalf("a request from before the server started = %q, want %q", got, RestartCurrent)
	}
	asked := time.Now()
	if got := b.RestartBefore("steady", asked, "restarted at the request of jane.doe"); got != RestartedNow {
		t.Fatalf("a request after the session began = %q, want %q", got, RestartedNow)
	}
	eventually(t, "the restarted server", func() bool {
		h, _ := b.Health("steady")
		return startsIn(starts) == 2 && h.Serving && h.Since.After(asked)
	})
	if got := b.RestartBefore("steady", asked, "asked again"); got != RestartCurrent {
		t.Errorf("the same request read again = %q, want %q", got, RestartCurrent)
	}
	h, _ := b.Health("steady")
	if h.Restarts != 1 || h.LastFailure != nil {
		t.Errorf("after a requested restart: %+v; want one restart and no failure", h)
	}
	if !rec.has("server_restart_requested") || rec.has("server_exited") {
		t.Errorf("the restart was logged as %v, want as requested and never as a death",
			rec.all())
	}
	if n := startsIn(starts); n != 2 {
		t.Errorf("started %d times, want 2", n)
	}
	if got := b.RestartBefore("nobody", asked, "asked"); got != RestartAbsent {
		t.Errorf("a server the bridge does not run = %q, want %q", got, RestartAbsent)
	}
}

// A RESTART OF A SERVER THAT IS DOWN TRIES IT NOW AND STARTS ITS SCHEDULE
// AGAIN. The schedule's base here is an hour, so a server that comes back
// within the test came back because it was asked, not on its own.
func TestARestartOfADownServerTriesItNow(t *testing.T) {
	t.Parallel()
	b, _ := supervisedBridge(t, Supervision{RetryFailedStarts: true, RetryBase: time.Hour})
	spec, starts := countedSpec(t, "late", "serve", map[string]string{helperFailStartsEnv: "1"})
	spec.StartupTimeout = 10 * time.Second
	if _, err := b.Add(t.Context(), spec); err == nil {
		t.Fatal("a start that exits at once was reported started")
	}
	h, _ := b.Health("late")
	if h.Serving || h.Class != FailExited || h.LastFailure == nil || h.Since.IsZero() {
		t.Fatalf("a start that failed reads %+v; want down since it failed, classed as an exit", h)
	}
	if got := b.RestartBefore("late", h.Since.Add(-time.Second), "asked"); got != RestartCurrent {
		t.Errorf("a request from before it went down = %q, want %q", got, RestartCurrent)
	}
	if got := b.RestartBefore("late", time.Now(), "asked"); got != RetriedNow {
		t.Fatalf("a request after it went down = %q, want %q", got, RetriedNow)
	}
	eventually(t, "the server, tried now", func() bool { return startsIn(starts) == 2 && serving(b, "late") })
	h, _ = b.Health("late")
	if h.Restarts != 1 || h.LastFailure == nil || h.LastFailure.Class != FailExited {
		t.Errorf("after coming up on the asked attempt: %+v; want one restart and the start "+
			"that failed kept", h)
	}
}
