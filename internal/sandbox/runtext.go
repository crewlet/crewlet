package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// MaxRunTextBytes bounds each piece of a collected run's own account of
// itself that the engine carries: the report it wrote, the detail of its
// failure, and its activity transcript.
//
// BYTES, because what it bounds is an EVENT. Every one of these rides the
// run's `agent_phase_completed{phase: sandbox}` record (and the report rides
// the resumed executor's too, as the tool result it reads), and an event over
// the queue's 8 MiB [github.com/crewlet/crewlet/internal/queue.MaxPayloadBytes]
// is refused — so an unbounded report does not arrive long, it does not arrive
// at all. 256 KiB of text is at most 1.5 MiB once JSON has escaped it (a
// control byte becomes six), and three of them are still well inside the
// ceiling. It was 100 000 RUNES, which is anything from 100 KB to 400 KB on the
// wire depending on the script the output was written in: a bound on the wrong
// unit for the only limit that matters.
//
// WHAT HAPPENS PAST IT DEPENDS ON WHO READS THE PIECE. The report and the
// failure detail are what the resumed executor acts on — what to tell the
// requester, what blocked the run — so one past the bound is CONDENSED by the
// seat's auxiliary model ([Condenser]) rather than cut. A cut used to keep the
// report's head, which a reader takes for the whole report: a run that seems
// to have said less than it did. The transcript is read by a PERSON — no model
// and no prompt reads it — so it is not condensed: it keeps whole lines from
// its start and from its end, and says how much of its middle it left out
// ([transcriptHeadBytes], [transcriptTailBytes]).
//
// ONE BOUND, NOT ONE PER PIECE: every one of them is the same run's account of
// itself, and a per-field cap would let them disagree about how much of one
// run survives.
const MaxRunTextBytes = 256 << 10

// MaxFileBytes is the most one [Sandbox.ReadFile] reads back out of a box, and
// a file past it is REFUSED ([ErrFileTooLarge]) rather than read in part.
//
// It bounds what a box can cost the engine's memory: every file read back is
// one the coding agent wrote, inside a box it can run any command in, and the
// host's own process is what holds it. 32 MiB is far past any report or
// question a run writes honestly, and is what one compaction can still be
// asked about many times over.
//
// THE REFUSAL IS THE POINT, FOR A FILE MEANT TO BE READ WHOLE. A reader that
// stops at its cap reports a clean end of file, so a file of exactly the cap
// cannot be told from one that was clipped there — and what ReadFile reads is
// a run's report, its question, its result line and its markers, precisely
// the content nothing downstream can sanity-check. A silently halved report
// reads as a finished one.
//
// IT DOES NOT GOVERN A RUN'S MACHINE STREAMS. Its stdout event log and its
// stderr grow with the run, and the engine keeps a bounded share of each in
// the end (a transcript of [MaxRunTextBytes], a failure from the end of the
// error stream), so refusing them whole refused a run for the size of its own
// log: a poll that could not read stdout never reached its liveness probe, and
// a collection that could not read it lost a result the report file held.
// Those are read as streams ([Sandbox.OpenFile]) or from their end
// ([Sandbox.ReadTail]), and nothing there is refused for its size.
const MaxFileBytes = 32 << 20

// ErrFileTooLarge is [Sandbox.ReadFile]'s answer for a file past
// [MaxFileBytes].
var ErrFileTooLarge = errors.New("sandbox: the file is larger than the engine reads back from a box")

// readCapped reads a file's content out of r, REFUSING one past
// [MaxFileBytes] rather than returning its first part — the one rule every
// backend's [Sandbox.ReadFile] follows.
//
// +1 so an overrun is visible: a reader stopped at exactly the cap cannot
// tell a file of that size from a longer one.
func readCapped(r io.Reader, path string) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxFileBytes {
		return nil, fmt.Errorf("%w: %s is past %d MiB, so it was not read — the coding agent "+
			"wrote more than a report, and a clipped one would read as a finished one",
			ErrFileTooLarge, path, MaxFileBytes>>20)
	}
	return raw, nil
}

// RunPart names which piece of a collected run's account a [Condenser] is
// asked to rewrite, because what a rewrite must keep differs between them.
type RunPart string

const (
	// PartReport is [Result.Text]: the report the coding agent wrote.
	PartReport RunPart = "report"
	// PartFailure is [Result.Error]: why the run did not finish — its exit
	// status, its CLI's own error, and what it printed to stderr.
	PartFailure RunPart = "failure"
)

