package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// MaxRunTextBytes bounds each of a collected run's report, failure detail and
// transcript (one bound, so they agree on how much of a run survives), in
// bytes: all ride the `executor_collected` event, refused whole past
// [github.com/crewlet/crewlet/internal/queue.MaxPayloadBytes] with the run's
// only spend record. Worst case, measured redacted ([Coordinator.fitText],
// [boundTranscript]): (3 × 256 KiB + 16 of refs [MaxDeliveredRefBytes] + 1 of
// question [decision.MaxQuestion] + 64 of diagnostic [events.MaxDiagnosticBytes] +
// 2 of reasons [MaxFellBackBytes], [boundReason]) × 6 for JSON ≈ 5 MiB, plus
// a few KiB of identifiers; a new field of box-written text needs a term here.
// Past it the report and failure are condensed ([Condenser]), since a cut head
// reads as the whole; the transcript, read only by a person, keeps whole lines
// from both ends and the note between them, all within it ([transcriptHeadBytes]).
const MaxRunTextBytes = 256 << 10

// MaxFileBytes is the most one [Sandbox.ReadFile] reads out of a box, bounding
// the engine's memory against a harness that can write anything; 32 MiB is far
// past any honest report. A file past it is REFUSED ([ErrFileTooLarge]), never
// read in part: a clipped report, question or marker reads as a finished one,
// and nothing downstream can tell. It does not govern a run's stdout and
// stderr, which grow with the run and are read as streams ([Sandbox.OpenFile])
// or from their end ([Sandbox.ReadTail]), never refused for size.
const MaxFileBytes = 32 << 20

// ErrFileTooLarge is [Sandbox.ReadFile]'s answer for a file past
// [MaxFileBytes]. The refusal is a [*FileTooLargeError], which names the cap
// that refused it.
var ErrFileTooLarge = errors.New("sandbox: the file is larger than the engine reads back from a box")

// FileTooLargeError is [ErrFileTooLarge] with the path and the cap that refused
// it, so a caller states the box's cap rather than a constant of its own.
type FileTooLargeError struct {
	// Path is the file that was not read.
	Path string
	// Limit is the most the box reads whole: [MaxFileBytes] on every box
	// the engine builds.
	Limit int
	// Size is the file's size where the box read it ([Sandbox.OpenWhole]),
	// zero where it read only past the limit.
	Size int64
}

func (e *FileTooLargeError) Error() string {
	return fmt.Sprintf("%v: %s is past %s, so it was not read — the harness wrote more "+
		"than a report, and a clipped one would read as a finished one",
		ErrFileTooLarge, e.Path, capSize(e.Limit))
}

// Is makes the error [ErrFileTooLarge] under errors.Is.
func (e *FileTooLargeError) Is(target error) bool { return target == ErrFileTooLarge }

// capSize is a whole-read cap as a reader says it: in MiB when it is a whole
// number of them, which [MaxFileBytes] is, and in KiB otherwise.
func capSize(n int) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return kib(n)
}

// readLimit is the whole-read cap a box holds its reads to: the one the suite
// set ([FakeSandbox.CapReads], export_test.go), else [MaxFileBytes]. Zero means
// MaxFileBytes, so no production constructor has a value to forget.
func readLimit(capped int) int {
	if capped > 0 {
		return capped
	}
	return MaxFileBytes
}

// ErrNotRegularFile is a read a box refused because its path is not a regular
// file (a named pipe, device, socket, directory or symbolic link): opened from
// the engine host it could block or never end ([NotRegularFileError] says
// which). A write over one is refused too ([NotRegularFileError.Write]): a
// write replaces a regular file whole, and anything else there is the box's
// own doing, left as it made it; a refused write fails the launch, but for a
// browser seat's helper, which withholds the browser instead ([EnsureBrowser]).
//
// It refuses that piece, not the box: what the path names is fixed, so a retry
// would be refused until the run was settled as lost, its spend uncharged. A
// collection describes the piece, as for [MaxFileBytes], and collects the rest.
var ErrNotRegularFile = errors.New("sandbox: the path is not a regular file")

