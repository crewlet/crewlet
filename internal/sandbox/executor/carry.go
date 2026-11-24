package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A run's harness session, carried between boxes: see the package doc's
// section of that name. A box that holds the session is compared by id and by
// the archive's digest. Its copy is never written over with an archive it
// already holds (packed or restored there), because that copy is the
// archive's or newer: a later run there may have failed its capture. A copy
// that does not hold the archive went no further than an earlier point, which
// the archive's box carried on from, so the archive replaces it. The box keeps
// the `cksum` (POSIX's) of every archive its copy holds
// ([Paths.SessionDigests]), written by the script that packed or restored it;
// a digest the box cannot take keeps the box's copy, which loses nothing.

// harnessVersionWait bounds [Runner.CheckCarry]'s one read of the box
// harness's release: a login shell and `--version`, about a second, mostly
// the runtime starting. Sixty seconds is room for a contended disk or CPU; a
// box silent past it is not answering, and the caller starts the run fresh
// rather than wait.
const harnessVersionWait = 60 * time.Second

// sessionRecord is what a run's script recorded of its session before its harness
// started ([Paths.SessionRecord]).
type sessionRecord struct {
	// id is the session the harness ran in, in the harness's own form; "" where the
	// record holds none this build reads.
	id string
	// continued says it is the session the run was asked to continue.
	continued bool
	// began says the record's `continued` line reads `no`: the run started a
	// session of its own, so the whole session is the run's
	// ([sandbox.RunStart]). A missing or other line is not known to have begun.
	began bool
	// from is where a run that continued its session began in it, as the
	// record holds it, raw: an OpenCode run's cursor ([runStartOf]), a Gemini
	// CLI or Qwen Code run's line count and digest ([chatRunStartOf]),
	// decoded by the harness the record is read for ([Runner.archiveAt]); ""
	// where none is recorded, it is past [recordFromMax], or the run did not
	// continue.
	from string
	// why is why a run asked to continue a session started a new one
	// ([sandbox.Result.FellBack]), as the box holds it: bounded by its one
	// reader, where the result is built ([Runner.Collect]). "" for other runs.
	why string
	// unshadowed says the record's `unshadowed` line reads exactly `yes`: the
	// run's box found no other MCP server that could spell the bridge's tools
	// ([shadowGuarded]). Any other line, or none, is not known to be so.
	unshadowed bool
	// release is the harness's `--version`, as it printed it.
	release string
}

// parseSessionRecord reads a run's record of its session, a line each:
// `id`, `continued <yes|no>`, `why` (why it started anew, one line, else
// empty), `release` (what --version printed), and before it `unshadowed yes`
// where a guarded run's box passed its guard and, for a run that continued,
// `from`, kept raw within [recordFromMax] for its harness's shape
// ([sessionRecord.runStart]). The first line of each key is the record's: the
// release, written last, may run on into the harness's own lines, where an
// `id` or a `from` is not the script's. An id no harness mints
// ([sandbox.ValidSessionID]) is no id; a `continued` of neither is a start not
// known. Walked a line at a time to the fourth key, never split: the record is
// agent-writable up to [sandbox.MaxFileBytes] and read on every collection and
// take (TestASessionRecordIsReadWhereItLies).
func parseSessionRecord(text string) sessionRecord {
	var (
		rec                                                                   sessionRecord
		readID, readContinued, readWhy, readFrom, readUnshadowed, readRelease bool
		from                                                                  string
	)
	for text != "" && (!readID || !readContinued || !readWhy || !readRelease) {
		var line string
		line, text, _ = strings.Cut(text, "\n")
		key, value, _ := strings.Cut(line, " ")
		value = strings.TrimSpace(value)
		switch {
		case key == "id" && !readID:
			readID = true
			if sandbox.ValidSessionID(value) {
				rec.id = value
			}
		case key == "continued" && !readContinued:
			readContinued = true
			rec.continued = value == "yes"
			rec.began = value == "no"
		case key == "why" && !readWhy:
			readWhy = true
			rec.why = value
		case key == "from" && !readFrom && !readRelease:
			readFrom = true
			from = value
		case key == "unshadowed" && !readUnshadowed && !readRelease:
			readUnshadowed = true
			rec.unshadowed = line == "unshadowed yes"
		case key == "release" && !readRelease:
			readRelease = true
			rec.release = value
		}
	}
	if rec.continued && len(from) <= recordFromMax {
		rec.from = from
	}
	return rec
}

