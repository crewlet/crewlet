package hostreach

import (
	_ "embed"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/shellword"
)

// The variables the engine hands the box's half of the check, over the run's
// environment: the port it listens on, a token for each route, the two dial
// bounds and the half's own deadline in milliseconds, the addresses past a
// rootless runtime's gateway, and this host's own addresses.
const (
	PortEnv     = "CREWLET_REACH_PORT"
	TokensEnv   = "CREWLET_REACH_TOKENS"
	RouteEnv    = "CREWLET_REACH_ROUTE_MS"
	AnswerEnv   = "CREWLET_REACH_ANSWER_MS"
	DeadlineEnv = "CREWLET_REACH_DEADLINE_MS"
	BeyondEnv   = "CREWLET_REACH_BEYOND"
	HostEnv     = "CREWLET_REACH_HOST"
)

// The check's bounds; the package doc's "Bounds" argues each.
const (
	// RouteBound is how long a dial may go unconnected before it leads
	// nowhere, and a proxy asked elsewhere may take to refuse it.
	RouteBound = 1500 * time.Millisecond
	// AnswerBound is how long a made connection may wait for its answer, and
	// a proxy's name for its addresses, before the check is held.
	AnswerBound = 5 * time.Second
	// CheckTimeout bounds the whole check: the engine's exec and the box's
	// half alike.
	CheckTimeout = 30 * time.Second
)

// reachJS is the box's half past its shell: reach.js, run with node -e.
//
//go:embed reach.js
var reachJS string

// linePrefix starts the box's one line, its grammar's version with it, as
// reach.js's LINE does.
const linePrefix = "crewlet-browser-reach/1"

// The box's half's own variables: the boot id and network namespace its
// shell read, for node to say.
const (
	kernelEnv = "CREWLET_REACH_KERNEL"
	netEnv    = "CREWLET_REACH_NET"
)

// script is the check's POSIX sh half: it reads the box's boot id and the
// inode of its network namespace, the word unreadable for either it cannot,
// then hands node the dials, or says no_node where the login shell finds no
// node. NODE_OPTIONS goes first, as ensure's prelude clears it: it loads code
// into every node. Built at first use, as the command and the line's grammar
// are, so a crewlet that runs no check (a push's hook) pays nothing at init.
var script = sync.OnceValue(func() string {
	return `unset NODE_OPTIONS
crewlet_kernel=
{ IFS= read -r crewlet_kernel; } 2>/dev/null <` + bootIDFile + `
[ -n "$crewlet_kernel" ] || crewlet_kernel=unreadable
crewlet_net=$(readlink ` + netNSFile + ` 2>/dev/null) || crewlet_net=
crewlet_net=${crewlet_net#net:\[}
crewlet_net=${crewlet_net%\]}
case $crewlet_net in '' | *[!0123456789]*) crewlet_net=unreadable ;; esac
if command -v node >/dev/null 2>&1; then
	` + kernelEnv + `=$crewlet_kernel ` + netEnv + `=$crewlet_net exec node -e ` + shellword.Quote(reachJS) + `
fi
printf '%s kernel=%s net=%s no_node\n' '` + linePrefix + `' "$crewlet_kernel" "$crewlet_net"
`
})

// command is [Command]'s answer, quoted once.
var command = sync.OnceValue(func() string { return "sh -lc " + shellword.Quote(script()) })

// Command is the command the engine runs the box's half with: the script in
// a login shell, as ensure runs, so it finds the node a job would.
func Command() string { return command() }

// netNSFile names the network namespace a process is in, as net:[<inode>].
const netNSFile = "/proc/self/ns/net"

// initNetNS is the inode of the network namespace a kernel boots with, which
// kernels since the fixed inodes of their initial namespaces give it
// (0xEFFFFFF9), and older ones never give any, since they number every
// namespace from 0xF0000000 up: a box in it shares the machine's network,
// whatever namespace this engine runs in.
const initNetNS = "4026531833"

// answerNet is the namespace a double of a clean box says it is in: one in
// the range kernels number every namespace but the initial one from.
const answerNet = "4026532289"

// Answer is the line a box on kernel k answers when nothing it dialed reached
// the engine: what a double of a clean box prints.
func Answer(k Kernel) string {
	return linePrefix + " kernel=" + string(k) + " net=" + answerNet + " " + string(wordChecked) + "\n"
}

// slirpGateways is the gateway slirp4netns and RootlessKit give a box's
// network, which their host-loopback setting leads to the host's loopback.
var slirpGateways = []string{"10.0.2.2", "fd00::2"}

// word is the box's verdict on its own dials, its line's last field.
type word string

const (
	wordChecked      word = "checked"
	wordHeld         word = "held"
	wordTooMany      word = "too_many"
	wordUnreadable   word = "unreadable"
	wordNoNode       word = "no_node"
	wordForwarded    word = "forwarded"
	wordProxyDown    word = "proxy_down"
	wordProxyRefused word = "proxy_refused"
)

// readBound is how much of the half's output the engine reads: the line runs
// about 90 bytes, and at most 134 as its grammar has it, after whatever a
// login shell prints before it.
const readBound = 4 << 10

// lineGrammar is the box's line, whole: its kernel, its network namespace,
// then its word.
var lineGrammar = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^` + regexp.QuoteMeta(linePrefix) + ` kernel=([0-9A-Za-z-]{1,64}) ` +
		`net=([0-9]{1,20}|unreadable) ` +
		`(checked|held|too_many|unreadable|no_node|forwarded|proxy_down|proxy_refused)$`)
})

// boxLine is what the box's line says: the boot id its shell read, or
// unreadable; the inode of its network namespace, or unreadable; and its
// word.
type boxLine struct {
	kernel string
	net    string
	word   word
}

// lineState is how the half's stdout reads.
type lineState int

const (
	lineRead lineState = iota
	lineNone
	lineOffGrammar
)

// parseLine reads the half's line from its stdout: the last non-empty line of
// its first [readBound] bytes, which must be the grammar whole.
func parseLine(stdout string) (boxLine, lineState) {
	if len(stdout) > readBound {
		stdout = stdout[:readBound]
	}
	var last string
	for l := range strings.Lines(stdout) {
		if l = strings.TrimRight(l, "\r\n"); l != "" {
			last = l
		}
	}
	if last == "" {
		return boxLine{}, lineNone
	}
	m := lineGrammar().FindStringSubmatch(last)
	if m == nil {
		return boxLine{}, lineOffGrammar
	}
	return boxLine{kernel: m[1], net: m[2], word: word(m[3])}, lineRead
}