// NotRegularFileError is [ErrNotRegularFile] with what the path was found to
// be, which is the sentence a refused piece is described by.
type NotRegularFileError struct {
	// Path is where the file was looked for.
	Path string
	// Kind is what it is, as a reader says it: "a named pipe", "a
	// directory", "a symbolic link".
	Kind string
	// Write is whether it was refused as a write rather than a read.
	Write bool
	// OnTheWay is whether Path is a component on the way to the file, refused
	// for not being a directory, rather than the file itself.
	OnTheWay bool
}

// Reason is why the path was not read — or not written, for a write — as the
// rest of a sentence about it.
func (e *NotRegularFileError) Reason() string {
	verb, done := "read", "read"
	if e.Write {
		verb, done = "written", "written"
	}
	if e.OnTheWay {
		return "is " + e.Kind + ", not a directory, so nothing beneath it was " + done
	}
	if e.Kind == kindSymlink {
		return "is a symbolic link, and a box's files are " + verb + " only where they lie, so it was not " + done
	}
	return "is " + e.Kind + ", not a regular file, so it was not " + done
}

func (e *NotRegularFileError) Error() string {
	if e.Write {
		return e.Path + " " + e.Reason() + ": the engine writes a box's file by replacing it whole, " +
			"and replaces nothing else the box put at a path — a link, a directory, a named pipe — " +
			"which is left as the box made it"
	}
	return e.Path + " " + e.Reason() + ": a box's markers, reports and streams are files " +
		"its runner writes, and reading anything else from the engine host could block it, " +
		"never end, or read past the box"
}

// Is makes the error [ErrNotRegularFile] under errors.Is.
func (e *NotRegularFileError) Is(target error) bool { return target == ErrNotRegularFile }

// kindSymlink is the [NotRegularFileError.Kind] of a symbolic link.
const kindSymlink = "a symbolic link"

// readCapped reads r whole, REFUSING more than limit ([readLimit]) rather than
// returning its first part: the rule every backend's [Sandbox.ReadFile]
// follows. It reads limit+1 so an overrun is visible.
func readCapped(r io.Reader, path string, limit int) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, &FileTooLargeError{Path: path, Limit: limit}
	}
	return raw, nil
}

// RunPart names which piece of a collected run's account a [Condenser] is
// asked to rewrite, because what a rewrite must keep differs between them.
type RunPart string

const (
	// PartReport is [Result.Text]: the report the harness wrote.
	PartReport RunPart = "report"
	// PartFailure is [Result.Error]: why the run did not finish — its exit
	// status, its harness's own error, and what it printed to stderr.
	PartFailure RunPart = "failure"
)

// RunParts is every part a [Condenser] can be asked about. A run's question
// is none of them: it is asked through a tool that holds it to its bounds
// before it is recorded ([Ask.Validate]), so there is nothing to condense.
var RunParts = []RunPart{PartReport, PartFailure}

// Valid reports whether the part is one this package names.
func (p RunPart) Valid() bool { return slices.Contains(RunParts, p) }

// MaxDeliveredRefBytes bounds the branches and pull requests a run's record
// lists as delivered. They are read from the whole report with no count (every
// `Delivered:` line, or every pull-request URL), so a report can name one per
// line. 16 KiB holds over 160 URLs, past what one run delivers; refs are
// deduplicated first, and what does not fit is counted on the record and in
// [runReport], not dropped unsaid.
const MaxDeliveredRefBytes = 16 << 10

