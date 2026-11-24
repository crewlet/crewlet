package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// kimiFrame is the part of one line of a Kimi run's stream that this runner
// reads: a KAP event the driver kept (`turn.*`, `item.*`, `context`),
// one of the driver's own (`crewlet.end`, `crewlet.refused`), or, after the
// script's mark, a
// `usage.record` of the session's wire.jsonl. DECODED LOOSELY: a field a
// release retyped reads as zero, and the rest still read.
type kimiFrame struct {
	Type   string          `json:"type"`
	TurnID string          `json:"turn_id"`
	Turn   kimiTurn        `json:"turn"`
	Step   kimiStep        `json:"step"`
	Item   kimiItem        `json:"item"`
	Max    int64           `json:"max_context_tokens"`
	Asked  bool            `json:"asked"`
	Code   json.RawMessage `json:"code"`
	// Why is a `crewlet.refused` line's: why the driver's guard ended the run.
	Why string `json:"why"`

	// A wire line's, after the script's mark: whose it is, a usage record's
	// figures, and a turn's error.
	AgentID string     `json:"agentId"`
	Usage   kimiRecord `json:"usage"`
	Error   *kimiError `json:"error"`
}

// kimiTurn is a KAP turn: its id, its steps, and how it ended.
type kimiTurn struct {
	TurnID    string     `json:"turn_id"`
	Steps     []kimiStep `json:"steps"`
	Outcome   string     `json:"outcome"`
	EndReason string     `json:"end_reason"`
	Error     *kimiError `json:"error"`
}

// kimiStep is one step of a turn: one model request.
type kimiStep struct {
	Index        int    `json:"index"`
	Outcome      string `json:"outcome"`
	FinishReason string `json:"finish_reason"`
}

