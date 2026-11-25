package executor

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// grokEvent is the part of one line of Grok Build's `streaming-json` stream
// (1.0.92) this runner reads: `text {data}`, `tool_call {toolCallId, toolName,
// rawInput}`, `tool_call_update {toolCallId, status, rawOutput, content}`,
// `usage {usage}` once per model request, `max_turns_reached`, `error
// {message}` and the run's `end {stopReason, sessionId}`. DECODED LOOSELY: a
// field a release retyped reads as zero, and the rest still read.
type grokEvent struct {
	Type       string          `json:"type"`
	Data       string          `json:"data"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	RawInput   json.RawMessage `json:"rawInput"`
	Status     *string         `json:"status"`
	Content    []struct {
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"content"`
	Usage      grokUsage `json:"usage"`
	StopReason string    `json:"stopReason"`
	SessionID  string    `json:"sessionId"`
	Message    string    `json:"message"`
}

// grokUsage is one request's counts on a `usage` line: InputTokens is the
// prompt WITHOUT its cached part, which CacheRead counts, and OutputTokens the
// reply with its reasoning in it (measured on 1.0.92: a 1,057-token prompt
// with 400 cached read as 657 and 400; 50 out with 20 reasoning as 50).
type grokUsage struct {
	InputTokens int `json:"input_tokens"`
	CacheRead   int `json:"cache_read_input_tokens"`
	CacheWrite  int `json:"cache_creation_input_tokens"`
	Output      int `json:"output_tokens"`
}

// The stop reasons of Grok Build's `end` (measured on 1.0.92): a run that ended
// on its own answer, one its turn cap stopped, and one cancelled, which under
// `--always-approve` is a run signalled to stop (SIGTERM or SIGINT, mid-request,
// mid-call or idle on a background subagent); only a run that may ask for an
// approval, never this runner's, is also cancelled where a call asked.
const (
	grokEndTurn   = "end_turn"
	grokCapped    = "max_turn_requests"
	grokCancelled = "cancelled"
)

// grokLimitWords are the words of a refusal Grok Build relays that name a spent
// plan or a rate limit, the grok profile's limit_markers (harnessprofile),
// which classify the run's limit report: its framing of a 402, and of a 429,
// whose code it writes first ("Some resource has been exhausted: Too many
// requests: rate limit exceeded…" and "…: Your team … has either used all
// available credits…", measured on 1.0.92); and its own sentences.
var grokLimitWords = []string{"status 402 Payment Required", "Some resource has been exhausted", "Rate limited",
	"usage limit", "spending limit"}

// grokOverflow is the words of the API's refusal of a request past the model's
// context as 1.0.92 relays it: "API error (status 400 Bad Request): Client
// specified an invalid argument: This model's maximum prompt length is 256000
// but the request contains 300512 tokens." (measured).
const grokOverflow = "maximum prompt length is"

// grokRecordMark is the type of the line a run's script adds to its stream
// once its harness exits, after which every line is one of the main session's
// record ([grokMainRecord]).
const grokRecordMark = "crewlet.main_record"

// grokEvents decodes a Grok Build run's stream ([Decoder]): its answer, the
// words of the main agent's last request that called no tool, off the main
// session's record the script adds after the mark ([grokMainRecord]), and off
// the stream only where it added none; how it ended, off its `end`, any
// `error` and the cap's event; the calls for its transcript; its spend, off
// every request's `usage`.
type grokEvents struct {
	sawLine, sawEvent bool
	sessionID         string

	// prose is the model's words of the request in hand, answer the words of
	// the last request that made no call: the stream's, whose untagged lines
	// may be a subagent's.
	prose  strings.Builder
	answer string

	// marked says the script added the main session's record, recorded the
	// words of its last request since the run's brief that made no call.
	marked   bool
	recorded string

	ended       bool
	stop        string
	capped      bool
	failure     string
	failureLine string
	limit       string
	overflowed  bool

	calls map[string]museCall
	seq   int

	// spent and requests are every `usage` line's: a subagent's requests ride
	// the run's one stream, untagged (measured on 1.0.92), and are its spend.
	spent    grokUsage
	requests int

	transcript transcriptLines
	following  bool
}

var (
	_ runSpender    = (*grokEvents)(nil)
	_ limitReporter = (*grokEvents)(nil)
)

