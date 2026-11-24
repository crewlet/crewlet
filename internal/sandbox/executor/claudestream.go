package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// Claude Code's stream-json, read into a transcript: one JSON object a line,
// `system`, `assistant` (`text`, `thinking`, `tool_use` blocks), `user`
// (`tool_result` blocks, `is_error` on a failure) and, last, `result` (how the
// run ended, its usage and cost). A subagent's lines carry its
// `parent_tool_use_id` and are activity like any other; the main thread's
// newest request is measured off the same lines ([claudeEvents.measure]).
//
// The transcript is OpenCode's shape ([activityline]), each call timed by the lines'
// own timestamps. Never a result's body or thinking; unknown types are skipped, and
// one system line shows, a hook's note ([claudeSteerLine]).

// claudeEventWire is the part of one stream line this decoder reads: the
// result's keys ([claudeResultWire]), a system line's text and a message, each
// by its exact name and raw, in the line's own bytes ([wireAlias]), read loosely
// by type only where the line's type asks ([claudeEvents.Line]). Exact names, as
// Claude Code's own JSON.parse reads them ([threadDecoding]) and as a message's
// blocks are ([eachClaudeBlock]): encoding/json would take a key differing only
// in case for this one, and merge a message written twice, where a transcript
// reads the last.
type claudeEventWire struct {
	claudeResultWire
	// Content is a system line's text, read only on an informational line
	// ([claudeSteerLine]); raw, because an unknown subtype may carry anything.
	Content json.RawMessage `json:"content"`
	// Message is an assistant or a user line's message ([claudeMessageWire]).
	Message json.RawMessage `json:"message"`
	// Timestamp is when the harness wrote an assistant or a user line: a
	// call's line and its result's time the call ([claudeEvents.took]).
	Timestamp json.RawMessage `json:"timestamp"`
	// ParentToolUseID is raw AS WRITTEN: null on a main-thread line, the
	// spawning call's id on a subagent's, and nil where the key is absent,
	// which is no main thread's ([claudeEvents.measure]).
	ParentToolUseID json.RawMessage `json:"parent_tool_use_id"`
}

// claudeMessageWire is one line's message, each key raw: its content (blocks,
// or a plain string on a prompt such as a subagent's first message), and an
// assistant message's id and usage. The stream repeats both on every line of
// one message (a line per block), so a usage is counted once, by its id.
type claudeMessageWire struct {
	Content json.RawMessage `json:"content"`
	ID      json.RawMessage `json:"id"`
	Usage   json.RawMessage `json:"usage"`
}

// claudeMessageOf is the message raw holds, its keys raw's own bytes
// ([wireAlias]); the zero message where raw is no object.
func claudeMessageOf(raw json.RawMessage) claudeMessageWire {
	var m claudeMessageWire
	if !wireObject(raw) || wireAlias(raw, &m) != nil {
		return claudeMessageWire{}
	}
	return m
}

// claudeUsage is one API call's tokens as the stream reports them: the
// uncached prompt, the cache's reads and writes, and the reply.
type claudeUsage struct {
	Input       int
	Output      int
	CacheRead   int
	CacheCreate int
}

// claudeUsageWire is a message's usage as its decode leaves it, each count
// raw ([looseUsage]).
type claudeUsageWire struct {
	Input       json.RawMessage `json:"input_tokens"`
	Output      json.RawMessage `json:"output_tokens"`
	CacheRead   json.RawMessage `json:"cache_read_input_tokens"`
	CacheCreate json.RawMessage `json:"cache_creation_input_tokens"`
}

// looseUsage is the usage raw holds, DECODED LOOSELY: a count that is no whole
// number reads as 0, the others as written, and a usage that is no object as
// no tokens.
func looseUsage(raw json.RawMessage) claudeUsage {
	var w claudeUsageWire
	if !wireObject(raw) || wireDecode(raw, &w) != nil {
		return claudeUsage{}
	}
	return claudeUsage{
		Input: int(looseInt(w.Input)), Output: int(looseInt(w.Output)),
		CacheRead: int(looseInt(w.CacheRead)), CacheCreate: int(looseInt(w.CacheCreate)),
	}
}

// total is every token of the call, the whole prompt and the reply: the
// convention every engine meter charges (llm.Completion.TotalTokens).
func (u claudeUsage) total() int { return u.Input + u.Output + u.CacheRead + u.CacheCreate }

