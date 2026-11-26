package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// The five runners counted live off their harness's own record
// ([recordCounted]), each by the rule its collection counts the same record by.

// codexRolloutName is a rollout's file name, `rollout-<time>-<thread>.jsonl`.
var codexRolloutName = regexp.MustCompile(`^rollout-[0-9A-Za-z.:-]+\.jsonl$`)

// liveRecord implements [recordCounted]: the rollout the script found for the
// run's thread, and its lines before the run began ([codexPointer]).
func (Codex) liveRecord(ctx context.Context, box sandbox.Sandbox, paths Paths) (string, int, bool, error) {
	return recordPlace{root: paths.CodexSessions(), depth: 3, name: codexRolloutName}.named(ctx, box, paths)
}

// recordTally implements [recordCounted].
func (Codex) recordTally(context.Context, sandbox.Sandbox, Paths) (recordTally, error) {
	return &codexRolloutCount{}, nil
}

// codexRolloutCount counts a rollout's responses as the collection counts the lines
// the script appends of it ([codexEvents.keepRollout], [codexRolloutSkipped]).
type codexRolloutCount struct{ responses, unread int }

// Line counts a response the script would append ([codexKept]).
func (c *codexRolloutCount) Line(line []byte) {
	if codexKept.Match(line) && codexResponseLine(line) {
		c.responses++
	}
}

// Skipped judges the line as the script wraps it, and only where the script
// would append it: its kind opens it ([codexRolloutKinds]).
func (c *codexRolloutCount) Skipped(line LongLine) {
	if !codexKept.Match(line.Head) {
		return
	}
	response, undecided := codexRolloutSkipped(wrapped(line, `{"type":"`+codexRolloutEvent+`","line":`, "}"))
	if response {
		c.responses++
	}
	if undecided {
		c.unread++
	}
}

func (c *codexRolloutCount) requests() int  { return c.responses }
func (c *codexRolloutCount) undecided() int { return c.unread }

// liveRecord implements [recordCounted]: the plugin's record, at the run's own
// path, cleared before the run starts ([Paths.HermesRecord]): no pointer to read. IN NEITHER IT NOR
// THE COLLECTION: the request for a summary Hermes makes once its round cap
// stops its loop, which the plugin is not told of (measured on 0.23.3: a cap of
// 3, three requests recorded and that one not).
//
//nolint:unparam // the interface's: the other runners read a pointer that can fail.
func (Hermes) liveRecord(_ context.Context, _ sandbox.Sandbox, paths Paths) (string, int, bool, error) {
	return paths.HermesRecord(), 0, true, nil
}

// recordTally implements [recordCounted].
func (Hermes) recordTally(context.Context, sandbox.Sandbox, Paths) (recordTally, error) {
	return &hermesRecordCount{}, nil
}

// hermesRecordCount counts the plugin's record as the collection counts the lines
// the script appends of it ([hermesEvents.Line]): each that reads, and each
// too long to read.
type hermesRecordCount struct{ n int }

func (h *hermesRecordCount) Line(line []byte) {
	var r hermesRequest
	if json.Unmarshal(line, &r) == nil {
		h.n++
	}
}

func (h *hermesRecordCount) Skipped(LongLine) { h.n++ }
func (h *hermesRecordCount) requests() int    { return h.n }
func (h *hermesRecordCount) undecided() int   { return 0 }

// liveRecord implements [recordCounted]: the session's events.jsonl, and its
// lines as the run began ([copilotJournalAtLaunch]). IT LAGS: Copilot writes a
// request's record about a second after it (measured on 1.0.114: at most 1.2
// s, half under 0.6 s), so a run its count ends goes about one tick further
// past its cap than the watch's 2 s tick alone would let it.
func (Copilot) liveRecord(ctx context.Context, box sandbox.Sandbox, paths Paths) (string, int, bool, error) {
	return recordPlace{root: paths.CopilotHome() + "/" + copilotSessionState, depth: 1,
		name: regexp.MustCompile(`^events\.jsonl$`)}.named(ctx, box, paths)
}

// recordTally implements [recordCounted]: the usage records the script appends
// to the stream after its mark ([copilotUsage]: a line opening with the type),
// each a request with its usage as [copilotEvents.Line] counts it; and the
// compactions the journal starts, which the stream starts too (measured on
// 1.0.114: 529 of each in one run).
func (Copilot) recordTally(context.Context, sandbox.Sandbox, Paths) (recordTally, error) {
	return &copilotJournalCount{}, nil
}

// copilotUsagePrefix is how the script's awk picks a usage record out.
const copilotUsagePrefix = `{"type":"session.usage_record"`

// copilotJournalCount is a journal's count. A CHECKPOINT, the copy of an earlier
// record a rotation writes into a new head, is no request of its own.
type copilotJournalCount struct{ n, compactions, unread int }

