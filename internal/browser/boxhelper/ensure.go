package boxhelper

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/shellword"
)

// EnsureTimeout bounds one run of ensure, the engine's side of ensure's own
// deadline (config.json's ensure_deadline_ms), which it passes first. It is
// internal/mcp's DefaultStartupTimeout, the engine's bound on a first npx or
// uvx fetch of an MCP server, which a box's first install of the tree is.
const EnsureTimeout = 120 * time.Second

// linePrefix starts ensure's line, its grammar's version with it.
const linePrefix = "crewlet-browser-ensure/1"

// letters is every ASCII letter, spelled out: a range in a shell's bracket
// expression is the locale's to read.
const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// prelude is the POSIX sh that runs before Node is asked to load the helper,
// since a box with no Node, or one too old to load the helper's modules,
// could run no helper to say so. It answers node_missing with no node on
// PATH, and node_too_old for a node whose version is not the line's grammar
// (three numbers, a prerelease's tag at most, 64 bytes in all) or whose major
// is below MinNodeMajor, compared as a number. NODE_OPTIONS goes first: it
// loads code into every node before its first line. Built at first use, as
// the line's grammars are, so a binary that links this package pays for none
// of them at init: every start of crewlet, a push's hook's included, runs
// every linked package's init (TestTheHookPaysLittleForTheEnginesInit).
var prelude = sync.OnceValue(func() string {
	return `unset NODE_OPTIONS
crewlet_withheld() { printf '%s withheld reason=%s cause=%s%s\n' '` + linePrefix + `' "$1" "$2" "$3"; exit 0; }
command -v node >/dev/null 2>&1 || crewlet_withheld ` + pair(browser.ReasonNodeMissing, browser.CauseNotOnPath) + ` ''
crewlet_node=$(node -p process.versions.node 2>/dev/null) || crewlet_node=
crewlet_release=${crewlet_node%%-*}
crewlet_tag=${crewlet_node#"$crewlet_release"}
crewlet_major=${crewlet_release%%.*} crewlet_rest=${crewlet_release#*.}
crewlet_minor=${crewlet_rest%%.*} crewlet_patch=${crewlet_rest#*.}
crewlet_digits() { case $1 in '' | *[!0123456789]*) return 1 ;; esac; [ ${#1} -le "$2" ]; }
crewlet_tagged() {
	[ -z "$1" ] && return 0
	case ${1#-} in '' | .* | *. | *..* | *[!0123456789` + letters + `.-]*) return 1 ;; esac
}
if crewlet_digits "$crewlet_major" 3 && crewlet_digits "$crewlet_minor" 4 && crewlet_digits "$crewlet_patch" 6 &&
	[ "$crewlet_major.$crewlet_minor.$crewlet_patch" = "$crewlet_release" ] && crewlet_tagged "$crewlet_tag" &&
	[ ${#crewlet_node} -le ` + strconv.Itoa(versionMax) + ` ]; then
	[ "$crewlet_major" -ge ` + strconv.Itoa(MinNodeMajor) + ` ] ||
		crewlet_withheld ` + pair(browser.ReasonNodeTooOld, browser.CauseBelowFloor) + ` " node=$crewlet_node"
else
	crewlet_withheld ` + pair(browser.ReasonNodeTooOld, browser.CauseBelowFloor) + ` ''
fi
`
})

// pair is a reason and its cause as the prelude hands them to
// crewlet_withheld: two words, internal/browser's own values.
func pair(r browser.EnsureReason, c browser.EnsureCause) string {
	return string(r) + " " + string(c)
}

// EnsureScript is ensure as a POSIX sh script for the box whose home is home:
// the prelude, then the helper's ensure, every path quoted.
func EnsureScript(home string) string {
	return prelude() + "exec node " + shellword.Quote(path.Join(Folder(home), "helper.mjs")) + " ensure\n"
}

// EnsureCommand is the command the engine runs ensure with in the box whose
// home is home: [EnsureScript] in a login shell, as a job runs, so ensure
// finds the node a job would.
func EnsureCommand(home string) string {
	return "sh -lc " + shellword.Quote(EnsureScript(home))
}

// Ensure is ensure's answer, read: ready with what runs, or withheld with
// why, and what ensure knew of the box either way.
type Ensure struct {
	Verdict browser.EnsureVerdict
	// Reason and Cause are a withheld answer's, empty on a ready one.
	Reason browser.EnsureReason
	Cause  browser.EnsureCause
	// Helper, Pin, Node and Chromium are what ensure knew: a ready answer
	// carries each, a withheld one those it learnt before its check failed.
	Helper   string
	Pin      string
	Node     string
	Chromium string
	// Build, Sandbox and Supervisor are a ready answer's.
	Build      browser.ChromiumBuild
	Sandbox    browser.ChromiumSandbox
	Supervisor browser.SupervisorStart
}

// UnreadableError is ensure's output that is no answer: no line at all
// (browser.CauseNoLine), or one off its grammar (browser.CauseBadLine), each
// the cause of browser.ReasonEnsureUnreadable.
type UnreadableError struct {
	Cause  browser.EnsureCause
	Detail string
}

func (e *UnreadableError) Error() string {
	return fmt.Sprintf("boxhelper: ensure's answer is unreadable (%s): %s", e.Cause, e.Detail)
}

