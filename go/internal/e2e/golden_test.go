package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// GATE G5, third leg — "a golden company runs end-to-end with the UI showing
// live turns".
//
// Every other test in this tree stops at a seam. This one starts a real engine
// on a real broker, wakes a real seat with a real trigger, drives a real
// Plan/Execute/Review loop against a scripted vendor endpoint, and reads the
// result off a WebSocket dialled the way the dashboard dials it — then feeds
// those exact frames through the dashboard's OWN store.js and socket.js.
//
// It is the only test that can catch the class of bug it was written for. The
// turn engine emitted NO events at all when this was written: every payload
// type existed, the projection keyed on all of them, the socket fanned them
// out, and the dashboard rendered them — and nothing in between ever published
// one, so a running company showed an empty dashboard for ever. Every
// component's own tests passed.

// dial opens a socket the way the dashboard's LiveSocket does.
func (n *node) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	target := "ws" + strings.TrimPrefix(n.server.URL, "http") + "/ws/stream"
	conn, _, err := websocket.Dial(t.Context(), target, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", target, err)
	}
	// The frames a live turn produces are far larger than the default cap:
	// a phase completion carries its prompts verbatim.
	conn.SetReadLimit(8 << 20)
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

// capture reads frames until stop says enough, or the deadline passes.
//
// Keeps the RAW bytes, not decoded maps: they are replayed verbatim through
// the dashboard's own client, and decoding and re-encoding here would mean the
// client was fed this test's re-serialization rather than the server's output.
type capture struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *capture) add(raw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, slices.Clone(raw))
}

func (c *capture) all() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.frames)
}

// kinds reports the envelope kind of every captured frame, in order.
func (c *capture) kinds(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, raw := range c.all() {
		var env struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(raw, &env) == nil {
			out = append(out, env.Kind)
		}
	}
	return out
}

// liveCalls reports every in-flight call pushed on an `agents` frame.
//
// An `agents` push, NOT an `event` one, and the distinction is the contract:
// agent_turn_progress is live-only, so the projection moves the seat's row and
// deliberately does not mirror it into the activity buffer. Asserting it as a
// feed entry — which the first version of this did — asserts the opposite of
// what the design says, and would have been "fixed" by persisting a round-by-
// round signal the phase record already covers.
func (c *capture) liveCalls(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range c.all() {
		var env struct {
			Kind string           `json:"kind"`
			Data []map[string]any `json:"data"`
		}
		if json.Unmarshal(raw, &env) != nil || env.Kind != "agents" {
			continue
		}
		for _, row := range env.Data {
			if call, ok := row["live_call"].(map[string]any); ok && call != nil {
				out = append(out, call)
			}
		}
	}
	return out
}

// seatStates reports every state an `agents` push put a seat in, by role.
func (c *capture) seatStates(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, raw := range c.all() {
		var env struct {
			Kind string           `json:"kind"`
			Data []map[string]any `json:"data"`
		}
		if json.Unmarshal(raw, &env) != nil || env.Kind != "agents" {
			continue
		}
		for _, row := range env.Data {
			role, _ := row["role"].(string)
			state, _ := row["state"].(string)
			if role != "" && state != "" {
				out[role] = append(out[role], state)
			}
		}
	}
	return out
}

