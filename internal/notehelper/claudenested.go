package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// THE CLAUDE CODE NESTED-INSTRUCTIONS ADAPTER (-adapter claude-nested), a
// one-shot one: the run's PostToolUse hook on a file tool runs the helper once
// per call, its input the hook's JSON on stdin and -config the run's index of
// its roots, NUL-separated. It answers, as the hook's additionalContext, what
// a native session loads on first working under a directory and a run with no
// settings source loads nowhere (the package doc's "Nested instructions").

// nestedInputBytes bounds the hook's input read: a PostToolUse carries the
// call's output, whose size is the tool's.
const nestedInputBytes = 64 << 20

// instructionBytes is the largest instruction file handed, Claude Code's own
// limit on one (4 MiB, measured on 2.1.330); a larger one is left out.
const instructionBytes = 4 << 20

// importDepth is how deep imports are followed, Claude Code's own.
const importDepth = 5

// claudeHookInput is the part of a PostToolUse hook's input the adapter reads.
type claudeHookInput struct {
	Event     string `json:"hook_event_name"`
	Cwd       string `json:"cwd"`
	ToolInput struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
	} `json:"tool_input"`
}

// claudeHookAnswer is a PostToolUse hook's answer.
type claudeHookAnswer struct {
	Output struct {
		Event   string `json:"hookEventName"`
		Context string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// runClaudeNested is one firing: the answer on stdout, or nothing. Every
// failure exits 0 with nothing on stdout, so the harness goes on as if the
// file had no instructions near it.
func runClaudeNested(_ context.Context, f flags, stdin io.Reader, stdout, stderr io.Writer,
	_ func(string) string) int {

	logger := log.New(stderr, "notehelper: ", 0)
	raw, err := io.ReadAll(io.LimitReader(stdin, nestedInputBytes))
	if err != nil {
		logger.Print("the hook's input could not be read")
		return exitDone
	}
	var in claudeHookInput
	if json.Unmarshal(raw, &in) != nil {
		logger.Print("the hook's input is not the JSON object Claude Code hands a hook")
		return exitDone
	}
	file := in.ToolInput.FilePath
	if file == "" {
		file = in.ToolInput.NotebookPath
	}
	if in.Event != "PostToolUse" || file == "" {
		return exitDone
	}
	if !filepath.IsAbs(file) {
		if !filepath.IsAbs(in.Cwd) {
			return exitDone
		}
		file = filepath.Join(in.Cwd, file)
	}
	index, err := os.ReadFile(f.config)
	if err != nil {
		logger.Printf("the run's index of its roots could not be read: %v", err)
		return exitDone
	}
	n := nestedRun{claims: filepath.Join(filepath.Dir(f.config), "seen"), log: logger}
	for _, r := range strings.Split(string(index), "\x00") {
		if r != "" && filepath.IsAbs(r) {
			n.roots = append(n.roots, filepath.Clean(r))
		}
	}
	text := n.handed(filepath.Clean(file))
	if text == "" {
		return exitDone
	}
	var answer claudeHookAnswer
	answer.Output.Event, answer.Output.Context = "PostToolUse", text
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(answer)
	return exitDone
}

// nestedRun is one firing's view of the run: its roots, its claims directory,
// and the text it hands.
type nestedRun struct {
	roots    []string
	claims   string
	log      *log.Logger
	out      strings.Builder
	resolved []string
}

// handed is what a firing on file hands: nothing for a file under no root.
func (n *nestedRun) handed(file string) string {
	top := ""
	for _, r := range n.roots {
		if under(file, r) && (top == "" || len(r) < len(top)) {
			top = r
		}
	}
	if top == "" {
		return ""
	}
	for _, r := range n.roots {
		if resolved, err := filepath.EvalSymlinks(r); err == nil {
			n.resolved = append(n.resolved, resolved)
		}
	}
	dirs := []string{filepath.Dir(file)}
	for d := dirs[0]; d != top && under(d, top); {
		d = filepath.Dir(d)
		dirs = append(dirs, d)
	}
	slices.Reverse(dirs)
	for _, d := range dirs {
		if !slices.Contains(n.roots, d) {
			for _, name := range []string{"CLAUDE.md", ".claude/CLAUDE.md", "CLAUDE.local.md"} {
				n.hand(filepath.Join(d, filepath.FromSlash(name)), 0)
			}
		}
		n.rules(d, file, !slices.Contains(n.roots, d))
	}
	return n.out.String()
}

// rules hands base's .claude/rules (recursively, each Markdown file): those
// whose `paths` match file, relative to base, and, with unscoped, those that
// name none.
func (n *nestedRun) rules(base, file string, unscoped bool) {
	dir := filepath.Join(base, ".claude", "rules")
	rel := filepath.ToSlash(strings.TrimPrefix(file, base+string(filepath.Separator)))
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an entry the walk cannot read hands nothing, and the rest still do
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		raw, _, ok := n.read(path)
		if !ok {
			return nil
		}
		front, _ := splitFrontmatter(raw)
		patterns := rulePatterns(pathsOf(front))
		if patterns == nil && unscoped || patterns != nil && newPathMatcher(patterns).matches(rel) {
			n.hand(path, 0)
		}
		return nil
	})
}

