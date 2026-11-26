package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// codexRolloutEvent is the type of the lines a Codex run's script appends to
// its stream once the harness has exited: each one line of the session's
// rollout the run appended, of a kind the result reads (usage, model, window,
// limits), wrapped as `{"type":…,"line":<the rollout line>}` ([codexScript]).
// `exec --json` carries none of them, and its turn.completed usage is the
// thread's running total, not the run's (measured on 0.171.0).
const codexRolloutEvent = "crewlet.rollout"

// codexLimitsEvent is the type of the limit report a Codex run's result
// carries ([sandbox.Result.Limits]), which the codex profile's
// `limit_reports` reads: the newest token_count.rate_limits the run's rollout
// lines held, each window keyed by its length in minutes and its use a
// fraction. Composed here because Codex writes its windows by position
// (primary, secondary) and its use in percent. An item, as `exec --json`
// writes each of its events, so its type sits at item.type, the profile's one
// event_type_path.
const codexLimitsEvent = "crewlet.codex_rate_limits"

// codexRetryNotice opens the error event Codex writes as it retries a
// request ("Reconnecting... 2/5 (…)", measured on 0.171.0).
const codexRetryNotice = "Reconnecting..."

// codexEvent is the part of one `exec --json` line this runner reads.
type codexEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Message  string          `json:"message"`
	Error    *codexError     `json:"error"`
	Item     *codexItem      `json:"item"`
	Line     json.RawMessage `json:"line"`
	Rounds   int             `json:"rounds"`
}

// codexError is a turn.failed event's error.
type codexError struct {
	Message string `json:"message"`
}

