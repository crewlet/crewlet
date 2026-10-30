package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
)

// answer is one request's status and its JSON error code, retried while the
// listener is still coming up.
func answer(t *testing.T, method, url string) (int, string) {
	t.Helper()
	// A POOL OF THIS TEST'S OWN, for [httpxtest]'s reason.
	probe := httpxtest.Pool(t)
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		res, err := probe.Do(req)
		if err == nil {
			var body map[string]any
			_ = json.NewDecoder(res.Body).Decode(&body)
			_ = res.Body.Close()
			code, _ := body["error"].(string)
			return res.StatusCode, code
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// bound reports whether something is listening on a port of this host.
func bound(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// A NODE WITH api.public BINDS TWO SOCKETS, each serving its own routes and
// answering 404 for the other's, and its stop closes both. The routes' split is
// the API's own (see internal/api's listener cases); what this holds is that a
// running node binds the public handler at all, on the address the file names.
func TestANodeWithAPublicListenerBindsTwoSockets(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

	e := testEngine(t)
	boot := bootstrapFor(t, reservePort(t))
	boot.API.Public = config.APIPublic{Host: "127.0.0.1", Port: reservePort(t)}
	surface, err := serveNodeLogged(t, boot, e, log)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	if surface.public == nil {
		surface.stop(context.Background(), logging.Get("test"))
		t.Fatal("api.public is set and the node bound no public listener")
	}
	admin := "http://127.0.0.1:" + strconv.Itoa(boot.API.Port)
	public := "http://127.0.0.1:" + strconv.Itoa(boot.API.Public.Port)

	if status, _ := answer(t, http.MethodGet, admin+"/health"); status != http.StatusOK {
		t.Errorf("GET /health on api.port = %d, want 200", status)
	}
	if status, code := answer(t, http.MethodGet, public+"/health"); status != http.StatusNotFound || code != "no_route" {
		t.Errorf("GET /health on the public listener = %d %q, want the 404 no_route", status, code)
	}
	// An unsigned delivery reaches the webhook edge's own refusal there,
	// and the 404 on api.port.
	if status, code := answer(t, http.MethodPost, public+"/webhooks/github"); status == http.StatusNotFound {
		t.Errorf("POST /webhooks/github on the public listener = %d %q: the edge is not served there",
			status, code)
	}
	if status, code := answer(t, http.MethodPost, admin+"/webhooks/github"); status != http.StatusNotFound || code != "no_route" {
		t.Errorf("POST /webhooks/github on api.port = %d %q, want the 404 no_route", status, code)
	}
	if !strings.Contains(logged.String(), "public_addr=127.0.0.1:"+strconv.Itoa(boot.API.Public.Port)) {
		t.Errorf("the node did not say where its public routes are served:\n%s", logged.String())
	}

	surface.stop(context.Background(), logging.Get("test"))
	for name, port := range map[string]int{"api.port": boot.API.Port, "api.public.port": boot.API.Public.Port} {
		if bound(port) {
			t.Errorf("%s is still listening after the node stopped", name)
		}
	}
}

// AN ARTIFACT LISTENER IS A THIRD SOCKET, serving the artifact origin and
// nothing the API serves: a probe there is an empty 404, not the API's JSON
// one, and its stop closes it with the others. The routes themselves are
// internal/artifactserve's to hold; what this holds is that a running node
// binds the origin's handler at all, on the address the file names, and says
// where and in which mode.
func TestANodeWithAnArtifactListenerBindsAThirdSocket(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

	e := testEngine(t)
	boot := bootstrapFor(t, reservePort(t))
	port := reservePort(t)
	boot.API.Artifacts = config.APIArtifacts{Host: "127.0.0.1", Port: port,
		ExternalURL: "http://127.0.0.1:" + strconv.Itoa(port)}
	surface, err := serveNodeLogged(t, boot, e, log)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	if surface.artifacts == nil {
		surface.stop(context.Background(), logging.Get("test"))
		t.Fatal("api.artifacts is set and the node bound no artifact listener")
	}
	origin := "http://127.0.0.1:" + strconv.Itoa(port)
	if status, code := answer(t, http.MethodGet, origin+"/health"); status != http.StatusNotFound || code != "" {
		t.Errorf("GET /health on the artifact listener = %d %q, want an empty 404", status, code)
	}
	if status, _ := answer(t, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(boot.API.Port)+"/health"); status != http.StatusOK {
		t.Errorf("GET /health on api.port = %d, want 200", status)
	}
	if !strings.Contains(logged.String(), "msg=api_artifacts_listening addr=127.0.0.1:"+strconv.Itoa(port)) ||
		!strings.Contains(logged.String(), "mode=shared") {
		t.Errorf("the node did not say where and how it serves artifacts:\n%s", logged.String())
	}
	surface.stop(context.Background(), logging.Get("test"))
	if bound(port) {
		t.Error("api.artifacts.port is still listening after the node stopped")
	}
}

// AN ARTIFACT BIND THAT FAILS TAKES THE NODE DOWN, for the public listener's
// reason: a dashboard whose every artifact frame finds nothing listening would
// look healthy to every probe.
func TestAnUnbindableArtifactPortFailsTheNode(t *testing.T) {
	t.Parallel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	e := testEngine(t)
	boot := bootstrapFor(t, reservePort(t))
	port := taken.Addr().(*net.TCPAddr).Port
	boot.API.Artifacts = config.APIArtifacts{Host: "127.0.0.1", Port: port,
		ExternalURL: "http://127.0.0.1:" + strconv.Itoa(port)}
	surface, err := serveNode(t, boot, e)
	if err == nil {
		surface.stop(context.Background(), logging.Get("test"))
		t.Fatal("binding an artifact port already in use reported success")
	}
	if bound(boot.API.Port) {
		t.Error("api.port is still listening although the node failed to start")
	}
}

// THE ARTIFACT ORIGIN IS BOUND BEFORE THE BOOT, with api.port, by the rule the
// origin is later served by ([api.ArtifactsListener]): an ingress node binds it
// and answers it as a starting origin answers (an empty 503 for a route it will
// serve, never the boot surface's JSON) and says where in `api_bound`; a node
// without ingress binds nothing there, whatever its file sets.
func TestTheArtifactListenerIsBoundBeforeTheBoot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		roles []string
		want  bool
	}{
		{"an ingress node", nil, true},
		{"a node without ingress", []string{"data", "seats", "workers"}, false},
	} {
		var logged bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))
		boot := bootstrapFor(t, reservePort(t))
		boot.Node.Roles = tc.roles
		port := reservePort(t)
		addr := "127.0.0.1:" + strconv.Itoa(port)
		boot.API.Artifacts = config.APIArtifacts{Host: "127.0.0.1", Port: port, ExternalURL: "http://" + addr}
		var asked []string
		bind := func(ctx context.Context, a string) (net.Listener, error) {
			asked = append(asked, a)
			return bindReserved(ctx, a)
		}
		listeners, err := bindListeners(t.Context(), boot, "node-0", bind, log)
		if err != nil {
			t.Fatalf("%s: bindListeners: %v", tc.name, err)
		}
		if got := listeners.artifacts != nil; got != tc.want || slices.Contains(asked, addr) != tc.want {
			listeners.close(context.Background(), log)
			t.Fatalf("%s: bound the artifact listener: %t (binds asked %v), want %t",
				tc.name, got, asked, tc.want)
		}
		if tc.want {
			if status, code := answer(t, http.MethodGet, "http://"+addr+"/t/ticket/index.html"); status != http.StatusServiceUnavailable || code != "" {
				t.Errorf("%s: a frame while the node starts = %d %q, want the origin's empty 503",
					tc.name, status, code)
			}
			if !strings.Contains(logged.String(), "artifacts_addr="+addr) {
				t.Errorf("%s: api_bound did not say where the artifact origin is bound:\n%s",
					tc.name, logged.String())
			}
		}
		listeners.close(context.Background(), log)
		if tc.want && bound(port) {
			t.Errorf("%s: api.artifacts.port is still listening after a failed boot closed it", tc.name)
		}
	}
}