// MaxFellBackBytes bounds why a run asked to continue a session started its
// own ([Result.FellBack]), as its runner reads it and the record carries it.
// The reason is one line: an engine sentence of a few hundred bytes, or for
// OpenCode a few words and then whatever OpenCode answered. A KiB holds the
// engine's sentences and the opening of OpenCode's error (the whole is on the
// error stream), as executorkeep.FailureMaxBytes and UnkeptReasonMaxBytes do.
// It must be held: the session record is a file the run's agent can write, and
// a record past the queue's ceiling is refused whole.
const MaxFellBackBytes = 1 << 10

// BoundFellBack is [Result.FellBack] as everything past its box carries it:
// redacted, then cut to [MaxFellBackBytes] ([boundReason]). Applied where the
// runner reads it, and again as the record is published ([collectedRecord]),
// which trusts no runner, nor a host of another build, to have bounded it.
func BoundFellBack(why string) string { return boundReason(why, MaxFellBackBytes) }

// boundReason is a reason as the launch's record carries it: REDACTED WHOLE,
// THEN CUT to bound bytes on a character and marked within it
// ([textcut.Within]), keeping the opening, where a sentence says what
// happened. It holds [MaxFellBackBytes] and why a session was not kept (a KiB,
// as executorkeep.UnkeptReasonMaxBytes). Redacted first, because a token
// straddling the cut would survive as an unrecognisable fragment; cut after
// every redaction, because a marker can be longer than what it replaces and a
// cut text redacted again can read a partial marker as a value. The cut reads
// one byte past its bound, so only that much of the redaction is kept
// ([redact.Prefix]). A reader that redacts again re-applies this whole rule.
func boundReason(why string, bound int) string {
	opening, _ := redact.Prefix(why, bound+1)
	return textcut.Within(opening, bound)
}

// MaxCondenseBytes is the most text one [Condenser] call reads: the engine
// compactor's compact.MaxChunks pieces of compact.ChunkBytes, 2 MiB; past it
// the compactor refuses before asking a model. Declared here because this
// package cannot import the compactor (it sits on the model providers), and
// pinned to those constants by the engine's test.
const MaxCondenseBytes = 2 << 20

// failureReserve is held back from a runner's failure detail for what the
// coordinator may put in front of it before condensing: the wall-clock-cap
// sentence and its separator ([endedAtDeadline]). A KiB is several times that
// (a test holds the sentence to half), leaving room to reword it. A runner
// holds its own failure to [MaxFailureBytes] exactly, measured redacted whole
// and with room for [KeepEndMark]; the coordinator's sentence ends on no
// credential's name, so its join adds nothing a redaction could grow.
const failureReserve = 1 << 10

// MaxFailureBytes is the most a runner's failure detail ([Result.Error]) may
// be: [MaxCondenseBytes] less [failureReserve], so a failure built around a
// long stderr can still be shown to a model whole. A runner composes its own
// sentences and gives the error stream only what is left.
const MaxFailureBytes = MaxCondenseBytes - failureReserve

// Condenser rewrites a piece of a collected run's account past
// [MaxRunTextBytes] into at most budget bytes on the seat's auxiliary model:
// the engine's compactor, whose chain and budget are the engine's. It is handed
// the run because the call is filed under the run's turn, and reads at most
// [MaxCondenseBytes] per call. The answer carries its own condensed label; an
// error means the caller's fallback stands. The spend is answered whatever the
// error (a too-long rewrite was still paid for), and rides [Result.Condensed]
// to the segment that resumes from it, since no segment runs during collection.
type Condenser interface {
	Condense(ctx context.Context, run PendingRun, part RunPart, text string,
		budget int) (string, AuxTokens, error)
}

