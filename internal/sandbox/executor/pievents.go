package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// piLine is the part of one line of Pi's `--mode json` stream (1.6.0) that this
// runner reads, or of the script's own mark ([piExtensionMark]). DECODED
// LOOSELY: a field a release retyped reads as zero, and the rest still read.
type piLine struct {
	Type string `json:"type"`
	// ID is the session's, on the stream's `session` header line.
	ID string `json:"id"`
	// Message is a message_start/message_end's message.
	Message *piMessage `json:"message"`
	// A tool call's start and end.
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	IsError    bool            `json:"isError"`
	// Result is a tool's result on tool_execution_end and a compaction's on
	// compaction_end, each read on its own type.
	Result json.RawMessage `json:"result"`
	// Entry is an entry an extension appended (entry_appended).
	Entry *piEntry `json:"entry"`
	// Aborted is agent_settled's and compaction_end's; Reason, ErrorMessage and
	// WillRetry a compaction's or a retry's.
	Aborted      bool   `json:"aborted"`
	Reason       string `json:"reason"`
	ErrorMessage string `json:"errorMessage"`
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"maxAttempts"`
}

// piMessage is a message as the stream and the session record carry it: its
// role (`user`, `assistant`, `toolResult`, `system`, `custom`), its content (a
// string, or an array of text, thinking and toolCall blocks), and on an
// assistant message why it stopped (`stop`, `toolUse`, `length`, `error`,
// `aborted`), its error and its usage.
type piMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Usage        *piUsage        `json:"usage"`
	ToolCallID   string          `json:"toolCallId"`
	ToolName     string          `json:"toolName"`
	IsError      bool            `json:"isError"`
}

// piUsage is one request's usage: `input` the prompt NOT served from the
// cache, its cached shares beside it (measured on 1.6.0).
type piUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

// piEntry is one session entry (session format 3), as the record holds it and
// entry_appended prints it: its tree place, its type, and each type's fields
// read here.
type piEntry struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	ParentID   string          `json:"parentId"`
	Timestamp  string          `json:"timestamp"`
	CustomType string          `json:"customType"`
	Content    json.RawMessage `json:"content"`
	// Details and Data are an extension's own, raw: another extension's of
	// another shape must not lose the entry its place on the branch.
	Details json.RawMessage `json:"details"`
	Data    json.RawMessage `json:"data"`
	Message *piMessage      `json:"message"`
	Summary string          `json:"summary"`
	// TargetID is a context_edit's: the entry it takes out of what the model
	// reads, or replaces.
	TargetID string `json:"targetId"`
}

// inbox is the inbox id a note's details carry, "" for none.
func (e piEntry) inbox() string {
	var d struct {
		Inbox string `json:"inbox"`
	}
	_ = json.Unmarshal(e.Details, &d)
	return d.Inbox
}

// data is the extension's own data of the entry, zero where it has none of
// this shape.
func (e piEntry) data() piEntryData {
	var d piEntryData
	_ = json.Unmarshal(e.Data, &d)
	return d
}

// piEntryData is the data of the extension's own entries: the cap that
// stopped the run, the servers that offered no tool or an mcp.json entry
// redefines, why pi leaves their tools out of its requests, the context pi
// reported.
type piEntryData struct {
	Turns         int      `json:"turns"`
	Missing       []string `json:"missing"`
	Redefined     []string `json:"redefined"`
	Hidden        string   `json:"hidden"`
	Tokens        int64    `json:"tokens"`
	ContextWindow int64    `json:"contextWindow"`
}

// piContent is a message's content as text blocks and tool calls: a string is
// one text block.
type piContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// piBlocks is raw's blocks, a string read as one text block; none for content
// of no shape read.
func piBlocks(raw json.RawMessage) []piContent {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil
		}
		return []piContent{{Type: "text", Text: s}}
	}
	var blocks []piContent
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	return blocks
}

// piText is the text blocks of raw, joined.
func piText(raw json.RawMessage) string {
	var parts []string
	for _, b := range piBlocks(raw) {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// decodePiLine decodes one line, false where it is no JSON object of a type.
func decodePiLine(line []byte) (piLine, bool) {
	var ev piLine
	if err := json.Unmarshal(line, &ev); err != nil {
		var typed struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &typed) != nil || typed.Type == "" {
			return piLine{}, false
		}
		return piLine{Type: typed.Type}, true
	}
	return ev, ev.Type != ""
}

// piAnswered is whether an assistant message stopped as an answer the model
// gave: `stop`, `toolUse` or `length`, never `error` or `aborted`.
func piAnswered(stop string) bool { return stop == "stop" || stop == "toolUse" || stop == "length" }

// piEvents decodes a Pi run's stream ([Decoder]): its answer, the last
// assistant message's text; how it ended, off the last assistant message, the
// extension's entries and agent_settled; the calls for its transcript; and its
// own spend, every assistant message's usage and every compaction's (the stream
// carries this process's messages alone, a resumed run's included).
type piEvents struct {
	sawLine, sawEvent bool
	sessionID         string

	// answer is the last assistant message's text, stop why it stopped, and
	// failure its error: the provider's status and words, which the entry's
	// limit and sign-in markers are matched against, alone, never the model's
	// prose beside them.
	answer, stop, failure string

	// marked is the script's mark ([piExtensionMark]); armed the extension's
	// word that it ran; capped the cap it says
	// stopped the run; missing the servers it says offered no tool,
	// redefined those an mcp.json entry stood in for; hidden why pi leaves
	// their tools out of its requests; settled and aborted agent_settled's.
	marked, armed    bool
	capped           int
	missing          []string
	redefined        []string
	hidden           string
	settled, aborted bool
	overflowFailed   string
	requests         int
	// retrying is whether the last request ended in a failed attempt, which
	// the next assistant message retries ([piEvents.counted]).
	retrying bool

	spent  piUsage
	last   int64
	window int64

	calls map[string]int
	seq   int

	transcript transcriptLines
	following  bool
}

var (
	_ runSpender     = (*piEvents)(nil)
	_ contextReader  = (*piEvents)(nil)
	_ windowReporter = (*piEvents)(nil)
)

// Entries implements [Decoder].
func (d *piEvents) Entries(end bool) []string {
	d.following = true
	d.answer = ""
	return d.transcript.take(end)
}

// Skipped implements [Decoder].
func (d *piEvents) Skipped(n int64, bound int) { d.transcript.skip(n, bound) }

// Line implements [Decoder].
func (d *piEvents) Line(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	d.sawLine = true
	ev, ok := decodePiLine(line)
	if !ok {
		return
	}
	d.sawEvent = true
	switch ev.Type {
	case "session":
		d.sessionID = firstNonBlank(ev.ID, d.sessionID)
	case piExtensionMark:
		d.marked = true
	case "message_end":
		if ev.Message != nil && ev.Message.Role == "assistant" {
			d.assistant(*ev.Message)
		}
	case "tool_execution_start":
		d.started(ev)
	case "tool_execution_end":
		d.completed(ev)
	case "entry_appended":
		if ev.Entry != nil {
			d.entry(*ev.Entry)
		}
	case "auto_retry_start":
		d.transcript.add(activityline.Words(activityline.TagError, fmt.Sprintf("retrying (attempt %d of %d): %s",
			ev.Attempt, ev.MaxAttempts, firstLine(ev.ErrorMessage, transcriptDetailLimit))))
	case "compaction_end":
		d.compacted(ev)
	case "agent_settled":
		d.settled, d.aborted = true, ev.Aborted
	}
}

// assistant reads one assistant message: its answer, its stop, its failure and
// its usage.
func (d *piEvents) assistant(m piMessage) {
	d.counted(m.StopReason)
	if m.Usage != nil {
		u := *m.Usage
		d.spent.Input += u.Input
		d.spent.Output += u.Output
		d.spent.CacheRead += u.CacheRead
		d.spent.CacheWrite += u.CacheWrite
		if whole := int64(u.Input + u.CacheRead + u.CacheWrite); whole > 0 {
			d.last = whole
		}
	}
	d.stop = m.StopReason
	text := piText(m.Content)
	if !d.following {
		d.answer = text
	}
	if strings.TrimSpace(text) != "" {
		d.transcript.add(text)
	}
	d.failure = ""
	if m.StopReason == "error" {
		d.failure = firstNonBlank(m.ErrorMessage, "the model's request failed")
		d.transcript.add(activityline.Words(activityline.TagError, firstLine(d.failure, transcriptDetailLimit)))
	}
}

// entry reads one entry the run appended: the extension's own, and a note it
// handed over.
func (d *piEvents) entry(e piEntry) {
	switch {
	case e.Type == "custom" && e.CustomType == piArmedType:
		d.armed = true
	case e.Type == "custom" && e.CustomType == piCapType:
		d.capped = max(e.data().Turns, 1)
	case e.Type == "custom" && e.CustomType == piBridgeType:
		d.missing, d.redefined, d.hidden = e.data().Missing, e.data().Redefined, e.data().Hidden
	case e.Type == "custom" && e.CustomType == piContextType:
		if w := e.data().ContextWindow; w > 0 && (d.window == 0 || w < d.window) {
			d.window = w
		}
	case e.Type == "custom_message" && e.CustomType == piNoteType:
		var text string
		if json.Unmarshal(e.Content, &text) != nil {
			text = piText(e.Content)
		}
		d.transcript.add(activityline.Words(activityline.TagSteer, firstLine(text, transcriptDetailLimit)))
	}
}

// compacted reads a compaction's end: its own request's usage, and a failure
// recovering from a conversation past the model's window.
func (d *piEvents) compacted(ev piLine) {
	var result struct {
		Usage *piUsage `json:"usage"`
	}
	if json.Unmarshal(ev.Result, &result) == nil && result.Usage != nil {
		d.spent.Input += result.Usage.Input
		d.spent.Output += result.Usage.Output
		d.spent.CacheRead += result.Usage.CacheRead
		d.spent.CacheWrite += result.Usage.CacheWrite
	}
	if ev.ErrorMessage != "" && !ev.Aborted && ev.Reason == "overflow" {
		d.overflowFailed = ev.ErrorMessage
	}
	if len(ev.Result) > 0 && string(ev.Result) != "null" {
		d.transcript.add(activityline.Words(activityline.TagNote, "the conversation was compacted"))
	}
}

// started notes a call's start and adds its line.
func (d *piEvents) started(ev piLine) {
	d.seq++
	if d.calls == nil {
		d.calls = map[string]int{}
	}
	if len(d.calls) < maxPendingCalls {
		d.calls[ev.ToolCallID] = d.seq
	}
	for _, l := range activityline.Call(d.seq, firstNonBlank(ev.ToolName, "tool"), piSubject(ev.Args)) {
		d.transcript.add(l)
	}
}

// completed adds a call's end.
func (d *piEvents) completed(ev piLine) {
	seq, ok := d.calls[ev.ToolCallID]
	delete(d.calls, ev.ToolCallID)
	if !ok {
		d.seq++
		seq = d.seq
	}
	if !ev.IsError {
		d.transcript.add(activityline.OK(seq, activityline.Took{}))
		return
	}
	var result struct {
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(ev.Result, &result)
	d.transcript.add(activityline.Failed(seq, firstNonBlank(firstLine(piText(result.Content), transcriptDetailLimit),
		"failed"), activityline.Took{}))
}

// piSubject is a call's input as a transcript names it: the field that says
// what it acts on, else the input as compact JSON.
func piSubject(raw json.RawMessage) string {
	var input struct {
		Command string `json:"command"`
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		URL     string `json:"url"`
	}
	_ = json.Unmarshal(raw, &input)
	if subject := firstNonBlank(input.Command, input.Path, input.Pattern, input.URL); subject != "" {
		return subject
	}
	return compactInput(raw)
}

// runSpend is the run's own spend ([runSpender]): every request the stream
// shows this process making, each assistant message's usage and each
// compaction's, stated wherever a request was read.
func (d *piEvents) runSpend() (runSpend, bool) {
	if !d.sawEvent {
		return runSpend{}, false
	}
	u := d.spent
	return runSpend{input: u.Input, cacheRead: u.CacheRead, cacheWrite: u.CacheWrite, output: u.Output}, true
}

// request is the newest request's whole prompt ([contextReader]): the last
// assistant message whose usage states one.
func (d *piEvents) request() contextRead {
	if d.last > 0 {
		return contextRead{measure: sandbox.RequestMeasure{Tokens: d.last, Own: true}}
	}
	return contextRead{why: "the stream reports no request with a usage: the run made none"}
}

// reportedWindow is the least model window pi reported holding the run to
// ([windowReporter]): the extension's context entries, from pi's own
// getContextUsage().
func (d *piEvents) reportedWindow() int64 { return d.window }

// Ended implements [Decoder]: `agent_settled`, which pi prints as its run
// ends, aborted or not.
func (d *piEvents) Ended() bool { return d.settled }

// Requests implements [Decoder]: the assistant messages the stream holds, one
// a request, but for one aborted before it was made, and a failed attempt and
// the retries after it counted once, as the one request they are.
func (d *piEvents) Requests() int { return d.requests }

// InFlight implements [Decoder]: none, as the extension aborts the turn
// before its request (measured on 1.6.0: a cap of 3, 3 requests).
func (d *piEvents) InFlight() int { return 0 }

// counted counts the request one assistant message ended. PI WRITES EVERY
// FAILED ATTEMPT AS A MESSAGE OF ITS OWN (`stopReason: "error"`), then retries
// it: measured on 1.6.0, a 429 and a 500 each retried once wrote 8 messages
// for 6 requests, and a 429 retried until pi gave up wrote 4 for none answered.
// An attempt the vendor refused bills nothing, so the attempts and the answer
// that ends them are one request, counted at the first.
func (d *piEvents) counted(stop string) {
	switch {
	case stop == "aborted":
	case stop == "error":
		if !d.retrying {
			d.requests++
		}
		d.retrying = true
	case d.retrying:
		d.retrying = false
	default:
		d.requests++
	}
}

// unheld implements [enforcerReporter]: a run its script marked as loading the
// extension, whose extension never said it ran.
func (d *piEvents) unheld() string {
	if !d.marked || d.armed {
		return ""
	}
	return "the run's extension never ran, so the seat's tools, its notes and its turn cap were not there: " +
		(Pi{}).notesOffWhy()
}

// Result implements [Decoder]: the answer, and how the run ended.
//
// A RUN WHOSE EXTENSION NEVER RAN fails whatever it answered: its script printed
// the mark ([piExtensionMark]) and its stream lacks the extension's word, so the
// seat's tools, its notes and its cap were never there. So does one whose
// requests number more than its cap. PI EXITS 0 ON A MODEL'S ERROR in JSON mode,
// so the stream, never the exit code, says a request failed: its last assistant
// message's `error`.
func (d *piEvents) Result() sandbox.Result {
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
	switch {
	case d.unheld() != "":
		res.Error = d.unheld()
	case len(d.redefined) > 0:
		res.Error = fmt.Sprintf("the seat's tools never reached this run: %s, which pi uses in place of the "+
			"server the run registers under that name; rename that entry", strings.Join(d.redefined, "; "))
	case len(d.missing) > 0:
		res.Error = fmt.Sprintf("the seat's tools never reached this run: no tool of %s was offered, so its "+
			"MCP server was unreachable or refused the run's token", strings.Join(d.missing, ", "))
	case d.hidden != "":
		res.Error = "the seat's tools never reached the model: " + d.hidden
	case d.capped > 0 && d.aborted:
		res.Error = fmt.Sprintf("this run reached its turn cap of %d model turns and was stopped", d.capped)
		res.RoundCapped = true
	case d.overflowFailed != "":
		res.Error = "the conversation outgrew the model's context and pi could not compact it: " + d.overflowFailed
		res.ContextExhausted = true
	case d.failure != "":
		res.Error = d.failure
	case !d.settled:
		res.Error = "the harness stopped before it reported how its run ended"
	case d.aborted || d.stop == "aborted":
		res.Error = "the run was aborted before it finished"
	case text == "":
		res.Error = "the harness produced no answer"
	default:
		res.Success = true
	}
	return res
}
