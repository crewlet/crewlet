package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// Claude Code's stream-json, read into a transcript.
//
// One JSON object a line, of the message types the vendor's Agent SDK
// documents: `system` (the session's `init`, retries, notices), `assistant`
// (an Anthropic message whose content blocks are `text`, `thinking` and
// `tool_use`), `user` (the tool results handed back, as `tool_result` blocks
// with `is_error` on a failure) and, last, `result` (how the run ended, its
// usage and cost — the object `--output-format json` printed alone). A
// subagent's messages ride the same stream with their `parent_tool_use_id`
// set, and are activity like any other.
//
// AND WHAT THE SESSION'S NEWEST REQUEST CARRIED, read off the same lines as the
// job ends ([claudeEvents.measure]): the main thread's newest request's input
// side, which a relaunch that continues the copy the job's box holds sends again
// before its brief, and which that relaunch's floor is held to
// ([sandbox.Result.LastRequest]).
//
// THE TRANSCRIPT IS OPENCODE'S SHAPE: what the agent said, one line per tool
// call naming the tool and the first line of what it ran, and a failed call's
// first line of error. NEVER a successful tool result's body — that is a whole
// file read, or everything a command printed, and it is what the transcript is
// a summary of; a stream carries it twice over (the tool_result block, and a
// `tool_use_result` copy beside it), and echoing it would grow the record and
// the live view by orders of magnitude and push far more of somebody's files
// through redaction. Thinking is not shown either: it is the model's working,
// not what the run did. A message type this build does not know is skipped,
// because a harness release adds them (the SDK's union has dozens).
//
// ONE SYSTEM LINE IS SHOWN: the line the run's own hooks print when they hand
// its model a person's note ([ClaudeHookAnswer]'s transcript line), which the
// harness streams as `system/informational`, its content `<event> says: <line>`
// (measured on 2.1.263 and 2.1.310), and which becomes a `[steer]` line — the
// one place a person watching the run sees the note arrive. Every other system
// line — the session's init, a retry, a hook's own start and end — is skipped,
// as before.

// claudeEventWire is the part of one stream message this decoder reads, as
// its one decode leaves it ([wireDecode]): the result message's keys, which
// the last line carries ([claudeResultWire]), a system line's text and a
// message, each by its exact name and raw, and each read, loosely by type,
// only where the line's type asks for it ([claudeEvents.Line]).
//
// BY ITS KEYS EXACTLY AS WRITTEN, as Claude Code's own JSON.parse reads them
// ([threadDecoding]), as a message's blocks are ([eachClaudeBlock]), so one
// line is read by one rule: decoded by encoding/json, a line was read with a
// key that differs from one it reads only in case taken for that one, and a
// message written twice read as the two merged, where its blocks were read
// exactly and the person's transcript of the same record read the last.
type claudeEventWire struct {
	claudeResultWire
	// Content is a system line's text: an informational line's, which is
	// the only one read ([claudeSteerLine]). Raw, because a system subtype
	// this build does not know may carry anything there.
	Content json.RawMessage `json:"content"`
	// Message is an assistant or a user line's message ([claudeMessageWire]).
	Message json.RawMessage `json:"message"`
	// ParentToolUseID is the call of the main thread's whose subagent wrote
	// the line, raw and AS WRITTEN: the literal null on a line of the main
	// thread's own, a call's id on a subagent's, and nil where the key is
	// absent, which is no main thread's ([claudeEvents.measure]).
	ParentToolUseID json.RawMessage `json:"parent_tool_use_id"`
}

// claudeMessageWire is one line's message as its decode leaves it, each key
// raw: its content, an array of blocks on an assistant message and on a tool
// result's user message, and a plain string on a user message that is a
// prompt (a subagent's first message); and an assistant message's id and
// usage, which the stream repeats on every line of the one message (a line
// per content block), so a message's usage is counted once, by its id.
type claudeMessageWire struct {
	Content json.RawMessage `json:"content"`
	ID      json.RawMessage `json:"id"`
	Usage   json.RawMessage `json:"usage"`
}

