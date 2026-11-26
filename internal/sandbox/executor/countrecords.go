package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// The runners counted live off their harness's own record ([recordCounted]),
// each by the rule its collection counts the same record by:
// read off the lines its script appends of it, or off the record itself
// ([recordCollected]).

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

// liveRecord implements [recordCounted]: the run's own telemetry file, at the
// run's own path, cleared before the run starts ([Paths.GeminiTelemetry]): no
// pointer to read. IT IS LARGE, about 130 KB a request (measured on 0.70.0:
// request metadata, and the system instructions and tool definitions Gemini
// CLI sends, never the messages), so a tick at the fastest measured pace (31
// requests in 2 s) reads some 4 MB, under [countReadBytes]. BOUNDED BY THE CAP:
// pinned for a capped run alone, it holds at most (cap + 1 + one tick's ~31)
// requests before the watch ends the run, until the next launch clears it or
// the box goes. IT LAGS, written a second at a time (at most 1.1 s, measured).
//
//nolint:unparam // the interface's: the runners whose scripts find their record read a pointer that can fail.
func (GeminiCLI) liveRecord(_ context.Context, _ sandbox.Sandbox, paths Paths) (string, int, bool, error) {
	return paths.GeminiTelemetry(), 0, true, nil
}

// recordTally implements [recordCounted].
func (GeminiCLI) recordTally(context.Context, sandbox.Sandbox, Paths) (recordTally, error) {
	return &geminiTelemetryCount{}, nil
}

// collectsFromRecord implements [recordCollected]: the stream states no request
// the side calls make, and the telemetry does.
func (GeminiCLI) collectsFromRecord() {}

// geminiResponse is the line of the telemetry file, a JSON object printed whole
// a field a line, that names one model response: each request answered, its
// side calls on the flash model included, a request retried after a 429 or a
// 500 once (measured on 0.70.0: 31 main and 10 side calls of 41 at the stand-in;
// 6 for 6 answered beside 2 failed attempts).
var geminiResponse = regexp.MustCompile(`^ *"event\.name": "gemini_cli\.api_response",$`)

// geminiTelemetryCount is a telemetry file's count of its responses.
type geminiTelemetryCount struct{ n int }

func (g *geminiTelemetryCount) Line(line []byte) {
	if geminiResponse.Match(line) {
		g.n++
	}
}

// Skipped is no response: the line naming one is that field alone.
func (g *geminiTelemetryCount) Skipped(LongLine) {}
func (g *geminiTelemetryCount) requests() int    { return g.n }
func (g *geminiTelemetryCount) undecided() int   { return 0 }

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
