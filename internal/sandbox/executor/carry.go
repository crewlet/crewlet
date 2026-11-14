package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A run's harness session, carried between boxes.
//
// EVERY RUN NAMES ITS SESSION BEFORE ITS HARNESS STARTS ([Harness.Session]): the one
// it continues, or a new one minted for it, so the run's record names it
// whether or not the run lives to report it ([sandbox.RunHandle.SessionID]).
// The run's script records, before the harness starts, the session it took up,
// whether that is the one it was asked to continue — and, where it is not, why
// — and the harness's release ([Paths.SessionRecord]); and once the harness has
// exited, it packs the session into one archive under the box's work directory
// ([Paths.SessionCapture]), which [Runner.OpenCapture] streams for a
// collection to keep. A run handed a session packed in another box
// ([sandbox.RunRequest.Carried]) has the archive written into its box as it
// starts ([Paths.SessionCarried]) and restored by its script before its harness
// takes the session up. What each harness's archive holds, and what it never
// holds, is the harness's own: see [ClaudeCode] and [OpenCode].
//
// A BOX THAT HOLDS THE SESSION IS COMPARED BY ID AND BY THE ARCHIVE'S DIGEST.
// Its copy is never written over with an archive it already holds — one the
// box packed itself, or restored before — because that copy is the archive's
// or newer: a run went on in this box after it, whose own capture may have
// failed or been refused. A copy that does not hold the archive went no
// further than an earlier point of the session, which the archive's own box
// carried on from, so the archive replaces it. The box keeps the digest of
// every archive of a session its copy holds ([Paths.SessionDigests]): the
// archive's `cksum`, POSIX's own, written by the script that packed or
// restored it. A digest the box cannot take keeps the box's copy, which loses
// nothing a run in the box did.
//
// WHETHER A BOX CAN CONTINUE A CARRIED SESSION is asked of the box before the
// run's brief is written ([Runner.CheckCarry]): its harness's release, read in the
// box, against the release that wrote the session and the floors each harness
// sets ([carrier.Continues]).

// harnessVersionWait bounds [Runner.CheckCarry]'s one read of the box harness's
// release: a login shell sourcing the image's profile and the harness printing its
// version, which takes about a second (OpenCode's runtime starting is most of
// the second its server takes to listen, measured on 2.0.32 —
// [openCodeListenWait]). Sixty seconds is the room that wait gives a box whose
// disk or CPU is contended; a box that has not answered by then is not
// answering, and the caller starts the run fresh rather than wait on it.
const harnessVersionWait = openCodeListenWait

// sessionRecord is what a run's script recorded of its session before its harness
// started ([Paths.SessionRecord]).
type sessionRecord struct {
	// id is the session the harness ran in, in the harness's own form; "" where the
	// record holds none this build reads.
	id string
	// continued says it is the session the run was asked to continue.
	continued bool
	// began says the run started a session of its own — the record's
	// `continued` line reads `no` — so the whole session is the run's
	// ([sandbox.RunStart]). False for a record whose line is missing or reads
	// anything else: such a record is not known to have begun its session.
	began bool
	// from is where a run that continued its session began in it: the newest
	// message the session held before the run, its id decoded off the cursor
	// the run's script recorded (`from`, [runStartOf]) — "" where the record
	// names none this build reads, and for a run that did not continue its
	// session.
	from string
	// why is why a run asked to continue a session started a new one in its
	// place, as its script said it ([sandbox.Result.FellBack]), the line as
	// the box holds it: held to its bound where the run's result is built
	// ([Runner.Collect]), the one reader of it. "" for every other run.
	why string
	// release is the harness's `--version`, as it printed it.
	release string
}