// eventTypes reports the type of every `event` frame, in order.
func (c *capture) eventTypes(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, raw := range c.all() {
		var env struct {
			Kind string `json:"kind"`
			Data struct {
				Type string `json:"type"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Kind == "event" {
			out = append(out, env.Data.Type)
		}
	}
	return out
}

// read pumps the socket into a capture until the context ends.
func read(ctx context.Context, conn *websocket.Conn, into *capture) {
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		into.add(raw)
	}
}

// wake publishes a trigger onto a seat's inbox, as a notification transport
// would.
func (n *node) wake(t *testing.T, handle, text string) {
	t.Helper()
	body := text
	ev := events.New(types.ExternalNotification{
		NotificationSource: "slack",
		SourceEventType:    "message",
		Sender:             "U0FOUNDER",
		Subject:            "How did the week go?",
		Body:               text,
		SalientBody:        &body,
	}, events.TraceContext{})
	if err := n.engine.Backends().Queue.Publish(t.Context(),
		topics.AgentInbox(handle), ev); err != nil {
		t.Fatalf("wake %s: %v", handle, err)
	}
}

func TestAGoldenCompanyRunsATurnOntoTheDashboard(t *testing.T) {
	n := start(t)

	// The seat has to be claimed before its inbox is consumed; publishing
	// earlier is safe (the queue is durable) but would make a failure here
	// read as a lost message rather than a slow claim.
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "ceo")
	})

	conn := n.dial(t)
	frames := &capture{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go read(ctx, conn, frames)

	// The snapshot lands on connect, before anything else — proof the
	// socket is live, so a later silence is a missing event rather than a
	// socket that never opened.
	waitFor(t, "the snapshot", func() bool {
		return slices.Contains(frames.kinds(t), "snapshot")
	})

	n.wake(t, "ceo", "How did the week go?")

	waitFor(t, "the turn to complete", func() bool {
		return slices.Contains(frames.eventTypes(t), "agent_turn_completed")
	})
	cancel()

	// --- what the model was actually asked ---------------------------- //
	if got := n.model.seen(); !slices.Contains(got, "plan") ||
		!slices.Contains(got, "execute") || !slices.Contains(got, "review") {
		t.Errorf("phases run = %v, want a full Plan/Execute/Review loop", got)
	}

	// --- what reached the socket -------------------------------------- //
	seen := frames.eventTypes(t)
	for _, want := range []string{
		"agent_phase_started",   // which phase, live
		"agent_phase_completed", // the durable record
		"agent_turn_completed",  // what ends the live row
		"turn_completed",        // the learning subsystem's record
	} {
		if !slices.Contains(seen, want) {
			t.Errorf("no %s reached the dashboard socket; saw %v", want, seen)
		}
	}

	// --- THE UI SHOWING A LIVE TURN ----------------------------------- //
	// The point of the gate. Not "an event arrived" but "the seat's row
	// moved": the projection put CEO into `working` and hung an in-flight
	// call off it naming the phase and the model.
	states := frames.seatStates(t)
	if !slices.Contains(states["CEO"], "working") {
		t.Errorf("the seat never showed as working; states = %v", states["CEO"])
	}
	calls := frames.liveCalls(t)
	if len(calls) == 0 {
		t.Fatal("no in-flight call ever reached the dashboard: the seat would " +
			"have gone from idle to done with nothing on screen in between")
	}
	phases := map[string]bool{}
	for _, call := range calls {
		if p, ok := call["phase"].(string); ok {
			phases[p] = true
		}
	}
	for _, want := range []string{"plan", "execute", "review"} {
		if !phases[want] {
			t.Errorf("no live call named the %s phase; saw %v", want, phases)
		}
	}
	if model, _ := calls[len(calls)-1]["model"].(string); model != "claude-golden" {
		t.Errorf("the live call names model %q, not the one that served it", model)
	}

	// --- and what the store kept -------------------------------------- //
	// The socket and the store are fed by DIFFERENT halves of the pipeline
	// — a broadcast subscription and a publish listener — so one working
	// says nothing about the other.
	rows, err := n.engine.Backends().Store.Events().List(t.Context(),
		store.ListQuery{Limit: 200})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	kept := map[string]bool{}
	for _, r := range rows {
		kept[r.Type] = true
	}
	for _, want := range []string{"agent_phase_completed", "agent_turn_completed"} {
		if !kept[want] {
			t.Errorf("%s is not in the event store; the feed would be empty "+
				"after a reload", want)
		}
	}
	if kept["agent_turn_progress"] {
		t.Error("agent_turn_progress was persisted; it is live-only")
	}
}

// --- the client's half ----------------------------------------------------- //

// replayScript drives the dashboard's own store.js and socket.js over the
// frames this server produced. See tests/test_dashboard/js/replay.mjs.
//
// PHASE 9 EDITS THIS, with the two constants in internal/api/dashboardjs_test.go
// it sits beside: the suites move when the Python tree goes.
const (
	replayScript  = "../../../tests/test_dashboard/js/replay.mjs"
	dashboardTree = "../../static/dashboard"
	dashboardEnv  = "CREWLET_DASHBOARD_ROOT"
	replayTimeout = 60 * time.Second
)

func TestTheDashboardClientCanReadWhatThisServerSends(t *testing.T) {
	// THE OTHER HALF OF THE GATE. The test above asserts the frames say the
	// right things; this one asserts the CLIENT can read them — and those
	// are different questions, which is the entire lesson of the bug that
	// prompted it.
	//
	// The `agents` push was going out as an object keyed by role. Every
	// field in it was correct. The server's tests asserted that shape and
	// passed; the dashboard's own suites passed; the socket delivered every
	// frame. And store.js guards applyAgents with Array.isArray, so it
	// dropped all of them, and a company running a full turn rendered idle
	// from the first phase to the last. Nothing on either side could see
	// it, because nothing on either side ran both.
	//
	// The client is the compatibility reference and wins any disagreement
	// (rewrite/decisions/502) — so a failure here is the SERVER's.
	node := nodeBinary(t)
	n := start(t)

	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "ceo")
	})
	conn := n.dial(t)
	frames := &capture{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go read(ctx, conn, frames)
	waitFor(t, "the snapshot", func() bool {
		return slices.Contains(frames.kinds(t), "snapshot")
	})

	n.wake(t, "ceo", "How did the week go?")
	waitFor(t, "the turn to complete", func() bool {
		return slices.Contains(frames.eventTypes(t), "agent_turn_completed")
	})
	cancel()

	// The RAW bytes, as strings, in arrival order. Not re-encoded: the
	// client must be fed what the server wrote, or the replay certifies
	// this test's serialization instead of the server's.
	raw := frames.all()
	texts := make([]string, 0, len(raw))
	for _, f := range raw {
		texts = append(texts, string(f))
	}
	payload, err := json.Marshal(texts)
	if err != nil {
		t.Fatalf("encoding the capture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "frames.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("writing the capture: %v", err)
	}

	tree, err := filepath.Abs(dashboardTree)
	if err != nil {
		t.Fatalf("resolving the dashboard tree: %v", err)
	}
	script, err := filepath.Abs(replayScript)
	if err != nil {
		t.Fatalf("resolving the replay script: %v", err)
	}

	runCtx, runCancel := context.WithTimeout(t.Context(), replayTimeout)
	defer runCancel()
	cmd := exec.CommandContext(runCtx, node, script, path)
	cmd.Env = append(os.Environ(), dashboardEnv+"="+tree)
	out, err := cmd.CombinedOutput()
	if runCtx.Err() != nil {
		t.Fatalf("the replay did not finish within %s:\n%s", replayTimeout, out)
	}
	if err != nil {
		t.Fatalf("the dashboard client could not read this server's frames "+
			"(%d captured):\n%s", len(texts), out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// nodeBinary finds node, or explains its absence the right way for the run.
//
// Locally a missing node skips: the dashboard's half is somebody else's
// problem and the rest of the gate still runs. In CI it is a red build — this
// is the only place the two halves of the wire protocol are checked against
// each other, so letting it go quiet retires the check behind a green tick.
func nodeBinary(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err == nil {
		return node
	}
	switch strings.ToLower(os.Getenv("CI")) {
	case "1", "true", "yes":
		t.Fatalf("node is not on PATH, so the client half of the wire-protocol "+
			"gate would skip and the build would still pass: %v", err)
	}
	t.Skip("node is not installed; the dashboard client replay needs it")
	return ""
}
