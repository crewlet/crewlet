package executor_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// NO OPENCODE ANSWER IS READ THROUGH A PIPE. Into a pipe, OpenCode 2.0.32 and
// 2.1.4 stop writing an answer after a 64 KiB multiple and exit 0, while into a
// regular file every release measured writes it whole, so every answer of
// OpenCode's that a run's script or a pack's script reads goes to a file and is
// read back from there. This holds every script the runner renders to that,
// one rule with no exception: no `opencode` call inside a command
// substitution, and none in a pipeline element a `|` follows, whether the call
// is that element or sits in a group, a subshell or a compound command that is,
// or in a function called there; each finding names its line. The run's
// script is rendered for a run that starts a session and for one that
// continues one, its copy compared or replaced, steered and with MCP servers,
// so with the readiness loop, its shadow check and the notes' cancel in it, as
// well as the login's import and exports every run's script carries; and the
// pack's for a pack on demand. A scan that saw none of the script's calls
// would prove nothing, so it has to have seen every kind the scripts make.
//
// Mutation: put any one call back into `$(…)`, or the export back into
// `| gzip`, and this goes red.
func TestNoOpenCodeAnswerIsReadThroughAPipe(t *testing.T) {
	t.Parallel()
	paths := executor.Paths{Home: "/home/crewlet"}
	servers := map[string]sandbox.MCPServer{
		"crewlet": {Name: "crewlet", Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:1/mcp/x", AlwaysLoaded: true},
		"github":  {Name: "github", Command: "false"},
	}
	run := sandbox.RunRequest{MCPServers: servers, Steer: steerWiring, Model: "anthropic/claude-sonnet-5"}
	continued := run
	continued.FreshWiring = &sandbox.Wiring{MCPServers: servers}
	session := executor.Session{ID: carriedOpenCode, Continues: true, Fresh: freshOpenCode}
	replaced := session
	replaced.Replaces = true
	scripts := map[string]string{
		"a run that starts its session": scriptOf(t,
			executor.OpenCode{}.Command(run, executor.OpenCode{}.Session(run), paths, "")),
		"a run that continues its session": scriptOf(t, executor.OpenCode{}.Command(continued, session, paths, "")),
		"a run that replaces the box's copy": scriptOf(t,
			executor.OpenCode{}.Command(continued, replaced, paths, "")),
		"a pack on demand": executor.OpenCode{}.Pack(paths, carriedOpenCode,
			paths.SessionPackStaging("p1"), paths.SessionPackHeld("p1"), false),
	}
	for name, script := range scripts {
		calls, piped, err := openCodePipes(script)
		if err != nil {
			t.Fatalf("%s: the scanner could not read the script: %v\n%s", name, err, script)
		}
		for _, p := range piped {
			t.Errorf("%s reads an OpenCode answer through a pipe, %s", name, p)
		}
		want := []string{"api", "session"}
		if name != "a pack on demand" {
			want = append(want, "--version", "auth", "serve", "run")
		}
		for _, sub := range want {
			if !slices.Contains(calls, sub) {
				t.Errorf("%s: the scan saw no `opencode %s` call, so it did not read the script whole; it saw %s",
					name, sub, strings.Join(calls, ", "))
			}
		}
	}
}

// THE PIPE SCANNER SEES EVERY WAY A SCRIPT CAN HAND OPENCODE A PIPE, and none
// that writes a file: a call inside `$(…)`, inside backquotes, inside one in
// double quotes or in arithmetic, in a case or a for's words; a call a `|`
// follows, or a group, subshell or compound command around it that one
// follows, or a function called in either place, however deep; and a trap's
// body. A construct it does not read is an error, never a pass. Without each
// row, the structural test above could pass a script it misread.
func TestThePipeScannerSeesEveryWayOfReadingThroughAPipe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		script string
		piped  int
	}{
		{`x=$(opencode --version)`, 1},
		{"x=`opencode --version`", 1},
		{`w="it said: $(opencode api session.get)"`, 1},
		{`n=$(( $(opencode api session.get) + 1 ))`, 1},
		{`case "$a" in b) x=$(opencode api session.get);; esac`, 1},
		{`for i in $(opencode api session.inbox.list); do :; done`, 1},
		{`opencode session export s | gzip -c > f`, 1},
		{`/usr/local/bin/opencode session export s | gzip -c > f`, 1},
		{`{ opencode session export s; echo $? >&4; } | gzip -c > f`, 1},
		{`( opencode api session.get ) | cat`, 1},
		{`if opencode api session.get > /dev/null; then :; fi | cat`, 1},
		{"xs=$( { { opencode session export s 2> e; echo $? >&4; } | gzip -c > f; } 4>&1 )", 1},
		{`ask() { opencode api "$@"; }; v=$(ask session.get)`, 1},
		{"outer() { inner; }\ninner() { opencode api session.get; }\nouter | cat", 1},
		{`trap 'x=$(opencode api session.get)' TERM`, 1},
		{`opencode api session.get > f 2> /dev/null; s=$(cat f); rm -f f`, 0},
		{`opencode session export s > f && gzip -c < f > g`, 0},
		{`cat f | opencode session import -`, 0},
		{`command -v opencode > /dev/null || exit 127`, 0},
		{`case "$p" in a|b) opencode api session.get > f;; esac`, 0},
		{`echo 'opencode api session.get | cat'; echo "$(printf '%s' "$out" | tr '\n' ' ')"`, 0},
		{`keep() { opencode auth export > "$1"; }; keep f; [ -z "$x" ] || keep g`, 0},
	} {
		_, piped, err := openCodePipes(tc.script)
		if err != nil {
			t.Errorf("the scanner could not read %q: %v", tc.script, err)
			continue
		}
		if len(piped) != tc.piped {
			t.Errorf("in %q the scanner found %d call(s) read through a pipe, want %d: %s",
				tc.script, len(piped), tc.piped, strings.Join(piped, "; "))
		}
	}
	for _, unread := range []string{"cat <<EOF\nopencode api session.get\nEOF", `x=$(opencode api session.get`,
		`if opencode api session.get; then :;`} {
		if _, _, err := openCodePipes(unread); err == nil {
			t.Errorf("the scanner read %q without an error: a construct it does not read has to fail the scan", unread)
		}
	}
	// A FINDING NAMES ITS LINE, one in a trap's body or in backquotes counted
	// from where that sits in the script.
	for _, script := range []string{"a\nb\nx=$(opencode --version)", "a\ntrap 'b\nx=$(opencode --version)' TERM",
		"a\nx=`b\nopencode --version`"} {
		_, piped, err := openCodePipes(script)
		if err != nil || len(piped) != 1 || !strings.Contains(piped[0], "line 3:") {
			t.Errorf("in %q the scanner reported %s (%v), want one finding on line 3", script,
				strings.Join(piped, "; "), err)
		}
	}
}

