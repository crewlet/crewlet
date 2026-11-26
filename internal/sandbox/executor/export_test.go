package executor

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// WriteHostProgram is [writeHostProgram] at 0700, for the stand-ins the
// external cases write.
func WriteHostProgram(t *testing.T, path, script string) {
	writeHostProgram(t, path, []byte(script), 0o700)
}

// Bounded holds r's failure — the error stream it reads from the end, and the
// whole failure it composes — to failure bytes, and the lines of an event
// stream it reads to line bytes, and returns r.
//
// For the cases past a bound (a failure past two mebibytes, a line past
// thirty-two), which at the real figures build and redact megabytes under
// -race: each runs at a bound it can fill and sizes its writes from it, and
// [TestARunnerReadsToTheEnginesOwnBounds] holds the real figures.
func Bounded(r *Runner, failure, line int) *Runner {
	r.failureBound, r.lineBound = failure, line
	return r
}

// Clocked has r bound a pack's release by now rather than the wall clock
// ([releaseContext]), and returns r: a case holds the release's deadline to the
// one its rule gives exactly, however long its take ran.
func Clocked(r *Runner, now func() time.Time) *Runner {
	r.now = now
	return r
}

// PackReleaseFloor is the least a pack's release is given.
const PackReleaseFloor = packReleaseFloor

// RedactContext is how much more than it keeps a read from the error stream's
// end takes, never shown, so what decides how the end it keeps redacts is read
// with it.
const RedactContext = redact.LookBack

// HumanSize is a byte count as the notes a run's failure writes say it.
func HumanSize(n int64) string { return humanSize(n) }

// UnreadNote is the line standing where the error stream's first unread bytes
// were left out of what was shown.
func UnreadNote(unread int64) string { return unreadNote(unread, false) }

// MaxLineBytes is the line bound every runner New builds reads to.
const MaxLineBytes = maxLineBytes

// ClaudeResultModelsMax is the most models a Claude Code result's spend model
// by model is read for.
const ClaudeResultModelsMax = claudeResultModelsMax

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
	// Call and SessionCall are how long it waits for each kind of call to its
	// own server ([openCodeBounded]).
	Call, SessionCall time.Duration
	// Clock is the command the script reads the time from, in whole seconds.
	Clock string
}

// OpenCodeCommandTimed is [OpenCode.Command] with the script run at times —
// for the cases about a wait running out, a sync coming round or a stop that
// must not wait, at lengths a test can tell apart.
func OpenCodeCommandTimed(req sandbox.RunRequest, session Session, paths Paths, times OpenCodeTimes) string {
	run := openCodeRunTimes()
	return "sh -c " + shellword.Quote(openCodeScript(req, session, paths, openCodeTimes{
		listen: cmp.Or(times.Listen, run.listen), mcp: cmp.Or(times.MCP, run.mcp),
		sync: cmp.Or(times.Sync, run.sync), stop: cmp.Or(times.Stop, run.stop),
		call: cmp.Or(times.Call, run.call), sessionCall: cmp.Or(times.SessionCall, run.sessionCall),
		clock: cmp.Or(times.Clock, run.clock),
	}))
}

// RecordStart is where the run a session record names began in its session,
// as the record says it ([parseSessionRecord]): whether it began the session,
// and the message it began after.
func RecordStart(text string) (began bool, after string) {
	start, _ := parseSessionRecord(text).runStart(OpenCode{})
	return start.Began, start.After
}

// RecordStarts is where the run a session record names began, read as h's
// archive reads it ([sessionRecord.runStart]): a message or a chat file's lines.
func RecordStarts(h Harness, text string) (sandbox.RunStart, sandbox.LinePrefix) {
	return parseSessionRecord(text).runStart(h)
}

// SessionRecordOf is the record of a run that worked in session, as
// [Runner.packNow] hands it to a pack.
func SessionRecordOf(session string) sessionRecord { return sessionRecord{id: session} }

// OpenCodeCursorTail is how much of a list answer's end a run's script reads
// its cursor from ([openCodeCursor]).
const OpenCodeCursorTail = openCodeCursorTail

// OpenCodeEndPath is the most a run's script spends after an end request
// reaches it while its harness runs, at the waits Command runs it at
// ([openCodeEndPath]).
func OpenCodeEndPath() time.Duration { return openCodeEndPath(openCodeRunTimes()) }

// OpenCodeEndCallTags is the tags of every call a run's script makes to its own
// server that no end request ends ([openCodeEndCalls]), sorted.
func OpenCodeEndCallTags() []string {
	return slices.Sorted(maps.Keys(openCodeEndCalls(openCodeRunTimes())))
}

