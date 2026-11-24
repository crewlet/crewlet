package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// copilotEvent is the part of one line of Copilot's `--output-format json`
// stream (1.0.114), or of its session's events.jsonl, that this runner reads:
// `{type, data, id, timestamp, parentId, ephemeral?}`, and on the last line of
// the stream `{type: "result", sessionId, exitCode, usage}`. DECODED LOOSELY: a
// field a release retyped reads as zero, and the rest still read.
type copilotEvent struct {
	Type string `json:"type"`
	// AgentID names a subagent's event, on the line's envelope beside its
	// type; the main agent's and the session's carry none (measured on
	// 1.0.114, subagent-note).
	AgentID   string          `json:"agentId"`
	Data      copilotData     `json:"data"`
	Timestamp string          `json:"timestamp"`
	SessionID string          `json:"sessionId"`
	ExitCode  json.RawMessage `json:"exitCode"`
}

// copilotData is the `data` of the events read: a field each kind names, the
// rest zero. Raw where a kind's field is read only on that kind.
type copilotData struct {
	SessionID string `json:"sessionId"`

	// ParentToolCallID names the call that started the subagent a turn is
	// of.
	ParentToolCallID string `json:"parentToolCallId"`

	// Content is a message's text; ClientTurnID and Delivery a user
	// message's: the id its sender chose, and `idle` (the brief), `steering`
	// (sent while a tool ran) or `queued` (sent as the agent stopped).
	Content      string          `json:"content"`
	ClientTurnID string          `json:"clientTurnId"`
	Delivery     string          `json:"delivery"`
	ToolRequests json.RawMessage `json:"toolRequests"`

	// A tool call's start and end.
	ToolCallID      string          `json:"toolCallId"`
	ToolName        string          `json:"toolName"`
	Arguments       json.RawMessage `json:"arguments"`
	MCPServerName   string          `json:"mcpServerName"`
	MCPConfigSource string          `json:"mcpConfigSource"`
	Success         json.RawMessage `json:"success"`
	Result          struct {
		Content string `json:"content"`
	} `json:"result"`
	Error json.RawMessage `json:"error"`

	// A session error's kind and words, and the warning or notice a session
	// logs.
	ErrorType string `json:"errorType"`
	ErrorCode string `json:"errorCode"`
	Message   string `json:"message"`

	// Aborted is an `assistant.idle` that ended an aborted turn.
	Aborted bool `json:"aborted"`

	// Turns is the cap the script's [copilotCapMark] line names.
	Turns int `json:"turns"`

	// Usage is one model request's, on a `session.usage_record`.
	Usage *copilotUsageRecord `json:"usage"`
}

// copilotUsageRecord is one request's usage (`session.usage_record`'s
// `data.usage`): InputTokens is the provider's whole prompt, its cached share
// inside it (measured: `prompt_tokens` 15918 with 7959 cached read as 15918 and
// 7959); Cost is AI credits, not dollars, and never read; MaxPromptTokens is
// the prompt budget Copilot held the model to.
type copilotUsageRecord struct {
	InputTokens      int   `json:"inputTokens"`
	OutputTokens     int   `json:"outputTokens"`
	CacheReadTokens  int   `json:"cacheReadTokens"`
	CacheWriteTokens int   `json:"cacheWriteTokens"`
	MaxPromptTokens  int64 `json:"maxPromptTokens"`
}

// copilotDenied is the code of a tool call a hook refused (`error.code`), and
// copilotDeniedBy the opening of its words: `Denied by preToolUse hook: <the
// hook's reason>` (measured on 1.0.114).
const (
	copilotDenied   = "denied"
	copilotDeniedBy = "Denied by "
)

// The session errors read as a spent plan or rate limit (`errorType`,
// session-events.d.ts on 1.0.114), and as a conversation past its model's
// context (measured: a provider's 400 "prompt is too long").
const (
	copilotRateLimit    = "rate_limit"
	copilotQuota        = "quota"
	copilotContextLimit = "context_limit"
)