// fitResult holds a collected run's account to its record: the report and the
// failure to [MaxRunTextBytes], condensed past it, plus the transcript and the
// refs (the run's own and its push record's, one budget, [boundPushed]).
// Applied ONCE, where every collected result enters the coordinator, before
// anything is published or resumed, so the record, its retries and the
// reviewer's [runReport] carry the same text; here rather than in a runner
// because the coordinator trusts no runner ([collectedRecord]) and an
// unbounded record is refused with the run's only spend. Two halves, because a
// box and its seat may be on different nodes ([BoxReader]): [boundBox], no
// model, where the box is read; [Coordinator.fitText], where the result is
// used, under the turn that pays, and where the push record is read.
func (c *Coordinator) fitResult(ctx context.Context, run PendingRun, result Result) Result {
	return c.fitText(ctx, run, boundBox(result))
}

// boundBox is the half of [Coordinator.fitResult] that reads no model: the
// transcript and the delivered refs held to their bounds, what was left out
// counted. Applied ONCE, where the box is read: a second pass counts nothing.
// The transcript's counts are ADDED to a runner's, which counted what it left
// out as it read the transcript: that one is within the bound, so this keeps
// it whole and adds nothing. A count below zero is no count.
func boundBox(result Result) Result {
	text, lines, bytes := boundTranscript(result.Transcript)
	result.Transcript = text
	result.TranscriptElidedLines = max(result.TranscriptElidedLines, 0) + lines
	result.TranscriptElidedBytes = max(result.TranscriptElidedBytes, 0) + bytes
	result.DeliveredRefs, result.DeliveredRefsElided = boundRefs(result.DeliveredRefs)
	if len(result.Event) > MaxRunEventBytes {
		// DROPPED WHOLE, never cut: a cut event is no JSON, and the one
		// reader it has classifies a decoded document or nothing.
		result.Event = ""
	}
	if len(result.Limits) > MaxRunEventBytes {
		result.Limits = ""
	}
	return result
}

// MaxRunEventBytes bounds a result's terminal event ([Result.Event]), sent back
// to the asking node whole or not at all. 16 KiB, the bound of a question a
// person reads: a result line holds usage, cost and its last text, a spent
// plan's line one sentence there, so a longer line ended on long text, is no
// limit report, and dropping it costs the field's one reader nothing.
const MaxRunEventBytes = 16 << 10

// BoxReadCarries are the bounds, tried in turn, a box's host holds each piece
// of a result's text to before replying to the asking node ([CarriedResult]):
// the first whose sealed reply fits the broker's ceiling. [MaxCondenseBytes]
// first, so the asker condenses exactly as it would a text it read itself;
// that fits [queue.MaxPayloadBytes] unless the text is written to grow
// several-fold under JSON, and then [MaxRunTextBytes], whose arithmetic fits
// whatever the text.
var BoxReadCarries = []int{MaxCondenseBytes, MaxRunTextBytes}

// BoxReadHeldBytes is the weight a box read's answer holds: the most one
// collection holds at once on the box's host ([Runner.Collect]: the result file
// parsed, the event stream a line at a time, the report, and their redactions),
// each piece near [MaxFileBytes]. internal/sandbox/executor's
// BenchmarkACollectionAtItsBounds measures it: 136 MiB at worst (a Claude Code
// result whose text, the stream's final message and the report are each near
// 32 MiB), 4.3 times the file bound, rounded up to the next whole one.
const BoxReadHeldBytes = 5 * MaxFileBytes

// The node's answer budget admits a box read at its cap: a negative constant
// does not compile.
const _ = uint(queue.AnswerBudgetBytes - BoxReadHeldBytes)

// CarriedResult is result with its report and failure each held to budget by
// whole lines, the note saying so counted inside it (a report keeping its
// start, a failure both its ends, [keepLines]), for a reply between nodes
// ([BoxReadCarries]).
func CarriedResult(result Result, budget int) Result {
	why := fmt.Sprintf("past the %d KiB a box's host sends back of it", budget>>10)
	result.Text = carriedPart(PartReport, result.Text, budget, why)
	result.Error = carriedPart(PartFailure, result.Error, budget, why)
	return result
}

