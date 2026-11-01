package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/whsec"
)

// A SIGNALLED NODE DRAINS WITH ITS PROBES UP.
//
// This is `crewlet run` itself, stopped the way an orchestrator stops it: a
// real SIGTERM to a real process, with a real turn in flight. A signal handler
// cannot be exercised from inside the process that installed it without
// taking the test binary with it, so the node runs in a CHILD: the test binary
// re-executes itself, the child calls [runEngine] — on the socket the parent
// reserved for its port ([drainProbeSocketFD]) — and the parent signals it and
// watches from outside.
//
// The turn is held in flight by the model it is waiting on, which answers
// only when the parent says so. That is what makes the drain last long enough
// to observe, and it is a real turn on the real path: a signed GitLab delivery
// on the webhook edge, the notification service, the seat's mailbox, and the
// CLI backend running the model's CLI ([heldCLI]) as a child of the node — for
// the seat's first turn's onboarding pass, the one call a turn makes on the
// seat's own model before its executor's run is launched and the turn parks.
const (
	drainProbeEnv     = "CREWLET_RUN_DRAIN_PROBE"
	drainProbeArgsEnv = "CREWLET_RUN_DRAIN_PROBE_ARGS"

	// Distinctive, and specifically not 0, 1 or 2: a test binary that ran
	// its tests exits 0 or 1 and a panic exits 2, so an exit code that
	// could be any of those would not say that run() itself returned.
	drainProbeExit = 7

	// drainProbeToken is the operator credential the child's Tier A
	// accepts, so a write is refused for draining rather than for having
	// no token.
	drainProbeToken = "drain-probe-token-long-enough"

	// drainProbeSecret is a well-formed Standard-Webhooks key, the only
	// shape the GitLab route verifies with.
	drainProbeSecret = "whsec_ZHJhaW4tcHJvYmUtc2lnbmluZy1rZXktMzItYnl0ZXM="

	// drainProbeBudget is how long any one wait here gets: a node booted
	// under the race detector on a machine running every other package.
	// Generous because it costs nothing when the condition is met; a wait
	// that times out is a staging bug, since nothing here races the clock.
	drainProbeBudget = 90 * time.Second
)

// parentDeathFD is the pipe the parent holds the write end of, inherited by
// the child as its first extra descriptor.
//
// THE CHILD'S LIFETIME IS THE PARENT'S, and it has to be enforced by the
// kernel rather than by the parent's good manners. Everything that reaps this
// child — the command's context, the cleanup — runs only when the test binary
// unwinds, and the two ways it does not unwind are ordinary: `go test
// -timeout` firing, and a panic in any other case in package main. The child
// is `crewlet run`: it has installed a signal handler and blocks for ever, so
// a leak holds the API port, a Turso database and an embedded NATS server
// until somebody notices.
//
// A pipe rather than syscall.SysProcAttr's Pdeathsig, which is linux-only:
// the release matrix is linux AND darwin, and a guard that covers half of it
// is the asymmetry internal/procgroup exists so that nobody writes twice. The
// kernel closes the write end when the parent dies however it dies, and the
// read below returns EOF.
const parentDeathFD = 3

// drainProbeSocketFD is the listening socket the parent reserved for the
// node's api.port, inherited by the child as its second extra descriptor.
//
// HANDED OVER rather than found free and bound again, for [reservePort]'s
// reason — and a child is where that matters most: it starts a whole process,
// loads its Tier A and resolves its node before its bind, and a port the parent
// had let go is one the rest of the suite can take in that time. The parent
// closes its own copy once the child has started, so the socket is the node's
// alone and closes when the node closes it.
const drainProbeSocketFD = 4

// exitWithTheParent ends this process the moment the parent's pipe closes.
func exitWithTheParent() {
	pipe := os.NewFile(parentDeathFD, "parent-death")
	if pipe == nil {
		fmt.Fprintln(os.Stderr, "probe: no parent-death pipe on fd", parentDeathFD)
		os.Exit(5)
	}
	go func() {
		// EOF, or any error: either way the parent is gone or the pipe
		// is unusable, and a child that cannot tell must not be the one
		// that outlives it.
		_, _ = io.Copy(io.Discard, pipe)
		fmt.Fprintln(os.Stderr, "probe: the parent went away")
		os.Exit(6)
	}()
}