// hand adds path, if it is an instruction file not yet handed this run, and
// then its imports.
func (n *nestedRun) hand(path string, depth int) {
	raw, resolved, ok := n.read(path)
	if !ok {
		return
	}
	_, body := splitFrontmatter(raw)
	body = cutBlockComments(body)
	if strings.TrimSpace(body) == "" || !n.claim(resolved) {
		return
	}
	n.out.WriteString("Contents of " + path + ":\n\n" + body + "\n\n")
	if depth >= importDepth {
		return
	}
	for _, imp := range importsOf(body, filepath.Dir(path)) {
		n.hand(imp, depth+1)
	}
}

// read is path's text and its path with links resolved, where it is a regular
// file no larger than [instructionBytes] lying under a root: a repository's
// link out of it hands nothing, as a native session follows no import out of
// its project.
func (n *nestedRun) read(path string) (text, resolved string, ok bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !slices.ContainsFunc(n.resolved, func(r string) bool { return under(resolved, r) }) {
		return "", "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() > instructionBytes {
		return "", "", false
	}
	raw, err := os.ReadFile(resolved) //nolint:gosec // a root's file, which the run's own model can read too
	if err != nil {
		return "", "", false
	}
	return string(raw), resolved, true
}

// claim reports whether this firing is the first of the run to hand the file
// at path (links resolved): it creates the file's claim, which no later
// firing can.
func (n *nestedRun) claim(path string) bool {
	sum := sha256.Sum256([]byte(path))
	f, err := os.OpenFile(filepath.Join(n.claims, hex.EncodeToString(sum[:])), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if !errors.Is(err, fs.ErrExist) {
			n.log.Printf("%s was not handed, since its claim could not be made: %v", path, err)
		}
		return false
	}
	_ = f.Close()
	return true
}

// under reports whether path lies strictly under dir.
func under(path, dir string) bool {
	return strings.HasPrefix(path, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// splitFrontmatter is s's frontmatter lines, between a first line `---` and
// the next, and the text after it; s whole where it opens none.
func splitFrontmatter(s string) (front []string, body string) {
	lines := strings.SplitAfter(s, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " \t\r\n") != "---" {
		return nil, s
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], " \t\r\n") == "---" {
			for _, l := range lines[1:i] {
				front = append(front, strings.TrimRight(l, "\r\n"))
			}
			return front, strings.Join(lines[i+1:], "")
		}
	}
	return nil, s
}

// pathsOf is the `paths` key of a frontmatter's lines: a scalar, a flow list
// or a block list, each item unquoted. Nil where there is none, or where
// Claude Code's YAML parse would fail (a plain item opening with an alias,
// anchor, tag or flow-mapping indicator), which leaves the rule unscoped.
func pathsOf(front []string) []string {
	for i, line := range front {
		rest, ok := strings.CutPrefix(line, "paths:")
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		switch {
		case rest == "":
			var out []string
			for _, item := range front[i+1:] {
				t := strings.TrimSpace(item)
				if t == "" || strings.HasPrefix(t, "#") {
					continue
				}
				v, ok := strings.CutPrefix(t, "-")
				if !ok || (item[0] != ' ' && item[0] != '\t' && item[0] != '-') {
					break
				}
				s, ok := yamlScalar(strings.TrimSpace(v))
				if !ok {
					return nil
				}
				out = append(out, s)
			}
			return out
		case strings.HasPrefix(rest, "["):
			end := strings.LastIndex(rest, "]")
			if end < 0 {
				return nil
			}
			var out []string
			for _, item := range splitFlow(rest[1:end]) {
				s, ok := yamlScalar(item)
				if !ok {
					return nil
				}
				out = append(out, s)
			}
			return out
		default:
			s, ok := yamlScalar(rest)
			if !ok {
				return nil
			}
			return []string{s}
		}
	}
	return nil
}

// splitFlow is a flow list's items: cut at each comma outside quotes, each
// trimmed, the empty dropped.
func splitFlow(s string) []string {
	var out []string
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ',':
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return slices.DeleteFunc(out, func(v string) bool { return v == "" })
}

