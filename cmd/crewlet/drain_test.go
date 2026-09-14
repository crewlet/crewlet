package main

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/whsec"
)

// A SIGNALLED NODE DRAINS WITH ITS PROBES UP.
//
// This is `crewlet run` itself, stopped the way an orchestrator stops it: a
// real SIGTERM to a real process, with a real turn in flight. A signal handler
// cannot be exercised from inside the process that installed it without
// taking the test binary with it, so the node runs in a CHILD: the test binary
// re-executes itself, the child calls [run], and the parent signals it and
// watches from outside.
//
// The turn is held in flight by the model it is waiting on, which answers
// only when the parent says so. That is what makes the drain last long enough
// to observe, and it is a real turn on the real path: a signed GitLab delivery
// on the webhook edge, the notification service, the seat's mailbox, and an
// Anthropic backend pointed at the parent.
const (
	drainProbeEnv     = "CREWLET_RUN_DRAIN_PROBE"
	drainProbeArgsEnv = "CREWLET_RUN_DRAIN_PROBE_ARGS"

	// Distinctive, and specifically not 0, 1 or 2: a test binary that ran
	// its tests exits 0 or 1 and a panic exits 2, so an exit code that
	// could be any of those would not say that run() itself returned.
	drainProbeExit = 7

	// drainProbeKey is the model credential the child's company names. A
	// placeholder: the model is the parent's own endpoint.
	drainProbeKey = "sk-ant-drain-probe"

	// drainProbeToken is the operator credential the child's Tier A
	// accepts, so a write is refused for draining rather than for having
	// no token.
	drainProbeToken = "drain-probe-token"

	// drainProbeSecret is a well-formed Standard-Webhooks key, the only
	// shape the GitLab route verifies with.
	drainProbeSecret = "whsec_ZHJhaW4tcHJvYmUtc2lnbmluZy1rZXktMzItYnl0ZXM="

	// drainProbeBudget is how long any one wait here gets: a node booted
	// under the race detector on a machine running every other package.
	// Generous because it costs nothing when the condition is met; a wait
	// that times out is a staging bug, since nothing here races the clock.
	drainProbeBudget = 90 * time.Second
)

// runDrainProbe is the child. It never returns.
func runDrainProbe() {
	// THE PROCESS'S OWN SINK, on stderr, because the parent reads the order
	// of the shutdown from it. JSON is set by the flag as well, below: run
	// applies the flags over whatever is installed.
	logging.Configure(slog.LevelInfo, logging.FormatJSON, os.Stderr)
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(drainProbeArgsEnv)), &args); err != nil {
		fmt.Fprintln(os.Stderr, "probe: args:", err)
		os.Exit(3)
	}
	if err := run(append([]string{"run"}, args...), os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "probe: run:", err)
		os.Exit(4)
	}
	os.Exit(drainProbeExit)
}