// runDrainProbe is the child of every case here that runs `crewlet run` in a
// process of its own ([startNodeChild]). It never returns.
func runDrainProbe() {
	exitWithTheParent()
	// THE PROCESS'S OWN SINK, on stderr, because the parent reads the order
	// of the shutdown from it. JSON is set by the flag as well, below: run
	// applies the flags over whatever is installed.
	logging.Configure(slog.LevelInfo, logging.FormatJSON, os.Stderr)
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(drainProbeArgsEnv)), &args); err != nil {
		fmt.Fprintln(os.Stderr, "probe: args:", err)
		os.Exit(3)
	}
	inherited := os.NewFile(drainProbeSocketFD, "api-socket")
	socket, err := net.FileListener(inherited)
	// The listener holds a copy of its own, and this descriptor left open
	// would keep the socket listening after the node's stop closed it.
	_ = inherited.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe: no api socket on fd", drainProbeSocketFD, err)
		os.Exit(8)
	}
	// THE ONE BIND THE NODE MAKES THERE is handed the inherited socket; any
	// other address is bound as the binary binds it.
	var once sync.Once
	bind := func(ctx context.Context, addr string) (net.Listener, error) {
		handed := false
		if addr == socket.Addr().String() {
			once.Do(func() { handed = true })
		}
		if handed {
			return socket, nil
		}
		return bindTCP(ctx, addr)
	}
	if err := runEngine(args, os.Stderr, bind); err != nil {
		fmt.Fprintln(os.Stderr, "probe: run:", err)
		os.Exit(4)
	}
	os.Exit(drainProbeExit)
}