// claudeBlock is one content block, as the stream carries it and the session's
// record keeps it ([ReadTranscript]): ONE SHAPE, because they are one format
// and a second decoder is a second answer to what a block holds. Read a block
// at a time ([eachClaudeBlock]).
type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// A tool_use block. Input is the call's input as written: the live
	// transcript names its subject ([claudeToolSubject]), a session's shows it whole.
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// A tool_result block. Content is a string or blocks ([claudeResultText]):
	// the live transcript reads it for a failed call alone, a session's for all.
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// claudeBlockWire is each key a [claudeBlock] holds, by its exact name and raw,
// in the element's own bytes ([wireAlias]), read into the block by
// [claudeBlockWire.block].
type claudeBlockWire struct {
	Type      json.RawMessage `json:"type"`
	Text      json.RawMessage `json:"text"`
	ID        json.RawMessage `json:"id"`
	Name      json.RawMessage `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID json.RawMessage `json:"tool_use_id"`
	IsError   json.RawMessage `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// block is the block w holds, DECODED LOOSELY: a key of the wrong kind reads as
// its field's zero value, the rest as written.
func (w claudeBlockWire) block() claudeBlock {
	return claudeBlock{
		Type: looseString(w.Type), Text: looseString(w.Text),
		ID: looseString(w.ID), Name: looseString(w.Name), Input: w.Input,
		ToolUseID: looseString(w.ToolUseID), IsError: looseTrue(w.IsError), Content: w.Content,
	}
}

// eachClaudeBlock hands each block of content to each, in order, valid only
// while each runs ([wireAlias]), and returns the first error each returns,
// reading no block after it. Content that is no list (a prompt's string) has no
// blocks, and one that stops decoding part way none after the fault.
//
// A BLOCK AT A TIME ([eachElement]), by exact key names ([threadDecoding]: as
// Claude Code's JSON.parse, invalid UTF-8 or a lone surrogate as U+FFFD) and
// loosely by type ([claudeBlockWire.block]), never the whole list first: that
// holds 136 bytes per element before reading any, over a GiB at a 32 MiB line
// ([maxLineBytes]). An element that is no object is the zero block and still
// handed over, so a reader that checks its caller between blocks does so
// between every two.
func eachClaudeBlock(content json.RawMessage, each func(claudeBlock) error) error {
	var (
		wire    claudeBlockWire
		stopped error
	)
	// The walk's own error is no reader's: the blocks before it were read.
	_ = eachElement(content, func(element json.RawMessage, _ int) error {
		wire = claudeBlockWire{}
		if wireObject(element) && wireAlias(element, &wire) != nil {
			wire = claudeBlockWire{}
		}
		stopped = each(wire.block())
		return stopped
	})
	return stopped
}

// looseString is raw where it is a JSON string, else "" (absent, null, or a
// key a release retyped). The kind is read off the first byte, so a value of
// another kind is never decoded.
func looseString(raw json.RawMessage) string {
	if len(raw) == 0 || raw[0] != '"' {
		return ""
	}
	var s string
	if wireDecode(raw, &s) != nil {
		return ""
	}
	return s
}

// looseTrue reports whether raw is JSON's true; anything else is false.
func looseTrue(raw json.RawMessage) bool { return string(raw) == "true" }

// looseInt is raw where it is a JSON number that is a whole int64, else 0
// (absent, another kind, a fraction or out of range). The kind is read off the
// first byte, so a value of another kind is never decoded.
func looseInt(raw json.RawMessage) int64 {
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0
	}
	var n int64
	if wireDecode(raw, &n) != nil {
		return 0
	}
	return n
}

// looseFloat is raw where it is a JSON number a float64 holds, else 0 (absent,
// another kind or out of range), read off the first byte as [looseInt] is.
func looseFloat(raw json.RawMessage) float64 {
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0
	}
	var f float64
	if wireDecode(raw, &f) != nil {
		return 0
	}
	return f
}

// claudeToolInput is the field each harness tool takes its subject in — Bash
// its command, Read, Edit and Write their file, NotebookEdit its notebook, Grep
// and Glob their pattern, WebFetch its URL, WebSearch its query, Task its
// description — each by its exact name and raw, read loosely ([claudeToolSubject]).
type claudeToolInput struct {
	Command      json.RawMessage `json:"command"`
	FilePath     json.RawMessage `json:"file_path"`
	NotebookPath json.RawMessage `json:"notebook_path"`
	Pattern      json.RawMessage `json:"pattern"`
	URL          json.RawMessage `json:"url"`
	Query        json.RawMessage `json:"query"`
	Path         json.RawMessage `json:"path"`
	Description  json.RawMessage `json:"description"`
}

