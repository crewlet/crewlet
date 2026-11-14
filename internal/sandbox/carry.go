package sandbox

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// A HARNESS'S OWN SESSION, CARRIED BETWEEN BOXES.
//
// A run's harness keeps its conversation in its box — Claude Code as a transcript
// under its configuration directory, OpenCode in its database — and the box
// is reclaimed when its turn settles. So a session that is to be continued in
// another box leaves the one it ran in PACKED, as one archive the box's run
// wrote when its harness exited ([Result.Captured]), and enters the next one as
// that archive, written into the box before its run starts
// ([RunRequest.Carried]) and restored there by the run's own script before its
// harness takes the session up. What a harness's archive holds, and what it never holds
// — a login, the harness's own settings, Claude Code's file-edit snapshots of the
// files a run edited (`file-history/`) — is its runner's to say
// (internal/sandbox/executor), and so is whether a box can continue one at all
// ([CarryCheck]).

// SessionArchive describes a harness's session packed into one archive: by
// the box its run ended in, or as it is carried into another.
type SessionArchive struct {
	// Harness is the runner whose harness wrote the session ("claude-code",
	// "opencode"): a session is continued only by the harness that wrote it.
	Harness string

	// SessionID is the session's id, in its harness's own form.
	SessionID string

	// Format is the version of the archive's layout, its runner's constant
	// for its harness: a runner restores only the format it packs, and refuses
	// any other rather than reading it as one.
	Format int

	// Version is the release of the harness that wrote the session, as its
	// `--version` gave it ("2.1.310"), which decides whether another box's
	// harness can continue it ([CarryCheck]); "" where the run that wrote it
	// recorded none this build can read.
	Version string

	// Size is the archive's length in bytes, as its box reported it when it
	// was described — read before its bytes are, so a reader can refuse one
	// too large to keep before reading any of it.
	Size int64

	// Path is where the box holds the archive, in the box's own paths. Set on
	// a capture ([Result.Captured]); a carried session's is its runner's to
	// choose.
	Path string
}

// CarriedSession is a session archive handed to a run to continue
// ([RunRequest.Carried]): what it is, and its bytes, read once by the run's
// start and never closed by it — the caller opened them and closes them.
type CarriedSession struct {
	Archive SessionArchive
	Body    io.Reader

	// Replace says the archive replaces the box's own copy of the session
	// even where that copy already holds it (carry.go's digest rule): set for
	// a session captured from a harness that was CUT OFF ([SessionCapture.Interrupted]),
	// whose copy in the box it ran in may hold a record its harness never
	// finished — a half-written transcript line, an OpenCode step that never
	// settled — which the archive leaves out, or calls made after the capture,
	// which the brief that continues it lists instead. False keeps a copy that
	// holds the archive, which is the archive or newer.
	Replace bool
}

// CarryCheck is a box's answer to whether a session carried from another box
// can be continued in it (executor's Runner.CheckCarry), with the release of
// the box's harness it read to decide, which the run's record carries.
type CarryCheck struct {
	// Version is the box harness's release as its `--version` gave it
	// ("2.1.310"); "" where it printed nothing this build can read, which is
	// taken for a current release.
	Version string

	// Continues says the session can be continued here.
	Continues bool

	// Why says, in a person's words, why it cannot; "" where it can.
	Why string
}

// ValidSessionID reports whether id has the shape every session id a harness
// mints has — Claude Code's UUID, OpenCode's `ses_` and its characters —
// loosely: letters, digits, `-` and `_`, at most
// [executorkeep.SessionIDMaxBytes] of them.
//
// ONE RULE, HELD WHEREVER A BOX NAMES A SESSION. The id is the box's word —
// a line of its session record, or what its harness printed, which its own
// agent can write as it likes — and it names an archive, is handed to a later
// run's script, and rides the launch's record, whose fit it may not decide. So
// a runner refuses any other as it reads the box (internal/sandbox/executor),
// and the record refuses it again as it is published ([collectedRecord]),
// whatever runner, or host of another build, read the box.
func ValidSessionID(id string) bool {
	return id != "" && len(id) <= executorkeep.SessionIDMaxBytes && !strings.ContainsFunc(id, outsideIDCharset)
}

// outsideIDCharset reports whether r is outside the characters an id a box
// hands the engine is held to — letters, digits, `-` and `_` — which a
// session's id ([ValidSessionID]) and a call's ([ValidCallID]) are each held
// to, at bounds of their own: each is the box's word, and each written as
// itself by the store's encoder, so neither costs more than its length.
func outsideIDCharset(r rune) bool {
	return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
}