func TestASignalledNodeDrainsWithItsProbesUp(t *testing.T) {
	// Not parallel: it boots a whole node in a second process, and a
	// package's parallel cases waiting on the same cores is time taken from
	// the one budget here that matters.
	client := drainProbeClient(t)
	model := newHeldCLI(t)
	forge := newAccountForge(t)
	// THE NODE'S PORT, HELD: see [drainProbeSocketFD].
	port, socket := reserveNodeSocket(t)
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	dir := t.TempDir()
	// THE KEYRING IS PART OF A RUNNABLE TIER A, and this case starts a node
	// from a real file in a real process: every record on every state log
	// is signed under it, so a document without one is a node that refuses
	// to boot rather than a node with one feature off.
	//
	// AND SO IS THE BROKER'S SIZE, beside its store_dir ([brokertest.StoreIn]
	// has why): without one the broker's limit is three quarters of the
	// runner's free disk, and on a small disk this node refuses to boot at
	// whichever state log's ceiling no longer fits.
	boot := writeFile(t, dir, "crewlet.yaml", fmt.Sprintf(`node:
  id: drain-probe
store:
  path: %s
stream:
  store_dir: %s
  store_max_bytes: %d
secrets:
  active_key_id: drain-probe
  keys:
    - id: drain-probe
      material: %s
api:
  host: 127.0.0.1
  port: %d
  external_url: "http://127.0.0.1:%d"
  auth:
    admin: serve
    tokens:
      - id: founder
        token: %s
        grants: [admin]
`, filepath.Join(dir, "crewlet.db"), filepath.Join(dir, "stream"), brokertest.StoreMaxBytes,
		testKeyMaterial(t), port, port, drainProbeToken))
	company := writeFile(t, dir, "company.yaml", fmt.Sprintf(`name: Acme
providers:
  sandbox: {fake: true}
  llm:
%s
%s
integrations:
  gitlab:
    enabled: true
    url: %s
    signing_secret: %s
roles:
  - name: CEO
    handle: ceo
    llm: [held, coder]
    mcp_env:
      gitlab:
        GITLAB_TOKEN: glpat-ceo
turn_engine:
  max_iterations: 1
`, model.entry("held", filepath.Join(dir, "llm")), unreachableCoder("coder", filepath.Join(dir, "coder")),
		forge.URL, drainProbeSecret))

	node := startNodeChild(t, "TestASignalledNodeDrainsWithItsProbesUp", socket,
		[]string{"-config", boot, "-company", company, "-log-format", "json"})
	// The model lets go of its held call BEFORE the child is killed —
	// registered after the child's own cleanup, so LIFO runs it first — or
	// the node's teardown waits on a call the model is still holding.
	t.Cleanup(model.letGo)
	alive := node.alive
	output := node.output

	// UP, with the seat claimed. Readiness first, because a probe is what
	// an orchestrator gates traffic on; the seat second, because a delivery
	// to a seat nobody holds would wait in its mailbox rather than start
	// the turn this case needs in flight.
	eventually(t, "the node to be ready", alive, func() bool {
		status, _ := probe(client, base+"/ready")
		return status == http.StatusOK
	})
	eventually(t, "the seat to be claimed", alive, func() bool {
		_, body := probe(client, base+"/health")
		seats, _ := body["seats"].([]any)
		return slices.Contains(seats, any("ceo"))
	})

	// A TURN IN FLIGHT, started on the real inbound path and held by the
	// model it is waiting on.
	if status := deliver(t, client, base, "delivery-1"); status != http.StatusOK {
		t.Fatalf("the delivery answered %d on a serving node", status)
	}
	select {
	case <-model.entered:
	case <-node.exited:
		t.Fatal("the node exited before its turn reached the model")
	case <-time.After(drainProbeBudget):
		t.Fatal("the turn never reached the model")
	}
	eventually(t, "the turn to count as in flight", alive, func() bool {
		_, body := probe(client, base+"/health")
		inFlight, _ := body["in_flight"].(float64)
		return inFlight >= 1
	})

	if err := node.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signalling the node: %v", err)
	}

	// LIVENESS STAYS 200 THROUGH THE DRAIN, and says what the node is
	// doing. This is the probe the node used to stop answering the moment
	// the signal arrived, because the listener closed before the drain: a
	// refused connection now is that bug, and no amount of waiting fixes it.
	eventually(t, "/health to report the drain", alive, func() bool {
		status, body := probe(client, base+"/health")
		if status == refused {
			t.Fatal("the node stopped answering /health on the signal, with its " +
				"turn still running: an orchestrator would kill it mid-turn")
		}
		return status == http.StatusOK && body["status"] == "shutting_down"
	})
	// And keeps answering, rather than for one lucky request: the turn is
	// still held, so every one of these is inside the drain.
	for range 10 {
		if status, body := probe(client, base+"/health"); status != http.StatusOK {
			t.Fatalf("/health answered %d mid-drain (%v): an orchestrator would "+
				"kill this node in the middle of the turn it is finishing", status, body)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// READINESS IS 503, and says why.
	if status, body := probe(client, base+"/ready"); status != http.StatusServiceUnavailable ||
		body["reason"] != "draining" {
		t.Errorf("/ready answered %d %v mid-drain, want 503 naming the drain", status, body)
	}

	// EVERY ROUTE THAT WOULD START WORK REFUSES, clearly: a second delivery
	// on the webhook edge, and a config write with a valid token.
	if status := deliver(t, client, base, "delivery-2"); status != http.StatusServiceUnavailable {
		t.Errorf("a delivery answered %d mid-drain, want 503", status)
	}
	if status, code := write(t, client, base+"/config"); status != http.StatusServiceUnavailable || code != "draining" {
		t.Errorf("PUT /config answered %d %q mid-drain, want 503 draining", status, code)
	}
	// And a read is still a read — presenting the credential every
	// guarded route now needs.
	if status, _ := probeAs(client, base+"/agents", drainProbeToken); status != http.StatusOK {
		t.Errorf("GET /agents answered %d mid-drain, want 200", status)
	}
	if !alive() {
		t.Fatal("the node exited while its turn was still held: the drain did not wait for it")
	}

	// THE TURN FINISHES, and only then does the node go.
	model.letGo()
	select {
	case <-node.exited:
	case <-time.After(drainProbeBudget):
		t.Fatal("the node did not exit once its turn finished")
	}
	node.exitedWith(t, drainProbeExit)

	// THE LISTENER CLOSED AFTER THE DRAIN AND BEFORE THE TEARDOWN: after,
	// because the probes above were served while the turn ran; before,
	// because both of them read what the teardown closes.
	order := logOrder(output.String(), "engine_draining", "drain_complete", "api_stopped", "engine_stopped")
	if !slices.IsSorted(order) || slices.Contains(order, -1) {
		t.Errorf("the shutdown ran out of order: engine_draining, drain_complete, "+
			"api_stopped and engine_stopped logged at lines %v", order)
	}
}

// reserveNodeSocket holds a loopback port for a node run in a child process,
// and returns it and the listening socket's descriptor to hand the child as
// [drainProbeSocketFD]. See there for why the socket is handed over rather
// than the port found free and bound again.
func reserveNodeSocket(t *testing.T) (int, *os.File) {
	t.Helper()
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve the node's port: %v", err)
	}
	//nolint:errcheck // Listen on a TCP address always yields *TCPAddr.
	port := held.Addr().(*net.TCPAddr).Port
	//nolint:errcheck // Listen on "tcp" always yields a *TCPListener.
	socket, err := held.(*net.TCPListener).File()
	// The copy is the socket now; this listener's descriptor is not needed.
	_ = held.Close()
	if err != nil {
		t.Fatalf("the reserved socket's descriptor: %v", err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	return port, socket
}

// nodeChild is `crewlet run` running in a child process ([runDrainProbe]), on
// the socket the parent reserved for its api.port.
type nodeChild struct {
	cmd    *exec.Cmd
	output *lockedBuffer
	// exited closes once the child has been reaped; waitErr is its exit,
	// and is read only after that.
	exited  chan struct{}
	waitErr error
}

// startNodeChild starts `crewlet run args` in a child, serving on socket, with
// env added to this process's environment. run names the case, for the
// child's command line: TestMain dispatches the child before any case runs,
// so it selects nothing, and is there for whoever reads a process listing.
//
// ON EVERY PATH OUT the child is killed and reaped, and its output logged when
// the case failed. A case holding something the child's teardown waits on
// lets it go in a cleanup registered AFTER this call, which LIFO runs first.
func startNodeChild(t *testing.T, run string, socket *os.File, args []string,
	env ...string,
) *nodeChild {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^"+run+"$")
	cmd.Env = append(append(os.Environ(),
		drainProbeEnv+"=1",
		drainProbeArgsEnv+"="+string(encoded)), env...)
	node := &nodeChild{cmd: cmd, output: &lockedBuffer{}, exited: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = node.output, node.output
	// THE PARENT-DEATH PIPE. See [parentDeathFD]: the child reads this end
	// and exits on EOF, which the kernel produces the moment this process
	// dies, including the ways that run no cleanup.
	death, hold, err := os.Pipe()
	if err != nil {
		t.Fatalf("parent-death pipe: %v", err)
	}
	// fd 3 and fd 4 in the child: [parentDeathFD], [drainProbeSocketFD].
	cmd.ExtraFiles = []*os.File{death, socket}
	// A BOUND ON Wait, so a grandchild holding the inherited output pipe
	// cannot make the reaping goroutine block for ever.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		_ = death.Close()
		_ = hold.Close()
		t.Fatalf("starting the node: %v", err)
	}
	// THIS COPY IS THE CHILD'S NOW. The write end stays open for as long as
	// this process lives, which is the whole mechanism.
	_ = death.Close()
	t.Cleanup(func() { _ = hold.Close() })
	// AND SO IS THE SOCKET, which must close when the node closes it: a
	// copy left open here would keep accepting connections nobody serves
	// once the node has gone, where its stop leaves them refused.
	_ = socket.Close()
	go func() {
		defer close(node.exited)
		node.waitErr = cmd.Wait()
	}()
	// On every path out, so a failed assertion cannot leave a node running.
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-node.exited
		if t.Failed() {
			t.Logf("the node's output:\n%s", node.output.String())
		}
	})
	return node
}

