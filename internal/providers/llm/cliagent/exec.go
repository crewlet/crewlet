package cliagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/textcut"
)

// termGrace is how long a terminated CLI has to exit before it is killed.
//
// The timeout has already fired, so the call is lost either way; this decides
// only whether the tree gets to flush. Five seconds is enough for a Node
// runtime to run its exit handlers and close its sockets, and short enough
// that a wedged child does not hold the seat's concurrency slot for a
// meaningful fraction of the next call. Same figure the sandbox uses for the
// same reason.
const termGrace = 5 * time.Second

// stderrTail is how many lines of a child's stream an operator-facing failure
// message quotes: the LAST fifty. Same figure the MCP supervisor uses.
//
// The stream is read after the process has exited, so its end is what the
// process said last, and the message quotes that rather than the whole of it
// — a stream can be [maxOutput] long, and a failure message is logged,
// published on the phase's event and rendered on the dashboard.
//
// WHERE THE EARLIER LINES ARE: in the engine's log, on one condition.
// [renderTail] emits every line the window leaves out as a
// cli_agent_omitted_line debug event, in order and numbered, and the window's
// own marker says so — so on a node logging at debug level the whole
// retained stream is there. Above debug the count in the marker is all that
// survives of them, which is the honest limit of the route and the same one
// the MCP supervisor's tailLines states. What [maxOutput] dropped before any
// of this is held nowhere; see [tail].
const stderrTail = 50

// maxOutput bounds what is read from a child's stdout and stderr.
//
// A CLI told to stream JSONL can emit tens of megabytes for a long run, and
// all of it is held in memory to be parsed. 32 MiB is far above any real
// completion — the largest observed is under 2 MiB — and far below what would
// put the engine under memory pressure with this provider's children all
// running: the ceiling on those is providers.llm.<key>.cli.max_concurrent, 4
// by default, which is the semaphore [New] builds and NOT node.max_concurrent
// (that one counts agent TURNS). Two buffers per child at 32 MiB is 256 MiB
// worst case beside the 200-400 MB resident each CLI costs anyway, and the
// worst case needs every child to overrun at once.
//
// A CONSTANT RATHER THAN A CONFIG FIELD, decided here rather than left open:
// both ends of the range are fixed by facts an operator cannot move — a real
// answer is an order of magnitude below it, and a process holding more than
// this per concurrent call is a memory problem whatever anybody believes about
// it. The refusal an overrun produces used to tell them to "raise
// cli.max_output_bytes", and that field has never existed in either config
// tier: `crewlet validate` refuses it as unknown, so the single remedy the
// message named was a dead end that read like a supported knob. What an
// overrun actually means is a profile streaming a whole transcript where it
// could take one terminal event (`event_type_path` and `text_events`, which
// [Profile.validate] refuses one without the other of), or a model that will
// not stop, and those are what the refusal names now.
//
// Overrunning it is never silent, and [cappedBuffer]'s doc enumerates every
// message that says so rather than asserting that they all do — a blanket
// claim there is what hid the one that did not. In short: stdout's overrun
// REFUSES a completion ([Provider.completion]) because a clipped answer is not
// an answer; a version probe REFUSES the one shape that can reach its line
// ([Provider.probeVersion]); and anything else rendered out of either stream
// is MARKED with that stream's own count ([tail], [rawResult.markerText]).
// [run] logs cli_agent_output_truncated beside all of them.
const maxOutput = 32 << 20

// invocation is one CLI call, fully resolved.
type invocation struct {
	binary  string
	args    []string
	stdin   string
	dir     string
	env     []string
	timeout time.Duration

	// onLine, when set, receives each COMPLETE line of stdout as the child
	// writes it, before the call returns. Nil takes the ordinary path where
	// stdout is only read after the process exits.
	//
	// Called from the goroutine os/exec runs the output copier on — one
	// goroutine, in order, and cmd.Wait does not return until it has
	// finished, so it never runs concurrently with the caller's own use of
	// the result.
	onLine func(string)
}

