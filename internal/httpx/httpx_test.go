package httpx_test

import (
	"net/http"
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