// alive reports whether the child is still running.
func (n *nodeChild) alive() bool {
	select {
	case <-n.exited:
		return false
	default:
		return true
	}
}

// exitedWith fails the case unless the child, which has exited, ended with
// code: [drainProbeExit] is run() returning nil.
func (n *nodeChild) exitedWith(t *testing.T, code int) {
	t.Helper()
	var exit *exec.ExitError
	if !errors.As(n.waitErr, &exit) || exit.ExitCode() != code {
		t.Fatalf("the node ended with %v, want run() to return and the probe to exit %d",
			n.waitErr, code)
	}
}

// heldCLI is the seat's model: a CLI that marks every call it is handed and
// answers none until the test lets it go, and then fails them all at once.
//
// A FAILURE rather than an answer, because the case is about the node that
// waits for the turn and not about the turn: a CLI that exits non-zero ends
// the call as a `server` failure, which the backend does not retry, and the
// one member behind it ([unreachableCoder]) has no CLI to start. The
// onboarding pass fails, which a turn carries on from, and the turn ends at
// its executor's launch however that goes. Nothing in the case turns on how
// the turn ends, only on its ending once the CLI is let go.
//
// /bin/sh UNDER THE `custom` PROFILE rather than this binary re-executed: a
// held call computes nothing, and both of its halves — the mark that a call
// arrived and the file it waits for — are things the parent sees from
// outside, as it sees the node's probes. `custom` because it ships nothing, so
// no vendor's sign-in, sentinels or configuration stands between the engine
// and a CLI that has none of them.
type heldCLI struct {
	t *testing.T
	// marker is the file a call creates on arrival; release is the one the
	// call waits for.
	marker, release string
	// entered closes once a call has arrived.
	entered  chan struct{}
	released sync.Once
}

