package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// GATE G5, third leg — "a golden company runs end-to-end with the UI showing
// live turns".
//
// Every other test in this tree stops at a seam. This one starts a real engine
// on a real broker, wakes a real seat with a real trigger, drives a real
// executor and reviewer loop against a scripted vendor endpoint, and reads the
// result off a WebSocket dialled the way the dashboard dials it, then feeds
// those exact frames through the dashboard's OWN protocol module (the store
// and the socket, built as static/dashboard/protocol.js).
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

// agentRows reports every seat row an `agents` push carried, in order.
func (c *capture) agentRows(t *testing.T) []map[string]any {
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
		out = append(out, env.Data...)
	}
	return out
}

// turnStages reports the stage of every turn an `agents` push put a seat on.
func (c *capture) turnStages(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, row := range c.agentRows(t) {
		if turn, _ := row["turn"].(map[string]any); turn != nil {
			if stage, _ := turn["stage"].(string); stage != "" {
				out = append(out, stage)
			}
		}
	}
	return out
}

// sawRefusal reports whether a seat's pushed meter carried a refused window:
// stamped, and judged `refusing` by the engine. By ROLE, which is the key an
// `agents` push carries its changed rows under.
func (c *capture) sawRefusal(t *testing.T, role string) bool {
	t.Helper()
	for _, row := range c.agentRows(t) {
		if row["role"] != role {
			continue
		}
		meter, _ := row["budget"].(map[string]any)
		windows, _ := meter["windows"].([]any)
		for _, raw := range windows {
			w, _ := raw.(map[string]any)
			if at, _ := w["refused_at"].(string); at != "" && w["state"] == "refusing" {
				return true
			}
		}
	}
	return false
}