// claudeMessageOf is the message raw holds, its keys raw: none where it is no
// object, as the whole decode it replaced left a message it could not fill.
func claudeMessageOf(raw json.RawMessage) claudeMessageWire {
	var m claudeMessageWire
	if !wireObject(raw) || wireDecode(raw, &m) != nil {
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

// looseUsage is the usage raw holds, DECODED LOOSELY, as the whole decode it
// replaced read one: a count of another kind than a whole number reads as
// none, the other counts as written, and a usage that is no object as no
// tokens at all.
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

// claudeBlock is one content block of a message, as the stream carries it and
// as the session's own record keeps it ([ReadTranscript]): ONE SHAPE for both,
// because they are one format, and a second decoder of it is a second answer
// to what a block holds. Read a block at a time ([eachClaudeBlock]).
type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// A tool_use block. Input is the call's input as the harness wrote it:
	// the live transcript names its subject ([claudeToolLine]), and a
	// session's transcript shows it whole.
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// A tool_result block. Content is the result's text as a string or as
	// blocks ([claudeResultText]): the live transcript reads it for a failed
	// call alone, and a session's transcript for every call.
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// claudeBlockWire is one content block as its decode leaves it ([wireDecode]):
// each key a [claudeBlock] holds, by its exact name and raw, read into the
// block by [claudeBlockWire.block].
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

// block is the block w holds, DECODED LOOSELY, as the stream's whole decode
// read one: a key of another kind than its field's reads as that field's zero
// value, the rest of the block as written.
func (w claudeBlockWire) block() claudeBlock {
	return claudeBlock{
		Type: looseString(w.Type), Text: looseString(w.Text),
		ID: looseString(w.ID), Name: looseString(w.Name), Input: w.Input,
		ToolUseID: looseString(w.ToolUseID), IsError: looseTrue(w.IsError), Content: w.Content,
	}
}

// eachClaudeBlock hands each block of a message's content to each, in order,
// as it reads it, and returns each's first error, as it returned it, having
// read no block after it. A content that is no list (a prompt's plain string)
// has no blocks, and one that stops decoding part way has none after the
// fault, which no content of a line decoded whole can have: both the stream
// and the transcript hold the line to JSON first.
//
// A BLOCK AT A TIME ([eachElement]), each decoded by its keys exactly as
// written ([threadDecoding]: as Claude Code's own JSON.parse reads them, a
// string's invalid UTF-8 or lone surrogate as U+FFFD) and loosely by type
// ([claudeBlockWire.block]), and never the whole list first. Decoded whole,
// a list held a block's 136 bytes for every element it listed, however small
// the element, before any was looked at: at the 32 MiB a line of the stream
// or of a session's record may hold ([maxLineBytes]), more than a GiB for one
// line. Read a block at a time, a list costs its reader one block's decode at
// a time and what it makes of each block. An element that is no object is
// the zero block, as the whole decode left it, so each is handed every
// element: a reader that checks its caller between blocks still does so
// between every two.
func eachClaudeBlock(content json.RawMessage, each func(claudeBlock) error) error {
	var (
		wire    claudeBlockWire
		stopped error
	)
	// The walk's own error is no reader's: the blocks before it were read.
	_ = eachElement(content, func(element json.RawMessage, _ int) error {
		wire = claudeBlockWire{}
		if wireObject(element) && wireDecode(element, &wire) != nil {
			wire = claudeBlockWire{}
		}
		stopped = each(wire.block())
		return stopped
	})
	return stopped
}

// looseString is raw where it is a JSON string, and "" where it is absent or
// of another kind, null among them: a key a release retyped, read as the
// whole decode it replaced left a string it could not fill. Its kind is read
// off its first byte, so a value of another kind is never decoded.
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

// looseTrue reports whether raw is JSON's true: false where it is false,
// absent or of another kind, as the whole decode it replaced left a boolean
// it could not fill.
func looseTrue(raw json.RawMessage) bool { return string(raw) == "true" }

// looseInt is raw where it is a JSON number that is a whole int64, and 0 where
// it is absent, of another kind, a fraction or past an int64, as the whole
// decode it replaced left an integer it could not fill. Its kind is read off
// its first byte, so a value of another kind is never decoded.
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

// looseFloat is raw where it is a JSON number a float64 holds, and 0 where it
// is absent, of another kind or past a float64's range, as the whole decode it
// replaced left a float it could not fill. Its kind is read off its first
// byte, so a value of another kind is never decoded.
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

// claudeToolInput is what a transcript line names of a tool call: the field
// each of the harness's tools takes its subject in — Bash its command, Read, Edit
// and Write their file, NotebookEdit its notebook, Grep and Glob their
// pattern, WebFetch its URL, WebSearch its query, Task its description — each
// as its decode leaves it, by its exact name and raw, read loosely
// ([claudeToolLine]).
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

	// tools names a pending call by its id, so the result that comes back
	// for it can say which tool failed — in the same read or a later one of
	// a followed stream. A call is forgotten once its result arrives, so the
	// map holds only the calls still in flight.
	tools map[string]string

	// spent is each assistant message's tokens by its id, the LARGEST the
	// stream reported for it (a message's lines carry its usage as it
	// grew), and spentTotal their sum: what [claudeEvents.Spent] answers.
	spent      map[string]int
	spentTotal int

	// own is the run's own spend as its results stated it, each result's
	// per-model figures for the calls since the one before
	// ([claudeResult.SinceLastResult]) added up; stated whether any result
	// carried them, and unread whether one carried them in a form they are
	// not read in: for more models than a result is read for, or as no
	// object ([claudeEvents.runSpend]).
	own            runSpend
	stated, unread bool

	// last is the input side of the newest request of the main thread the
	// stream reported, 0 where none since the stream began or since a line
	// that cleared it ([claudeEvents.measure]); usage whether any assistant
	// line carried a usage at all, and main whether any was read as the main
	// thread's; and cleared what the collection says where a line cleared
	// the newest figure ([claudeEvents.request]), its why empty where none
	// did.
	last        int64
	usage, main bool
	cleared     contextRead

	// window is the least context window above zero the run's results
	// reported holding any model to ([claudeEvents.reportedWindow]), 0 where
	// none reported one.
	window int64
}

// measure reads one assistant line's request for the measure of the session
// its box holds as the job ends ([sandbox.Result.LastRequest]): where the line
// is the main thread's (parent, its `parent_tool_use_id`, present and null) and
// its usage reads by the layer's one rule ([readUsage]), the request's input
// side, its uncached input, cache reads and cache writes, as measure.go adds
// a capture's up, is the newest figure where it is above zero; a message's
// lines repeat its input side, so the newest line of the newest message is the
// newest request's.
//
// NO EARLIER FIGURE IS EVER TAKEN FOR A LATER REQUEST: a main-thread line whose
// usage does not read clears the figure, as a line past the stream's bound does
// ([claudeEvents.Skipped]) and one that does not decode ([claudeEvents.lost]),
// since the request it reported may be the newest and carried anything. A line
// with no usage says nothing of its request, and changes nothing.
//
// A KEY THAT IS ABSENT IS NO MAIN THREAD: every line of a stream measured
// carries the key (2.1.310), null on the main thread and a call's id on a
// subagent's, so a line that carries none is of a shape this build does not
// read, and its usage is no figure. A release that drops the key leaves every
// job unmeasured, which the collection says at warning ([claudeEvents.request]),
// and a relaunch then holds the copy to the figure carried along its lineage,
// never to a subagent's.
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

// request is what the newest main-thread request of the session the run worked
// in carried, as the stream reported it, and why there is no figure where there
// is none ([contextReader]). OWN ALWAYS: a run's stream reports its own
// process's requests, never an earlier job's.
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

// runSpend is the run's own spend, summed over every result its stream
// carried, and whether any carried the per-run figures (2.1.310 and later).
// A run can print several results — a person's note handed to it as it
// finished carries its loop on to another — and each states only the calls
// since the one before, so the sum is the whole run's.
//
// NONE ONCE ONE RESULT'S FIGURES ARE UNREAD: a result naming more models than
// a result is read for ([claudeResultModelsMax]) is read for none of them,
// and so is one whose figures are there and no object, null among them
// ([claudeResult.sinceLastResult]), so the sum of the others is no run's
// spend, and would charge the run for some of its calls and not the rest.
// Stated by none, the run is charged by its result's figures for its
// session, as a run on a release that states none is ([Runner.ownSpend]).
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
		// Not an event: something the harness printed as text (a banner, a
		// crash), which is part of what the run said.
		d.transcript.add(string(line))
		return
	}
	var ev claudeEventWire
	if wireDecode(line, &ev) != nil {
		// A partial object: the stream read mid-write, which the next read has
		// whole, or its last line cut short where its harness was killed
		// writing it, which no read ever has whole. Either may be a later
		// request's line.
		d.lost(fmt.Sprintf("a line of %s that does not decode, cut short where its harness was killed "+
			"writing it,", humanSize(int64(len(line)))))
		return
	}
	switch looseString(ev.Type) {
	case "system":
		if line, ok := claudeSteerLine(looseString(ev.Subtype), ev.Content); ok {
			d.transcript.add(line)
		}
	case "assistant":
		message := claudeMessageOf(ev.Message)
		d.count(looseString(message.ID), looseUsage(message.Usage))
		d.measure(ev.ParentToolUseID, message.Usage)
		_ = eachClaudeBlock(message.Content, func(block claudeBlock) error {
			switch block.Type {
			case "text":
				if text := strings.TrimSpace(block.Text); text != "" {
					d.transcript.add(text)
				}
			case "tool_use":
				d.transcript.add(claudeToolLine(block))
				if block.ID != "" {
					d.tools[block.ID] = block.Name
				}
			}
			return nil
		})
	case "user":
		_ = eachClaudeBlock(claudeMessageOf(ev.Message).Content, func(block claudeBlock) error {
			if block.Type != "tool_result" {
				return nil
			}
			name := d.tools[block.ToolUseID]
			delete(d.tools, block.ToolUseID)
			if block.IsError {
				if name == "" {
					name = "tool"
				}
				line := "[tool] " + name + " → error"
				if failure := strings.TrimSpace(claudeResultText(block.Content)); failure != "" {
					line += ": " + firstLine(failure, transcriptDetailLimit)
				}
				d.transcript.add(line)
			}
			return nil
		})
	case "result":
		// A run that succeeded ends on its final text, which the stream
		// already carried as the assistant's last message; one that did not
		// says how, as [ClaudeCode.Parse] reads the same line.
		result := ev.result()
		if !result.succeeded() {
			d.transcript.add("[error] the run ended: " + firstLine(result.failure(), transcriptDetailLimit))
		}
		// PRESENT, EVEN EMPTY, IS STATED: a result with no calls since the
		// one before says the run spent nothing more, which is an answer. One
		// whose spend is unread, as one naming more models than a result is
		// read for is, or one whose spend is no object
		// ([claudeResult.sinceLastResult]), leaves the run's own spend
		// unread, and costs the line nothing else.
		switch models, stated, err := result.sinceLastResult(); {
		case err != nil:
			d.unread = true
		case stated:
			d.state(models)
		}
	}
}