// copilotQuery is the errorType of a request Copilot refused before sending it,
// and copilotPastBudget the opening of its words where the reason is the
// conversation past the model's prompt budget: "Request exceeds the model's
// prompt budget after reserving output (73601 tokens; limit 40000)" (measured on
// 1.0.114, budget-refused). `query` names no reason of its own and the line no
// other field, so the words tell this refusal from any other `query`.
const (
	copilotQuery      = "query"
	copilotPastBudget = "Request exceeds the model's prompt budget"
)

// decodeCopilotEvent decodes one line, false where it is no JSON object of a
// type.
func decodeCopilotEvent(line []byte) (copilotEvent, bool) {
	var ev copilotEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		// LOOSELY: a field of another type fails the whole decode, so the
		// line is read again for its type alone.
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &typed) != nil || typed.Type == "" {
			return copilotEvent{}, false
		}
		return copilotEvent{Type: typed.Type}, true
	}
	return ev, ev.Type != ""
}

// copilotEvents decodes a Copilot run's stream ([Decoder]): its answer, the
// last root `assistant.message`'s content; how it ended, off the `result` line,
// the last `session.error`, an abort and the extension's cap line; the calls
// for its transcript; and, after the script's mark ([copilotUsageMark]), the
// run's own usage records.
type copilotEvents struct {
	sawLine, sawEvent bool
	sessionID         string

	// answer is the content of the last root assistant message.
	answer string

	// result is whether the stream's `result` line was read, and exit its
	// exitCode.
	result bool
	exit   int

	// failure is the last session error's words, its line and kind; limit the
	// last line of a rate limit or a spent quota ([limitReporter]).
	failure, failureLine, failureType string
	limit                             string

	// aborted is an `abort` line, idleAborted an `assistant.idle` whose turn
	// was aborted, and capped the extension's line saying the cap stopped the
	// run ([copilotCapMessage]): the three together are a capped run.
	aborted, idleAborted bool
	capped               string

	// capTurns is the turn cap the script said the run had, 0 for none, and
	// armed whether the extension that enforces it said it ran.
	capTurns int
	armed    bool

	// denied is the first seat tool call a hook refused: the tool and why.
	denied string

	// calls is each running call's place in the transcript and whether it is
	// a seat tool's (one the run's own MCP configuration declared).
	calls map[string]copilotCall
	seq   int

	// marked is whether the script's usage mark was read; spent what the
	// records after it state, requests how many, and window the least prompt
	// budget one stated.
	marked   bool
	spent    copilotUsageRecord
	requests int
	window   int64

	transcript transcriptLines
	following  bool
}

// copilotCall is one call the stream started: its place in the transcript, its
// tool, and whether the tool is the seat's.
type copilotCall struct {
	seq  int
	tool string
	seat bool
}

var (
	_ runSpender     = (*copilotEvents)(nil)
	_ limitReporter  = (*copilotEvents)(nil)
	_ windowReporter = (*copilotEvents)(nil)
)

// Entries implements [Decoder].
func (d *copilotEvents) Entries(end bool) []string {
	d.following = true
	d.answer = ""
	return d.transcript.take(end)
}

// Skipped implements [Decoder].
func (d *copilotEvents) Skipped(n int64, bound int) { d.transcript.skip(n, bound) }

