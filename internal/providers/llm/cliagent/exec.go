package cliagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// termGrace is how long a terminated CLI has to exit before it is killed.
//
// The timeout has already fired, so the call is lost either way; this decides
// only whether the tree gets to flush. Five seconds is enough for a Node
// runtime to run its exit handlers and close its sockets, and short enough
// that a wedged child does not hold the seat's concurrency slot for a
// meaningful fraction of the next call. Same figure the sandbox uses for the
// same reason.
//
// Every provider [New] builds carries it as [Provider.termGrace], which is
// what every child this package starts is given — a seat's call, the doctor's
// version probe and the credential commands alike — so one figure governs
// them all and the suite can witness a kill without sitting out five seconds
// of it.
const termGrace = 5 * time.Second

// stderrTail is how many lines of a failed child's stderr are reported.
//
// A CLI that crashes on startup prints a stack trace; the first line names
// the failure and the rest names the vendor's own internals. Fifty lines
// carries a real Node or Rust trace intact while keeping one bad provider out
// of the log budget for every other seat. Same figure the MCP supervisor uses.
const stderrTail = 50

// maxOutput bounds what is read from a child's stdout and stderr.
//
// A CLI told to stream JSONL can emit tens of megabytes for a long run, and
// all of it is held in memory to be parsed. 32 MiB is far above any real
// completion — the largest observed is under 2 MiB — and far below what would
// put the engine under memory pressure with max_concurrent processes running.
const maxOutput = 32 << 20

// invocation is one CLI call, fully resolved.
//
// Its env is the child's WHOLE environment, from [buildEnv], and it is
// required: [run] refuses a nil one, because os/exec reads a nil Cmd.Env as
// "inherit the engine's".
type invocation struct {
	binary  string
	args    []string
	stdin   string
	dir     string
	env     []string
	timeout time.Duration

	// grace is how long a child signalled to stop has before it is killed —
	// the provider's [Provider.termGrace]. Zero is refused by [run] rather
	// than read as os/exec's own zero, which is no bound at all: a child
	// that ignored SIGTERM would then hold the call open for ever.
	grace time.Duration

	// onLine, when set, receives each COMPLETE line of stdout as the child
	// writes it, before the call returns. Nil takes the ordinary path where
	// stdout is only read after the process exits.
	//
	// Called from the goroutine [run] copies stdout on — one goroutine, in
	// order, and run does not return until it has finished, so it never runs
	// concurrently with the caller's own use of the result.
	onLine func(string)

	// everyLine forwards every line to onLine, past the stdout cap too —
	// for a run whose record is read line by line as it goes, where each
	// line is the record rather than part of an answer the cap would refuse.
	everyLine bool

	// clock bounds the child by stretches of model work — see [stretch].
	// Nil: timeout bounds the whole run, as one stretch nothing pauses.
	clock *stretch

	// stop ends the run when it is closed: whoever asked for it is done with
	// it. The child is stopped the way a timeout stops it — its group
	// signalled, a grace, then killed — and the result says so
	// ([rawResult.stopped]). Nil never closes.
	stop <-chan struct{}

	// failOn are sentinels which, written on stderr, mean the CLI is
	// running without the confinement its profile gave it: the first line
	// carrying one stops the run at once — see
	// [cliprofile.Profile.FailureMarkers].
	failOn []string
}

// lineTee forwards complete lines to a callback while passing every byte
// through to the real sink.
//
// A CLI's stdout arrives in whatever chunks the pipe hands over, which split
// mid-line, so a consumer that wants JSONL events has to reassemble them. The
// tail after the last newline is deliberately NOT delivered: a partial JSON
// object is not parseable, and the buffered copy is what the extractor reads
// once the process exits, so nothing is lost by waiting.
//
// BOUNDED BY THE SINK'S OWN CAP, which it used not to be: the sink drops past
// [maxOutput] and still reports every byte written, so this kept appending a
// line with no newline in it for as long as the child printed one — the very
// memory the cap exists to protect. Past the cap it forwards nothing more,
// because the answer it would be streaming is refused as clipped anyway.
//
// UNLESS every line is the point ([invocation.everyLine]): a run whose record
// is read as it goes needs the lines past the cap as much as the ones before
// it, and holds none of them. Then only ONE line is bounded — a line longer
// than the cap is skipped to its end rather than held.
type lineTee struct {
	sink  io.Writer
	on    func(string)
	buf   []byte
	limit int
	seen  int
	// every forwards past limit, which then bounds one line instead.
	every bool
	// skipping is a line past the bound, dropped up to its newline.
	skipping bool
}

