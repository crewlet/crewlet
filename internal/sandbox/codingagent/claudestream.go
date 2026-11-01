package codingagent

import (
	"bytes"
	"encoding/json"
	"errors"
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
// THE TRANSCRIPT IS OPENCODE'S SHAPE: what the agent said, one line per tool
// call naming the tool and the first line of what it ran, and a failed call's
// first line of error. NEVER a successful tool result's body — that is a whole
// file read, or everything a command printed, and it is what the transcript is
// a summary of; a stream carries it twice over (the tool_result block, and a
// `tool_use_result` copy beside it), and echoing it would grow the record and
// the live view by orders of magnitude and push far more of somebody's files
// through redaction. Thinking is not shown either: it is the model's working,
// not what the run did. A message type this build does not know is skipped,
// because a CLI release adds them (the SDK's union has dozens).
//
// ONE SYSTEM LINE IS SHOWN: the line the run's own hooks print when they hand
// its model a person's note ([ClaudeHookAnswer]'s transcript line), which the
// CLI streams as `system/informational`, its content `<event> says: <line>`
// (measured on 2.1.263 and 2.1.310), and which becomes a `[steer]` line — the
// one place a person watching the run sees the note arrive. Every other system
// line — the session's init, a retry, a hook's own start and end — is skipped,
// as before.

// claudeEvent is the part of one stream message this decoder reads: the
// result message's fields, which the last line carries, and a message's
// content, which every assistant and user line does.
type claudeEvent struct {
	claudeResult
	// Content is a system line's text — an informational line's, which is
	// the only one read ([claudeSteerLine]). Raw, because a system subtype
	// this build does not know may carry anything there.
	Content json.RawMessage `json:"content"`
	Message struct {
		// Content is an array of blocks on an assistant message and on a
		// tool result's user message, and a plain string on a user
		// message that is a prompt (a subagent's first message).
		Content json.RawMessage `json:"content"`
		// ID and Usage are an assistant message's, which the stream repeats
		// on every line of the one message (a line per content block), so
		// a message's usage is counted once, by its id.
		ID    string      `json:"id"`
		Usage claudeUsage `json:"usage"`
	} `json:"message"`
}

// claudeUsage is one API call's tokens as the stream reports them: the
// uncached prompt, the cache's reads and writes, and the reply.
type claudeUsage struct {
	Input       int `json:"input_tokens"`
	Output      int `json:"output_tokens"`
	CacheRead   int `json:"cache_read_input_tokens"`
	CacheCreate int `json:"cache_creation_input_tokens"`
}

// total is every token of the call, the whole prompt and the reply: the
// convention every engine meter charges (llm.Completion.TotalTokens).
func (u claudeUsage) total() int { return u.Input + u.Output + u.CacheRead + u.CacheCreate }

// claudeBlock is one content block of a message.
type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// A tool_use block.
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input claudeToolInput `json:"input"`

	// A tool_result block. Content is the result's text as a string or as
	// text blocks; it is only ever read for a failed call.
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// claudeToolInput is what a transcript line names of a tool call: the field
// each of the CLI's tools takes its subject in — Bash its command, Read, Edit
// and Write their file, NotebookEdit its notebook, Grep and Glob their
// pattern, WebFetch its URL, WebSearch its query, Task its description.
type claudeToolInput struct {
	Command      string `json:"command"`
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Pattern      string `json:"pattern"`
	URL          string `json:"url"`
	Query        string `json:"query"`
	Path         string `json:"path"`
	Description  string `json:"description"`
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
	// ([claudeResult.SinceLastResult]) added up, and stated whether any
	// result carried them ([claudeEvents.runSpend]).
	own    runSpend
	stated bool
}

// runSpend is the run's own spend, summed over every result its stream
// carried, and whether any carried the per-run figures (2.1.310 and later).
// A run can print several results — a person's note handed to it as it
// finished carries its loop on to another — and each states only the calls
// since the one before, so the sum is the whole run's.
func (d *claudeEvents) runSpend() (runSpend, bool) { return d.own, d.stated }

// state adds one result's per-run figures, model by model in name order, so
// the price is summed in one order on every read.
func (d *claudeEvents) state(models map[string]claudeModelUsage) {
	d.stated = true
	for _, name := range slices.Sorted(maps.Keys(models)) {
		m := models[name]
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
		// Not an event: something the CLI printed as text (a banner, a
		// crash), which is part of what the run said.
		d.transcript.add(string(line))
		return
	}
	var ev claudeEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			// A partial object: the stream read mid-write. The next read
			// has it whole.
			return
		}
	}
	switch ev.Type {
	case "system":
		if line, ok := claudeSteerLine(ev.Subtype, ev.Content); ok {
			d.transcript.add(line)
		}
	case "assistant":
		d.count(ev.Message.ID, ev.Message.Usage)
		for _, block := range claudeBlocks(ev.Message.Content) {
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
		}
	case "user":
		for _, block := range claudeBlocks(ev.Message.Content) {
			if block.Type != "tool_result" {
				continue
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
		}
	case "result":
		// A run that succeeded ends on its final text, which the stream
		// already carried as the assistant's last message; one that did not
		// says how, as [ClaudeCode.Parse] reads the same line.
		if !ev.succeeded() {
			d.transcript.add("[error] the run ended: " + firstLine(ev.failure(), transcriptDetailLimit))
		}
		// PRESENT, EVEN EMPTY, IS STATED: a result with no calls since the
		// one before says the run spent nothing more, which is an answer.
		if ev.SinceLastResult != nil {
			d.state(ev.SinceLastResult)
		}
	}
}

