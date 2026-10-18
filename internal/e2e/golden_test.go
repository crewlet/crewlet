package e2e

import (
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// GATE G5, third leg — "a golden company runs end-to-end with the UI showing
// live turns".
//
// Every other test in this tree stops at a seam. This one starts a real engine
// on a real broker, wakes a real seat with a real trigger, drives a real
// executor and reviewer loop against a scripted model behind the real CLI
// backend, and reads the result off a WebSocket dialled the way the dashboard
// dials it, then feeds those exact frames through the dashboard's OWN protocol
// module (the store and the socket, built as static/dashboard/protocol.js).
//
// It is the only test that can catch the class of bug it was written for. The
// turn engine emitted NO events at all when this was written: every payload
// type existed, the projection keyed on all of them, the socket fanned them
// out, and the dashboard rendered them — and nothing in between ever published
// one, so a running company showed an empty dashboard for ever. Every
// component's own tests passed.

// dial opens a socket on the path the dashboard's LiveSocket dials, as the
// deployment's own token — a script, acting as itself, which is shown no chat;
// [dialAs] is a person's tab.
func (n *node) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	target := "ws" + strings.TrimPrefix(n.server.URL, "http") + "/ws/stream"
	conn, _, err := websocket.Dial(t.Context(), target, socketOptions())
	if err != nil {
		t.Fatalf("dial %s: %v", target, err)
	}
	// The frames a live turn produces are far larger than the default cap:
	// a phase completion carries its prompts verbatim.
	conn.SetReadLimit(8 << 20)
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

// capture holds every frame a socket delivered, in order.
//
// It keeps the RAW bytes, not decoded maps: they are replayed verbatim through
// the dashboard's own client, and decoding and re-encoding here would mean the
// client was fed this test's re-serialization rather than the server's output.
//
// AND EACH FRAME DECODED ONCE, as it arrives, for the readers below — which
// a case polls every 20 ms for as long as it waits. Decoded on every poll
// instead, a capture of several hundred frames under the race detector cost
// about a core for the whole wait, which every case running beside it paid.
type capture struct {
	mu     sync.Mutex
	frames []frame
}

// frame is one captured frame: its bytes, and the parts of its envelope the
// readers ask for.
type frame struct {
	raw []byte
	// kind is the envelope's, set when the envelope decoded (enveloped);
	// the parts below are set when this kind's data decoded (decoded) —
	// the two conditions each reader's own decode used to apply.
	kind      string
	enveloped bool
	decoded   bool
	// event is an `event` frame's type and payload.
	eventType string
	payload   map[string]any
	// rows is an `agents` frame's seat rows.
	rows []map[string]any
	// body is a `tokens`, `health` or `snapshot` frame's data.
	body map[string]any
	// chat is a chat change frame's data (every chat push kind but the
	// two below), presence a `chat_presence` frame's and read a
	// `chat_cursor_moved` frame's.
	chat     stream.ChatChange
	presence stream.ChatPresenceFrame
	read     coord.PersonChatRead
}

// decodeFrame reads one frame's envelope. A frame that does not decode keeps
// its bytes and nothing else, exactly as the readers treated one before: the
// replay is what holds the client to every byte.
func decodeFrame(raw []byte) frame {
	f := frame{raw: slices.Clone(raw)}
	var env struct {
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return f
	}
	f.kind, f.enveloped = env.Kind, true
	switch env.Kind {
	case "event":
		var ev struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal(env.Data, &ev) == nil {
			f.eventType, f.payload, f.decoded = ev.Type, ev.Payload, true
		}
	case "agents":
		f.decoded = json.Unmarshal(env.Data, &f.rows) == nil
	case "tokens", "health", "snapshot":
		f.decoded = json.Unmarshal(env.Data, &f.body) == nil
	case string(stream.KindChatPresence):
		f.decoded = json.Unmarshal(env.Data, &f.presence) == nil
	case string(stream.KindChatCursorMoved):
		f.decoded = json.Unmarshal(env.Data, &f.read) == nil
	default:
		if slices.Contains(stream.ChatPushKinds(), stream.Kind(env.Kind)) {
			f.decoded = json.Unmarshal(env.Data, &f.chat) == nil
		}
	}
	return f
}

func (c *capture) add(raw []byte) {
	f := decodeFrame(raw)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frames = append(c.frames, f)
}

// snapshot is every frame captured so far, in order. The frames themselves
// are never written after add, so the copy can be read without the lock.
func (c *capture) snapshot() []frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.frames)
}

// all is every captured frame's bytes, in order.
func (c *capture) all() [][]byte {
	frames := c.snapshot()
	out := make([][]byte, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.raw)
	}
	return out
}