// recordFromMax bounds the `from` a record keeps raw ([sessionRecord.from]):
// the longer of an OpenCode cursor's ([runStartCursorMax]) and a chat file's
// lines and digest ([chatRunStartMax]). A longer one is no start of either.
var recordFromMax = max(runStartCursorMax, chatRunStartMax)

// runStart is where the run rec records began in its session, read by the
// shape its harness writes ([lineStarter]): a chat file's lines and digest
// beside nothing else, or an OpenCode cursor's message; neither where the
// record holds no start of that shape.
func (rec sessionRecord) runStart(h Harness) (sandbox.RunStart, sandbox.LinePrefix) {
	start := sandbox.RunStart{Began: rec.began}
	if _, lines := h.(lineStarter); lines {
		return start, chatRunStartOf(rec.from)
	}
	start.After = runStartOf(rec.from)
	return start, sandbox.LinePrefix{}
}

// unshadowedBy reports whether rec says its run's box passed h's guard: only
// a guarded harness's record is read so ([shadowGuarded]).
func (rec sessionRecord) unshadowedBy(h Harness) bool {
	_, guarded := h.(shadowGuarded)
	return guarded && rec.unshadowed
}

// shadowGuarded is a harness whose run's script checks, before its harness
// starts, that no MCP server its box could load spells a tool as the harness
// spells the bridge's, and records `unshadowed yes` where none does (Gemini
// CLI's, [geminiScript]); only its archives are described so
// ([sandbox.SessionArchive.Unshadowed]).
type shadowGuarded interface{ guardsShadows() }

// lineStarter is a harness whose run records where it began in its chat
// file as a line count and the digest of those lines (Gemini CLI, Qwen Code;
// [chatRunStartFrom]), never a message id.
type lineStarter interface{ startsByLines() }

// chatRunStartMax is the longest `from` [chatRunStartOf] reads: three
// decimals, a line count to [sandbox.MaxRunStartLines], a CRC to a uint32's
// and a length to an int64's, and the two spaces between them.
var chatRunStartMax = len(strconv.Itoa(sandbox.MaxRunStartLines)) + len(strconv.FormatUint(math.MaxUint32, 10)) +
	len(strconv.FormatInt(math.MaxInt64, 10)) + 2

// chatRunStartFrom is the shell, run once a continued session's file "$cf"
// is gated, cut and torn back ([chatSessionGate]), setting n to its line count
// (0 where there is none) and from to `<lines> <crc> <bytes>`, the count and
// the POSIX cksum of exactly those lines, "" where the run did not continue,
// the file holds no whole line or the digest was not taken.
const chatRunStartFrom = `n=0 from=; if [ -n "$cf" ]; then n=$(wc -l < "$cf" | tr -d ' ') || n=0; ` +
	`if [ "$k" = yes ] && [ "$n" -gt 0 ] 2> /dev/null && c=$(head -n "$n" "$cf" | cksum); then from="$n $c"; fi; fi`

// chatRecordFrom is the record's `from` line, written only where the run
// recorded one ([chatRunStartFrom]), between `why` and `release`.
const chatRecordFrom = `[ -z "$from" ] || printf 'from %s\n' "$from"`

