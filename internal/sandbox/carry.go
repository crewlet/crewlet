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
// A run's harness keeps its conversation in its box (Claude Code as a
// transcript, OpenCode in its database) and the box is reclaimed when its turn
// settles. So a session to be continued in another box leaves PACKED, as one
// archive the run wrote when its harness exited ([Result.Captured]), and
// enters the next as that archive, written in before its run starts
// ([RunRequest.Carried]) and restored by the run's own script. What an
// archive holds, and never holds (a login, the harness's settings, Claude
// Code's file-edit snapshots of the files a run edited, `file-history/`), is
// its runner's to say (internal/sandbox/executor), and so is whether a box can
// continue one ([CarryCheck]).

// SessionArchive describes a harness's session packed into one archive: by
// the box its run ended in, or as it is carried into another.
type SessionArchive struct {
	// Harness is the runner whose harness wrote the session ("claude-code",
	// "opencode"): a session is continued only by the harness that wrote it.
	Harness string

	// SessionID is the session's id, in its harness's own form.
	SessionID string

	// Entry is the providers.llm entry the session ran under, as an identity
	// naming no credential (internal/providers/llm/harness's
	// Provider.EntryID), "" where its record names none: a launch under
	// another entry continues it only where [EntryContinues] says so.
	Entry string

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

	// RunStart is where the run that packed the archive began in its session,
	// as the run's script recorded it in its box ([RunStart]): the box's word,
	// as the session's id and its release are, and kept by the take as the
	// take keeps the session ([SessionCapture.RunBegan]). The zero value is not
	// known: a runner whose script records none, and an older host's answer.
	// Its Lines is always zero here: a chat file's start is Prefix, which only
	// the take turns into a count.
	RunStart RunStart

	// Prefix is where a Gemini CLI or Qwen Code run began in its chat file, as
	// its script counted and digested it; zero where it recorded none.
	Prefix LinePrefix

	// Unshadowed is the box's word that, as its run started, no MCP server its
	// harness could load named a tool as the harness names the engine's
	// (Gemini CLI's guard, internal/sandbox/executor's geminiShadowGuard), kept
	// by the take ([SessionCapture.RunUnshadowed]). False is "not checked":
	// a harness with no guard, a box that failed it, an older build's word.
	Unshadowed bool
}

// RunStart is where the run that packed a session's archive began in that
// session: the whole session its own (Began); every message after the one
// After names, the newest the session held before the run, as the run's script
// asked its server (internal/sandbox/executor); or, in a chat file, every line
// after its first Lines, a count only a take that checked the box's digest of
// those lines keeps ([LinePrefix], [TakenSession.RunLines]).
//
// THE ZERO VALUE IS "NOT KNOWN", which says nothing of where the run began: a
// run whose record names none — a harness whose script records no start, a
// continued run whose server never answered, an older build's record — and
// never "the whole session".
type RunStart struct {
	Began bool
	After string
	Lines int64
}

// Known reports whether s says where its run began: exactly one of it began
// its session, it began after a message whose id is one a box may name
// ([ValidMessageID]), or it began after its session's first Lines lines,
// within [MaxRunStartLines].
func (s RunStart) Known() bool {
	switch {
	case s.Began:
		return s.After == "" && s.Lines == 0
	case s.Lines != 0:
		return s.After == "" && s.Lines > 0 && s.Lines <= MaxRunStartLines
	}
	return ValidMessageID(s.After)
}

// MaxRunStartLines bounds a run start's line count ([RunStart.Lines]): 1<<27,
// a chat file at the take's 256 MiB plaintext bound holding a line of at
// least two bytes (a byte and its break) each. A count past it names no line
// any kept session holds.
const MaxRunStartLines = 1 << 27

// LinePrefix is a box's word on the lines of its chat file a run began after
// ([SessionArchive.Prefix]): their count, and the POSIX `cksum` of exactly
// those lines, CRC and length, as the run's script took it before its harness
// started. NEVER KEPT ON A CAPTURE: the take checks it against the archive's
// own bytes, and keeps only the count it found whole ([TakenSession.RunLines]).
type LinePrefix struct {
	Lines int64
	CRC   uint32
	Bytes int64
}