// openCodePipes reads a POSIX sh script and answers the second word of every
// OpenCode call it makes (`opencode api …` is "api"), and every call whose
// output a pipe carries: inside a command substitution, or in a pipeline
// element a `|` follows, at any level of grouping. A function whose body makes
// such a call, directly or through another such function, is a call wherever
// it is called. A construct the scanner does not read is an error.
func openCodePipes(script string) (calls, piped []string, err error) {
	cmds, err := scanShell(script)
	if err != nil {
		return nil, nil, err
	}
	makes := map[string]bool{"opencode": true}
	for grew := true; grew; {
		grew = false
		for _, c := range cmds {
			if c.fn != "" && !makes[c.fn] && c.calls(makes) {
				makes[c.fn], grew = true, true
			}
		}
	}
	for _, c := range cmds {
		if at := c.opencode(); at >= 0 && at+1 < len(c.words) {
			calls = append(calls, c.words[at+1])
		}
		if !c.calls(makes) {
			continue
		}
		switch {
		case c.inSubst:
			piped = append(piped, fmt.Sprintf("inside a command substitution, line %d: %s", c.line, c.text))
		case c.piped:
			piped = append(piped, fmt.Sprintf("into a pipe, line %d: %s", c.line, c.text))
		}
	}
	return calls, piped, nil
}

// shCommand is one simple command of a script: its words, each plain one (no
// quoting, escape or expansion in it) as written and every other as "", and
// where its output goes.
type shCommand struct {
	words   []string
	text    string
	line    int    // the script's line it starts on, from 1
	inSubst bool   // inside a command substitution
	piped   bool   // in a pipeline element a `|` follows, at some level
	fn      string // the function whose body it is in, or ""
}

// opencode is where the command's first `opencode` word is, a path to one
// included, or -1.
func (c *shCommand) opencode() int {
	for i, w := range c.words {
		if w == "opencode" || strings.HasSuffix(w, "/opencode") {
			return i
		}
	}
	return -1
}

// calls reports whether any of the command's words names a command in names,
// `opencode` by a path too.
func (c *shCommand) calls(names map[string]bool) bool {
	return c.opencode() >= 0 || slices.ContainsFunc(c.words, func(w string) bool { return names[w] })
}

