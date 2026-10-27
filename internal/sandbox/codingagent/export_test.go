package codingagent

import (
	"cmp"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// Bounded holds r's failure — the error stream it reads from the end, and the
// whole failure it composes — to failure bytes, and the lines of an event
// stream it reads to line bytes, and returns r.
//
// FOR THE CASES THAT GO PAST A BOUND: a failure past two mebibytes, or a line
// past thirty-two, is the case each is about, and at the real figures every one
// built and redacted megabytes under -race. Each runs at a bound it can fill,
// sizes what it writes from that bound, and leaves the real figures to
// [TestARunnerReadsToTheEnginesOwnBounds].
func Bounded(r *Runner, failure, line int) *Runner {
	r.failureBound, r.lineBound = failure, line
	return r
}

// RedactContext is how much more than it keeps a read from the error stream's
// end takes, so a credential that began before what is kept is still redacted.
const RedactContext = redactContext

// UnreadNote is the line standing where the error stream's first unread bytes
// were left out of what was shown.
func UnreadNote(unread int64) string { return unreadNote(unread, false) }

// MaxLineBytes is the line bound every runner New builds reads to.
const MaxLineBytes = maxLineBytes

// LiveWindow and LiveBacklog are the live reading's: how much of a stream's
// end it begins from, and the most one read catches up before beginning again.
const (
	LiveWindow  = liveWindow
	LiveBacklog = liveBacklog
)

// OpenCodeTimes is the waits a timed case runs a run's script at
// ([OpenCodeCommandTimed]); a field left zero is [OpenCode.Command]'s own.
type OpenCodeTimes struct {
	// Listen is how long the script waits for its server to listen, MCP for
	// its MCP servers to connect, Sync how often it exports a login it was
	// given, and Stop how long it gives a process of its own to go.
	Listen, MCP, Sync, Stop time.Duration
}

// OpenCodeCommandTimed is [OpenCode.Command] with the script run at times —
// for the cases about a wait running out, a sync coming round or a stop that
// must not wait, at lengths a test can tell apart.
func OpenCodeCommandTimed(req sandbox.RunRequest, paths Paths, times OpenCodeTimes) string {
	return "sh -c " + shellQuote(openCodeScript(req, paths, newOpenCodeSession(), openCodeTimes{
		listen: cmp.Or(times.Listen, openCodeListenWait), mcp: cmp.Or(times.MCP, openCodeMCPWait),
		sync: cmp.Or(times.Sync, openCodeLoginSync), stop: cmp.Or(times.Stop, openCodeStopWait),
	}))
}

// JSONStringify is how a run's script spells a server name it looks for in an
// OpenCode answer.
func JSONStringify(s string) string { return jsonStringify(s) }

// ClaudeHookContext is what Claude Code puts after an event's name before a
// hook's additional context, and ClaudeNoteEvents the events a run's notes are
// taken at — what a test holds the instruction and the measured placements to.
const ClaudeHookContext = claudeHookContext

var ClaudeNoteEvents = []string{claudeEventToolBatch, claudeEventFinish}