// Valid reports whether the part is one this package names.
func (p RunPart) Valid() bool { return p == PartReport || p == PartFailure }

// Condenser rewrites a piece of a collected run's account that is past
// [MaxRunTextBytes] into at most budget bytes, on the seat's own auxiliary
// model — the engine's compactor, behind the interface this package needs,
// because the chain it runs on and the budget it is charged to are the
// engine's.
//
// The answer carries its own label saying it was condensed. An error means no
// rewrite could be had, and the caller's fallback stands. It is handed the
// RUN, not just its seat, because a rewrite is a model call the engine files
// under the turn the run belongs to.
type Condenser interface {
	Condense(ctx context.Context, run PendingRun, part RunPart, text string, budget int) (string, error)
}

// fitResult holds the report and the failure detail of a collected run to
// [MaxRunTextBytes], condensing whichever is past it.
//
// Applied ONCE, where every collected result enters the coordinator, and
// before anything is published or resumed — so the run's phase record, the
// resumed executor's tool result and the record's retries all carry the same
// text.
//
// THE TRANSCRIPT TOO, and here rather than in the runner: this is the one
// home of the rule that the run's record fits its event, and the coordinator
// already trusts no runner to have redacted what it hands back (see
// [runPhase]). Bounded in a runner, a runner that forgot the bound would
// publish an unbounded transcript, and the record carrying the run's only
// spend would be refused whole.
func (c *Coordinator) fitResult(ctx context.Context, run PendingRun, result Result) Result {
	result.Text = c.fitPart(ctx, run, PartReport, result.Text)
	result.Error = c.fitPart(ctx, run, PartFailure, result.Error)
	result.Transcript, result.TranscriptElidedLines, result.TranscriptElidedBytes =
		boundTranscript(result.Transcript)
	return result
}

// The two halves of a long transcript a run's record keeps, in whole lines.
//
// THE END GETS THE LARGER SHARE, because it is where the run's conclusion and
// what broke sit — the reason the failure detail keeps its end too
// ([wholeLines]). The START is kept as well, because it is the run's plan, its
// clone and its first exploration of the code, which a log cut to its end
// loses entirely: a reader of a long run then cannot see what it set out to
// do. 64 KiB is about two hundred of OpenCode's transcript lines — a tool line
// is a name and at most 160 bytes of what it ran — which is that opening with
// room to spare; the rest of [MaxRunTextBytes] is the end.
const (
	transcriptHeadBytes = 64 << 10
	transcriptTailBytes = MaxRunTextBytes - transcriptHeadBytes
)