// ClaudeCodeCommandTimed is [ClaudeCode.Command] with the harness given stop to
// go once its run is asked to end, rather than [sandbox.JobStopGrace]: for the
// cases about a harness that outlives its grace, at a length a test can wait
// out.
func ClaudeCodeCommandTimed(req sandbox.RunRequest, session Session, paths Paths, configPath string,
	stop time.Duration) string {
	return "sh -c " + shellword.Quote(claudeScript(req, session, paths, configPath, stop))
}

// ClaudeProjectDir is the project directory every Claude Code run in a box
// files its sessions under, and the archive formats each harness's run packs its
// session in: what a test reads a box's layout and a capture by.
const (
	ClaudeProjectDir      = claudeProjectDir
	ClaudeArchiveFormat   = claudeArchiveFormat
	OpenCodeArchiveFormat = openCodeArchiveFormat
	GeminiArchiveFormat   = geminiArchiveFormat
	QwenArchiveFormat     = qwenArchiveFormat
)

// HeldPlainMax is the most plaintext a pack beside a working harness packs
// ([heldPlainMax]): what a case sizes a session past it by.
const HeldPlainMax = heldPlainMax

// OpenCodeSaveRoom defines the shell functions an OpenCode save reads its box's
// room and its need by ([openCodeSaveRoom]): what a case feeds memory lines.
var OpenCodeSaveRoom = openCodeSaveRoom

// OpenCodeStops is a run's script's stops ([openCodeStops]) beside the end they
// stop with ([endFunction]) at stop, and OpenCodeExporter its login exporter
// ([openCodeExporter]) at sync into file: a case about stopping the exporter
// runs the two alone, since a run of the whole script cannot choose the
// instant it is stopped.
func OpenCodeStops(stop time.Duration) string { return endFunction(stop) + openCodeStops() }

// OpenCodeExporter is [OpenCodeStops]' other half.
func OpenCodeExporter(sync time.Duration, file string) string {
	return openCodeExporter(sync, file)
}

// JSONStringify is how a run's script spells a server name it looks for in an
// OpenCode answer.
func JSONStringify(s string) string { return jsonStringify(s) }

// ClaudeHookContext is what Claude Code puts after an event's name before a
// hook's additional context, and ClaudeNoteEvents the events a run's notes are
// taken at — what a test holds the instruction and the measured placements to.
const ClaudeHookContext = claudeHookContext

var ClaudeNoteEvents = []string{claudeEventToolBatch, claudeEventFinish}

// The sentences a malformed archive is said in ([MalformedArchiveError.Why]),
// one for each way an archive is not what its format describes.
const (
	WhyNotGzip   = whyNotGzip
	WhyCorrupt   = whyCorrupt
	WhyCutShort  = whyCutShort
	WhyNotTar    = whyNotTar
	WhyNotExport = whyNotExport
)

// ReadTranscriptWithin is [ReadTranscript] held to bounds: the cases about a
// bound run at one they can fill — a page of three entries, a read budget of a
// few kibibytes — and leave the engine's own to
// [TestTheTranscriptBoundsAreTheEnginesOwn].
func ReadTranscriptWithin(ctx context.Context, harness string, format int,
	open func(context.Context) (io.ReadCloser, error), from int,
	bounds TranscriptBounds) (Transcript, error) {
	return readTranscript(ctx, harness, format, open, from, ReadByPerson, bounds, TranscriptFilter{})
}

// ReadTranscriptFiltered is [ReadTranscriptAs] for a seat, held to bounds:
// for the cases about a filter at a bound they can fill.
func ReadTranscriptFiltered(ctx context.Context, harness string, format int,
	open func(context.Context) (io.ReadCloser, error), from int, bounds TranscriptBounds,
	filter TranscriptFilter) (Transcript, error) {
	return readTranscript(ctx, harness, format, open, from, ReadBySeat, bounds, filter)
}

// SparseBomb is a gzipped tar whose one member is named as a session's
// transcript and is a sparse file declaring size bytes, every one of them a
// hole: a few blocks of archive that hand their reader size bytes the archive
// does not hold ([sparseMember]).
func SparseBomb(t *testing.T, size int64) []byte { return sparseBomb(t, size) }

// TranscriptReadsHeld is how many of the process's [TranscriptReadsAtOnce]
// slots reads hold now: what a case reads to see a slot taken before an
// archive opens and given back only once it is closed.
func TranscriptReadsHeld() int { return len(transcriptReads) }

// SeatTranscriptReadsHeld is how many of the seats' [SeatTranscriptReadsAtOnce]
// slots reads hold now.
func SeatTranscriptReadsHeld() int { return len(seatTranscriptReads) }

// HoldingsReadsHeld is how many of the readings' [HoldingsReadsAtOnce] slots
// are held.
func HoldingsReadsHeld() int { return len(holdingsReads) }

// The Crewlet trailers' keys, for the cases that read a commit's trailers.
const (
	SeatTrailer     = seatTrailer
	RunTrailer      = runTrailer
	WorkItemTrailer = workItemTrailer
)