// A PUBLIC BIND THAT FAILS TAKES THE NODE'S LISTENER DOWN WITH IT. A node that
// served its dashboard while every webhook found nothing listening would look
// healthy to every probe and be deaf to every integration. Both are bound
// before the boot, so the failure is found before anything slow has run.
func TestAnUnbindablePublicPortFailsTheNode(t *testing.T) {
	t.Parallel()
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })

	e := testEngine(t)
	boot := bootstrapFor(t, reservePort(t))
	boot.API.Public = config.APIPublic{Host: "127.0.0.1", Port: taken.Addr().(*net.TCPAddr).Port}
	surface, err := serveNode(t, boot, e)
	if err == nil {
		surface.stop(context.Background(), logging.Get("test"))
		t.Fatal("binding a public port already in use reported success")
	}
	if !strings.Contains(err.Error(), "bind") {
		t.Errorf("the error does not say what failed: %v", err)
	}
	if bound(boot.API.Port) {
		t.Error("api.port is still listening although the node failed to start")
	}
}

// A STOP THAT RUNS BEFORE THE LISTENER IS SERVED STILL LEAVES NOTHING BOUND.
// listenAPI serves from a goroutine, and Shutdown closes only the listeners
// Serve has registered, so a stop the scheduler ran first — a failed public
// bind's, a moment after api.port was bound — returned with the socket still
// accepting connections. This holds that window open: a socket nothing has
// served yet, stopped, and the Serve that starts after the stop.
func TestAListenerStoppedBeforeItIsServedIsClosed(t *testing.T) {
	t.Parallel()
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := socket.Addr().(*net.TCPAddr).Port
	l := &apiListener{server: &http.Server{ReadHeaderTimeout: apiReadHeaderTimeout}, socket: socket}

	shutdownListener(t.Context(), l, "api", logging.Get("test"))
	if bound(port) {
		t.Error("the socket is still listening after its stop returned")
	}
	if err := l.server.Serve(socket); !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("a Serve that started after the stop = %v, want %v", err, http.ErrServerClosed)
	}
}