func (t *lineTee) Write(b []byte) (int, error) {
	n, err := t.sink.Write(b)
	if t.seen += n; t.seen > t.limit && !t.every {
		t.buf = nil
		return n, err
	}
	rest := b[:n]
	for len(rest) > 0 {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			if !t.skipping {
				t.buf = append(t.buf, rest...)
			}
			if len(t.buf) > t.limit {
				t.buf, t.skipping = nil, true
			}
			break
		}
		if !t.skipping {
			t.on(string(append(t.buf, rest[:i]...)))
		}
		t.buf, t.skipping = t.buf[:0], false
		rest = rest[i+1:]
	}
	return n, err
}

// errHostStopped is the cause a run's context is cancelled with when whoever
// asked for the run closed [invocation.stop].
var errHostStopped = errors.New("the run was stopped by the engine")

// failureMarkerError is the cause a run's context is cancelled with when the
// CLI wrote one of [invocation.failOn] on stderr.
type failureMarkerError struct{ line string }

func (e *failureMarkerError) Error() string {
	return "the CLI reported its confinement failed to load: " + e.line
}

// stderrWatch forwards stderr to its sink and calls stop with the first line
// carrying one of the markers, once.
func stderrWatch(sink io.Writer, markers []string, stop func(line string)) io.Writer {
	var once sync.Once
	return &lineTee{sink: sink, limit: maxOutput, every: true, on: func(line string) {
		for _, m := range markers {
			if strings.Contains(line, m) {
				once.Do(func() { stop(strings.TrimSpace(line)) })
				return
			}
		}
	}}
}

// rawResult is what a child produced.
type rawResult struct {
	stdout   string
	stderr   string
	exitCode int
	// timedOut reports that the wall-clock cap fired, which is a different
	// fact from the context the caller passed being cancelled.
	timedOut bool

	// stopped reports a child stopped because [invocation.stop] closed
	// before it exited — whoever asked for the run ended it.
	stopped bool

	// failureMarker is the stderr line that carried one of
	// [invocation.failOn], which stopped the run; "" for none.
	failureMarker string

	// droppedStdout and droppedStderr are the bytes each stream lost past
	// maxOutput. SEPARATE, because the two mean different things: a clipped
	// stdout is a clipped ANSWER and the caller must refuse it, while a
	// clipped stderr costs only diagnosis. Summing them, as this used to,
	// produced a warning that could not say which had happened.
	droppedStdout, droppedStderr int

	// heldStdout and heldStderr report a stream that something the CLI
	// started still held open [invocation.grace] after the CLI was done —
	// exited, or its call ended, whichever came first — so its read was cut
	// there. Separate for the reason the dropped counts are: a held stdout
	// is an answer nobody can show is whole, and a held stderr costs only
	// diagnosis.
	heldStdout, heldStderr bool

	// waitErr is what waiting for a child that RAN reported besides its exit
	// status: the call's context ending under a child that then exited 0, a
	// prompt something the CLI started held unread on stdin past the grace
	// ([exec.ErrWaitDelay]), a signal that could not be sent. A child that
	// never ran is [run]'s error instead, and leaves no result at all.
	waitErr error

	// grace is the invocation's, kept for the message that says how long a
	// held stream was waited for.
	grace time.Duration
}

// unfinished says why a child that ran cannot be taken as having finished
// cleanly, or "" when it can: the reason a caller that reads what the child
// printed refuses it. A deadline and a non-zero exit are not among them, since
// every caller already tells those apart and classifies each as itself. A held
// stderr is not among them either: what it cost is diagnosis, and what was
// printed on stdout stands.
func (r *rawResult) unfinished() string {
	switch {
	case r.heldStdout:
		return fmt.Sprintf("something it started still held its stdout open %s after "+
			"the CLI was done, so what it printed cannot be shown to be whole", r.grace)
	case r.waitErr != nil:
		return fmt.Sprintf("waiting for it to finish failed: %v", r.waitErr)
	default:
		return ""
	}
}