// heldCLIPoll is how often the parent looks for the marker and a held call for
// the release: fast enough that neither wait is what a case spends its budget
// on, slow enough to cost nothing beside a booting node.
const heldCLIPoll = 50 * time.Millisecond

// newHeldCLI makes the files, and watches for the first call until the case
// ends. The let-go is registered here as well, so a case that fails before it
// lets go itself never leaves a call held through the node's teardown.
func newHeldCLI(t *testing.T) *heldCLI {
	t.Helper()
	dir := t.TempDir()
	m := &heldCLI{
		t:       t,
		marker:  filepath.Join(dir, "entered"),
		release: filepath.Join(dir, "release"),
		entered: make(chan struct{}),
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		tick := time.NewTicker(heldCLIPoll)
		defer tick.Stop()
		for {
			if _, err := os.Stat(m.marker); err == nil {
				close(m.entered)
				return
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-stopped })
	t.Cleanup(m.letGo)
	return m
}

// letGo releases every call held now and every one made after.
func (m *heldCLI) letGo() {
	m.released.Do(func() {
		if err := os.WriteFile(m.release, nil, 0o600); err != nil {
			m.t.Errorf("releasing the held CLI: %v", err)
		}
	})
}

// entry is the providers.llm entry, under key, whose CLI this is, keeping its
// homes under stateDir — indented to sit under `providers: llm:`.
//
// The paths are quoted twice over: single quotes inside the script, so the
// shell reads each as one word, and the whole script with %q, a Go-quoted
// string being a YAML double-quoted scalar.
func (m *heldCLI) entry(key, stateDir string) string {
	script := fmt.Sprintf("touch '%s'; while [ ! -e '%s' ]; do sleep %g; done; "+
		"echo 'the drain probe has no model' >&2; exit 1",
		m.marker, m.release, heldCLIPoll.Seconds())
	return fmt.Sprintf(`    %s:
      harness: custom
      model: claude-drain-probe
      state_dir: %q
      overrides:
        binary: /bin/sh
        complete_args: ["-c", %q]
        model_args: ["--model", "{model}"]
        output: text`, key, stateDir, script)
}

// unreachableCoder is the providers.llm entry, under key, that the seat's
// executor launches on: a claude-code entry, the CLI a run in the fake catalogue
// is played for, whose binary does not exist — so a call on this host that falls
// through to it fails at once rather than starting a real CLI on the machine
// running the suite. Indented to sit under `providers: llm:`.
func unreachableCoder(key, stateDir string) string {
	return fmt.Sprintf(`    %s:
      harness: claude-code
      model: sonnet
      state_dir: %q
      overrides:
        binary: %q`, key, stateDir, filepath.Join(stateDir, "no-cli-here"))
}

// newAccountForge is a GitLab that answers the one thing the engine reads a
// seat's credential for: whose account it is. That is what makes an issue
// assigned to ceo-bot wake the seat holding that credential.
func newAccountForge(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/user" || r.Header.Get("PRIVATE-TOKEN") != "glpat-ceo" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"username":"ceo-bot","id":9}`)
	}))
	t.Cleanup(server.Close)
	return server
}