// A SEATS NODE SERVES ITS OWN BOXES THEIR TOOLS ON LOOPBACK, WITH NO FLEET
// ADDRESS. A node without the ingress role binds api.port for its probes, and
// a `direct` box is a process of its host: so the node hands that box its own
// listener on loopback, builds its bridge from that address alone, and serves
// the bridge's route there, beside the probes. Before, it derived no address
// and mounted no route, so a seat pool with no CREWLET_MCP_BRIDGE_URL — a
// satellite on a private host, a Kubernetes seat pool with no route in —
// refused every executor launch. It still binds no public listener, whatever
// the shared file says: the network reaches its probes alone (internal/api's
// Probes holds the off-host half).
//
// Mutation: derive no address on a node without ingress and the engine builds
// no bridge; mount no route on the probe surface and the box's connect meets a
// 404; leave the bridge off the surface and the stop waits out a held poll.
func TestASeatsNodeServesItsOwnBoxesTheirToolsOnLoopback(t *testing.T) {
	t.Parallel()
	var logged lockedLog
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

	port := reservePort(t)
	boot := bootstrapFor(t, port)
	boot.API.Public = config.APIPublic{Host: "127.0.0.1", Port: reservePort(t)}
	boot.Node.Roles = []string{"seats"}
	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		t.Fatal(err)
	}
	// BOUND FIRST, as runEngine binds: the engine is built from the address
	// the bind derived.
	listeners, err := bindListeners(t.Context(), boot, nodeID, bindReserved, log)
	if err != nil {
		t.Fatalf("bindListeners: %v", err)
	}
	host := "http://127.0.0.1:" + strconv.Itoa(port)
	if got := listeners.boxHostURL(boot, nodeID); got != host {
		listeners.close(context.Background(), log)
		t.Fatalf("a seats node's direct boxes dial %q, want its own listener %q", got, host)
	}
	// AND NO FLEET ADDRESS: an environment that names none.
	e := testEngineWith(t, engine.Options{
		BoxHostURL: listeners.boxHostURL(boot, nodeID), Environment: config.MapSource{},
	})
	bridge := e.Bridge()
	if bridge == nil {
		listeners.close(context.Background(), log)
		t.Fatal("a seats node with its own listener built no bridge, so every launch is refused")
	}
	if got := bridge.Reaches(true); got != host {
		t.Errorf("a direct box is handed %q, want the node's own listener", got)
	}
	if got := bridge.Reaches(false); got != "" {
		t.Errorf("a box off the host is handed %q, with no fleet address set", got)
	}
	// THE PROCESS'S CHAT HUB, as `crewlet run` attaches it beside the engine
	// on every node, a seats node serving no API included.
	chatLive := api.NewEngineChatLive(t.Context(), e)
	t.Cleanup(func() { chatLive.Stop(context.WithoutCancel(t.Context())) })
	surface, err := serveBound(t, boot, e, reconcilerFor(t, boot, e), chatLive, listeners, log)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	if surface == nil {
		t.Fatalf("a seats node bound nothing:\n%s", logged.String())
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })
	if surface.public != nil {
		t.Error("a seats node bound a public listener; only an ingress node serves the network the bridge")
	}
	if surface.bridge != bridge {
		t.Error("the surface holds no bridge, so its stop waits out every held poll for notes")
	}
	if !strings.Contains(logged.String(), "box_host_url="+host) {
		t.Errorf("the node did not say where its own boxes dial it:\n%s", logged.String())
	}

	// THE BOX'S OWN SESSION, opened as a launch opens it for a direct box,
	// reached on loopback at the address the box is handed.
	seat := &org.Role{Name: "Dev", DeclaredHandle: "dev"}
	url := bridge.Open(t.Context(), &mcpbridge.Session{
		RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Dev", OnHost: true,
		Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil).
			ForTurn(&turnctx.Turn{RunID: "run-1", Seat: seat,
				Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}}}),
	})
	if !strings.HasPrefix(url, host+mcpbridge.PathPrefix) {
		t.Fatalf("the session was minted at %q, want the node's own listener", url)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "claude", Version: "test"}, nil).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: httpxtest.Pool(t)}, nil)
	if err != nil {
		t.Fatalf("a direct box could not reach its tools on its node's own listener: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	if _, err := session.ListTools(ctx, nil); err != nil {
		t.Errorf("the box's tools/list over its node's own listener: %v", err)
	}
	// A TOKEN THE FLEET DID NOT SIGN is refused there, before anything runs.
	if status, code := answer(t, http.MethodPost, host+mcpbridge.PathPrefix+"not-a-token"); status != http.StatusUnauthorized ||
		code != string(httpjson.CodeInvalidToken) {
		t.Errorf("POST %snot-a-token on api.port = %d %q, want the bridge's 401",
			mcpbridge.PathPrefix, status, code)
	}
	if status, _ := answer(t, http.MethodGet, host+"/health"); status != http.StatusOK {
		t.Errorf("GET /health on api.port = %d, want the liveness probe's 200", status)
	}
}

