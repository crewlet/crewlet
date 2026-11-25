package executor

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// museEnvelope is the part of one line of Muse Code's `exec --json` stream
// (1.4.25), or of a session's session.jsonl, that this runner reads:
// `{schema_version, id, stream{kind,id}, sequence, recorded_at, record_type,
// payload_type, payload}`. DECODED LOOSELY: a field a release retyped reads
// as zero, and the rest still read.
type museEnvelope struct {
	PayloadType string          `json:"payload_type"`
	Stream      museStream      `json:"stream"`
	RecordedAt  int64           `json:"recorded_at"`
	Payload     json.RawMessage `json:"payload"`
}

type museStream struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// musePayload is the payload fields the stream's events read, each named by
// its kind, the rest zero.
type musePayload struct {
	// Text is a delta's prose, a terminal's answer or a result's text;
	// Reason a failed terminal's words.
	Text   string `json:"text"`
	Reason string `json:"reason"`

	// Event is a task's lifecycle event.
	Event struct {
		Kind           string     `json:"kind"`
		TaskID         string     `json:"task_id"`
		TaskKind       string     `json:"task_kind"`
		IdempotencyKey string     `json:"idempotency_key"`
		Usage          *museUsage `json:"usage"`
	} `json:"event"`

	// CallID and Facts are a tool result's.
	CallID string `json:"call_id"`
	Facts  struct {
		Outcome  string `json:"outcome"`
		ToolName string `json:"tool_name"`
	} `json:"correlation_facts"`

	// Calls and Models are the run's usage, once per run; Completed and
	// Cancelled the calls that were answered or cut off, a failed attempt being
	// neither ([museEvents.calls]).
	Calls     int         `json:"calls"`
	Completed *int        `json:"completed_calls"`
	Cancelled int         `json:"cancelled_calls"`
	Models    []museUsage `json:"models"`
}

// calls is the requests a `run.usage` counts: those answered and those cut off,
// never an attempt that failed. MUSE RETRIES A FAILED ATTEMPT AND STATES IT
// APART (`failed_calls`): measured on 1.4.25 against a stand-in, a 429 and a
// 500 each retried once stated 8 calls, 6 completed and 2 failed, for 6
// requests. An attempt that failed bills nothing. A usage that states no
// completed calls is read by its calls.
func (p musePayload) calls() int {
	if p.Completed == nil {
		return p.Calls
	}
	return *p.Completed + p.Cancelled
}