// boundTranscript holds a run's transcript to its record: REDACTED WHOLE
// FIRST, then whole lines from its start up to [transcriptHeadBytes] and from
// its end up to [transcriptTailBytes], with one note line between them naming
// how many lines and bytes were left out — which it also answers, for the
// record's own fields. A transcript that fits is carried as it is.
//
// Redacted before anything is dropped, for the reason everything read out of a
// box is: a secret straddling the cut survives as a fragment the pattern no
// longer recognises. And in WHOLE LINES, because the line a byte count lands
// in reads as a whole line it never was; a single line longer than its half
// is the one case that cannot be met in whole lines, and it keeps its own
// start (or end), on a character, marked.
func boundTranscript(text string) (string, int, int) {
	text = redact.Secrets(text)
	if len(text) <= MaxRunTextBytes {
		return text, 0, 0
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	head, size := 0, 0
	for head < len(lines) && size+len(lines[head]) <= transcriptHeadBytes {
		size += len(lines[head])
		head++
	}
	tail, size := len(lines), 0
	for tail > head && size+len(lines[tail-1]) <= transcriptTailBytes {
		size += len(lines[tail-1])
		tail--
	}

	// What lies between the halves; the text is past the budget, so there is
	// always some. Where a half kept no whole line, its edge line keeps its
	// own start or end instead.
	middle := lines[head:tail]
	const marker = "…"
	var startOf, endOf string
	if head == 0 {
		// Within the half with room for the line break it ends on, so a
		// line that did not fit whole is always marked as kept in part.
		startOf = textcut.Within(strings.TrimRight(middle[0], "\n"), transcriptHeadBytes-1) + "\n"
	}
	if tail == len(lines) {
		endOf = textcut.Tail(middle[len(middle)-1], transcriptTailBytes-len(marker))
	}
	dropped := 0
	for _, line := range middle {
		dropped += len(line)
	}
	// Bytes kept of a partial line are not left out; the marker each carries
	// is not content, so it is not counted against what was.
	if startOf != "" {
		dropped -= len(startOf) - len(marker)
	}
	if endOf != "" {
		dropped -= len(endOf) - len(marker)
	}
	// Lines kept in part are not lines left out: one when only an edge line
	// was partial, or when the whole text is that one line; two when both
	// edges were.
	partial := 0
	if startOf != "" {
		partial++
	}
	if endOf != "" && (startOf == "" || len(middle) > 1) {
		partial++
	}
	whole := len(middle) - partial

	var b strings.Builder
	b.WriteString(strings.Join(lines[:head], ""))
	b.WriteString(startOf)
	b.WriteString(elidedNote(whole, partial, dropped))
	b.WriteString(endOf)
	b.WriteString(strings.Join(lines[tail:], ""))
	return b.String(), whole, dropped
}

// elidedNote is the line standing where a transcript's middle was left out:
// whole lines dropped, lines kept only in part, and every byte not kept.
func elidedNote(whole, partial, bytes int) string {
	var what string
	switch {
	case partial == 0:
		what = fmt.Sprintf("%d line(s), %s,", whole, kib(bytes))
	case whole == 0:
		what = fmt.Sprintf("%s of %d long line(s)", kib(bytes), partial)
	default:
		what = fmt.Sprintf("%d line(s) and part of %d more, %s in all,", whole, partial, kib(bytes))
	}
	return fmt.Sprintf("(%s left out here: a run's record keeps the first %d KiB and the last %d KiB "+
		"of its transcript, in whole lines)\n", what, transcriptHeadBytes>>10, transcriptTailBytes>>10)
}

// kib is a size as a reader says it, in KiB rounded up.
func kib(n int) string { return fmt.Sprintf("%d KiB", (n+1023)/1024) }

func (c *Coordinator) fitPart(ctx context.Context, run PendingRun, part RunPart, text string) string {
	if len(text) <= MaxRunTextBytes {
		return text
	}
	if c.condense != nil {
		rewritten, err := c.condense.Condense(ctx, run, part, text, MaxRunTextBytes)
		if err == nil && len(rewritten) <= MaxRunTextBytes {
			return rewritten
		}
		detail := "the rewrite came back past the bound"
		if err != nil {
			detail = err.Error()
		}
		log.WarnContext(ctx, "sandbox_run_text_not_condensed", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "part", string(part), "bytes", len(text), "error", detail)
	}
	return wholeLines(part, text, MaxRunTextBytes)
}

// wholeLines is a piece that could not be condensed, held to budget by
// leaving WHOLE LINES out and saying how many — never by cutting one.
//
// From the END of a report, because a report is written to be read from the
// top and its summary is where it starts; from the START of a failure, because
// its conclusion — the line naming what broke — is the last thing a process
// prints, after everything that led to it. The note stands where
// the left-out lines were and is not counted against the budget, for the
// reason [github.com/crewlet/crewlet/internal/textcut] gives: the budget
// bounds the content. A single line past the whole budget leaves nothing, and
// the note then says so rather than showing a fragment of it.
func wholeLines(part RunPart, text string, budget int) string {
	lines := strings.SplitAfter(text, "\n")
	kept, size := 0, 0
	if part == PartReport {
		for kept < len(lines) && size+len(lines[kept]) <= budget {
			size += len(lines[kept])
			kept++
		}
		left := lines[kept:]
		return strings.Join(lines[:kept], "") + omittedLines(left, "later")
	}
	for kept < len(lines) && size+len(lines[len(lines)-1-kept]) <= budget {
		size += len(lines[len(lines)-1-kept])
		kept++
	}
	left := lines[:len(lines)-kept]
	return omittedLines(left, "earlier") + strings.Join(lines[len(lines)-kept:], "")
}

// omittedLines is the note standing where whole lines were left out.
func omittedLines(left []string, which string) string {
	if len(left) == 0 {
		return ""
	}
	n := 0
	for _, line := range left {
		n += len(line)
	}
	note := fmt.Sprintf("(%d %s line(s), %d KiB, not shown: past the %d KiB a run's record "+
		"carries, and no model could condense them)", len(left), which, (n+1023)/1024,
		MaxRunTextBytes>>10)
	if which == "later" {
		return "\n" + note
	}
	return note + "\n"
}
