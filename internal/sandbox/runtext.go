package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

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
// at all, and it takes the run's only spend record with it. It was 100 000
// RUNES, which is anything from 100 KB to 400 KB on the wire depending on the
// script the output was written in: a bound on the wrong unit for the only
// limit that matters.
//
// THE WHOLE RECORD FITS, and this is its arithmetic. The record's variable
// text is the report, the failure detail and the transcript (each at most
// this), the question in its notes ([MaxQuestionBytes]) and the delivered
// refs ([MaxDeliveredRefBytes]); everything else on it is identifiers and
// counts, a few KiB. JSON can grow text six-fold — a control byte becomes
// \u00XX, `<`, `>` and `&` become \u003c and its kin, an invalid byte becomes
// \ufffd — so the worst case is (3 × 256 + 16 + 16) KiB × 6, about 4.7 MiB,
// with the ceiling's other 3 MiB to spare. A field added to the record that
// carries text a box wrote needs its own bound and a line here, or this
// promise is no longer one.
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
	// PartQuestion is [Result.Question]: what the run stopped to ask a
	// person.
	PartQuestion RunPart = "question"
)

// RunParts is every part a [Condenser] can be asked about.
var RunParts = []RunPart{PartReport, PartFailure, PartQuestion}

// Valid reports whether the part is one this package names.
func (p RunPart) Valid() bool { return slices.Contains(RunParts, p) }

// MaxQuestionBytes bounds the question a run parks on.
//
// SIZED TO WHERE IT TRAVELS, and it travels further than anything else a run
// writes: it is the note on the run's phase record, a field of the park's
// announcement, a field of the run's coordination row — read and written whole
// on every status flip and every listing of active runs for as long as it
// waits — the banner a person answers it from, the operator's `answer_run`
// reply, and the resumed executor's answer text, which re-sends it on every
// later round of the turn. 16 KiB is about four thousand tokens: a page of
// prose, room for a question, its context and its options with plenty to
// spare. A question longer than a page is the report put in the wrong place —
// the report has 256 KiB of its own — and the shim's own route already stops
// near 128 KiB, the most one argument can carry. Past it the question is
// condensed by the seat's auxiliary model, never cut.
const MaxQuestionBytes = 16 << 10

// MaxDeliveredRefBytes bounds the branches and pull requests a run's record
// lists as delivered.
//
// THEY ARE SCRAPED FROM THE WHOLE REPORT, by a pattern with no count to it, so
// a report that pasted a list of pull requests — or a run that wrote one URL
// on every line of a 30 MiB file — handed the record one entry per match. A
// pull-request URL is typically under a hundred bytes, so 16 KiB lists more
// than a hundred and sixty of them, past what any one run delivers; the refs
// are deduplicated first, and what does not fit is COUNTED on the record and
// in the resumed executor's text rather than dropped unsaid.
const MaxDeliveredRefBytes = 16 << 10

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
//
// THE SPEND IS ANSWERED WHATEVER THE ERROR SAYS: a rewrite that came back too
// long was still paid for. It rides the collected result to the segment that
// resumes from it ([Result.Condensed]), which is the one that pays it to the
// turn's work item — no segment is running while a run is collected, so no
// segment's own tally could.
type Condenser interface {
	Condense(ctx context.Context, run PendingRun, part RunPart, text string,
		budget int) (string, AuxTokens, error)
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
	// THIS COLLECTION'S OWN, counted from nothing: the field is the
	// coordinator's, and a runner that set it would be charging the turn
	// for a rewrite nobody made.
	result.Condensed = AuxTokens{}
	// THE QUESTION FIRST, because a question nobody can be asked becomes
	// part of the failure — fitted after it, the refusal rode past the
	// failure's bound.
	if result.NeedsInput {
		result = c.fitQuestion(ctx, run, result)
	}
	result.Text = c.fitPart(ctx, run, PartReport, result.Text, MaxRunTextBytes, &result.Condensed)
	result.Error = c.fitPart(ctx, run, PartFailure, result.Error, MaxRunTextBytes, &result.Condensed)
	result.Transcript, result.TranscriptElidedLines, result.TranscriptElidedBytes =
		boundTranscript(result.Transcript)
	result.DeliveredRefs, result.DeliveredRefsElided = boundRefs(result.DeliveredRefs)
	return result
}