// chatRunStartOf is the prefix a Gemini CLI or Qwen Code run's `from` names
// ([chatRunStartFrom]): exactly `<lines> <crc> <bytes>` in decimal, the lines
// within [sandbox.MaxRunStartLines] and above zero, the CRC a uint32, the
// bytes at least the lines (each line holds its break); zero for anything
// else, so a cursor is never lines, nor lines a cursor.
func chatRunStartOf(from string) sandbox.LinePrefix {
	fields := strings.Split(from, " ")
	if len(from) > chatRunStartMax || len(fields) != 3 {
		return sandbox.LinePrefix{}
	}
	var n [3]uint64
	for i, f := range fields {
		if f == "" || f[0] == '+' || (len(f) > 1 && f[0] == '0') {
			return sandbox.LinePrefix{}
		}
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return sandbox.LinePrefix{}
		}
		n[i] = v
	}
	lines, crc, bytes := n[0], n[1], n[2]
	if lines == 0 || lines > sandbox.MaxRunStartLines || crc > math.MaxUint32 || bytes > math.MaxInt64 ||
		bytes < lines {
		return sandbox.LinePrefix{}
	}
	return sandbox.LinePrefix{Lines: int64(lines), CRC: uint32(crc), Bytes: int64(bytes)}
}

// runStartCursorMax is the longest cursor [runStartOf] reads: the unpadded
// base64url of a cursor naming a message whose id is at its bound
// ([sandbox.MaxMessageIDBytes]), with the longer of the two directions a cursor
// names. A longer one names no message this build keeps, and is never decoded.
var runStartCursorMax = base64.RawURLEncoding.EncodedLen(
	len(`{"id":"","order":"desc","direction":"previous"}`) + sandbox.MaxMessageIDBytes)

// errNoRunStart is a cursor that names no start [runStartOf] reads.
var errNoRunStart = errors.New("executor: no start")