// claudeEvents decodes one stream into a transcript, read whole or followed
// across reads ([Decoder]). The result itself is read apart
// ([ClaudeCode.Parse]), from the stream's last line.
type claudeEvents struct {
	transcript transcriptLines

	// ended is whether a `result` line was read ([claudeEvents.Ended]).
	ended bool

	// calls is each pending call by its id, so its result, in this read or a
	// later one, names it ([activityline]). A call is forgotten when its result
	// arrives, so the map holds only calls in flight; seq numbers the run's calls.
	calls map[string]claudeCall
	seq   int

	// spent is each assistant message's tokens by id, the LARGEST reported
	// (a message's lines carry its usage as it grew), and spentTotal their
	// sum ([claudeEvents.Spent]).
	spent      map[string]int
	spentTotal int

	// requests is each assistant message id the stream named: one model
	// request each, its lines repeating the id as the message grows
	// ([claudeEvents.Requests]).
	requests map[string]struct{}

	// own is the run's own spend, its results' per-model figures summed
	// ([claudeResult.sinceLastResult]); stated whether any result carried
	// them, unread whether one carried them unreadably ([claudeEvents.runSpend]).
	own            runSpend
	stated, unread bool

	// last is the input side of the main thread's newest request, 0 where
	// none since the start or since a line cleared it ([claudeEvents.measure]);
	// usage whether any assistant line carried a usage, main whether any was
	// the main thread's; cleared what the collection says once a line cleared
	// the figure ([claudeEvents.request]), its why empty where none did.
	last        int64
	usage, main bool
	cleared     contextRead

	// window is the least context window above zero the run's results
	// reported holding any model to ([claudeEvents.reportedWindow]), 0 where
	// none reported one.
	window int64

	// limits is the stream's last `rate_limit_event` line, verbatim
	// ([claudeEvents.limitReport]).
	limits string
}

// measure reads one assistant line for the session's newest request
// ([sandbox.Result.LastRequest]): on a main-thread line (parent present and
// null) whose usage reads ([readUsage]), the input side — uncached input, cache
// reads and writes, summed as measure.go sums a capture's — is the newest figure
// where above zero. A message's lines repeat its input side, so the newest line
// of the newest message is the newest request's.
//
// NO EARLIER FIGURE STANDS FOR A LATER REQUEST: a main-thread usage that does not
// read may be the newest, so it clears the figure, as [claudeEvents.Skipped] and
// [claudeEvents.lost] do; no usage changes nothing. An ABSENT key is no main
// thread: every measured line carries it (2.1.310), so a release dropping it
// leaves jobs unmeasured, warned at [claudeEvents.request], never a subagent's.
func (d *claudeEvents) measure(parent, usage json.RawMessage) {
	if usage == nil {
		return
	}
	d.usage = true
	if !bytes.Equal(parent, jsonNull) {
		return
	}
	d.main = true
	var u threadUsage
	if err := readUsage(usage, &u); err != nil {
		d.last = 0
		d.cleared = contextRead{why: "the newest main-thread request's usage does not read: " + err.Error(),
			failed: true}
		return
	}
	if n := u.Input + u.CacheRead + u.CacheWrite; n > 0 {
		d.last, d.cleared = n, contextRead{}
	}
}

// request is what the newest main-thread request carried, as the stream
// reported it, or why there is no figure ([contextReader]). OWN ALWAYS: a run's
// stream reports its own process's requests, never an earlier job's.
func (d *claudeEvents) request() contextRead {
	switch {
	case d.last > 0:
		return contextRead{measure: sandbox.RequestMeasure{Tokens: d.last, Own: true}}
	case d.cleared.why != "":
		return d.cleared
	case d.usage && !d.main:
		return contextRead{why: "the stream's assistant lines carry a usage, and none says it is the main " +
			"thread's (parent_tool_use_id present and null): a release this build does not read", failed: true}
	}
	return contextRead{why: "the stream reports no main-thread request with a usage: the run made none"}
}

// runSpend is the run's own spend summed over every result its stream carried,
// and whether any carried per-run figures (2.1.310 and later). A run can print
// several results (a note handed to it as it finished carries its loop on),
// each stating only the calls since the one before, so the sum is the run's.
//
// NONE ONCE ONE RESULT'S FIGURES ARE UNREAD (more models than
// [claudeResultModelsMax], or no object: [claudeResult.sinceLastResult]): the
// others' sum would charge some calls and not the rest. Stated by none, the run
// is charged by its result's figures for its session ([Runner.ownSpend]).
func (d *claudeEvents) runSpend() (runSpend, bool) {
	if d.unread {
		return runSpend{}, false
	}
	return d.own, d.stated
}