// carriedPart is one piece held to budget, note included — the room
// [condensable] leaves for its note, for the same reason.
func carriedPart(part RunPart, text string, budget int, why string) string {
	if len(text) <= budget {
		return text
	}
	room := len(omittedNote(len(text)+1, "intervening", len(text), why)) + 1
	return keepLines(part, text, budget-room, why)
}

// fitText is the half of [Coordinator.fitResult] a model may write: the report
// and the failure, each held to its bound and condensed past it.
//
// Each is measured REDACTED, as the record carries it, and a model's rewrite is
// redacted before it is measured ([Coordinator.condensed]): a redaction can
// lengthen text sevenfold (a password written in pieces becomes a marker per
// piece) and the publish redacts again, so a piece measured unredacted could
// ride the record at seven times its bound. Over redacted text the publish's
// pass changes nothing, save the one shape internal/redact's package doc
// names, so [MaxRunTextBytes]'s arithmetic holds whatever the runner did.
func (c *Coordinator) fitText(ctx context.Context, run PendingRun, result Result) Result {
	// THIS COLLECTION'S OWN, counted from nothing: a runner that set it would
	// charge the turn for a rewrite nobody made.
	result.Condensed = AuxTokens{}
	result.Text = c.fitPart(ctx, run, PartReport, redact.Secrets(result.Text), MaxRunTextBytes, &result.Condensed)
	result.Error = c.fitPart(ctx, run, PartFailure, redact.Secrets(result.Error), MaxRunTextBytes,
		&result.Condensed)
	// ONE BUDGET FOR THE RUN'S REFS: the push record's get what the run's own,
	// bounded in [boundBox], leave ([boundPushed]).
	left := MaxDeliveredRefBytes
	for _, ref := range result.DeliveredRefs {
		left -= len(ref)
	}
	result.Pushed, result.PushedElided = boundPushed(result.Pushed, result.PushedElided, left)
	// AND WHY THE PUSH RECORD WAS UNREAD, built from Gitlet's answer: redacted
	// and held to a record's diagnostic bound.
	result.PushedUnread = events.Diagnostic(result.PushedUnread)
	// AND WHAT BECAME OF THE CHANGES IT ASKED TO OPEN, each sentence as
	// that one is ([fitOpened]).
	result.Opened = fitOpened(result.Opened)
	return result
}

// boundRefs is a run's delivered refs as its record carries them: redacted,
// deduplicated in order, valid UTF-8 and held to [MaxDeliveredRefBytes] in
// whole refs (a shortened URL names nothing), with how many more the run
// named. Redacted here, first, rather than trusted to a runner: a `Delivered:`
// line can carry a credential, and the bound measures what is carried. A
// non-UTF-8 ref is DROPPED, not mended, and counted: JSON would turn its bad
// bytes into U+FFFD, naming a URL never delivered or merging two refs past the
// dedup. A branch's grammar
// ([github.com/crewlet/crewlet/internal/gitref.ValidBranch]) holds it to UTF-8
// but a URL's does not, so the rule sits here, where every runner's refs pass.
func boundRefs(refs []string) ([]string, int) {
	seen := make(map[string]bool, len(refs))
	var kept []string
	size, left := 0, 0
	for _, ref := range refs {
		ref = redact.Secrets(ref)
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if !utf8.ValidString(ref) || size+len(ref) > MaxDeliveredRefBytes {
			left++
			continue
		}
		size += len(ref)
		kept = append(kept, ref)
	}
	return kept, left
}

// The two halves of a long transcript a run's record keeps, in whole lines.
// The END gets the larger share: the conclusion and what broke sit there
// ([wholeLines]). The START is kept for the run's plan, which a log cut to its
// end loses. 64 KiB is some 370 tool lines at their longest (a tool's name and
// at most 160 bytes of what it ran, never its output); the rest of
// [MaxRunTextBytes] is the end, but for [transcriptNoteRoom].
const (
	transcriptHeadBytes = 64 << 10
	transcriptTailBytes = MaxRunTextBytes - transcriptHeadBytes - transcriptNoteRoom
)