// lineTee forwards complete lines to a callback while passing every byte
// through to the real sink.
//
// A CLI's stdout arrives in whatever chunks the pipe hands over, which split
// mid-line, so a consumer that wants JSONL events has to reassemble them. The
// tail after the last newline is deliberately NOT delivered: a partial JSON
// object is not parseable, and the buffered copy is what the extractor reads
// once the process exits, so nothing is lost by waiting.
type lineTee struct {
	sink io.Writer
	on   func(string)
	buf  []byte
}

func (t *lineTee) Write(b []byte) (int, error) {
	n, err := t.sink.Write(b)
	if n > 0 {
		t.buf = append(t.buf, b[:n]...)
		for {
			i := bytes.IndexByte(t.buf, '\n')
			if i < 0 {
				break
			}
			line := string(t.buf[:i])
			t.buf = t.buf[i+1:]
			t.on(line)
		}
	}
	return n, err
}

// rawResult is what a child produced.
type rawResult struct {
	stdout   string
	stderr   string
	exitCode int
	// timedOut reports that the wall-clock cap fired, which is a different
	// fact from the context the caller passed being cancelled.
	timedOut bool

	// droppedStdout and droppedStderr are the bytes each stream lost past
	// maxOutput. SEPARATE, because the two mean different things: a clipped
	// stdout is a clipped ANSWER and the caller must refuse it, while a
	// clipped stderr costs only diagnosis. Summing them, as this used to,
	// produced a warning that could not say which had happened.
	droppedStdout, droppedStderr int
}

// stderrTailText and stdoutTailText render one stream for an operator-facing
// message, each carrying ITS OWN drop count.
//
// THE PAIRING IS THE POINT and it is why [tail] is never called directly from
// here — every render goes through [renderTail], with the count of the
// stream it was handed. tail cannot say a stream was shortened unless it is
// told, and the count it needs sits in a field beside three others of the
// same type — so a caller free to pass one would be free to pass stderr's
// count for stdout's text, which renders a marker for a loss that did not
// happen and no marker for one that did. Two methods and no loose call sites
// means the wrong pairing cannot be written.
func (r *rawResult) stderrTailText(ctx context.Context) string {
	return renderTail(ctx, log, "stderr", r.stderr, r.droppedStderr)
}

func (r *rawResult) stdoutTailText(ctx context.Context) string {
	return renderTail(ctx, log, "stdout", r.stdout, r.droppedStdout)
}

// failureTailText renders whichever stream has something to say about a failed
// run, with the drop count of the stream it chose.
//
// parsed is the answer [extract] read out of stdout, so STDOUT's count is the
// one that belongs beside it: the cap that clipped that stream is the only
// loss that can have reached what was read out of it. Not a count OF parsed —
// a JSONL profile assembles its answer out of decoded events
// ([extractStream]), so the bytes the cap dropped are the stream's own and
// [tail] reports them as that.
func (r *rawResult) failureTailText(ctx context.Context, parsed string) string {
	if strings.TrimSpace(r.stderr) != "" {
		return r.stderrTailText(ctx)
	}
	if strings.TrimSpace(parsed) != "" {
		return renderTail(ctx, log, "stdout", parsed, r.droppedStdout)
	}
	return r.stdoutTailText(ctx)
}

// stderrDetailText appends a CLI's stderr to a message, or nothing when it
// wrote none.
//
// A trailing empty ":" after a sentence that already said what went wrong is
// how a message stops reading like one — and stderr is genuinely absent on the
// paths that use this, because a CLI that exits 0 usually says nothing there.
func (r *rawResult) stderrDetailText(ctx context.Context) string {
	if strings.TrimSpace(r.stderr) == "" {
		return ""
	}
	return " It wrote on stderr:\n" + r.stderrTailText(ctx)
}

// stdoutFirstLine is the opening line of stdout, and whether [maxOutput] could
// have fallen INSIDE that line.
//
// The pairing again, for the one caller that wants a line rather than a tail.
// A version probe reads the first line and nothing else, and the cap keeps the
// HEAD of a stream — so an overrun normally costs a version probe nothing at
// all: whatever ran past the cap came after the newline that ended the version
// string. The single case where it does cost something is a stdout with no
// newline anywhere in the retained bytes, because then the first line IS where
// the cap fell and what survives is a prefix of it.
//
// ASKED, NOT INFERRED, which is why this returns the fact rather than the
// caller deriving it: the two inputs to that answer — the drop count and
// whether the retained text ever broke a line — sit in different places, and a
// caller reading only c.dropped would refuse every long-but-fine version probe
// while a caller reading only the newline would refuse none of the broken
// ones.
func (r *rawResult) stdoutFirstLine() (line string, cut bool) {
	head, _, broke := strings.Cut(r.stdout, "\n")
	return strings.TrimSpace(head), r.droppedStdout > 0 && !broke
}

