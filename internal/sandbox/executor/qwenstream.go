package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A Qwen Code run's stream, as its helper writes it (qwen_helper.mjs): every
// ACP session/update notification Qwen Code sent, verbatim, a line each —
// agent_message_chunk, agent_thought_chunk, tool_call and tool_call_update,
// measured on 0.28.4 — and the helper's own lines, each typed
// `crewlet_qwen_*`, saying what the notifications do not: the session, each
// note handed, the window, the 429's words and how the turn ended.

// The helper's own line types.
const (
	qwenSessionEvent = "crewlet_qwen_session"
	qwenNoteEvent    = "crewlet_qwen_note"
	qwenWindowEvent  = "crewlet_qwen_window"
	qwenLimitEvent   = "crewlet_qwen_limit"
	qwenEndEvent     = "crewlet_qwen_end"
)

// qwenEndTurn is the stopReason of a turn Qwen Code finished: the one success.
const qwenEndTurn = "end_turn"

// qwenMaxTurns is the stopReason of a turn its run's round cap ended
// (settings' model.maxSessionTurns, qwenSettings).
const qwenMaxTurns = "max_turn_requests"

// qwenLimitExit is what the helper exits with where Qwen Code failed the turn
// on its vendor's 429 after its own retries: the status mod 256 (the
// helper's limitExit). Only such a run's limit line is read ([limitGate]):
// the chat file the line quotes is the box's, which the model's shell can
// write.
const qwenLimitExit = 429 % 256

// limitExit reports whether a Qwen Code run that exited with code ended on
// its vendor's 429 ([limitGate]).
func (QwenCode) limitExit(code int) bool { return code == qwenLimitExit }

// qwenContextSentences are the providers' own words for a request past the
// model's window, which Qwen Code relays in the prompt's error details
// (OpenAI's and DashScope's, measured under ACP and stream-json on 0.28.4);
// read off the end line alone, which no tool output reaches.
var qwenContextSentences = []string{"maximum context length", "Range of input length should be"}

// qwenLineWire is the part of one line this runner reads, each value raw and
// read loosely, so a retyped field is skipped rather than the line lost.
type qwenLineWire struct {
	Type          json.RawMessage `json:"type"`
	Method        json.RawMessage `json:"method"`
	Params        json.RawMessage `json:"params"`
	SessionID     json.RawMessage `json:"session_id"`
	Message       json.RawMessage `json:"message"`
	ContextWindow json.RawMessage `json:"context_window"`
	StopReason    json.RawMessage `json:"stop_reason"`
	Error         json.RawMessage `json:"error"`
}

// qwenUpdateWire is one session/update's update.
type qwenUpdateWire struct {
	SessionUpdate json.RawMessage `json:"sessionUpdate"`
	Content       json.RawMessage `json:"content"`
	ToolCallID    json.RawMessage `json:"toolCallId"`
	Status        json.RawMessage `json:"status"`
	Title         json.RawMessage `json:"title"`
	RawInput      json.RawMessage `json:"rawInput"`
	Meta          json.RawMessage `json:"_meta"`
}

// qwenMetaWire is an update's `_meta`: Qwen Code's own fields beside ACP's.
type qwenMetaWire struct {
	ToolName         json.RawMessage `json:"toolName"`
	ParentToolCallID json.RawMessage `json:"parentToolCallId"`
	DurationMs       json.RawMessage `json:"durationMs"`
	Usage            json.RawMessage `json:"usage"`
}

// qwenUsageWire is one request's counts, on an empty agent_message_chunk.
type qwenUsageWire struct {
	InputTokens      json.RawMessage `json:"inputTokens"`
	OutputTokens     json.RawMessage `json:"outputTokens"`
	ThoughtTokens    json.RawMessage `json:"thoughtTokens"`
	CachedReadTokens json.RawMessage `json:"cachedReadTokens"`
}

// qwenErrorWire is the end line's error: the prompt's JSON-RPC error.
type qwenErrorWire struct {
	Code    json.RawMessage `json:"code"`
	Message json.RawMessage `json:"message"`
	Details json.RawMessage `json:"details"`
}

// qwenCall is one call the stream opened: its number once its line is
// written (0 before), and its tool.
type qwenCall struct {
	seq  int
	tool string
}

