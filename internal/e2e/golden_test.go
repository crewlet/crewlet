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

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tools"
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

// lastRollup returns the most recent `tokens` frame's payload, or nil.
func (c *capture) lastRollup(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	for _, raw := range c.all() {
		var env struct {
			Kind string         `json:"kind"`
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal(raw, &env) == nil && env.Kind == "tokens" {
			out = env.Data
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
	n.wakeInTrace(t, handle, text, events.TraceContext{})
}

// wakeInTrace is wake with a chosen trace, so a test can follow it through.
func (n *node) wakeInTrace(t *testing.T, handle, text string, tc events.TraceContext) {
	t.Helper()
	body := text
	ev := events.New(types.ExternalNotification{
		NotificationSource: "slack",
		SourceEventType:    "message",
		Sender:             "U0FOUNDER",
		// NEVER the message text. The subject was hardcoded to the string
		// most callers also pass as the body, so subject and body were
		// indistinguishable downstream — and every assertion that a turn
		// received what was sent passed while the engine was handing the
		// seat its SUBJECT and dropping the body. A subject that cannot
		// equal the body is what makes those assertions mean something.
		Subject:     "a message for " + handle,
		Body:        text,
		SalientBody: &body,
	}, tc)
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

	// THE LAST of the turn's events, not the first. agent_turn_completed
	// and turn_completed are two publishes on one path, in that order, so
	// waiting for the earlier one leaves the assertion below racing the
	// later — which passes on an idle machine and fails under load, the
	// worst way for a gate to be wrong.
	waitFor(t, "the turn to complete", func() bool {
		return slices.Contains(frames.eventTypes(t), "turn_completed")
	})
	// And the spend rollup for the FINISHED turn. It rides the shared tick
	// rather than the publish path, so a tick that fires mid-turn produces
	// a real but partial rollup — waiting for "a tokens frame" would assert
	// against whichever phases happened to be done, which is a coin flip
	// (measured: two rows instead of three, one run in three).
	waitFor(t, "the spend rollup for the whole turn", func() bool {
		r := frames.lastRollup(t)
		if r == nil {
			return false
		}
		phases, _ := r["by_phase"].([]any)
		return len(phases) == 4 // onboarding, plan, execute, review
	})
	cancel()

	// --- what the model was actually asked ---------------------------- //
	got := n.model.seen()
	for _, want := range []string{"onboarding", "plan", "execute", "review"} {
		if !slices.Contains(got, want) {
			t.Errorf("the %s phase never ran; phases = %v", want, got)
		}
	}
	// Onboarding runs BEFORE Plan and on its own budget — that is the whole
	// reason it is a phase rather than a hint inside Plan's prompt, where it
	// could spend the plan budget on reading and starve submit_plan.
	if len(got) > 0 && got[0] != "onboarding" {
		t.Errorf("the first model call was %q, not the onboarding pass", got[0])
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
	for _, want := range []string{"onboarding", "plan", "execute", "review"} {
		if !phases[want] {
			t.Errorf("no live call named the %s phase; saw %v", want, phases)
		}
	}
	if model, _ := calls[len(calls)-1]["model"].(string); model != "claude-golden" {
		t.Errorf("the live call names model %q, not the one that served it", model)
	}

	// --- and what the turn cost ---------------------------------------- //
	rollup := frames.lastRollup(t)
	if rollup == nil {
		t.Fatal("no spend rollup reached the dashboard")
	}
	totals, _ := rollup["totals"].(map[string]any)
	if n, _ := totals["total_tokens"].(float64); n <= 0 {
		t.Errorf("the rollup reports %v tokens for a turn that ran three "+
			"phases", totals["total_tokens"])
	}
	if n, _ := totals["calls"].(float64); n != 4 {
		t.Errorf("the rollup counted %v calls, want one per phase "+
			"(onboarding, plan, execute, review)", totals["calls"])
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
// frames this server produced. See tests/dashboard/js/replay.mjs.
const (
	replayScript  = "../../tests/dashboard/js/replay.mjs"
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
	// — so a failure here is the SERVER's.
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

func TestTheSeatCanReachItsBuiltins(t *testing.T) {
	t.Parallel()
	// The catalogue a planner is SHOWN, and the surface it can actually
	// call, are built from the epoch's registry — which NewCompany leaves
	// empty, because building an epoch must be something `crewlet validate`
	// can do without a database. So the engine fills it, per epoch, and a
	// node that forgot would boot a company whose agents have no way to
	// find a colleague or recall their own work, with nothing failing.
	n := start(t)
	company := n.engine.Company()
	if company == nil {
		t.Fatal("no epoch")
	}
	have := map[string]bool{}
	for _, name := range company.Tools.Snapshot().Names() {
		have[name] = true
	}
	for _, want := range []string{
		builtin.LookupColleagueTool, // needs only the turn's org
		builtin.A2AAskTool,          // needs the channel store and the queue
		builtin.UseSkillTool,        // needs the skill store
		builtin.RefineSkillTool,
		builtin.QueryEpisodesTool,
		builtin.RefreshMemoryTool,
		builtin.ReflectAndPersistTool,
		builtin.MarkOnboardedTool,
	} {
		if !have[want] {
			t.Errorf("%s is not in the epoch's registry, so no seat can call "+
				"it and no planner is told it exists", want)
		}
	}
}

func TestAToolActsForTheSeatThatCalledIt(t *testing.T) {
	t.Parallel()
	// The seat comes from the SURFACE the runner built, never from the
	// model's arguments — which is what stops one agent asking a question,
	// writing a note or marking an onboarding step as another.
	n := start(t)
	company := n.engine.Company()
	seat := company.Org.AgentSeatByHandle("ceo")
	if seat == nil {
		t.Fatal("no ceo seat")
	}
	entry, ok := company.Tools.Snapshot().Lookup(builtin.LookupColleagueTool)
	if !ok {
		t.Fatal("lookup_colleague is not registered")
	}
	seated, ok := entry.Tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("%s is a plain Callable, so it cannot know who called it",
			builtin.LookupColleagueTool)
	}

	// With a turn: it resolves against that turn's pinned org.
	res, err := seated.CallForTurn(t.Context(),
		&turnctx.Turn{Seat: seat, Org: company.Org},
		map[string]any{"query": "founder"})
	if err != nil {
		t.Fatalf("CallForTurn: %v", err)
	}
	if res.Failed || !strings.Contains(res.Output, "founder") {
		t.Errorf("lookup failed: %+v", res)
	}
	// The human seat's entry must say a2a_ask will not reach them —
	// otherwise an agent opens a channel no turn ever answers and waits.
	if !strings.Contains(res.Output, builtin.A2AAskTool) {
		t.Errorf("a human result does not warn about a2a_ask:\n%s", res.Output)
	}

	// Without one: refused, not guessed at.
	if res, _ := entry.Tool.Call(t.Context(), map[string]any{"query": "founder"}); !res.Failed {
		t.Errorf("a seatless call resolved anyway: %+v", res)
	}
}

func TestATightBudgetRefusesTheTurnRatherThanSpendingPastIt(t *testing.T) {
	t.Parallel()
	// THE SEAM WAS NEVER SUPPLIED. runner.Config.Budget existed and every
	// turn passed nil, so a company with `token_budget: 100000` spent
	// without limit and the number in its config was decoration. Money
	// leaves the building for every token, so this is the one counter that
	// fails CLOSED — a charge that cannot be made stops the round rather
	// than silently un-capping the company.
	n := startWith(t, func(doc string) string {
		return doc + "\ntoken_budget: 200\n"
	})
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "ceo")
	})
	n.wake(t, "ceo", "How did the week go?")

	// The scripted model reports 150 tokens on its first call and 130 on
	// the next, so the cap bites partway through the turn rather than
	// before it starts — which is the case a pre-flight check would miss.
	budgets := n.engine.Backends().Fleet
	waitFor(t, "the budget to be charged", func() bool {
		used, err := budgets.Used(t.Context(), coord.OrgScope)
		return err == nil && used > 0
	})
	waitFor(t, "the turn to stop", func() bool {
		used, err := budgets.Used(t.Context(), coord.OrgScope)
		if err != nil {
			return false
		}
		// Settled: no further charge fits under the cap.
		return used > 0 && used+150 > 200
	})

	used, err := budgets.Used(t.Context(), coord.OrgScope)
	if err != nil {
		t.Fatalf("used: %v", err)
	}
	if used > 200 {
		t.Errorf("the company spent %d against a cap of 200", used)
	}
	// And the SEAT's counter moved with it: one charge, both scopes.
	company := n.engine.Company()
	id, _ := company.Org.AgentIDFor(company.Org.AgentSeatByHandle("ceo"))
	seatUsed, err := budgets.Used(t.Context(), coord.AgentScope(id.String()))
	if err != nil {
		t.Fatalf("seat used: %v", err)
	}
	if seatUsed != used {
		t.Errorf("seat spent %d and the org %d; one charge must move both",
			seatUsed, used)
	}
}

// The trace a wake starts must reach the events the turn it caused writes —
// through the broker, the dispatcher, the turn engine and the publish listener
// — or `GET /events/trace/{id}` answers with the wake alone and the dashboard's
// trace view has nothing to arrange.
//
// This is the gate for the whole tracing change, and it is here rather than in
// internal/tracing for one reason: every component's own tests stop
// at a seam and substitute the thing on the other side, so "does anything
// actually connect these" is the one question none of them asks.
func TestATurnsEventsJoinTheTriggersTrace(t *testing.T) {
	n := start(t)

	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "ceo")
	})

	// A trace this test can recognise, in the shape a real tracer emits.
	const (
		traceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
		triggerSpan = "00f067aa0ba902b7"
	)
	n.wakeInTrace(t, "ceo", "How did the week go?",
		events.TraceContext{TraceID: traceID, SpanID: triggerSpan})

	waitFor(t, "the turn to complete", func() bool {
		rows, err := n.engine.Backends().Store.Events().List(t.Context(),
			store.ListQuery{Limit: 200})
		if err != nil {
			return false
		}
		for _, r := range rows {
			if r.Type == "agent_turn_completed" {
				return true
			}
		}
		return false
	})

	rows, err := n.engine.Backends().Store.Events().List(t.Context(),
		store.ListQuery{Limit: 200})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	var checked int
	spans := map[string]bool{}
	var sawTurnParent bool
	for _, r := range rows {
		switch r.Type {
		case "agent_phase_completed", "agent_turn_completed":
		default:
			continue
		}
		checked++

		if r.TraceID != traceID {
			t.Errorf("%s carries trace_id %q, want the trigger's %q — the turn "+
				"started a trace of its own", r.Type, r.TraceID, traceID)
		}
		// The bug this replaces: the turn used to publish the TRIGGER's
		// span id as its own, and a resumed turn published none at all.
		if r.SpanID == "" {
			t.Errorf("%s carries no span_id; the dashboard cannot place it", r.Type)
		}
		if r.SpanID == triggerSpan {
			t.Errorf("%s claims the trigger's span %q as its own", r.Type, r.SpanID)
		}
		spans[r.SpanID] = true
		if r.ParentSpanID == triggerSpan {
			sawTurnParent = true
		}
	}

	if checked == 0 {
		t.Fatal("no turn events were stored; this test asserted nothing")
	}
	// More than one, because each phase publishes under its OWN phase span
	// and the turn event under the turn's. One id across the whole turn is
	// what the dashboard's tree collapses to a single node, and it is what
	// this looked like before phase events stopped inheriting a fixed trace.
	if len(spans) < 2 {
		t.Errorf("turn events carry %d distinct span id(s); the trace tree "+
			"cannot separate the phases from the turn", len(spans))
	}
	if !sawTurnParent {
		t.Errorf("nothing hung off the trigger's span %q — the turn did not "+
			"join the trace that woke it", triggerSpan)
	}

	// --- and what the LOGS said ---------------------------------------- //
	// The other half of the correlation: a trace is only useful if the lines
	// the engine wrote while a span was open name it, so an operator can go
	// from a slow span to the log lines underneath it. This is what the
	// conversion of the turn path onto slog's *Context methods buys, and
	// without an assertion it would rot the first time someone wrote
	// log.Info in a frame that has a ctx.
	lines := logs.linesFor(traceID)
	if len(lines) == 0 {
		t.Fatalf("no log line carried trace_id %s; the turn ran without "+
			"correlation", traceID)
	}
	var withSpan int
	for _, line := range lines {
		if strings.Contains(line, `"span_id":"`) {
			withSpan++
		}
	}
	if withSpan == 0 {
		t.Errorf("%d lines carried the trace id but none carried a span id",
			len(lines))
	}
}