// Entries implements [Decoder].
func (d *grokEvents) Entries(end bool) []string {
	d.following = true
	return d.transcript.take(end)
}

// Skipped implements [Decoder]. A record line past the bound may be a request
// that made a call, so it takes the answer back.
func (d *grokEvents) Skipped(n int64, bound int) {
	if d.marked {
		d.recorded = ""
		return
	}
	d.transcript.skip(n, bound)
}

// Line implements [Decoder].
func (d *grokEvents) Line(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	if d.marked {
		d.record(line)
		return
	}
	if bytes.Equal(bytes.TrimSpace(line), []byte(`{"type":"`+grokRecordMark+`"}`)) {
		d.marked = true
		return
	}
	d.sawLine = true
	var e grokEvent
	if json.Unmarshal(line, &e) != nil || e.Type == "" {
		return
	}
	d.sawEvent = true
	switch e.Type {
	case "text":
		d.prose.WriteString(e.Data)
	case "usage":
		// A REQUEST ENDS: its words, before any of its calls, are the answer
		// so far; a call after them takes it back.
		d.requests++
		d.spend(e.Usage)
		d.flush()
	case "tool_call":
		d.flush()
		d.answer = ""
		d.started(e)
	case "tool_call_update":
		d.updated(e)
	case "max_turns_reached":
		d.capped = true
	case "error":
		d.flush()
		d.failure = firstNonBlank(e.Message, "the run failed")
		d.failureLine = boundedEvent(line)
		d.limit = ""
		for _, w := range grokLimitWords {
			if strings.Contains(e.Message, w) {
				d.limit = d.failureLine
			}
		}
		d.overflowed = strings.Contains(e.Message, grokOverflow)
		d.transcript.add(activityline.Words(activityline.TagError, d.failure))
	case "end":
		d.flush()
		d.ended, d.stop = true, e.StopReason
		if e.SessionID != "" {
			d.sessionID = e.SessionID
		}
	}
}

// flush ends the words of the request in hand: the answer so far, and its
// transcript's line.
func (d *grokEvents) flush() {
	if text := strings.TrimSpace(d.prose.String()); text != "" {
		d.transcript.add(text)
		if !d.following {
			d.answer = text
		}
	}
	d.prose.Reset()
}

// record reads a line of the main session's record after the mark: a brief of
// the run's (a prompt with no synthetic_reason; Grok's notice that a
// background subagent completed carries one) starts its answer over, a
// request that made a call takes it back, and one with words is it.
func (d *grokEvents) record(line []byte) {
	l, ok := decodeGrokLine(line)
	switch {
	case !ok:
	case l.Type == "user" && l.Prompt != nil && l.Reason == "":
		d.recorded = ""
	case l.Type == "assistant" && len(l.Calls) > 0:
		d.recorded = ""
	case l.Type == "assistant":
		if text := strings.TrimSpace(l.text()); text != "" {
			d.recorded = text
		}
	}
}

// started notes a call's start and adds its line: the tool the model asked for,
// use_tool's by the name it gave.
func (d *grokEvents) started(e grokEvent) {
	d.seq++
	tool := firstNonBlank(e.ToolName, "tool")
	var input struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	subject := compactInput(e.RawInput)
	if tool == "use_tool" && json.Unmarshal(e.RawInput, &input) == nil && input.ToolName != "" {
		tool, subject = input.ToolName, compactInput(input.ToolInput)
	} else if s := kimiSubject(e.RawInput); s != "" {
		subject = s
	}
	if d.calls == nil {
		d.calls = map[string]museCall{}
	}
	if len(d.calls) < maxPendingCalls {
		d.calls[e.ToolCallID] = museCall{seq: d.seq, tool: tool}
	}
	for _, l := range activityline.Call(d.seq, tool, subject) {
		d.transcript.add(l)
	}
}