// lockedLog is a log destination the node's own goroutines may write while a
// case reads it.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// api.port 0 IS STILL THE HARD OFF SWITCH on a node without ingress, whatever
// the shared file says about api.public: the satellite the deployment guide
// starts with `-api-port 0` binds nothing, so a second process on the host
// keeps every port for the ingress node.
func TestASeatsNodeWithNoAPIPortBindsNothing(t *testing.T) {
	t.Parallel()
	publicPort := reservePort(t)
	bridge := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"),
		BaseURL:  "http://127.0.0.1:" + strconv.Itoa(publicPort),
	})
	e := testEngineWithBridge(t, bridge)
	boot := bootstrapFor(t, 0)
	boot.API.Public = config.APIPublic{Host: "127.0.0.1", Port: publicPort}
	boot.Node.Roles = []string{"data", "seats", "workers"}

	// NOTHING BOUND AT ALL, asked of the one path every listener is opened
	// through rather than of the public port after the fact — which this
	// case holds itself, so nothing else can be listening there either.
	listeners, err := bindListeners(t.Context(), boot, e.Node().ID(), bindNothing(t),
		logging.Get("test"))
	if err != nil || listeners != nil {
		listeners.close(context.Background(), logging.Get("test"))
		t.Fatalf("bindListeners on api.port 0 = %v, %v; want nothing bound", listeners, err)
	}
	surface, err := serveAPI(t.Context(), boot, e, nil, nil, nil, nil, nil, listeners,
		engine.DeepHost{}, logging.Get("test"))
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	if surface != nil {
		surface.stop(context.Background(), logging.Get("test"))
		t.Fatal("a node with api.port 0 built an HTTP surface")
	}
}