// reportedWindow is the least context window the run's process reported
// holding any model to, over every result's per-run figures (2.1.310 and
// later), its subagents' models among them, 0 where none reported one
// ([sandbox.Result.ReportedWindow]). THE LEAST, so a subagent on a model of a
// smaller window lowers it: a continuation told more than it needs, never
// less. A result whose figures are unread reports nothing.
func (d *claudeEvents) reportedWindow() int64 { return d.window }

// Skipped counts a line past the stream's bound into the transcript, and clears
// the newest request's figure: the line may be a later request's, which carried
// anything ([claudeEvents.lost]).
func (d *claudeEvents) Skipped(n int64, bound int) {
	d.transcript.skip(n, bound)
	d.lost(fmt.Sprintf("a line of %s, past the %s a line of the stream is read within,", humanSize(n),
		humanSize(int64(bound))))
}

// lost clears the newest request's figure for a line the stream holds and the
// decoder could not read, which line says: past the stream's bound, or not
// decoding. It may be a later request's, which carried anything, so the figure
// before it is no measure of the newest request ([claudeEvents.measure]), and
// the collection says why there is none at info, since such a line is the
// run's own circumstance, never a release this build does not read. Nothing
// where no figure stands, which keeps why there is none as it was.
func (d *claudeEvents) lost(line string) {
	if d.last > 0 {
		d.last = 0
		d.cleared = contextRead{why: line + " followed the newest main-thread request's figure"}
	}
}