// run executes one invocation, terminating the whole process TREE when its
// call ends first, and when something the CLI left behind holds the call's
// pipes past the grace.
//
// The tree, not the process: a coding CLI is a launcher over a Node or Bun
// runtime that forks helpers, and signalling only the process Go started
// leaves those holding the seat's memory and its network sockets.
//
// AN ERROR MEANS NO CHILD RAN: an invocation refused before the fork, a pipe
// the host would not give, a binary exec could not start. Everything that
// happened to a child that did start — its exit, the deadline, a stream
// something it left behind still held, a wait that failed — is in the result,
// because only the caller can say what each means, and a caller that read
// every error as "could not start" stopped a seat's fallback chain on a CLI
// that had printed a whole answer and exited 0 ([Provider.Complete]).
func run(ctx context.Context, in invocation) (*rawResult, error) {
	// A MISSING ENVIRONMENT IS NOT AN EMPTY ONE. os/exec hands a child whose
	// Env is nil the ENGINE's own environment — its keyring, its Tier A
	// tokens, the org's chat token and every provider key — which is the
	// one thing the allowlist exists to keep out of a vendor's CLI. The
	// version probe reached here with none once and ran `--version` with
	// more access than any completion ever got; refused here, the next
	// caller that forgets cannot do the same silently.
	if in.env == nil {
		return nil, fmt.Errorf("cliagent: %s was about to run with no environment "+
			"of its own, which os/exec would fill with the engine's; it was not "+
			"started", in.binary)
	}
	if in.grace <= 0 {
		return nil, fmt.Errorf("cli-agent: starting %q with no termination grace — "+
			"a child that ignored SIGTERM would hold the call open for ever; "+
			"an invocation takes its provider's termGrace", in.binary)
	}
	// ONE CLOCK FOR EVERY RUN: a text call's is never paused, so its one
	// stretch is the whole call — see [stretch].
	clock := in.clock
	if clock == nil {
		clock = newStretch(in.timeout)
	}
	defer clock.stop()
	callCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	cmd := exec.CommandContext(callCtx, in.binary, in.args...) //nolint:gosec // binary and args come from a validated profile
	cmd.Dir = in.dir
	cmd.Env = in.env
	if in.stdin != "" {
		cmd.Stdin = strings.NewReader(in.stdin)
	}
	var stdout, stderr cappedBuffer
	stdout.limit = maxOutput
	stderr.limit = maxOutput
	sink := io.Writer(&stdout)
	if in.onLine != nil {
		sink = &lineTee{sink: &stdout, on: in.onLine, limit: maxOutput, every: in.everyLine}
	}
	errSink := io.Writer(&stderr)
	var marker atomic.Pointer[string]
	if len(in.failOn) > 0 {
		errSink = stderrWatch(&stderr, in.failOn, func(line string) {
			marker.Store(&line)
			cancel(&failureMarkerError{line: line})
		})
	}

	// THE OUTPUT ON PIPES OF OUR OWN, handed to the child as files. Given
	// any other io.Writer, os/exec makes the pipe itself and copies it on a
	// goroutine its Wait waits for, and when something the CLI started still
	// holds a pipe after the CLI has exited, it closes both once WaitDelay is
	// out and reports one [exec.ErrWaitDelay] for the pair. Which stream was
	// held is the whole question: a held stderr costs only diagnosis and the
	// answer stands, while a held stdout is an answer nobody can show is
	// whole. Owning the read ends is what lets [outputPipe] say which.
	outPipe, err := newOutputPipe()
	if err != nil {
		return nil, fmt.Errorf("cli-agent: starting %q: %w", in.binary, err)
	}
	errPipe, err := newOutputPipe()
	if err != nil {
		outPipe.abandon()
		return nil, fmt.Errorf("cli-agent: starting %q: %w", in.binary, err)
	}
	cmd.Stdout, cmd.Stderr = outPipe.w, errPipe.w
	procgroup.Set(cmd)

	// Cancel signals the GROUP rather than letting os/exec signal the one
	// process it knows about, and WaitDelay bounds how long a child that
	// ignored SIGTERM can hold the call open.
	cmd.Cancel = func() error { return procgroup.Terminate(cmd.Process.Pid) }
	cmd.WaitDelay = in.grace

	if err := cmd.Start(); err != nil {
		outPipe.abandon()
		errPipe.abandon()
		return nil, fmt.Errorf("cli-agent: starting %q: %w", in.binary, err)
	}
	pgid := cmd.Process.Pid
	outPipe.drainInto(sink)
	errPipe.drainInto(errSink)
	clock.start()
	go func() {
		// The clock running out and the caller ending the run end it the
		// way the call's context ending does — Cancel signals the group,
		// WaitDelay bounds the grace — each under a cause of its own, so the
		// result can say which it was.
		select {
		case <-clock.expired():
			cancel(errStretch)
		case <-in.stop:
			cancel(errHostStopped)
		case <-callCtx.Done():
		}
	}()

	// THE GRACE RUNS FROM WHICHEVER COMES FIRST, the CLI exiting or its call
	// ending, which is os/exec's own rule for WaitDelay: a call stopped under
	// a stubborn tree is then let go one grace after it ended rather than
	// two, the first spent waiting for the CLI to exit and the second for
	// its pipes.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var waitErr error
	waited := false
	select {
	case waitErr = <-exited:
		waited = true
	case <-callCtx.Done():
	}
	letGoBy := time.Now().Add(in.grace)
	if !waited {
		// Bounded: Cancel has signalled the group, and os/exec kills the
		// CLI once WaitDelay is out.
		waitErr = <-exited
	}

	// GATED ON THE CONTEXT ENDING, not on the deadline specifically.
	// cmd.Cancel and WaitDelay fire on cancellation exactly as they do on a
	// timeout, so both leave the same survivor — but the reap only ran for
	// the timeout. On the one path where nothing else comes back for it,
	// shutdown, a forking Node or Bun subtree holding this seat's workspace
	// and sockets outlived the engine: os/exec's WaitDelay kill reaches the
	// immediate process, never the group.
	//
	// res.timedOut keeps its narrower meaning — cliagent.go classifies a
	// deadline as KindTimeout and a cancellation is not one.
	reap := func(reason error) {
		if killErr := procgroup.Kill(pgid); killErr != nil && !errors.Is(killErr, errors.ErrUnsupported) {
			log.WarnContext(ctx, "cli_agent_group_kill_failed",
				"pgid", pgid, "reason", reason, "error", killErr)
		}
	}
	if err := callCtx.Err(); err != nil {
		// The group was signalled by Cancel and given WaitDelay to go;
		// anything still alive after Wait returned is a survivor that
		// ignored SIGTERM, and it holds a concurrency slot until killed.
		// Before the pipes are drained, so the tree's last words arrive.
		reap(err)
	}

	cause := context.Cause(callCtx)
	res := &rawResult{
		// The clock running out, or the caller's own deadline — both a
		// call that ran out of time rather than one somebody stopped.
		timedOut: errors.Is(cause, errStretch) || errors.Is(callCtx.Err(), context.DeadlineExceeded),
		// Only a child still running when the stop came: one that exited
		// on its own a moment before was not stopped, whatever followed.
		stopped: !waited && errors.Is(cause, errHostStopped),
		grace:   in.grace,
	}
	if line := marker.Load(); line != nil {
		res.failureMarker = *line
	}
	// Decided AT THE DEADLINE, before its holder is killed or the read ends
	// are closed: a stream that reached its end only because of either is
	// one that was cut.
	res.heldStdout = !outPipe.endedBy(letGoBy)
	res.heldStderr = !errPipe.endedBy(letGoBy)
	// A HOLDER IS A SURVIVOR, and it goes with the group. Something the CLI
	// started that still held one of the call's pipes a grace after the CLI
	// was done — its stdout or stderr, or its stdin with the prompt unread,
	// which os/exec reports as ErrWaitDelay — is attached to a call that is
	// over, and closing the pipe under it ends none of it: a Node runtime
	// ignores the SIGPIPE its next write raises and holds the seat's memory
	// and sockets for as long as it likes. Killed BEFORE the pipes are
	// closed, which is os/exec's own order, so it dies of the SIGKILL rather
	// than of the pipe.
	if callCtx.Err() == nil && (res.heldStdout || res.heldStderr || errors.Is(waitErr, exec.ErrWaitDelay)) {
		reap(errors.New("it held the call's output past the grace"))
	}
	outPipe.release()
	errPipe.release()

	// Read only now that both copiers have returned: they write these
	// buffers, and release is what waits for them.
	res.stdout, res.stderr = stdout.String(), stderr.String()
	res.droppedStdout, res.droppedStderr = stdout.Truncated(), stderr.Truncated()
	for _, s := range []struct {
		stream  string
		dropped int
		held    bool
	}{{"stdout", res.droppedStdout, res.heldStdout}, {"stderr", res.droppedStderr, res.heldStderr}} {
		// NAMES THE STREAM. A truncated stream is how a usage figure or a
		// closing fence goes missing, and an operator debugging a mangled
		// answer needs to know which half was cut — the caller refuses on
		// stdout and carries on for stderr.
		if s.dropped > 0 {
			log.WarnContext(ctx, "cli_agent_output_truncated", "binary", in.binary,
				"stream", s.stream, "dropped_bytes", s.dropped, "limit_bytes", maxOutput)
		}
		// And for the same reason a held one: the caller refuses an answer
		// on a held stdout, and only this line says that a held stderr cut
		// the diagnosis of a call that otherwise answered.
		if s.held {
			log.WarnContext(ctx, "cli_agent_output_held", "binary", in.binary,
				"stream", s.stream, "grace", in.grace)
		}
	}

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
	case errors.As(waitErr, &exitErr):
		res.exitCode = exitErr.ExitCode()
	default:
		// THE CHILD RAN, so this is part of its result rather than run's
		// error, which says only that no child did.
		res.waitErr = waitErr
	}
	return res, nil
}

