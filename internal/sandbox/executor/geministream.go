package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// Gemini CLI's stream-json, as 0.70.0 prints it: one event a line,
// live (a call's tool_use is written before the tool returns), of six kinds
// — init, message, tool_use, tool_result, error and result — and the usage
// line the run's script appends ([geminiUsageEvent]).

// geminiUsageEvent is the type of the line the run's script appends after the
// harness exits: every request of the session, its tokens summed off the chat
// file (gemini_helper.mjs), because stream-json's result splits only the
// prompt and the reply out of its total, and the tool prompt in the rest is
// billed as input ([geminiEvents.runSpend]).
const geminiUsageEvent = "crewlet_gemini_usage"

// geminiUnbridgedEvent is the type of the line the run's script appends where
// the bridge's BeforeTool hook stopped a call another server served under a
// name of the bridge's ([geminiBridgedHook]): its message fails the run.
const geminiUnbridgedEvent = "crewlet_gemini_unbridged"

// geminiBlockedPrefix opens the warning Gemini CLI prints where an AfterAgent
// hook blocked the run's finish: the block's reason, the note the take script
// handed it (measured on 0.70.0).
const geminiBlockedPrefix = "Agent execution blocked: "

// geminiAPIErrorExit is what Gemini CLI exits with where the vendor's API
// refused a request with HTTP 429: it exits with the response's status mod 256
// (429 -> 173, 400 -> 144, 500 -> 244; measured on 0.70.0), and a quota is a
// 429, so only a run that exited so ended on its plan's limit.
const geminiAPIErrorExit = 429 % 256

// limitExit reports whether a Gemini CLI run that exited with code ended on
// its vendor's 429 ([limitGate]), the one ending a limit is read off.
func (GeminiCLI) limitExit(code int) bool { return code == geminiAPIErrorExit }

// geminiTurnLimitError is the type of the result's error where the run's
// round cap ended it (settings' model.maxSessionTurns, geminiSettings):
// measured on 0.70.0, a cap of 2 and of 3 ended the run after 2 and 3
// requests with this error, status "error" and exit 53, where every other
// failure measured carries another type.
const geminiTurnLimitError = "FatalTurnLimitedError"

// geminiContextSentence is the API's own words for a request past the model's
// window, which Gemini CLI relays in its result's error (measured with the
// stub's 400 INVALID_ARGUMENT, exit 144). Read off the result alone, which no
// tool output reaches.
const geminiContextSentence = "exceeds the maximum number of tokens allowed"