// Entries implements [Decoder]. Nothing else here grows with the stream: the
// pending calls are forgotten as their results arrive.
func (d *claudeEvents) Entries() []string { return d.transcript.take() }

func (d *claudeEvents) Result() sandbox.Result {
	return sandbox.Result{Transcript: d.transcript.String()}
}

// claudeSteerPrefix is how the harness opens the informational line a hook of the
// run's own prints — the event that fired it and the line's own opening
// ([ClaudeHookAnswer]) — and which events those are.
var claudeSteerPrefix = regexp.MustCompile(`^(?:` + claudeEventToolBatch + `|` +
	claudeEventFinish + `) says: crewlet: `)

// claudeSteerLine is the transcript line for a system message of subtype, its
// content raw: `[steer]` and what the run's own hook said — its first line,
// redacted whole and bounded as a live transcript's line is ([firstLine]) — for
// the line it prints as it hands the model a note, and nothing for any other.
func claudeSteerLine(subtype string, raw json.RawMessage) (string, bool) {
	said, ok := claudeSteerSaid(subtype, raw)
	if !ok {
		return "", false
	}
	line := firstLine(said, transcriptDetailLimit)
	if line == "" {
		return "", false
	}
	return "[steer] " + line, true
}

// claudeSteerSaid is what the run's own hook said in a system message of
// subtype, its content raw, as it handed the model a note — the content after
// the event's opening, AS THE HARNESS RECORDED IT — and false for every other
// system message. ONE READING of which message that is and where what the
// hook said begins, for the live transcript ([claudeSteerLine]) and for a
// captured session's ([ReadTranscript]); each shows it in its own way, which
// is why neither the redaction nor a bound is here. The live view redacts the
// whole of it and cuts a line for a phase event; a captured session's reader
// redacts and bounds an entry's text as it hands the entry out, and only then
// — a record before the page it is reading is counted and never redacted.
func claudeSteerSaid(subtype string, raw json.RawMessage) (string, bool) {
	if subtype != "informational" {
		return "", false
	}
	var content string
	if wireDecode(raw, &content) != nil {
		return "", false
	}
	loc := claudeSteerPrefix.FindStringIndex(content)
	if loc == nil {
		return "", false
	}
	return content[loc[1]:], true
}

