package executor

import (
	"cmp"
	"context"
	"io"
	"testing"
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
// end takes, so a credential that began before what is kept is still redacted.
const RedactContext = redactContext

// StatedSizeGrain is what the size an entry states of a text a read redacted
// only the opening of is rounded up to.
const StatedSizeGrain = statedSizeGrain

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
	// Clock is the command the script reads the time from, in whole seconds.
	Clock string
}

// OpenCodeCommandTimed is [OpenCode.Command] with the script run at times —
// for the cases about a wait running out, a sync coming round or a stop that
// must not wait, at lengths a test can tell apart.
func OpenCodeCommandTimed(req sandbox.RunRequest, session Session, paths Paths, times OpenCodeTimes) string {
	return "sh -c " + sandbox.ShellQuote(openCodeScript(req, session, paths, openCodeTimes{
		listen: cmp.Or(times.Listen, openCodeListenWait), mcp: cmp.Or(times.MCP, openCodeMCPWait),
		sync: cmp.Or(times.Sync, openCodeLoginSync), stop: cmp.Or(times.Stop, sandbox.JobStopGrace),
		clock: cmp.Or(times.Clock, openCodeClock),
	}))
}

// ClaudeCodeCommandTimed is [ClaudeCode.Command] with the harness given stop to
// go once its run is asked to end, rather than [sandbox.JobStopGrace]: for the
// cases about a harness that outlives its grace, at a length a test can wait
// out.
func ClaudeCodeCommandTimed(req sandbox.RunRequest, session Session, paths Paths, configPath string,
	stop time.Duration) string {
	return "sh -c " + sandbox.ShellQuote(claudeScript(req, session, paths, configPath, stop))
}

// ClaudeProjectDir is the project directory every Claude Code run in a box
// files its sessions under, and the archive formats each harness's run packs its
// session in: what a test reads a box's layout and a capture by.
const (
	ClaudeProjectDir      = claudeProjectDir
	ClaudeArchiveFormat   = claudeArchiveFormat
	OpenCodeArchiveFormat = openCodeArchiveFormat
)

// HeldPlainMax is the most plaintext a pack beside a working harness packs
// ([heldPlainMax]): what a case sizes a session past it by.
const HeldPlainMax = heldPlainMax

// OpenCodeStops is a run's script's stops ([openCodeStops]) beside the end they
// stop with ([endFunction]) at stop, and OpenCodeExporter its login exporter
// ([openCodeExporter]) at sync into file: the two pieces a case about stopping
// the exporter runs on their own, since no run of the whole script can choose
// the instant its exporter is stopped.
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

// The Crewlet trailers' keys, for the cases that read a commit's trailers.
const (
	SeatTrailer     = seatTrailer
	RunTrailer      = runTrailer
	WorkItemTrailer = workItemTrailer
)

// AttributionHook is the hook a run asking for a is handed, byte for byte, or
// the refusal its start would answer.
func AttributionHook(a sandbox.CommitAttribution) ([]byte, error) {
	trailers, err := attributionTrailers(a)
	if err != nil {
		return nil, err
	}
	return attributionHook(trailers), nil
}

// ClaudeSkillsFloor, ClaudeNotesFloor and ClaudeCompactionPinFloor are the
// oldest Claude Code releases a run is started with its skills' plugin, and the
// settings that keep its hook on, with its notes' hooks, and with its settings'
// pinned copy ([claudeSkillsFloor], [claudeNotesFloor],
// [claudeCompactionPinFloor]).
const (
	ClaudeSkillsFloor        = claudeSkillsFloor
	ClaudeNotesFloor         = claudeNotesFloor
	ClaudeCompactionPinFloor = claudeCompactionPinFloor
)

// ReleaseAtLeast reports whether a Claude Code release is at or after floor,
// as every gate on the engine's side reads one ([releaseAtLeast]).
func ReleaseAtLeast(version, floor string) bool { return releaseAtLeast(version, floor) }