// fitQuestion holds a question to [MaxQuestionBytes]: condensed past it, and
// where no model can condense it, whole lines from its start — a question is
// read from the top, like a report — with the rest said.
//
// A QUESTION NO LINE OF WHICH FITS IS NOT ASKED. Its first line alone is past
// the bound, so whole lines leave nobody anything to answer, and a fragment
// of it read as the question is a question the agent never asked. The run is
// not parked on it: it resumes as not succeeded, saying why, and the executor
// can ask the coding agent again for a question a person can read.
func (c *Coordinator) fitQuestion(ctx context.Context, run PendingRun, result Result) Result {
	question := result.Question
	if len(question) <= MaxQuestionBytes {
		return result
	}
	if rewritten, ok := c.condensed(ctx, run, PartQuestion, question, MaxQuestionBytes,
		&result.Condensed); ok {
		result.Question = rewritten
		return result
	}
	if first, _, _ := strings.Cut(question, "\n"); len(first)+1 <= MaxQuestionBytes {
		result.Question = wholeLines(PartQuestion, question, MaxQuestionBytes)
		return result
	}
	refusal := fmt.Sprintf("the question the coding agent asked is %s with no line break in its "+
		"first %d KiB, past what a question carries, and no model could condense it, so nobody "+
		"was asked it", kib(len(question)), MaxQuestionBytes>>10)
	log.WarnContext(ctx, "sandbox_question_not_asked", "turn_id", run.TurnID,
		"launch_id", run.LaunchID, "bytes", len(question))
	result.NeedsInput, result.Question, result.AskTo = false, "", ""
	result.Success = false
	if result.Error == "" {
		result.Error = refusal
	} else {
		result.Error = refusal + ":\n" + result.Error
	}
	return result
}

// boundRefs is a run's delivered refs, deduplicated in the order they were
// found and held to [MaxDeliveredRefBytes], with how many did not fit.
//
// WHOLE REFS ONLY: a ref is an identifier, and a shortened URL names nothing.
func boundRefs(refs []string) ([]string, int) {
	seen := make(map[string]bool, len(refs))
	var kept []string
	size, left := 0, 0
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if size+len(ref) > MaxDeliveredRefBytes {
			left++
			continue
		}
		size += len(ref)
		kept = append(kept, ref)
	}
	return kept, left
}

// The two halves of a long transcript a run's record keeps, in whole lines.
//
// THE END GETS THE LARGER SHARE, because it is where the run's conclusion and
// what broke sit — the reason the failure detail keeps its end too
// ([wholeLines]). The START is kept as well, because it is the run's plan and
// its first exploration of the code, which a log cut to its end loses
// entirely: a reader of a long run then cannot see what it set out to do.
// 64 KiB is some 370 tool lines at their longest — a tool line is the tool's
// name and at most 160 bytes of what it ran, never its output — and more at
// their usual length, which is that opening with room to spare; the rest of
// [MaxRunTextBytes] is the end.
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

// KeepEnd is the end of text in WHOLE LINES, at most keep bytes of it, and the
// offset in text where what it keeps begins — the one rule for showing a
// stream's end, shared by the live view's windows and the error stream a
// collection reads.
//
// A window opens wherever a byte count put it, and the line it opened inside
// is the end of something nobody can read whole from here, so it starts at the
// next line instead — or right at the count, where that is already a line's
// start. A single line longer than keep is the one case whole lines cannot
// meet, and it keeps its own end on a character, marked with a leading "…"
// that is not part of the text the offset counts: a process's last line is
// where it says what went wrong.
func KeepEnd(text string, keep int) (string, int) {
	if len(text) <= keep {
		return text, 0
	}
	cut := len(text) - keep
	if cut > 0 && text[cut-1] == '\n' {
		return text[cut:], cut
	}
	if i := strings.IndexByte(text[cut:], '\n'); i >= 0 && cut+i+1 < len(text) {
		return text[cut+i+1:], cut + i + 1
	}
	start := cut
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return "…" + text[start:], start
}