func (d *claudeEvents) Skipped(n int64, bound int) { d.transcript.skip(n, bound) }

// Entries implements [Decoder]. Nothing else here grows with the stream: the
// pending calls are forgotten as their results arrive.
func (d *claudeEvents) Entries() []string { return d.transcript.take() }

func (d *claudeEvents) Result() sandbox.Result {
	return sandbox.Result{Transcript: d.transcript.String()}
}

// claudeSteerPrefix is how the CLI opens the informational line a hook of the
// run's own prints — the event that fired it and the line's own opening
// ([ClaudeHookAnswer]) — and which events those are.
var claudeSteerPrefix = regexp.MustCompile(`^(?:` + claudeEventToolBatch + `|` +
	claudeEventFinish + `) says: crewlet: `)

// claudeSteerLine is the transcript line for a system message of subtype, its
// content raw: `[steer]` and what the run's own hook said, for the line it
// prints as it hands the model a note, and nothing for any other.
func claudeSteerLine(subtype string, raw json.RawMessage) (string, bool) {
	if subtype != "informational" {
		return "", false
	}
	var content string
	if json.Unmarshal(raw, &content) != nil {
		return "", false
	}
	loc := claudeSteerPrefix.FindStringIndex(content)
	if loc == nil {
		return "", false
	}
	said := firstLine(content[loc[1]:], transcriptDetailLimit)
	if said == "" {
		return "", false
	}
	return "[steer] " + said, true
}

// claudeBlocks is a message's content as blocks; a plain-string content (a
// prompt) has none.
func claudeBlocks(raw json.RawMessage) []claudeBlock {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		var mismatch *json.UnmarshalTypeError
		if !errors.As(err, &mismatch) {
			return nil
		}
	}
	return blocks
}

// claudeToolLine renders one tool call for the transcript: its name and the
// first line of its subject, bounded as OpenCode's are.
func claudeToolLine(block claudeBlock) string {
	name := firstNonBlank(block.Name, "tool")
	line := "[tool] " + name
	in := block.Input
	if detail := firstNonBlank(in.Command, in.FilePath, in.NotebookPath, in.Pattern,
		in.URL, in.Query, in.Path, in.Description); detail != "" {
		line += ": " + firstLine(detail, transcriptDetailLimit)
	}
	return line
}

// claudeResultText is a tool result's text, from a string or from its text
// blocks.
func claudeResultText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var texts []string
	for _, part := range parts {
		if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n")
}