// Line implements [Decoder].
func (d *copilotEvents) Line(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	d.sawLine = true
	ev, ok := decodeCopilotEvent(line)
	if !ok {
		return
	}
	d.sawEvent = true
	data := ev.Data
	root := ev.AgentID == "" && data.ParentToolCallID == ""
	switch ev.Type {
	case "session.start":
		d.sessionID = firstNonBlank(data.SessionID, d.sessionID)
	case "result":
		d.result = true
		d.sessionID = firstNonBlank(ev.SessionID, d.sessionID)
		d.exit = int(looseInt(ev.ExitCode))
	case "assistant.message":
		if !root {
			return
		}
		if !d.following {
			d.answer = data.Content
		}
		if text := strings.TrimSpace(data.Content); text != "" {
			d.transcript.add(text)
		}
	case "user.message":
		if strings.HasPrefix(data.ClientTurnID, copilotTurnPrefix) {
			d.transcript.add(activityline.Words(activityline.TagSteer, firstLine(data.Content, transcriptDetailLimit)))
		}
	case "tool.execution_start":
		d.started(data)
	case "tool.execution_complete":
		d.completed(data)
	case "session.error":
		d.failure, d.failureType = firstNonBlank(data.Message, data.ErrorType, "the session failed"), data.ErrorType
		d.failureLine = boundedEvent(line)
		if data.ErrorType == copilotRateLimit || data.ErrorType == copilotQuota {
			d.limit = d.failureLine
		}
		d.transcript.add(activityline.Words(activityline.TagError, d.failure))
	case "abort":
		d.aborted = true
	case "assistant.idle":
		d.idleAborted = d.idleAborted || (root && data.Aborted)
	case "session.warning", "session.info":
		switch {
		case strings.HasPrefix(data.Message, copilotCapMessage):
			d.capped = strings.TrimPrefix(data.Message, "crewlet: ")
		case data.Message == copilotArmedMessage:
			d.armed = true
		}
	case copilotCapMark:
		d.capTurns = data.Turns
	case copilotUsageMark:
		d.marked = true
	case "session.usage_record":
		// ONLY AFTER THE MARK: the script adds the run's own records there,
		// and a stream that printed one of its own would be counted twice.
		if d.marked && data.Usage != nil {
			u := data.Usage
			d.spent.InputTokens += u.InputTokens
			d.spent.OutputTokens += u.OutputTokens
			d.spent.CacheReadTokens += u.CacheReadTokens
			d.spent.CacheWriteTokens += u.CacheWriteTokens
			d.requests++
			if u.MaxPromptTokens > 0 && (d.window == 0 || u.MaxPromptTokens < d.window) {
				d.window = u.MaxPromptTokens
			}
		}
	}
}

// started notes a call's start and adds its line.
func (d *copilotEvents) started(data copilotData) {
	d.seq++
	if d.calls == nil {
		d.calls = map[string]copilotCall{}
	}
	tool := firstNonBlank(data.ToolName, "tool")
	// A SEAT TOOL is one of an MCP server the run's own configuration
	// declared, which Copilot files as the user's (`mcpConfigSource`); a
	// repository's `.mcp.json` server is the workspace's.
	seat := data.MCPServerName != "" && data.MCPConfigSource == "user"
	if len(d.calls) < maxPendingCalls {
		d.calls[data.ToolCallID] = copilotCall{seq: d.seq, tool: tool, seat: seat}
	}
	for _, l := range activityline.Call(d.seq, tool, copilotSubject(data.Arguments)) {
		d.transcript.add(l)
	}
}

// completed adds a call's end, and notes a seat tool a hook refused.
func (d *copilotEvents) completed(data copilotData) {
	call, ok := d.calls[data.ToolCallID]
	delete(d.calls, data.ToolCallID)
	if !ok {
		d.seq++
		call = copilotCall{seq: d.seq, tool: firstNonBlank(data.ToolName, "tool")}
	}
	if looseTrue(data.Success) {
		d.transcript.add(activityline.OK(call.seq, activityline.Took{}))
		return
	}
	var failed struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	_ = json.Unmarshal(data.Error, &failed)
	why := firstNonBlank(failed.Message, errorText(data.Error), "failed")
	d.transcript.add(activityline.Failed(call.seq, why, activityline.Took{}))
	if call.seat && d.denied == "" && failed.Code == copilotDenied && strings.HasPrefix(failed.Message, copilotDeniedBy) {
		d.denied = fmt.Sprintf("a hook refused the seat's tool %s, so the run could not do its work through it: %s",
			call.tool, failed.Message)
	}
}

// copilotSubject is a call's input as a transcript names it: the field that
// says what it acts on, else the input as compact JSON.
func copilotSubject(raw json.RawMessage) string {
	var input struct {
		Command string `json:"command"`
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		URL     string `json:"url"`
		Desc    string `json:"description"`
	}
	_ = json.Unmarshal(raw, &input)
	if subject := firstNonBlank(input.Command, input.Path, input.Pattern, input.URL, input.Desc); subject != "" {
		return subject
	}
	return compactInput(raw)
}

