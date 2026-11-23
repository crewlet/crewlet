package mcpbridge_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// THE HOST A CALL MAY NAME: any, on a token that resolves. Every case runs over
// a real listener on 127.0.0.1, so a call arrives on loopback as one through a
// proxy on the engine host does.

// namingHost delivers every request under host, as a reverse proxy or tunnel
// that passes the public name on delivers a box's call to the node.
type namingHost struct {
	host string
	next http.RoundTripper
}

func (n namingHost) RoundTrip(r *http.Request) (*http.Response, error) {
	named := r.Clone(r.Context())
	named.Host = n.host
	return n.next.RoundTrip(named)
}

// connectAs connects an MCP client the way a harness's own client would,
// over server's own pool, every request it sends naming host.
func connectAs(t *testing.T, server *httptest.Server, endpoint, host string) (*mcp.ClientSession, error) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "coding-agent", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: namingHost{host: host, next: server.Client().Transport}},
	}, nil)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, nil
}

// postAs is one bare JSON-RPC ping to endpoint naming host, and its status.
func postAs(t *testing.T, server *httptest.Server, endpoint, host string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("post as %s: %v", host, err)
	}
	_ = res.Body.Close()
	return res.StatusCode
}

// portOf is the port of a server's URL.
func portOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("server address %q: %v", raw, err)
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		t.Fatalf("the server listens on %s, and every case here needs a call to arrive on loopback", u.Host)
	}
	return u.Port()
}

// A SEAT'S CALL IS SERVED UNDER WHATEVER HOST IT ARRIVES WITH. A reverse proxy
// or tunnel on the engine host (Caddy, cloudflared, ngrok as each comes)
// reaches the node on 127.0.0.1 passing the public name on, the hop a box's
// call takes where the fleet's address is that proxy. The token in the path is the credential:
// the run's own is served whatever host it names, and one another keyring
// signed is refused 401 under the same names, its tool never run.
//
// Mutation: refuse a call whose Host is not a loopback name, and the public
// names are refused; admit a token that does not verify, and the forged call
// is served.
func TestASeatsCallIsServedUnderTheHostAProxyPassesOn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	endpoint := f.open(t)
	if endpoint == "" {
		t.Fatal("no endpoint was minted")
	}
	// FORGED: the same run, job and shape, signed under another keyring.
	other := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "another-key"), BaseURL: f.server.URL,
	})
	// MOUNTED, as Open requires before it mints, and served by nothing: only
	// its signature is wanted.
	_ = other.Handler()
	forged := other.Open(t.Context(), &mcpbridge.Session{
		RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Engineer", Surface: emptySurface(),
	})
	t.Cleanup(func() { other.Close("run-1") })
	if forged == "" || forged == endpoint {
		t.Fatalf("no forged endpoint to try: %q", forged)
	}

	port := portOf(t, f.server.URL)
	hosts := []string{
		"crewlet.example.com", "crewlet.example.com:443", "rebound.example.com:" + port,
		"127.0.0.1:" + port, "host.docker.internal:" + port,
	}
	for _, host := range hosts {
		if got := postAs(t, f.server, forged, host); got != http.StatusUnauthorized {
			t.Errorf("Host %s: a forged token's call answered %d, want 401", host, got)
		}
		sess, err := connectAs(t, f.server, endpoint, host)
		if err != nil {
			t.Errorf("Host %s: a seat's call on loopback was refused: %v", host, err)
			continue
		}
		if res := callTool(t, sess, "read_page", map[string]any{}); res.IsError {
			t.Errorf("Host %s: the seat's tool failed: %s", host, text(res))
		}
		_ = sess.Close()
	}
	if ran := len(f.byName["read_page"].seen()); ran != len(hosts) {
		t.Errorf("the seat's tool ran %d times, want once for each of the %d hosts its run's "+
			"token was presented under and never for the forged one", ran, len(hosts))
	}
}