// The line's bounds: what of ensure's output the engine reads, and how long
// its one line may run. The longest line the grammar admits, every value at
// its bound, is under 380 bytes, so lineBound leaves a later build's keys
// room, and readBound holds the line after whatever a login shell prints
// before it.
const (
	readBound = 4 << 10
	lineBound = 512
)

// versionMax bounds a node or pin a line carries, its prerelease's tag
// included: a tree's directory name carries the pin, and a version of more
// is none a release names.
const versionMax = 64

// prerelease is a version's optional prerelease tag, as semver.org spells
// one: ci.yml's latest Node is an alpha (27.0.0-alpha.0.1.1), and
// @playwright/mcp's next is a prerelease too.
const prerelease = `(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?`

// fieldGrammar is what each key's value must be; a key it does not name is a
// later build's, and ignored. A closed value is held to its type's Valid, so
// the line's grammar is never a second copy of internal/browser's sets.
var fieldGrammar = sync.OnceValue(func() map[string]func(string) bool {
	return map[string]func(string) bool{
		"helper":     regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString,
		"pin":        versionGrammar(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,6}` + prerelease + `$`),
		"node":       versionGrammar(`^[0-9]{1,3}\.[0-9]{1,4}\.[0-9]{1,6}` + prerelease + `$`),
		"chromium":   regexp.MustCompile(`^[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,6}\.[0-9]{1,6}$`).MatchString,
		"build":      func(v string) bool { return browser.ChromiumBuild(v).Valid() },
		"sandbox":    func(v string) bool { return browser.ChromiumSandbox(v).Valid() },
		"supervisor": func(v string) bool { return browser.SupervisorStart(v).Valid() },
		"reason":     regexp.MustCompile(`^[a-z_]{1,64}$`).MatchString,
		"cause":      regexp.MustCompile(`^[a-z_]{1,64}$`).MatchString,
	}
})

// versionGrammar is a version's grammar, re, within versionMax.
func versionGrammar(re string) func(string) bool {
	g := regexp.MustCompile(re)
	return func(v string) bool { return len(v) <= versionMax && g.MatchString(v) }
}

var (
	keyGrammar   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[a-z_]{1,16}$`) })
	valueGrammar = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[\x21-\x7e]{1,128}$`) })
)

// ParseEnsure reads ensure's answer from its stdout: the last non-empty line
// of the first 4 KiB, which must be ensure's grammar whole, every closed value
// one of the box's. An unknown key is ignored. A known key off its grammar
// or given twice, one an answer requires missing, a reason the box never
// prints, or a cause not its reason's, is an *UnreadableError, as is no line.
func ParseEnsure(stdout string) (Ensure, error) {
	if len(stdout) > readBound {
		stdout = stdout[:readBound]
	}
	var line string
	for _, l := range strings.Split(stdout, "\n") {
		if l != "" {
			line = l
		}
	}
	if line == "" {
		return Ensure{}, &UnreadableError{Cause: browser.CauseNoLine, Detail: "ensure printed no line"}
	}
	bad := func(format string, a ...any) (Ensure, error) {
		return Ensure{}, &UnreadableError{Cause: browser.CauseBadLine, Detail: fmt.Sprintf(format, a...)}
	}
	if len(line) > lineBound {
		return bad("its line runs %d bytes, past %d", len(line), lineBound)
	}
	words := strings.Split(line, " ")
	if len(words) < 3 || words[0] != linePrefix {
		return bad("%q is not %s, a verdict and its fields", line, linePrefix)
	}
	e := Ensure{Verdict: browser.EnsureVerdict(words[1])}
	if !e.Verdict.Valid() {
		return bad("%q is no verdict", words[1])
	}
	fields := map[string]string{}
	for _, w := range words[2:] {
		key, value, ok := strings.Cut(w, "=")
		if !ok || !keyGrammar().MatchString(key) || !valueGrammar().MatchString(value) {
			return bad("%q is no field", w)
		}
		valid, known := fieldGrammar()[key]
		if !known {
			continue
		}
		if _, twice := fields[key]; twice {
			return bad("%s is given twice", key)
		}
		if !valid(value) {
			return bad("%s=%q is off its grammar", key, value)
		}
		fields[key] = value
	}
	required := []string{"reason", "cause"}
	if e.Verdict == browser.EnsureReady {
		required = []string{"helper", "pin", "node", "chromium", "build", "sandbox", "supervisor"}
		for _, key := range []string{"reason", "cause"} {
			if _, ok := fields[key]; ok {
				return bad("a ready answer carries %s", key)
			}
		}
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return bad("a %s answer lacks %s", e.Verdict, key)
		}
	}
	e.Helper, e.Pin, e.Node, e.Chromium = fields["helper"], fields["pin"], fields["node"], fields["chromium"]
	e.Build = browser.ChromiumBuild(fields["build"])
	e.Sandbox = browser.ChromiumSandbox(fields["sandbox"])
	e.Supervisor = browser.SupervisorStart(fields["supervisor"])
	if e.Verdict == browser.EnsureWithheld {
		e.Reason, e.Cause = browser.EnsureReason(fields["reason"]), browser.EnsureCause(fields["cause"])
		if !e.Reason.Printed() {
			return bad("%q is no reason a box gives", e.Reason)
		}
		if !e.Reason.Takes(e.Cause) {
			return bad("%q is no cause of %s", e.Cause, e.Reason)
		}
	}
	return e, nil
}