// MaxMessageIDBytes bounds a session's message id as the engine keeps one
// ([ValidMessageID]): SIXTY-FOUR BYTES of letters, digits, `-` and `_`. The
// ids OpenCode mints are `msg_` and twenty-six characters, thirty bytes
// (testdata/opencode/<version>/export.json in internal/sandbox/executor), so
// the bound is twice them and a little more, as the call id's is
// ([MaxBridgeCallIDBytes]), and an id past it is no id rather than one cut:
// two ids cut alike would name one message.
const MaxMessageIDBytes = 64

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
// the box's harness it read to decide, asked before the run's brief is chosen:
// what decides whether the brief told by a reading of what the session holds
// is handed ([ExactTelling]).
type CarryCheck struct {
	// Version is the box harness's release as its `--version` gave it
	// ("2.1.310"); "" where it printed nothing this build can read, which
	// continues no carried session (the box named no release any rule was
	// read for) and matches no release a reading was read for
	// ([ExactTelling.Resumer]).
	Version string

	// ResumeRead says Version is a release whose resume this build read
	// (internal/sandbox/executor's claudeResumeRead): only its resume
	// rebuilds a hook's note as the job's session showed it, so only it is
	// handed a brief that counts such a read held ([UnheldTelling]).
	ResumeRead bool

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

// ValidMessageID reports whether id is one a box may name a message of its
// session by, as the engine keeps one ([RunStart.After]): letters, digits, `-`
// and `_`, at most [MaxMessageIDBytes] of them — the shape a session's id and a
// call's are held to, at a bound of its own. The id is the box's word, a line of
// its session record, so a runner refuses any other as it reads the box
// (internal/sandbox/executor) and the take as it keeps the start
// ([SessionCapture.Bounded]).
func ValidMessageID(id string) bool {
	return id != "" && len(id) <= MaxMessageIDBytes && !strings.ContainsFunc(id, outsideIDCharset)
}

// outsideIDCharset reports whether r is outside the characters an id a box
// hands the engine is held to — letters, digits, `-` and `_` — which a
// session's id ([ValidSessionID]), a call's ([ValidCallID]) and a message's
// ([ValidMessageID]) are each held to, at bounds of their own, and the call a
// hook's serve answered as well ([keptCallOf]): each is the box's word, and
// each written as itself by the store's encoder, so none costs more than its
// length but for a string id's own two quotes.
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
	archiveJudge

	// CheckCarry asks box whether its harness can continue archive, a session
	// packed in another box: [Runner.CheckCarry] in internal/sandbox/executor,
	// which reads the box harness's release. Three answers: it can, it cannot
	// (and why), and the box could not be asked, an error.
	CheckCarry(ctx context.Context, box Sandbox, archive SessionArchive) (CarryCheck, error)

	// CheckInPlace asks box whether its harness continues the copy of a
	// session the box itself holds, before a relaunch there hands the brief
	// that continues it: [Runner.CheckInPlace] in internal/sandbox/executor,
	// which holds a harness that continues no session on a release it was not
	// read for (Codex, Copilot, Kimi Code, Muse Code, Pi, Grok Build) to it, the
	// box's release and the release that wrote the copy alike. Three answers, as CheckCarry's.
	CheckInPlace(ctx context.Context, box Sandbox) (CarryCheck, error)
}

// archiveJudge is a runner as a launch under one entry asks whether it takes
// up a carried session ([SessionRestorer], [HoldingsReader]).
type archiveJudge interface {
	// RestoresUnder reports whether this runner restores archive for a launch
	// under entry (an identity as [SessionArchive.Entry] holds one) —
	// written by its harness, in the one archive format it packs, by a
	// release whose sessions another box continues, under an entry
	// [EntryContinues] lets it continue under — and, where it does not, why:
	// what can be known before any box is asked, the box's own release aside.
	RestoresUnder(archive SessionArchive, entry string) (bool, string)
}