// ValidRelease reports whether release has the shape of a harness's release as
// the engine reads one off its `--version` ("2.1.310"): three runs of digits
// joined by dots, at most [executorkeep.HarnessVersionMaxBytes] bytes, the
// most the executor log records one at.
//
// A RELEASE HAS A LENGTH, AND ITS NUMBERS DO NOT: they are runs of digits a box
// printed, so a run as long as the line read as a release. One past the bound
// is refused, never cut, because a shortened release names another — and read
// as none, it is what every reader takes a release it cannot read for, a
// current one. Held where [ValidSessionID] is, for its reasons.
func ValidRelease(release string) bool {
	if release == "" || len(release) > executorkeep.HarnessVersionMaxBytes {
		return false
	}
	parts := 0
	for part := range strings.SplitSeq(release, ".") {
		parts++
		if part == "" || strings.ContainsFunc(part, func(r rune) bool { return r < '0' || r > '9' }) {
			return false
		}
	}
	return parts == 3
}

// SessionRestorer is a [Runner] that continues a session carried from another
// box (internal/sandbox/executor's Runner, for a harness it packs and
// restores): what the launch asks of it before it hands a run a carried session,
// and what every reader asks that says whether a turn's session can be
// continued at all ([ContinuityOf]).
//
// OPTIONAL, as [SessionTaker] is: a runner that carries no session continues
// none, and a launch on it starts every relaunch fresh.
type SessionRestorer interface {
	// Restores reports whether this runner restores archive at all — written
	// by its harness, in the one archive format it packs, by a release whose
	// sessions another box continues — and, where it does not, why: what can
	// be known of a carried session before any box is asked, the box's own
	// release aside.
	Restores(archive SessionArchive) (bool, string)

	// CheckCarry asks box whether its harness can continue archive, a session
	// packed in another box: [Runner.CheckCarry] in internal/sandbox/executor,
	// which reads the box harness's release. Three answers: it can, it cannot
	// (and why), and the box could not be asked, an error.
	CheckCarry(ctx context.Context, box Sandbox, archive SessionArchive) (CarryCheck, error)
}

// SessionTaker is a [Runner] whose harness's session a capture takes out of a
// box (internal/sandbox/executor's Runner, for a harness it packs and restores):
// the archive the run packed when its harness exited — at its own end, or at
// the end a stop, a loss or its turn's cap asked of it — or one packed now, on
// demand, for a job that never got to: one killed before its script could
// pack, one lost with its engine whose box is still there, one a box would not
// end; or a save a taker packed while the job still worked ([SessionTake.Save]).
//
// THREE ANSWERS: the session, taken; the zero [TakenSession], where the box
// holds no session to take — a harness that wrote none, a run whose record
// names none; and an error, where it holds one that could not be taken — a
// pack that failed, an archive past its bound — which the capture stamps
// refused, so a later launch knows the job's session was not kept rather than
// reading an older capture as its latest.
//
// OPTIONAL, as [noteReader] is: a runner that carries no session — a harness not
// taught to pack one, a test's — keeps nothing, and every capture of its runs
// is no capture at all.
type SessionTaker interface {
	TakeSession(ctx context.Context, box Sandbox, take SessionTake) (TakenSession, error)
}

// SessionTake is what a capture asks of a box ([SessionTaker]).
type SessionTake struct {
	// Pack says the session is packed NOW, by the runner's own pack: a job
	// still working, which a box would not end, and which may have gone on
	// since anything it packed. False is a job that is over: the archive its
	// script packed when its harness exited where it left one — at its own
	// end, or at the end it was asked for, a stop's, a loss's or its turn's
	// cap's — and one packed now where it left none (a crash before its pack,
	// a job killed part way through it, a job lost with its engine). Every
	// pack made now, either way, is the take's own, streamed from a place of
	// its own and never moved over the job's capture, which the job's script
	// alone writes: so each read streams the archive its own pack made, and
	// one that streams the job's capture streams an archive nothing moves
	// over while it reads it. Every pack records the archive's digest in the
	// box as the script's does — before it moves the archive where its read
	// reads it, so a pack that could not record it fails — so the box never
	// takes its own archive for another box's.
	Pack bool

	// Save names the SAVE taken in place of the job's own session: the path
	// internal/sandbox/executor's Runner.PackSave packed it to and described
	// it at ([SessionArchive.Path]) — a save the taking process packed of the
	// box's session while its job still worked, under its taker lease
	// ([SessionCapture.Taker]), at a place of that save's own — streamed by
	// this take alone and removed once it is done with it. Never the job's
	// capture, which every read of a job that is over takes as its final
	// archive; never a path another save moves over, which a take that reads
	// its archive twice would find changed between its passes; and never
	// packed by the take: the save is packed first, so the scrub's values
	// (Scrub) are read after the pack, and hold every value the box was
	// handed by then. A path that is no save of the box's is refused, and so
	// is Pack set beside it. "" takes the job's own session.
	Save string

	// Scrub is every secret the engine itself put in the box, by value — its
	// login, its environment's credentials ([HandedSecrets]) — which is taken
	// out of every file of the archive in its raw form and in the form a JSON
	// string holds it in, before the redaction every text leaving a box
	// passes.
	Scrub []string

	// MaxBytes is the largest archive kept (executorkeep.SessionMaxBytes):
	// one the box describes as larger is refused before a byte of it is read,
	// and it bounds what the scrub reads out of the archive — the plaintext
	// it expands to, and each piece of it the scrub holds whole — so an
	// archive the box's own code wrote cannot cost the engine host more than
	// that.
	MaxBytes int64
}