// AttributionHook is the hook a run asking for a is handed in the hooks
// directory hooks, byte for byte, or the refusal its start would answer.
func AttributionHook(a sandbox.CommitAttribution, hooks string) ([]byte, error) {
	trailers, err := attributionTrailers(a)
	if err != nil {
		return nil, err
	}
	return attributionHook(trailers, hooks), nil
}

// HooksParameter is the GIT_CONFIG_PARAMETERS item a run's wrapper points git
// at the hooks directory hooks with ([hooksParameter]).
func HooksParameter(hooks string) string { return hooksParameter(hooks) }

// ClaudeNoProxyHolder stands in a run's settings for its NO_PROXY list until
// the script merges it ([claudeNoProxyHolder]).
const ClaudeNoProxyHolder = claudeNoProxyHolder

// ClaudeSkillsFloor, ClaudeNotesFloor and ClaudeSettingsPinFloor are the
// oldest Claude Code releases a run is started with its skills' plugin, and the
// settings that keep its hook on, with its notes' hooks, and with its settings'
// pinned copy ([claudeSkillsFloor], [claudeNotesFloor],
// [claudeSettingsPinFloor]).
const (
	ClaudeSkillsFloor      = claudeSkillsFloor
	ClaudeNotesFloor       = claudeNotesFloor
	ClaudeSettingsPinFloor = claudeSettingsPinFloor
)

// ReleaseAtLeast reports whether a Claude Code release is at or after floor,
// as every gate on the engine's side reads one ([releaseAtLeast]).
func ReleaseAtLeast(version, floor string) bool { return releaseAtLeast(version, floor) }

// SkippedLine, handed to [StreamRequest] as a line, is one past the stream's
// bound ([MaxLineBytes]) rather than a line read.
const SkippedLine = "\x00a line past the stream's bound"

// StreamRequest is what h's decoder reads off a stream of lines of the newest
// request of the session its run worked in ([contextReader]): the measure, and
// where there is none, why, and whether that is a reading that failed, as the
// collection logs it at warning.
func StreamRequest(h Harness, lines ...string) (sandbox.RequestMeasure, string, bool) {
	dec := h.Events()
	for _, line := range lines {
		if line == SkippedLine {
			dec.Skipped(LongLine{Size: int64(maxLineBytes) + 1, Bound: maxLineBytes})
			continue
		}
		dec.Line([]byte(line))
	}
	reader, ok := dec.(contextReader)
	if !ok {
		return sandbox.RequestMeasure{}, "the harness's decoder reads no request", false
	}
	read := reader.request()
	return read.measure, read.why, read.failed
}

// FollowedNames is how many message ids an OpenCode decoder holds while a live
// reading follows it ([openCodeEvents.named]): first read before the reading
// first takes its entries, and the rest after, counted before it takes them
// again.
func FollowedNames(first string, rest ...string) int {
	d := &openCodeEvents{}
	d.Line([]byte(first))
	_ = d.Entries(false)
	for _, line := range rest {
		d.Line([]byte(line))
	}
	return len(d.named)
}

// OpenCodeContextPages and OpenCodeContextBytes are the most pages, and bytes
// in all, a run's script asks of its session's message list; and
// OpenCodeContextEvent the type of the lines it prints them in.
const (
	OpenCodeContextPages = openCodeContextPages
	OpenCodeContextBytes = openCodeContextBytes
	OpenCodeContextEvent = openCodeContextEvent
)

// ClaudeConfirmsATake is Claude Code's case of the note contract's confirm path
// (TestANoteServedIsConfirmedOffItsJobsOwnRecord): its hooks' records name the
// takes asked, each event held to its type.
func ClaudeConfirmsATake(t *testing.T) {
	t.Run("a hook record names only the takes it was asked", hookRecordNamesOnlyTheTakesItWasAsked)
	t.Run("a hook's event is held to its type", hooksEventIsHeldToItsType)
}

// RunnerNamed is a runner driving the harness this build names name.
func RunnerNamed(name string) *Runner { return New(harnessNamed(name)) }

// GeminiConfirmsATake is Gemini CLI's case of the note contract's confirm path
// (TestANoteServedIsConfirmedOffItsJobsOwnRecord): the take script's record of
// its takes, confirmed in order off the session's own user messages.
func GeminiConfirmsATake(t *testing.T) {
	t.Run("a run's receipts are read off its takes and its session", geminiReceiptsAreReadOffItsTakesAndItsSession)
}