// markerText renders a classified sentinel hit for the operator: the vendor's
// own line, plus — where the CLI said more than that line — what it is one
// line OF, and the drop count of the stream it came out of.
//
// THE LINE IS SELECTED, NOT CUT, and that distinction is the reason this is
// not simply a call to [tail]. [classifyMarkers] hands back the line the
// sentinel matched IN, which is the vendor's sentence wherever the vendor put
// it; a prefix or a tail of the stream would be the first or last event of a
// JSONL transcript and say nothing about the plan. But a selection a reader
// cannot tell from a whole reply fails the same way an unmarked cut does — so
// where the stream held more, the message says how much more.
//
// AND SAYS THE REST IS KEPT NOWHERE, because it is. A CLI's stdout and stderr
// are read once into [cappedBuffer]s, parsed, and dropped with the call: there
// is no store column, no event payload and no retrieval tool that holds them,
// and the classified error this line becomes carries only itself. An invented
// recovery route would be worse than the admitted gap, and the gap is the
// argument for selecting the vendor's own sentence rather than an arbitrary
// window of the stream.
//
// THE STREAM IS ASKED, NOT INFERRED — [markerHit.Stream] is set by the
// classifier because it is the only frame that knows which haystack it found
// the sentinel in. A caller free to choose would be free to pair the line with
// the OTHER stream's drop count: a marker for a loss that did not happen and
// none for one that did, which is precisely the mistake [rawResult]'s paired
// methods exist to make unwritable.
//
// located is the HAYSTACK, not the stream. [Provider.completion] hands
// [classifyMarkers] the extractor's answer where one resolved and stdout where
// none did, and passes this the same value — so located is what
// [markerHit.Said] was taken out of, and the only honest fallback for a hit
// carrying no line.
//
// THE SIZE AND THE "IS THAT ALL OF IT" TEST COME FROM THE STREAM, because what
// the sentence below claims is what the CLI wrote. On a JSONL profile located
// is a few hundred bytes of answer assembled out of a transcript of thousands
// ([extractStream]): measuring located would tell an operator stdout held the
// answer's own length, and testing the matched line against located returns
// the bare line whenever the answer IS that line — the unmarked selection this
// function exists to prevent. The drop count still comes from the stream
// [markerHit.Stream] names, so the pairing is untouched.
func (r *rawResult) markerText(hit markerHit, located string) string {
	stream, haystack, dropped := r.stdout, located, r.droppedStdout
	if hit.Stream == markerStderr {
		stream, haystack, dropped = r.stderr, r.stderr, r.droppedStderr
	}
	line := nonEmpty(hit.Said, firstLine(haystack))
	if strings.TrimSpace(stream) == line && dropped == 0 {
		// The line IS everything that stream said, so there is nothing to
		// mark. A standing clause about a remainder that does not exist
		// is how a reader learns to skip the sentence it hangs off.
		return line
	}
	detail := fmt.Sprintf("%d bytes on %s", len(stream), hit.Stream)
	if dropped > 0 {
		detail += fmt.Sprintf(", with a further %d that the engine's %d-byte output "+
			"cap dropped before this build ever saw them", dropped, maxOutput)
	}
	return fmt.Sprintf("%s … that is the line this profile's sentinel matched, out of "+
		"%s; the rest is the CLI's own output and is kept nowhere …", line, detail)
}