// outputPipe is one of a child's output streams, on a pipe [run] owns both
// ends of: the write end handed to the child as a file, and the read end a
// copier drains into the stream's sink.
type outputPipe struct {
	r, w *os.File
	// done is closed when the copier has returned, and eof — written before
	// that — says whether it returned because every holder of the write end
	// let go, rather than because the read end was closed under it.
	done chan struct{}
	eof  bool
}

func newOutputPipe() (*outputPipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("making a pipe for its output: %w", err)
	}
	return &outputPipe{r: r, w: w, done: make(chan struct{})}, nil
}

// abandon closes both ends of a pipe no child was given, its copier never
// started.
func (p *outputPipe) abandon() {
	_ = p.r.Close()
	_ = p.w.Close()
}

// drainInto drops the parent's copy of the write end, which the child now has
// its own of, and starts copying what the child writes into sink.
//
// The write end goes FIRST: while the parent holds it the pipe never reaches
// its end, whoever else lets go.
func (p *outputPipe) drainInto(sink io.Writer) {
	_ = p.w.Close()
	go func() {
		defer close(p.done)
		_, err := io.Copy(sink, p.r)
		p.eof = err == nil
	}()
}

// endedBy waits until the stream reaches its end or the deadline passes, and
// reports whether it reached its end first.
func (p *outputPipe) endedBy(deadline time.Time) bool {
	select {
	case <-p.done:
		return p.eof
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-p.done:
		return p.eof
	case <-timer.C:
		return false
	}
}