// runStartOf is the id of the message cursor names, where cursor is a `next`
// cursor of a session's message list asked newest first: what an OpenCode
// run's script records as where the run began ([openCodeRunStart]); "" for
// anything else. OpenCode's cursor is the unpadded base64url of
// `JSON.stringify({id, order, direction})` (read in OpenCode 2's bundle),
// read whole or not at all: held to [runStartCursorMax], decoded strictly and
// walked as one object ([eachKey]) of exactly those keys, each once, each a
// string. Order must be `desc` and direction `next`: any other names the
// session's OLDEST message, reading the whole session as the run's. The id
// must be one a box may name ([sandbox.ValidMessageID]), whole: two ids cut
// alike would name one message.
func runStartOf(cursor string) string {
	if cursor == "" || len(cursor) > runStartCursorMax {
		return ""
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(cursor)
	if err != nil {
		return ""
	}
	var id, order, direction string
	var seen [3]bool
	err = eachKey(raw, func(name []byte, value json.RawMessage, _ int) error {
		var into *string
		var at int
		switch string(name) {
		case "id":
			into, at = &id, 0
		case "order":
			into, at = &order, 1
		case "direction":
			into, at = &direction, 2
		default:
			return errNoRunStart
		}
		if seen[at] {
			return errNoRunStart
		}
		seen[at] = true
		return wireDecode(value, into)
	})
	// A KEY MISSING is one of these refusals too: no order is not `desc`, no
	// direction not `next`, and no id no message id.
	if err != nil || order != "desc" || direction != "next" || !sandbox.ValidMessageID(id) {
		return ""
	}
	return id
}

// releaseOf is the release a harness's `--version` printed, "2.1.310", or ""
// where it printed none this code can read: a bare MAJOR.MINOR.PATCH alone
// ([harnessprofile.Release]), so a pre-release names none.
//
// What it reads was printed in the box (the record is agent-writable,
// `--version` whatever PATH finds), so a file-long run of digits would read
// as a release. So it is held to a release's shape ([sandbox.ValidRelease]),
// as the launch's record holds it: past the executor log's bound it is
// refused, never cut, since a shortened release names another; read as none,
// it continues no Codex session, and Claude Code's and OpenCode's readers
// each take it as their rules say.
func releaseOf(printed string) string {
	if release := harnessprofile.Release(printed); sandbox.ValidRelease(release) {
		return release
	}
	return ""
}

// bareReleaseShell is the script's side of [releaseOf]: it sets the shell
// variable name to the bare MAJOR.MINOR.PATCH the `--version` output in v
// names, else "", so a box's gate and the engine's check read one release.
// Globbing is off while v is split: it is the box's own words.
func bareReleaseShell(name string) string {
	return fmt.Sprintf(`%[1]s=; set -f; for rw in $v; do rw=${rw#v}; rw=${rw%%.}; case "$rw" in [0-9]*) %[1]s=$rw; break;; esac; done; set +f
case "$%[1]s" in *[!0-9.]*|.*|*.|*..*|*.*.*.*) %[1]s= ;; *.*.*) ;; *) %[1]s= ;; esac
`, name)
}

// captured describes the session archive the box's last run packed
// ([Paths.SessionCapture]), for the session rec names, or nil where there is
// none ([Runner.archiveAt]).
func (r *Runner) captured(ctx context.Context, box sandbox.Sandbox, paths Paths,
	rec sessionRecord) (*sandbox.SessionArchive, error) {
	return r.archiveAt(ctx, box, paths.SessionCapture(), rec)
}

// archiveAt describes the session archive the box holds at path (the run's
// capture [Paths.SessionCapture], a pack on demand's [Paths.SessionPackHeld]
// or a save [Paths.SessionSave]) for the session rec names, or nil where there
// is none: a harness that carries no session, no archive, a record naming no
// session, or a path that is no regular file, which costs only the capture.
func (r *Runner) archiveAt(ctx context.Context, box sandbox.Sandbox, path string,
	rec sessionRecord) (*sandbox.SessionArchive, error) {

	if _, ok := r.harness.(carrier); !ok || rec.id == "" {
		return nil, nil
	}
	end, err := box.ReadTail(ctx, path, 0)
	if errors.Is(err, sandbox.ErrNotRegularFile) {
		log.WarnContext(ctx, "harness_capture_unread", "harness", r.harness.Name(), "session", rec.id,
			"detail", "the path the run's session is packed to holds something other than a file, "+
				"so the session was not captured", "error", err.Error())
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if end.Size == 0 {
		return nil, nil
	}
	archive := r.recorded(rec)
	archive.Size, archive.Path, archive.Unshadowed = end.Size, path, rec.unshadowedBy(r.harness)
	archive.RunStart, archive.Prefix = rec.runStart(r.harness)
	return &archive, nil
}

// OpenCapture streams the session archive the box's last run packed when its
// harness exited, with its description ([sandbox.SessionArchive]) read before
// its bytes so a reader can refuse one too large to keep, for the collection
// that carries it to another box. A nil reader and a zero description where
// the box holds none ([sandbox.Result.Captured] says why); the caller closes
// the reader.
//
// Streamed, never read whole: an archive grows with its session. The run's
// script moves it into place whole, once, as its harness exits, and no pack on
// demand moves anything there ([Runner.PackCapture]), so a take's second pass
// ([scrubTar]) reads what its first read; an agent rewriting it since is the
// box's doing, which the restorer's checks meet.
func (r *Runner) OpenCapture(ctx context.Context, box sandbox.Sandbox) (sandbox.SessionArchive, io.ReadCloser, error) {
	return r.openCapture(ctx, box, "")
}

// openCapture is [Runner.OpenCapture] of a minting harness's job bound to
// bound ([sandbox.SessionTake.Session]), which OpenCapture names none of.
func (r *Runner) openCapture(ctx context.Context, box sandbox.Sandbox, bound string) (sandbox.SessionArchive,
	io.ReadCloser, error) {

	paths := PathsFor(box)
	rec, refusal, err := r.readSessionRecord(ctx, box, paths, bound)
	if err != nil {
		return sandbox.SessionArchive{}, nil, fmt.Errorf("executor: reading which session the run packed: %w", err)
	}
	if refusal != "" {
		return sandbox.SessionArchive{}, nil, nil
	}
	described, err := r.captured(ctx, box, paths, rec)
	if err != nil || described == nil {
		return sandbox.SessionArchive{}, nil, err
	}
	body, err := box.OpenFile(ctx, described.Path)
	if err != nil {
		return sandbox.SessionArchive{}, nil, fmt.Errorf("executor: opening the session the run packed: %w", err)
	}
	return *described, body, nil
}

// CheckCarry answers whether archive, packed in ANOTHER box, can be continued
// in box, asked before the run starts there and before its brief is chosen
// (briefs are written before the box is acquired): the box harness's release
// ([Runner.boxRelease]), held against the writer, the archive's format and
// the harness's floors ([carrier.Continues]).
// The release read is returned either way, with whether its resume was read
// ([claudeResumeRead], [sandbox.UnheldTelling]): a brief told by a reading
// goes only to the writer's release ([sandbox.ExactTelling]).
//
// Three answers, as a lease's: it can, it cannot (and why), and an error where
// the box could not be asked, never read as either.
func (r *Runner) CheckCarry(ctx context.Context, box sandbox.Sandbox, archive sandbox.SessionArchive) (sandbox.CarryCheck, error) {
	carries, ok := r.harness.(carrier)
	if !ok {
		return sandbox.CarryCheck{Loss: executorkeep.LossHarnessChanged,
			Why: r.harness.Name() + " carries no session between boxes"}, nil
	}
	release, silent, err := r.boxRelease(ctx, box, carries)
	if err != nil || silent != "" {
		return sandbox.CarryCheck{Why: silent}, err
	}
	check := sandbox.CarryCheck{Version: release}
	check.ResumeRead = r.resumeRead(check.Version)
	if restores := r.restores(archive); !restores.Continues {
		check.Loss, check.Why = restores.Loss, restores.Why
		return check, nil
	}
	check.Continues, check.Why = carries.Continues(check.Version, archive.Version)
	return check, nil
}

// inPlaceGate is a [carrier] whose box continues no session, not even the
// copy it holds, on a release it was not read for: Codex, Kimi Code, Copilot,
// Muse Code, Hermes Agent and Pi, whose one supported release alone continues
// a session ([Codex.continuesInPlace], [Kimi.continuesInPlace],
// [Copilot.continuesInPlace], [Muse.continuesInPlace], [Hermes.continuesInPlace],
// [Pi.continuesInPlace]).
type inPlaceGate interface {
	carrier
	continuesInPlace(box string) (bool, string)
}

// CheckInPlace answers whether box's harness continues the copy of a session
// the box itself holds ([sandbox.SessionRestorer]), asked before a relaunch
// in the box chooses the brief that continues it: yes without asking for a
// harness that holds no release to it, else its release read as
// [Runner.CheckCarry] reads it and held to its harness's rule
// ([inPlaceGate]), and then the release that wrote the copy held to the
// writer's ([carrier.ContinuesWriter]), so a copy a newer release wrote is
// not continued by a box downgraded since. Three answers, as CheckCarry's.
func (r *Runner) CheckInPlace(ctx context.Context, box sandbox.Sandbox) (sandbox.CarryCheck, error) {
	gate, ok := r.harness.(inPlaceGate)
	if !ok {
		return sandbox.CarryCheck{Continues: true}, nil
	}
	release, silent, err := r.boxRelease(ctx, box, gate)
	if err != nil || silent != "" {
		return sandbox.CarryCheck{Why: silent}, err
	}
	check := sandbox.CarryCheck{Version: release, ResumeRead: r.resumeRead(release)}
	if check.Continues, check.Why = gate.continuesInPlace(release); !check.Continues {
		return check, nil
	}
	writer, unknown, err := copyWriter(ctx, box)
	switch {
	case err != nil:
		return sandbox.CarryCheck{}, err
	case unknown != "":
		check.Continues, check.Why = false, unknown
		return check, nil
	}
	check.Continues, check.Why = gate.ContinuesWriter(writer)
	return check, nil
}

// copyWriter is the release that wrote the copy of a session box holds, as
// [releaseOf] reads it: the release the record of the box's last run names
// ([Paths.SessionRecord]), the run whose session a relaunch in the box is
// offered, written before its harness ran. Why it is not known where the box
// holds no record (a box reads a missing file as empty) or one it refuses;
// an error where the box could not be asked.
func copyWriter(ctx context.Context, box sandbox.Sandbox) (writer, unknown string, err error) {
	recorded, refusal, err := readWhole(ctx, box, PathsFor(box).SessionRecord(),
		"the record of the session its harness ran in")
	switch {
	case err != nil:
		return "", "", fmt.Errorf("executor: reading the record of the box's last run: %w", err)
	case refusal != "":
		return "", copyWriterUnknown + refusal, nil
	case strings.TrimSpace(recorded) == "":
		return "", copyWriterUnknown + "its last run left no record", nil
	}
	return releaseOf(parseSessionRecord(recorded).release), "", nil
}

// copyWriterUnknown opens why a box continues no copy whose writer it cannot
// name ([copyWriter]).
const copyWriterUnknown = "the box cannot say which release wrote its copy of the session: "

// boxRelease is the release box's harness says, read by one login-shell exec
// of its `--version` with the box's bin directory first on PATH as the
// wrapper puts it ([withBoxBin]), as [releaseOf] reads it; why it said none
// where the command failed; and an error where the box could not be asked.
func (r *Runner) boxRelease(ctx context.Context, box sandbox.Sandbox, carries carrier) (release, silent string, err error) {
	ask := "PATH=" + shellword.Quote(PathsFor(box).BinDir()) + `:"$PATH"; export PATH; exec ` +
		carries.VersionCommand() + " < /dev/null"
	res, err := box.Exec(ctx, "sh -lc "+shellword.Quote(ask), sandbox.ExecOptions{TimeoutSec: harnessVersionWait.Seconds()})
	if err != nil {
		return "", "", fmt.Errorf("executor: asking the box's %s for its release: %w", r.harness.Name(), err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Sprintf("the box's %s did not say its release: `%s` exited %d: %s", r.harness.Name(),
			carries.VersionCommand(), res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit)), nil
	}
	return releaseOf(res.Stdout), "", nil
}

// resumeReader is a [carrier] whose releases' resumes this build read.
type resumeReader interface{ resumeRead(release string) bool }

// resumeRead reports whether release, of this runner's harness, is one whose
// resume this build read: what a box's carry check
// ([sandbox.CarryCheck.ResumeRead]) and a take's archive
// ([sandbox.TakenSession.ResumeRead]) each report. False for a harness that
// is no [resumeReader].
func (r *Runner) resumeRead(release string) bool {
	h, ok := r.harness.(resumeReader)
	return ok && h.resumeRead(release)
}

// RestoresUnder reports whether this runner restores archive for a launch
// under entry ([sandbox.SessionRestorer]): [Runner.restores], and an entry
// [sandbox.EntryContinues] lets it continue under, by this harness's measured
// trait ([Runner.ResumesOnLaunchRoute]). The one answer a launch and every
// reader of whether a session can be continued take ([sandbox.RestorerUnder]),
// so none restates the floors.
func (r *Runner) RestoresUnder(archive sandbox.SessionArchive, entry string) sandbox.SessionContinuity {
	if restores := r.restores(archive); !restores.Continues {
		return restores
	}
	return sandbox.EntryContinues(archive.Entry, entry, r.harness.Name(), r.ResumesOnLaunchRoute())
}

// ResumesOnLaunchRoute reports whether this runner's harness was measured to
// send a resumed session's calls on its launch's route ([launchRouted]).
func (r *Runner) ResumesOnLaunchRoute() bool { return ResumesOnLaunchRoute(r.harness.Name()) }

// restores reports whether this runner restores archive at all, whatever
// entry a launch runs under: written by its own harness, in the one format it
// packs, by a release whose sessions another box continues — everything
// [Runner.CheckCarry] decides but the box's own release, which only the box can
// say.
//
// Each refusal is the harness that changed or the writer's release or format
// ([executorkeep.LossHarnessChanged], [executorkeep.LossReleaseFloor]).
func (r *Runner) restores(archive sandbox.SessionArchive) sandbox.SessionContinuity {
	carries, ok := r.harness.(carrier)
	switch {
	case archive.Harness != r.harness.Name():
		return sandbox.SessionContinuity{Loss: executorkeep.LossHarnessChanged, Why: fmt.Sprintf("the session "+
			"was written by %q, and this runner drives %s: a session is continued only by the harness that "+
			"wrote it", archive.Harness, r.harness.Name())}
	case !ok:
		return sandbox.SessionContinuity{Loss: executorkeep.LossReleaseFloor,
			Why: r.harness.Name() + " carries no session between boxes"}
	case archive.Format != carries.ArchiveFormat():
		return sandbox.SessionContinuity{Loss: executorkeep.LossReleaseFloor, Why: fmt.Sprintf("the session's "+
			"archive is format %d, and this build restores %s's format %d alone", archive.Format,
			r.harness.Name(), carries.ArchiveFormat())}
	}
	if continues, why := carries.ContinuesWriter(archive.Version); !continues {
		return sandbox.SessionContinuity{Loss: executorkeep.LossReleaseFloor, Why: why}
	}
	return sandbox.SessionContinuity{Continues: true}
}

// unrestorable is a take's refusal of archive where no box of this build
// continues it ([Runner.restores]), nil where one would: asked as soon as a
// take or a save knows the release that wrote the session, before any of it is
// packed or read ([sandbox.UnrestorableError]).
func (r *Runner) unrestorable(archive sandbox.SessionArchive) error {
	if restores := r.restores(archive); !restores.Continues {
		return &sandbox.UnrestorableError{Loss: restores.Loss, Why: restores.Why}
	}
	return nil
}

// recorded is the archive of the session rec names as its record alone says
// it, before any of it is packed: [Runner.archiveAt] adds the rest.
func (r *Runner) recorded(rec sessionRecord) sandbox.SessionArchive {
	archive := sandbox.SessionArchive{Harness: r.harness.Name(), SessionID: rec.id,
		Version: releaseOf(rec.release)}
	if carries, ok := r.harness.(carrier); ok {
		archive.Format = carries.ArchiveFormat()
	}
	return archive
}

var _ sandbox.SessionRestorer = (*Runner)(nil)

// boxNamedNoRelease is why a box whose harness's `--version` printed no
// release this build reads continues no carried session, for every harness:
// a release no rule here was read for is never taken for a current one.
func boxNamedNoRelease(harness string) string {
	return fmt.Sprintf("this box's %s named no release this build reads, so it continues no session "+
		"carried from another box", harness)
}

// releaseReads reports whether version, as a harness's `--version` printed it,
// is a release this code reads ([releaseNumbers]).
func releaseReads(version string) bool {
	_, ok := releaseNumbers(version)
	return ok
}

// writerNamedNoRelease is why a session whose writer named no release this
// build reads ([releaseOf]: a pre-release, a snapshot, none at all) continues
// in no box, for every harness: a session no rule here was read for is never
// taken for a current release's, which a box older than its writer would then
// continue.
func writerNamedNoRelease(harness string) string {
	return fmt.Sprintf("the session was written by a %s that named no release this build reads, so no "+
		"box continues it", harness)
}

// carriedOlder is the reason a box's release cannot continue a session a newer
// one wrote, shared by both harnesses: a release reads what it and those before it
// wrote, never what a later one adds to a session.
func carriedOlder(harness, box, writer string) string {
	return fmt.Sprintf("this box runs %s %s, older than the %s that wrote the session", harness, box, writer)
}
