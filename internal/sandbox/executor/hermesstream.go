package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// hermesEvent is the part of one stream line this runner reads: Hermes's
// `--format stream-json` events (system, text, tool_use, tool_result, result)
// and the lines the run's script adds after them.
type hermesEvent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	CallID    string          `json:"tool_call_id"`
	Input     json.RawMessage `json:"input"`
	Output    string          `json:"output"`
	IsError   bool            `json:"is_error"`
	SessionID string          `json:"session_id"`
	ExitCode  int             `json:"exit_code"`
	Error     string          `json:"error"`
	Line      json.RawMessage `json:"line"`
	Rounds    int             `json:"rounds"`
}

// hermesRequest is one line of the plugin's record of a response
// ([hermesRequestEvent]): `input_tokens` without the cached part, as Hermes
// normalises a response (measured on 0.23.3: 67 + 40 cached of a 107-token
// prompt), and `output_tokens` with any reasoning in it.
type hermesRequest struct {
	TurnID        string `json:"turn_id"`
	Model         string `json:"model"`
	FinishReason  string `json:"finish_reason"`
	ContextLength int64  `json:"context_length"`
	ToolCalls     int    `json:"tool_calls"`
	Usage         struct {
		Input      int `json:"input_tokens"`
		Output     int `json:"output_tokens"`
		CacheRead  int `json:"cache_read_tokens"`
		CacheWrite int `json:"cache_write_tokens"`
	} `json:"usage"`
}

// hermesEvents decodes a Hermes run's stream ([Decoder]): the model's words and
// tool calls for the transcript, the result event's answer and status, and,
// once the run is over, the plugin's record and the round cap's line.
type hermesEvents struct {
	undecidedLines

	transcript transcriptLines
	// seq numbers the run's tool calls, and calls each started call's number.
	seq   int
	calls map[string]int
	// words is the model's text since its last call or answer, said as one
	// entry when the next call starts or the run ends.
	words strings.Builder
	// answer is the result event's text; done says a result event came.
	answer        string
	answerDropped int
	readBound     int
	exitCode      int
	failure       string
	done          bool
	sawLine       bool
	following     bool
	capped        int
	requests      []hermesRequest
}

// bound is [hermesEvents.readBound], or [sandbox.MaxFileBytes] where unset.
func (d *hermesEvents) bound() int {
	if d.readBound > 0 {
		return d.readBound
	}
	return sandbox.MaxFileBytes
}

// Entries implements [Decoder].
func (d *hermesEvents) Entries(end bool) []string {
	d.following = true
	d.answer = ""
	d.requests = nil
	if end {
		d.sayWords()
	}
	return d.transcript.take(end)
}

// Line implements [Decoder].
func (d *hermesEvents) Line(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	d.sawLine = true
	var ev hermesEvent
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Type {
	case "system":
		// A RELAUNCH after a discard starts its own stream ([hermesDiscard]):
		// the refused launch's ending is not the run's.
		d.done, d.answer, d.failure, d.exitCode = false, "", "", 0
	case "text":
		d.words.WriteString(ev.Text)
	case "tool_use":
		d.sayWords()
		d.seq++
		if d.calls == nil {
			d.calls = map[string]int{}
		}
		d.calls[ev.CallID] = d.seq
		for _, l := range activityline.Call(d.seq, ev.Name, compactInput(ev.Input)) {
			d.transcript.add(l)
		}
	case "tool_result":
		seq, known := d.calls[ev.CallID]
		if !known {
			d.seq++
			seq = d.seq
			for _, l := range activityline.Call(seq, ev.Name, "") {
				d.transcript.add(l)
			}
		}
		delete(d.calls, ev.CallID)
		if ev.IsError {
			d.transcript.add(activityline.Failed(seq, firstNonBlank(ev.Output, "failed"), activityline.Took{}))
			return
		}
		d.transcript.add(activityline.OK(seq, activityline.Took{}))
	case "result":
		d.sayWords()
		d.done, d.exitCode = true, ev.ExitCode
		d.failure = strings.TrimSpace(ev.Error)
		if !d.following {
			d.keepAnswer(strings.TrimSpace(ev.Text))
		}
		if d.failure != "" {
			d.transcript.add(activityline.Words(activityline.TagError, d.failure))
		}
	case hermesRequestEvent:
		var r hermesRequest
		if !d.following && json.Unmarshal(ev.Line, &r) == nil {
			d.requests = append(d.requests, r)
		}
	case hermesCappedEvent:
		d.capped = max(ev.Rounds, 1)
	}
}

// sayWords adds the model's words since its last call as one entry.
func (d *hermesEvents) sayWords() {
	if text := strings.TrimSpace(d.words.String()); text != "" {
		d.transcript.add(text)
	}
	d.words.Reset()
}

// keepAnswer holds text as the run's answer, within [hermesEvents.bound].
func (d *hermesEvents) keepAnswer(text string) {
	d.answer, d.answerDropped = text, 0
	if len(text) > d.bound() {
		kept, _ := settledHead(text[:d.bound()])
		d.answer = strings.TrimSpace(kept)
		d.answerDropped = len(text) - len(d.answer)
	}
}