// release closes the read end, which ends a copy still blocked on a holder
// that has not let go, and waits for the copier to return, so nothing writes
// into the sink after it.
func (p *outputPipe) release() {
	_ = p.r.Close()
	<-p.done
}

// cappedBuffer accumulates output up to a limit and then drops the rest.
//
// Dropping rather than failing the call: a CLI whose output overran the cap
// has almost certainly already emitted its answer, and turning that into an
// error would lose a completion the operator paid for. The overrun is
// reported by [cappedBuffer.Truncated] so the caller can say so.
type cappedBuffer struct {
	buf     bytes.Buffer
	limit   int
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.dropped += len(p)
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.dropped += len(p) - room
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// Truncated is how many bytes were dropped past the cap.
func (c *cappedBuffer) Truncated() int { return c.dropped }

// tail returns the last [stderrTail] lines of s, with a marker when lines
// were dropped.
//
// The depth is a constant rather than a parameter because there is one honest
// answer to "how much of a crashing CLI's stderr does an operator need", and
// every caller here is reporting exactly that; a per-call number would be a
// knob nobody has a reason to set differently.
func tail(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= stderrTail {
		return strings.Join(lines, "\n")
	}
	kept := lines[len(lines)-stderrTail:]
	return fmt.Sprintf("… %d earlier lines omitted …\n%s",
		len(lines)-stderrTail, strings.Join(kept, "\n"))
}

// extract reads a CLI's stdout into the answer and its token counts.
type extracted struct {
	text       string
	input      int
	output     int
	cacheRead  int
	cacheWrite int
	// reasoning is reasoning tokens a CLI reports APART from output, which
	// the completion adds to the output count — see [cliprofile.UsagePaths].
	reasoning int
	// toolPrompt is the prompt a model-side tool put in front of the model,
	// reported APART from the input count, which the completion adds to it —
	// see [cliprofile.UsagePaths].
	toolPrompt int
	// reported is false when no usage path resolved, so the counts are
	// estimates. `crewlet llm doctor` prints which of the two a provider
	// gets, because a budget built on estimates is a different promise.
	reported bool
	// failed is the CLI's own is_error flag, for a vendor that reports a
	// failure inside a successful exit.
	failed bool

	// located reports that the profile FOUND where this CLI puts its
	// answer: a declared text path resolved to a string — empty or not —
	// or the output mode makes the whole of stdout the answer.
	//
	// THREE-VALUED EXTRACTION, and it has to be. "The model answered with
	// nothing" and "this profile does not describe this CLI's output" are
	// different facts about a run, and collapsing them into an empty
	// string is what made a Claude Code telemetry envelope — session id,
	// millisecond timings, a token breakdown, `"result":""` — render on a
	// dashboard as the sentence an agent had spoken. The fallback that
	// did it read `if out.text == "" { out.text = stdout }`, which cannot
	// tell the two apart because [firstString] returned "" for both.
	//
	// Set by [extract] only; there is no other constructor, which is why
	// the false zero value is not a claim about anything.
	located bool
}

// extract pulls the answer and the usage out of one CLI's stdout.
func extract(p cliprofile.Profile, stdout string) extracted {
	switch p.EffectiveOutput() {
	case cliprofile.OutputText:
		// Located by definition: a text profile declares that stdout IS
		// the answer, so there is no path that could fail to resolve.
		return extracted{text: strings.TrimSpace(stdout), located: true}
	case cliprofile.OutputJSONL:
		return extractStream(p, stdout)
	default:
		return extractObject(p, stdout)
	}
}

// extractObject reads one JSON document.
//
// A CLI that printed a banner before its JSON is common enough to handle
// here: the outermost braces are tried when the whole of stdout does not
// parse, which costs nothing and saves an operator an override.
func extractObject(p cliprofile.Profile, stdout string) extracted {
	doc, ok := decodeObject(strings.TrimSpace(stdout))
	if !ok {
		if bare, found := outermostObject(stdout); found {
			doc, ok = decodeObject(bare)
		}
	}
	if !ok {
		// Not JSON at all: the CLI printed prose, which is still an
		// answer. Reporting an unparseable-output error here would fail
		// a turn over a vendor's banner.
		//
		// LOCATED, unlike the resolved-nothing case below: there was no
		// document to look inside, so the whole reply is the answer on
		// the same reading a text profile takes. It is also the path a
		// spent subscription arrives on — the vendor's sentence about
		// the plan, printed plain on a zero exit — and losing it would
		// cost the marker classification its haystack.
		return extracted{text: strings.TrimSpace(stdout), located: true}
	}
	text, located := firstString(doc, p.TextPaths)
	out := extracted{text: text, located: located}
	out.failed = firstBool(doc, p.ErrorPaths)
	out.input, out.reported = firstInt(doc, p.Usage.Input)
	if got, ok := firstInt(doc, p.Usage.Output); ok {
		out.output, out.reported = got, true
	}
	out.cacheRead, _ = firstInt(doc, p.Usage.CacheRead)
	out.cacheWrite, _ = firstInt(doc, p.Usage.CacheWrite)
	out.reasoning, _ = firstInt(doc, p.Usage.Reasoning)
	out.toolPrompt, _ = firstInt(doc, p.Usage.ToolPrompt)
	return out
}

// outermostObject returns the span from the first '{' to the last '}'.
func outermostObject(reply string) (string, bool) {
	start := strings.IndexByte(reply, '{')
	end := strings.LastIndexByte(reply, '}')
	if start < 0 || end <= start {
		return "", false
	}
	return reply[start : end+1], true
}

// extractStream reads a JSONL event stream.
//
// Text CONCATENATES in stream order — an event stream spells one answer
// across several events — while usage figures take the LAST value found,
// because a stream reports a running total and the final one is the total.
func extractStream(p cliprofile.Profile, stdout string) extracted {
	e := streamExtractor{p: p}
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if doc, ok := decodeObject(line); ok {
			e.event(doc)
		}
	}
	return e.result()
}

