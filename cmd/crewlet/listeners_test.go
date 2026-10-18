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
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/runtoken"
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

// A SEATS NODE SERVES ITS PROBES AND NOTHING ELSE, even with a bridge and a
// public listener in the shared file: a box reaches its seats' sessions
// through the nodes that serve the API, which relay its call here over the
// broker, so this node binds no public listener and mounts no bridge route.
func TestASeatsNodeServesNoBridgeRoute(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

	publicPort := reservePort(t)
	bridge := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"),
		BaseURL:  "http://127.0.0.1:" + strconv.Itoa(publicPort),
	})
	e := testEngineWithBridge(t, bridge)
	boot := bootstrapFor(t, reservePort(t))
	boot.API.Public = config.APIPublic{Host: "127.0.0.1", Port: publicPort}
	boot.Node.Roles = []string{"seats"}

	surface, err := serveNodeLogged(t, boot, e, log)
	if err != nil {
		t.Fatalf("serveAPI: %v", err)
	}
	if surface == nil {
		t.Fatalf("a seats node bound nothing:\n%s", logged.String())
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })

	if surface.public != nil {
		t.Error("a seats node bound a public listener; only an ingress node serves the bridge's route")
	}
	private := "http://127.0.0.1:" + strconv.Itoa(boot.API.Port)
	if status, _ := answer(t, http.MethodGet, private+"/health"); status != http.StatusOK {
		t.Errorf("GET /health on api.port = %d, want the liveness probe's 200", status)
	}
	// The guard exempts the bridge's prefix wholesale, so a 404 here is the
	// router finding no bridge, where a mounted one answers 401.
	if status, _ := answer(t, http.MethodPost, private+mcpbridge.PathPrefix+"not-a-token"); status != http.StatusNotFound {
		t.Errorf("POST %snot-a-token on api.port = %d, want 404: a seats node "+
			"mounts no bridge route", mcpbridge.PathPrefix, status)
	}
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