// run executes one invocation, terminating the whole process TREE on timeout.
//
// The tree, not the process: a coding CLI is a launcher over a Node or Bun
// runtime that forks helpers, and signalling only the process Go started
// leaves those holding the seat's memory and its network sockets.
func run(ctx context.Context, in invocation) (*rawResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, in.timeout)
	defer cancel()

	cmd := exec.CommandContext(callCtx, in.binary, in.args...) //nolint:gosec // binary and args come from a validated profile
	cmd.Dir = in.dir
	cmd.Env = in.env
	if in.stdin != "" {
		cmd.Stdin = strings.NewReader(in.stdin)
	}
	var stdout, stderr cappedBuffer
	stdout.limit = maxOutput
	stderr.limit = maxOutput
	cmd.Stdout = io.Writer(&stdout)
	if in.onLine != nil {
		cmd.Stdout = &lineTee{sink: &stdout, on: in.onLine}
	}
	cmd.Stderr = &stderr
	procgroup.Set(cmd)

	// Cancel signals the GROUP rather than letting os/exec signal the one
	// process it knows about, and WaitDelay bounds how long a child that
	// ignored SIGTERM can hold the call open.
	cmd.Cancel = func() error { return procgroup.Terminate(cmd.Process.Pid) }
	cmd.WaitDelay = termGrace

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cli-agent: starting %q: %w", in.binary, err)
	}
	pgid := cmd.Process.Pid
	waitErr := cmd.Wait()

	res := &rawResult{
		stdout:   stdout.String(),
		stderr:   stderr.String(),
		timedOut: errors.Is(callCtx.Err(), context.DeadlineExceeded),
	}
	res.droppedStdout, res.droppedStderr = stdout.Truncated(), stderr.Truncated()
	for _, s := range []struct {
		stream  string
		dropped int
	}{{"stdout", res.droppedStdout}, {"stderr", res.droppedStderr}} {
		if s.dropped == 0 {
			continue
		}
		// NAMES THE STREAM. A truncated stream is how a usage figure or a
		// closing fence goes missing, and an operator debugging a mangled
		// answer needs to know which half was cut — the caller refuses on
		// stdout and carries on for stderr.
		log.WarnContext(ctx, "cli_agent_output_truncated", "binary", in.binary,
			"stream", s.stream, "dropped_bytes", s.dropped, "limit_bytes", maxOutput)
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
	if err := callCtx.Err(); err != nil {
		// The group was signalled by Cancel and given WaitDelay to go;
		// anything still alive after Wait returned is a survivor that
		// ignored SIGTERM, and it holds a concurrency slot until killed.
		if killErr := procgroup.Kill(pgid); killErr != nil && !errors.Is(killErr, errors.ErrUnsupported) {
			log.WarnContext(ctx, "cli_agent_group_kill_failed",
				"pgid", pgid, "reason", err, "error", killErr)
		}
	}

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		return res, nil
	case errors.As(waitErr, &exitErr):
		res.exitCode = exitErr.ExitCode()
		return res, nil
	default:
		return res, fmt.Errorf("cli-agent: running %q: %w", in.binary, waitErr)
	}
}