// shScan is a recursive-descent reading of the POSIX sh the runner renders:
// lists, and-or lists, pipelines, groups, subshells, if, while, until, for,
// case, function definitions, redirections, quoting, and command
// substitution and arithmetic nested in words. A here-document, and anything
// else it does not read, is an error.
type shScan struct {
	src   string
	line  int // the script's line src starts on
	pos   int
	subst int
	fn    []string
	cmds  []*shCommand
}

type shError string

func (e shError) Error() string { return string(e) }

func scanShell(src string) (cmds []*shCommand, err error) {
	s := &shScan{src: src, line: 1}
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(shError)
			if !ok {
				panic(r)
			}
			err = e
		}
	}()
	s.list()
	if s.blanksAndNewlines(); !s.eof() {
		s.fail("nothing ends the script's list here")
	}
	return s.cmds, nil
}

func (s *shScan) fail(format string, args ...any) {
	near := s.src[min(s.pos, len(s.src)):]
	near = near[:min(len(near), 40)]
	panic(shError(fmt.Sprintf(format, args...) + fmt.Sprintf(" (at byte %d, before %q)", s.pos, near)))
}

func (s *shScan) eof() bool          { return s.pos >= len(s.src) }
func (s *shScan) at(tok string) bool { return strings.HasPrefix(s.src[min(s.pos, len(s.src)):], tok) }

func (s *shScan) peek() byte {
	if s.eof() {
		return 0
	}
	return s.src[s.pos]
}

// lineAt is the script's line that offset i of src is on.
func (s *shScan) lineAt(i int) int {
	return s.line + strings.Count(s.src[:min(i, len(s.src))], "\n")
}

// blanks skips spaces, tabs, escaped newlines and a comment.
func (s *shScan) blanks() {
	for !s.eof() {
		switch {
		case s.peek() == ' ' || s.peek() == '\t':
			s.pos++
		case s.at("\\\n"):
			s.pos += 2
		case s.peek() == '#':
			for !s.eof() && s.peek() != '\n' {
				s.pos++
			}
		default:
			return
		}
	}
}

func (s *shScan) blanksAndNewlines() {
	for s.blanks(); s.peek() == '\n'; s.blanks() {
		s.pos++
	}
}

// shReserved are the reserved words, each only where a command could start.
var shReserved = []string{"if", "then", "elif", "else", "fi", "done", "do", "while", "until", "for", "case",
	"esac", "in", "{", "}", "!"}

// reserved is the reserved word that starts here, or "".
func (s *shScan) reserved() string {
	for _, w := range shReserved {
		if end := s.pos + len(w); s.at(w) && (end >= len(s.src) || strings.IndexByte(" \t\n;&|()<>", s.src[end]) >= 0) {
			return w
		}
	}
	return ""
}

func (s *shScan) expect(tok string) {
	if s.blanks(); !s.at(tok) {
		s.fail("want %q", tok)
	}
	s.pos += len(tok)
}

func (s *shScan) expectReserved(w string) {
	if s.blanksAndNewlines(); s.reserved() != w {
		s.fail("want %q", w)
	}
	s.pos += len(w)
}

// list reads commands up to what ends it, which it leaves for its caller: the
// end, `)`, `;;`, or a reserved word that closes a compound command.
func (s *shScan) list() []int {
	var all []int
	for {
		s.blanksAndNewlines()
		switch s.reserved() {
		case "then", "elif", "else", "fi", "do", "done", "esac", "}":
			return all
		}
		if s.eof() || s.peek() == ')' || s.at(";;") {
			return all
		}
		all = append(all, s.andOr()...)
		s.blanks()
		switch {
		case s.at(";;"):
			return all
		case s.peek() == ';', s.peek() == '\n', s.peek() == '&' && !s.at("&&"):
			s.pos++
		default:
			return all
		}
	}
}

func (s *shScan) andOr() []int {
	all := s.pipeline()
	for s.blanks(); s.at("&&") || s.at("||"); s.blanks() {
		s.pos += 2
		s.blanksAndNewlines()
		all = append(all, s.pipeline()...)
	}
	return all
}

// pipeline reads one, marking every command of each element a `|` follows as
// piped, the commands of a group, subshell or compound element included.
func (s *shScan) pipeline() []int {
	if s.blanks(); s.reserved() == "!" {
		s.pos++
	}
	elem := s.command()
	all := slices.Clone(elem)
	for s.blanks(); s.peek() == '|' && !s.at("||"); s.blanks() {
		s.pos++
		for _, i := range elem {
			s.cmds[i].piped = true
		}
		s.blanksAndNewlines()
		elem = s.command()
		all = append(all, elem...)
	}
	return all
}