// codexItem is an item.* event's item, loosely: a field retyped leaves the
// others read.
type codexItem struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Message   string          `json:"message"`
	Command   string          `json:"command"`
	Output    string          `json:"aggregated_output"`
	ExitCode  *int            `json:"exit_code"`
	Status    string          `json:"status"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Error     *codexError     `json:"error"`
	Changes   []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
}

// codexEvents decodes a Codex run's stream ([Decoder]): the items of its turn
// for the transcript, the last agent message as its answer, how its turn
// ended, and, once the run is over, the rollout lines its script appends.
type codexEvents struct {
	transcript transcriptLines
	// seq numbers the run's tool calls, and calls each started item's number.
	seq   int
	calls map[string]int
	// answer is the newest agent message: a turn's final answer is its last
	// (measured: a continued turn writes two).
	answer        string
	answerDropped int
	readBound     int
	threadID      string
	completed     bool
	failure       string
	lastError     string
	sawLine       bool
	following     bool
	// capped is the round cap the helper marked a turn interrupted at, 0
	// where none ([codexCappedEvent]); interrupted says the run's rollout lines
	// record a turn aborted as interrupted; helperLost, that the helper ended
	// while Codex worked ([codexHelperLostEvent]).
	capped      int
	interrupted bool
	helperLost  bool
	// responses is the model responses the run's rollout lines record
	// ([codexEvents.Requests]), counted as each line is fed, whatever the
	// rollout's bound kept of it.
	responses int
	undecidedLines
	// rollout is the rollout lines the script appended, each with its line
	// break, held to [codexEvents.bound]; rolloutDropped counts what was not.
	rollout        bytes.Buffer
	rolloutDropped int
	// read is the rollout lines as read, once the result first asks
	// ([codexEvents.figures]); every line has been fed by then.
	read *codexRead
}

// codexRead is what [codexEvents.figures] read.
type codexRead struct {
	rollout codexRollout
	ok      bool
}

// bound is [codexEvents.readBound], or [sandbox.MaxFileBytes] where unset.
func (d *codexEvents) bound() int {
	if d.readBound > 0 {
		return d.readBound
	}
	return sandbox.MaxFileBytes
}

// Entries implements [Decoder].
func (d *codexEvents) Entries(end bool) []string {
	d.following = true
	d.answer = ""
	d.rollout.Reset()
	return d.transcript.take(end)
}

// Line implements [Decoder].
func (d *codexEvents) Line(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	d.sawLine = true
	var ev codexEvent
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Type {
	case "thread.started":
		d.threadID = ev.ThreadID
	case "item.started":
		d.started(ev.Item)
	case "item.completed":
		d.itemCompleted(ev.Item)
	case "turn.completed":
		d.completed = true
	case "turn.failed":
		if ev.Error != nil {
			d.failure = firstNonBlank(ev.Error.Message, "the turn failed")
		} else {
			d.failure = "the turn failed"
		}
		d.transcript.add(activityline.Words(activityline.TagError, d.failure))
	case "error":
		// A retry notice, or the fatal error turn.failed repeats: said, and
		// read as the failure only where the turn says none and never off a
		// retry, which repeats the cause mid-way (a 401 is retried five times,
		// measured on 0.171.0).
		if !strings.HasPrefix(ev.Message, codexRetryNotice) {
			d.lastError = ev.Message
		}
		d.transcript.add(activityline.Words(activityline.TagNote, ev.Message))
	case codexRolloutEvent:
		d.keepRollout(ev.Line)
	case codexCappedEvent:
		d.capped = max(ev.Rounds, 1)
	case codexHelperLostEvent:
		d.helperLost = true
		d.transcript.add(activityline.Words(activityline.TagError, codexHelperLostWords))
	}
}

// started numbers a tool item as it starts and says its call, so a live
// reading shows a long command while it runs.
func (d *codexEvents) started(item *codexItem) {
	if item == nil || !codexToolItem(item.Type) {
		return
	}
	d.seq++
	if d.calls == nil {
		d.calls = map[string]int{}
	}
	d.calls[item.ID] = d.seq
	for _, l := range activityline.Call(d.seq, codexToolName(item), codexSubject(item)) {
		d.transcript.add(l)
	}
}

// itemCompleted reads a finished item: an agent message's words, a tool's
// result (its call too where its start was not seen), a warning.
func (d *codexEvents) itemCompleted(item *codexItem) {
	if item == nil {
		return
	}
	switch {
	case item.Type == "agent_message":
		text := strings.TrimSpace(item.Text)
		if text == "" {
			return
		}
		if !d.following {
			d.keepAnswer(text)
		}
		d.transcript.add(text)
	case item.Type == "error":
		// A WARNING, never a failure (measured: a model's fallback metadata,
		// a reroute): the run goes on.
		d.transcript.add(activityline.Words(activityline.TagNote, item.Message))
	case codexToolItem(item.Type):
		seq, known := d.calls[item.ID]
		if !known {
			d.seq++
			seq = d.seq
			for _, l := range activityline.Call(seq, codexToolName(item), codexSubject(item)) {
				d.transcript.add(l)
			}
		}
		delete(d.calls, item.ID)
		if why := codexItemFailure(item); why != "" {
			d.transcript.add(activityline.Failed(seq, why, activityline.Took{}))
			return
		}
		d.transcript.add(activityline.OK(seq, activityline.Took{}))
	}
}

// codexToolItem reports whether an item type is a call the model made.
func codexToolItem(kind string) bool {
	switch kind {
	case "command_execution", "mcp_tool_call", "file_change", "web_search":
		return true
	}
	return false
}

// codexToolName is a tool item's tool, as Codex's hooks name it: the shell,
// a patch, or `mcp__<server>__<tool>`.
func codexToolName(item *codexItem) string {
	switch item.Type {
	case "command_execution":
		return "shell"
	case "file_change":
		return "apply_patch"
	case "mcp_tool_call":
		return "mcp__" + item.Server + "__" + item.Tool
	}
	return item.Type
}

// codexSubject is what a tool item worked on: its command, the paths its
// patch changed, or its arguments compacted.
func codexSubject(item *codexItem) string {
	switch item.Type {
	case "command_execution":
		return item.Command
	case "file_change":
		paths := make([]string, 0, len(item.Changes))
		for _, c := range item.Changes {
			paths = append(paths, c.Path)
		}
		return strings.Join(paths, " ")
	}
	return compactInput(item.Arguments)
}

// codexItemFailure is the first line of why a finished tool item failed, ""
// for one that did not.
func codexItemFailure(item *codexItem) string {
	if item.Error != nil && item.Error.Message != "" {
		return item.Error.Message
	}
	if item.Status != "failed" {
		return ""
	}
	if item.ExitCode != nil {
		return firstNonBlank(item.Output, "exit "+strconv.Itoa(*item.ExitCode))
	}
	return "failed"
}

// keepAnswer holds text as the run's answer, within [codexEvents.bound].
func (d *codexEvents) keepAnswer(text string) {
	d.answer, d.answerDropped = text, 0
	if len(text) > d.bound() {
		kept, _ := settledHead(text[:d.bound()])
		d.answer = strings.TrimSpace(kept)
		d.answerDropped = len(text) - len(d.answer)
	}
}

// keepRollout holds one appended rollout line for the result's figures,
// within [codexEvents.bound]; a live reading holds none.
func (d *codexEvents) keepRollout(line json.RawMessage) {
	if d.following || len(line) == 0 {
		return
	}
	var aborted struct {
		Type    string `json:"type"`
		Payload struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &aborted) == nil && aborted.Type == "event_msg" &&
		aborted.Payload.Type == "turn_aborted" && aborted.Payload.Reason == "interrupted" {
		d.interrupted = true
	}
	if codexResponseLine(line) {
		d.responses++
	}
	if d.rolloutDropped > 0 || d.rollout.Len()+len(line)+1 > d.bound() {
		d.rolloutDropped += len(line) + 1
		return
	}
	d.rollout.Write(line)
	d.rollout.WriteByte('\n')
}

// Skipped implements [Decoder].
func (d *codexEvents) Skipped(line LongLine) {
	d.sawLine = true
	d.transcript.skip(line.Size, line.Bound)
	switch lineType(line) {
	case codexRolloutEvent:
		response, undecided := codexRolloutSkipped(line)
		if response {
			d.responses++
		}
		if undecided {
			d.undecided++
		}
	case "":
		d.undecided++
	}
}

// codexRolloutSkipped is whether a rollout line the script appended
// ([codexRolloutEvent]), past the bound, is a response, or may be one the count
// cannot read. A ROLLOUT LINE NAMES ITS KIND AT ITS HEAD; a response's
// token_count is numbers alone, so its usage is in one window or the other.
func codexRolloutSkipped(line LongLine) (response, undecided bool) {
	switch {
	case !inEither(line, `"token_count"`) && !bytes.Contains(line.Head, []byte(`"payload"`)):
		return false, true
	case inEither(line, `"token_count"`) && inEither(line, `"last_token_usage":{`):
		return true, false
	}
	return false, false
}

// figures is what the run's appended rollout lines say, and false where the
// script appended none or they do not read.
func (d *codexEvents) figures() (codexRollout, bool) {
	if d.read == nil {
		d.read = &codexRead{}
		if d.rollout.Len() > 0 {
			read, err := readCodexRollout(bytes.NewReader(d.rollout.Bytes()))
			d.read.rollout, d.read.ok = read, err == nil
		}
	}
	return d.read.rollout, d.read.ok
}

// runSpend implements [runSpender]: the turns the run's own rollout lines
// record, every one appended while it ran ([codexScript]).
func (d *codexEvents) runSpend() (runSpend, bool) {
	read, ok := d.figures()
	if !ok || len(read.Turns()) == 0 {
		return runSpend{}, false
	}
	return read.Usage().spend(), true
}

// reportedWindow implements [windowReporter]: the raw window Codex held the
// run's model to, from the 95% its token_count states ([codexRawWindow]), in
// the units the window is pinned and judged in ([codexWindowTokens]).
func (d *codexEvents) reportedWindow() int64 {
	read, ok := d.figures()
	if !ok {
		return 0
	}
	return codexRawWindow(read.ContextWindow())
}

// limitReport implements [limitReporter]: the run's newest plan limits as a
// [codexLimitsEvent] line, "" where its rollout lines carried none (an API
// key's run).
func (d *codexEvents) limitReport() string {
	read, ok := d.figures()
	if !ok {
		return ""
	}
	limits, ok := read.Limits()
	if !ok {
		return ""
	}
	return codexLimitsLine(limits)
}

// codexLimitsLine is limits as a [codexLimitsEvent] item: `reached` where a
// window is used up, naming it and its reset as `window` and `resets_at`; each
// window by its minutes, its use as a fraction and its reset in epoch seconds.
func codexLimitsLine(limits codexLimits) string {
	type window struct {
		Used     float64 `json:"used"`
		ResetsAt int64   `json:"resets_at,omitempty"`
	}
	type report struct {
		Type     string            `json:"type"`
		State    string            `json:"state"`
		Window   string            `json:"window,omitempty"`
		ResetsAt int64             `json:"resets_at,omitempty"`
		Windows  map[string]window `json:"windows"`
	}
	line := report{Type: codexLimitsEvent, State: "ok", Windows: map[string]window{}}
	for _, w := range []*codexLimitWindow{limits.Primary, limits.Secondary} {
		if w == nil || w.Window <= 0 || math.IsNaN(w.UsedPercent) {
			continue
		}
		key := strconv.FormatInt(int64(w.Window.Minutes()), 10)
		entry := window{Used: w.UsedPercent / 100}
		if !w.ResetsAt.IsZero() {
			entry.ResetsAt = w.ResetsAt.Unix()
		}
		line.Windows[key] = entry
		if w.UsedPercent >= 100 {
			line.State, line.Window, line.ResetsAt = "reached", key, entry.ResetsAt
		}
	}
	raw, err := json.Marshal(struct {
		Item report `json:"item"`
	}{line})
	if err != nil {
		return ""
	}
	return string(raw)
}

// Ended implements [Decoder]: the turn's end, `turn.completed` or
// `turn.failed`.
func (d *codexEvents) Ended() bool { return d.completed || d.failure != "" }

// Requests implements [Decoder]: the model responses the rollout lines the
// script appends after Codex exits record ([codexResponseLine]). exec --json's
// own stdout marks no response, so a stream cut before those lines counts none,
// and a run with no rollout lines never ended as a success anyway.
func (d *codexEvents) Requests() int { return d.responses }

// InFlight implements [Decoder]: one. The note helper interrupts the turn
// once it hears the cap-th response, and Codex may have run that response's
// call and made the next request by then: measured on 0.171.0 with the per-run
// helper, a cap of 3 stopped after 3 requests in 14 runs of 20 and after 4 in 6.
func (d *codexEvents) InFlight() int { return 1 }

// codexResponseLine reports whether line, a rollout line, records one model
// response: a token_count stating the response's own usage
// (`info.last_token_usage`), which Codex writes once each response completes; a
// token_count carrying only rate limits states none. Measured on 0.171.0
// against a stand-in: 31 lines for 31 responses, and none for a 429 or a 500
// it retried.
func codexResponseLine(line []byte) bool {
	var ev struct {
		Type    string `json:"type"`
		Payload struct {
			Type string `json:"type"`
			Info *struct {
				Last json.RawMessage `json:"last_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	return json.Unmarshal(line, &ev) == nil && ev.Type == "event_msg" && ev.Payload.Type == "token_count" &&
		ev.Payload.Info != nil && wireObject(ev.Payload.Info.Last)
}