// parseSessionRecord reads a run's record of its session: `id <id>`,
// `continued <yes|no>`, `why <why it started anew>` and `release <what
// --version printed>`, a line each, `why` empty for a run that did not — and,
// for an OpenCode run that continued its session, `from <cursor>` before the
// release, where the run began in the session ([openCodeRunStart]).
//
// THE FIRST LINE OF EACH KEY IS THE RECORD'S. The script writes the release
// last, as the harness printed it, and a harness that printed more than one line runs
// on into lines of its own — an `id` there is the harness's text, never the
// script's, and so is a `from`, which is read only where it lies before the
// release. The script writes `why` on one line, whatever the words it
// quotes held. An id that is not one a harness mints
// ([sandbox.ValidSessionID]) is no id. `from` is read only where `continued`
// reads `yes`, and only whole ([runStartOf]); `continued` reading `no` is a
// run that began its session, and anything else, or no such line, says
// neither: a start not known, never the whole session.
//
// READ WHERE IT LIES, AND NO FURTHER THAN ITS FOUR KEYS. The record is a file
// the run's own agent can write, as long as the 32 MiB the engine reads whole
// ([sandbox.MaxFileBytes]), and it is read on every collection and every take
// of the box's session. Split into a slice of its lines, with a set of every
// key it named, a record of empty lines cost sixteen times its size, half a
// GiB, on each of those reads; walked a line at a time it costs nothing past
// its own bytes, and the walk ends once the four keys are read, since no later
// line of one is the record's.
//
// AND NOTHING IN IT IS HELD HERE: the take reads the id alone, so `why` is
// held to its bound by the one reader of it, where the run's result is built
// ([Runner.Collect], [sandbox.BoundFellBack]), rather than redacted on every
// read of the record and thrown away by all but one.
func parseSessionRecord(text string) sessionRecord {
	var (
		rec                                                   sessionRecord
		readID, readContinued, readWhy, readFrom, readRelease bool
		from                                                  string
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
		case key == "release" && !readRelease:
			readRelease = true
			rec.release = value
		}
	}
	if rec.continued {
		rec.from = runStartOf(from)
	}
	return rec
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
// run's script records as where the run began ([openCodeRunStart]). "" for
// anything else.
//
// READ WHOLE OR NOT AT ALL, each step refusing what fails. OpenCode's cursor is
// the unpadded base64url of `JSON.stringify({id, order, direction})` of the
// page's message (read in the 2.0.32, 2.1.4 and 2.2.7 bundles; measured on
// 2.0.32 and 2.2.7, testdata/opencode/<version>/context): held to
// [runStartCursorMax] before it is decoded, decoded strictly, then walked as
// one object ([eachKey]) whose keys are `id`, `order` and `direction` and no
// other, each once, each a string. Its order must be `desc` and its direction
// `next`: a cursor of another order or direction names the session's OLDEST
// message, from which the whole session would read as the run's. And its id
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

// releasePattern is a release's MAJOR.MINOR.PATCH, wherever a harness's
// `--version` prints it: first for Claude Code (`2.1.310 (Claude Code)`),
// after a name for OpenCode (`opencode v2.0.32`).
var releasePattern = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)

// releaseOf is the release a harness's `--version` printed, "2.1.310", or "" where
// it printed none this code can read — one longer than the executor log
// records a release at among them.
//
// A RELEASE HAS A LENGTH, AND THE PATTERN DOES NOT: its numbers are runs of
// digits, and what it reads was printed in the box — the session record's
// `release` line is a file the run's own agent can write, and `--version` is
// whatever the box's PATH finds — so a run of digits as long as the file read
// as a release, which the launch's record carried whole and a capture of the
// session named, for the log to refuse. So what the pattern finds is held to
// a release's shape ([sandbox.ValidRelease]), the one the launch's record
// holds it to again: past the log's bound it is refused, never cut, because a
// shortened release names another one; and read as none, it is what every
// reader takes a release it cannot read for: a current one.
func releaseOf(printed string) string {
	if release := releasePattern.FindString(printed); sandbox.ValidRelease(release) {
		return release
	}
	return ""
}

// captured describes the session archive the box's last run packed
// ([Paths.SessionCapture]), for the session rec names, or nil where there is
// none ([Runner.archiveAt]).
func (r *Runner) captured(ctx context.Context, box sandbox.Sandbox, paths Paths,
	rec sessionRecord) (*sandbox.SessionArchive, error) {
	return r.archiveAt(ctx, box, paths.SessionCapture(), rec)
}

