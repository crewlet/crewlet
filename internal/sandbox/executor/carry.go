package executor

import (
	"context"
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
	// why is why a run asked to continue a session started a new one in its
	// place, as its script said it ([sandbox.Result.FellBack]); "" for every
	// other run.
	why string
	// release is the harness's `--version`, as it printed it.
	release string
}

// parseSessionRecord reads a run's record of its session: `id <id>`,
// `continued <yes|no>`, `why <why it started anew>` and `release <what
// --version printed>`, a line each, `why` empty for a run that did not.
//
// THE FIRST LINE OF EACH KEY IS THE RECORD'S. The script writes the release
// last, as the harness printed it, and a harness that printed more than one line runs
// on into lines of its own — an `id` there is the harness's text, never the
// script's. The script writes `why` on one line, whatever the words it
// quotes held. An id that is not one a harness mints ([validSessionID]) is no id.
func parseSessionRecord(text string) sessionRecord {
	var rec sessionRecord
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		key, value, _ := strings.Cut(line, " ")
		if seen[key] {
			continue
		}
		seen[key] = true
		value = strings.TrimSpace(value)
		switch key {
		case "id":
			if validSessionID(value) {
				rec.id = value
			}
		case "continued":
			rec.continued = value == "yes"
		case "why":
			rec.why = value
		case "release":
			rec.release = value
		}
	}
	return rec
}

// validSessionID reports whether id has the shape every session id a runner
// names has — a UUID, `ses_` and hexadecimal digits — loosely: letters,
// digits, `-` and `_`, at most 128 of them. The record is a file in a box the
// run's own agent can write, and an id read from it names an archive and is
// handed to a later run's script.
func validSessionID(id string) bool {
	return id != "" && len(id) <= 128 && !strings.ContainsFunc(id, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_'
	})
}

// releasePattern is a release's MAJOR.MINOR.PATCH, wherever a harness's
// `--version` prints it: first for Claude Code (`2.1.310 (Claude Code)`),
// after a name for OpenCode (`opencode v2.0.32`).
var releasePattern = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+`)

// releaseOf is the release a harness's `--version` printed, "2.1.310", or "" where
// it printed none this code can read.
func releaseOf(printed string) string { return releasePattern.FindString(printed) }

// captured describes the session archive the box's last run packed
// ([Paths.SessionCapture]), for the session rec names, or nil where there is
// none: a harness that carries no session, a run that packed none, a record that
// names no session, or a path that holds no regular file — the box's own
// doing, which costs the capture and nothing else.
func (r *Runner) captured(ctx context.Context, box sandbox.Sandbox, paths Paths,
	rec sessionRecord) (*sandbox.SessionArchive, error) {

	carries, ok := r.harness.(carrier)
	if !ok || rec.id == "" {
		return nil, nil
	}
	end, err := box.ReadTail(ctx, paths.SessionCapture(), 0)
	if errors.Is(err, sandbox.ErrNotRegularFile) {
		log.WarnContext(ctx, "harness_capture_unread", "harness", r.harness.Name(), "session", rec.id,
			"detail", "the path the run packs its session to holds something other than a file, "+
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
		Version: releaseOf(rec.release), Size: end.Size, Path: paths.SessionCapture(),
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
func (r *Runner) OpenCapture(ctx context.Context, box sandbox.Sandbox) (sandbox.SessionArchive, io.ReadCloser, error) {
	paths := PathsFor(box)
	recorded, refusal, err := readWhole(ctx, box, paths.SessionRecord(), "the record of the session its harness ran in")
	if err != nil {
		return sandbox.SessionArchive{}, nil, fmt.Errorf("executor: reading which session the run packed: %w", err)
	}
	if refusal != "" {
		return sandbox.SessionArchive{}, nil, nil
	}
	described, err := r.captured(ctx, box, paths, parseSessionRecord(recorded))
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
	ask := "PATH=" + shellQuote(PathsFor(box).BinDir()) + `:"$PATH"; export PATH; exec ` +
		carries.VersionCommand() + " < /dev/null"
	res, err := box.Exec(ctx, "sh -lc "+shellQuote(ask), sandbox.ExecOptions{TimeoutSec: harnessVersionWait.Seconds()})
	if err != nil {
		return sandbox.CarryCheck{}, fmt.Errorf("executor: asking the box's %s for its release: %w",
			r.harness.Name(), err)
	}
	if res.ExitCode != 0 {
		return sandbox.CarryCheck{Why: fmt.Sprintf("the box's %s did not say its release: `%s` exited %d: %s",
			r.harness.Name(), carries.VersionCommand(), res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))}, nil
	}
	check := sandbox.CarryCheck{Version: releaseOf(res.Stdout)}
	switch {
	case archive.Harness != r.harness.Name():
		check.Why = fmt.Sprintf("the session was written by %q, and this box runs %s: a session is "+
			"continued only by the harness that wrote it", archive.Harness, r.harness.Name())
	case archive.Format != carries.ArchiveFormat():
		check.Why = fmt.Sprintf("the session's archive is format %d, and this build restores %s's "+
			"format %d alone", archive.Format, r.harness.Name(), carries.ArchiveFormat())
	default:
		check.Continues, check.Why = carries.Continues(check.Version, archive.Version)
	}
	return check, nil
}

// carriedOlder is the reason a box's release cannot continue a session a newer
// one wrote, shared by both harnesses: a release reads what it and those before it
// wrote, never what a later one adds to a session.
func carriedOlder(harness, box, writer string) string {
	return fmt.Sprintf("this box runs %s %s, older than the %s that wrote the session", harness, box, writer)
}
