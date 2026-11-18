package assistant

import (
	"path"
	"strings"

	"github.com/crewlet/crewlet/internal/activityline"
)

// A deep run's transcript ([activityline]) in a person's words: no host path, no
// tool's own name, no call to Crewlet (the run's step lists those), a failure
// named by its call. The run's report has its paths cut the same way.

// crewletToolPrefix opens the name the harness gives a tool of the engine's own
// MCP server.
const crewletToolPrefix = "mcp__crewlet__"

// toolVerbs are the plain words for the harnesses' own tools, by the tool's
// name in lower case. A tool that works on one file names it.
var toolVerbs = map[string]struct {
	verb     string
	namesOne bool
}{
	"bash":         {"Ran a command", false},
	"shell":        {"Ran a command", false},
	"read":         {"Read", true},
	"write":        {"Wrote", true},
	"edit":         {"Edited", true},
	"multiedit":    {"Edited", true},
	"notebookedit": {"Edited", true},
	"glob":         {"Looked through its files", false},
	"grep":         {"Searched its files", false},
	"list":         {"Looked through its files", false},
	"ls":           {"Looked through its files", false},
	"websearch":    {"Searched the web", false},
	"webfetch":     {"Read a web page", false},
	"todowrite":    {"Updated its plan", false},
	"task":         {"Started a helper for part of the work", false},
	"toolsearch":   {"Looked up a tool", false},
}

// personLines puts one run's transcript in a person's words a line at a time,
// keeping each call in flight so its failure names it.
type personLines struct {
	// calls is each call in flight by its number: what it was shown as, ""
	// for one a person is not shown.
	calls map[int]string
}

// maxCallsInFlight bounds the calls remembered: more than a harness runs at
// once. A failure of one not remembered is said without its verb.
const maxCallsInFlight = 64

// line is one transcript line as a person reads it, and false for a line a
// person is not shown.
func (p *personLines) line(raw string) (string, bool) {
	l := activityline.Parse(strings.TrimSpace(raw))
	switch l.Tag {
	case activityline.Say, activityline.TagNote:
		return withoutPaths(l.Text), strings.TrimSpace(l.Text) != ""
	case activityline.TagSteer:
		return "Read a note: " + withoutPaths(l.Text), true
	case activityline.TagError:
		return "The run hit an error.", true
	case activityline.TagCall:
		said := callWords(l.Tool, l.Text)
		if p.calls == nil {
			p.calls = map[int]string{}
		}
		if len(p.calls) < maxCallsInFlight {
			p.calls[l.Seq] = said
		}
		return said, said != ""
	case activityline.TagFailed:
		said, known := p.calls[l.Seq]
		delete(p.calls, l.Seq)
		switch {
		case !known:
			return "A step failed.", true
		case said == "":
			return "", false
		}
		return said + ", which failed", true
	case activityline.TagOK:
		delete(p.calls, l.Seq)
	}
	return "", false
}

// callWords is a call in a person's words, "" for a call to Crewlet.
func callWords(name, subject string) string {
	if strings.HasPrefix(name, crewletToolPrefix) {
		return ""
	}
	v, known := toolVerbs[strings.ToLower(name)]
	if !known {
		return "Used a tool"
	}
	if !v.namesOne {
		return v.verb
	}
	if file := baseOf(subject); file != "" {
		return v.verb + " " + file
	}
	return v.verb + " a file"
}

// baseOf is the name of the file a path names, or "" for no path.
func baseOf(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || strings.ContainsAny(p, " \t") {
		return ""
	}
	base := path.Base(p)
	if base == "." || base == "/" {
		return ""
	}
	return base
}

// withoutPaths is text with every absolute file path (one starting at "/" or
// "~/" and naming at least one directory) cut to the file's name, and a path
// to a directory left out. A URL is left whole: its path follows a host.
func withoutPaths(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		start, ok := pathStart(text, i)
		if !ok {
			out.WriteByte(text[i])
			i++
			continue
		}
		end := start
		for end < len(text) && pathByte(text[end]) {
			end++
		}
		token := text[start:end]
		// A SENTENCE'S FULL STOP is not part of the path.
		trail := ""
		for strings.HasSuffix(token, ".") || strings.HasSuffix(token, ",") {
			trail = token[len(token)-1:] + trail
			token = token[:len(token)-1]
		}
		if strings.Count(strings.TrimPrefix(token, "~"), "/") < 2 {
			out.WriteString(text[i:end])
			i = end
			continue
		}
		out.WriteString(text[i:start])
		if base := path.Base(token); !strings.HasSuffix(token, "/") && base != "/" {
			out.WriteString(base)
		}
		out.WriteString(trail)
		i = end
	}
	return out.String()
}

// pathStart reports whether an absolute path starts at text[i]: a "/" or "~/"
// that does not follow a word character, a colon or another slash (which a
// URL's path, a ratio or a relative path does).
func pathStart(text string, i int) (int, bool) {
	c := text[i]
	home := c == '~' && i+1 < len(text) && text[i+1] == '/'
	if c != '/' && !home {
		return 0, false
	}
	if i > 0 {
		prev := text[i-1]
		if prev == ':' || prev == '/' || prev == '.' || pathWordByte(prev) {
			return 0, false
		}
	}
	return i, true
}

// pathByte reports a byte a path is made of.
func pathByte(c byte) bool {
	return pathWordByte(c) || strings.IndexByte("/._-~@+%", c) >= 0 || c >= 0x80
}

func pathWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