// ReadQwenSession is the transcript a Qwen Code chat file reads as
// ([transcriptBuilder.qwenSession]), held to bounds.
func ReadQwenSession(ctx context.Context, raw []byte, bounds TranscriptBounds) (Transcript, error) {
	b := newTranscriptBuilder(0, bounds, TranscriptFilter{})
	if err := b.qwenSession(ctx, bytes.NewReader(raw)); err != nil && !errors.Is(err, errTranscriptPage) {
		return Transcript{}, err
	}
	return Transcript{Entries: b.entries, Places: b.places, From: b.from, Next: b.next, Cut: b.cut}, nil
}

// ReadGeminiSession is the transcript a Gemini CLI main session file reads as
// ([transcriptBuilder.geminiSession]), held to bounds.
func ReadGeminiSession(ctx context.Context, raw []byte, bounds TranscriptBounds) (Transcript, error) {
	b := newTranscriptBuilder(0, bounds, TranscriptFilter{})
	if err := b.geminiSession(ctx, bytes.NewReader(raw)); err != nil && !errors.Is(err, errTranscriptPage) {
		return Transcript{}, err
	}
	return Transcript{Entries: b.entries, Places: b.places, From: b.from, Next: b.next, Cut: b.cut}, nil
}

// MeterCount is the count h's take meters archive at, release the box's word
// on the release that wrote it ([carrier.meter]).
func MeterCount(h Harness, release string, archive io.Reader) *sandbox.ResumedCount {
	read, _ := h.(carrier).meter(context.Background(), archive, "", release, new(passBudget))
	return read.count
}

// GeminiPoint and QwenPoint are where each harness compresses a run on model
// ([geminiPoint], [qwenPoint]).
func GeminiPoint(model string) int64 { return geminiPoint(model) }
func QwenPoint(model string) int64   { return qwenPoint(model) }

// The bytes each box point hands over beside the brief at least: Gemini CLI's
// session context's fixed part ([geminiContextBase]) and Qwen Code's dated
// reminder ([qwenReminderBytes]).
const (
	GeminiContextBase = geminiContextBase
	QwenReminderBytes = qwenReminderBytes
)

// QwenClearsAt is the tool answers' size past which Qwen Code clears the
// oldest at its first request ([qwenClearsAt]), and QwenSyntheticFailureBytes
// the failure it hands an unanswered call ([qwenSyntheticFailureBytes]).
const (
	QwenClearsAt              = qwenClearsAt
	QwenSyntheticFailureBytes = qwenSyntheticFailureBytes
)

// EachLine is [eachLine], for the cases of a line past a bound the suite
// lowers.
func EachLine(r io.Reader, dec Decoder, bound int) error { return eachLine(r, dec, bound) }

// CollectedCount is what [Runner.Collect] counts of stream, read whole through
// its own reading of an event stream.
func CollectedCount(r *Runner, stream []byte) (sandbox.RequestCount, error) {
	read, _, err := r.decodeLines(bytes.NewReader(stream), false)
	return sandbox.RequestCount{Requests: read.requests, InFlight: read.inFlight, Undecided: read.undecided}, err
}

// CountReadBytes is the most one read of a live count takes.
const CountReadBytes = countReadBytes

// CountingBy is r's live count of a job, each read taking at most readBytes.
func CountingBy(r *Runner, readBytes int64) sandbox.Counter {
	switch c := r.Count(sandbox.RunHandle{}).(type) {
	case *counter:
		c.readBytes = readBytes
		return c
	case *recordCount:
		c.readBytes = readBytes
		return c
	case *maxCount:
		for _, part := range c.parts {
			switch p := part.(type) {
			case *counter:
				p.readBytes = readBytes
			case *recordCount:
				p.readBytes = readBytes
			}
		}
		return c
	}
	return nil
}

// RecordCounted reports whether h is counted live off its harness's own
// record of the session rather than its stream.
func RecordCounted(h Harness) bool {
	_, one := h.(recordCounted)
	_, several := h.(recordsCounted)
	return one || several
}

// CodexRolloutKinds is the rollout lines a Codex run's script appends.
const CodexRolloutKinds = codexRolloutKinds

// MuseTraceNeedle is how a Muse Code run's script picks its traces out.
const MuseTraceNeedle = museTraceNeedle

// CollectedRecordCount is what r's collection counts off its harness's own
// record in box ([recordCollected]), or its records beside a stream that counts
// none of them ([recordsCounted]); none for a harness counted off its stream.
func CollectedRecordCount(ctx context.Context, r *Runner, box sandbox.Sandbox) (sandbox.RequestCount, error) {
	if rs, ok := r.harness.(recordsCounted); ok {
		return r.collectedRecords(ctx, box, rs, sandbox.RequestCount{})
	}
	rc, ok := r.harness.(recordCollected)
	if !ok {
		return sandbox.RequestCount{}, nil
	}
	return r.collectedRecord(ctx, box, rc)
}

// StoreCounted reports whether h is counted off its session store, asked
// through the box.
func StoreCounted(h Harness) bool {
	_, ok := h.(storeCounted)
	return ok
}