// TakenSession is a session archive on its way out of its box: described, and
// streamed as it is to be kept — every value of [SessionTake.Scrub] taken out
// and every file redacted, each in a way that leaves the file what it was (a
// JSON document still JSON, a transcript still one JSON value a line).
//
// The zero value is a box that holds no session to take.
type TakenSession struct {
	// Archive describes the archive as the box packed it; its Size is the
	// box's archive, before the scrub, which may change it.
	Archive SessionArchive

	// Body streams the archive scrubbed, a piece at a time. The caller
	// closes it, which is safe while a Read is in flight and ends one.
	Body io.ReadCloser

	// Keep records in the box that its own copy of the session holds the
	// archive Body yielded — the digest a carried archive is compared with
	// before it is restored ([RunRequest.Carried]) — so that this archive,
	// carried back into the box it was taken from, is never restored over
	// the copy that box went on with. It refuses a Body not read to its end,
	// whose digest is no archive's.
	Keep func(ctx context.Context) error

	// Context is the WHOLE INPUT of the last request the session's harness
	// sent its model, cache included, as the archive's own transcript
	// recorded it — Claude Code's last assistant message's usage, OpenCode's
	// last settled assistant message's tokens — once Body has been read to
	// its end: the context a launch that continues the session sends again on
	// its first request, which that launch's budget floor reads
	// ([SessionCapture.ContextTokens]). Zero where the transcript recorded
	// none this build reads, and where Body was not read whole. Nil from a
	// taker that measures nothing.
	Context func() int64

	// Count is what Claude Code counts the session at before its next
	// request, as the archive's own main thread bounds it read whole
	// ([ResumedCount]), once Body has been read to its end: a bound, or a mark
	// saying why there is none ([CountUnread] where Body was not read whole).
	// Nil from a taker that counts nothing, as one of another harness is, and
	// a nil count from it is none.
	Count func() *ResumedCount
}

// SessionPackWait bounds a runner's on-demand pack of a session in its box
// ([SessionTake.Pack]), a save's among them.
//
// TWO MINUTES: a minute for what a box's harness takes to come up on a
// contended box (an OpenCode export runs against a private server it starts,
// which takes about a second and is given a minute — the room a run's own
// server is given to listen), and a minute for tar and gzip over a session at
// its bound, which reads some hundreds of mebibytes of JSON at the tens of
// mebibytes a second a contended box still packs at. A pack that has not
// finished by then is not going to, and the capture is refused rather than
// left holding the box.
//
// A CLAUDE CODE PACK BESIDE A WORKING HARNESS starts no server, and its first
// minute is what its checks add (internal/sandbox/executor's claudeHeldTar):
// the wait for the box's clock, five seconds at most; walks of the session's
// files with find, milliseconds (7 to 22 ms measured for the whole manifest),
// and one more before the marker, which sets a file stamped in the box's
// future back to the box's present; and its archive decompressed once to list
// it, once more for the session's own transcript, which it reads back on every
// pack, and once more for each subagent's thread written as it was packed,
// each such pass reading the whole archive. gunzip reads several times faster
// than gzip compresses — a judgment, decompression's pace unmeasured — so at
// the tens of mebibytes a second the pack's own minute is sized by, a pass
// over a session at its plaintext bound takes a few seconds, and that first
// minute covers some ten read-backs — the main transcript's, and those of its
// subagents' threads growing as the session is packed. A pack past it is
// refused as any other is, the session's last capture standing.
const SessionPackWait = 2 * time.Minute