// archiveAt describes the session archive the box holds at path — the run's
// capture ([Paths.SessionCapture]), a pack on demand's ([Paths.SessionPackHeld])
// or a save ([Paths.SessionSave]) —
// for the session rec names, or nil where there is none: a harness that
// carries no session, no archive there, a record that names no session, or a
// path that holds no regular file — the box's own doing, which costs the
// capture and nothing else.
func (r *Runner) archiveAt(ctx context.Context, box sandbox.Sandbox, path string,
	rec sessionRecord) (*sandbox.SessionArchive, error) {

	carries, ok := r.harness.(carrier)
	if !ok || rec.id == "" {
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
	return &sandbox.SessionArchive{
		Harness: r.harness.Name(), SessionID: rec.id, Format: carries.ArchiveFormat(),
		Version: releaseOf(rec.release), Size: end.Size, Path: path,
		RunStart: sandbox.RunStart{Began: rec.began, After: rec.from},
	}, nil
}

// OpenCapture streams the session archive the box's last run packed when its
// harness exited, described ([sandbox.SessionArchive]) — its harness, its session, its
// format, the release that wrote it and its size, read before its bytes so a
// reader can refuse one too large to keep — for the collection that carries it
// to another box. A nil reader, and a zero description, where the box holds
// none ([sandbox.Result.Captured] says why there may be none); the caller
// closes the reader.
//
// READ AS A STREAM, never whole: an archive grows with its session. The size
// is the archive's as it was described, and the archive is moved into place
// whole by the run that packed it, so the stream is one whole archive; a box
// whose own agent rewrote the file since is the box's doing, which the
// restorer's checks meet.
//
// AND NOTHING MOVES OVER IT WHILE IT IS READ, so a take that opens it again
// for a second pass ([scrubTar]) reads the archive its first pass read: the
// run's script moves it into place once, as its harness exits, and no pack the
// engine makes on demand moves anything there ([Runner.PackCapture]).
func (r *Runner) OpenCapture(ctx context.Context, box sandbox.Sandbox) (sandbox.SessionArchive, io.ReadCloser, error) {
	paths := PathsFor(box)
	rec, refusal, err := r.readSessionRecord(ctx, box, paths)
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

// CheckCarry answers whether archive — a session packed in ANOTHER box — can
// be continued in box, asked of the box before the run is started there and
// before its brief is written: the box's harness's release, read in the box with
// one login-shell exec of its `--version`, the box's own bin directory first
// on its PATH as the run's wrapper puts it ([withBoxBin]), held against the
// harness that wrote the session, the archive's format and the harness's own floors
// ([carrier.Continues]). The release it read is returned either way, for the
// run's record.
//
// THREE ANSWERS, as a lease's are: it can, it cannot (and why), and the box
// could not be asked — an error, never read as either.
func (r *Runner) CheckCarry(ctx context.Context, box sandbox.Sandbox, archive sandbox.SessionArchive) (sandbox.CarryCheck, error) {
	carries, ok := r.harness.(carrier)
	if !ok {
		return sandbox.CarryCheck{Why: r.harness.Name() + " carries no session between boxes"}, nil
	}
	ask := "PATH=" + sandbox.ShellQuote(PathsFor(box).BinDir()) + `:"$PATH"; export PATH; exec ` +
		carries.VersionCommand() + " < /dev/null"
	res, err := box.Exec(ctx, "sh -lc "+sandbox.ShellQuote(ask), sandbox.ExecOptions{TimeoutSec: harnessVersionWait.Seconds()})
	if err != nil {
		return sandbox.CarryCheck{}, fmt.Errorf("executor: asking the box's %s for its release: %w",
			r.harness.Name(), err)
	}
	if res.ExitCode != 0 {
		return sandbox.CarryCheck{Why: fmt.Sprintf("the box's %s did not say its release: `%s` exited %d: %s",
			r.harness.Name(), carries.VersionCommand(), res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))}, nil
	}
	check := sandbox.CarryCheck{Version: releaseOf(res.Stdout)}
	if restores, why := r.Restores(archive); !restores {
		check.Why = why
		return check, nil
	}
	check.Continues, check.Why = carries.Continues(check.Version, archive.Version)
	return check, nil
}

// Restores reports whether this runner restores archive at all
// ([sandbox.SessionRestorer]): written by its own harness, in the one format it
// packs, by a release whose sessions another box continues — everything
// [Runner.CheckCarry] decides but the box's own release, which only the box can
// say. The one answer a launch and every reader of whether a session can be
// continued take ([sandbox.ContinuityOf]), so neither restates the floors.
func (r *Runner) Restores(archive sandbox.SessionArchive) (bool, string) {
	carries, ok := r.harness.(carrier)
	switch {
	case !ok:
		return false, r.harness.Name() + " carries no session between boxes"
	case archive.Harness != r.harness.Name():
		return false, fmt.Sprintf("the session was written by %q, and this runner drives %s: a session "+
			"is continued only by the harness that wrote it", archive.Harness, r.harness.Name())
	case archive.Format != carries.ArchiveFormat():
		return false, fmt.Sprintf("the session's archive is format %d, and this build restores %s's "+
			"format %d alone", archive.Format, r.harness.Name(), carries.ArchiveFormat())
	}
	// THE WRITER'S FLOOR ALONE: a box release that cannot be read is taken
	// for a current one, so an empty one asks nothing of the box.
	return carries.Continues("", archive.Version)
}

var _ sandbox.SessionRestorer = (*Runner)(nil)

// carriedOlder is the reason a box's release cannot continue a session a newer
// one wrote, shared by both harnesses: a release reads what it and those before it
// wrote, never what a later one adds to a session.
func carriedOlder(harness, box, writer string) string {
	return fmt.Sprintf("this box runs %s %s, older than the %s that wrote the session", harness, box, writer)
}
