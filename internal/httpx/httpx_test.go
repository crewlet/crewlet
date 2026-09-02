package httpx_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// ONE TRANSPORT, SHARED. This is the whole point, and the thing a
// well-meaning "give each client its own tuned transport" change would undo:
// a per-client clone is strictly WORSE than sharing the stdlib default, since
// each keeps a private pool and N clients against one host hold N pools of
// idle connections while reusing nothing between them.
func TestEveryClientSharesOneTransport(t *testing.T) {
	t.Parallel()
	a := httpx.Client(time.Second)
	b := httpx.Client(30 * time.Second)

	if a.Transport != b.Transport {
		t.Fatal("two clients got two transports: each keeps its own connection " +
			"pool, so callers to one host reuse nothing between them")
	}
	if a.Transport != httpx.Transport() {
		t.Error("Client did not use the shared transport")
	}
}

// THE POOL IS RAISED off the stdlib default of 2 per host, process-wide,
// which is what every client leaving Transport nil was silently getting.
func TestTheSharedTransportKeepsAWarmPoolPerHost(t *testing.T) {
	t.Parallel()
	transport, ok := httpx.Transport().(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want an *http.Transport", httpx.Transport())
	}
	if transport.MaxIdleConnsPerHost != httpx.MaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d",
			transport.MaxIdleConnsPerHost, httpx.MaxIdleConnsPerHost)
	}
	if transport.MaxIdleConnsPerHost == http.DefaultMaxIdleConnsPerHost {
		t.Error("the stdlib default of 2 churns a TLS handshake per call past " +
			"the second concurrent turn against one endpoint")
	}
}

// AND IT IS A CLONE, not a hand-built transport. Building one silently drops
// ProxyFromEnvironment, which is how a deployment behind a corporate proxy
// stops reaching anything with no error that says so.
func TestTheSharedTransportKeepsTheProcessProxyAndHTTP2(t *testing.T) {
	t.Parallel()
	transport, ok := httpx.Transport().(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T", httpx.Transport())
	}
	if transport.Proxy == nil {
		t.Error("the transport carries no proxy function")
	}
	if !transport.ForceAttemptHTTP2 {
		t.Error("HTTP/2 was lost, which is the multiplexing vendor endpoints rely on")
	}
	// The clone must not have disturbed the default itself: every caller
	// that has not been migrated still uses it.
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("DefaultTransport = %T", http.DefaultTransport)
	}
	if base.MaxIdleConnsPerHost != 0 {
		t.Errorf("http.DefaultTransport was mutated: MaxIdleConnsPerHost = %d",
			base.MaxIdleConnsPerHost)
	}
}

// The timeout is per caller, because it is a property of what is being asked
// for rather than of the network.
func TestEachClientKeepsItsOwnTimeout(t *testing.T) {
	t.Parallel()
	if got := httpx.Client(5 * time.Second).Timeout; got != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", got)
	}
	if got := httpx.Client(0).Timeout; got != 0 {
		t.Errorf("Timeout = %v, want none", got)
	}
}

// CLOSING ONE CLIENT'S IDLE CONNECTIONS CLOSES EVERY CLIENT'S.
//
// http.Client.CloseIdleConnections forwards to its transport, and here that is
// the process's ONE transport — so there is no such thing as "the idle
// connections this client holds". Three providers had a Close that believed
// otherwise, and calling one on a config swap would have dropped the warm
// connections of every other provider, all seven vendor clients, every remote
// MCP server and the sandbox control plane.
//
// The property is not a defect to fix — it is the unavoidable other side of
// one shared pool, and the reason nothing may call the method. Pinned here so
// the next caller finds a failing test rather than a plausible-looking
// cleanup.
func TestClosingIdleConnectionsIsProcessWide(t *testing.T) {
	// Not parallel: it closes the shared transport's idle connections, which
	// is precisely what it is here to demonstrate.
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	get := func(c *http.Client) {
		t.Helper()
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("close body: %v", err)
		}
	}

	victim := httpx.Client(0)
	get(victim)
	get(victim)
	if got := conns.Load(); got != 1 {
		t.Fatalf("two sequential calls opened %d connections, want the pool to "+
			"have reused one", got)
	}

	// A DIFFERENT client — another provider, another vendor — closes what it
	// would reasonably believe are its own idle connections.
	httpx.Client(0).CloseIdleConnections()

	get(victim)
	if got := conns.Load(); got != 2 {
		t.Errorf("conns = %d after another client's CloseIdleConnections; if this "+
			"is 1 the pool is no longer shared, and every doc in this package "+
			"saying it is has become wrong", got)
	}
}