// yamlScalar is one YAML scalar's string: quoted ones unquoted, a plain one
// with its comment cut; false for a plain one YAML reads as no string.
func yamlScalar(v string) (string, bool) {
	switch {
	case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
		var s string
		if json.Unmarshal([]byte(v), &s) != nil {
			return "", false
		}
		return s, true
	case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'"), true
	case v == "" || strings.ContainsRune("*&!{[@`|>%", rune(v[0])):
		return "", false
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v, true
}

// blockComment is an HTML comment opening a line, which Claude Code leaves
// out of an instruction file's text.
var blockComment = regexp.MustCompile(`(?m)^ {0,3}<!--[\s\S]*?-->[ \t]*\n?`)

// cutBlockComments is s without its block-level HTML comments.
func cutBlockComments(s string) string { return blockComment.ReplaceAllString(s, "") }

// importRef is an import: `@` and a path, at a line's start or after a space.
var importRef = regexp.MustCompile(`(?:^|\s)@((?:[^\s\\]|\\ )+)`)

// importsOf is body's imports as paths, relative ones resolved from dir and
// `~/` from the home: outside code blocks, code spans and comments, a
// `#fragment` cut, and only a path Claude Code reads as one (opening with
// `./`, `~/` or `/`, or a letter, digit, `.`, `_` or `-`) to a text file.
func importsOf(body, dir string) []string {
	var out []string
	for _, text := range proseOf(body) {
		for _, m := range importRef.FindAllStringSubmatch(text, -1) {
			p, _, _ := strings.Cut(m[1], "#")
			p = strings.ReplaceAll(p, `\ `, " ")
			if p == "" || p == "/" || !importable(p) || !textExtension(p) {
				continue
			}
			switch {
			case strings.HasPrefix(p, "~/"):
				home, err := os.UserHomeDir()
				if err != nil {
					continue
				}
				p = filepath.Join(home, p[2:])
			case !filepath.IsAbs(p):
				p = filepath.Join(dir, p)
			}
			if !slices.Contains(out, filepath.Clean(p)) {
				out = append(out, filepath.Clean(p))
			}
		}
	}
	return out
}

// importable reports whether p reads as a path to Claude Code.
func importable(p string) bool {
	if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, "/") {
		return true
	}
	c := p[0]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
}

// textExtensions are the extensions Claude Code imports (measured on 2.1.330):
// a file with another is left out, one with none is read.
var textExtensions = strings.Fields(`.md .txt .text .json .yaml .yml .toml .xml .csv .html .htm .css .scss
	.sass .less .js .ts .tsx .jsx .mjs .cjs .mts .cts .py .pyi .pyw .rb .erb .rake .go .rs .java .kt .kts .scala
	.c .cpp .cc .cxx .h .hpp .hxx .cs .swift .sh .bash .zsh .fish .ps1 .bat .cmd .env .ini .cfg .conf .config
	.properties .sql .graphql .gql .proto .vue .svelte .astro .ejs .hbs .pug .jade .php .pl .pm .lua .r .dart
	.ex .exs .erl .hrl .clj .cljs .cljc .edn .hs .lhs .elm .ml .mli .f .f90 .f95 .for .cmake .make .makefile
	.gradle .sbt .rst .adoc .asciidoc .org .tex .latex .lock .log .diff .patch`)

// textExtension reports whether p's extension is one Claude Code imports.
func textExtension(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	return ext == "" || slices.Contains(textExtensions, ext)
}

// codeFence opens or closes a fenced code block.
var codeFence = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")

// inlineComment is an HTML comment anywhere in a line.
var inlineComment = regexp.MustCompile(`<!--[\s\S]*?-->`)

// proseOf is body's text outside fenced code blocks, with its code spans and
// HTML comments blanked: where an import may be written.
func proseOf(body string) []string {
	body = inlineComment.ReplaceAllString(body, " ")
	var out []string
	open := ""
	for _, line := range strings.Split(body, "\n") {
		if m := codeFence.FindStringSubmatch(line); m != nil {
			switch {
			case open == "":
				open = m[1]
				continue
			case m[1][0] == open[0] && len(m[1]) >= len(open):
				open = ""
				continue
			}
		}
		if open == "" {
			out = append(out, blankCodeSpans(line))
		}
	}
	return out
}

// blankCodeSpans is line with each code span, a run of backticks to the next
// run of as many, turned to a space.
func blankCodeSpans(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '`' {
			b.WriteByte(line[i])
			i++
			continue
		}
		n := 0
		for i+n < len(line) && line[i+n] == '`' {
			n++
		}
		closeAt := -1
		for j := i + n; j < len(line); {
			if line[j] != '`' {
				j++
				continue
			}
			m := 0
			for j+m < len(line) && line[j+m] == '`' {
				m++
			}
			if m == n {
				closeAt = j + m
				break
			}
			j += m
		}
		if closeAt < 0 {
			b.WriteString(line[i : i+n])
			i += n
			continue
		}
		b.WriteByte(' ')
		i = closeAt
	}
	return b.String()
}