// Result implements [Decoder].
func (d *codexEvents) Result() sandbox.Result {
	if !d.sawLine {
		return sandbox.Result{Error: "the harness produced no output"}
	}
	res := sandbox.Result{Text: d.answer, SessionID: d.threadID, DeliveredRefs: deliveredRefs(d.answer)}
	if d.answerDropped > 0 {
		res.Text = withNote(res.Text, fmt.Sprintf("(%s more of the answer not kept: past the %s a run's "+
			"answer is read to)", humanSize(int64(d.answerDropped)), humanSize(int64(d.bound()))))
	}
	res.Success = d.completed && d.failure == "" && d.answer != "" && !d.roundCapped() && !d.helperLost
	res.Transcript, res.TranscriptElidedLines, res.TranscriptElidedBytes = d.transcript.result()
	if !res.Success {
		res.Error = firstNonBlank(d.failure, d.lastError, "the harness produced no answer")
		switch {
		case d.roundCapped():
			res.Error, res.RoundCapped = d.cappedWords(), true
		case d.helperLost && d.failure == "":
			res.Error = codexHelperLostWords
		}
	}
	return res
}

// codexHelperLostWords is what a run whose helper ended under it says.
const codexHelperLostWords = "the note helper ended while Codex worked, so the run's turn was interrupted " +
	"rather than left without its round cap, notes or skill loads"

// roundCapped reports that the run ended on its round cap: the helper marked
// it, and the rollout records the turn interrupted. An interrupt with no mark
// keeps its own cause.
func (d *codexEvents) roundCapped() bool { return d.capped > 0 && d.interrupted }

// cappedWords is what a run ended at its round cap says of its end.
func (d *codexEvents) cappedWords() string {
	return fmt.Sprintf("the run reached its round cap of %d model responses before it finished", d.capped)
}
