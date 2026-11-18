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
// the runtime starting, as for [openCodeListenWait]. Sixty seconds is room
// for a contended disk or CPU; a box silent past it is not answering, and the
// caller starts the run fresh rather than wait.
const harnessVersionWait = openCodeListenWait

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
	// from is where a run that continued its session began in it: the newest
	// message the session held before the run, decoded off the recorded
	// cursor ([runStartOf]); "" where none is read or the run did not continue.
	from string
	// why is why a run asked to continue a session started a new one
	// ([sandbox.Result.FellBack]), as the box holds it: bounded by its one
	// reader, where the result is built ([Runner.Collect]). "" for other runs.
	why string
	// release is the harness's `--version`, as it printed it.
	release string
}

// parseSessionRecord reads a run's record of its session, a line each:
// `id`, `continued <yes|no>`, `why` (why it started anew, one line, else
// empty), `release` (what --version printed) and, for an OpenCode run that
// continued, `from <cursor>` before the release ([openCodeRunStart]). The
// first line of each key is the record's: the release, written last, may run
// on into the harness's own lines, where an `id` or a `from` is not the
// script's. An id no harness mints ([sandbox.ValidSessionID]) is no id;
// `from` is read only whole ([runStartOf]) and where `continued` reads `yes`;
// a `continued` of neither is a start not known, never the whole session.
// Walked a line at a time to the fourth key, never split: the record is
// agent-writable up to [sandbox.MaxFileBytes] and read on every collection
// and take (TestASessionRecordIsReadWhereItLies).
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
// run's script records as where the run began ([openCodeRunStart]); "" for
// anything else. OpenCode's cursor is the unpadded base64url of
// `JSON.stringify({id, order, direction})` (read in the 2.0.32, 2.1.4 and
// 2.2.7 bundles; testdata/opencode/<version>/context),
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

// releasePattern is a release's MAJOR.MINOR.PATCH, wherever a harness's
// `--version` prints it: first for Claude Code (`2.1.310 (Claude Code)`),
// after a name for OpenCode (`opencode v2.0.32`).
var releasePattern = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)

// releaseOf is the release a harness's `--version` printed, "2.1.310", or ""
// where it printed none this code can read.
//
// The pattern has no length, but what it reads was printed in the box (the
// record is agent-writable, `--version` whatever PATH finds), so a file-long
// run of digits would read as a release. So it is held to a release's shape
// ([sandbox.ValidRelease]), as the launch's record holds it: past the
// executor log's bound it is refused, never cut, since a shortened release
// names another; read as none, every reader takes it for a current one.
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

// archiveAt describes the session archive the box holds at path (the run's
// capture [Paths.SessionCapture], a pack on demand's [Paths.SessionPackHeld]
// or a save [Paths.SessionSave]) for the session rec names, or nil where there
// is none: a harness that carries no session, no archive, a record naming no
// session, or a path that is no regular file, which costs only the capture.
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

// CheckCarry answers whether archive, packed in ANOTHER box, can be continued
// in box, asked before the run starts there and before its brief is chosen
// (briefs are written before the box is acquired): the box harness's release,
// read by one login-shell exec of its `--version` with the box's bin directory
// first on PATH as the wrapper puts it ([withBoxBin]), held against the
// writer, the archive's format and the harness's floors ([carrier.Continues]).
// The release read is returned either way: the launch hands a brief told by a
// reading of the session only where it is exactly the writer's
// ([sandbox.ExactTelling]).
//
// Three answers, as a lease's: it can, it cannot (and why), and an error where
// the box could not be asked, never read as either.
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