// streamExtractor is [extractStream] one event at a time: what a JSONL stream
// says about its answer, its failure flag and its usage, read as the lines
// arrive — by the extraction of a finished stdout, and by a run whose record is
// read while it goes ([recordReader]), so the two cannot read one stream two
// ways.
type streamExtractor struct {
	p    cliprofile.Profile
	out  extracted
	text strings.Builder
}

// event reads one decoded line.
func (e *streamExtractor) event(doc map[string]any) {
	p, out := e.p, &e.out
	if chunk, ok := textOf(p, doc); ok {
		// LOCATED ON THE FIRST EVENT THAT CARRIES THE PATH, even when that
		// event's text is empty: a stream spells one answer across many
		// events and an empty fragment is an ordinary part of one. What
		// matters is whether this profile recognises the stream's shape at
		// all.
		out.located = true
		if chunk != "" {
			if e.text.Len() > 0 {
				e.text.WriteString("\n")
			}
			e.text.WriteString(chunk)
		}
	}
	if firstBool(doc, p.ErrorPaths) {
		out.failed = true
	}
	if got, ok := firstInt(doc, p.Usage.Input); ok {
		out.input, out.reported = got, true
	}
	if got, ok := firstInt(doc, p.Usage.Output); ok {
		out.output, out.reported = got, true
	}
	if got, ok := firstInt(doc, p.Usage.CacheRead); ok {
		out.cacheRead = got
	}
	if got, ok := firstInt(doc, p.Usage.CacheWrite); ok {
		out.cacheWrite = got
	}
	if got, ok := firstInt(doc, p.Usage.Reasoning); ok {
		out.reasoning = got
	}
	if got, ok := firstInt(doc, p.Usage.ToolPrompt); ok {
		out.toolPrompt = got
	}
}

// result is what the stream said, so far.
func (e *streamExtractor) result() extracted {
	out := e.out
	out.text = strings.TrimSpace(e.text.String())
	return out
}

// usageOutcome is what [extracted.applyUsageReport] made of a usage report.
//
// THREE WAYS TO TAKE NOTHING, and only one of them is silent. No report to read
// is a profile that reads none, or a report the place that obtains it already
// failed to get and said so. A report that arrived and could not be read is
// different: the CLI did its part and this profile no longer describes what it
// printed — a release that renamed `data.tokens` — so every call is charged an
// estimate, and nothing would say why if this did not.
type usageOutcome int

const (
	// usageNoReport: the profile reads no report, or none arrived.
	usageNoReport usageOutcome = iota
	// usageApplied: both prompt counts came from the report.
	usageApplied
	// usageUndecodable: the report is not one JSON object.
	usageUndecodable
	// usageUnresolved: it decodes, and usage.input and usage.output do not
	// both resolve in it.
	usageUnresolved
)

// unreadable reports whether a report arrived and none of it was taken.
func (o usageOutcome) unreadable() bool {
	return o == usageUndecodable || o == usageUnresolved
}

// String is the reason a warning names.
func (o usageOutcome) String() string {
	switch o {
	case usageApplied:
		return "applied"
	case usageUndecodable:
		return "the report is not one JSON object"
	case usageUnresolved:
		return "usage.input and usage.output do not both resolve in it"
	default:
		return "no report"
	}
}