// THE PUBLIC LISTENER CLOSES FIRST, AND api.port ANSWERS UNTIL IT HAS. The
// probes are on api.port and an orchestrator reads them for as long as the
// process lives, while the public listener may spend its whole grace on a
// bridge session that a build keeps quiet: closed in the other order, a
// liveness probe fails on a node doing exactly what it should.
func TestTheProbesOutliveThePublicListener(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	log := logging.Get("test")

	held := make(chan struct{})
	release := make(chan struct{})
	publicServer, publicAddr, err := listenAPI(ctx, bindTCP, "127.0.0.1:0", http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			close(held)
			<-release
			w.WriteHeader(http.StatusOK)
		}), log)
	if err != nil {
		t.Fatal(err)
	}
	adminServer, adminAddr, err := listenAPI(ctx, bindTCP, "127.0.0.1:0", http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), log)
	if err != nil {
		t.Fatal(err)
	}
	surface := &httpSurface{server: adminServer, public: publicServer}

	// A request the public listener is still serving, like a bridge
	// session, so its shutdown waits.
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+publicAddr+"/mcp/x", nil)
		if res, doErr := httpxtest.Pool(t).Do(req); doErr == nil {
			_ = res.Body.Close()
		}
	}()
	<-held
	stopped := make(chan struct{})
	go func() {
		surface.stop(context.Background(), log)
		close(stopped)
	}()

	// The public listener has begun to close...
	port := func(addr string) int {
		_, p, _ := net.SplitHostPort(addr)
		n, _ := strconv.Atoi(p)
		return n
	}
	deadline := time.Now().Add(apiShutdownGrace)
	for bound(port(publicAddr)) {
		if time.Now().After(deadline) {
			t.Fatal("the public listener never began to close")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// ...and while it waits on its request, api.port still answers.
	if status, _ := answer(t, http.MethodGet, "http://"+adminAddr+"/health"); status != http.StatusOK {
		t.Errorf("api.port answered %d while the public listener was closing, want 200", status)
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(3 * apiShutdownGrace):
		t.Fatal("the surface never stopped")
	}
	if bound(port(adminAddr)) {
		t.Error("api.port is still listening after the surface stopped")
	}
}

// A HELD POLL FOR A RUN'S NOTES DOES NOT SPEND THE LISTENER'S GRACE. A coding
// CLI's poll is held for longer than the grace while no note waits
// (internal/api/mcpbridge), so a stop that left it to the listener would spend
// the whole grace on a request with nothing to answer and log the cut; the
// surface releases every held poll before it closes a listener — as a 503 the
// CLI pauses on, never an empty answer it would ask again at once — and the
// CLI polls again wherever the seat goes next.
//
// Mutation: drop the bridge's shutdown from the surface's stop, and this stop
// takes the whole grace.
func TestAHeldNotePollDoesNotSpendTheListenersGrace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	log := logging.Get("test")
	bridge := bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"), BaseURL: "http://x"})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	mux.Handle(mcpbridge.SteerPattern, bridge.SteerHandler())
	publicServer, publicAddr, err := listenAPI(ctx, bindTCP, "127.0.0.1:0", mux, log)
	if err != nil {
		t.Fatal(err)
	}
	inbox := &quietInbox{reached: make(chan struct{})}
	seat := &org.Role{Name: "Dev", DeclaredHandle: "dev"}
	url := bridge.Open(ctx, &mcpbridge.Session{
		RunID: "run-1", Launch: "job-1", Handle: "dev", Role: "Dev", Steer: inbox,
		Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil).
			ForTurn(&turnctx.Turn{RunID: "run-1", Seat: seat,
				Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}}}),
	})
	if url == "" {
		t.Fatal("no endpoint was minted")
	}
	token := url[strings.LastIndex(url, "/")+1:]
	polled := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			"http://"+publicAddr+mcpbridge.PathPrefix+token+"/steer/notes", nil)
		res, doErr := httpxtest.Pool(t).Do(req)
		if doErr != nil {
			polled <- 0
			return
		}
		_ = res.Body.Close()
		polled <- res.StatusCode
	}()
	select {
	case <-inbox.reached:
	case <-time.After(apiShutdownGrace):
		t.Fatal("the poll never reached the run's inbox")
	}
	surface := &httpSurface{public: publicServer, bridge: bridge}
	began := time.Now()
	surface.stop(context.Background(), log)
	if took := time.Since(began); took >= apiShutdownGrace/2 {
		t.Errorf("the surface took %s to stop with a poll held, against a grace of %s", took, apiShutdownGrace)
	}
	if status := <-polled; status != http.StatusServiceUnavailable {
		t.Errorf("the held poll was answered %d, want 503: a refusal its poller pauses on", status)
	}
}