// geminiEventWire is the part of one event this runner reads, each value raw
// and read loosely, so a retyped field is skipped rather than the event lost.
type geminiEventWire struct {
	Type       json.RawMessage `json:"type"`
	SessionID  json.RawMessage `json:"session_id"`
	Role       json.RawMessage `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolName   json.RawMessage `json:"tool_name"`
	ToolID     json.RawMessage `json:"tool_id"`
	Parameters json.RawMessage `json:"parameters"`
	Status     json.RawMessage `json:"status"`
	Output     json.RawMessage `json:"output"`
	Error      json.RawMessage `json:"error"`
	Severity   json.RawMessage `json:"severity"`
	Message    json.RawMessage `json:"message"`
	Requests   json.RawMessage `json:"requests"`
	Tokens     json.RawMessage `json:"tokens"`
	Missing    json.RawMessage `json:"missing"`
	Stats      json.RawMessage `json:"stats"`
}

// geminiStats is a result's token totals over every API request of the run:
// total each request's whole count, input its whole prompt (the cached part
// inside it), output its reply without the reasoning.
type geminiStats struct {
	Total  int `json:"total_tokens"`
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
	Cached int `json:"cached"`
}

// geminiToolInput is the field each of Gemini CLI's own tools takes its
// subject in: run_shell_command its command, the file tools their file, the
// directory tools their directory, glob and grep their pattern, the web tools
// their query or URL.
type geminiToolInput struct {
	Command     json.RawMessage `json:"command"`
	FilePath    json.RawMessage `json:"file_path"`
	DirPath     json.RawMessage `json:"dir_path"`
	Path        json.RawMessage `json:"path"`
	Pattern     json.RawMessage `json:"pattern"`
	Query       json.RawMessage `json:"query"`
	URL         json.RawMessage `json:"url"`
	Description json.RawMessage `json:"description"`
}

// geminiTokens is the usage line's sums, as the chat file names them: input
// is each request's WHOLE prompt (the cached part inside it), tool the prompt
// a model-side tool built, billed as input, and thoughts the reasoning,
// billed as output.
type geminiTokens struct {
	Input    int `json:"input"`
	Output   int `json:"output"`
	Cached   int `json:"cached"`
	Thoughts int `json:"thoughts"`
	Tool     int `json:"tool"`
}

// geminiEvents decodes one stream into the answer, the result and a readable
// transcript, read whole or followed ([Decoder]).
//
// THE ANSWER IS THE LAST REPLY'S TEXT: the assistant's deltas since the last
// event of any other kind. A run handed a note at its finish answers again
// after the block's warning, and that later reply is the answer.
type geminiEvents struct {
	undecidedLines

	transcript transcriptLines

	// said is the assistant text since the last other event, written to the
	// transcript as one entry when another event arrives; answer is the same
	// text kept for the result, up to [sandbox.MaxFileBytes], dropped what it
	// wrote past that.
	said    strings.Builder
	answer  strings.Builder
	dropped int

	// calls is each pending call's number by its id, forgotten as its result
	// arrives; seq numbers the run's calls ([activityline]).
	calls map[string]int
	seq   int

	session string
	result  []byte
	status  string
	errText string
	errType string
	// unbridged is why the bridge's hook stopped a call, "" for none
	// ([geminiUnbridgedEvent]).
	unbridged string
	usage     *geminiTokens
	stats     *geminiStats
	requests  int
	sawLine   bool

	// following is a decoder read through Entries, which keeps nothing a
	// Result would need.
	following bool
}

// Entries implements [Decoder].
func (d *geminiEvents) Entries(end bool) []string {
	d.following = true
	d.answer.Reset()
	if end {
		d.flushSaid()
	}
	return d.transcript.take(end)
}

func (d *geminiEvents) Line(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	d.sawLine = true
	var ev geminiEventWire
	if line[0] != '{' || wireAlias(line, &ev) != nil {
		// Not an event, or one cut where its harness was killed: text the
		// harness printed is part of what the run said.
		d.flushSaid()
		d.transcript.add(string(line))
		return
	}
	kind := looseString(ev.Type)
	if kind == "message" && looseString(ev.Role) == "assistant" {
		chunk := looseString(ev.Content)
		d.said.WriteString(chunk)
		if !d.following {
			d.keepAnswer(chunk)
		}
		return
	}
	d.flushSaid()
	switch kind {
	case "init":
		d.session = looseString(ev.SessionID)
	case "message":
		// The brief, echoed: the next reply starts afresh.
		d.answer.Reset()
		d.dropped = 0
	case "tool_use":
		d.answer.Reset()
		d.dropped = 0
		d.seq++
		for _, l := range activityline.Call(d.seq, firstNonBlank(looseString(ev.ToolName), "tool"),
			geminiToolSubject(ev.Parameters)) {
			d.transcript.add(l)
		}
		if id := looseString(ev.ToolID); id != "" {
			if d.calls == nil {
				d.calls = map[string]int{}
			}
			d.calls[id] = d.seq
		}
	case "tool_result":
		id := looseString(ev.ToolID)
		seq, ok := d.calls[id]
		if !ok {
			return
		}
		delete(d.calls, id)
		if looseString(ev.Status) == "success" {
			d.transcript.add(activityline.OK(seq, activityline.Took{}))
		} else {
			d.transcript.add(activityline.Failed(seq, firstNonBlank(geminiErrorText(ev.Error),
				looseString(ev.Output), "failed"), activityline.Took{}))
		}
	case "error":
		message := looseString(ev.Message)
		if note, ok := strings.CutPrefix(message, geminiBlockedPrefix); ok {
			d.answer.Reset()
			d.dropped = 0
			if l := firstLine(note, transcriptDetailLimit); l != "" {
				d.transcript.add(activityline.Words(activityline.TagSteer, l))
			}
			return
		}
		d.transcript.add(activityline.Words(activityline.TagError, firstLine(message, transcriptDetailLimit)))
	case "result":
		d.status = looseString(ev.Status)
		d.errText = geminiErrorText(ev.Error)
		d.errType = geminiErrorType(ev.Error)
		var stats geminiStats
		if wireObject(ev.Stats) && wireDecode(ev.Stats, &stats) == nil {
			d.stats = &stats
		}
		if !d.following {
			d.result = bytes.Clone(line)
		}
		if d.status != "success" {
			d.transcript.add(activityline.Words(activityline.TagError,
				"the run ended: "+firstLine(firstNonBlank(d.errText, d.status, "with no status"), transcriptDetailLimit)))
		}
	case geminiUnbridgedEvent:
		if d.unbridged == "" {
			d.unbridged = firstNonBlank(looseString(ev.Message), "a call named inside the bridge's names was not the bridge's")
		}
	case geminiUsageEvent:
		var tokens geminiTokens
		if wireObject(ev.Tokens) && wireDecode(ev.Tokens, &tokens) == nil {
			d.usage = &tokens
			_ = wireDecode(ev.Requests, &d.requests)
		} else if why := looseString(ev.Missing); why != "" {
			d.transcript.add(activityline.Words(activityline.TagNote, firstLine("the run's own share of its "+
				"session's tokens was not read off its chat file: "+why, transcriptDetailLimit)))
		}
	}
}

// keepAnswer adds chunk to the answer within [sandbox.MaxFileBytes], counting
// what lies past it.
func (d *geminiEvents) keepAnswer(chunk string) {
	if d.dropped > 0 || d.answer.Len()+len(chunk) > sandbox.MaxFileBytes {
		d.dropped += len(chunk)
		return
	}
	d.answer.WriteString(chunk)
}

// flushSaid writes the assistant's text so far to the transcript as one entry.
func (d *geminiEvents) flushSaid() {
	if text := strings.TrimSpace(d.said.String()); text != "" {
		d.transcript.add(text)
	}
	d.said.Reset()
}

// Skipped counts a line past the stream's bound into the transcript.
func (d *geminiEvents) Skipped(line LongLine) {
	d.sawLine = true
	d.flushSaid()
	d.transcript.skip(line.Size, line.Bound)
	switch lineType(line) {
	case geminiUsageEvent:
		if n, ok := windowInt(line.Head, "requests", false); ok {
			d.requests = n
		} else if n, ok := windowInt(line.Tail, "requests", true); ok {
			d.requests = n
		} else {
			d.undecided++
		}
	case "":
		d.undecided++
	}
}

// runSpend is the run's own spend, every request its process made, whether it
// started its session or continued one. The result's totals count those alone
// (a headless resume adds none of the session's earlier spend to them: 0.70.0
// fills its counters from a resumed file only in its interactive session
// browser); the chat file keeps no entry for a compression's two or Gemini
// CLI's other requests of its own, so its sums alone under-charge. The result's
// rest past the prompt and reply is the reasoning and the tool prompt: the
// usage line's tool prompt, summed over the run's own responses alone
// ([geminiScript]), moves to the input, the rest stays output. A run that ended
// with no result is charged the chat file's sums of its own responses. With no
// sums (a usage line saying why it read none), the result is charged unsplit,
// and no result charges nothing: never the whole session's sums.
func (d *geminiEvents) runSpend() (runSpend, bool) {
	tool := 0
	if d.usage != nil {
		tool = d.usage.Tool
	}
	if s := d.stats; s != nil && s.Total >= s.Input+s.Output+tool {
		return runSpend{input: s.Input - s.Cached + tool, cacheRead: s.Cached, output: s.Total - s.Input - tool}, true
	}
	if d.usage == nil {
		return runSpend{}, false
	}
	u := d.usage
	return runSpend{input: u.Input - u.Cached + u.Tool, cacheRead: u.Cached, output: u.Output + u.Thoughts}, true
}

// Ended implements [Decoder]: the stream's `result` event.
func (d *geminiEvents) Ended() bool { return d.result != nil }

// Requests implements [Decoder]: the requests the run's chat file states, which
// the script appends after Gemini CLI exits ([geminiUsageEvent]), its main
// model's alone (measured on 0.70.0: 31 of a run's 41, the 10 others its flash
// side calls). THE TURN CAP'S COUNT is the larger of it and the run's
// telemetry's, which holds the side calls too ([GeminiCLI.countedRecords]).
func (d *geminiEvents) Requests() int { return d.requests }

// InFlight implements [Decoder]: none, as Gemini CLI's own maxSessionTurns
// stops before the next request (measured on 0.70.0: a cap of 3, 3 requests).
func (d *geminiEvents) InFlight() int { return 0 }

func (d *geminiEvents) Result() sandbox.Result {
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
	success := d.status == "success" && d.unbridged == ""
	if !success {
		// A FAILED RUN'S WORDS ARE ITS ERROR'S: a limit or a refused sign-in is
		// read off the answer before the error, and the model's own reply can
		// quote the vendor's sentence, so only the failure's error may name one.
		text = ""
	}
	res := sandbox.Result{Text: text, Success: success, SessionID: d.session,
		DeliveredRefs: deliveredRefs(text), Turns: d.requests}
	res.Transcript, res.TranscriptElidedLines, res.TranscriptElidedBytes = d.transcript.result()
	if spend, ok := d.runSpend(); ok {
		res = spend.on(res)
	}
	if res.Success {
		return res
	}
	switch {
	case d.unbridged != "":
		res.Error, res.Unbridged = d.unbridged, true
	case d.result == nil:
		res.Error = "the harness stopped before it reported how its run ended: its stream has no result event"
	default:
		res.Error = firstNonBlank(d.errText, "the harness reported its run ended "+firstNonBlank(d.status, "with no status"))
		res.RoundCapped = d.errType == geminiTurnLimitError
		if res.RoundCapped {
			res.Error = "the run reached its turn cap before it finished: " + res.Error
		}
		res.Event = string(d.result)
		res.ContextExhausted = strings.Contains(d.errText, geminiContextSentence)
	}
	return res
}

// geminiErrorText is an error object's message, or the value itself where it
// is a string.
func geminiErrorText(raw json.RawMessage) string {
	if s := looseString(raw); s != "" {
		return s
	}
	var e struct {
		Message json.RawMessage `json:"message"`
	}
	if wireObject(raw) && wireAlias(raw, &e) == nil {
		return looseString(e.Message)
	}
	return ""
}

// geminiErrorType is an error object's type, "" where it names none.
func geminiErrorType(raw json.RawMessage) string {
	var e struct {
		Type json.RawMessage `json:"type"`
	}
	if wireObject(raw) && wireAlias(raw, &e) == nil {
		return looseString(e.Type)
	}
	return ""
}

// geminiToolSubject is what one call works on: its subject field
// ([geminiToolInput]), or for a tool with none (an MCP server's) its input as
// compact JSON.
func geminiToolSubject(raw json.RawMessage) string {
	var in geminiToolInput
	if wireObject(raw) && wireAlias(raw, &in) != nil {
		in = geminiToolInput{}
	}
	if subject := firstNonBlank(looseString(in.Command), looseString(in.FilePath), looseString(in.DirPath),
		looseString(in.Path), looseString(in.Pattern), looseString(in.Query), looseString(in.URL),
		looseString(in.Description)); subject != "" {
		return subject
	}
	return compactInput(raw)
}