// applyUsageReport overlays a vendor's SEPARATE usage report on what stdout
// said, for a profile whose CLI writes one or prints one to a usage command —
// see [cliprofile.Profile.UsageFileArgs] and [cliprofile.Profile.UsageArgs] —
// and says what it made of it.
//
// BOTH PROMPT COUNTS COME FROM THE FILE OR NEITHER DOES. A partial overlay
// would pair one source's input count with another's output count, and the
// sum is what a budget is charged. The two cache figures are not part of that
// test: a provider that caches nothing reports neither, and zero is the true
// answer there.
//
// Taking nothing leaves the counts to whatever stdout reported, which for a
// CLI that reports none there is the estimate.
func (e *extracted) applyUsageReport(p cliprofile.Profile, raw string) usageOutcome {
	if !p.ReadsUsageReport() || strings.TrimSpace(raw) == "" {
		return usageNoReport
	}
	doc, ok := decodeObject(strings.TrimSpace(raw))
	if !ok {
		return usageUndecodable
	}
	input, gotInput := firstInt(doc, p.Usage.Input)
	output, gotOutput := firstInt(doc, p.Usage.Output)
	if !gotInput || !gotOutput {
		// The report exists but this profile cannot read both counts
		// out of it, which is drift rather than a zero-token call —
		// better left to what stdout said than charging a real output
		// count against a prompt this build read as zero.
		return usageUnresolved
	}
	e.input, e.output, e.reported = input, output, true
	e.cacheRead, _ = firstInt(doc, p.Usage.CacheRead)
	e.cacheWrite, _ = firstInt(doc, p.Usage.CacheWrite)
	e.reasoning, _ = firstInt(doc, p.Usage.Reasoning)
	e.toolPrompt, _ = firstInt(doc, p.Usage.ToolPrompt)
	return usageApplied
}

// usageSource names the profile field a usage report came through, for the
// warning an unreadable one logs. The two are exclusive
// ([cliprofile.Profile.Validate]).
func usageSource(p cliprofile.Profile) string {
	if len(p.UsageFileArgs) > 0 {
		return "usage_file_args"
	}
	return "usage_args"
}

// textOf reads the assistant's text out of ONE line of a jsonl stream,
// honouring the profile's event filter.
//
// A line whose discriminator is not one this profile calls text is not a
// missing path — it is an event about something else, and it must not even
// count as "located": a stream that spliced a tool's output into the reply
// and then repeated the reply would look, to every frame downstream, exactly
// like a model that had said all of it.
func textOf(p cliprofile.Profile, doc map[string]any) (string, bool) {
	if len(p.EventTypePath) == 0 {
		return firstString(doc, p.TextPaths)
	}
	kind, ok := lookup(doc, p.EventTypePath)
	if !ok {
		return "", false
	}
	name, isString := kind.(string)
	if !isString || !slices.Contains(p.TextEvents, name) {
		return "", false
	}
	return firstString(doc, p.TextPaths)
}

// classifyFailureEvents is the [cliprofile.MarkerScopeEvents] scope: every
// stdout line decoded, only those whose discriminator names one of the CLI's
// own failure events kept, and each sentinel compared by EQUALITY with the
// string at the profile's marker path in such an event.
//
// NEVER A SUBSTRING SEARCH, which is what this scope was first: OpenCode 2
// prints a tool call's arguments as the model's own raw JSON object, so a
// model can put any sentinel on stdout spelled exactly as the CLI spells it,
// on a healthy call. What it cannot do is change the type of the event its
// words arrive in, or move them to where the path points in an event of the
// failure type — the CLI writes those. A line that does not decode, has no
// discriminator, or whose class is not a string is no failure report.
//
// Limit markers before auth markers, the precedence the text scopes keep.
// The matched event's whole line is what the operator is shown: it carries
// the provider's own message beside the class.
func classifyFailureEvents(p cliprofile.Profile, stdout string) (markerHit, bool) {
	type failure struct{ class, line string }
	var failures []failure
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimSpace(line)
		doc, ok := decodeObject(line)
		if !ok {
			continue
		}
		kind, _ := lookup(doc, p.EventTypePath)
		if name, isString := kind.(string); !isString || !slices.Contains(p.MarkerEvents, name) {
			continue
		}
		value, _ := lookup(doc, p.MarkerPath)
		if class, isString := value.(string); isString {
			failures = append(failures, failure{class: class, line: line})
		}
	}
	for _, marker := range p.LimitMarkers {
		for _, f := range failures {
			if f.class == marker.Sentinel {
				return markerHit{Kind: llm.KindRateLimit, Said: f.line, Source: sourceEvent}, true
			}
		}
	}
	for _, marker := range p.AuthMarkers {
		for _, f := range failures {
			if f.class == marker.Sentinel {
				return markerHit{Kind: llm.KindAuth, Said: f.line, Source: sourceEvent}, true
			}
		}
	}
	return markerHit{}, false
}