// claudeToolLine renders one tool call for the transcript: its name and the
// first line of its subject, bounded as OpenCode's are.
func claudeToolLine(block claudeBlock) string {
	name := firstNonBlank(block.Name, "tool")
	line := "[tool] " + name
	// DECODED LOOSELY, as the block around it is, and by its keys exactly as
	// written: a field a release retyped leaves the others named, and an input
	// that is no object names no subject at all — the call's name is still
	// its line.
	var in claudeToolInput
	if wireObject(block.Input) && wireDecode(block.Input, &in) != nil {
		in = claudeToolInput{}
	}
	if detail := firstNonBlank(looseString(in.Command), looseString(in.FilePath),
		looseString(in.NotebookPath), looseString(in.Pattern), looseString(in.URL), looseString(in.Query),
		looseString(in.Path), looseString(in.Description)); detail != "" {
		line += ": " + firstLine(detail, transcriptDetailLimit)
	}
	return line
}

// claudeResultText is a tool result's text, from a string or from its blocks
// ([toolAnswer]): each text block's, a line apart, and an image named where it
// was, since a result that was a screenshot read as empty would say the tool
// answered nothing. "" for a content of any other kind.
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

// toolAnswer is what a tool answered, as text, from list, its answer's parts:
// what said makes of each part, by its `type`, `text` and `mime` (each its
// string, or "" where it is of another kind or the part is no object), a line
// apart, in order, and nothing of a part said passes over. "" for a list that
// does not decode to its end, as its whole decode answered. ONE READING of a
// tool's answer for both harnesses: a Claude Code result's content blocks
// ([claudeResultText]) and an OpenCode tool's content ([openCodeAnswer]).
//
// A PART AT A TIME ([eachElement]), each decoded by its keys exactly as
// written ([threadDecoding]) and loosely by type, and never the whole list
// first, which held a part for every element the list held before any was
// looked at, however small the element. Read so, a list costs one part's
// decode at a time, and the text it says.
func toolAnswer(list json.RawMessage, said func(kind, text, mime string) (string, bool)) string {
	var (
		answer strings.Builder
		kept   bool
		part   struct {
			Type json.RawMessage `json:"type"`
			Text json.RawMessage `json:"text"`
			Mime json.RawMessage `json:"mime"`
		}
	)
	if eachElement(list, func(element json.RawMessage, _ int) error {
		// A part that is no object is the zero part, which says nothing, as
		// the whole decode left it.
		part.Type, part.Text, part.Mime = nil, nil, nil
		if wireObject(element) && wireDecode(element, &part) != nil {
			part.Type, part.Text, part.Mime = nil, nil, nil
		}
		text, ok := said(looseString(part.Type), looseString(part.Text), looseString(part.Mime))
		if !ok {
			return nil
		}
		if kept {
			answer.WriteByte('\n')
		}
		answer.WriteString(text)
		kept = true
		return nil
	}) != nil {
		return ""
	}
	return answer.String()
}
