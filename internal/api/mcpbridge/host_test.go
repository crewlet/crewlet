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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE HOST A CALL MAY NAME: any, on a token that resolves, for every session
// but one held to this host's loopback; for that one, a loopback host name or
// a container runtime's name for the engine host, and nothing else. Every case
// here goes over a real listener on 127.0.0.1, so a call arrives on loopback
// exactly as one does through a reverse proxy or tunnel on the engine host.

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

// connectAs connects an MCP client the way a coding agent's own client would,
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
// or tunnel on the engine host — Caddy, cloudflared and ngrok each as it comes
// — connects to the node on 127.0.0.1 and passes the public name on, and that
// is the hop every call of an e2b executor behind one takes. The token in the
// path is the route's credential, so a call carrying the run's own is served
// whatever it names, and one carrying a token another keyring signed is
// refused 401 under the same names, its tool never run.
//
// Mutation: judge every session's Host as a held one's (call loopbackRefusal
// for every opening), and the public names are refused; admit a token that
// does not verify, and the forged call is served.
func TestASeatsCallIsServedUnderTheHostAProxyPassesOn(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	endpoint := f.open(t)
	if endpoint == "" {
		t.Fatal("no endpoint was minted")
	}
	// FORGED: the same run and job, the same shape, signed under another
	// keyring than the fleet's.
	other := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "another-key"), BaseURL: f.server.URL,
	})
	// MOUNTED, as Open requires of a bridge before it mints anything, and
	// served by nothing: only its signature is wanted.
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

// A SESSION HELD TO LOOPBACK ADMITS A LOOPBACK NAME OR A RUNTIME'S NAME FOR THE
// ENGINE HOST, AND NO OTHER. Its token is worth nothing off this host's
// loopback ([mcpbridge.Session.Loopback]), and a page in a browser on the host,
// under a name its author points at 127.0.0.1, sends its requests to the
// loopback under that name (DNS rebinding) — so a held run's leaked token must
// not be spendable by one either. The runtime's names are admitted because
// Docker Desktop and Podman's machine on macOS and Windows deliver a container
// box's call to the host's loopback under them. A refusal is the route's own
// 401, saying no more than any refused token does, and the run's tool never
// runs.
//
// Mutation: drop the Host from the hold, and every rebound name is served;
// drop the runtime's names, and a held container run on Docker Desktop has
// none of its tools.
func TestALoopbackSessionAdmitsOnlyLoopbackAndTheRuntimesNames(t *testing.T) {
	t.Parallel()
	s := serve(t)
	endpoint := s.bridge.Open(t.Context(), &mcpbridge.Session{RunID: "deep-1", Launch: "job-1",
		Surface: s.surface, Caller: nobody{bound: true}, Loopback: true})
	if endpoint == "" {
		t.Fatal("the bridge opened no session")
	}
	port := portOf(t, s.server.URL)
	admitted := 0
	for host, want := range map[string]bool{
		"127.0.0.1:" + port:                                true,
		"localhost:" + port:                                true,
		"engine.localhost:" + port:                         true,
		"[::1]:" + port:                                    true,
		"host.docker.internal:" + port:                     true,
		"host.containers.internal:" + port:                 true,
		"Host.Docker.Internal:" + port:                     true,
		"host.docker.internal":                             true,
		"crewlet.example.com":                              false,
		"rebound.example.com:" + port:                      false,
		"host.docker.internal.rebound.example.com:" + port: false,
		"docker.internal:" + port:                          false,
	} {
		if !want {
			if got := postAs(t, s.server, endpoint, host); got != http.StatusUnauthorized {
				t.Errorf("Host %s on loopback: a held session's call answered %d, want 401", host, got)
			}
			continue
		}
		sess, err := connectAs(t, s.server, endpoint, host)
		if err != nil {
			t.Errorf("Host %s on loopback: a held session's call was refused: %v", host, err)
			continue
		}
		if got := text(callTool(t, sess, "probe", nil)); got != string(iam.Unknown) {
			t.Errorf("Host %s: the call was decided as %q, want the caller's %q", host, got, iam.Unknown)
		}
		_ = sess.Close()
		admitted++
	}
	if ran := int(s.surface.calls.Load()); ran != admitted {
		t.Errorf("the held run's tool ran %d times, want %d: once for each admitted host and "+
			"never for a refused one", ran, admitted)
	}
}

// aliasedBox is a box whose network is not the engine host's, as a
// container's is: all [sandbox.EngineAddress] asks of it is the name the host
// answers to inside it.
type aliasedBox struct{ sandbox.Sandbox }

func (aliasedBox) EngineHost() string { return "host.docker.internal" }

// A BASE THE REHOSTING MOVES IS A BASE THE HOLD COVERS. A container box is
// handed the runtime's name for the engine host in place of a loopback base
// ([sandbox.EngineAddress]), and a run on the engine host on such a base is
// held to loopback ([mcpbridge.Bridge.LoopbackBase]), so that the name widens
// nothing. The two must read "loopback" alike: read narrower here, as it was,
// a container run on a base such as http://engine.localhost:8000 was moved to
// the runtime's name and held to nothing.
//
// Mutation: read a base as loopback only for `localhost` or an address, and
// the `.localhost` name and the upper-cased one go red.
func TestABaseTheRehostingMovesIsABaseTheHoldCovers(t *testing.T) {
	t.Parallel()
	for base, loopback := range map[string]bool{
		"http://127.0.0.1:8000":             true,
		"http://127.0.0.2:8000":             true,
		"http://localhost:8000":             true,
		"http://LOCALHOST:8000":             true,
		"http://engine.localhost:8000":      true,
		"http://[::1]:8000":                 true,
		"https://engine.example.com":        false,
		"http://localhost.example.com:8000": false,
		"http://10.0.0.5:8000":              false,
		"http://host.docker.internal:8000":  false,
	} {
		b := bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"), BaseURL: base})
		if got := b.LoopbackBase(); got != loopback {
			t.Errorf("%s: LoopbackBase = %v, want %v", base, got, loopback)
		}
		if moved := sandbox.EngineAddress(aliasedBox{}, base) != base; moved != loopback {
			t.Errorf("%s: a container box is handed it moved=%v, want %v — the rehosting and "+
				"the hold disagree about what loopback is", base, moved, loopback)
		}
	}
}