// qwenEvents decodes one stream into the answer, the result and a readable
// transcript, read whole or followed ([Decoder]).
//
// THE ANSWER IS THE LAST REPLY'S TEXT: the message chunks since the last call
// or note. A thought is never the answer nor in the transcript, as Claude
// Code's thinking is not. A subagent's update (`_meta.parentToolCallId`) is
// passed over but for its request, which the run's count and spend take: the
// run denies the agent tool (measured on 0.28.4: none offered), so none is
// expected, and one a repository's settings let through is still the run's.
type qwenEvents struct {
	transcript transcriptLines

	said    strings.Builder
	answer  strings.Builder
	dropped int

	calls map[string]*qwenCall
	seq   int

	session  string
	window   int64
	spend    runSpend
	requests int
	end      []byte
	endSaid  bool
	stop     string
	errText  string
	limit    []byte
	sawLine  bool

	following bool
}

// Entries implements [Decoder].
func (d *qwenEvents) Entries(end bool) []string {
	d.following = true
	d.answer.Reset()
	if end {
		d.flushSaid()
	}
	return d.transcript.take(end)
}

func (d *qwenEvents) Line(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	d.sawLine = true
	var ev qwenLineWire
	if line[0] != '{' || wireAlias(line, &ev) != nil {
		// Not a line of the stream's, or one cut where the helper was killed:
		// text the helper printed is part of what the run said.
		d.flushSaid()
		d.transcript.add(string(line))
		return
	}
	if looseString(ev.Method) == "session/update" {
		var params struct {
			Update json.RawMessage `json:"update"`
		}
		if wireObject(ev.Params) && wireAlias(ev.Params, &params) == nil {
			d.update(params.Update)
		}
		return
	}
	d.flushSaid()
	switch looseString(ev.Type) {
	case qwenSessionEvent:
		d.session = looseString(ev.SessionID)
	case qwenNoteEvent:
		d.resetAnswer()
		if l := firstLine(looseString(ev.Message), transcriptDetailLimit); l != "" {
			d.transcript.add(activityline.Words(activityline.TagSteer, l))
		}
	case qwenWindowEvent:
		if n := looseInt(ev.ContextWindow); n > 0 && (d.window == 0 || n < d.window) {
			d.window = n
		}
	case qwenLimitEvent:
		if !d.following {
			d.limit = bytes.Clone(line)
		}
	case qwenEndEvent:
		d.endSaid = true
		d.stop = looseString(ev.StopReason)
		d.errText = qwenErrorText(ev.Error)
		if !d.following {
			d.end = bytes.Clone(line)
		}
		if d.stop != qwenEndTurn {
			d.transcript.add(activityline.Words(activityline.TagError,
				"the run ended: "+firstLine(firstNonBlank(d.errText, qwenStopSaid(d.stop)), transcriptDetailLimit)))
		}
	}
}

// update reads one session/update.
func (d *qwenEvents) update(raw json.RawMessage) {
	var u qwenUpdateWire
	if !wireObject(raw) || wireAlias(raw, &u) != nil {
		return
	}
	var meta qwenMetaWire
	if wireObject(u.Meta) && wireAlias(u.Meta, &meta) != nil {
		meta = qwenMetaWire{}
	}
	// EVERY REQUEST COUNTS, a subagent's too ([qwenEvents.Requests]); the rest of
	// a subagent's update is its own thread, never the run's answer.
	chunk := looseString(u.SessionUpdate) == "agent_message_chunk"
	if chunk && wireObject(meta.Usage) {
		d.countRequest(meta.Usage)
	}
	if looseString(meta.ParentToolCallID) != "" {
		return
	}
	switch looseString(u.SessionUpdate) {
	case "agent_message_chunk":
		var content struct {
			Text json.RawMessage `json:"text"`
		}
		if wireObject(u.Content) && wireAlias(u.Content, &content) == nil {
			chunk := looseString(content.Text)
			d.said.WriteString(chunk)
			if !d.following {
				d.keepAnswer(chunk)
			}
		}
	case "tool_call":
		d.flushSaid()
		d.resetAnswer()
		id := looseString(u.ToolCallID)
		if id == "" {
			return
		}
		if d.calls == nil {
			d.calls = map[string]*qwenCall{}
		}
		call := &qwenCall{tool: firstNonBlank(looseString(meta.ToolName), looseString(u.Title), "tool")}
		d.calls[id] = call
		if qwenHasInput(u.RawInput) {
			d.opened(call, u.RawInput)
		}
	case "tool_call_update":
		call, ok := d.calls[looseString(u.ToolCallID)]
		if !ok {
			return
		}
		if call.seq == 0 && qwenHasInput(u.RawInput) {
			d.flushSaid()
			d.opened(call, u.RawInput)
		}
		switch looseString(u.Status) {
		case "completed":
			d.flushSaid()
			d.opened(call, nil)
			d.transcript.add(activityline.OK(call.seq, qwenTook(meta.DurationMs)))
			delete(d.calls, looseString(u.ToolCallID))
		case "failed":
			d.flushSaid()
			d.opened(call, nil)
			d.transcript.add(activityline.Failed(call.seq, firstNonBlank(qwenContentText(u.Content), "failed"),
				qwenTook(meta.DurationMs)))
			delete(d.calls, looseString(u.ToolCallID))
		}
	}
}