// RestorerUnder is runner as a launch under entry asks it ([ArchiveRestorer]):
// the one way a runner answers [ContinuityOf] and [BaseContinuityOf], so no
// reader decides a session continues without the entry it would run under.
// Nil where runner is.
func RestorerUnder(runner archiveJudge, entry string) ArchiveRestorer {
	if runner == nil {
		return nil
	}
	return entryRestorer{runner: runner, entry: entry}
}

// entryRestorer is [RestorerUnder]'s answer.
type entryRestorer struct {
	runner archiveJudge
	entry  string
}

func (r entryRestorer) Restores(archive SessionArchive) (bool, string) {
	return r.runner.RestoresUnder(archive, r.entry)
}

// EntryContinues is THE decision whether a session that ran under the
// providers.llm entry ran continues in a launch of harness under the entry
// launch, and where not, why. A harness keeps what a session was created with
// in its own files, so A CARRIED SESSION CONTINUES ONLY UNDER THE ENTRY IT RAN
// UNDER — the same key, auth mode and route — unless launchRouted: its harness
// was measured, on its newest release, to send a resumed session's calls on
// the route its launch hands it. An unrecorded entry is no match. ADR-0073's
// amendment "a carried session continues only under the entry it ran under".
func EntryContinues(ran, launch, harness string, launchRouted bool) (bool, string) {
	const rule = " was not measured to send a resumed session's calls on its launch's route, and a carried " +
		"session continues only under the entry it ran under"
	switch {
	case launchRouted:
		return true, ""
	case ran == "":
		return false, "the session's record names no providers.llm entry it ran under, and " + harness + rule
	case ran != launch:
		return false, "the session ran under another providers.llm entry, or under this one with another " +
			"auth mode or route, and " + harness + rule
	}
	return true, ""
}

// SessionTaker is a [Runner] whose harness's session a capture takes out of a
// box (internal/sandbox/executor's Runner): the archive the run packed when
// its harness exited, one packed now for a job that never got to (killed
// early, lost with its engine, a box that would not end), or a save packed
// while the job still worked ([SessionTake.Save]).
//
// THREE ANSWERS: the session, taken; the zero [TakenSession], where the box
// holds none; and an error, where it holds one that could not be taken (a
// failed pack, an archive past its bound), which the capture stamps refused
// so a later launch knows the session was not kept rather than reading an
// older capture as the latest. OPTIONAL, as [noteReader] is: a runner that
// carries no session keeps nothing.
type SessionTaker interface {
	TakeSession(ctx context.Context, box Sandbox, take SessionTake) (TakenSession, error)
}

// SessionTake is what a capture asks of a box ([SessionTaker]).
type SessionTake struct {
	// Pack says the session is packed NOW, by the runner's own pack: a job
	// still working, which may have gone on since anything it packed. False
	// is a job that is over: the archive its script packed when its harness
	// exited where it left one, and one packed now where it left none (a
	// crash before its pack, a job killed part way, a job lost with its
	// engine). Every pack made now is the take's own, streamed from a place of
	// its own and never moved over the job's capture, which only the job's
	// script writes, so each read streams an archive nothing moves while it
	// reads. Every pack records the archive's digest in the box, as the
	// script's does, before moving the archive where its read reads it (a pack
	// that could not record it fails), so the box never takes its own archive
	// for another box's.
	Pack bool

	// Save names the SAVE taken in place of the job's own session: the path
	// internal/sandbox/executor's Runner.PackSave packed it to and described
	// it at ([SessionArchive.Path]): a save the taking process packed of the
	// box's session while its job still worked, under its taker lease
	// ([SessionCapture.Taker]), streamed by this take alone and removed once
	// done. Never the job's capture (every read of a finished job takes it as
	// its final archive); never a path another save moves over, which a take
	// reading its archive twice would find changed; never packed by the take:
	// the save is packed first, so the scrub's values (Scrub) are read after
	// the pack and hold every value the box was handed by then. A path that
	// is no save of the box's is refused, and so is Pack beside it. "" takes
	// the job's own session.
	Save string

	// Scrub is every secret the engine itself put in the box, by value — its
	// login, its environment's credentials ([HandedSecrets]) — which is taken
	// out of every file of the archive in its raw form and in the form a JSON
	// string holds it in, before the redaction every text leaving a box
	// passes.
	Scrub []string

	// MaxBytes is the largest archive kept (executorkeep.SessionMaxBytes):
	// one the box describes as larger is refused before a byte of it is read,
	// and it bounds the plaintext the scrub reads out of the archive, and with
	// it what the scrub holds, which holds no piece whole: an archive the
	// box's own code wrote cannot cost the engine host more than that.
	MaxBytes int64

	// Scope is whose sessions the take's scrub shares what it learned
	// between: the run's agent (PendingRun.AgentID), which the engine
	// writes, so a box timing its takes learns nothing of another agent's
	// texts. "" shares with every other take that names none.
	Scope string

	// Session is the session bound to a minting harness's job
	// ([LaunchRecord.Minted]): the take names that one, and one whose box
	// names another is refused, wrapping [ErrSessionBindRefused], so no
	// capture is kept under an id never bound. Ignored for a harness that
	// names its session as its run starts.
	Session string

	// Spool is where the take's scrub spools what its first reading of the
	// archive wrote, for its second to copy, and the room it holds for that
	// ([SessionSpool]). A take that names no directory is refused.
	Spool SessionSpool
}