// museUsage is one model's usage on `run.usage` (`models[]`), or one request's
// on a session record's `model_completed`: InputTokens is the whole prompt,
// its cached share inside it, and OutputTokens the reply, its reasoning inside
// it (measured on 1.4.25: 1000 input with 400 cached read as 1000 and 400).
type museUsage struct {
	InputTokens      int `json:"input_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	OutputTokens     int `json:"output_tokens"`
}

// decodeMuseEnvelope decodes one line, false where it is no JSON object of a
// payload type.
func decodeMuseEnvelope(line []byte) (museEnvelope, bool) {
	var env museEnvelope
	if json.Unmarshal(line, &env) != nil || env.PayloadType == "" {
		return museEnvelope{}, false
	}
	return env, true
}

// The failure reasons of a run Muse Code ended on a rate limit or a spent
// balance (`run.terminal.failed`'s reason: "API error 429: rate limited
// (rate_limit_error) (after 3 provider attempts)", "API error 402: Billing
// verification failed (billing_error)", measured against a stand-in model),
// and the opening of a run its turn cap stopped ("model did not reach a
// terminal state within 2 step(s)", `--max-model-steps 2`, measured).
const (
	museRateLimit = "(rate_limit_error)"
	museBilling   = "(billing_error)"
	museCapped    = "model did not reach a terminal state within "
)

// museChildMark is the type of the line a run's script adds to its stream
// once its harness exits, after which every line is a `model_completed`
// record of one of the session's subagents' threads written by this run
// ([museChildUsage]): Muse's `run.usage` is the main thread's alone
// (`children_included: false`), and its stream carries nothing of a child.
const museChildMark = "crewlet.child_usage"

// museEvents decodes a Muse Code run's stream ([Decoder]): its answer, off the
// last `run.terminal.completed` line; how it ended, off the last terminal and
// any run its cap stopped; the calls for its transcript; its usage, off every
// `run.usage` and, after the script's mark, its subagents' records. ONE EXEC
// MAY HOLD SEVERAL RUNS: Muse starts another once a subagent's result arrives
// after its run ended (measured on 1.4.25).
type museEvents struct {
	sawLine, sawEvent bool
	sessionID         string

	// prose is the model's words since its last request began, a delta at a
	// time.
	prose strings.Builder

	// answer is the last terminal's text, completed whether it said the run
	// completed, failure a failed last terminal's reason and its line, limit
	// its line where it names a rate limit or a spent balance; capped the
	// reason of any run its cap stopped.
	answer      string
	completed   bool
	failure     string
	failureLine string
	limit       string
	capped      string

	// calls is each running call's place in the transcript, by its call id.
	calls map[string]museCall
	tasks map[string]museCall
	seq   int

	// spent is what the run's usage and its children's records state,
	// stated whether `run.usage` was read, and requests how many requests
	// the main thread made.
	spent    museUsage
	stated   bool
	requests int
	marked   bool

	transcript transcriptLines
	following  bool
}

// museCall is one call the stream started: its place and its tool.
type museCall struct {
	seq  int
	tool string
}

var (
	_ runSpender    = (*museEvents)(nil)
	_ limitReporter = (*museEvents)(nil)
)

// Entries implements [Decoder].
func (d *museEvents) Entries(end bool) []string {
	d.following = true
	d.answer = ""
	return d.transcript.take(end)
}

// Skipped implements [Decoder].
func (d *museEvents) Skipped(n int64, bound int) { d.transcript.skip(n, bound) }

// Line implements [Decoder].
func (d *museEvents) Line(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	d.sawLine = true
	env, ok := decodeMuseEnvelope(line)
	if !ok {
		return
	}
	d.sawEvent = true
	if env.Stream.Kind == "session" && d.sessionID == "" {
		d.sessionID = env.Stream.ID
	}
	var p musePayload
	_ = json.Unmarshal(env.Payload, &p)
	switch env.PayloadType {
	case "run.output.delta":
		d.prose.WriteString(p.Text)
	case "task.lifecycle.proposed":
		d.flush()
		if tool, isTool := strings.CutPrefix(p.Event.TaskKind, "tool."); isTool {
			d.started(p.Event.TaskID, tool)
		}
	case "task.lifecycle.side_effect_intent":
		// THE CALL'S ID, `tool:<call id>`, which its result names.
		if id, isCall := strings.CutPrefix(p.Event.IdempotencyKey, "tool:"); isCall {
			if call, ok := d.tasks[p.Event.TaskID]; ok {
				delete(d.tasks, p.Event.TaskID)
				if len(d.calls) < maxPendingCalls {
					d.calls[id] = call
				}
			}
		}
	case "task.lifecycle.completed":
		d.flush()
	case "tool.result":
		d.result(p)
	case "run.usage":
		// ONCE PER RUN, the main thread's: whole prompts, cache inside.
		d.stated = true
		d.requests += p.calls()
		for _, m := range p.Models {
			d.spend(m)
		}
	case museChildMark:
		d.marked = true
	case "runtime.session":
		// ONLY AFTER THE MARK: the script adds the run's subagents' requests
		// there, and the stream itself prints no record of a session.
		if d.marked && p.Event.Kind == "model_completed" && p.Event.Usage != nil {
			d.spend(*p.Event.Usage)
		}
	case "run.terminal.completed":
		d.flush()
		d.completed, d.failure, d.failureLine, d.limit = true, "", "", ""
		if !d.following {
			d.answer = p.Text
		}
	case "run.terminal.failed":
		d.flush()
		d.completed, d.limit, d.failureLine = false, "", ""
		d.failure = firstNonBlank(p.Reason, "the run failed")
		if strings.HasPrefix(p.Reason, museCapped) {
			d.capped = p.Reason
		}
		// WHOLE OR NOT AT ALL: past what a record keeps of an event, the
		// reason alone is said.
		d.failureLine = boundedEvent(line)
		if strings.Contains(p.Reason, museRateLimit) || strings.Contains(p.Reason, museBilling) {
			d.limit = d.failureLine
		}
		d.transcript.add(activityline.Words(activityline.TagError, d.failure))
	}
}

// flush adds the model's words since its last request began, if any.
func (d *museEvents) flush() {
	if text := strings.TrimSpace(d.prose.String()); text != "" {
		d.transcript.add(text)
	}
	d.prose.Reset()
}

// started notes a call's start and adds its line. The stream carries no
// call's input, so the line names the tool alone.
func (d *museEvents) started(task, tool string) {
	d.seq++
	if d.tasks == nil {
		d.tasks, d.calls = map[string]museCall{}, map[string]museCall{}
	}
	if len(d.tasks) < maxPendingCalls {
		d.tasks[task] = museCall{seq: d.seq, tool: tool}
	}
	for _, l := range activityline.Call(d.seq, tool, "") {
		d.transcript.add(l)
	}
}

// result adds a call's end: `tool.result`'s outcome, its text the reason of
// one that failed.
func (d *museEvents) result(p musePayload) {
	call, ok := d.calls[p.CallID]
	delete(d.calls, p.CallID)
	if !ok {
		d.seq++
		call = museCall{seq: d.seq, tool: firstNonBlank(p.Facts.ToolName, "tool")}
	}
	if p.Facts.Outcome == "" || p.Facts.Outcome == "success" {
		d.transcript.add(activityline.OK(call.seq, activityline.Took{}))
		return
	}
	d.transcript.add(activityline.Failed(call.seq, firstNonBlank(p.Text, p.Facts.Outcome), activityline.Took{}))
}

// spend adds u to the run's spend.
func (d *museEvents) spend(u museUsage) {
	d.spent.InputTokens += u.InputTokens
	d.spent.CachedTokens += u.CachedTokens
	d.spent.CacheWriteTokens += u.CacheWriteTokens
	d.spent.OutputTokens += u.OutputTokens
}

// runSpend is the run's own spend ([runSpender]): its `run.usage` and its
// subagents' records, the prompt not served from the cache being the whole
// less its cached share. Stated wherever `run.usage` was read.
func (d *museEvents) runSpend() (runSpend, bool) {
	if !d.stated {
		return runSpend{}, false
	}
	u := d.spent
	return runSpend{input: u.InputTokens - u.CachedTokens - u.CacheWriteTokens, cacheRead: u.CachedTokens,
		cacheWrite: u.CacheWriteTokens, output: u.OutputTokens}, true
}

// limitReport is the run's rate limit or spent balance line ([limitReporter]):
// its failed terminal, only where Muse's own failure names one.
func (d *museEvents) limitReport() string { return d.limit }

// Ended implements [Decoder]: a terminal line, `run.terminal.completed` or
// `run.terminal.failed`.
func (d *museEvents) Ended() bool { return d.completed || d.failure != "" }

// Requests implements [Decoder]: the calls each exec run's `run.usage` states,
// but for the failed attempts it states apart ([musePayload.calls]).
func (d *museEvents) Requests() int { return d.requests }

// InFlight implements [Decoder]: none, as Muse's own --max-model-steps stops
// before the next request (measured on 1.4.25: a cap of 3, 3 requests).
func (d *museEvents) InFlight() int { return 0 }

// Result implements [Decoder]: the answer, and how the run ended.
func (d *museEvents) Result() sandbox.Result {
	if !d.sawLine {
		return sandbox.Result{Error: "the harness produced no output"}
	}
	if !d.sawEvent {
		return sandbox.Result{Error: "the harness's output is not its event stream, so it never reported how its run ended"}
	}
	d.flush()
	text := strings.TrimSpace(d.answer)
	res := sandbox.Result{Text: text, SessionID: d.sessionID, DeliveredRefs: deliveredRefs(text), Turns: d.requests}
	res.Transcript, res.TranscriptElidedLines, res.TranscriptElidedBytes = d.transcript.result()
	if spent, stated := d.runSpend(); stated {
		res = spent.on(res)
	}
	switch {
	case d.capped != "":
		res.Error = d.capped
		res.RoundCapped = true
	case d.failure != "":
		res.Error = d.failure
		res.Event = d.failureLine
	case !d.completed:
		res.Error = "the harness stopped before it reported how its run ended"
	case text == "":
		res.Error = "the harness produced no answer"
	default:
		res.Success = true
	}
	res.Limits = d.limit
	return res
}