// limitEventHit is one decoded stdout event read as the CLI's own structured
// limit report ([cliprofile.LimitEvent]), when it meets every condition of
// one: the vendor's sentence is the string at the event's said path, or the
// whole line where it names none, and the reset is read from that sentence as
// of the run's end, in its zone. [classifyEvents] walks the stream.
func limitEventHit(p cliprofile.Profile, doc map[string]any, line string, out cliOutput) (markerHit, bool) {
	for _, event := range p.LimitEvents {
		if !matches(doc, event.Match) {
			continue
		}
		said := line
		if text, found := textAt(doc, event.SaidPath); found {
			said = text
		}
		return markerHit{Kind: llm.KindRateLimit, Reset: resetAt(said, event.Reset, out.at, out.zone),
			Said: said, Source: sourceEvent}, true
	}
	return markerHit{}, false
}

func decodeObject(s string) (map[string]any, bool) {
	if s == "" || s[0] != '{' {
		return nil, false
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return nil, false
	}
	return doc, true
}

// lookup walks one path into a decoded document.
func lookup(doc map[string]any, path cliprofile.Path) (any, bool) {
	var current any = doc
	for _, step := range path {
		next, ok := child(current, step)
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

// child is one step of a path: an object's member by key, or an array's
// element by decimal index.
func child(node any, step string) (any, bool) {
	switch n := node.(type) {
	case map[string]any:
		next, ok := n[step]
		return next, ok
	case []any:
		idx, err := strconv.Atoi(step)
		if err != nil || idx < 0 || idx >= len(n) {
			return nil, false
		}
		return n[idx], true
	default:
		return nil, false
	}
}

// members is every value of an object or an array, for a
// [cliprofile.PathEvery] step; nothing else has members.
func members(node any) []any {
	switch n := node.(type) {
	case map[string]any:
		out := make([]any, 0, len(n))
		for _, v := range n {
			out = append(out, v)
		}
		return out
	case []any:
		return n
	default:
		return nil
	}
}

// firstString reads the answer out of a decoded document, and reports whether
// any declared path RESOLVED at all.
//
// The bool is the whole point, and it is not the same as a non-empty return:
// a path that resolved to "" says the CLI answered with nothing, a path that
// resolved to nothing says this profile no longer describes this CLI. Callers
// that collapse the two hand a vendor's telemetry back as the model's words —
// see [extracted.located].
//
// A resolved-but-empty path does NOT stop the walk: `text_paths` is a list
// precisely so a vendor that moved the field between releases needs no
// override, and cursor-agent's `[["result"], ["response"]]` depends on an
// empty `result` falling through to `response`. So the first NON-EMPTY hit
// wins, and an all-empty walk still reports located.
func firstString(doc map[string]any, paths []cliprofile.Path) (string, bool) {
	located := false
	for _, path := range paths {
		v, ok := lookup(doc, path)
		if !ok {
			continue
		}
		s, isString := v.(string)
		if !isString {
			continue
		}
		located = true
		if s != "" {
			return s, true
		}
	}
	return "", located
}

func firstBool(doc map[string]any, paths []cliprofile.Path) bool {
	for _, path := range paths {
		v, ok := lookup(doc, path)
		if !ok {
			continue
		}
		if b, isBool := v.(bool); isBool {
			return b
		}
	}
	return false
}

// firstInt reads a token count: the first path that resolves to one wins.
func firstInt(doc map[string]any, paths []cliprofile.Path) (int, bool) {
	for _, path := range paths {
		if n, ok := countAt(doc, path); ok {
			return n, true
		}
	}
	return 0, false
}

// countAt reads the count one path names. At a [cliprofile.PathEvery] step it
// is the SUM of the count the rest of the path names under every member, and it
// resolves when at least one member's does: a member without the count (a model
// a call never billed) adds nothing, and a path no member resolves is a profile
// that no longer describes its CLI, which the caller turns into an estimate
// rather than a zero.
func countAt(node any, path cliprofile.Path) (int, bool) {
	for i, step := range path {
		if step == cliprofile.PathEvery {
			sum, found := 0, false
			for _, member := range members(node) {
				if n, ok := countAt(member, path[i+1:]); ok {
					sum, found = sum+n, true
				}
			}
			return sum, found
		}
		next, ok := child(node, step)
		if !ok {
			return 0, false
		}
		node = next
	}
	return asCount(node)
}

// asCount reads one count, accepting the float64 every JSON number decodes to
// as well as a string, which some CLIs emit for large counts.
func asCount(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case json.Number:
		if parsed, err := n.Int64(); err == nil {
			return int(parsed), true
		}
	case string:
		if parsed, err := strconv.Atoi(n); err == nil {
			return parsed, true
		}
	}
	return 0, false
}