// transcriptNoteRoom is the room the note standing for a transcript's middle
// ([elidedNote]) takes inside [MaxRunTextBytes]: several times the note at its
// widest, some 220 bytes. Inside the bound, a bounded transcript fits it, so
// bounding it again keeps it whole and counts nothing.
const transcriptNoteRoom = 1 << 10

// boundTranscript holds a run's transcript to its record: REDACTED WHOLE FIRST
// (a secret straddling a cut would survive as an unrecognisable fragment),
// then whole lines from its start up to [transcriptHeadBytes] and its end up to
// [transcriptTailBytes], with a note line between counting the lines and bytes
// left out, which it also returns. Whole lines, because a cut line reads as a
// whole one; a single line past its half keeps its own start (or end), on a
// character, marked. A transcript that fits is carried as it is.
func boundTranscript(text string) (string, int, int) {
	// ONE implementation of the rule: the halves hold the text, so they
	// answer it whatever its length.
	var halves TranscriptHalves
	_, _ = halves.WriteString(redact.Secrets(text))
	return halves.Bounded()
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
	return activityline.Words(activityline.TagNote, fmt.Sprintf("%s left out here: a run's record keeps the "+
		"first %d KiB and the last %d KiB of its transcript, in whole lines", what, transcriptHeadBytes>>10,
		transcriptTailBytes>>10)) + "\n"
}

// KeepEnd is the end of text in WHOLE LINES, at most keep bytes, and the offset
// in text where it begins: the one rule for a stream's end, shared by the live
// view's windows and a collection's error stream. A window a byte count opened
// mid-line starts at the next line (or at the count, if a line starts there),
// since that line cannot be read whole. A single line longer than keep keeps
// its own end on a character, marked with [KeepEndMark] outside the offset's
// count: a process's last line is where it says what went wrong.
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
	return KeepEndMark + text[start:], start
}

// KeepEndMark is what [KeepEnd] puts in front of a single line it keeps only
// the end of — outside the keep bytes it counts, so a caller holding its
// answer to a bound leaves room for it.
const KeepEndMark = "…"

// kib is a size as a reader says it, in KiB rounded up.
func kib(n int) string { return fmt.Sprintf("%d KiB", (n+1023)/1024) }

// fitPart holds one piece of a run's account to budget: condensed past it, and
// in whole lines where no model can condense it ([wholeLines]).
//
// A failure past [MaxCondenseBytes] is first brought inside it, since nothing
// outside this package keeps [MaxFailureBytes] and the compactor would refuse
// it unasked: whole lines from both ends (the engine's start, the failure's
// end) with a note inside the bound, and that is condensed. The fallback is
// taken from the whole failure, so it carries one note, not two.
func (c *Coordinator) fitPart(ctx context.Context, run PendingRun, part RunPart, text string,
	budget int, spent *AuxTokens,
) string {
	if len(text) <= budget {
		return text
	}
	input := text
	if part == PartFailure && len(input) > MaxCondenseBytes {
		input = condensable(input)
		if len(input) <= budget {
			// What is left after the middle went fits whole: nothing for a model to
			// do, and a condensed label would say what did not happen.
			return input
		}
	}
	if rewritten, ok := c.condensed(ctx, run, part, input, budget, spent); ok {
		return rewritten
	}
	return wholeLines(part, text, budget)
}

// condensable is a failure past [MaxCondenseBytes] held to it — note
// included — by the failure's own whole-lines rule.
func condensable(text string) string {
	why := fmt.Sprintf("past the %d KiB one condensation reads", MaxCondenseBytes>>10)
	// The note's width at its widest — every line and every byte of the
	// text counted — so the note it gets cannot be longer than the room.
	room := len(omittedNote(len(text)+1, "intervening", len(text), why)) + 1
	return keepLines(PartFailure, text, MaxCondenseBytes-room, why)
}

