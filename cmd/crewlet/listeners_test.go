package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

// listening reports whether socket — the very socket a listener holds, never
// the port it was bound to — is still open, and so still able to complete a
// connection.
//
// NOT A DIAL OF ITS PORT, which is what this asked until a loaded run failed
// on it. A port its socket has let go is anybody's: the kernel hands it to the
// next bind that asks for any port and finds it, and the suite binds such
// ports by the thousand, every package's cases in one network namespace. So a
// dial after a stop reaches whatever holds the port by then, and says nothing
// of the listener that stopped: in round 140 of
// [TestAStoppedListenerAcceptsNothing], with internal/engine and
// internal/code/host binding beside it, a dial of 127.0.0.1:35409 was accepted
// after its listener's Serve had returned, which closes that listener's socket
// before it returns. The socket answers for itself: Go marks it closed as its
// Close begins, and Close returns only once the descriptor is closed, which
// takes the listening socket out of the kernel's lookups — nothing can connect
// to it after that.
func listening(t *testing.T, socket net.Listener) bool {
	t.Helper()
	conn, ok := socket.(syscall.Conn)
	if !ok {
		t.Fatalf("%T is not a socket that can say whether it is open", socket)
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatalf("ask %s whether it is open: %v", socket.Addr(), err)
	}
	switch err := raw.Control(func(uintptr) {}); {
	case err == nil:
		return true
	case errors.Is(err, net.ErrClosed):
		return false
	default:
		t.Fatalf("ask %s whether it is open: %v", socket.Addr(), err)
		return false
	}
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
	for name, socket := range map[string]net.Listener{"api.port": surface.server.socket,
		"api.public.port": surface.public.socket} {
		if listening(t, socket) {
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
	if listening(t, surface.artifacts.socket) {
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
	api := reserveSocket(t)
	boot := bootstrapFor(t, api.Addr().(*net.TCPAddr).Port)
	port := taken.Addr().(*net.TCPAddr).Port
	boot.API.Artifacts = config.APIArtifacts{Host: "127.0.0.1", Port: port,
		ExternalURL: "http://127.0.0.1:" + strconv.Itoa(port)}
	surface, err := serveNode(t, boot, e)
	if err == nil {
		surface.stop(context.Background(), logging.Get("test"))
		t.Fatal("binding an artifact port already in use reported success")
	}
	if listening(t, api) {
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
		listeners, err := bindListeners(t.Context(), boot, "node-0", false, bind, log)
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
		if tc.want && listening(t, listeners.artifacts.socket) {
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
	api := reserveSocket(t)
	boot := bootstrapFor(t, api.Addr().(*net.TCPAddr).Port)
	boot.API.Public = config.APIPublic{Host: "127.0.0.1", Port: taken.Addr().(*net.TCPAddr).Port}
	surface, err := serveNode(t, boot, e)
	if err == nil {
		surface.stop(context.Background(), logging.Get("test"))
		t.Fatal("binding a public port already in use reported success")
	}
	if !strings.Contains(err.Error(), "bind") {
		t.Errorf("the error does not say what failed: %v", err)
	}
	if listening(t, api) {
		t.Error("api.port is still listening although the node failed to start")
	}
}

// A STOP THAT RUNS BEFORE THE LISTENER IS SERVED STILL LEAVES NOTHING BOUND,
// AND STILL WAITS FOR THE SERVE IT WAS STARTED WITH. listenAPI serves from a
// goroutine, and Shutdown closes only the listeners Serve has registered, so a
// stop the scheduler ran first — a failed public bind's, a moment after
// api.port was bound — returned with the socket still accepting connections.
// This holds that window open: a socket nothing has served yet, stopped, and
// the Serve that starts after the stop. The socket is closed while that Serve
// has still not run, and the stop returns only once it has, since the
// goroutine serving a listener is its stop's to end.
func TestAListenerStoppedBeforeItIsServedIsClosed(t *testing.T) {
	t.Parallel()
	socket, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	l := &apiListener{server: &http.Server{ReadHeaderTimeout: apiReadHeaderTimeout},
		socket: socket, served: served}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		shutdownListener(context.Background(), l, "api", logging.Get("test"))
	}()
	for deadline := time.Now().Add(10 * time.Second); listening(t, socket); {
		if time.Now().After(deadline) {
			t.Fatal("the socket is still listening while its stop waits for Serve")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-stopped:
		t.Fatal("the stop returned before the goroutine serving its socket had run")
	default:
	}
	serveErr := l.server.Serve(socket)
	close(served)
	if !errors.Is(serveErr, http.ErrServerClosed) {
		t.Errorf("a Serve that started after the stop = %v, want %v", serveErr, http.ErrServerClosed)
	}
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the stop never returned once the Serve it waited for had")
	}
	if listening(t, socket) {
		t.Error("the socket is still listening after its stop returned")
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
	publicListener, publicAddr, err := listenAPI(ctx, bindTCP, "127.0.0.1:0", http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			close(held)
			<-release
			w.WriteHeader(http.StatusOK)
		}), log)
	if err != nil {
		t.Fatal(err)
	}
	adminListener, adminAddr, err := listenAPI(ctx, bindTCP, "127.0.0.1:0", http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), log)
	if err != nil {
		t.Fatal(err)
	}
	surface := &httpSurface{server: adminListener, public: publicListener}

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
	deadline := time.Now().Add(apiShutdownGrace)
	for listening(t, publicListener.socket) {
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
	if listening(t, adminListener.socket) {
		t.Error("api.port is still listening after the surface stopped")
	}
}

// THE SHUTDOWN FINISHES ITS GIT REQUESTS WITH THE LISTENER UP, after the
// engine's drain and before the listener closes. A clone or a push the close
// cut would be begun again from nothing on a peer — the listener's own grace
// is sized for a REST read — and a git request still running reads the estate
// and the object store the teardown closes, so the teardown waits too.
func TestTheShutdownFinishesGitBeforeTheListenerCloses(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	log := logging.Get("test")
	admin, addr, err := listenAPI(ctx, bindTCP, "127.0.0.1:0", http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }), log)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var steps []string
	step := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, s)
	}
	taken := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(steps)
	}
	finishing, finished := make(chan struct{}), make(chan struct{})
	surface := &httpSurface{server: admin, drainGit: func(context.Context) error {
		step("git")
		close(finishing)
		<-finished
		return nil
	}}
	stopped := make(chan struct{})
	go func() {
		shutdown(context.Background(), &recordingEngine{step: step}, surface, log)
		close(stopped)
	}()

	select {
	case <-finishing:
	case <-time.After(10 * time.Second):
		t.Fatal("the shutdown never waited for the git requests it was serving")
	}
	// While the git requests finish: the engine has drained and is not torn
	// down, and the listener still answers.
	if got := taken(); !slices.Equal(got, []string{"drain", "git"}) {
		t.Errorf("the git requests were waited for after %v, want after the drain alone", got)
	}
	if status, _ := answer(t, http.MethodGet, "http://"+addr+"/health"); status != http.StatusOK {
		t.Errorf("the listener answered %d while its git requests finished, want 200", status)
	}

	close(finished)
	select {
	case <-stopped:
	case <-time.After(3 * apiShutdownGrace):
		t.Fatal("the shutdown never ended once its git requests had")
	}
	if got := taken(); !slices.Equal(got, []string{"drain", "git", "stop"}) {
		t.Errorf("the shutdown ran %v, want the drain, the git requests and then the teardown", got)
	}
	if listening(t, admin.socket) {
		t.Error("api.port is still listening after the shutdown")
	}
}

