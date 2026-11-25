package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// cursorLine is the part of one line of Cursor's `stream-json` stream
// ([cursorSupported]) that this runner reads, of the `turn_ended` line its
// transcript records ([cursorTurnEnded]), or of the script's cap line
// ([cursorCapType]). DECODED LOOSELY: a field a release retyped reads as zero.
type cursorLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	// SessionID is every stream line's; Model system/init's.
	SessionID string `json:"session_id"`
	// Message is an assistant line's.
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	// A tool call's id, the model response it is part of, and its call.
	CallID      string                     `json:"call_id"`
	ModelCallID string                     `json:"model_call_id"`
	ToolCall    map[string]json.RawMessage `json:"tool_call"`
	// The result line's.
	IsError bool       `json:"is_error"`
	Result  string     `json:"result"`
	Usage   *cursorUse `json:"usage"`
	// The transcript's turn_ended: success or error, and the error's words.
	Status string `json:"status"`
	Error  string `json:"error"`
	// Cap is the cap line's.
	Cap int `json:"cap"`
	// Attempt is a retry's and a reconnection's.
	Attempt int `json:"attempt"`
}

// cursorUse is a result's usage: the prompt NOT served from the cache, its
// cached shares beside it, and the reply (measured: sent 100 with 40 cached,
// reported 60 and 40).
type cursorUse struct {
	Input      int `json:"inputTokens"`
	Output     int `json:"outputTokens"`
	CacheRead  int `json:"cacheReadTokens"`
	CacheWrite int `json:"cacheWriteTokens"`
}

// cursorCall is one tool call as a tool_call line carries it: its arguments,
// and on its completion its result, a success or an error.
type cursorCall struct {
	Args   json.RawMessage `json:"args"`
	Result *struct {
		Success json.RawMessage `json:"success"`
		Error   *struct {
			Error string `json:"error"`
		} `json:"error"`
	} `json:"result"`
}

// cursorEvents decodes a Cursor run's stream ([Decoder]): its answer, the last
// assistant line's text; how it ended, off its result line, Cursor's own
// turn_ended, or the script's cap line; the calls for its transcript; and its
// own spend, the result's usage (a resumed run reports its own).
type cursorEvents struct {
	sawLine, sawEvent bool
	sessionID         string

	answer string
	// result is the result line's text, resulted that one was read and
	// failed its is_error; turnEnded the transcript's turn_ended, turnError
	// its error's words; capped the cap the script says stopped the run.
	result           string
	resulted, failed bool
	turnEnded        bool
	turnStatus       string
	turnError        string
	capped           int
	// responses is the model responses that called a tool, counted where a
	// call starts under a model call other than the last one started
	// ([cursorEvents.responded]); lastCall that model call.
	responses  int
	lastCall   string
	spent      cursorUse
	stated     bool
	calls      map[string]int
	seq        int
	transcript transcriptLines
	following  bool
}

var _ runSpender = (*cursorEvents)(nil)

// Entries implements [Decoder].
func (d *cursorEvents) Entries(end bool) []string {
	d.following = true
	d.answer = ""
	return d.transcript.take(end)
}

// Skipped implements [Decoder].
func (d *cursorEvents) Skipped(n int64, bound int) { d.transcript.skip(n, bound) }

// Line implements [Decoder].
func (d *cursorEvents) Line(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	d.sawLine = true
	var ev cursorLine
	if err := json.Unmarshal(line, &ev); err != nil || ev.Type == "" {
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &typed) != nil || typed.Type == "" {
			return
		}
		ev = cursorLine{Type: typed.Type}
	}
	d.sawEvent = true
	d.sessionID = firstNonBlank(ev.SessionID, d.sessionID)
	switch ev.Type {
	case "assistant":
		if ev.Message != nil {
			d.assistant(piText(ev.Message.Content))
		}
	case "tool_call":
		switch ev.Subtype {
		case "started":
			d.responded(ev.ModelCallID)
			d.started(ev)
		case "completed":
			d.completed(ev)
		}
	case "connection", "retry":
		if ev.Subtype == "reconnecting" || ev.Subtype == "starting" {
			d.transcript.add(activityline.Words(activityline.TagError,
				fmt.Sprintf("reaching Cursor's service again (attempt %d)", ev.Attempt)))
		}
	case "result":
		d.resulted, d.failed, d.result = true, ev.IsError || ev.Subtype != "success", ev.Result
		if ev.Usage != nil {
			d.spent, d.stated = *ev.Usage, true
		}
	case "turn_ended":
		d.turnEnded, d.turnStatus, d.turnError = true, ev.Status, ev.Error
		if ev.Status != "success" {
			d.transcript.add(activityline.Words(activityline.TagError,
				firstLine(firstNonBlank(ev.Error, "the turn ended "+ev.Status), transcriptDetailLimit)))
		}
	case cursorCapType:
		d.capped = max(ev.Cap, 1)
	}
}

// assistant reads one assistant line: the answer, as the last one's text.
func (d *cursorEvents) assistant(text string) {
	if !d.following {
		d.answer = text
	}
	if strings.TrimSpace(text) != "" {
		d.transcript.add(text)
	}
}

// cursorToolName is a call's tool as the stream names it: an MCP call's
// `<server>-<tool>`, another's kind with its `ToolCall` suffix dropped
// (`shellToolCall` is `shell`).
func cursorToolName(kind string, call cursorCall) string {
	if kind == "mcpToolCall" {
		var args struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(call.Args, &args)
		if args.Name != "" {
			return args.Name
		}
	}
	return firstNonBlank(strings.TrimSuffix(kind, "ToolCall"), "tool")
}