// opened writes call's line, once: Qwen Code opens a call before its input
// is known (`rawInput` empty) and states it on the update that starts it, so
// the line waits for the input, or for the result of a call that never said.
func (d *qwenEvents) opened(call *qwenCall, input json.RawMessage) {
	if call.seq != 0 {
		return
	}
	d.seq++
	call.seq = d.seq
	for _, l := range activityline.Call(call.seq, call.tool, qwenToolSubject(input)) {
		d.transcript.add(l)
	}
}

// countRequest adds one request's counts to the run's spend. INPUT HOLDS THE
// CACHE: inputTokens is the request's whole prompt, cachedReadTokens the part
// of it served from the cache (OpenAI's prompt_tokens and its cached share;
// measured under ACP on 0.28.4, where each request's context_usage total is its
// inputTokens, the cached prefix inside it). OUTPUT HOLDS THE THOUGHTS:
// outputTokens is the reply's whole count, reasoning included (OpenAI's
// completion_tokens; every request's totalTokens is its input and output
// alone), so thoughtTokens is never added to it.
func (d *qwenEvents) countRequest(raw json.RawMessage) {
	var u qwenUsageWire
	if wireAlias(raw, &u) != nil {
		return
	}
	input, cached := int(looseInt(u.InputTokens)), int(looseInt(u.CachedReadTokens))
	d.spend.input += max(0, input-cached)
	d.spend.cacheRead += max(0, cached)
	d.spend.output += max(0, int(looseInt(u.OutputTokens)))
	d.requests++
}

func (d *qwenEvents) resetAnswer() {
	d.answer.Reset()
	d.dropped = 0
}

// keepAnswer adds chunk to the answer within [sandbox.MaxFileBytes], counting
// what lies past it.
func (d *qwenEvents) keepAnswer(chunk string) {
	if d.dropped > 0 || d.answer.Len()+len(chunk) > sandbox.MaxFileBytes {
		d.dropped += len(chunk)
		return
	}
	d.answer.WriteString(chunk)
}

// flushSaid writes the assistant's text so far to the transcript as one entry.
func (d *qwenEvents) flushSaid() {
	if text := strings.TrimSpace(d.said.String()); text != "" {
		d.transcript.add(text)
	}
	d.said.Reset()
}

// Skipped counts a line past the stream's bound into the transcript.
func (d *qwenEvents) Skipped(n int64, bound int) {
	d.sawLine = true
	d.flushSaid()
	d.transcript.skip(n, bound)
}

// runSpend is the run's own spend, off its requests' counts: every request of
// a session the run started, which it spent alone ([runSpender]).
func (d *qwenEvents) runSpend() (runSpend, bool) { return d.spend, d.requests > 0 }

// reportedWindow is the window Qwen Code said it holds the run's model to
// (qwen/status/session/context_usage), 0 for none ([windowReporter]).
func (d *qwenEvents) reportedWindow() int64 { return d.window }

// Ended implements [Decoder]: the stream's end line.
func (d *qwenEvents) Ended() bool { return d.endSaid }

// Requests implements [Decoder]: the usage the driver writes for each request,
// a subagent's included. Measured on 0.28.4 against a stand-in: 31 for 31, and
// a 429 and a 500 it retried wrote none of their own.
func (d *qwenEvents) Requests() int { return d.requests }