// updated adds a call's end: a final status's, `completed` or `failed`, its
// text the reason of one that failed. An update with no status is the call
// starting.
func (d *grokEvents) updated(e grokEvent) {
	if e.Status == nil {
		return
	}
	call, ok := d.calls[e.ToolCallID]
	if !ok {
		return
	}
	switch *e.Status {
	case "completed":
		delete(d.calls, e.ToolCallID)
		d.transcript.add(activityline.OK(call.seq, activityline.Took{}))
	case "failed":
		delete(d.calls, e.ToolCallID)
		var why string
		for _, c := range e.Content {
			why = firstNonBlank(why, c.Content.Text)
		}
		d.transcript.add(activityline.Failed(call.seq, firstNonBlank(why, "the call failed"), activityline.Took{}))
	}
}

// spend adds u to the run's spend.
func (d *grokEvents) spend(u grokUsage) {
	d.spent.InputTokens += u.InputTokens
	d.spent.CacheRead += u.CacheRead
	d.spent.CacheWrite += u.CacheWrite
	d.spent.Output += u.Output
}

// runSpend is the run's own spend ([runSpender]): every request's `usage`, its
// subagents' included, the prompt not served from the cache as Grok states it.
// Stated wherever a request was read. The session-title request every run, and
// every subagent, makes first, and a session's compaction state no `usage`
// (measured on 1.0.92), so the run is charged the rest of its requests. Grok's `end` total omits a
// background subagent's and says `usage_is_incomplete`, so it is not read.
func (d *grokEvents) runSpend() (runSpend, bool) {
	if d.requests == 0 {
		return runSpend{}, false
	}
	u := d.spent
	return runSpend{input: u.InputTokens, cacheRead: u.CacheRead, cacheWrite: u.CacheWrite, output: u.Output}, true
}

// limitReport is the run's rate limit or spent plan line ([limitReporter]): its
// error, only where Grok's words name one.
func (d *grokEvents) limitReport() string { return d.limit }

// Ended implements [Decoder]: the run's `end`, its turn cap's event, or an
// `error`, which 1.0.92 prints with no `end` after it (measured).
func (d *grokEvents) Ended() bool { return d.ended || d.capped || d.failure != "" }

// Requests implements [Decoder]: the `usage` lines, one a request, every
// agent's, a subagent's included. NOT COUNTED: the session title's request,
// which writes no usage line. Measured on 1.0.92 against a stand-in: one
// title request a run beside the loop's, whose 12 of 12 and 3 under a cap of
// 3 were counted, and 6 with a 429 and a 500 retried.
func (d *grokEvents) Requests() int { return d.requests }

// InFlight implements [Decoder]: none, as Grok's own --max-turns stops before
// the next request (measured on 1.0.92: a cap of 3, 3 requests of its loop).
func (d *grokEvents) InFlight() int { return 0 }

// Result implements [Decoder]: the answer, and how the run ended.
func (d *grokEvents) Result() sandbox.Result {
	if !d.sawLine {
		return sandbox.Result{Error: "the harness produced no output"}
	}
	if !d.sawEvent {
		return sandbox.Result{Error: "the harness's output is not its event stream, so it never reported how its run ended"}
	}
	d.flush()
	text := strings.TrimSpace(d.answer)
	if d.marked {
		text = d.recorded
	}
	// TURNS ARE THE PROVIDER CALLS THE SPEND COVERS (tokens.LaunchCalls), a
	// subagent's included, as Copilot's runner counts its subagents' requests;
	// Grok's own `num_turns` counts the main agent's alone.
	res := sandbox.Result{Text: text, SessionID: d.sessionID, DeliveredRefs: deliveredRefs(text), Turns: d.requests}
	res.Transcript, res.TranscriptElidedLines, res.TranscriptElidedBytes = d.transcript.result()
	if spent, stated := d.runSpend(); stated {
		res = spent.on(res)
	}
	switch {
	case d.capped || d.stop == grokCapped:
		res.Error = "the run reached its turn cap before it answered"
		res.RoundCapped = true
	case d.failure != "":
		res.Error = d.failure
		res.Event = d.failureLine
		res.ContextExhausted = d.overflowed
	case !d.ended:
		res.Error = "the harness stopped before it reported how its run ended"
	case d.stop == grokCancelled:
		res.Error = "the harness cancelled its run before it answered: it was signalled to stop"
	case d.stop != grokEndTurn:
		res.Error = "the harness ended its run on " + firstNonBlank(d.stop, "no stop reason")
	case text == "":
		res.Error = "the harness produced no answer"
	default:
		res.Success = true
	}
	res.Limits = d.limit
	return res
}