// kib is a size as a reader says it, in KiB rounded up.
func kib(n int) string { return fmt.Sprintf("%d KiB", (n+1023)/1024) }

func (c *Coordinator) fitPart(ctx context.Context, run PendingRun, part RunPart, text string,
	budget int, spent *AuxTokens,
) string {
	if len(text) <= budget {
		return text
	}
	if rewritten, ok := c.condensed(ctx, run, part, text, budget, spent); ok {
		return rewritten
	}
	return wholeLines(part, text, budget)
}

// condensed is the seat's auxiliary model's rewrite of a piece past its
// budget, or false — said in the log — where none could be had. What the
// attempt cost is added to spent either way.
func (c *Coordinator) condensed(ctx context.Context, run PendingRun, part RunPart, text string,
	budget int, spent *AuxTokens,
) (string, bool) {
	if c.condense == nil {
		return "", false
	}
	rewritten, cost, err := c.condense.Condense(ctx, run, part, text, budget)
	*spent = spent.Plus(cost)
	if err == nil && len(rewritten) <= budget {
		return rewritten, true
	}
	detail := "the rewrite came back past the bound"
	if err != nil {
		detail = err.Error()
	}
	log.WarnContext(ctx, "sandbox_run_text_not_condensed", "turn_id", run.TurnID,
		"launch_id", run.LaunchID, "part", string(part), "bytes", len(text), "error", detail)
	return "", false
}

// wholeLines is a piece that could not be condensed, held to budget by
// leaving WHOLE LINES out and saying how many — never by cutting one.
//
// A report (and a question) keeps its START, because each is written to be
// read from the top and its point is where it starts. A failure keeps BOTH
// ENDS: its end, for most of the budget, because the line naming what broke
// is the last thing a process prints; and its start, up to an eighth of it
// ([failureHeadShare]), because that is where the ENGINE speaks — a piece it
// could not read, a question nobody could be asked, the exit status and the
// CLI's own error all come before the error stream they introduce. Kept from
// its end alone, a failure with a long error stream lost every one of them.
//
// The note stands where the left-out lines were and is not counted against
// the budget, for the reason [github.com/crewlet/crewlet/internal/textcut]
// gives: the budget bounds the content. A single line past the whole budget
// leaves nothing, and the note then says so rather than showing a fragment
// of it.
func wholeLines(part RunPart, text string, budget int) string {
	lines := strings.SplitAfter(text, "\n")
	head, size := 0, 0
	if part != PartFailure {
		for head < len(lines) && size+len(lines[head]) <= budget {
			size += len(lines[head])
			head++
		}
		return strings.Join(lines[:head], "") + omittedLines(lines[head:], "later", budget)
	}
	for head < len(lines) && size+len(lines[head]) <= budget/failureHeadShare {
		size += len(lines[head])
		head++
	}
	tail := len(lines)
	for tail > head && size+len(lines[tail-1]) <= budget {
		size += len(lines[tail-1])
		tail--
	}
	which := "earlier"
	if head > 0 {
		which = "intervening"
	}
	return strings.Join(lines[:head], "") + omittedLines(lines[head:tail], which, budget) +
		strings.Join(lines[tail:], "")
}

// failureHeadShare is the fraction of a failure's budget its START keeps when
// no model could condense it: an eighth, 32 KiB of [MaxRunTextBytes]. The
// engine's own sentences at its start are a few hundred bytes each, so this
// holds every one of them and the opening of a long error the CLI reported;
// the other seven eighths are the end, where a process names what broke.
const failureHeadShare = 8

// omittedLines is the note standing where whole lines were left out: after
// what was kept for "later" lines, before it otherwise.
func omittedLines(left []string, which string, budget int) string {
	if len(left) == 0 {
		return ""
	}
	n := 0
	for _, line := range left {
		n += len(line)
	}
	note := fmt.Sprintf("(%d %s line(s), %d KiB, not shown: past the %d KiB a run's record "+
		"carries for it, and no model could condense them)", len(left), which, (n+1023)/1024,
		budget>>10)
	if which == "later" {
		return "\n" + note
	}
	return note + "\n"
}