// InFlight implements [Decoder]: none, as Qwen Code's own maxSessionTurns
// stops before the next request (measured on 0.28.4: a cap of 3, 3 requests).
func (d *qwenEvents) InFlight() int { return 0 }

func (d *qwenEvents) Result() sandbox.Result {
	d.flushSaid()
	if !d.sawLine {
		return sandbox.Result{Error: "the harness produced no output"}
	}
	text := strings.TrimSpace(d.answer.String())
	if d.dropped > 0 {
		kept, held := settledHead(d.answer.String())
		text = withNote(strings.TrimSpace(kept), fmt.Sprintf("(%s more of the answer not kept: past the %s "+
			"a run's answer is read to)", humanSize(int64(d.dropped+held)), humanSize(sandbox.MaxFileBytes)))
	}
	success := d.endSaid && d.stop == qwenEndTurn
	if !success {
		// A FAILED RUN'S WORDS ARE ITS ERROR'S, as a Gemini CLI run's are.
		text = ""
	}
	res := sandbox.Result{Text: text, Success: success, SessionID: d.session,
		DeliveredRefs: deliveredRefs(text), Turns: d.requests}
	res.Transcript, res.TranscriptElidedLines, res.TranscriptElidedBytes = d.transcript.result()
	if spend, ok := d.runSpend(); ok {
		res = spend.on(res)
	}
	if success {
		return res
	}
	switch {
	case !d.endSaid:
		res.Error = "the harness stopped before it reported how its run ended: its stream has no end line"
	default:
		res.Error = firstNonBlank(d.errText, qwenStopSaid(d.stop))
		res.RoundCapped = d.stop == qwenMaxTurns
		// THE LIMIT LINE WHERE THERE IS ONE, the provider's words for the 429,
		// else the end line: what the run's ending is classified by.
		res.Event = string(firstNonEmpty(d.limit, d.end))
		for _, s := range qwenContextSentences {
			res.ContextExhausted = res.ContextExhausted || strings.Contains(d.errText, s)
		}
	}
	return res
}

// firstNonEmpty is the first of lines that holds anything.
func firstNonEmpty(lines ...[]byte) []byte {
	for _, l := range lines {
		if len(l) > 0 {
			return l
		}
	}
	return nil
}

// qwenStopSaid is how a turn that did not finish ended, in a person's words.
func qwenStopSaid(stop string) string {
	switch stop {
	case "cancelled":
		return "the turn was cancelled before it finished"
	case qwenMaxTurns:
		return "the run reached its turn cap before it finished"
	case "":
		return "the harness reported its run ended with no stop reason"
	}
	return "the harness reported its turn stopped: " + stop
}

// qwenErrorText is the end line's error in words: the provider's details
// where Qwen Code relayed them, else its own message.
func qwenErrorText(raw json.RawMessage) string {
	var e qwenErrorWire
	if !wireObject(raw) || wireAlias(raw, &e) != nil {
		return ""
	}
	return firstNonBlank(looseString(e.Details), looseString(e.Message))
}

// qwenHasInput reports whether raw is a call's input with anything in it.
func qwenHasInput(raw json.RawMessage) bool {
	return wireObject(raw) && !bytes.Equal(bytes.TrimSpace(raw), []byte("{}"))
}

// qwenToolSubject is what one call works on, read as [geminiToolSubject]
// reads a call of the harness Qwen Code was forked from: a subject field it
// knows (a command, a path, a pattern), else the input as compact JSON.
func qwenToolSubject(raw json.RawMessage) string {
	if !qwenHasInput(raw) {
		return ""
	}
	return geminiToolSubject(raw)
}

// qwenTook is a call's duration as its update states it.
func qwenTook(raw json.RawMessage) activityline.Took {
	if ms := looseInt(raw); ms > 0 {
		return activityline.Took{D: time.Duration(ms) * time.Millisecond, Known: true}
	}
	return activityline.Took{}
}

// qwenContentText is a failed call's words: the text of its content blocks.
func qwenContentText(raw json.RawMessage) string {
	var blocks []struct {
		Content struct {
			Text json.RawMessage `json:"text"`
		} `json:"content"`
	}
	if len(raw) == 0 || raw[0] != '[' || wireDecode(raw, &blocks) != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if t := looseString(b.Content.Text); t != "" {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n")
}
