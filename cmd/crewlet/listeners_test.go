package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
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

// THE BRIDGE IS A PUBLIC ROUTE ON EVERY NODE. A seats node without ingress
// serves its probes on api.port and its tool bridge beside them, so with
// api.public set it binds that address for the bridge alone and api.port
// carries the probes alone — one file for every role, and one bridge address
// rule across the fleet.
func TestASeatsNodeServesItsBridgeOnThePublicListener(t *testing.T) {
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
		t.Fatalf("a seats node with a bridge bound nothing:\n%s", logged.String())
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })

	public := "http://127.0.0.1:" + strconv.Itoa(publicPort)
	if status, _ := answer(t, http.MethodPost, public+mcpbridge.PathPrefix+"not-a-token"); status != http.StatusUnauthorized {
		t.Errorf("POST %snot-a-token on the public listener = %d, want the bridge's own 401",
			mcpbridge.PathPrefix, status)
	}
	if status, _ := answer(t, http.MethodGet, public+"/health"); status == http.StatusOK {
		t.Error("GET /health answered on the public listener; the probes stay on api.port")
	}
	private := "http://127.0.0.1:" + strconv.Itoa(boot.API.Port)
	if status, _ := answer(t, http.MethodGet, private+"/health"); status != http.StatusOK {
		t.Errorf("GET /health on api.port = %d, want the liveness probe's 200", status)
	}
	// The guard exempts the bridge's prefix wholesale, so a 404 here is the
	// router finding no bridge on api.port, where a mounted one answers 401.
	if status, _ := answer(t, http.MethodPost, private+mcpbridge.PathPrefix+"not-a-token"); status != http.StatusNotFound {
		t.Errorf("POST %snot-a-token on api.port = %d, want 404: the bridge is "+
			"served on the public listener only", mcpbridge.PathPrefix, status)
	}
	if !strings.Contains(logged.String(), "public_addr=127.0.0.1:"+strconv.Itoa(publicPort)) {
		t.Errorf("the node did not name the bridge's public listener:\n%s", logged.String())
	}
}

// api.port 0 IS STILL THE HARD OFF SWITCH on a node without ingress, whatever
// the shared file says about api.public: the satellite the deployment guide
// starts with `-api-port 0` binds no tool bridge on the public port either, so
// a second process on the host keeps that port for the ingress node.
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
	listeners, err := bindListeners(t.Context(), boot, e.Node().ID(), true, bindNothing(t),
		logging.Get("test"))
	if err != nil || listeners != nil {
		listeners.close(context.Background(), logging.Get("test"))
		t.Fatalf("bindListeners on api.port 0 = %v, %v; want nothing bound", listeners, err)
	}
	surface, err := serveAPI(t.Context(), boot, e, nil, nil, nil, nil, listeners, logging.Get("test"))
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