// quietInbox is a run's inbox with no note for ever, saying when it is first
// polled.
type quietInbox struct {
	once    sync.Once
	reached chan struct{}
}

func (*quietInbox) Take(context.Context, string) string { return "" }

func (i *quietInbox) Wait(context.Context, int) ([]mcpbridge.SteerNote, <-chan struct{}) {
	i.once.Do(func() { close(i.reached) })
	return nil, nil
}

func (*quietInbox) Read(context.Context, []string) error { return nil }

func (*quietInbox) Close() {}

// A DIRECT BOX DIALS THE LISTENER ITS NODE BOUND, ON LOOPBACK, ON EVERY NODE
// THAT RUNS SEATS. The `direct` cell's run is a process of this host, so the
// node's own socket is an address it can always reach, and that is what lets
// the quickstart's company — one node, one direct cell, no
// CREWLET_MCP_BRIDGE_URL — and a seat pool with no fleet address run their
// first turn. The listener is the one the box's routes are served on:
// api.public where an ingress node binds one, api.port otherwise — which a
// node without the ingress role serves its own host alone, beside its probes.
// Only a node running seats launches a box, so an ingress node with no seat
// derives none.
//
// Mutation: derive from api.port whatever the public listener, and the public
// row dials a port the bridge is not served on; derive none on a node without
// ingress, and the seat pool's row refuses every launch.
func TestADirectBoxDialsTheListenerItsNodeBound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		roles  []string
		public bool
		want   string // "api", "public" or ""
	}{
		{"an ingress seats node on api.port", []string{"data", "ingress", "seats", "workers"}, false, "api"},
		{"an ingress seats node with a public listener", []string{"data", "ingress", "seats", "workers"}, true, "public"},
		{"a seats node without ingress", []string{"data", "seats", "workers"}, false, "api"},
		{"a seat pool with a public listener in its file", []string{"seats"}, true, "api"},
		{"an ingress node that runs no seat", []string{"data", "ingress", "workers"}, false, ""},
		{"a node without ingress that runs no seat", []string{"data", "workers"}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot := bootstrapFor(t, reservePort(t))
			boot.Node.Roles = tc.roles
			if tc.public {
				boot.API.Public = config.APIPublic{Host: "127.0.0.1", Port: reservePort(t)}
			}
			listeners, err := bindListeners(t.Context(), boot, "node-0", bindReserved,
				logging.Get("test"))
			if err != nil {
				t.Fatalf("bindListeners: %v", err)
			}
			t.Cleanup(func() { listeners.close(context.Background(), logging.Get("test")) })
			want := ""
			switch tc.want {
			case "api":
				want = "http://127.0.0.1:" + strconv.Itoa(boot.API.Port)
			case "public":
				want = "http://127.0.0.1:" + strconv.Itoa(boot.API.Public.Port)
			}
			if got := listeners.boxHostURL(boot, "node-0"); got != want {
				t.Errorf("a direct box dials %q, want %q", got, want)
			}
		})
	}
}

