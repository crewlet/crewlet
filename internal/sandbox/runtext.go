package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
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
// to have said less than it did. The transcript is read by a person watching
// what the run did, and only its END answers that, so it keeps its last
// 256 KiB with a marker in front.
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
// host's own process is what holds it. 32 MiB is far past any report or log a
// run produces honestly, and is what one compaction can still be asked about
// many times over.
//
// THE REFUSAL IS THE POINT. A reader that stops at its cap reports a clean
// end of file, so a file of exactly the cap cannot be told from one that was
// clipped there — and the files read back are a run's report and its stderr,
// which is precisely the content nothing downstream can sanity-check. A
// silently halved report reads as a finished one.
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
// text. The transcript is the runner's to bound; see [MaxRunTextBytes].
func (c *Coordinator) fitResult(ctx context.Context, run PendingRun, result Result) Result {
	result.Text = c.fitPart(ctx, run, PartReport, result.Text)
	result.Error = c.fitPart(ctx, run, PartFailure, result.Error)
	return result
}

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
// prints, after the clone and the dependency install. The note stands where
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