// deliver posts one signed GitLab delivery assigning an issue to the seat,
// and returns the status it got. Each id is a distinct delivery, so the
// fleet's dedupe cannot be what answers the second one.
func deliver(t *testing.T, client *http.Client, base, id string) int {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"object_kind": "issue",
		"user":        map[string]any{"username": "human-dev"},
		"project":     map[string]any{"id": 7, "path_with_namespace": "acme/api"},
		"object_attributes": map[string]any{
			"action": "open", "iid": 42, "title": "Fix the login redirect",
			"description": "the redirect loops on staging",
			"url":         "https://gitlab.example.com/acme/api/-/issues/42",
		},
		"assignees": []any{map[string]any{"username": "ceo-bot"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	key, ok := whsec.Key(drainProbeSecret)
	if !ok {
		t.Fatal("the probe's signing secret is not a whsec key")
	}
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + stamp + "."))
	mac.Write(body)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/webhooks/gitlab", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitlab-Event", "Issue Hook")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", stamp)
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("delivering %s: %v", id, err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// write sends an authenticated config write and returns its status and the
// error code it carried.
func write(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url,
		strings.NewReader("name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+drainProbeToken)
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer res.Body.Close()
	var body struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body.Error
}

// refused is the status [probe] reports for a connection nothing accepted,
// which is a listener that is not there rather than one that is slow.
const refused = -1

// drainProbeClient is the one client every request in this case rides.
//
// ITS OWN POOL, from [httpxtest], never the process-global one: the two
// httptest servers this case stands up sweep http.DefaultTransport when they
// close, for every server in the binary rather than their own, and
// internal/httpx's guard fails the build over exactly that. The child's
// listener is a real one the engine opened, which is the case [httpxtest.Pool]
// exists for.
//
// The timeout is well past anything a request here takes, because one that
// timed out on a loaded machine must not read as a listener that went away.
// probeAs is [probe] presenting a bearer token, for a guarded route.
func probeAs(client *http.Client, url, token string) (int, map[string]any) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := client.Do(req)
	if errors.Is(err, syscall.ECONNREFUSED) {
		return refused, nil
	}
	if err != nil {
		return 0, nil
	}
	defer func() { _ = res.Body.Close() }()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

func drainProbeClient(t *testing.T) *http.Client {
	t.Helper()
	client := httpxtest.Pool(t)
	client.Timeout = 15 * time.Second
	return client
}

// probe GETs a URL once and returns its status and decoded body. Zero is a
// request that got no answer, and [refused] one whose connection was refused.
//
// NO CREDENTIAL, because every path it is pointed at is an exempt one — the
// probes and the dashboard shell. A guarded route needs [probeAs].
func probe(client *http.Client, url string) (int, map[string]any) {
	res, err := client.Get(url) //nolint:noctx // a probe against the child's own listener
	if errors.Is(err, syscall.ECONNREFUSED) {
		return refused, nil
	}
	if err != nil {
		return 0, nil
	}
	defer res.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

// eventually polls until cond holds, failing if it never does or if the node
// exits first, which is a wait that can no longer succeed.
func eventually(t *testing.T, what string, alive, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(drainProbeBudget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		if !alive() {
			t.Fatalf("the node exited while waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// logOrder is the line on which each message was first logged, or -1.
func logOrder(output string, messages ...string) []int {
	at := make([]int, len(messages))
	for i := range at {
		at[i] = -1
	}
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for line := 0; scanner.Scan(); line++ {
		var record struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			continue
		}
		if i := slices.Index(messages, record.Msg); i >= 0 && at[i] == -1 {
			at[i] = line
		}
	}
	return at
}

// writeFile writes one file into dir and returns its path.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// lockedBuffer is a buffer the child's two streams write to and the test
// reads while they do.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