// state adds one result's per-run figures, model by model in name order, so
// the price is summed in one order on every read.
func (d *claudeEvents) state(models map[string]claudeModelUsage) {
	d.stated = true
	for _, name := range slices.Sorted(maps.Keys(models)) {
		m := models[name]
		if m.ContextWindow > 0 && (d.window == 0 || m.ContextWindow < d.window) {
			d.window = m.ContextWindow
		}
		d.own.input += m.InputTokens
		d.own.cacheRead += m.CacheReadInputTokens
		d.own.cacheWrite += m.CacheCreationInputTokens
		d.own.output += m.OutputTokens
		d.own.costUSD += m.CostUSD
	}
}

// Spent is every token the run's model calls have reported so far: what a
// live reading checks a run's token cap against while it works.
func (d *claudeEvents) Spent() int { return d.spentTotal }

// count records one assistant message's usage.
func (d *claudeEvents) count(id string, u claudeUsage) {
	if id == "" || u.total() == 0 {
		return
	}
	if d.spent == nil {
		d.spent = map[string]int{}
	}
	if seen := d.spent[id]; u.total() > seen {
		d.spentTotal += u.total() - seen
		d.spent[id] = u.total()
	}
}

func (d *claudeEvents) Line(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	if line[0] != '{' {
		// Not an event: text the harness printed (a banner, a crash), which
		// is part of what the run said.
		d.transcript.add(string(line))
		return
	}
	var ev claudeEventWire
	if wireAlias(line, &ev) != nil {
		// A partial object: read mid-write (the next read has it whole) or
		// cut short where the harness was killed. Either may be a later
		// request's line.
		d.lost(fmt.Sprintf("a line of %s that does not decode, cut short where its harness was killed "+
			"writing it,", humanSize(int64(len(line)))))
		return
	}
	switch looseString(ev.Type) {
	case claudeLimitEvent:
		d.limits = string(line)
	case "system":
		if line, ok := claudeSteerLine(looseString(ev.Subtype), ev.Content); ok {
			d.transcript.add(line)
		}
	case "assistant":
		message := claudeMessageOf(ev.Message)
		d.requested(looseString(message.ID))
		d.count(looseString(message.ID), looseUsage(message.Usage))
		d.measure(ev.ParentToolUseID, message.Usage)
		_ = eachClaudeBlock(message.Content, func(block claudeBlock) error {
			switch block.Type {
			case "text":
				if text := strings.TrimSpace(block.Text); text != "" {
					d.transcript.add(text)
				}
			case "tool_use":
				d.seq++
				for _, l := range activityline.Call(d.seq, firstNonBlank(block.Name, "tool"),
					claudeToolSubject(block)) {
					d.transcript.add(l)
				}
				if block.ID != "" {
					if d.calls == nil {
						d.calls = map[string]claudeCall{}
					}
					d.calls[block.ID] = claudeCall{seq: d.seq, at: claudeStamp(ev.Timestamp)}
				}
			}
			return nil
		})
	case "user":
		_ = eachClaudeBlock(claudeMessageOf(ev.Message).Content, func(block claudeBlock) error {
			if block.Type != "tool_result" {
				return nil
			}
			call := d.calls[block.ToolUseID]
			delete(d.calls, block.ToolUseID)
			took := call.took(claudeStamp(ev.Timestamp))
			if block.IsError {
				d.transcript.add(activityline.Failed(call.seq, claudeResultText(block.Content), took))
			} else {
				d.transcript.add(activityline.OK(call.seq, took))
			}
			return nil
		})
	case "result":
		d.ended = true
		// A run that succeeded ends on its final text, already carried as the
		// assistant's last message, so its text is decoded only where it did
		// not, to say how, as [ClaudeCode.Parse] reads the same line.
		wire := ev.claudeResultWire
		text := wire.Result
		wire.Result = nil
		result := wire.result()
		if !result.succeeded() {
			result.Result = looseString(text)
			d.transcript.add(activityline.Words(activityline.TagError,
				"the run ended: "+firstLine(result.failure(), transcriptDetailLimit)))
		}
		// PRESENT, EVEN EMPTY, IS STATED: no calls since the last result says
		// nothing more was spent. Unread figures ([claudeResult.sinceLastResult])
		// leave the run's own spend unread, and cost the line nothing else.
		switch models, stated, err := result.sinceLastResult(); {
		case err != nil:
			d.unread = true
		case stated:
			d.state(models)
		}
	}
}