// A CONNECTION THAT NEVER SENT A REQUEST DOES NOT HOLD A LISTENER'S STOP
// (#315). net/http counts one as active until it is five seconds old — the
// whole shutdown grace — so a stop with a client's unused connection open
// (one its pool dialled and never used, one a browser opened ahead of a
// request) spent its grace on it and logged api_shutdown_failed, the warning
// for a stuck request. The stop closes it, as it closes an idle one.
//
// Mutation: register no close of the unused connections, and the stop runs
// to its deadline.
func TestAConnectionThatSentNothingDoesNotHoldTheStop(t *testing.T) {
	t.Parallel()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	listener, addr, err := listenAPI(t.Context(), bindTCP, "127.0.0.1:0", ok, logging.Get("test"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownListener(context.Background(), listener, "api", logging.Get("test")) })
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// TAKEN BY THE SERVER, as the stop will find it.
	for deadline := time.Now().Add(10 * time.Second); listener.unused.count() == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the server never took the connection")
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(t.Context(), apiShutdownGrace/2)
	defer cancel()
	if err := listener.server.Shutdown(ctx); err != nil {
		t.Errorf("the listener's stop with a connection open that sent nothing: %v — the connection held it", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("the unused connection reads %v after the stop, want it closed (EOF)", err)
	}
}

// recordingEngine is a [drainer] that records each half of its stop.
type recordingEngine struct{ step func(string) }

func (e *recordingEngine) Drain(context.Context) { e.step("drain") }
func (e *recordingEngine) Stop(context.Context)  { e.step("stop") }

// A STOPPED LISTENER HAS STOPPED SERVING AND ACCEPTS NOTHING, HOWEVER SOON
// AFTER ITS START IT STOPS. [http.Server.Shutdown] closes only the listeners
// Serve has registered and waits only for the Serve calls that registered one,
// and listenAPI starts Serve on a goroutine, so a stop that won that race — a
// public bind that failed tearing api.port down at once is exactly that stop —
// returned with the goroutine still to run and the socket listening until it
// did: the kernel completed connections a node that had reported its failed
// start would never answer. So [shutdownListener] closes the socket itself and
// then waits for that goroutine.
//
// Each round is a fresh listener stopped at once and then checked three ways.
// Its Serve has RETURNED, because the goroutine listenAPI starts belongs to
// the stop: a stop that does not wait for it fails this in round 0 at any
// GOMAXPROCS, Serve having yet to return in over 96% of such stops on 2 or 4
// cores and in every one on 1 (Go 1.27.2, measured). Its socket is CLOSED,
// which is what refuses a person's dial — asked of the socket itself
// ([listening]), never by dialling its address: a loaded run's dial of a
// stopped listener's port was accepted by another socket that had taken the
// port, after that listener's Serve had returned and closed it. The stop's
// own close of the socket is not this case's to hold: it closes the socket in
// every round whether or not the stop waits, and
// [TestAListenerStoppedBeforeItIsServedIsClosed] holds it by keeping the
// window open rather than racing for it. What the socket catches here is a
// stop that does neither, which leaves it to a Serve still to run. And nothing
// is logged at warn or above, read once every round's Serve has returned, so
// no round's goroutine can log after the check. A round is a bind and a stop,
// so the 500 take about a tenth of a second under the race detector.
func TestAStoppedListenerAcceptsNothing(t *testing.T) {
	t.Parallel()
	var logged lockedBuffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for round := range 500 {
		listener, addr, err := listenAPI(t.Context(), bindTCP, "127.0.0.1:0", ok, log)
		if err != nil {
			t.Fatal(err)
		}
		shutdownListener(context.Background(), listener, "api", log)
		select {
		case <-listener.served:
		default:
			t.Fatalf("round %d: the stop of %s returned while the goroutine serving it was still running",
				round, addr)
		}
		if listening(t, listener.socket) {
			t.Fatalf("round %d: %s is still listening after its listener stopped", round, addr)
		}
	}
	if logged.String() != "" {
		t.Errorf("stopping a listener logged a failure:\n%s", logged.String())
	}
}