// Skipped implements [Decoder].
func (d *hermesEvents) Skipped(line LongLine) {
	d.sawLine = true
	d.transcript.skip(line.Size, line.Bound)
	switch lineType(line) {
	case hermesRequestEvent:
		if !d.following {
			d.requests = append(d.requests, hermesRequest{})
		}
	case "":
		d.undecided++
	}
}

// runSpend implements [runSpender]: the responses the plugin recorded, every
// one this run's own, since the script empties the record before Hermes starts.
func (d *hermesEvents) runSpend() (runSpend, bool) {
	if len(d.requests) == 0 {
		return runSpend{}, false
	}
	var s runSpend
	for _, r := range d.requests {
		s.input += r.Usage.Input
		s.cacheRead += r.Usage.CacheRead
		s.cacheWrite += r.Usage.CacheWrite
		s.output += r.Usage.Output
	}
	return s, true
}

// reportedWindow implements [windowReporter]: the least window Hermes held the
// run's model to, as each response's record states it.
func (d *hermesEvents) reportedWindow() int64 {
	var least int64
	for _, r := range d.requests {
		if r.ContextLength > 0 && (least == 0 || r.ContextLength < least) {
			least = r.ContextLength
		}
	}
	return least
}

// roundCapped reports that the run ended on its round cap: Hermes said so as
// it exited 1 ([hermesCappedEvent]) AND the last turn's record holds the cap's
// tool rounds, each a response that called a tool. Either alone keeps the
// run's own cause.
func (d *hermesEvents) roundCapped() bool {
	if d.capped == 0 || len(d.requests) == 0 {
		return false
	}
	last, rounds := d.requests[len(d.requests)-1].TurnID, 0
	for _, r := range d.requests {
		if r.TurnID == last && r.ToolCalls > 0 {
			rounds++
		}
	}
	return rounds >= d.capped
}

// Ended implements [Decoder]: the stream-json `result` event, which the client
// prints as its run ends. Measured on 0.23.3: a finished run (exit 0), a
// refused one (exit 1, its error) and one whose client took SIGINT or whose
// gateway took SIGTERM (exit 130) each end on one; a client killed, by
// SIGTERM or SIGKILL, prints none. A relaunch after a discard is read on its
// own result ([hermesEvents.Line]'s `system`).
func (d *hermesEvents) Ended() bool { return d.done }

// Requests implements [Decoder]: the responses the run's plugin recorded,
// which the script appends after Hermes exits ([hermesRequestEvent]): its loop's
// alone (measured on 0.23.3: 31 for 31, 6 with a 429 and a 500 retried, and not
// the summary a run its round cap stopped asks for). NOT THE TURN CAP'S COUNT,
// which the session store holds, that summary included ([Hermes.storeRequests]).
func (d *hermesEvents) Requests() int { return len(d.requests) }

// InFlight implements [Decoder]: none, as Hermes's own --max-turns stops its
// loop before the next request (measured on 0.23.3: a cap of 3, 3 requests of
// its loop). NOT COUNTED: the summary request Hermes makes once its loop is
// capped, which the plugin's record leaves out (a fourth request there).
func (d *hermesEvents) InFlight() int { return 0 }

// Result implements [Decoder].
func (d *hermesEvents) Result() sandbox.Result {
	if !d.sawLine {
		return sandbox.Result{Error: "the harness produced no output"}
	}
	d.sayWords()
	res := sandbox.Result{Text: d.answer, DeliveredRefs: deliveredRefs(d.answer), Turns: len(d.requests)}
	if d.answerDropped > 0 {
		res.Text = withNote(res.Text, fmt.Sprintf("(%s more of the answer not kept: past the %s a run's "+
			"answer is read to)", humanSize(int64(d.answerDropped)), humanSize(int64(d.bound()))))
	}
	res.Success = d.done && d.exitCode == 0 && d.failure == "" && d.answer != "" && !d.roundCapped()
	res.Transcript, res.TranscriptElidedLines, res.TranscriptElidedBytes = d.transcript.result()
	if !res.Success {
		switch {
		case d.roundCapped():
			res.Error, res.RoundCapped = fmt.Sprintf("the run reached its round cap of %d tool rounds before "+
				"it finished", d.capped), true
		case !d.done:
			res.Error = "the harness ended before it gave a result"
		default:
			res.Error = firstNonBlank(d.failure, d.exitWords(), "the harness produced no answer")
		}
	}
	return res
}

// exitWords is a failed result's own text, said as its failure where Hermes
// put its reason there (a refused key or a spent plan is its answer's text),
// and its exit status otherwise.
func (d *hermesEvents) exitWords() string {
	if d.exitCode == 0 {
		return ""
	}
	if d.answer != "" {
		return d.answer
	}
	return fmt.Sprintf("hermes exited %d", d.exitCode)
}