// claudeLimitEvent is the event Claude Code writes the plan's limit windows in,
// read by the engine's profile ([sandbox.Result.Limits]).
const claudeLimitEvent = "rate_limit_event"

// limitReport is the stream's last limit report line, "" where it wrote none.
func (d *claudeEvents) limitReport() string { return d.limits }

// reportedWindow is the least context window any result's per-run figures
// reported for a model, subagents' included, 0 where none did
// ([sandbox.Result.ReportedWindow]). THE LEAST, so a continuation is told more
// than it needs, never less. Unread figures report nothing.
func (d *claudeEvents) reportedWindow() int64 { return d.window }

// Skipped counts a line past the stream's bound into the transcript and clears
// the newest request's figure ([claudeEvents.lost]).
func (d *claudeEvents) Skipped(n int64, bound int) {
	d.transcript.skip(n, bound)
	d.lost(fmt.Sprintf("a line of %s, past the %s a line of the stream is read within,", humanSize(n),
		humanSize(int64(bound))))
}

// lost clears the newest request's figure for a line the decoder could not
// read (line says why: past the bound, or not decoding): it may be a later
// request's, so the figure before it measures nothing ([claudeEvents.measure]).
// Said at info: such a line is the run's own circumstance, not an unread
// release. A no-op where no figure stands, keeping the reason as it was.
func (d *claudeEvents) lost(line string) {
	if d.last > 0 {
		d.last = 0
		d.cleared = contextRead{why: line + " followed the newest main-thread request's figure"}
	}
}

// Entries implements [Decoder]. Nothing else here grows with the stream: the
// pending calls are forgotten as their results arrive.
func (d *claudeEvents) Entries(end bool) []string { return d.transcript.take(end) }

// Ended implements [Decoder]: a `result` line, which Claude Code prints as its
// run ends.
func (d *claudeEvents) Ended() bool { return d.ended }

// Requests implements [Decoder]: the assistant messages the stream named, by
// id, every thread's, a subagent's included. Never the result's `num_turns`,
// which is the harness's own word on its count and absent where it was cut off.
func (d *claudeEvents) Requests() int { return len(d.requests) }

// requested records one assistant message's id.
func (d *claudeEvents) requested(id string) {
	if id == "" {
		return
	}
	if d.requests == nil {
		d.requests = map[string]struct{}{}
	}
	d.requests[id] = struct{}{}
}

func (d *claudeEvents) Result() sandbox.Result {
	var r sandbox.Result
	r.Transcript, r.TranscriptElidedLines, r.TranscriptElidedBytes = d.transcript.result()
	return r
}

