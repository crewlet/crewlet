package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// A STARTING NODE ANSWERS ITS PROBES BEFORE IT SERVES, and nothing its company
// configures holds the answer that ends a startup probe.
//
// `crewlet run` itself, in a child process on the socket this case reserved
// ([startNodeChild]), with a company whose one shared tool server never speaks:
// the boot sits in that server's start — after the node has recovered what it
// holds, before its HTTP surface exists — for as long as this case keeps the
// server waiting. Inside that window the listener is already bound, and
// answers what a starting node answers: /health 200 `starting` in the
// `preparing` phase, /ready 503 `starting`, and every other route 503
// `starting` with a Retry-After and the security headers. Then the server is
// let go, and the same socket answers as the node's own surface.
//
// The window is held open by the server rather than by a hook in the engine,
// because the point is the real boundary: a tool server the company configures
// is exactly what used to keep the listener shut until a startup probe killed
// the node.
func TestAStartingNodeAnswersItsProbesBeforeItServes(t *testing.T) {
	// Not parallel, for the drain case's reason.
	client := drainProbeClient(t)
	port, socket := reserveNodeSocket(t)
	base := "http://127.0.0.1:" + strconv.Itoa(port)

	dir := t.TempDir()
	// THE RELEASE: the tool server waits for this file to exist and then
	// exits without a word, which ends its start at once as a failed one —
	// a server that fails only loses its own tools, and the boot goes on.
	release := filepath.Join(dir, "release-the-tool-server")
	// THE BROKER SIZED beside its store_dir, for the drain case's reason
	// ([brokertest.StoreIn]): a limit read off the runner's free disk boots
	// this node on one machine and refuses it on a fuller one.
	boot := writeFile(t, dir, "crewlet.yaml", fmt.Sprintf(`node:
  id: startup-probe
store:
  path: %s
stream:
  store_dir: %s
  store_max_bytes: %d
secrets:
  active_key_id: startup-probe
  keys:
    - id: startup-probe
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
	// A STARTUP TIMEOUT FAR PAST THIS CASE'S OWN BUDGET, so it is the release
	// that ends the window and never the engine's bound on the server.
	company := writeFile(t, dir, "company.yaml", fmt.Sprintf(`name: Acme
providers:
  sandbox: {fake: true}
  llm:
    held:
      model: claude-startup-probe
      state_dir: %q
mcp_servers:
  - name: silent
    command: /bin/sh
    args: ["-c", "while [ ! -e '%s' ]; do sleep 0.1; done"]
    startup_timeout_seconds: 3600
roles:
  - name: CEO
    handle: ceo
    llm: held
`, filepath.Join(dir, "llm"), release))

	node := startNodeChild(t, "TestAStartingNodeAnswersItsProbesBeforeItServes", socket,
		[]string{"-config", boot, "-company", company, "-log-format", "json"})
	// LET GO BEFORE THE CHILD IS KILLED (LIFO), so a failed case does not
	// leave the boot waiting on the server through the teardown.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })

	// RECOVERED AND PREPARING: /health turns 200 the moment the engine has
	// recovered what the node holds, with the boot still in the tool
	// server's start. Anything it answers before that is the recovery's 503,
	// and never the node's own surface.
	eventually(t, "/health to report the boot preparing", node.alive, func() bool {
		status, _, body := askNode(t, client, http.MethodGet, base+"/health", "")
		if status == http.StatusServiceUnavailable &&
			(body["status"] != api.StatusStarting || body["phase"] != api.PhaseRecovering) {
			t.Fatalf("/health answered 503 %v while the node recovers, want "+
				"status %q in phase %q", body, api.StatusStarting, api.PhaseRecovering)
		}
		return status == http.StatusOK && body["phase"] == api.PhasePreparing
	})
	status, _, health := askNode(t, client, http.MethodGet, base+"/health", "")
	if status != http.StatusOK || health["status"] != api.StatusStarting ||
		health["node"] != "startup-probe" || health["version"] == nil {
		t.Errorf("/health = %d %v while the company's tool server starts, want 200 "+
			"starting naming the node and its version", status, health)
	}
	// NOTHING ABOUT A RUNNING ENGINE, which a starting node has none of: a
	// zero standing in for its queue or its seats would be a fiction.
	for _, field := range []string{"queue", "seats", "posture", "applied_epoch", "started_at"} {
		if _, present := health[field]; present {
			t.Errorf("/health carries %q before the engine serves: %v", field, health)
		}
	}

	// NOT READY, and saying why.
	status, _, ready := askNode(t, client, http.MethodGet, base+"/ready", "")
	if status != http.StatusServiceUnavailable || ready["reason"] != api.ReasonStarting ||
		ready["ready"] != false || ready["configured"] != false || ready["draining"] != false {
		t.Errorf("/ready = %d %v while the node starts, want 503 %q", status, ready,
			api.ReasonStarting)
	}

	// EVERY OTHER ROUTE WAITS, and says how long to: a guarded read with a
	// valid credential and a webhook delivery alike, with the headers every
	// answer of the node's carries.
	for _, route := range [][2]string{
		{http.MethodGet, "/agents"}, {http.MethodPost, "/webhooks/gitlab"},
		{http.MethodGet, "/dashboard"}, {http.MethodPut, "/config"},
	} {
		status, header, body := askNode(t, client, route[0], base+route[1], drainProbeToken)
		if status != http.StatusServiceUnavailable || body["error"] != string(httpjson.CodeStarting) {
			t.Errorf("%s %s = %d %v while the node starts, want 503 %q",
				route[0], route[1], status, body, httpjson.CodeStarting)
		}
		if got, want := header.Get("Retry-After"),
			strconv.Itoa(int(api.BootRetryAfter/time.Second)); got != want {
			t.Errorf("%s %s carries Retry-After %q, want %q", route[0], route[1], got, want)
		}
		if header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s carries no security headers: %v", route[0], route[1], header)
		}
	}
	if order := logOrder(node.output.String(), "api_bound", "engine_recovered",
		"api_listening"); order[0] == -1 || order[1] < order[0] || order[2] != -1 {
		t.Errorf("api_bound, engine_recovered and api_listening logged at lines %v "+
			"inside the window; want the bind, then the recovery, and no handover yet", order)
	}

	// THE SERVER LET GO, and the same socket becomes the node's own surface.
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatalf("releasing the tool server: %v", err)
	}
	eventually(t, "the node to be ready", node.alive, func() bool {
		status, _, _ := askNode(t, client, http.MethodGet, base+"/ready", "")
		return status == http.StatusOK
	})
	status, _, health = askNode(t, client, http.MethodGet, base+"/health", "")
	if status != http.StatusOK || health["status"] != api.StatusOK || health["configured"] != true {
		t.Errorf("/health = %d %v once the node serves, want 200 ok and configured", status, health)
	}
	if _, present := health["phase"]; present {
		t.Errorf("/health still names a boot phase once the node serves: %v", health)
	}
	if status, _, body := askNode(t, client, http.MethodGet, base+"/agents", drainProbeToken); status != http.StatusOK {
		t.Errorf("GET /agents = %d %v once the node serves, want 200", status, body)
	}
	if order := logOrder(node.output.String(), "api_bound", "engine_recovered",
		"api_listening"); !slices.IsSorted(order) || slices.Contains(order, -1) {
		t.Errorf("api_bound, engine_recovered and api_listening logged at lines %v, "+
			"want all three in that order", order)
	}

	// AND IT STOPS AS ANY NODE DOES.
	if err := node.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signalling the node: %v", err)
	}
	select {
	case <-node.exited:
	case <-time.After(drainProbeBudget):
		t.Fatal("the node did not exit on its signal")
	}
	node.exitedWith(t, drainProbeExit)
}

// askNode sends one request, presenting token when it is not empty, and
// returns its status, its headers and its decoded body. Zero is a request that
// got no answer — the child still starting its process, on a loaded machine —
// and [refused] one whose connection was refused, as [probe] reports them, so
// a wait retries either and an assertion fails on it by its status.
//
// The body is returned only when it is a JSON object, which is every answer
// an assertion here reads a field of. A listing such as /agents answers a
// JSON array, which is JSON all the same, and its caller asks only for its
// status; a body that is not JSON at all fails the test.
func askNode(t *testing.T, client *http.Client, method, url, token string,
) (int, http.Header, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url,
		strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := client.Do(req)
	if errors.Is(err, syscall.ECONNREFUSED) {
		return refused, http.Header{}, nil
	}
	if err != nil {
		return 0, http.Header{}, nil
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return 0, http.Header{}, nil
	}
	var decoded any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s %s answered %d with a body that is not JSON: %q",
				method, url, res.StatusCode, raw)
		}
	}
	body, _ := decoded.(map[string]any)
	return res.StatusCode, res.Header, body
}