// runSpend is the run's own spend ([runSpender]): the records after the
// script's mark, the prompt not served from the cache being the whole less
// its cached shares, since Copilot's inputTokens counts them. Stated wherever
// the mark was read, a run that made no request spending nothing.
func (d *copilotEvents) runSpend() (runSpend, bool) {
	if !d.marked {
		return runSpend{}, false
	}
	u := d.spent
	return runSpend{input: u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens, cacheRead: u.CacheReadTokens,
		cacheWrite: u.CacheWriteTokens, output: u.OutputTokens}, true
}

// limitReport is the run's last rate limit or spent quota line
// ([limitReporter]): a `session.error` of errorType `rate_limit` or `quota`,
// whose `errorCode` names the window (`user_weekly_rate_limited`,
// `quota_exceeded`, …; not measured against GitHub's service).
func (d *copilotEvents) limitReport() string { return d.limit }

// reportedWindow is the least prompt budget a request of the run was held to
// ([windowReporter]): `maxPromptTokens` of its usage records.
func (d *copilotEvents) reportedWindow() int64 { return d.window }

// Ended implements [Decoder]: the stream's `result` event, which Copilot
// prints as its run ends, with its exit status.
func (d *copilotEvents) Ended() bool { return d.result }

// Requests implements [Decoder]: the usage records of the session's own
// events the script appends, one a model request, a subagent's included.
func (d *copilotEvents) Requests() int { return d.requests }

// Result implements [Decoder]: the answer, and how the run ended.
//
// A RUN WITH A CAP WHOSE EXTENSION NEVER RAN fails whatever it answered: its
// script named the cap ([copilotCapMark]) and its stream lacks the extension's
// [copilotArmedMessage], so nothing held it to the cap; and so does one whose
// own usage records, a model request each, number more than its cap.
//
// A CAPPED RUN is one the extension aborted ([copilotCapMessage]): its line, an
// `abort`, an aborted idle and exit 1 with no session error, all four. The line
// alone labels nothing, so a run that logged it and went on is read by how it
// ended.
func (d *copilotEvents) Result() sandbox.Result {
	if !d.sawLine {
		return sandbox.Result{Error: "the harness produced no output"}
	}
	if !d.sawEvent {
		return sandbox.Result{Error: "the harness's output is not its event stream, so it never reported how its run ended"}
	}
	text := strings.TrimSpace(d.answer)
	res := sandbox.Result{Text: text, SessionID: d.sessionID, DeliveredRefs: deliveredRefs(text), Turns: d.requests}
	res.Transcript, res.TranscriptElidedLines, res.TranscriptElidedBytes = d.transcript.result()
	if spent, stated := d.runSpend(); stated {
		res = spent.on(res)
	}
	capped := d.capped != "" && d.aborted && d.idleAborted && d.result && d.exit == 1 && d.failure == ""
	switch {
	case d.capTurns > 0 && !d.armed:
		// FAILED CLOSED: the run went on with nothing holding it to its cap.
		res.Error = fmt.Sprintf("this run had a turn cap of %d model turns, and the extension that enforces "+
			"it never ran, so the cap was not enforced: %s", d.capTurns, (Copilot{}).notesOffWhy())
	case d.denied != "":
		res.Error = d.denied
	case d.failure != "":
		res.Error = d.failure
		res.ContextExhausted = d.failureType == copilotContextLimit ||
			(d.failureType == copilotQuery && strings.HasPrefix(d.failure, copilotPastBudget))
		res.Event = d.failureLine
	case capped:
		res.Error = d.capped
		res.RoundCapped = true
	case !d.result:
		res.Error = "the harness stopped before it reported how its run ended"
	case d.aborted:
		res.Error = fmt.Sprintf("the run was aborted before it finished, and exited with status %d", d.exit)
	case d.exit != 0:
		res.Error = fmt.Sprintf("the harness exited with status %d", d.exit)
	case text == "":
		res.Error = "the harness produced no answer"
	default:
		res.Success = true
	}
	res.Limits = d.limit
	return res
}