// seatStates reports every activity an `agents` push put a seat in, by role.
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
			activity, _ := row["activity"].(string)
			if role != "" && activity != "" {
				out[role] = append(out[role], activity)
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

// lastHealth reports the newest health body the socket carried — a snapshot's
// or a tick's, since both carry the whole envelope.
func (c *capture) lastHealth(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	for _, raw := range c.all() {
		var env struct {
			Kind string         `json:"kind"`
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		switch env.Kind {
		case "snapshot":
			if health, ok := env.Data["health"].(map[string]any); ok {
				out = health
			}
		case "health":
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

// phaseRecords returns the payload of every `agent_phase_completed` frame.
//
// The PAYLOAD, which is what separates this from [capture.eventTypes]: a phase
// record without one has no prompts, no tool calls, no decision and no
// duration, and the payload is exactly what the socket carries and a listing
// does not.
func (c *capture) phaseRecords(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range c.all() {
		var env struct {
			Kind string `json:"kind"`
			Data struct {
				Type    string         `json:"type"`
				Payload map[string]any `json:"payload"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &env) != nil || env.Kind != "event" {
			continue
		}
		if env.Data.Type == "agent_phase_completed" && env.Data.Payload != nil {
			out = append(out, env.Data.Payload)
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
	n.publishWake(t, handle, text, "", tc)
}

// wakeInConversation is wake on a named thread, the way a real chat message
// arrives: the notification service stamps the conversation onto the envelope
// and the inbox partitions on it.
//
// Separate from [node.wake] rather than folded into it, because the stamp is
// what the inbox COALESCES on — two stamped wakes are one digest turn, which
// is right for a thread and wrong for the tests that wake a seat twice and
// expect two turns.
func (n *node) wakeInConversation(t *testing.T, handle, text, conversation string) {
	t.Helper()
	n.publishWake(t, handle, text, conversation, events.TraceContext{})
}

// wakeInDirectThread is a message in a thread on a DIRECT MESSAGE — the one
// shape where the two keys differ. The inbox partitions on the thread, because
// that is the batch the reply belongs to, while the conversation is the whole
// DM line, because that is what a person is talking on.
//
// Worth a helper of its own: a wake whose two keys are equal cannot tell a
// reader of either field from a reader of the right one, so every assertion
// about which key something was filed under passes for a frame that copied the
// other.
func (n *node) wakeInDirectThread(t *testing.T, handle, text, channel, thread string) {
	t.Helper()
	n.publishWakeKeyed(t, handle, text, channel+":"+thread, channel, events.TraceContext{})
}

func (n *node) publishWake(t *testing.T, handle, text, conversation string, tc events.TraceContext) {
	t.Helper()
	n.publishWakeKeyed(t, handle, text, conversation, conversation, tc)
}

func (n *node) publishWakeKeyed(t *testing.T, handle, text, partition, conversation string, tc events.TraceContext) {
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
	// BOTH KEYS, as the notification service stamps them. A golden wake
	// that named only the partition would leave the turn's ledger entry
	// filed through the identity read's peer fallback rather than through
	// the field this build stamps — green either way, and silent the day
	// that fallback is the only thing holding it up.
	notify.Stamp(ev, partition, conversation)
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
		return len(phases) == 3 // onboarding, execute, review
	})
	cancel()

	// --- what the model was actually asked ---------------------------- //
	got := n.model.seen()
	for _, want := range []string{"onboarding", "execute", "review"} {
		if !slices.Contains(got, want) {
			t.Errorf("the %s phase never ran; phases = %v", want, got)
		}
	}
	// Onboarding runs BEFORE the executor and on its own budget — that is
	// the whole reason it is a phase rather than a hint inside the
	// executor's prompt, where it could spend the turn's budget on reading
	// and starve submit_work.
	//
	// THE FIRST PHASE, not the first model call. The turn-start prefetch
	// reaches the same endpoint for its own auxiliary passes — the memory
	// filter, the knowledge query, the episode summary — and it runs
	// before any phase by design: the context a turn reasons over is
	// FROZEN before the runner exists, so a self_iterate loop cannot move
	// the system prompt underneath a turn. Those passes are labelled
	// `aux:` for exactly this reason and are not what this invariant is
	// about.
	if first := firstPhase(got); first != "onboarding" {
		t.Errorf("the first phase was %q, not the onboarding pass; calls = %v", first, got)
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
	for _, want := range []string{"onboarding", "execute", "review"} {
		if !phases[want] {
			t.Errorf("no live call named the %s phase; saw %v", want, phases)
		}
	}
	if model, _ := calls[len(calls)-1]["model"].(string); model != "claude-golden" {
		t.Errorf("the live call names model %q, not the one that served it", model)
	}

	// --- THE ENGINE'S OWN HEALTH, WHOLE ----------------------------------- //
	// The push carries the envelope GET /health answers, so a screen reads
	// the applied epoch, the posture and the fleet's size off the socket
	// rather than polling for them. It carried three fields once, and every
	// other fact on the body was a second request at a cadence of its own.
	health := frames.lastHealth(t)
	if health == nil {
		t.Fatal("no health body reached the dashboard")
	}
	// PRESENT rather than positive: this harness seeds its company from a
	// file, which is active before the control plane has minted an epoch,
	// and the body says 0 for exactly that. What must never happen is the
	// field missing, which is the push narrowed back to a few fields.
	if _, present := health["applied_epoch"].(float64); !present {
		t.Errorf("the pushed health carries no applied epoch: %v", health)
	}
	if health["configured"] != true {
		t.Errorf("the pushed health says configured = %v on a configured node",
			health["configured"])
	}
	if health["posture"] != "serve" {
		t.Errorf("the pushed health's posture is %v, want serve", health["posture"])
	}
	if nodes, _ := health["nodes"].(float64); nodes != 1 {
		t.Errorf("the pushed health counts %v nodes on a one-node company, want 1",
			health["nodes"])
	}
	// THE ALARM COUNT THROUGH THE REAL RUNTIME, not a fake one. The API's
	// own tests hand the envelope a runtime that states its alarms, which
	// proves the body and nothing about whether the engine's evaluation
	// reaches it. This company has never taken a backup, so the retention
	// loop's first evaluation — which runs at boot, before any turn — has
	// `backup_age` standing, and a count of zero or an absent field is the
	// wiring between the two dropped.
	alarms, _ := health["alarms"].(map[string]any)
	if alarms == nil {
		t.Errorf("the pushed health carries no alarm count on a node whose "+
			"alarm table has been evaluated: %v", health)
	} else if count, _ := alarms["count"].(float64); count < 1 {
		t.Errorf("the pushed health counts %v alarms on a company that has "+
			"never taken a backup, want backup_age among them", alarms["count"])
	} else if worst, _ := alarms["worst"].(string); worst == "" {
		t.Errorf("the pushed health counts %v alarms and names none of them "+
			"worst: %v", count, alarms)
	}
	for _, floor := range []string{"event_history_seconds", "spend_history_seconds"} {
		if seconds, _ := health[floor].(float64); seconds <= 0 {
			t.Errorf("the pushed health's %s is %v", floor, health[floor])
		}
	}
	// THE BOOT SEED'S COVERAGE THROUGH THE REAL ENGINE. The API's test seeds
	// a LiveState by hand, which proves the field is rendered and nothing
	// about whether `crewlet run`'s seed reaches it: absent here is the seed
	// never running (a live screen booted empty) or its coverage dropped on
	// the way to the envelope, and this node missing from it, or unanswered,
	// is a seed that read its own store over the fan-out and failed.
	self, _ := health["node"].(string)
	seeded, _ := health["seeded_from"].(map[string]any)
	if seeded == nil {
		t.Errorf("the pushed health carries no seeded_from on a node whose live "+
			"state was seeded at boot: %v", health)
	} else {
		answered := false
		nodes, _ := seeded["nodes"].([]any)
		for _, raw := range nodes {
			node, _ := raw.(map[string]any)
			if node["id"] == self && node["answered"] == true {
				answered = true
			}
		}
		if self == "" || !answered {
			t.Errorf("the boot seed's coverage %v does not name this node (%q) as "+
				"answered", seeded, self)
		}
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
	if n, _ := totals["calls"].(float64); n != 3 {
		t.Errorf("the rollup counted %v calls, want one per phase "+
			"(onboarding, execute, review)", totals["calls"])
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

	// --- and how long each phase took --------------------------------- //
	// A PHASE MEASURES ITSELF. The duration used to be derivable only by
	// pairing this record with the `agent_phase_started` that shares its
	// key, and the reader that needs it most — a dashboard deep-linked into
	// a turn WHILE IT RUNS — never has both events: its query was answered
	// before the phase started, and afterwards it buffers only completed
	// envelopes. So the measurement travels on the record, and this is the
	// only place in the suite that proves it survives the publisher, the
	// broker, the broadcast and the JSON round trip rather than existing in
	// a struct literal.
	records := frames.phaseRecords(t)
	if len(records) == 0 {
		t.Fatal("no phase record reached the socket with its payload")
	}
	for _, rec := range records {
		ms, ok := rec["duration_ms"].(float64)
		if !ok {
			t.Errorf("the %v phase record carries no duration_ms at all; the "+
				"field is not on the wire", rec["phase"])
			continue
		}
		if ms <= 0 {
			t.Errorf("the %v phase reports %v ms — a phase that ran three model "+
				"rounds against a live provider took longer than nothing",
				rec["phase"], ms)
		}
	}

	// --- and its timeline -------------------------------------------- //
	// Every round the phase ran and every call it made carries its OWN
	// start and duration, measured by the tool loop — the same survival
	// question as the phase's duration above, for the figures a waterfall
	// is drawn from. A round list shorter than rounds_used is a timeline
	// with a hole in it, and a call with no start cannot be placed at all.
	for _, rec := range records {
		used, _ := rec["rounds_used"].(float64)
		if used == 0 {
			continue
		}
		rounds, _ := rec["rounds"].([]any)
		if len(rounds) != int(used) {
			t.Errorf("the %v phase ran %v rounds and timed %d", rec["phase"], used, len(rounds))
		}
		for _, raw := range rounds {
			round, _ := raw.(map[string]any)
			if at, _ := round["started_at"].(string); at == "" {
				t.Errorf("a %v round carries no start: %v", rec["phase"], round)
			}
			if _, ok := round["duration_ms"].(float64); !ok {
				t.Errorf("a %v round carries no duration: %v", rec["phase"], round)
			}
		}
		if limit, _ := rec["max_rounds"].(float64); limit < used {
			t.Errorf("the %v phase ran %v rounds under a stated cap of %v", rec["phase"], used, limit)
		}
		calls, _ := rec["tool_executions"].([]any)
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			if at, _ := call["started_at"].(string); at == "" {
				t.Errorf("the %v call %v carries no start", rec["phase"], call["name"])
			}
			if origin, _ := call["origin"].(string); origin == "" {
				t.Errorf("the %v call %v names no origin", rec["phase"], call["name"])
			}
		}
	}

	// --- and what the prompt cache served ----------------------------- //
	// The provider reports a cached prefix on every round, so every phase
	// record carries cache counts — and the rollup is folded from TWO
	// producers that must both carry them: the live projection (the
	// `tokens` frame above) and the event store's promoted columns
	// (schema/0032). Either one dropping them reads, on every screen, as a
	// cache that never hit.
	var cacheRead, cacheWrite float64
	keys := map[string]bool{}
	for _, rec := range records {
		r, _ := rec["cache_read_tokens"].(float64)
		w, _ := rec["cache_write_tokens"].(float64)
		cacheRead += r
		cacheWrite += w
		if key, _ := rec["provider_key"].(string); key != "" {
			keys[key] = true
		}
	}
	if cacheRead <= 0 || cacheWrite <= 0 {
		t.Fatalf("the phase records carry %v cached / %v written tokens; the "+
			"provider reported both on every round", cacheRead, cacheWrite)
	}
	if got, _ := totals["cache_read_tokens"].(float64); got != cacheRead {
		t.Errorf("the live rollup counts %v cached tokens, want the records' %v",
			got, cacheRead)
	}
	if got, _ := totals["cache_write_tokens"].(float64); got != cacheWrite {
		t.Errorf("the live rollup counts %v cache-written tokens, want the "+
			"records' %v", got, cacheWrite)
	}
	stored, err := n.engine.Backends().Store.Events().PhaseTokens(t.Context(),
		store.PhaseTokenQuery{SinceDays: 1})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	var storedRead, storedWrite int
	for _, r := range stored {
		storedRead += r.CacheReadTokens
		storedWrite += r.CacheWriteTokens
		if !keys[r.ProviderKey] {
			t.Errorf("a stored phase names provider key %q; the records named %v",
				r.ProviderKey, keys)
		}
	}
	if float64(storedRead) != cacheRead || float64(storedWrite) != cacheWrite {
		t.Errorf("the store holds %d cached / %d written tokens, want the "+
			"records' %v / %v", storedRead, storedWrite, cacheRead, cacheWrite)
	}
}

// --- the client's half ----------------------------------------------------- //

// The replay script, tests/dashboard/js/replay.mjs, drives the dashboard's
// own protocol module, the store and the socket built as
// static/dashboard/protocol.js, over the frames this server produced; it is
// told where that build is through dashboardEnv.
const (
	dashboardEnv  = "CREWLET_DASHBOARD_ROOT"
	replayTimeout = 60 * time.Second

	// replayOperator and replayToken are the founder's credential in the
	// capture: the token's id is the operator id the founder's seat binds,
	// which is what admits them to the act transport as a person.
	replayOperator = "founder"
	replayToken    = "e2e-replay-founder-token-0123456789"
)

// actCapture is one `/operator/act` exchange as the replay receives it: the
// tool and arguments the dashboard would send, the request id it would send
// them under, and the engine's answer, status and body byte for byte.
type actCapture struct {
	Tool      string         `json:"tool"`
	Args      map[string]any `json:"args"`
	RequestID string         `json:"request_id"`
	Status    int            `json:"status"`
	Body      string         `json:"body"`
}

// captureAct makes one real write as the founder, through the route the
// dashboard writes through, and returns the exchange for the replay.
//
// THE SERVER'S HALF OF THE SESSION FLOOR, held here before the client reads
// it: the answer names a position in the grammar a read accepts back, and a
// read of the person's own state AT that position is served at `session` and
// already holds the write. A position the engine then refused as
// `min_position`, or one it served from before the write landed, is a floor
// the dashboard would wait on for ever or wait on for nothing — and nothing
// else in the tree holds the answer and the read to one another.
//
// A PIN, because it wakes nobody: the capture has already ended, and a write
// that routed a notice would start a turn the test then tears down under.
func captureAct(t *testing.T, n *node) []byte {
	t.Helper()
	authed := func(method, path string, body []byte) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method,
			n.server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+replayToken)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := n.server.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("%s %s: reading the answer: %v", method, path, err)
		}
		return res.StatusCode, raw
	}

	// WHO THE TOKEN IS, and that the node offers them the write: the viewer
	// names the act transport's verbs from the same surface that serves it.
	status, raw := authed(http.MethodGet, "/viewer", nil)
	var viewer struct {
		Handle string   `json:"handle"`
		Acts   []string `json:"acts"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &viewer) != nil || viewer.Handle == "" {
		t.Fatalf("the founder's token names no seat: %d %s", status, raw)
	}
	exchange := actCapture{
		Tool: "set_pins",
		Args: map[string]any{"favorites": map[string]any{
			"add": []any{map[string]any{"kind": "project", "id": "ENG"}},
		}},
		RequestID: uuid.Must(uuid.NewV7()).String(), // the act transport requires a UUIDv7
	}
	if !slices.Contains(viewer.Acts, exchange.Tool) {
		t.Fatalf("the viewer does not offer %s to the founder (acts %v)",
			exchange.Tool, viewer.Acts)
	}

	body, err := json.Marshal(map[string]any{
		"request_id": exchange.RequestID, "args": exchange.Args,
	})
	if err != nil {
		t.Fatalf("encoding the act request: %v", err)
	}
	exchange.Status, raw = authed(http.MethodPost, "/operator/act/"+exchange.Tool, body)
	exchange.Body = string(raw)
	var answer struct {
		Outcome  string `json:"outcome"`
		Position string `json:"position"`
	}
	if exchange.Status != http.StatusOK || json.Unmarshal(raw, &answer) != nil {
		t.Fatalf("the founder's %s was not answered: %d %s", exchange.Tool,
			exchange.Status, raw)
	}
	if answer.Outcome != "applied" && answer.Outcome != "pending" {
		t.Fatalf("the founder's %s came to %q, want applied or pending: %s",
			exchange.Tool, answer.Outcome, raw)
	}
	if _, err := tracker.ParseLogPosition(answer.Position); err != nil {
		t.Fatalf("the act answer's position is not one a read accepts back: %v", err)
	}

	// THE READ AT THE FLOOR: served at `session`, and holding the write.
	query := url.Values{"read_level": {"session"}, "min_position": {answer.Position}}
	status, raw = authed(http.MethodGet,
		"/work/people/"+url.PathEscape(viewer.Handle)+"?"+query.Encode(), nil)
	var person struct {
		Level     string             `json:"read_level"`
		Favorites []tracker.Favorite `json:"favorites"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &person) != nil {
		t.Fatalf("a read at the act's floor was not served: %d %s", status, raw)
	}
	if person.Level != "session" {
		t.Errorf("a read at the act's floor was served at %q, want session", person.Level)
	}
	if !slices.Contains(person.Favorites, tracker.Favorite{Kind: "project", ID: "ENG"}) {
		t.Errorf("a read at the act's floor does not hold the write: favorites %v",
			person.Favorites)
	}

	out, err := json.Marshal(exchange)
	if err != nil {
		t.Fatalf("encoding the act capture: %v", err)
	}
	return out
}

func TestTheDashboardClientCanReadWhatThisServerSends(t *testing.T) {
	// THE OTHER HALF OF THE GATE. The test above asserts the frames say the
	// right things; this one asserts the CLIENT can read them — and those
	// are different questions, which is the entire lesson of the bug that
	// prompted it.
	//
	// The `agents` push was going out as an object keyed by role. Every
	// field in it was correct. The server's tests asserted that shape and
	// passed; the dashboard's own suites passed; the socket delivered every
	// frame. And the store guards applyAgents with Array.isArray, so it
	// dropped all of them, and a company running a full turn rendered idle
	// from the first phase to the last. Nothing on either side could see
	// it, because nothing on either side ran both.
	//
	// The client is the compatibility reference and wins any disagreement
	// — so a failure here is the SERVER's.
	node := nodeBinary(t)
	// A CEILING the company's turn cannot reach, so the capture carries the
	// live token meters as well: the `budget` push is a frame the client
	// folds like any other, and one only a capped company is sent.
	//
	// AND A SECOND SEAT WHOSE OWN DAY IS SMALLER THAN ONE MODEL CALL, so the
	// capture carries a window the gate has actually REFUSED. A meter that is
	// only ever `ok` proves the happy half of the frame and nothing about
	// `refused_at` — the field that was once dropped on its way from the
	// counter to the push, which left every "refusing charges" row the
	// dashboard draws unreachable. The refusal is the engine's own, made by
	// the gate a real turn charges through; the seat is its own scope, so
	// the company's turn beside it is charged against nothing it spent.
	//
	// AND A PERSON WHO CAN WRITE: the founder's seat binds a bearer token, so
	// the capture can end with a real `/operator/act` answer — the one the
	// dashboard's session floor is raised from.
	n := startBooted(t, func(doc string) string {
		doc = strings.Replace(doc, "roles:\n", "roles:\n"+
			"  - name: CFO\n"+
			"    handle: cfo\n"+
			"    llm: scripted\n"+
			"    token_budget: {day: 100}\n", 1)
		doc = strings.Replace(doc, "      slack_user_id: U0FOUNDER\n",
			"      slack_user_id: U0FOUNDER\n"+
				"      crewlet_operator_id: "+replayOperator+"\n", 1)
		return doc + "\ntoken_budget: {day: 100000000}\n"
	}, func(boot *config.Bootstrap) {
		boot.API.Auth.Tokens = []config.APIToken{{ID: replayOperator, Token: replayToken}}
	})

	waitFor(t, "the seats to be claimed", func() bool {
		held := n.engine.Node().Host().Held()
		return slices.Contains(held, "ceo") && slices.Contains(held, "cfo")
	})
	waitFor(t, "the native backends to hydrate", n.engine.NativeHydrated)
	conn := n.dial(t)
	frames := &capture{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go read(ctx, conn, frames)
	waitFor(t, "the snapshot", func() bool {
		return slices.Contains(frames.kinds(t), "snapshot")
	})

	// THE REFUSAL FIRST, and settled on the counter's own stamp, so the
	// budget push waited for below is one published after it.
	n.wake(t, "cfo", "What did we spend this week?")
	budgets := n.engine.Backends().Fleet
	company := n.engine.Company()
	cfoID, _ := company.Org.AgentIDFor(company.Org.AgentSeatByHandle("cfo"))
	waitFor(t, "the seat's own ceiling to refuse a charge", func() bool {
		got, err := budgets.Used(t.Context(), coord.AgentScope(cfoID.String()),
			coord.WindowsAt(time.Now(), time.UTC))
		return err == nil && !got.In(period.Day).RefusedAt.IsZero()
	})

	// THE COMPANY'S TURN IS ON A WORK ITEM, woken the way a task wakes its
	// assignee: filed to the tracker, applied, and routed by the change feed
	// to the seat it names. A chat wake names no item, so every
	// `live_call.work_item` this capture held was null and the client's
	// reading of the field went untested — the one field every task page,
	// turn row and live row now keys its link on.
	task := newTask("ENG", "Summarise how the week went")
	task.Assignee = "ceo"
	// WITH ITS WAKE, built the way every create surface builds one: a
	// record carrying no notification is a change nobody asked to hear about.
	wake := tracker.Wake{Kind: tracker.ChangeCreated, After: task}.Notify(nil)
	filed, err := operator(t, n).CreateTask(t.Context(), "e2e-replay-item", task, wake)
	if err != nil {
		t.Fatalf("file the task that wakes the seat: %v", err)
	}
	// BOTH TURNS, the refused seat's and the company's — counted rather than
	// matched on the item, so a live call that stopped naming it fails the
	// assertion below by name instead of timing this wait out.
	waitFor(t, "both seats' turns to complete", func() bool {
		ended := 0
		for _, kind := range frames.eventTypes(t) {
			if kind == "agent_turn_completed" {
				ended++
			}
		}
		return ended >= 2
	}, func() string {
		var items []any
		for _, call := range frames.liveCalls(t) {
			items = append(items, call["work_item"])
		}
		return fmt.Sprintf("events %v; live calls' items %v; filed %s (%s); model saw %v",
			frames.eventTypes(t), items, filed.Key, filed.Outcome, n.model.seen())
	})
	// The meters are published on engine.BudgetReportInterval, so one
	// carrying the refusal lands within an interval of it at the latest.
	waitFor(t, "a budget push carrying the refusal", func() bool {
		return frames.sawRefusal(t, "CFO")
	})
	cancel()

	// --- what the frames say, before the client reads them ------------- //
	// The server's half of each field the replay then holds the client to,
	// so a red replay can be told apart from a server that never sent it.
	calls := frames.liveCalls(t)
	var onItem, capped int
	for _, call := range calls {
		if item, _ := call["work_item"].(map[string]any); item != nil {
			onItem++
			// READ AS A STRING, like the other two: a missing key decodes
			// to nil, which is not "" and would pass an equality test.
			id, _ := item["id"].(string)
			if item["key"] != filed.Key || item["project"] != "ENG" || id == "" {
				t.Errorf("a live call names work item %v, want %s in ENG", item, filed.Key)
			}
		}
		if limit, _ := call["max_rounds"].(float64); limit > 0 {
			capped++
		}
	}
	if onItem == 0 {
		t.Errorf("no live call named the work item %s its turn was woken for", filed.Key)
	}
	if capped == 0 {
		t.Error("no live call stated its round cap, so no running phase can say " +
			"how far through it is")
	}
	stages := frames.turnStages(t)
	if !slices.Contains(stages, "phase") {
		t.Errorf("no agents push put a seat's turn in the `phase` stage; saw %v", stages)
	}
	for _, stage := range stages {
		if !slices.Contains([]string{"context", "phase", "parked"}, stage) {
			t.Errorf("a turn's stage %q is not one the engine defines", stage)
		}
	}

	// THE FLEET AND ITS ALARMS, the server's half of what the replay holds
	// the health card to: a live node counted, and a standing alarm with its
	// worst severity — the company has never taken a backup.
	health := frames.lastHealth(t)
	if nodes, _ := health["nodes"].(float64); nodes < 1 {
		t.Errorf("the last health push counts %v live nodes, want at least this one",
			health["nodes"])
	}
	alarms, _ := health["alarms"].(map[string]any)
	count, _ := alarms["count"].(float64)
	worst, _ := alarms["worst"].(string)
	if count < 1 || worst == "" {
		t.Errorf("the last health push carries no standing alarm with a worst "+
			"severity (%v) on a company that has never taken a backup", alarms)
	}

	// --- a write, and the floor it raises ------------------------------- //
	answer := captureAct(t, n)

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

	root := sourcetree.Root(t)
	tree := filepath.Join(root, "static", "dashboard")
	script := filepath.Join(root, "tests", "dashboard", "js", "replay.mjs")

	runCtx, runCancel := context.WithTimeout(t.Context(), replayTimeout)
	defer runCancel()
	actPath := filepath.Join(t.TempDir(), "act.json")
	if err := os.WriteFile(actPath, answer, 0o600); err != nil {
		t.Fatalf("writing the act answer: %v", err)
	}
	cmd := exec.CommandContext(runCtx, node, script, path, actPath)
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
// In CI it is a red build — this is the only place the two halves of the wire
// protocol are checked against each other, so letting it go quiet retires the
// check behind a green tick. Elsewhere a missing node skips, and that skip is
// deliberately NOT declared in `internal/skipgate/allowed.go`: `make test-solo`
// refuses to start without node (`require-node`), and a run that got past it
// anyway fails on the undeclared skip rather than reporting a pass. Only a
// bare `go test ./internal/e2e/` — the fast loop, not a gate — skips quietly.
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
	// The catalogue an executor is SHOWN, and the surface it can actually
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
				"it and no executor is told it exists", want)
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
	// turn passed nil, so a company with a `token_budget:` ceiling spent
	// without limit and the number in its config was decoration. Money
	// leaves the building for every token, so this is the one counter that
	// fails CLOSED — a charge that cannot be made stops the round rather
	// than silently un-capping the company.
	//
	// THE CAP leaves room for the turn-start prefetch's knowledge query and
	// ONE round of the turn's own loop, and not for a second — so the cap
	// bites partway through the turn rather than before it starts, which is
	// the case a pre-flight check would miss.
	aux, round := textReplyUsage.tokens(), toolUseUsage.tokens()
	limit := aux + round + round/2
	n := startWith(t, func(doc string) string {
		return doc + fmt.Sprintf("\ntoken_budget: {day: %d}\n", limit)
	})
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "ceo")
	})
	n.wake(t, "ceo", "How did the week go?")

	// SETTLED ON THE REFUSAL, which the gate records on the scope that
	// made it. The second charge is attempted only after the first has
	// returned, so once the company has refused one both counters are
	// final. Waiting on the org's counter alone read it between the
	// org's write and the seat's (a charge is two writes, org first), and
	// failed a correct engine on a loaded machine with the seat at 0.
	//
	// Read against the company's day, which is UTC for a company that names
	// no clock: the day the cap is written for.
	budgets := n.engine.Backends().Fleet
	today := func() coord.Windows { return coord.WindowsAt(time.Now(), time.UTC) }
	waitFor(t, "the company cap to refuse a charge", func() bool {
		rows, err := budgets.Usage(t.Context(), today())
		if err != nil {
			return false
		}
		for _, row := range rows {
			if row.Scope == coord.OrgScope {
				return !row.In(period.Day).RefusedAt.IsZero()
			}
		}
		return false
	})

	orgToday, err := budgets.Used(t.Context(), coord.OrgScope, today())
	if err != nil {
		t.Fatalf("used: %v", err)
	}
	used := orgToday.In(period.Day).Used
	// THE CAP GOVERNS WHAT THE LOOP ADMITS, and only that. The counter also
	// holds every AUXILIARY completion — the prefetch's knowledge query
	// here, a reflection pass after a turn — and those are RECORDED whole
	// after they return, past the ceiling included, because their size is
	// known only from the answer and no refusal can un-spend them
	// (coord.Budgets.PostCharge). So `used <= limit` is not the engine's
	// promise, and it failed a correct engine the moment the prefetch was
	// charged. The promise is that no charge the loop's gate ADMITTED took
	// the company past its cap: what was recorded before the loop began
	// counts against the room its rounds had, and only what was recorded
	// after the loop's first call can stand above the cap.
	//
	// Ordered by what the model ANSWERED, read after the counter: the turn
	// is sequential, so a completion answered before the loop's first call
	// was recorded before any of its rounds, and an auxiliary call answered
	// after it but not yet recorded only makes the bound looser, never one
	// a correct engine fails.
	calls := n.model.seen()
	loopBegan, auxAfter := false, 0
	for _, call := range calls {
		switch {
		case !strings.HasPrefix(call, "aux:"):
			loopBegan = true
		case loopBegan:
			auxAfter++
		}
	}
	if !loopBegan {
		t.Fatalf("the cap refused a charge but the model answered no round of "+
			"the turn's own loop: %v", calls)
	}
	if atLastAdmit := used - auxAfter*aux; atLastAdmit > limit {
		t.Errorf("the turn loop admitted charges up to %d against a cap of %d a "+
			"day (the counter reads %d, %d of it recorded by auxiliary calls "+
			"after the loop began); model calls %v",
			atLastAdmit, limit, used, auxAfter*aux, calls)
	}
	// And the SEAT's counter moved with it: one charge, both scopes.
	//
	// WAITED FOR rather than read once, because the two counters are two
	// KEYS AND NO TRANSACTION — [coord/kv.FleetStore.Charge] says so and
	// builds the all-or-nothing property out of ordering instead: the org
	// is charged first and compensated if the seat then refuses. So there
	// is a real window in which the org has moved and the seat has not,
	// and the wait above lands inside it whenever the org's bump is what
	// satisfied it.
	//
	// Read synchronously, this asserted an ATOMICITY the design does not
	// claim, and CI caught it: `seat spent 0 and the org 150`. The
	// invariant is that the two agree once the charge is through, which is
	// what this now says.
	//
	// BOTH READ ON EVERY POLL rather than the seat against the org figure
	// read above: an auxiliary pass after the turn — a reflection — is
	// recorded on both scopes too, and a seat chasing a frozen org figure
	// would overshoot it and never catch it.
	company := n.engine.Company()
	id, _ := company.Org.AgentIDFor(company.Org.AgentSeatByHandle("ceo"))
	var seatUsed, orgUsed int
	waitFor(t, "the seat's counter to catch the org's", func() bool {
		orgNow, err := budgets.Used(t.Context(), coord.OrgScope, today())
		if err != nil {
			return false
		}
		got, err := budgets.Used(t.Context(), coord.AgentScope(id.String()), today())
		if err != nil {
			return false
		}
		orgUsed, seatUsed = orgNow.In(period.Day).Used, got.In(period.Day).Used
		return seatUsed == orgUsed
	}, func() string {
		return fmt.Sprintf("seat spent %d and the org %d; one charge must "+
			"move both", seatUsed, orgUsed)
	})
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

// firstPhase is the first non-auxiliary call the model saw, or "" for none.
func firstPhase(calls []string) string {
	for _, call := range calls {
		if !strings.HasPrefix(call, "aux:") {
			return call
		}
	}
	return ""
}