func (c *copilotJournalCount) Line(line []byte) {
	var ev struct {
		Type       string `json:"type"`
		Checkpoint bool   `json:"checkpoint"`
		Data       struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"data"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Checkpoint {
		return
	}
	switch {
	case ev.Type == "session.compaction_start":
		c.compactions++
	case bytes.HasPrefix(line, []byte(copilotUsagePrefix)) && wireObject(ev.Data.Usage):
		c.n++
	}
}

func (c *copilotJournalCount) Skipped(line LongLine) {
	switch t := lineType(line); {
	case bytes.Contains(line.Head, []byte(`"checkpoint":true`)):
	case t == "session.compaction_start":
		c.compactions++
	case t == "session.usage_record" && bytes.HasPrefix(line.Head, []byte(copilotUsagePrefix)):
		c.n++
	case t == "":
		c.unread++
	}
}

func (c *copilotJournalCount) requests() int  { return c.n + c.compactions }
func (c *copilotJournalCount) undecided() int { return c.unread }

// geminiChatName is a session's chat file name, `session-<time>-<id8>.jsonl`.
var geminiChatName = regexp.MustCompile(`^session-[0-9A-Za-z.:_-]+\.jsonl$`)

// liveRecord implements [recordCounted]: the chat file the script found for
// the run's session ([geminiPointer]). IN NEITHER IT NOR THE COLLECTION: the
// side calls Gemini CLI makes on its flash model ([geminiEvents.Requests]).
func (GeminiCLI) liveRecord(ctx context.Context, box sandbox.Sandbox, paths Paths) (string, int, bool, error) {
	return recordPlace{root: paths.GeminiChats(), depth: 0, name: geminiChatName}.named(ctx, box, paths)
}

// recordTally implements [recordCounted]: the helper's own rule
// (gemini_helper.mjs, usage): the distinct responses with tokens, but those the
// session held as the run began ([Paths.GeminiUsageBefore]). Where that list
// was never written the helper counts none, and the script names no record
// ([geminiPointer]).
func (GeminiCLI) recordTally(ctx context.Context, box sandbox.Sandbox, paths Paths) (recordTally, error) {
	raw, err := box.ReadFile(ctx, paths.GeminiUsageBefore())
	if err != nil {
		return nil, fmt.Errorf("executor: reading the responses the session held as the run began: %w", err)
	}
	before := map[string]bool{}
	for _, id := range strings.Split(string(raw), "\n") {
		before[id] = true
	}
	return &geminiChatCount{before: before, ids: map[string]bool{}}, nil
}

// geminiChatCount is a chat file's count. ONE ID A RESPONSE: the file appends a
// response's entry again as it completes (measured on 0.70.0: 91 entries for
// 31 responses). The ids held are the responses', one a request.
type geminiChatCount struct {
	before, ids map[string]bool
	unread      int
}

func (g *geminiChatCount) Line(line []byte) {
	var entry struct {
		Type   string          `json:"type"`
		ID     string          `json:"id"`
		Tokens json.RawMessage `json:"tokens"`
	}
	if json.Unmarshal(line, &entry) != nil || entry.Type != "gemini" || entry.ID == "" ||
		!jsTruthy(entry.Tokens) {
		return
	}
	g.add(entry.ID)
}

// Skipped reads the entry's kind and id at its head, where the file writes
// them; a line whose head names no kind may be a response the count cannot read.
func (g *geminiChatCount) Skipped(line LongLine) {
	switch t := windowField(line.Head, "type", false); {
	case t == "gemini" && windowField(line.Head, "id", false) != "":
		g.add(windowField(line.Head, "id", false))
	case t == "":
		g.unread++
	}
}

func (g *geminiChatCount) add(id string) {
	if !g.before[id] {
		g.ids[id] = true
	}
}

func (g *geminiChatCount) requests() int  { return len(g.ids) }
func (g *geminiChatCount) undecided() int { return g.unread }

// jsTruthy reports whether a JSON value reads as true where the helper tests
// it: present, and neither null, false, a zero nor an empty string.
func jsTruthy(v json.RawMessage) bool {
	switch s := strings.TrimSpace(string(v)); s {
	case "", "null", "false", `""`:
		return false
	default:
		var n float64
		return json.Unmarshal([]byte(s), &n) != nil || n != 0
	}
}

// liveRecord implements [recordCounted]: the main thread's session.jsonl the
// script found for the run's session, and its lines as the run began
// ([musePointer]).
func (Muse) liveRecord(ctx context.Context, box sandbox.Sandbox, paths Paths) (string, int, bool, error) {
	return recordPlace{root: paths.MuseSessions(), depth: 4,
		name: regexp.MustCompile(`^session\.jsonl$`)}.named(ctx, box, paths)
}

// recordTally implements [recordCounted].
func (Muse) recordTally(context.Context, sandbox.Sandbox, Paths) (recordTally, error) {
	return &museTraceCount{}, nil
}

// museTraceCount counts a session's model requests as the collection counts the
// lines the script appends of it ([museTraceLine], [museTraceSkipped]).
type museTraceCount struct{ n, unread int }

func (m *museTraceCount) Line(line []byte) {
	if museTraceLine(line) {
		m.n++
	}
}

// Skipped judges a line the script would append, one holding the trace's kind
// at an end, as the collection does. A record names its payload's type before
// its payload, so a line whose head is no session event's is none; an event's
// keys are sorted, its kind among them, so a session event whose ends show no
// trace may be one the count cannot read.
func (m *museTraceCount) Skipped(line LongLine) {
	switch {
	case inEither(line, museTraceNeedle):
		m.n++
	case bytes.Contains(line.Head, []byte(`"payload_type":"runtime.session"`)):
		m.unread++
	}
}

func (m *museTraceCount) requests() int  { return m.n }
func (m *museTraceCount) undecided() int { return m.unread }