// cursorCallOf is a tool_call line's one call and its kind.
func cursorCallOf(ev cursorLine) (string, cursorCall) {
	for kind, raw := range ev.ToolCall {
		var c cursorCall
		if json.Unmarshal(raw, &c) == nil {
			return kind, c
		}
	}
	return "", cursorCall{}
}

// started notes a call's start and adds its line.
func (d *cursorEvents) started(ev cursorLine) {
	kind, call := cursorCallOf(ev)
	d.seq++
	if d.calls == nil {
		d.calls = map[string]int{}
	}
	if len(d.calls) < maxPendingCalls {
		d.calls[ev.CallID] = d.seq
	}
	for _, l := range activityline.Call(d.seq, cursorToolName(kind, call), piSubject(call.Args)) {
		d.transcript.add(l)
	}
}

// completed adds a call's end: failed where Cursor recorded an error.
func (d *cursorEvents) completed(ev cursorLine) {
	_, call := cursorCallOf(ev)
	seq, ok := d.calls[ev.CallID]
	delete(d.calls, ev.CallID)
	if !ok {
		d.seq++
		seq = d.seq
	}
	if call.Result != nil && call.Result.Error != nil {
		d.transcript.add(activityline.Failed(seq, firstNonBlank(firstLine(call.Result.Error.Error,
			transcriptDetailLimit), "failed"), activityline.Took{}))
		return
	}
	d.transcript.add(activityline.OK(seq, activityline.Took{}))
}

// runSpend is the run's own spend ([runSpender]): the result's usage, stated
// where the run reached its result.
func (d *cursorEvents) runSpend() (runSpend, bool) {
	if !d.stated {
		return runSpend{}, false
	}
	u := d.spent
	return runSpend{input: u.Input, cacheRead: u.CacheRead, cacheWrite: u.CacheWrite, output: u.Output}, true
}

// Ended implements [Decoder]: Cursor's own account of how its turn ended, its
// result line or its transcript's turn_ended, or the script's cap line, which
// stops a run that would otherwise print neither. Cursor prints nothing of
// its end when it is interrupted, terminated or killed (measured on
// [cursorSupported]).
func (d *cursorEvents) Ended() bool { return d.resulted || d.turnEnded || d.capped > 0 }

// Requests implements [Decoder]: the responses that called a tool
// ([cursorEvents.responded]), and the response that answered, where one did;
// [cursorEvents.Result]'s Turns too.
func (d *cursorEvents) Requests() int {
	text := strings.TrimSpace(firstNonBlank(d.answer, d.result))
	if text != "" && d.resulted && !d.failed {
		return d.responses + 1
	}
	return d.responses
}

// InFlight implements [Decoder]: one. The follower interrupts Cursor once it
// reads the cap-th response that calls a tool off the stream, and by then the
// call has run and the next request is made: measured on [cursorSupported],
// a cap of 3 stopped after 4 requests.
func (d *cursorEvents) InFlight() int { return 1 }

// responded counts the response a call that starts under modelCall made, once:
// a response's calls all start before the next request is made, which needs
// their results, so a model call other than the last one started is a new
// response. EXACT AT ANY LENGTH, where a set of the model calls seen would be
// bounded and under-count past its bound. A call's end can come later (one in
// the background), so only starts are read. Measured on [cursorSupported]:
// 119, 167 and 4 responses counted alike by starts and by every model call
// seen.
func (d *cursorEvents) responded(modelCall string) {
	if modelCall == "" || modelCall == d.lastCall {
		return
	}
	d.lastCall = modelCall
	d.responses++
}

// Result implements [Decoder]: the answer, and how the run ended. A RESULT
// WINS: a run whose cap-th response was its answer finished before the
// interrupt reached it.
func (d *cursorEvents) Result() sandbox.Result {
	if !d.sawLine {
		return sandbox.Result{Error: "the harness produced no output"}
	}
	if !d.sawEvent {
		return sandbox.Result{Error: "the harness's output is not its event stream, so it never reported how its run ended"}
	}
	text := strings.TrimSpace(firstNonBlank(d.answer, d.result))
	res := sandbox.Result{Text: text, SessionID: d.sessionID, DeliveredRefs: deliveredRefs(text),
		Turns: d.Requests()}
	res.Transcript, res.TranscriptElidedLines, res.TranscriptElidedBytes = d.transcript.result()
	if spent, stated := d.runSpend(); stated {
		res = spent.on(res)
	}
	switch {
	case d.resulted && d.failed:
		res.Error = firstNonBlank(strings.TrimSpace(d.result), d.turnError, "the run failed")
	case d.resulted:
		if text == "" {
			res.Error = "the harness produced no answer"
		} else {
			res.Success = true
		}
	case d.capped > 0:
		res.Error = fmt.Sprintf("this run reached its turn cap of %d model responses and was stopped", d.capped)
		res.RoundCapped = true
	case d.turnEnded && d.turnStatus != "success":
		res.Error = firstNonBlank(strings.TrimSpace(d.turnError), "the turn ended "+d.turnStatus)
	case d.turnEnded:
		if text == "" {
			res.Error = "the harness produced no answer"
		} else {
			res.Success = true
		}
	default:
		res.Error = "the harness stopped before it reported how its run ended"
	}
	return res
}