// cappedBuffer accumulates output up to a limit and then drops the rest.
//
// Dropping rather than failing the call: a CLI whose output overran the cap
// has almost certainly already emitted its answer, and turning that into an
// error would lose a completion the operator paid for. The overrun is
// reported by [cappedBuffer.Truncated] so the caller can say so, and the
// claim that every caller does is ENUMERATED rather than asserted — [run] has
// exactly two call sites, and this lists what each one renders:
//
//   - A COMPLETION ([Provider.Complete], which is also what every `doctor`
//     probe goes through). A clipped stdout is REFUSED naming the cap, because
//     a clipped answer is not an answer ([Provider.completion]); a stderr tail
//     is MARKED with its own count ([tail], reachable only through
//     [rawResult]'s paired methods); and the single line a limit or auth
//     sentinel matched on carries the count of the stream it was taken out of
//     ([rawResult.markerText]).
//   - A VERSION PROBE ([Provider.probeVersion]). It reads one line, so the cap
//     can only reach that line when the retained stdout holds no newline at
//     all; [rawResult.stdoutFirstLine] is what says whether it did, and the
//     probe REFUSES the prefix naming version_args rather than reporting it as
//     a version.
//
// [run] also logs cli_agent_output_truncated at warn for either stream before
// any of those renders, and that line names the STREAM as well as the cap,
// which no single message above can — but a log line is not an operator-facing
// message, and the list above is about the messages. A cut nobody can see is
// the failure this type would otherwise be.
//
// # The cut lands on a rune boundary
//
// The cap falls wherever the pipe happened to be, which is mid-character for
// any output that is not ASCII: a progress frame's box-drawing runes, a
// model's em dash, a path with a non-Latin filename in it. A bare p[:room]
// splits that character and leaves the buffer invalid UTF-8 — the one shared
// rule [github.com/crewlet/crewlet/internal/textcut] exists for, where a JSON
// encoder substitutes U+FFFD, a model reads a replacement character and a
// terminal prints a box. stderr is the reachable path: it is pasted into a
// classified error, which is logged, stored as an event and rendered on the
// dashboard.
//
// The walk that repairs it IS textcut's — [textcut.TrimSplitRune], the edge
// sibling of the three budget cuts, which answers the only question left once
// a cap has been spent: does what remains END on a whole character. The
// engine's other capped buffer, internal/sandbox's control-output capture,
// asks it of its own head window, and the two asked it in two identical
// private copies until this one moved. What stays here is the part that is
// genuinely this type's: see [cappedBuffer.trimSplitRune] for why the question
// has to be asked of the buffer rather than of the chunk being written.
//
// # What it holds is a PREFIX, and it stops at the first byte it loses
//
// Once anything has been dropped nothing more is appended, however much room
// the rune trim just handed back. Two separate things go wrong otherwise, and
// the second is the reason the first is not merely cosmetic:
//
//   - What follows a cut is not contiguous with what precedes it, so
//     appending it would splice two byte ranges the stream never had next to
//     each other and present the result as the CLI's opening output. An
//     operator reading a crash trace would be shown a line that never
//     followed the one above it.
//   - The bytes that arrive first to fill that room are the CONTINUATION
//     BYTES of the character the trim just removed — they fit it exactly, by
//     construction — so a buffer that kept accepting would put the invalid
//     encoding straight back, one byte at a time, on precisely the input
//     shape this type is written for: a pipe handing over small reads.
type cappedBuffer struct {
	buf     bytes.Buffer
	limit   int
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	// dropped > 0 IS the sealed state rather than a flag beside it: the
	// only way to drop a byte is to reach the cap, and the only way to
	// reach the cap is to stop accepting. A second field could disagree
	// with this one; a derived answer cannot.
	if c.dropped > 0 {
		c.dropped += len(p)
		return len(p), nil
	}
	room := max(c.limit-c.buf.Len(), 0)
	if len(p) <= room {
		// Everything fit, so the cap took nothing and there is no cut of
		// ours to undo. Whatever this leaves at the end of the buffer is
		// the stream's own shape, and the next Write continues it. A
		// chunk that fills the buffer EXACTLY is this case too: nothing
		// has been lost yet, and if the stream ends here nothing ever
		// will be — trimming on a full-but-intact buffer would remove a
		// character the reader was entitled to.
		c.buf.Write(p)
		return len(p), nil
	}
	c.buf.Write(p[:room])
	c.dropped = len(p) - room
	// THE CUT IS TAKEN HERE, EXACTLY ONCE, on the write that first loses a
	// byte — which is also the only write that can know what was lost. It
	// is not always the write that overran: a chunk filling the buffer to
	// the limit returns above intact, and the cut falls to the NEXT chunk,
	// which reaches this line with room == 0 and p[:0] written. That is the
	// ordinary shape of a streaming CLI, so it is the case that matters.
	//
	// The split character counts as dropped like every byte past the cap,
	// because to every reader of this number that is what it is: bytes the
	// reader is not being shown.
	c.dropped += c.trimSplitRune()
	return len(p), nil
}

