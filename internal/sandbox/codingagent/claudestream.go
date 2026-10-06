package codingagent

import (
	"bytes"
	"encoding/json"
	"errors"
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

// claudeEvent is the part of one stream message this decoder reads: the
// result message's fields, which the last line carries, and a message's
// content, which every assistant and user line does.
type claudeEvent struct {
	claudeResult
	Message struct {
		// Content is an array of blocks on an assistant message and on a
		// tool result's user message, and a plain string on a user
		// message that is a prompt (a subagent's first message).
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

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

// claudeEvents decodes one read of the stream into a transcript. The result
// itself is read apart ([ClaudeCode.Parse]), from the stream's last line.
type claudeEvents struct {
	transcript transcriptLines

	// tools names a pending call by its id, so the result that comes back
	// for it can say which tool failed. A call is forgotten once its result
	// arrives, so the map holds only the calls still in flight.
	tools map[string]string
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
	case "assistant":
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
	}
}

func (d *claudeEvents) Skipped(n int64) { d.transcript.skip(n) }

func (d *claudeEvents) Result() sandbox.Result {
	return sandbox.Result{Transcript: d.transcript.String()}
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