func (s *shScan) command() []int {
	s.blanks()
	var cmds []int
	switch r := s.reserved(); {
	case s.peek() == '(':
		s.pos++
		cmds = s.list()
		s.expect(")")
	case r == "{":
		s.pos++
		cmds = s.list()
		s.expectReserved("}")
	case r == "if":
		cmds = s.ifClause()
	case r == "while" || r == "until":
		s.pos += len(r)
		cmds = s.list()
		s.expectReserved("do")
		cmds = append(cmds, s.list()...)
		s.expectReserved("done")
	case r == "for":
		cmds = s.forClause()
	case r == "case":
		cmds = s.caseClause()
	case r != "" && r != "!":
		s.fail("%q where a command starts", r)
	default:
		return s.simple()
	}
	s.redirects()
	return cmds
}

func (s *shScan) ifClause() []int {
	s.pos += len("if")
	all := s.list()
	s.expectReserved("then")
	all = append(all, s.list()...)
	for {
		switch s.blanksAndNewlines(); s.reserved() {
		case "elif":
			s.pos += len("elif")
			all = append(all, s.list()...)
			s.expectReserved("then")
			all = append(all, s.list()...)
		case "else":
			s.pos += len("else")
			all = append(all, s.list()...)
		case "fi":
			s.pos += len("fi")
			return all
		default:
			s.fail("an if with no fi")
		}
	}
}

func (s *shScan) forClause() []int {
	s.pos += len("for")
	s.blanks()
	s.word()
	if s.blanks(); s.reserved() == "in" {
		s.pos += len("in")
		for s.blanks(); !s.eof() && s.peek() != ';' && s.peek() != '\n'; s.blanks() {
			s.word()
		}
	}
	if s.blanks(); s.peek() == ';' {
		s.pos++
	}
	s.expectReserved("do")
	all := s.list()
	s.expectReserved("done")
	return all
}

func (s *shScan) caseClause() []int {
	s.pos += len("case")
	s.blanks()
	s.word()
	s.expectReserved("in")
	var all []int
	for {
		if s.blanksAndNewlines(); s.reserved() == "esac" {
			s.pos += len("esac")
			return all
		}
		if s.peek() == '(' {
			s.pos++
		}
		for {
			s.blanks()
			s.word()
			if s.blanks(); s.peek() != '|' {
				break
			}
			s.pos++
		}
		s.expect(")")
		all = append(all, s.list()...)
		if s.blanksAndNewlines(); s.at(";;") {
			s.pos += 2
		} else if s.reserved() != "esac" {
			s.fail("a case item with no ;;")
		}
	}
}

// simple reads a simple command, or a function definition, whose body's
// commands it records as the function's.
func (s *shScan) simple() []int {
	start := s.pos
	c := &shCommand{inSubst: s.subst > 0}
	if len(s.fn) > 0 {
		c.fn = s.fn[len(s.fn)-1]
	}
	var raws []string
	var starts []int
	redirected := false
	for {
		s.blanks()
		switch ch := s.peek(); {
		case ch == 0 || ch == '\n' || ch == ';' || ch == '&' || ch == '|' || ch == ')':
			if len(raws) == 0 && !redirected {
				s.fail("no command")
			}
			c.text = strings.TrimSpace(s.src[start:s.pos])
			c.line = s.lineAt(start)
			s.cmds = append(s.cmds, c)
			s.trapBody(raws, starts)
			return []int{len(s.cmds) - 1}
		case ch == '<' || ch == '>':
			s.redirect()
			redirected = true
		case ch == '(':
			if len(raws) != 1 || c.words[0] == "" {
				s.fail("a ( inside a command")
			}
			s.pos++
			s.expect(")")
			s.blanksAndNewlines()
			s.fn = append(s.fn, raws[0])
			s.command()
			s.fn = s.fn[:len(s.fn)-1]
			return nil
		default:
			at := s.pos
			raw, plain := s.word()
			raws, starts = append(raws, raw), append(starts, at)
			if !plain {
				raw = ""
			}
			c.words = append(c.words, raw)
		}
	}
}

// trapBody reads the single-quoted action of a `trap` as the code it is.
func (s *shScan) trapBody(raws []string, starts []int) {
	if len(raws) < 2 || raws[0] != "trap" || len(raws[1]) < 2 || raws[1][0] != '\'' ||
		strings.Count(raws[1], "'") != 2 || !strings.HasSuffix(raws[1], "'") {
		return
	}
	s.nested(raws[1][1:len(raws[1])-1], starts[1]+1, s.subst)
}