// condensed is the seat's auxiliary model's rewrite of a piece past its
// budget, or false, logged, where none could be had; the attempt's cost is
// added to spent either way. The rewrite is REDACTED BEFORE IT IS MEASURED, and
// one the redaction puts past budget is refused like one too long, so the
// redacted whole-lines fallback stands: no runner redacted the model's text,
// and the publish's redaction could lengthen it past its bound ([boundReason]).
func (c *Coordinator) condensed(ctx context.Context, run PendingRun, part RunPart, text string,
	budget int, spent *AuxTokens,
) (string, bool) {
	if c.condense == nil {
		return "", false
	}
	rewritten, cost, err := c.condense.Condense(ctx, run, part, text, budget)
	*spent = spent.Plus(cost)
	var detail string
	if err == nil {
		// The whole redaction's size decides, and one within budget is all
		// of its opening ([redact.Prefix]).
		redacted, whole := redact.Prefix(rewritten, budget)
		if whole <= budget {
			return redacted, true
		}
		detail = fmt.Sprintf("the rewrite, redacted, came back at %d bytes, past the %d-byte bound", whole, budget)
	} else {
		detail = err.Error()
	}
	log.WarnContext(ctx, "sandbox_run_text_not_condensed", "turn_id", run.TurnID,
		"launch_id", run.LaunchID, "part", string(part), "bytes", len(text), "error", detail)
	return "", false
}

// wholeLines is a piece that could not be condensed, held to budget by leaving
// WHOLE LINES out and saying how many, never by cutting one. A report (or a
// question) keeps its START, where its point is. A failure keeps BOTH ENDS: its
// end for most of the budget, where a process names what broke, and its start
// up to [failureHeadShare], where the engine speaks (an unread piece, the exit
// status, the harness's error), which an end-only cut of a long stderr loses.
// The note stands where the lines were and is outside the budget, which bounds
// content ([github.com/crewlet/crewlet/internal/textcut]); a single line past
// the whole budget leaves nothing, and the note says so.
func wholeLines(part RunPart, text string, budget int) string {
	return keepLines(part, text, budget, fmt.Sprintf(
		"past the %d KiB a run's record carries for it, and no model could condense them", budget>>10))
}

// keepLines is [wholeLines] with the note's reason given: why the lines it
// stands for were left out.
func keepLines(part RunPart, text string, budget int, why string) string {
	lines := strings.SplitAfter(text, "\n")
	head, size := 0, 0
	if part != PartFailure {
		for head < len(lines) && size+len(lines[head]) <= budget {
			size += len(lines[head])
			head++
		}
		return strings.Join(lines[:head], "") + omittedLines(lines[head:], "later", why)
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
	return strings.Join(lines[:head], "") + omittedLines(lines[head:tail], which, why) +
		strings.Join(lines[tail:], "")
}

// failureHeadShare is the share of a failure's budget its START keeps when no
// model could condense it: an eighth, 32 KiB of [MaxRunTextBytes], holding the
// engine's sentences (a few hundred bytes each) and the opening of a long
// harness error; the rest is the end, where a process names what broke.
const failureHeadShare = 8

// omittedLines is the note standing where whole lines were left out: after
// what was kept for "later" lines, before it otherwise.
func omittedLines(left []string, which, why string) string {
	if len(left) == 0 {
		return ""
	}
	n := 0
	for _, line := range left {
		n += len(line)
	}
	note := omittedNote(len(left), which, n, why)
	if which == "later" {
		return "\n" + note
	}
	return note + "\n"
}

// omittedNote is the words of that note, for lines lines of bytes bytes.
func omittedNote(lines int, which string, bytes int, why string) string {
	return fmt.Sprintf("(%d %s line(s), %d KiB, not shown: %s)", lines, which, (bytes+1023)/1024, why)
}