// kimiError is a turn's error: its code (`provider.rate_limit`,
// `loop.max_steps_exceeded`, …) and words.
type kimiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// kimiItem is one item of a turn: a user or assistant message, or a tool call
// with its input, output or error.
type kimiItem struct {
	Type      string          `json:"type"`
	ItemID    string          `json:"item_id"`
	StepIndex int             `json:"step_index"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Outcome   string          `json:"outcome"`
	Error     json.RawMessage `json:"error"`
}

// kimiRecord is one request's usage in the wire log: the prompt not served
// from the cache (inputOther), the cache's reads and writes, and the reply.
type kimiRecord struct {
	InputOther         int `json:"inputOther"`
	Output             int `json:"output"`
	InputCacheRead     int `json:"inputCacheRead"`
	InputCacheCreation int `json:"inputCacheCreation"`
}

// The turn errors read as a spent plan or rate limit and as the turn cap
// (2.7.2's error codes, measured).
const (
	kimiRateLimit   = "provider.rate_limit"
	kimiMaxSteps    = "loop.max_steps_exceeded"
	kimiMaxStepsEnd = "max_steps"
)

// kimiOverflow is the opening of a turn's error words where the conversation is
// past its model's context: a provider's 400 "exceeded model token limit" is
// APIContextOverflowError, the compaction Kimi then tries fails the same way,
// and the turn ends on `compaction.failed` with "APIContextOverflowError: <the
// provider's words>" (measured on 2.7.2). The code names no reason of its own.
const kimiOverflow = "APIContextOverflowError"

// decodeKimiFrame decodes one line, false where it is no JSON object of a type.
func decodeKimiFrame(line []byte) (kimiFrame, bool) {
	var f kimiFrame
	if err := json.Unmarshal(line, &f); err != nil {
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &typed) != nil || typed.Type == "" {
			return kimiFrame{}, false
		}
		return kimiFrame{Type: typed.Type}, true
	}
	return f, f.Type != ""
}

// kimiEvents decodes a Kimi run's stream ([Decoder]): its answer, the last
// assistant message's text; how it ended, off its last turn and the driver's
// `crewlet.end`; its calls for the transcript; its model requests, a turn's
// steps; its window; and, after the script's mark ([kimiUsageMark]), the
// run's own usage.
type kimiEvents struct {
	sawLine, sawEvent bool

	// answer is the text of the last step's assistant messages, step the
	// step it is of.
	answer, step string

	// last is the turn that ended last, lastLine its line; turns how many
	// turns ended.
	last     kimiTurn
	lastLine string
	turns    int

	// limit is the last rate limit's line in the session's record
	// ([limitReporter]), failed the last failed turn's.
	limit, failed string

	// ended is whether the driver's `crewlet.end` was read, asked whether it
	// was asked to end, and code its exit code.
	ended, asked bool
	code         int

	// refused is why the driver's guard ended the run (`crewlet.refused`),
	// which fails it whatever its turns answered: a guard ending a run mid-turn
	// leaves the turn's earlier text as its answer.
	refused string

	// steps is the most steps each turn was seen to make, a model request
	// each, by turn.
	steps map[string]int

	// calls is each running call's place in the transcript.
	calls map[string]int
	seq   int

	// window is the least context window a `context` frame stated.
	window int64

	// marked is whether the script's usage mark was read, and spent what the
	// records after it state.
	marked bool
	spent  kimiRecord

	transcript transcriptLines
	following  bool
}

var (
	_ runSpender     = (*kimiEvents)(nil)
	_ limitReporter  = (*kimiEvents)(nil)
	_ windowReporter = (*kimiEvents)(nil)
)

// Entries implements [Decoder].
func (d *kimiEvents) Entries(end bool) []string {
	d.following = true
	d.answer = ""
	return d.transcript.take(end)
}

// Skipped implements [Decoder].
func (d *kimiEvents) Skipped(n int64, bound int) { d.transcript.skip(n, bound) }

// Line implements [Decoder].
func (d *kimiEvents) Line(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	d.sawLine = true
	f, ok := decodeKimiFrame(line)
	if !ok {
		return
	}
	d.sawEvent = true
	switch f.Type {
	case "turn.updated":
		d.stepped(f.TurnID, f.Step)
	case "turn.ended":
		if d.marked {
			d.recordEnded(f, line)
			return
		}
		for _, s := range f.Turn.Steps {
			d.stepped(f.Turn.TurnID, s)
		}
		d.turnEnded(f.Turn, line)
	case "item.started":
		if f.Item.Type == "tool_call" {
			d.started(f.Item)
		}
	case "item.ended":
		d.item(f.TurnID, f.Item)
	case "context":
		if f.Max > 0 && (d.window == 0 || f.Max < d.window) {
			d.window = f.Max
		}
	case "crewlet.end":
		d.ended, d.asked, d.code = true, f.Asked, int(looseInt(f.Code))
	case "crewlet.refused":
		d.refused = firstNonBlank(d.refused, f.Why)
	case kimiUsageMark:
		d.marked = true
	case "usage.record":
		// ONLY AFTER THE MARK: the script adds the run's own records there.
		if d.marked {
			u := f.Usage
			d.spent.InputOther += u.InputOther
			d.spent.Output += u.Output
			d.spent.InputCacheRead += u.InputCacheRead
			d.spent.InputCacheCreation += u.InputCacheCreation
		}
	}
}

// stepped notes that turn made at least s's index of steps, its steps
// numbered from one, each a model request.
func (d *kimiEvents) stepped(turn string, s kimiStep) {
	if s.Index <= d.steps[turn] {
		return
	}
	if d.steps == nil {
		d.steps = map[string]int{}
	}
	d.steps[turn] = s.Index
}

// requests is how many model requests the run's turns made.
func (d *kimiEvents) requests() int {
	n := 0
	for _, steps := range d.steps {
		n += steps
	}
	return n
}

// turnEnded notes a turn's end: the last one read is how the run ended. A turn
// ended again (KAP corrects an ended turn by sending it once more) replaces
// itself.
func (d *kimiEvents) turnEnded(t kimiTurn, line []byte) {
	if t.TurnID != d.last.TurnID {
		d.turns++
	}
	d.last, d.lastLine = t, boundedEvent(line)
	if t.Error != nil && t.Outcome == "failed" {
		d.transcript.add(activityline.Words(activityline.TagError, firstNonBlank(t.Error.Message, t.Error.Code)))
	}
}

// recordEnded notes a main-agent turn's end the script added from the
// session's record (`turn.ended`, its error's code at the top): the line a
// profile's limit and sign-in events read (the kimi-code profile's `in:
// record`), as the text phases read it off the same file.
func (d *kimiEvents) recordEnded(f kimiFrame, line []byte) {
	if f.AgentID != "main" || f.Error == nil {
		return
	}
	d.failed = boundedEvent(line)
	if f.Error.Code == kimiRateLimit {
		d.limit = d.failed
	}
}

// item reads one ended item: a message's words, a call's end.
func (d *kimiEvents) item(turn string, it kimiItem) {
	switch it.Type {
	case "assistant_message":
		text := strings.TrimSpace(it.Text)
		if text == "" {
			return
		}
		if !d.following {
			step := fmt.Sprintf("%s:%d", turn, it.StepIndex)
			if step != d.step || d.answer == "" {
				d.answer, d.step = it.Text, step
			} else {
				d.answer += "\n\n" + it.Text
			}
		}
		d.transcript.add(text)
	case "user_message":
		if strings.HasPrefix(it.ItemID, kimiNotePrefix) {
			d.transcript.add(activityline.Words(activityline.TagSteer, firstLine(it.Text, transcriptDetailLimit)))
		}
	case "tool_call":
		d.completed(it)
	}
}

// started notes a call's start and adds its line.
func (d *kimiEvents) started(it kimiItem) {
	if _, seen := d.calls[it.ItemID]; seen {
		return
	}
	d.seq++
	if d.calls == nil {
		d.calls = map[string]int{}
	}
	if len(d.calls) < maxPendingCalls {
		d.calls[it.ItemID] = d.seq
	}
	for _, l := range activityline.Call(d.seq, firstNonBlank(it.Name, "tool"), kimiSubject(it.Input)) {
		d.transcript.add(l)
	}
}

// completed adds a call's end, its start's line first where none was read.
func (d *kimiEvents) completed(it kimiItem) {
	seq, ok := d.calls[it.ItemID]
	if !ok {
		d.started(it)
		seq = d.seq
	}
	delete(d.calls, it.ItemID)
	if it.Outcome != "failed" {
		d.transcript.add(activityline.OK(seq, activityline.Took{}))
		return
	}
	var why string
	if json.Unmarshal(it.Error, &why) != nil {
		why = errorText(it.Error)
	}
	d.transcript.add(activityline.Failed(seq, firstNonBlank(why, "failed"), activityline.Took{}))
}

// kimiSubject is a call's input as a transcript names it: the field that says
// what Kimi's tool acts on (Bash's command, Read's path, Grep's pattern,
// FetchURL's url, Agent's description), else the input as compact JSON.
func kimiSubject(raw json.RawMessage) string {
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
// script's mark, every request of every agent, compactions included. Stated
// wherever the mark was read, a run that made no request spending nothing.
func (d *kimiEvents) runSpend() (runSpend, bool) {
	if !d.marked {
		return runSpend{}, false
	}
	u := d.spent
	return runSpend{input: u.InputOther, cacheRead: u.InputCacheRead, cacheWrite: u.InputCacheCreation,
		output: u.Output}, true
}

// limitReport is the run's last rate limit's line ([limitReporter]): a turn's
// end in the session's record with error code `provider.rate_limit`, whose
// words are the service's (a 429's body; not measured against Moonshot's
// service).
func (d *kimiEvents) limitReport() string { return d.limit }

// reportedWindow is the least context window the session stated
// ([windowReporter]): `max_context_tokens` of its `context` frames, the
// model alias's maxContextSize.
func (d *kimiEvents) reportedWindow() int64 { return d.window }

// Ended implements [Decoder]: a turn Kimi said ended, and the driver's
// `crewlet.end`, printed once the session it drives went idle.
func (d *kimiEvents) Ended() bool { return d.ended && d.turns > 0 }

// Requests implements [Decoder]: the steps of every turn, one model request a
// step ([kimiEvents.requests]).
func (d *kimiEvents) Requests() int { return d.requests() }

// Result implements [Decoder]: the answer, and how the run ended, off its last
// turn and the driver's end.
func (d *kimiEvents) Result() sandbox.Result {
	if !d.sawLine {
		return sandbox.Result{Error: "the harness produced no output"}
	}
	if !d.sawEvent {
		return sandbox.Result{Error: "the harness's output is not its event stream, so it never reported how its run ended"}
	}
	text := strings.TrimSpace(d.answer)
	res := sandbox.Result{Text: text, DeliveredRefs: deliveredRefs(text), Turns: d.requests()}
	res.Transcript, res.TranscriptElidedLines, res.TranscriptElidedBytes = d.transcript.result()
	if spent, stated := d.runSpend(); stated {
		res = spent.on(res)
	}
	last := d.last
	switch {
	case d.refused != "":
		res.Error = d.refused
	case last.Error != nil && (last.Error.Code == kimiMaxSteps || last.EndReason == kimiMaxStepsEnd):
		res.Error = firstNonBlank(last.Error.Message, "the run reached its turn cap")
		res.RoundCapped = true
	case last.Outcome == "failed":
		res.Error = "the run's last turn failed"
		if last.Error != nil {
			res.Error = firstNonBlank(last.Error.Message, last.Error.Code, res.Error)
			res.ContextExhausted = strings.HasPrefix(last.Error.Message, kimiOverflow)
		}
		res.Event = firstNonBlank(d.failed, d.lastLine)
	case !d.ended:
		res.Error = "the harness stopped before it reported how its run ended"
	case d.asked:
		res.Error = fmt.Sprintf("the run was ended on request, and exited with status %d", d.code)
	case d.turns == 0:
		res.Error = "the harness ended its run before any turn of it ended"
	case last.Outcome != "completed":
		res.Error = fmt.Sprintf("the run's last turn ended %s", firstNonBlank(last.Outcome, "without an outcome"))
	case d.code != 0:
		res.Error = fmt.Sprintf("the harness exited with status %d", d.code)
	case text == "":
		res.Error = "the harness produced no answer"
	default:
		res.Success = true
	}
	res.Limits = d.limit
	return res
}