// A NODE WITHOUT INGRESS HANDS ITS BOXES ITS LISTENER ONLY WHERE LOOPBACK
// REACHES IT. Such a node serves its boxes' routes to this host's loopback
// alone, so the address it derives has to be one a call from the host arrives
// on from loopback: a listener on every interface or on a loopback address.
// One bound to a single address that is not loopback is dialled at that
// address, and the call arrives from it — refused as from off the host — so
// such a node derives none, and its direct boxes dial the fleet's address. An
// ingress node serves the routes to anyone, so its single address is dialled
// as it is.
//
// Mutation: derive on every bind for a node without ingress and the private
// address row hands a box an address every call to which is refused.
func TestANodeWithoutIngressHandsItsBoxesOnlyAListenerLoopbackReaches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		ingress bool
		addr    net.Addr
		want    string
	}{
		{"every IPv4 interface", false, &net.TCPAddr{IP: net.IPv4zero, Port: 8001}, "http://127.0.0.1:8001"},
		{"every IPv6 interface", false, &net.TCPAddr{IP: net.IPv6unspecified, Port: 8001}, "http://127.0.0.1:8001"},
		{"IPv4 loopback", false, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8001}, "http://127.0.0.1:8001"},
		{"IPv6 loopback", false, &net.TCPAddr{IP: net.IPv6loopback, Port: 8001}, "http://[::1]:8001"},
		{"one private address", false, &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 8001}, ""},
		{"one private address, ingress", true, &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 8001}, "http://10.0.0.5:8001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			boot := bootstrapFor(t, 8001)
			boot.Node.Roles = []string{"data", "seats"}
			if tc.ingress {
				boot.Node.Roles = append(boot.Node.Roles, "ingress")
			}
			listeners := &nodeListeners{api: &apiListener{socket: boundAt{addr: tc.addr}}}
			if got := listeners.boxHostURL(boot, "node-0"); got != tc.want {
				t.Errorf("a direct box dials %q, want %q", got, tc.want)
			}
		})
	}
}

// boundAt is a socket that says it is bound at addr, for a case about what a
// node derives from where it bound rather than about the bind.
type boundAt struct {
	net.Listener
	addr net.Addr
}

func (b boundAt) Addr() net.Addr { return b.addr }

// LOOPBACK FOR A LISTENER ON EVERY INTERFACE, ITS OWN ADDRESS OTHERWISE. A
// socket bound to every interface answers on loopback, which no network
// policy between the host and itself can refuse; one bound to a single
// address answers only there, so loopback would be refused.
func TestALoopbackURLIsWhereThisHostDialsTheSocket(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		addr net.Addr
		want string
	}{
		{"every IPv4 interface", &net.TCPAddr{IP: net.IPv4zero, Port: 8080}, "http://127.0.0.1:8080"},
		{"every IPv6 interface", &net.TCPAddr{IP: net.IPv6unspecified, Port: 8080}, "http://127.0.0.1:8080"},
		{"no address at all", &net.TCPAddr{Port: 8080}, "http://127.0.0.1:8080"},
		{"one private address", &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 8080}, "http://10.0.0.5:8080"},
		{"IPv6 loopback", &net.TCPAddr{IP: net.IPv6loopback, Port: 8080}, "http://[::1]:8080"},
		{"not TCP", &net.UnixAddr{Name: "/run/crewlet.sock", Net: "unix"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := loopbackURL(tc.addr); got != tc.want {
				t.Errorf("loopbackURL(%v) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}
