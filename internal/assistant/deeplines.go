package assistant

import (
	"path"
	"strings"
)

// WHAT A PERSON IS SHOWN OF A DEEP RUN AT WORK.
//
// A run's live reading is its coding CLI's own transcript
// (internal/sandbox/executor): every tool call as "[tool] <name>:
// <subject>", where the subject is a command or a file path on the engine's
// host ("Write: /private/tmp/.../assistant-boxes/.../outbox/report.md"), and
// every paragraph the model writes. Drawn as it is, the deep-work card showed a
// person the engine's box paths, its MCP tool names ("mcp__crewlet__api_request")
// and its CLI's internals ("ToolSearch: select:..."). So each line is put in
// the person's words here, once, before it is kept:
//
//   - a call to Crewlet is left out, because the run's step already lists it,
//     nested and in its own plain sentence ([turn.stepStart]);
//   - any other tool call is a plain verb: "Ran a command", "Wrote report.md",
//     "Searched the web", with a file named by its name alone;
//   - a paragraph keeps its words with every absolute path cut to the file's
//     name ([withoutPaths]).
//
// The run's report is cut the same way before the model reads it, so an
// answer cannot repeat a host path either.

// crewletToolPrefix opens the name the CLI gives a tool of the engine's own
// MCP server.
const crewletToolPrefix = "mcp__crewlet__"

// toolVerbs are the plain words for the coding CLIs' own tools, by the tool's
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

// personLine is one transcript line as a person reads it, and false for a line
// a person is not shown.
func personLine(line string) (string, bool) {
	line = strings.TrimSpace(line)
	switch {
	case line == "":
		return "", false
	case strings.HasPrefix(line, "[error]"):
		return "The run hit an error.", true
	case !strings.HasPrefix(line, "[tool]"):
		return withoutPaths(line), true
	}
	call := strings.TrimSpace(strings.TrimPrefix(line, "[tool]"))
	failed := false
	if head, _, cut := strings.Cut(call, " → error"); cut {
		call, failed = head, true
	}
	name, subject, _ := strings.Cut(call, ":")
	name, subject = strings.TrimSpace(name), strings.TrimSpace(subject)
	if strings.HasPrefix(name, crewletToolPrefix) {
		return "", false
	}
	said := "Used a tool"
	if v, known := toolVerbs[strings.ToLower(name)]; known {
		said = v.verb
		if v.namesOne {
			if file := baseOf(subject); file != "" {
				said += " " + file
			} else {
				said += " a file"
			}
		}
	}
	if failed {
		said += ", which failed"
	}
	return said, true
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