// trimSplitRune drops a trailing character the CAP split, and reports how
// many bytes that took.
//
// IT READS THE BUFFER, NOT THE CHUNK just written, because a rune straddles
// two Writes whenever the pipe hands one over: os/exec copies whatever a read
// returned, so a lead byte can arrive in one chunk and its continuation bytes
// in the next. Walking back inside the chunk alone leaves the case where the
// walk consumes all of it — the buffer then still ends on a lead byte whose
// remainder was in the very chunk the cap cut away, which is the failure this
// function exists to prevent rather than a corner of it. In the case that
// matters most the chunk is not even readable: a buffer already at the limit
// contributes p[:0], so the split character is entirely behind the caller's
// view and only the buffer holds it.
//
// BYTES THE CLI ITSELF EMITTED BROKEN ARE LEFT ALONE — binary on stdout, a
// short write of its own, a lead byte no UTF-8 encoding has. Rewriting those
// would be this package claiming a cut it did not make, and would put bytes
// into a count that means "what the cap took". Which bytes those are is
// [textcut.TrimSplitRune]'s decision and its doc is where the argument for the
// predicate lives, because the sandbox's capture needs the same one and two
// copies of it would eventually stop agreeing. All this function does is ask
// it of the BUFFER and turn its answer into the truncation and the count.
func (c *cappedBuffer) trimSplitRune() int {
	b := c.buf.Bytes()
	kept := textcut.TrimSplitRune(b)
	if len(kept) == len(b) {
		return 0
	}
	c.buf.Truncate(len(kept))
	return len(b) - len(kept)
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// Truncated is how many bytes were dropped past the cap.
func (c *cappedBuffer) Truncated() int { return c.dropped }

// tail renders one of a child's streams for a message an operator will read:
// the last [stderrTail] lines of text, with a marker for everything the reader
// is NOT being shown.
//
// The depth is a constant rather than a parameter because there is one honest
// answer to "how much of a crashing CLI's stderr does an operator need", and
// every caller here is reporting exactly that; a per-call number would be a
// knob nobody has a reason to set differently.
//
// # Two cuts meet here, and the reader must be able to see both
//
// The first is this function's own: the lines before the last [stderrTail]
// are omitted, and the marker counts them and names the debug event
// [renderTail] logs each one as — which is why a message renders through that
// and never through this directly. The second was taken long before, at the
// far end of the stream, by [maxOutput] — dropped is what
// [cappedBuffer.Truncated] reported for THIS text, and it is the reason the
// marker cannot simply say "the last fifty lines". The cap keeps the HEAD, so
// a stream that overran it has no end left to take a tail from: these lines
// end where the cap fell, not where the process stopped, and an operator
// reading a crash trace has to know the crash itself may be the part that went
// missing. Unmarked, a stream cut at 32 MiB reads as a CLI that simply stopped
// talking.
//
// NEITHER CUT CAN LAND MID-CHARACTER: this one splits on '\n', and the cap
// walks back to a rune boundary (see [cappedBuffer]).
//
// dropped is passed rather than inferred because only the buffer that took the
// cut knows it, and [rawResult] pairs each stream with its own count so that no
// call site here has to pick one — the count for the stream a caller did NOT
// choose is exactly the mistake that pairing removes. The number of bytes is
// all that is recoverable: what the cap dropped is held nowhere, by design, so
// the flag IS the mechanism and cli_agent_output_truncated is where the limit
// that took them is named.
func tail(text string, dropped int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	out := strings.Join(lines, "\n")
	if len(lines) > stderrTail {
		out = fmt.Sprintf("… %d earlier lines omitted — at debug level the log "+
			"has each one as %s …\n%s", len(lines)-stderrTail, omittedLineEvent,
			strings.Join(lines[len(lines)-stderrTail:], "\n"))
	}
	if dropped <= 0 {
		return out
	}
	return fmt.Sprintf(
		"%s\n… and %d further bytes were dropped at the engine's output cap, so "+
			"this ends where the cap fell rather than where the CLI stopped …",
		out, dropped)
}

// omittedLineEvent is the debug event each line a [tail] window leaves out is
// logged as. A constant because the window's marker names it to the operator,
// and a marker naming an event nothing emits would send them looking for it.
const omittedLineEvent = "cli_agent_omitted_line"

// renderTail is [tail] for a message, with the lines it leaves out sent to the
// log at DEBUG — see [stderrTail] for why that is where they go.
//
// Every render of a stream goes through here rather than calling [tail]
// directly, because the marker [tail] writes says the omitted lines are in the
// log, and only this makes that true. The logger is a parameter so a test can
// read what was emitted without pointing the process-wide sink at a buffer.
func renderTail(ctx context.Context, logger *slog.Logger, stream, text string, dropped int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if omitted := len(lines) - stderrTail; omitted > 0 {
		for i, line := range lines[:omitted] {
			logger.DebugContext(ctx, omittedLineEvent, "stream", stream,
				"line_no", i+1, "line", line)
		}
	}
	return tail(text, dropped)
}

// extract reads a CLI's stdout into the answer and its token counts.
type extracted struct {
	text       string
	input      int
	output     int
	cacheRead  int
	cacheWrite int
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
func extract(p Profile, stdout string) extracted {
	switch p.output() {
	case OutputText:
		// Located by definition: a text profile declares that stdout IS
		// the answer, so there is no path that could fail to resolve.
		return extracted{text: strings.TrimSpace(stdout), located: true}
	case OutputJSONL:
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
func extractObject(p Profile, stdout string) extracted {
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
	return out
}

// extractStream reads a JSONL event stream.
//
// Text CONCATENATES in stream order — an event stream spells one answer
// across several events — while usage figures take the LAST value found,
// because a stream reports a running total and the final one is the total.
func extractStream(p Profile, stdout string) extracted {
	var out extracted
	var text strings.Builder
	for line := range strings.SplitSeq(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		doc, ok := decodeObject(line)
		if !ok {
			continue
		}
		if chunk, ok := textOf(p, doc); ok {
			// LOCATED ON THE FIRST EVENT THAT CARRIES THE PATH, even
			// when that event's text is empty: a stream spells one
			// answer across many events and an empty fragment is an
			// ordinary part of one. What matters is whether this
			// profile recognises the stream's shape at all.
			out.located = true
			if chunk == "" {
				continue
			}
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			text.WriteString(chunk)
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
	}
	out.text = strings.TrimSpace(text.String())
	return out
}

// applyUsageFile overlays a vendor's SEPARATE usage report on what stdout
// said, for a profile whose CLI writes one — see [Profile.UsageFileArgs].
//
// BOTH PROMPT COUNTS COME FROM THE FILE OR NEITHER DOES. A partial overlay
// would pair one source's input count with another's output count, and the
// sum is what a budget is charged. The two cache figures are not part of that
// test: a provider that caches nothing reports neither, and zero is the true
// answer there.
func (e *extracted) applyUsageFile(p Profile, raw string) {
	if len(p.UsageFileArgs) == 0 || strings.TrimSpace(raw) == "" {
		return
	}
	doc, ok := decodeObject(strings.TrimSpace(raw))
	if !ok {
		return
	}
	input, gotInput := firstInt(doc, p.Usage.Input)
	output, gotOutput := firstInt(doc, p.Usage.Output)
	if !gotInput || !gotOutput {
		// The report exists but this profile cannot read both counts
		// out of it, which is drift rather than a zero-token call. Left
		// to the estimate, the same answer a CLI that reports nothing
		// gets — and better than charging a real output count against a
		// prompt this build read as zero. The two cache figures are not
		// in the test: a provider that caches nothing reports neither,
		// and zero is the true answer there.
		return
	}
	e.input, e.output, e.reported = input, output, true
	e.cacheRead, _ = firstInt(doc, p.Usage.CacheRead)
	e.cacheWrite, _ = firstInt(doc, p.Usage.CacheWrite)
}

// textOf reads the assistant's text out of ONE line of a jsonl stream,
// honouring the profile's event filter.
//
// A line whose discriminator is not one this profile calls text is not a
// missing path — it is an event about something else, and it must not even
// count as "located": a stream that spliced a tool's output into the reply
// and then repeated the reply would look, to every frame downstream, exactly
// like a model that had said all of it.
func textOf(p Profile, doc map[string]any) (string, bool) {
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
func lookup(doc map[string]any, path Path) (any, bool) {
	var current any = doc
	for _, step := range path {
		switch node := current.(type) {
		case map[string]any:
			next, ok := node[step]
			if !ok {
				return nil, false
			}
			current = next
		case []any:
			idx, err := strconv.Atoi(step)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, false
			}
			current = node[idx]
		default:
			return nil, false
		}
	}
	return current, true
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
func firstString(doc map[string]any, paths []Path) (string, bool) {
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

func firstBool(doc map[string]any, paths []Path) bool {
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

// firstInt reads a token count, accepting the float64 every JSON number
// decodes to as well as a string, which some CLIs emit for large counts.
func firstInt(doc map[string]any, paths []Path) (int, bool) {
	for _, path := range paths {
		v, ok := lookup(doc, path)
		if !ok {
			continue
		}
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
	}
	return 0, false
}
