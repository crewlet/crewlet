// Package activityline is the grammar of an executor's activity transcript: a
// tagged line per tool call, input line and result, the agent's words untagged.
// The executor writes it and every reader parses it here; the dashboard copies [Tags].
package activityline

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// Tag opens a line of the grammar; the agent's own words carry none ([Say]).
type Tag string

const (
	// Say is a line of what the agent or its harness wrote: no tag.
	Say Tag = ""
	// TagCall is a tool call: its number in the run, its tool and a preview
	// of its subject (a command, a path, a pattern).
	TagCall Tag = "tool"
	// TagInput is one line of a call's whole subject, written where the
	// preview does not hold it.
	TagInput Tag = "input"
	// TagOK and TagFailed are a call's result, with how long it ran where
	// the harness says; a failure carries the first line of why.
	TagOK     Tag = "ok"
	TagFailed Tag = "failed"
	// TagSteer is a note a person sent the run, as its hook handed it over.
	TagSteer Tag = "steer"
	// TagError is how a run that did not succeed ended.
	TagError Tag = "error"
	// TagNote is the engine's own word about the transcript: lines it did
	// not read, or a middle the record's bound left out.
	TagNote Tag = "note"
)

// Tags is every tag, in the order the dashboard's ACTIVITY_TAGS lists them.
var Tags = []Tag{TagCall, TagInput, TagOK, TagFailed, TagSteer, TagError, TagNote}

// Valid reports whether this build writes t.
func (t Tag) Valid() bool {
	for _, known := range Tags {
		if t == known {
			return true
		}
	}
	return t == Say
}

// DetailLimit bounds a call's preview and a failure's line, in bytes, markers
// included: a readable command line.
const DetailLimit = 160

// InputLimit bounds a call's whole subject as [TagInput] lines carry it, in
// bytes: a script of some two hundred lines, so the record's 256 KiB still
// holds dozens of such calls besides the rest.
const InputLimit = 8 << 10

// Took is how long a call ran, where its harness says.
type Took struct {
	D     time.Duration
	Known bool
}

// Line is one line of the grammar, read by [Parse].
type Line struct {
	Tag Tag
	// Seq is the call's number in its run, from 1, on a call, an input and
	// a result; 0 on a result whose call is not known.
	Seq int
	// Tool is a call's tool, as its harness names it.
	Tool string
	// Text is a call's preview, an input line, a failure's first line, or
	// the words of any other line.
	Text string
	// More is a call's count of subject lines past its first.
	More int
	// Took is a result's duration.
	Took Took
}

// Call is a call's lines: its header and, where the preview does not hold the
// subject whole, the subject a line at a time within [InputLimit]. REDACTED
// ONCE, before either is cut, so the preview and the input say the same.
func Call(seq int, tool, subject string) []string {
	subject = strings.TrimSpace(redact.Secrets(subject))
	head := tag(TagCall, seq, Took{}) + " " + tool
	if subject == "" {
		return []string{head}
	}
	preview := cut(subject, DetailLimit)
	lines := []string{head + ": " + preview}
	if !strings.Contains(subject, "\n") && preview == subject {
		return lines
	}
	held := subject
	if len(held) > InputLimit {
		// WHOLE LINES: a line cut part way would read as all of it.
		held = textcut.Bytes(subject, InputLimit)
		if end := strings.LastIndexByte(held, '\n'); end > 0 {
			held = held[:end]
		} else {
			held = textcut.Within(subject, InputLimit)
		}
	}
	for _, l := range strings.Split(held, "\n") {
		lines = append(lines, tag(TagInput, seq, Took{})+" "+l)
	}
	return lines
}

// OK is a call's result line for a call that succeeded.
func OK(seq int, took Took) string { return tag(TagOK, seq, took) }

// Failed is a call's result line for a call that failed, with the first line
// of why, redacted and bounded as a preview is.
func Failed(seq int, why string, took Took) string {
	line := tag(TagFailed, seq, took)
	if why = Preview(why, DetailLimit); why != "" {
		line += " " + why
	}
	return line
}

// Words is a line of tag t and text: a steer, an error or a note.
func Words(t Tag, text string) string { return "[" + string(t) + "] " + text }

// Preview is s's first line, REDACTED WHOLE BEFORE IT IS CUT (a token cut first
// survives as a fragment no rule recognises), within limit bytes, markers
// included: "…" where the line is cut, a count of the lines after it.
func Preview(s string, limit int) string { return cut(strings.TrimSpace(redact.Secrets(s)), limit) }

func cut(s string, limit int) string {
	line, rest, more := strings.Cut(s, "\n")
	suffix := ""
	if more {
		suffix = fmt.Sprintf(" (+%d more line(s))", strings.Count(rest, "\n")+1)
	}
	if limit <= 0 || len(line)+len(suffix) <= limit {
		return line + suffix
	}
	return textcut.Within(line, max(limit-len(suffix), 0)) + suffix
}

func tag(t Tag, seq int, took Took) string {
	var b strings.Builder
	b.WriteString("[" + string(t))
	if seq > 0 {
		b.WriteString(" " + strconv.Itoa(seq))
	}
	if took.Known {
		b.WriteString(" " + strconv.FormatInt(max(took.D.Milliseconds(), 0), 10) + "ms")
	}
	b.WriteString("]")
	return b.String()
}

// Parse reads one line. A line that does not open with a known tag, well
// formed, is the agent's words, whole.
func Parse(line string) Line {
	words := Line{Tag: Say, Text: line}
	end := strings.IndexByte(line, ']')
	if !strings.HasPrefix(line, "[") || end < 0 {
		return words
	}
	fields := strings.Fields(line[1:end])
	if len(fields) == 0 || Tag(fields[0]) == Say || !Tag(fields[0]).Valid() {
		return words
	}
	out := Line{Tag: Tag(fields[0]), Text: strings.TrimPrefix(line[end+1:], " ")}
	args := fields[1:]
	if len(args) > 0 && !strings.HasSuffix(args[0], "ms") {
		n, err := strconv.Atoi(args[0])
		if err != nil || n <= 0 {
			return words
		}
		out.Seq, args = n, args[1:]
	}
	if len(args) > 0 {
		ms, err := strconv.ParseInt(strings.TrimSuffix(args[0], "ms"), 10, 64)
		if err != nil || ms < 0 {
			return words
		}
		out.Took, args = Took{D: time.Duration(ms) * time.Millisecond, Known: true}, args[1:]
	}
	if len(args) > 0 {
		return words
	}
	switch out.Tag {
	case TagSteer, TagError, TagNote:
		if out.Seq != 0 || out.Took.Known {
			return words
		}
	case TagInput:
		if out.Seq == 0 || out.Took.Known {
			return words
		}
	case TagCall:
		if out.Seq == 0 || out.Took.Known || out.Text == "" {
			return words
		}
		name, detail, _ := strings.Cut(out.Text, ": ")
		out.Tool = name
		out.Text, out.More = moreOf(detail)
	}
	return out
}

// moreOf is a preview without its "(+N more line(s))" suffix, and N.
func moreOf(detail string) (string, int) {
	const open, closing = " (+", " more line(s))"
	if !strings.HasSuffix(detail, closing) {
		return detail, 0
	}
	at := strings.LastIndex(detail, open)
	if at < 0 {
		return detail, 0
	}
	n, err := strconv.Atoi(detail[at+len(open) : len(detail)-len(closing)])
	if err != nil || n <= 0 {
		return detail, 0
	}
	return detail[:at], n
}