func TestASignalledNodeDrainsWithItsProbesUp(t *testing.T) {
	// Not parallel: it boots a whole node in a second process, and a
	// package's parallel cases waiting on the same cores is time taken from
	// the one budget here that matters.
	model := newHeldModel(t)
	forge := newAccountForge(t)
	port := freePort(t)
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	dir := t.TempDir()
	boot := writeFile(t, dir, "crewlet.yaml", fmt.Sprintf(`node:
  id: drain-probe
store:
  path: %s
stream:
  store_dir: %s
api:
  host: 127.0.0.1
  port: %d
  auth:
    tokens:
      - id: founder
        token: %s
`, filepath.Join(dir, "crewlet.db"), filepath.Join(dir, "stream"), port, drainProbeToken))
	company := writeFile(t, dir, "company.yaml", fmt.Sprintf(`name: Acme
providers:
  llm:
    held:
      type: anthropic
      model: claude-drain-probe
      base_url: %s
      api_keys: ["${CREWLET_DRAIN_PROBE_KEY}"]
integrations:
  gitlab:
    enabled: true
    url: %s
    signing_secret: %s
roles:
  - name: CEO
    handle: ceo
    llm: held
    mcp_env:
      gitlab:
        GITLAB_TOKEN: glpat-ceo
turn_engine:
  max_iterations: 1
  max_tool_rounds: 3
`, model.url, forge.URL, drainProbeSecret))

	args, err := json.Marshal([]string{"-config", boot, "-company", company, "-log-format", "json"})
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(t.Context(), os.Args[0],
		"-test.run=^TestASignalledNodeDrainsWithItsProbesUp$")
	child.Env = append(os.Environ(),
		drainProbeEnv+"=1",
		drainProbeArgsEnv+"="+string(args),
		"CREWLET_DRAIN_PROBE_KEY="+drainProbeKey)
	var output lockedBuffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatalf("starting the node: %v", err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() {
		defer close(exited)
		waitErr = child.Wait()
	}()
	// On every path out, so a failed assertion cannot leave a node running
	// or the model holding a request the node is waiting on.
	t.Cleanup(func() {
		model.letGo()
		_ = child.Process.Kill()
		<-exited
		if t.Failed() {
			t.Logf("the node's output:\n%s", output.String())
		}
	})
	alive := func() bool {
		select {
		case <-exited:
			return false
		default:
			return true
		}
	}

	// UP, with the seat claimed. Readiness first, because a probe is what
	// an orchestrator gates traffic on; the seat second, because a delivery
	// to a seat nobody holds would wait in its mailbox rather than start
	// the turn this case needs in flight.
	eventually(t, "the node to be ready", alive, func() bool {
		status, _ := probe(base + "/ready")
		return status == http.StatusOK
	})
	eventually(t, "the seat to be claimed", alive, func() bool {
		_, body := probe(base + "/health")
		seats, _ := body["seats"].([]any)
		return slices.Contains(seats, any("ceo"))
	})

	// A TURN IN FLIGHT, started on the real inbound path and held by the
	// model it is waiting on.
	if status := deliver(t, base, "delivery-1"); status != http.StatusOK {
		t.Fatalf("the delivery answered %d on a serving node", status)
	}
	select {
	case <-model.entered:
	case <-exited:
		t.Fatal("the node exited before its turn reached the model")
	case <-time.After(drainProbeBudget):
		t.Fatal("the turn never reached the model")
	}
	eventually(t, "the turn to count as in flight", alive, func() bool {
		_, body := probe(base + "/health")
		inFlight, _ := body["in_flight"].(float64)
		return inFlight >= 1
	})

	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signalling the node: %v", err)
	}

	// LIVENESS STAYS 200 THROUGH THE DRAIN, and says what the node is
	// doing. This is the probe the node used to stop answering the moment
	// the signal arrived, because the listener closed before the drain: a
	// refused connection now is that bug, and no amount of waiting fixes it.
	eventually(t, "/health to report the drain", alive, func() bool {
		status, body := probe(base + "/health")
		if status == refused {
			t.Fatal("the node stopped answering /health on the signal, with its " +
				"turn still running: an orchestrator would kill it mid-turn")
		}
		return status == http.StatusOK && body["status"] == "shutting_down"
	})
	// And keeps answering, rather than for one lucky request: the turn is
	// still held, so every one of these is inside the drain.
	for range 10 {
		if status, body := probe(base + "/health"); status != http.StatusOK {
			t.Fatalf("/health answered %d mid-drain (%v): an orchestrator would "+
				"kill this node in the middle of the turn it is finishing", status, body)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// READINESS IS 503, and says why.
	if status, body := probe(base + "/ready"); status != http.StatusServiceUnavailable ||
		body["reason"] != "draining" {
		t.Errorf("/ready answered %d %v mid-drain, want 503 naming the drain", status, body)
	}

	// EVERY ROUTE THAT WOULD START WORK REFUSES, clearly: a second delivery
	// on the webhook edge, and a config write with a valid token.
	if status := deliver(t, base, "delivery-2"); status != http.StatusServiceUnavailable {
		t.Errorf("a delivery answered %d mid-drain, want 503", status)
	}
	if status, code := write(t, base+"/config"); status != http.StatusServiceUnavailable || code != "draining" {
		t.Errorf("PUT /config answered %d %q mid-drain, want 503 draining", status, code)
	}
	// And a read is still a read.
	if status, _ := probe(base + "/agents"); status != http.StatusOK {
		t.Errorf("GET /agents answered %d mid-drain, want 200", status)
	}
	if !alive() {
		t.Fatal("the node exited while its turn was still held: the drain did not wait for it")
	}

	// THE TURN FINISHES, and only then does the node go.
	model.letGo()
	select {
	case <-exited:
	case <-time.After(drainProbeBudget):
		t.Fatal("the node did not exit once its turn finished")
	}
	var exit *exec.ExitError
	if !errors.As(waitErr, &exit) || exit.ExitCode() != drainProbeExit {
		t.Fatalf("the node ended with %v, want run() to return and the probe to exit %d",
			waitErr, drainProbeExit)
	}

	// THE LISTENER CLOSED AFTER THE DRAIN AND BEFORE THE TEARDOWN: after,
	// because the probes above were served while the turn ran; before,
	// because both of them read what the teardown closes.
	order := logOrder(output.String(), "engine_draining", "drain_complete", "api_stopped", "engine_stopped")
	if !slices.IsSorted(order) || slices.Contains(order, -1) {
		t.Errorf("the shutdown ran out of order: engine_draining, drain_complete, "+
			"api_stopped and engine_stopped logged at lines %v", order)
	}
}

// heldModel is an Anthropic Messages endpoint that answers nothing until the
// test lets it go, and then refuses everything at once.
//
// A REFUSAL rather than an answer, because the case is about the node that
// waits for the turn and not about the turn: a request the model rejects ends
// it on the spot, with no retry (the provider makes none) and no second model
// to fall back to.
type heldModel struct {
	url      string
	entered  chan struct{}
	release  chan struct{}
	enter    sync.Once
	released sync.Once
}

func newHeldModel(t *testing.T) *heldModel {
	t.Helper()
	m := &heldModel{entered: make(chan struct{}), release: make(chan struct{})}
	server := httptest.NewServer(http.HandlerFunc(m.serve))
	// The let-go is registered AFTER the close, so LIFO runs it first: a
	// server closing while a request is still held waits for that request
	// for ever.
	t.Cleanup(server.Close)
	t.Cleanup(m.letGo)
	m.url = server.URL
	return m
}

func (m *heldModel) letGo() { m.released.Do(func() { close(m.release) }) }

func (m *heldModel) serve(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	m.enter.Do(func() { close(m.entered) })
	select {
	case <-m.release:
	case <-r.Context().Done():
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error",`+
		`"message":"the drain probe has no model"}}`)
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
func deliver(t *testing.T, base, id string) int {
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
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delivering %s: %v", id, err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// write sends an authenticated config write and returns its status and the
// error code it carried.
func write(t *testing.T, url string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, url,
		strings.NewReader("name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+drainProbeToken)
	res, err := http.DefaultClient.Do(req)
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

// probe GETs a URL once and returns its status and decoded body. Zero is a
// request that got no answer, and [refused] one whose connection was refused.
//
// The timeout is well past anything a probe takes, because a probe that
// timed out on a loaded machine must not read as a listener that went away.
func probe(url string) (int, map[string]any) {
	client := http.Client{Timeout: 15 * time.Second}
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
