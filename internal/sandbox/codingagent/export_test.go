package codingagent

import (
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

// OpenCodeCommandTimed is [OpenCode.Command] with the script waiting listen
// for its server and mcp for its MCP servers, and exporting a login it was
// given every sync — for the cases about a wait running out or a sync coming
// round, at lengths a test can afford.
func OpenCodeCommandTimed(req sandbox.RunRequest, paths Paths, listen, mcp, sync time.Duration) string {
	return "sh -c " + shellQuote(openCodeScript(req, paths, newOpenCodeSession(),
		openCodeTimes{listen: listen, mcp: mcp, sync: sync}))
}

// OpenCodeListenWait and OpenCodeMCPWait are the waits [OpenCode.Command] runs
// its script at, for a timed case whose subject is another of them.
const (
	OpenCodeListenWait = openCodeListenWait
	OpenCodeMCPWait    = openCodeMCPWait
)

// JSONStringify is how a run's script spells a server name it looks for in an
// OpenCode answer.
func JSONStringify(s string) string { return jsonStringify(s) }
