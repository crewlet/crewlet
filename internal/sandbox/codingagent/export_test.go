package codingagent

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

// Bounds is the failure and line bounds r reads to.
func Bounds(r *Runner) (failure, line int) { return r.failureBound, r.lineBound }

// MaxLineBytes is the line bound every runner New builds reads to.
const MaxLineBytes = maxLineBytes

// LiveWindow and LiveBacklog are the live reading's: how much of a stream's
// end it begins from, and the most one read catches up before beginning again.
const (
	LiveWindow  = liveWindow
	LiveBacklog = liveBacklog
)