// nested reads code held in a word as a script of its own, its commands
// recorded beside this one's.
func (s *shScan) nested(src string, at, subst int) {
	inner := &shScan{src: src, line: s.lineAt(at), subst: subst, fn: slices.Clone(s.fn), cmds: s.cmds}
	inner.list()
	if inner.blanksAndNewlines(); !inner.eof() {
		inner.fail("nothing ends the nested list here")
	}
	s.cmds = inner.cmds
}

func (s *shScan) redirect() {
	if s.at("<<") {
		s.fail("a here-document, which this scanner does not read")
	}
	s.pos++
	if ch := s.peek(); ch == '&' || ch == '>' || ch == '|' {
		s.pos++
	}
	s.blanks()
	s.word()
}

// redirects reads those after a compound command, an IO number's included.
func (s *shScan) redirects() {
	for {
		s.blanks()
		i := s.pos
		for i < len(s.src) && s.src[i] >= '0' && s.src[i] <= '9' {
			i++
		}
		if i >= len(s.src) || (s.src[i] != '<' && s.src[i] != '>') {
			return
		}
		s.pos = i
		s.redirect()
	}
}

// word reads one word as written, and whether it is plain: no quoting, escape
// or expansion in it.
func (s *shScan) word() (string, bool) {
	start, plain := s.pos, true
	for !s.eof() {
		switch s.peek() {
		case ' ', '\t', '\n', ';', '&', '|', '<', '>', '(', ')':
			if s.pos == start {
				s.fail("no word")
			}
			return s.src[start:s.pos], plain
		case '\\':
			s.pos += 2
		case '\'':
			s.singleQuoted()
		case '"':
			s.doubleQuoted()
		case '`':
			s.backquoted()
		case '$':
			s.dollar()
		default:
			s.pos++
			continue
		}
		plain = false
	}
	if s.pos > len(s.src) {
		s.fail("a word cut short")
	}
	return s.src[start:s.pos], plain
}

func (s *shScan) singleQuoted() {
	end := strings.IndexByte(s.src[s.pos+1:], '\'')
	if end < 0 {
		s.fail("an unterminated single quote")
	}
	s.pos += end + 2
}

func (s *shScan) doubleQuoted() {
	for s.pos++; ; {
		switch s.peek() {
		case 0:
			s.fail("an unterminated double quote")
		case '"':
			s.pos++
			return
		case '\\':
			s.pos += 2
		case '$':
			s.dollar()
		case '`':
			s.backquoted()
		default:
			s.pos++
		}
	}
}

// backquoted reads an old-style command substitution, its escapes undone.
func (s *shScan) backquoted() {
	var inner strings.Builder
	open := s.pos
	for s.pos++; ; {
		switch {
		case s.eof():
			s.fail("an unterminated backquote")
		case s.peek() == '`':
			s.pos++
			s.nested(inner.String(), open+1, s.subst+1)
			return
		case s.peek() == '\\' && s.pos+1 < len(s.src) && strings.IndexByte("`\\$", s.src[s.pos+1]) >= 0:
			inner.WriteByte(s.src[s.pos+1])
			s.pos += 2
		default:
			inner.WriteByte(s.peek())
			s.pos++
		}
	}
}

// dollar reads an expansion: `$(…)`, the substitution it is, `$((…))`,
// `${…}`, or the `$` of a plain parameter.
func (s *shScan) dollar() {
	switch {
	case s.at("$(("):
		s.pos += 3
		s.arithmetic()
	case s.at("$("):
		s.pos += 2
		s.subst++
		s.list()
		s.expect(")")
		s.subst--
	case s.at("${"):
		for s.pos += 2; ; {
			switch s.peek() {
			case 0:
				s.fail("an unterminated ${")
			case '}':
				s.pos++
				return
			case '\\':
				s.pos += 2
			case '\'':
				s.singleQuoted()
			case '"':
				s.doubleQuoted()
			case '`':
				s.backquoted()
			case '$':
				s.dollar()
			default:
				s.pos++
			}
		}
	default:
		s.pos++
	}
}

func (s *shScan) arithmetic() {
	for depth := 0; ; {
		switch {
		case s.eof():
			s.fail("an unterminated $((")
		case s.peek() == '$':
			s.dollar()
		case s.peek() == '"':
			s.doubleQuoted()
		case s.peek() == '(':
			depth++
			s.pos++
		case s.peek() == ')' && depth > 0:
			depth--
			s.pos++
		case s.at("))"):
			s.pos += 2
			return
		default:
			s.pos++
		}
	}
}