// claudeSteerPrefix opens the `system/informational` line a hook of the run's
// own prints as it hands the model a note or a late call ([ClaudeCode.hookAnswer]):
// the event that fired it, then the line's own opening (measured on 2.1.263
// and 2.1.310).
var claudeSteerPrefix = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^(?:` + claudeEventToolBatch + `|` +
		claudeEventFinish + `) says: crewlet: `)
})

// claudeSteerLine is a [activityline.TagSteer] line: the first line of what the run's own hook
// said ([claudeSteerSaid]), redacted and bounded as a live line is
// ([firstLine]): the one place a person watching sees the note arrive. Nothing
// for any other system message.
func claudeSteerLine(subtype string, raw json.RawMessage) (string, bool) {
	said, ok := claudeSteerSaid(subtype, raw)
	if !ok {
		return "", false
	}
	line := firstLine(said, transcriptDetailLimit)
	if line == "" {
		return "", false
	}
	return activityline.Words(activityline.TagSteer, line), true
}

// claudeSteerSaid is what the run's own hook said as it handed the model a
// note or a late call — the content after the event's opening, AS THE HARNESS
// RECORDED IT, less the take's token, cut here alone ([withoutTakeToken]) — and
// false for any other system message. ONE READING for the live transcript
// ([claudeSteerLine]) and a captured session's ([ReadTranscript]). Neither
// redaction nor a bound is here, because each applies its own: the live view
// redacts the whole and cuts a line; the session reader redacts and bounds an
// entry only as it hands it out, so a record before its page is never redacted.
func claudeSteerSaid(subtype string, raw json.RawMessage) (string, bool) {
	if subtype != "informational" {
		return "", false
	}
	var content string
	if wireDecode(raw, &content) != nil {
		return "", false
	}
	return claudeSteerContent(content)
}

// claudeSteerContent is [claudeSteerSaid] of an informational record's content,
// decoded.
func claudeSteerContent(content string) (string, bool) {
	loc := claudeSteerPrefix().FindStringIndex(content)
	if loc == nil {
		return "", false
	}
	return withoutTakeToken(content[loc[1]:]), true
}

// claudeToolSubject is what one call works on, whole: its subject field
// ([claudeToolInput]), or for a tool with none (an MCP server's) its input as
// compact JSON. [activityline.Call] previews and bounds it.
func claudeToolSubject(block claudeBlock) string {
	// DECODED LOOSELY by exact key names, as the block is: a retyped field
	// leaves the others named, and an input that is no object names no
	// subject — the call's name is still its line.
	var in claudeToolInput
	if wireObject(block.Input) && wireDecode(block.Input, &in) != nil {
		in = claudeToolInput{}
	}
	if subject := firstNonBlank(looseString(in.Command), looseString(in.FilePath),
		looseString(in.NotebookPath), looseString(in.Pattern), looseString(in.URL), looseString(in.Query),
		looseString(in.Path), looseString(in.Description)); subject != "" {
		return subject
	}
	return compactInput(block.Input)
}

// claudeCall is a call in flight: its number in the run, and when the line
// that made it was written (zero where the line says no time).
type claudeCall struct {
	seq int
	at  time.Time
}

// took is how long the call ran, from its line's time to its result's: known
// only where both lines carry one, in order.
func (c claudeCall) took(done time.Time) activityline.Took {
	if c.at.IsZero() || done.IsZero() || done.Before(c.at) {
		return activityline.Took{}
	}
	return activityline.Took{D: done.Sub(c.at), Known: true}
}

// claudeStamp is a line's timestamp, or zero where it carries none that reads.
func claudeStamp(raw json.RawMessage) time.Time {
	at, err := time.Parse(time.RFC3339Nano, looseString(raw))
	if err != nil {
		return time.Time{}
	}
	return at
}

// claudeResultText is a tool result's text, from a string or its blocks
// ([toolAnswer]): each text block's, a line apart, and an image named where it
// was, since a screenshot read as empty would say the tool answered nothing.
// "" for content of any other kind.
func claudeResultText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return looseString(raw)
	}
	return toolAnswer(raw, func(kind, text, _ string) (string, bool) {
		switch kind {
		case "text":
			return text, strings.TrimSpace(text) != ""
		case "image":
			return claudeImagePlaceholder, true
		}
		return "", false
	})
}

// claudeImagePlaceholder is what a tool result's image reads as in text.
const claudeImagePlaceholder = "[an image]"

// toolAnswer is a tool's answer as text, from list, its parts: what said makes
// of each part by its `type`, `text` and `mime` (each its string, or "" where of
// another kind or the part is no object), a line apart, in order, skipping a
// part said passes over. "" for a list that does not decode to its end:
// Claude Code's result blocks ([claudeResultText]), which an OpenCode call's
// answer is joined as, a part at a time ([callResult]).
//
// A PART AT A TIME ([eachElement]), by exact key names ([threadDecoding]) and
// loosely by type, each read in place ([eachKeyInPlace]), never the whole list
// first, which would hold a part per element before reading any. AN ANSWER OF
// ONE PART IS ITS TEXT, never copied again into a builder.
func toolAnswer(list json.RawMessage, said func(kind, text, mime string) (string, bool)) string {
	var (
		answer strings.Builder
		first  string
		kept   int
	)
	if eachElement(list, func(element json.RawMessage, _ int) error {
		// A part that is no object is the zero part, which says nothing.
		var kind, text, mime json.RawMessage
		if wireObject(element) {
			_ = eachKeyInPlace(element, func(name []byte, value json.RawMessage) error {
				switch string(name) {
				case "type":
					kind = value
				case "text":
					text = value
				case "mime":
					mime = value
				}
				return nil
			})
		}
		line, ok := said(looseString(kind), looseString(text), looseString(mime))
		switch {
		case !ok:
			return nil
		case kept == 0:
			first = line
		case kept == 1:
			answer.WriteString(first)
			fallthrough
		default:
			answer.WriteByte('\n')
			answer.WriteString(line)
		}
		kept++
		return nil
	}) != nil {
		return ""
	}
	if kept == 1 {
		return first
	}
	return answer.String()
}