// SessionSpool is the room a take's scrub spools in ([SessionTake.Spool]): a
// directory of the process's own, so the sweep that removes a crashed
// process's spools removes the scrub's too; the bytes the spool may write
// there, counted as written and refused past; and Held, told the bytes the
// spool holds whenever that moves, none once it is removed, which it is
// before the take's body ends.
type SessionSpool struct {
	Dir   string
	Bytes int64
	Held  func(bytes int64)
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

	// Next is the harness's own estimate of the session's next request
	// ([NextRequest]), off the same read of the archive's main thread as
	// Count, once Body has been read to its end: zero where the thread was
	// not read whole ([SessionCapture.NextRequestTokens]). Nil from a taker
	// that estimates nothing, as one of another harness is.
	Next func() NextRequest

	// Count is what the harness counts the session at before its next
	// request, as the archive bounds it read whole ([ResumedCount]): Claude
	// Code's main thread, OpenCode's export. Once Body has been read to its
	// end: a bound, or a mark saying why there is none ([CountUnread] where
	// Body was not read whole). Nil from a taker that counts nothing, and a
	// nil count from it is none.
	Count func() *ResumedCount

	// CountProvider is the provider of the reply an OpenCode count is
	// anchored on, off the same read, once Body has been read to its end
	// ([SessionCapture.CountProvider]): "" for a Claude Code count and where
	// no reply anchored one. Nil from a taker that names none.
	CountProvider func() string

	// RunLines is the run start's line count ([RunStart.Lines]) once Body has
	// been read to its end: Archive.Prefix's Lines where the archive's chat
	// file holds that many whole lines and their CRC and length are the
	// prefix's, else 0, as for a file the box rewrote, cut or rewound before
	// it was taken. Nil from a taker that checks none.
	RunLines func() int64

	// ResumeRead says Archive.Version is a release whose resume this build
	// read, as [CarryCheck.ResumeRead] says of a box's (the executor's
	// claudeResumeRead), false for every other harness's: only such a Claude
	// Code capture is raised ([SessionCapture.placeAtNewest]).
	ResumeRead bool
}

// SessionPackWait bounds a runner's on-demand pack of a session in its box
// ([SessionTake.Pack]), a save's among them. TWO MINUTES: a minute for a
// box's harness to come up on a contended box (an OpenCode export starts a
// private server), and a minute for tar and gzip over a session at its bound,
// some hundreds of mebibytes of JSON at the tens of mebibytes a second a
// contended box packs at. A pack not finished by then is not going to; the
// capture is refused rather than left holding the box. A Claude Code pack
// beside a working harness starts no server; its first minute covers its
// checks (internal/sandbox/executor's claudeHeldTar): the clock wait, five
// seconds at most; find walks (7 to 22 ms measured); its archive decompressed
// once to list it and once per transcript read-back (gunzip is several times
// faster than gzip compresses, unmeasured): some ten read-backs.
const SessionPackWait = 2 * time.Minute