// kinds reports the envelope kind of every captured frame, in order.
func (c *capture) kinds(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, f := range c.snapshot() {
		if f.enveloped {
			out = append(out, f.kind)
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
	for _, row := range c.agentRows(t) {
		if call, ok := row["live_call"].(map[string]any); ok && call != nil {
			out = append(out, call)
		}
	}
	return out
}

// agentRows reports every seat row an `agents` push carried, in order.
func (c *capture) agentRows(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, f := range c.snapshot() {
		if f.kind == "agents" && f.decoded {
			out = append(out, f.rows...)
		}
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
// stamped, and judged `refusing` by the engine. By the seat's AGENT ID, the
// one key an `agents` push carries its changed rows under and the client
// merges them on.
func (c *capture) sawRefusal(t *testing.T, agentID string) bool {
	t.Helper()
	for _, row := range c.agentRows(t) {
		if row["agent_id"] != agentID {
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

// seatStates reports every activity an `agents` push put a seat in, by the
// agent id the client merges the row onto its roster by.
func (c *capture) seatStates(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, row := range c.agentRows(t) {
		id, _ := row["agent_id"].(string)
		activity, _ := row["activity"].(string)
		if id != "" && activity != "" {
			out[id] = append(out[id], activity)
		}
	}
	return out
}

// rosterIDs reports the agent ids the snapshot's roster carries — the rows an
// `agents` push is merged onto.
func (c *capture) rosterIDs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, raw := range c.all() {
		var env struct {
			Kind string `json:"kind"`
			Data struct {
				Agents []map[string]any `json:"agents"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &env) != nil || env.Kind != "snapshot" {
			continue
		}
		for _, row := range env.Data.Agents {
			if id, _ := row["agent_id"].(string); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// seatAgentID is a seat's agent id on the running company.
func seatAgentID(t *testing.T, n *node, handle string) string {
	t.Helper()
	c := n.engine.Company()
	if c == nil || c.Org == nil {
		t.Fatalf("the node runs no company, so seat %q has no agent id", handle)
	}
	id, ok := c.Org.AgentIDFor(c.Org.AgentSeatByHandle(handle))
	if !ok {
		t.Fatalf("seat %q is no agent seat in the running company", handle)
	}
	return id.String()
}

// lastRollup returns the most recent `tokens` frame's payload, or nil.
func (c *capture) lastRollup(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	for _, f := range c.snapshot() {
		if f.kind == "tokens" && f.decoded {
			out = f.body
		}
	}
	return out
}

// lastHealth reports the newest health body the socket carried — a snapshot's
// or a tick's, since both carry the whole envelope.
func (c *capture) lastHealth(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	for _, f := range c.snapshot() {
		if !f.decoded {
			continue
		}
		switch f.kind {
		case "snapshot":
			if health, ok := f.body["health"].(map[string]any); ok {
				out = health
			}
		case "health":
			out = f.body
		}
	}
	return out
}

// eventTypes reports the type of every `event` frame, in order.
func (c *capture) eventTypes(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, f := range c.snapshot() {
		if f.kind == "event" && f.decoded {
			out = append(out, f.eventType)
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
	for _, f := range c.snapshot() {
		if f.kind == "event" && f.decoded && f.eventType == "agent_phase_completed" &&
			f.payload != nil {
			out = append(out, f.payload)
		}
	}
	return out
}

// chatChanges reports every committed chat record a frame announced in one
// room, in arrival order.
func (c *capture) chatChanges(room string) []stream.ChatChange {
	var out []stream.ChatChange
	for _, f := range c.snapshot() {
		if f.decoded && f.chat.ChannelID == room {
			out = append(out, f.chat)
		}
	}
	return out
}

// sawPresence reports whether any presence frame showed one room as match
// accepts it.
func (c *capture) sawPresence(room string, match func(stream.ChatRoomPresence) bool) bool {
	for _, f := range c.snapshot() {
		if f.kind != string(stream.KindChatPresence) || !f.decoded {
			continue
		}
		if p, ok := f.presence.Rooms[room]; ok && match(p) {
			return true
		}
	}
	return false
}

// chatReads reports every read state a frame carried, in order.
func (c *capture) chatReads() []coord.PersonChatRead {
	var out []coord.PersonChatRead
	for _, f := range c.snapshot() {
		if f.kind == string(stream.KindChatCursorMoved) && f.decoded {
			out = append(out, f.read)
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
	// BOTH KEYS, as the notification service stamps them: the identity is
	// read from its own field alone, so a wake naming only the partition
	// names no conversation.
	notify.Stamp(ev, partition, conversation)
	if err := n.engine.Backends().Queue.Publish(t.Context(),
		seatInbox(t, n, handle), ev); err != nil {
		t.Fatalf("wake %s: %v", handle, err)
	}
}

func TestAGoldenCompanyRunsATurnOntoTheDashboard(t *testing.T) {
	t.Parallel()
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
	//
	// EACH OF THE TURN'S PHASES BY NAME, never a count of rows: the rollup
	// also draws the AUXILIARY model's spend under its own band, and a count
	// that said "three" read the turn-start context's records — which the
	// rollup now holds — as a turn with a phase too many.
	phasesOf := func() map[string]bool {
		have := map[string]bool{}
		r := frames.lastRollup(t)
		if r == nil {
			return have
		}
		rows, _ := r["by_phase"].([]any)
		for _, row := range rows {
			if m, ok := row.(map[string]any); ok {
				name, _ := m["phase"].(string)
				have[name] = true
			}
		}
		return have
	}
	waitFor(t, "the spend rollup for the whole turn", func() bool {
		have := phasesOf()
		return have["onboarding"] && have["execute"] && have["review"]
	})
	// AND WHAT ITS CONTEXT COST. The turn-start passes are the turn's own
	// auxiliary records, flushed as the turn ends, so the rollup draws them
	// under the Auxiliary band — where they reached no spend figure at all
	// before, while the counter was charged for them.
	if slices.ContainsFunc(n.model.seen(), func(call string) bool {
		return call == "aux:memory" || call == "aux:knowledge" || call == "aux:recall"
	}) {
		waitFor(t, "the turn's auxiliary spend in the rollup", func() bool {
			return phasesOf()["auxiliary"]
		})
	}
	cancel()

	// --- what the model was actually asked ---------------------------- //
	got := n.model.seen()
	for _, want := range []string{"onboarding", "execute", "review"} {
		if !slices.Contains(got, want) {
			t.Errorf("the %s phase never ran; phases = %v", want, got)
		}
	}
	// EACH ON ITS ROUTE: a seat's rounds keep the CLI's own prompt and add
	// the seat's after it, and an auxiliary pass replaces it, having no use
	// for a coding agent's instructions.
	if bad := n.model.misrouted(); len(bad) > 0 {
		t.Errorf("system prompts on the wrong channel: %s", strings.Join(bad, "; "))
	}
	// Onboarding runs BEFORE the executor and on its own budget — that is
	// the whole reason it is a phase rather than a hint inside the
	// executor's prompt, where it could spend the turn's budget on reading
	// and starve submit_work.
	//
	// THE FIRST PHASE, not the first model call. The turn-start prefetch
	// reaches the same model for its own auxiliary passes — the memory
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
	ceo := seatAgentID(t, n, "ceo")
	states := frames.seatStates(t)
	if !slices.Contains(states[ceo], "working") {
		t.Errorf("the seat never showed as working; states = %v", states)
	}
	// AND EVERY PUSHED ROW NAMES A SEAT THE ROSTER CARRIES, by the one key
	// the client merges on. A push keyed by anything the roster row does not
	// also carry — the role name it used to be — reaches no row: two seats
	// sharing a name share one, and a renamed seat's in-flight rows reach
	// none, while every assertion above still passes.
	roster := frames.rosterIDs(t)
	if !slices.Contains(roster, ceo) {
		t.Errorf("the snapshot's roster does not carry the CEO's agent id; roster = %v", roster)
	}
	for id := range states {
		if !slices.Contains(roster, id) {
			t.Errorf("an agents push moved %q, which no roster row carries — "+
				"the client has nothing to merge it onto", id)
		}
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
	// THE TURN'S PHASES, apart from the auxiliary band: the rollup also holds
	// what the auxiliary model spent — the turn-start context, and the
	// reflection's calls once the ledger flushes them — which no phase record
	// carries, so every comparison with the phase records below is made over
	// the phases' own rows.
	phaseTotals := map[string]float64{}
	byPhase, _ := rollup["by_phase"].([]any)
	for _, raw := range byPhase {
		row, _ := raw.(map[string]any)
		if row["phase"] == tokens.PhaseAuxiliary {
			continue
		}
		for _, key := range []string{"calls", "cache_read_tokens", "cache_write_tokens"} {
			v, _ := row[key].(float64)
			phaseTotals[key] += v
		}
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
	// ONE CALL PER PROVIDER CALL: a phase's rounds, as every spend figure
	// counts them (node/0040).
	var wantCalls int
	for _, rec := range records {
		rounds, _ := rec["rounds"].([]any)
		used, _ := rec["rounds_used"].(float64)
		wantCalls += tokens.PhaseCalls(len(rounds), int(used))
	}
	if got := phaseTotals["calls"]; got != float64(wantCalls) {
		t.Errorf("the rollup counted %v calls over the turn's phases, want the %d "+
			"provider calls their records state", got, wantCalls)
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
			// EVERY CALL UNDER THE ID ITS REQUEST GAVE IT: Claude Code hands
			// the server its tool_use id with each call, which is how the
			// engine joins a call to the request that asked for it, and the
			// id a running call and its answered row are matched on.
			if id, _ := call["call_id"].(string); !strings.HasPrefix(id, "toolu_") {
				t.Errorf("the %v call %v carries call_id %q, not the tool_use id its request "+
					"named", rec["phase"], call["name"], call["call_id"])
			}
		}
		// ROUNDS ARE MODEL REQUESTS, AND THE CLI RUNS THAT MADE THEM NAME
		// THEM. A phase's loop is its CLI's own, so the record carries each
		// run beside the requests: one contiguous stretch of rounds per run,
		// together every round the phase used — each run having shown its
		// requests, as Claude Code's record does — and the model that served.
		covered := 0
		invocations, _ := rec["invocations"].([]any)
		for _, raw := range invocations {
			run, _ := raw.(map[string]any)
			first, _ := run["first_round"].(float64)
			last, _ := run["last_round"].(float64)
			if first == 0 && last == 0 {
				// A run that made no request before it ended.
				continue
			}
			if int(first) != covered+1 || last < first {
				t.Errorf("a %v CLI run made rounds %v to %v after rounds up to %d were "+
					"another's: %v", rec["phase"], first, last, covered, run)
			}
			covered = int(last)
			if run["requests_observed"] != true || run["model"] != "claude-golden" {
				t.Errorf("a %v CLI run reads %v, want one that showed its requests, served "+
					"by claude-golden", rec["phase"], run)
			}
		}
		if covered != int(used) {
			t.Errorf("the %v phase used %v rounds and its CLI runs %v made %d", rec["phase"],
				used, invocations, covered)
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
	if got := phaseTotals["cache_read_tokens"]; got != cacheRead {
		t.Errorf("the live rollup counts %v cached tokens over the phases, want the "+
			"records' %v", got, cacheRead)
	}
	if got := phaseTotals["cache_write_tokens"]; got != cacheWrite {
		t.Errorf("the live rollup counts %v cache-written tokens over the phases, "+
			"want the records' %v", got, cacheWrite)
	}
	stored, err := n.engine.Backends().Store.Events().PhaseTokens(t.Context(),
		store.PhaseTokenQuery{SinceDays: 1, At: time.Now()})
	if err != nil {
		t.Fatalf("phase tokens: %v", err)
	}
	var storedRead, storedWrite int
	for _, r := range stored {
		if r.Phase == tokens.PhaseAuxiliary {
			// The auxiliary model's records, held to their own cases.
			continue
		}
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

// The replay script beside this file, replayScript, drives the dashboard's
// own protocol module, the store and the socket built as
// static/dashboard/protocol.js, over the frames this server produced; it is
// told where that build is through dashboardEnv. The script is named relative
// to this package's directory, where `go test` runs every test binary by
// contract, so its path has no depth to count.
const (
	replayScript  = "replay.mjs"
	dashboardEnv  = "CREWLET_DASHBOARD_ROOT"
	replayTimeout = 60 * time.Second
)

// captureAct makes one real write as a person signed in to the identity
// estate, through the route the dashboard writes through, and returns the
// exchange for the replay.
//
// AS THE PERSON THE SESSION RESOLVES TO, and nothing else: the browser holds
// the cookie a password sign-in minted, presents the deployment's Origin, and
// sends its own operation key — exactly what the dashboard's `act` sends. No
// seat is bound to a credential in the company document; the person is bound
// to their seat in the identity directory, which is what makes the write
// theirs.
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
func captureAct(t *testing.T, n *node, b *browser) actCapture {
	t.Helper()
	const tool = "set_pins"
	who := viewerOf(t, n, b, tool)
	if who.Handle != janeSeat {
		t.Fatalf("the session acts as seat %q, want %s at %s", who.Handle, janeLogin, janeSeat)
	}
	exchange := b.act(n, tool, map[string]any{"favorites": map[string]any{
		"add": []any{map[string]any{"kind": "project", "id": "ENG"}},
	}})
	// NOT WAITED FOR HERE: the floor is the claim, so the read below is what
	// has to wait on it.
	answer := answerOf(t, exchange)
	positionOf(t, answer)

	// THE READ AT THE FLOOR: served at `session`, and holding the write.
	query := url.Values{"read_level": {"session"}, "min_position": {*answer.Position}}
	status, raw := b.sendRaw(n, http.MethodGet,
		"/worklet/people/"+url.PathEscape(who.Handle)+"?"+query.Encode(), "", nil)
	var record struct {
		Level     string             `json:"read_level"`
		Favorites []worklet.Favorite `json:"favorites"`
	}
	if status != http.StatusOK || json.Unmarshal(raw, &record) != nil {
		t.Fatalf("a read at the act's floor was not served: %d %s", status, raw)
	}
	if record.Level != "session" {
		t.Errorf("a read at the act's floor was served at %q, want session", record.Level)
	}
	if !slices.Contains(record.Favorites, worklet.Favorite{Kind: "project", ID: "ENG"}) {
		t.Errorf("a read at the act's floor does not hold the write: favorites %v",
			record.Favorites)
	}
	return exchange
}

// viewer is what `/viewer` answers a person: the seat they act as, and the
// act transport's verbs the node offers them.
type viewer struct {
	Handle string   `json:"handle"`
	Acts   []string `json:"acts"`
}

// viewerOf asks the node who b is signed in as, failing the case unless it
// names a seat and offers every tool the case is about to press: the viewer
// names the act transport's verbs from the same surface that serves them.
func viewerOf(t *testing.T, n *node, b *browser, tools ...string) viewer {
	t.Helper()
	status, raw := b.sendRaw(n, http.MethodGet, "/viewer", "", nil)
	var who viewer
	if status != http.StatusOK || json.Unmarshal(raw, &who) != nil || who.Handle == "" {
		t.Fatalf("the session names no seat: %d %s", status, raw)
	}
	for _, tool := range tools {
		if !slices.Contains(who.Acts, tool) {
			t.Fatalf("the viewer does not offer %s to %s (acts %v)", tool, who.Handle,
				who.Acts)
		}
	}
	return who
}

// chatCapture is what the founder's tab did in the company's own chat while
// the frames were captured, which the replay holds the client's store to.
//
// NOTHING IN IT IS READ FROM THE FRAMES. What the engine minted is read from
// what it answered or holds — the room's id from the create's receipt, the
// person from `/viewer`, a message's id from its post's receipt or the room's
// transcript, the numbering from that transcript, the last record's position
// from the prune's answer — and everything else is what this capture did,
// written down as it did it. So a frame that said the wrong thing is caught
// by the comparison rather than copied into the expectation.
type chatCapture struct {
	// Room is the room the founder made and talked in, and Person and Seat
	// are the founder's seat and the agent seat that answered there.
	Room   string `json:"room"`
	Person string `json:"person"`
	Seat   string `json:"seat"`

	// Seq is the room's highest `channel_seq`, as the engine minted it.
	Seq int64 `json:"seq"`

	// Changes is every record the room took while the frames were
	// captured, each the gesture this capture made, written down as it
	// made it: its op, the message it named, who made it — the founder as
	// the human seat a person writes as ([chatlet.AuthorHuman]), through the
	// act transport and the prune route alike, or the seat as an agent —
	// and the thread a post was said in. The one thing read back is a
	// posted message's id, which only the engine can mint: from the post's
	// receipt, or for the seat's answer from the room's transcript.
	//
	// STATED RATHER THAN READ BACK, on purpose: an author or a thread the
	// write path recorded wrongly is in what the engine answers and holds
	// as well as on the frame, so read back from either it would be the
	// expectation the frame is held to, and the comparison would pass.
	Changes []chatRecord `json:"changes"`

	// Newest is where the room's last record sits on the log: what the
	// client's per-room high-water mark has to end at.
	Newest statelog.Position `json:"newest"`

	// Cursor is the packed position the founder marked the room read
	// through.
	Cursor int64 `json:"cursor"`
}

// chatRecord is one record a room took: its kind, the message it names (none
// for a record about the room itself), who made it and as what, and the
// thread a post was said in. The JSON names are the frame's own
// ([stream.ChatChange]), so each side reads the other's without a mapping.
type chatRecord struct {
	Op         chatlet.OpKind     `json:"op"`
	MessageID  string             `json:"message_id,omitempty"`
	Author     string             `json:"author"`
	AuthorKind chatlet.AuthorKind `json:"author_kind"`
	ThreadRoot string             `json:"thread_root,omitempty"`
}

// recordOf is the record a frame announced, in [chatRecord]'s terms.
func recordOf(change stream.ChatChange) chatRecord {
	return chatRecord{
		Op: change.Op, MessageID: change.MessageID, Author: change.Author,
		AuthorKind: change.AuthorKind, ThreadRoot: change.ThreadRoot,
	}
}

// captureChat has the founder use the company's own chat from the captured
// tab, the way its chat screen does — every write through the act transport
// or the human write surface, the room looked at with the socket's one
// client-to-server frame — while a seat answers there, and returns its
// account of it ([chatCapture]) with the exchange whose floor the replay holds
// the client to.
//
// EVERY CHAT PUSH KIND IS ON THE CAPTURE BEFORE IT RETURNS, checked against
// the engine's own list ([stream.ChatPushKinds]) rather than one written
// here, so a kind the engine adds is a capture that stops passing until this
// produces it — and the replay then holds the client to dispatching it. A
// kind the capture never carries is a kind the replay cannot certify.
//
// AND EVERY FRAME NAMES THE RECORD IT ANNOUNCES: its op, the message, the
// author and their kind, and a post's thread — held here against the
// capture's account in both directions, and by the replay against the store
// the client filed them in. A frame that named the wrong message makes a
// screen re-read the wrong range of a room, and only a comparison per frame
// can see it: the room's messages taken together are named by its posts
// alone.
func captureChat(t *testing.T, n *node, founder *browser, conn *websocket.Conn,
	frames *capture) (chatCapture, actCapture) {

	t.Helper()
	who := viewerOf(t, n, founder,
		chatlet.CreateChannelTool, chatlet.PostMessageTool, chatlet.ReactToMessageTool,
		chatlet.ReplyInThreadTool, chatlet.EditMessageTool, chatlet.DeleteMessageTool,
		chatlet.MarkChatReadTool, chatlet.LeaveChannelTool)
	const seat = "ceo"
	out := chatCapture{Person: who.Handle, Seat: seat}
	// BY THE FOUNDER, AS A PERSON WRITES: the human seat their session
	// resolves to.
	by := func(op chatlet.OpKind, message, thread string) chatRecord {
		return chatRecord{Op: op, MessageID: message, Author: who.Handle,
			AuthorKind: chatlet.AuthorHuman, ThreadRoot: thread}
	}

	// A ROOM, made from the tab with the seat in it.
	made := openRoomAs(t, n, founder, map[string]any{
		"channel": "launch", "kind": string(chatlet.KindPublic),
		"topic": "what ships on Thursday", "members": []any{seat},
	})
	out.Room = made.Channel
	out.Changes = append(out.Changes, by(chatlet.OpCreate, "", ""))
	// LOOKED AT, as the room's screen does when it opens: the one frame a
	// tab sends that asks nothing, and the reason its presence is probed.
	focusOn(t, conn, made.Channel)

	// SAID, through the composer's route — the exchange the replay holds
	// the client's floor to, so it is read at that floor here first, NOT
	// waited for: the read is what has to wait on it.
	said := founder.act(n, chatlet.PostMessageTool, map[string]any{
		"channel": made.Channel, "body": "The launch checklist is on the wiki.",
	})
	saidAnswer := answerOf(t, said)
	positionOf(t, saidAnswer)
	first := receiptOf[chatPostReceipt](t, saidAnswer).MessageID
	out.Changes = append(out.Changes, by(chatlet.OpPost, first, ""))
	query := url.Values{"read_level": {"session"}, "min_position": {*saidAnswer.Position}}
	status, raw := founder.sendRaw(n, http.MethodGet,
		"/chatlet/channels/"+url.PathEscape(made.Channel)+"/messages?"+query.Encode(), "", nil)
	var page chatlet.Transcript
	if status != http.StatusOK || json.Unmarshal(raw, &page) != nil {
		t.Fatalf("a read of the room at the post's floor was not served: %d %s", status, raw)
	}
	if page.Level != statelog.ReadSession {
		t.Errorf("a read of the room at the post's floor was served at %q, want session",
			page.Level)
	}
	if !slices.ContainsFunc(page.Messages, func(v chatlet.MessageView) bool {
		return v.Message.ID == first
	}) {
		t.Errorf("a read of the room at the post's floor does not hold the post: %v",
			bodies(page))
	}

	// ACKNOWLEDGED, with the message's reaction button — the dashboard's
	// own press, an act like every other.
	founder.acted(n, chatlet.ReactToMessageTool, map[string]any{
		"channel": made.Channel, "message_id": first, "emoji": ":eyes:",
	})
	out.Changes = append(out.Changes, by(chatlet.OpReact, first, ""))

	// ASKED, IN THE MESSAGE'S THREAD, and the seat answers there: woken by
	// a thread reply, it claims to have answered, is refused for having said
	// nothing, replies in the thread — HELD on that round until the room's
	// presence has shown it working, which is only true while its turn runs
	// ([chatScript.hold]) — and submits citing the reply. Three rounds,
	// which is the golden company's cap, so the turn ends on its own
	// submission rather than on the rescue path.
	hold := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	const answer = "Yes: Thursday, once the checklist is signed off."
	n.model.script(&chatScript{channel: made.Channel, thread: first, answer: answer, hold: hold})
	t.Cleanup(func() { n.model.script(nil) })
	asked := receiptOf[chatPostReceipt](t, founder.acted(n, chatlet.ReplyInThreadTool,
		map[string]any{
			"channel": made.Channel, "reply_to": first,
			"body": "@ceo is the launch still on for Thursday?",
		}))
	// IN THE THREAD IT WAS SAID IN, as the capture said it — never the
	// receipt's thread_root, which is copied from the very message the
	// record and its frame carry, so read back it would agree with a reply
	// the write path filed under the wrong thread, or under none. The
	// receipt is checked here as well, so a mis-threaded reply is named
	// where it happened rather than only where its frame stops matching.
	if asked.ThreadRoot != first {
		t.Errorf("the founder's reply was filed under %q, not the thread it replied in (%s)",
			asked.ThreadRoot, first)
	}
	out.Changes = append(out.Changes, by(chatlet.OpPost, asked.MessageID, first))
	waitFor(t, "the room's presence to show the founder looking and the seat at work", func() bool {
		looking := frames.sawPresence(made.Channel, func(p stream.ChatRoomPresence) bool {
			return slices.Contains(p.Viewing, who.Handle)
		})
		working := frames.sawPresence(made.Channel, func(p stream.ChatRoomPresence) bool {
			return slices.ContainsFunc(p.Working, func(w stream.ChatWorking) bool {
				return w.Handle == seat
			})
		})
		return looking && working
	}, func() string {
		return fmt.Sprintf("frames %v; the model saw %v", frames.kinds(t), n.model.seen())
	})
	release()
	var answered chatlet.MessageView
	waitFor(t, "the seat's answer in the thread", func() bool {
		room := transcript(t, n, who.Handle, made.Channel)
		i := slices.IndexFunc(room.Messages, func(v chatlet.MessageView) bool {
			return v.Message.Author == seat
		})
		if i < 0 {
			return false
		}
		answered = room.Messages[i]
		return true
	})
	if answered.Message.ThreadRoot != first {
		t.Errorf("the seat answered under %q, not in the thread it was asked in (%s)",
			answered.Message.ThreadRoot, first)
	}
	// THE SEAT'S ANSWER AS IT WAS ASKED FOR: by the seat, as an agent, in
	// the thread — only the id the engine minted is the transcript's.
	out.Changes = append(out.Changes, chatRecord{Op: chatlet.OpPost,
		MessageID: answered.Message.ID, Author: seat,
		AuthorKind: chatlet.AuthorAgent, ThreadRoot: first})
	// AND ITS TURN DELIVERED: the script served the submission citing the
	// reply, and the engine took it rather than writing a rescue.
	waitFor(t, "the seat to submit citing its answer", n.model.chatSubmitted)
	deliveredThrough(t, n, chatlet.ReplyInThreadTool)

	// CHANGED, TAKEN BACK, READ AND LEFT, each from the tab.
	founder.acted(n, chatlet.EditMessageTool, map[string]any{
		"channel": made.Channel, "message_id": first,
		"body": "The launch checklist is on the wiki, and signed off.",
	})
	out.Changes = append(out.Changes, by(chatlet.OpEdit, first, ""))
	founder.acted(n, chatlet.DeleteMessageTool, map[string]any{
		"channel": made.Channel, "message_id": first,
	})
	out.Changes = append(out.Changes, by(chatlet.OpDelete, first, ""))
	out.Cursor = answered.Position.Packed()
	founder.acted(n, chatlet.MarkChatReadTool, map[string]any{
		"cursors": map[string]any{made.Channel: out.Cursor},
	})
	founder.acted(n, chatlet.LeaveChannelTool, map[string]any{"channel": made.Channel})
	out.Changes = append(out.Changes, by(chatlet.OpMembers, "", ""))

	// THE ROOM'S NUMBERING, read back before the prune takes its rows.
	for _, view := range transcript(t, n, who.Handle, made.Channel).Messages {
		out.Seq = max(out.Seq, view.ChannelSeq)
	}

	// AND PRUNED, through the human write surface's destruction route — the
	// one record whose frame tells a screen that everything it holds below
	// an instant is gone, and the last the room takes here.
	out.Newest = pruneRoom(t, n, founder, made.Channel)
	out.Changes = append(out.Changes, by(chatlet.OpPrune, "", ""))

	// AND THE TAB WAS TOLD ALL OF IT: a frame for every record, naming what
	// this capture did, and the founder's own read state.
	onFrames := func() []chatRecord {
		var seen []chatRecord
		for _, change := range frames.chatChanges(made.Channel) {
			seen = append(seen, recordOf(change))
		}
		return seen
	}
	waitFor(t, "every chat record and the founder's read state on the socket", func() bool {
		seen := onFrames()
		for _, want := range out.Changes {
			if !slices.Contains(seen, want) {
				return false
			}
		}
		return slices.ContainsFunc(frames.chatReads(), func(r coord.PersonChatRead) bool {
			return r.Handle == who.Handle && r.Cursors[made.Channel] == out.Cursor
		})
	}, func() string {
		return fmt.Sprintf("want %+v; the room's frames say %+v", out.Changes, onFrames())
	})
	// AND NOTHING ELSE: a frame naming a record the room never took is the
	// same failure the other way round — a message re-read that was never
	// touched, under an author who never touched it.
	for _, got := range onFrames() {
		if !slices.Contains(out.Changes, got) {
			t.Errorf("a frame announced %+v, which is no record the room took: %+v",
				got, out.Changes)
		}
	}
	kinds := frames.kinds(t)
	for _, kind := range stream.ChatPushKinds() {
		if !slices.Contains(kinds, string(kind)) {
			t.Errorf("the capture carries no %s frame, so the replay cannot hold the "+
				"client to reading one", kind)
		}
	}
	for _, change := range frames.chatChanges(made.Channel) {
		if change.ChannelSeq > out.Seq {
			t.Errorf("a %s frame names channel_seq %d in a room the engine numbered to %d",
				change.Op, change.ChannelSeq, out.Seq)
		}
	}
	return out, said
}

// pruneRoom prunes a room through `POST /chatlet/channels/{id}/prune` as the
// person b is signed in as — an administrator, with the proof of identity the
// sign-in just made, which is the step-up the route asks — with everything
// said in it so far before the cutoff, and returns where the prune landed once
// this node has applied it.
//
// THE CUTOFF TO THE NANOSECOND, because a prune takes what was stored BEFORE
// it and the room's last messages were said within this second: RFC 3339
// trimmed to whole seconds would leave them standing. The route echoes the
// value back in `?confirm=`, as an operator has to, and the request carries an
// operation key of its own, as every write on that surface does.
func pruneRoom(t *testing.T, n *node, b *browser, channelID string) statelog.Position {
	t.Helper()
	cutoff := time.Now().UTC().Format(time.RFC3339Nano)
	query := url.Values{"cutoff": {cutoff}, "confirm": {cutoff}}
	status, raw := b.sendRaw(n, http.MethodPost,
		"/chatlet/channels/"+url.PathEscape(channelID)+"/prune?"+query.Encode(),
		statelog.NewOpID(time.Now(), ""), nil)
	var pruned struct {
		Outcome  statelog.Outcome `json:"outcome"`
		Position *string          `json:"position"`
	}
	if (status != http.StatusOK && status != http.StatusAccepted) ||
		json.Unmarshal(raw, &pruned) != nil || pruned.Position == nil {
		t.Fatalf("the prune of %s was not answered with a position: %d %s", channelID, status, raw)
	}
	if pruned.Outcome != statelog.OutcomeApplied && pruned.Outcome != statelog.OutcomePending {
		t.Fatalf("the prune of %s came to %q: %s", channelID, pruned.Outcome, raw)
	}
	at, err := statelog.ParsePosition(*pruned.Position)
	if err != nil {
		t.Fatalf("the prune of %s answered a position a read cannot take back: %v", channelID, err)
	}
	if err := n.engine.WaitCommitted(t.Context(), at); err != nil {
		t.Fatalf("waiting for the prune of %s: %v", channelID, err)
	}
	return at
}

func TestTheDashboardClientCanReadWhatThisServerSends(t *testing.T) {
	t.Parallel()
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
	// the gate a real turn's calls pass — a charge it refuses, or the call
	// the turn's meter holds once the turn's own context assembly has
	// filled the day, which the meter records as the refusal a charge would
	// have (coord.Budgets.Refuse) — so what is waited on is the counter's
	// stamp, which both write. The seat is its own scope, so the company's
	// turn beside it is charged against nothing it spent.
	//
	// Both ceilings are written where an operator writes them — the
	// company's through `/config`, the seat's through its org chart runtime
	// half — in the `{day: N}` form, before the socket opens. And the
	// company runs MID-DAY ON ITS OWN CLOCK, so the refusal waited for below
	// is stamped in the day it is read back in ([middayZone]).
	//
	// AND A PERSON WHO CAN WRITE, WHOSE TAB THE CAPTURE IS: a password
	// sign-in on this node, bound to the founder's seat by the invitation
	// that enrolled her, so the capture carries real `/operator/act` answers
	// — the ones the dashboard's session floor is raised from — and is taken
	// on a person's socket, the only kind the company's own chat is pushed
	// to, every chat frame being decided per recipient seat. She holds
	// ADMIN, which is every permission there is, so the one tab is sent
	// every kind of frame a tab can be: the activity feed an administrator
	// reads, the inbox the tracker's holders are told of, and the chat. One
	// tab is one person, as on the dashboard: two sockets would be two frame
	// streams with no order between them for the one store the replay
	// feeds.
	n := startNode(t, nodeSpec{company: func(doc string) string {
		doc = strings.Replace(doc, "roles:\n", "roles:\n"+
			"  - name: CFO\n"+
			"    handle: cfo\n"+
			"    llm: scripted\n", 1)
		return doc + "\ntimezone: " + middayZone() + "\n"
	}, boot: withSignIn})
	waitFor(t, "the native backends to hydrate", hydrated(t, n.engine))
	setCompanyCeilings(t, n, map[period.Period]int{period.Day: 100_000_000})
	setSeatCeilings(t, n, "cfo", map[period.Period]int{period.Day: 100})
	zone := companyMidday(t, n.engine)
	admin := jane
	admin.grants = []iam.Grant{iam.GrantAdmin}
	tab := newBrowser(t)
	signInAs(t, n, tab, admin)

	waitFor(t, "the seats to be claimed", func() bool {
		held := n.engine.Node().Host().Held()
		return slices.Contains(held, "ceo") && slices.Contains(held, "cfo")
	})
	conn := dialAs(t, n, tab)
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
	cfoID := seatAgentID(t, n, "cfo")
	waitFor(t, "the seat's own ceiling to refuse a call", func() bool {
		got, err := budgets.Used(t.Context(), coord.AgentScope(cfoID),
			coord.WindowsAt(time.Now(), zone))
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
	// AND ANSWERED ON THE ITEM, which is what an assignment owes: the
	// engine reads a task wake as somebody waiting on a tool, so the
	// endpoint's default — `delivered` citing nothing, honest for the
	// unaddressed wakes it serves — is refused here until the rounds run
	// out and the turn ends on an outcome the engine wrote. The seat
	// comments on the item and submits citing the comment, which is how a
	// real assignee closes the loop. Queued BEFORE the item is filed,
	// because its wake can reach the executor before the create returns,
	// and naming the item by the id this test minted, its key being the
	// tracker's to assign.
	const summary = "Three PRs merged, one incident, zero regressions."
	n.model.callOnExecute(builtin.CommentOnWorkTool, map[string]any{
		"item": task.ID, "body": summary,
	})
	n.model.callOnExecute(runner.SubmitWorkTool, map[string]any{
		"outcome": string(turn.OutcomeDelivered), "summary": summary,
		"deliveries": []any{builtin.CommentOnWorkTool},
	})
	// WITH ITS WAKE, built the way every create surface builds one: a
	// record carrying no notification is a change nobody asked to hear about.
	wake := worklet.Wake{Kind: worklet.ChangeCreated, After: task}.Notify(nil)
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
	// AND THE COMPANY'S TURN DELIVERED, on its own submission citing the
	// comment rather than on the rescue path.
	deliveredThrough(t, n, builtin.CommentOnWorkTool)
	// The meters are published on engine.BudgetReportInterval, so one
	// carrying the refusal lands within an interval of it at the latest.
	waitFor(t, "a budget push carrying the refusal", func() bool {
		return frames.sawRefusal(t, cfoID)
	})

	// AND THE COMPANY'S OWN CHAT, used from the tab while a seat answers
	// in it — after both turns, so the seat's chat turn is the only one
	// running and the waits above counted only theirs.
	talked, posted := captureChat(t, n, tab, conn, frames)
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
	// the engine panel to: a live node counted, and a standing alarm with the
	// one that has stood longest named, since the company has never taken a
	// backup.
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

	// --- two writes, and the floors they raise ------------------------- //
	// ON TWO LOGS: a pin on the tracker's, made after the capture, and the
	// founder's first message on chat's, made during it. Each names the
	// questions that read its log, which the replay holds the floor to.
	pinned := captureAct(t, n, tab)
	pinned.Reads = []string{"work_person", "work_items"}
	posted.Reads = []string{"chat_messages", "chat_channels"}
	answers, err := json.Marshal([]actCapture{pinned, posted})
	if err != nil {
		t.Fatalf("encoding the act captures: %v", err)
	}
	chatted, err := json.Marshal(talked)
	if err != nil {
		t.Fatalf("encoding the chat capture: %v", err)
	}

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

	tree := filepath.Join(sourcetree.Root(t), "static", "dashboard")

	runCtx, runCancel := context.WithTimeout(t.Context(), replayTimeout)
	defer runCancel()
	actPath := filepath.Join(t.TempDir(), "act.json")
	if err := os.WriteFile(actPath, answers, 0o600); err != nil {
		t.Fatalf("writing the act answers: %v", err)
	}
	chatPath := filepath.Join(t.TempDir(), "chat.json")
	if err := os.WriteFile(chatPath, chatted, 0o600); err != nil {
		t.Fatalf("writing the chat capture: %v", err)
	}
	cmd := exec.CommandContext(runCtx, node, replayScript, path, actPath, chatPath)
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

func TestATightBudgetStopsTheTurnAndCountsTheRoundItRefused(t *testing.T) {
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
	//
	// THE CEILING IS WRITTEN WHERE AN OPERATOR WRITES IT, through `/config`
	// once the node is running, and the case waits for the node to apply it
	// before it wakes the seat: a ceiling seeded from the company file would
	// prove the boot and nothing about a running gate picking up a cap moved
	// under it.
	//
	// AND ALL OF IT IN ONE COMPANY DAY, which is what the arithmetic assumes:
	// the gate cuts each round's windows when the round is charged, so with
	// the company's midnight between the onboarding round and the execute
	// round each lands in a day with room and nothing is ever refused. The
	// company runs on a zone whose local time is noon as the case starts
	// ([middayZone]).
	aux, round := textReplyUsage.tokens(), toolUseUsage.tokens()
	limit := aux + round + round/2
	n := startWith(t, func(doc string) string {
		return doc + "\ntimezone: " + middayZone() + "\n"
	})
	setCompanyCeilings(t, n, map[period.Period]int{period.Day: limit})
	zone := companyMidday(t, n.engine)
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
	// Read against the company's day, on the zone the engine cuts it on: the
	// day the cap is written for.
	budgets := n.engine.Backends().Fleet
	today := func() coord.Windows { return coord.WindowsAt(time.Now(), zone) }
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

	// EVERY COMPLETION THE MODEL ANSWERED IS ON THE COUNTER, ONCE — the
	// rounds the gate refused included. A round is charged once its reply is
	// in, so a refused round was billed, and the counter counts it
	// (coord.Budgets.Charge); the auxiliary completions — the prefetch's
	// knowledge query here — are recorded whole after they return
	// (coord.Budgets.PostCharge). So the counter reads past the cap by every
	// round the gate refused, and `used <= limit` is not the engine's promise:
	// what it promises is that the counter is what the company was billed. A
	// counter that dropped a refused round read short of that, and let the
	// next, smaller round in on room the refused one had already used.
	//
	// What the gate ADMITS is certified where it can be judged charge by
	// charge — the contract suite on the real broker (coordtest's budget
	// cases, a refused round followed by a smaller one among them) and the
	// tool loop's refused-round case — because here every charge, admitted or
	// refused, lands on the one figure.
	//
	// WAITED FOR, re-reading the model's log and the counter together on
	// every poll: a completion's charge lands a moment after its answer, so
	// the two agree once the charges are through, and the poll that sees them
	// agree is a snapshot in which every refused round so far is counted.
	// Every round of the loop here is a tool-use reply, and every auxiliary
	// pass a text reply, so each request's cost is known from its kind. EVERY
	// request the model answered is summed: each is one model request of a
	// scripted CLI run, whose usage the backend reads off the record the CLI
	// prints for it, and a stop never lands between the model's answer and
	// that record ([cliRun.handleStop]) — so there is no request the engine
	// was billed for and never read.
	var used, want int
	var calls []string
	waitFor(t, "the counter to hold every completion the model answered", func() bool {
		calls = n.model.seen()
		org, err := budgets.Used(t.Context(), coord.OrgScope, today())
		if err != nil {
			return false
		}
		used, want = org.In(period.Day).Used, 0
		for _, call := range calls {
			if strings.HasPrefix(call, "aux:") {
				want += aux
			} else {
				want += round
			}
		}
		return used == want
	}, func() string {
		return fmt.Sprintf("the counter reads %d against a cap of %d, and the model "+
			"answered %d tokens' worth: %v", used, limit, want, calls)
	})
	loop := 0
	for _, call := range calls {
		if !strings.HasPrefix(call, "aux:") {
			loop++
		}
	}
	// PARTWAY THROUGH THE TURN: the loop ran at least the round that fitted
	// and the round that did not, and the counter holds the second past the
	// cap.
	if loop < 2 || used <= limit {
		t.Errorf("the cap refused a charge after %d round(s) of the turn's own loop with "+
			"the counter at %d of %d, want a round that fitted and a refused one counted "+
			"past the cap; model calls %v", loop, used, limit, calls)
	}
	// And the SEAT's counter moved with it: one charge, both scopes.
	//
	// WAITED FOR rather than read once, because the two counters are two
	// KEYS AND NO TRANSACTION — [coord/kv.FleetStore.Charge] says so and
	// builds its order out of sequence instead: the org is counted and
	// judged first, then the seat, which a round the company refused is
	// counted on all the same. So there is a real window in which the org
	// has moved and the seat has not, and the wait above lands inside it
	// whenever the org's write is what satisfied it.
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

// AN ONBOARDING PASS THE BUDGET REFUSES ENDS THE TURN THERE.
//
// The cap leaves room for the turn-start prefetch's knowledge query and half
// of the onboarding pass's first round, so that round is billed and refused —
// and, counted, it leaves the window past its ceiling, so every later call of
// the turn is certain to be refused. The turn used to log the refusal as an
// onboarding failure and enter its loop anyway, where the executor's first
// round was billed and refused in turn. It ends on the onboarding's refusal
// now, published as budget_exhausted like a refusal inside the loop, with no
// executor phase opened and no executor call made.
func TestAnOnboardingRefusalEndsTheTurnBeforeTheExecutor(t *testing.T) {
	t.Parallel()
	aux, round := textReplyUsage.tokens(), toolUseUsage.tokens()
	limit := aux + round/2
	n := startWith(t, func(doc string) string {
		return doc + fmt.Sprintf("\ntoken_budget: {day: %d}\n", limit)
	})
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "ceo")
	})
	n.wake(t, "ceo", "How did the week go?")

	stored := func(eventType string) []*events.Event {
		rows, err := n.engine.Backends().Store.Events().List(t.Context(),
			store.ListQuery{Type: eventType, Limit: 50})
		if err != nil {
			return nil
		}
		var out []*events.Event
		for _, row := range rows {
			full, err := n.engine.Backends().Store.Events().ByID(t.Context(), row.ID, time.Now())
			if err != nil {
				return nil
			}
			var ev events.Event
			if err := json.Unmarshal(full.Payload, &ev); err != nil {
				t.Fatalf("decode a stored %s: %v", eventType, err)
			}
			out = append(out, &ev)
		}
		return out
	}
	// The completion is published after every phase event of the turn, so
	// once it is stored an executor phase that opened would be too.
	waitFor(t, "the turn to end on the budget", func() bool {
		return len(stored("budget_exhausted")) > 0 && len(stored("agent_turn_completed")) > 0
	})

	var phases []string
	for _, ev := range stored("agent_phase_started") {
		if started, ok := events.DataAs[*types.AgentPhaseStarted](ev); ok {
			phases = append(phases, string(started.Phase))
		}
	}
	if !slices.Contains(phases, "onboarding") {
		t.Fatalf("phases opened = %v, want the onboarding pass the budget refused", phases)
	}
	if slices.Contains(phases, "execute") {
		t.Errorf("phases opened = %v: the turn entered its loop after the budget had "+
			"already refused it", phases)
	}
	if calls := n.model.seen(); slices.Contains(calls, "execute") {
		t.Errorf("the executor called the model after the onboarding pass was refused: %v", calls)
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
	t.Parallel()
	n := start(t)

	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "ceo")
	})

	// A trace this test can recognise, in the shape a real tracer emits —
	// WATCHED before the wake, because the recorder keeps only the lines of a
	// trace somebody asked for, and a line logged before the ask is gone.
	const (
		traceID     = "4bf92f3577b34da6a3ce929d0e0e4736"
		triggerSpan = "00f067aa0ba902b7"
	)
	logs.watch(t, traceID)
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
