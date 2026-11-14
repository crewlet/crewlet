package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A run's session, TAKEN out of its box to be kept (sandbox.SessionTaker).
//
// THREE ROADS IN, ONE OUT. The archive a run's script packed when its harness
// exited ([Runner.OpenCapture]) — at its own end, or at the end a stop, a loss
// or its turn's cap asked of it (end.go) — is taken as it is; a job that never
// got to pack — killed before it could, lost with its engine with its box left
// behind, still working where a box would not end it — is packed now, by the
// very pack its script runs ([Runner.PackCapture]); and a SAVE, packed while
// the job still works to a place of that save's own, named after the taker it
// is taken under ([Runner.PackSave]), is taken from that place alone by the
// take that names it ([sandbox.SessionTake.Save]), never read as the job's
// final archive and never packed by the take. Every way,
// the archive leaves the box through ONE scrub ([scrubArchive]): every value the
// engine put in the box taken out, and every file redacted, each read and
// written as what it is, so what is kept is the session minus the secrets and
// still the archive its harness restores.
//
// AND THE BOX IS TOLD WHAT WAS KEPT. A box keeps the digest of every archive its
// own copy of the session holds ([Paths.SessionDigests]), and a restore never
// writes an archive the box holds over its copy (carry.go). The scrub changes the
// archive's bytes, so the archive kept is one the box never packed: carried back
// into this box it would read as another box's and be restored over the copy
// that went on past it. So once the archive kept has been read whole, its
// digest is recorded beside the pack's ([sandbox.TakenSession.Keep]).

var _ sandbox.SessionTaker = (*Runner)(nil)

// TakeSession streams the session the box's last run worked in, as a capture
// keeps it ([sandbox.SessionTaker]): packed now where take says so, and else
// the archive the run's script left, or — where it left none — one packed now;
// or, for a take that names a save, that save as [Runner.PackSave] left it,
// packed by nothing here; scrubbed of take's values and redacted, a piece at a
// time, within the bounds take's archive bound sets ([scrubBounds]). The zero
// value where the box holds no session: a harness this runner carries none
// for, a run whose record names none, one whose harness wrote nothing to pack,
// or a save no longer there. An error where it holds one that cannot be taken:
// a pack that failed, an archive the box describes as past take's bound,
// refused before a byte of it is read, or a save named by a path that is no
// save's.
func (r *Runner) TakeSession(ctx context.Context, box sandbox.Sandbox,
	take sandbox.SessionTake) (sandbox.TakenSession, error) {

	carries, ok := r.harness.(carrier)
	if !ok {
		return sandbox.TakenSession{}, nil
	}
	if take.MaxBytes <= 0 {
		return sandbox.TakenSession{}, errors.New("executor: a take names no bound on the archive it keeps, " +
			"and a session is never read out of a box unbounded")
	}
	// A JOB THAT IS OVER IS TAKEN AS ITS SCRIPT LEFT IT where its script
	// packed it, and packed now where it did not — one that crashed before
	// its pack, one killed part way through it, one lost with its engine —
	// by the same pack, under the rule its own end packs by, its harness
	// gone; a job still working is packed now as it stands, held to the rule
	// of a pack beside a harness that may still write (take.Pack says which).
	// A SAVE is that save's own archive and nothing else: never the job's
	// capture, which every read of a job that is over takes as its last,
	// and never packed here: it is packed first, so that the scrub's values
	// are read once it is.
	//
	// EVERY READ STREAMS AN ARCHIVE NOTHING MOVES OVER WHILE IT READS IT:
	// the one its own pack made, kept for this take alone
	// ([Paths.SessionPackHeld]), or the one its own save made
	// ([Paths.SessionSave]), each released once the take is done with it —
	// once both passes of the scrub have read it, or once it is refused
	// unread; or, where the job's script packed it, the job's capture, which
	// only that script writes, once, as its harness exits
	// ([Runner.PackCapture] says why).
	var (
		archive sandbox.SessionArchive
		body    io.ReadCloser
		release = func(context.Context) {}
		err     error
	)
	switch {
	case take.Save != "" && take.Pack:
		return sandbox.TakenSession{}, errors.New("executor: a take of a save packs nothing: the save is " +
			"packed to its taker's own path first, and the take reads it there")
	case take.Save != "":
		archive, body, release, err = r.openSave(ctx, box, take.Save)
	default:
		if !take.Pack {
			archive, body, err = r.OpenCapture(ctx, box)
		}
		if err == nil && body == nil {
			archive, body, release, err = r.PackCapture(ctx, box, take.Pack)
		}
	}
	if err != nil || body == nil {
		return sandbox.TakenSession{}, err
	}
	if archive.Size > take.MaxBytes {
		_ = body.Close()
		release(ctx)
		return sandbox.TakenSession{}, fmt.Errorf("executor: the session's archive is %d bytes, past the %d "+
			"a session is kept at", archive.Size, take.MaxBytes)
	}
	bounds := boundsFor(take.MaxBytes)
	scrub := newScrubber(take.Scrub)
	archive.RunStart = keptRunStart(scrub, archive.RunStart)
	read, write := io.Pipe()
	out := &takenBody{PipeReader: read, done: make(chan struct{})}
	if !out.reading(body) {
		_ = body.Close()
	}
	sum := &cksum{}
	// WHAT THE SESSION'S LAST REQUEST CARRIED, and what Claude Code counts it
	// at, read off the archive as it is kept (measure.go), beside the take and
	// within its context, never holding it up.
	meter := newContextMeter(ctx, carries.layout(), archive.SessionID, archive.Version)
	go func() {
		defer close(out.done)
		reopen := func() (io.ReadCloser, error) {
			again, openErr := box.OpenFile(ctx, archive.Path)
			if openErr != nil {
				return nil, openErr
			}
			if !out.reading(again) {
				_ = again.Close()
				return nil, errTakeClosed
			}
			return again, nil
		}
		scrubErr := scrubArchive(carries.layout(), body, reopen, io.MultiWriter(write, sum, meter), scrub, bounds)
		out.finished(scrubErr, sum.String())
		// THE METER ENDS BEFORE THE READER DOES, so a caller that has read the
		// take to its end and asks what it measured waits on nothing but the
		// meter's last line.
		meter.finish(scrubErr)
		_ = write.CloseWithError(scrubErr)
		// AFTER THE READER'S END, which a release never holds up, and before
		// the goroutine's, which the reader's close waits for: no pack's
		// archive outlives the take that was handed it.
		release(ctx)
	}()
	paths := PathsFor(box)
	keep := func(ctx context.Context) error {
		digest, digestErr := out.digest()
		if digestErr != nil {
			return digestErr
		}
		return keepDigest(ctx, box, paths, archive.SessionID, digest)
	}
	return sandbox.TakenSession{Archive: archive, Body: out, Keep: keep, Context: meter.context,
		Count: meter.count}, nil
}

// keptRunStart is where an archive's run began in its session as the take
// keeps it ([sandbox.SessionArchive.RunStart]): the message it names spelled as
// the kept archive spells that message's id — every message's `id` passes the
// rewrite of a string under a key that is no credential's ([keptString]) — so
// the kept session holds the message under the id the start names. Where that
// rewrite leaves no message id ([sandbox.ValidMessageID]: a value the take names
// in it, a shape of a secret), the start is dropped, never the capture: the
// capture is kept naming no start, as one whose run recorded none.
func keptRunStart(scrub scrubber, start sandbox.RunStart) sandbox.RunStart {
	if start.After == "" {
		return start
	}
	kept := keptString(scrub, start.After)
	if !sandbox.ValidMessageID(kept) {
		return sandbox.RunStart{}
	}
	return sandbox.RunStart{After: kept}
}

// errTakeClosed is a scrub whose reader was closed before it reopened the
// box's archive for its second pass.
var errTakeClosed = errors.New("executor: the session's reader was closed while it was taken")

// takenBody is [Runner.TakeSession]'s reader: the scrub's pipe, and the box's
// archive the scrub is reading now, which a close ends too.
//
// ITS CLOSE ENDS EVERYTHING AND WAITS FOR IT. A read of the pipe in flight is
// ended by the pipe's close, a write the scrub had blocked on it fails, and a
// read of the box's archive the scrub had blocked on is ended by closing that
// archive — so the scrub's goroutine returns, and the close waits for it: no
// goroutine of a take outlives the reader the caller was handed.
type takenBody struct {
	*io.PipeReader
	done chan struct{}

	mu     sync.Mutex
	src    io.Closer
	closed bool
	// sum is the digest of everything the scrub wrote, set once it wrote
	// the whole archive; failed is why it did not.
	sum    string
	failed error
}

// reading names src as the archive the scrub reads now, false where the reader
// is already closed and src must not be read.
func (t *takenBody) reading(src io.Closer) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	if t.src != nil {
		_ = t.src.Close()
	}
	t.src = src
	return true
}

// finished records how the scrub ended.
func (t *takenBody) finished(err error, sum string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.src != nil {
		_ = t.src.Close()
		t.src = nil
	}
	if err != nil {
		t.failed = err
		return
	}
	t.sum = sum
}

// digest is the digest of the archive the scrub wrote, once it wrote all of it
// and the reader read it, or why there is none.
func (t *takenBody) digest() (string, error) {
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.failed != nil:
		return "", fmt.Errorf("executor: the session was not taken whole: %w", t.failed)
	case t.sum == "":
		return "", errors.New("executor: the session was not taken whole")
	}
	return t.sum, nil
}

// Close ends the take — see [takenBody].
func (t *takenBody) Close() error {
	err := t.PipeReader.Close()
	t.mu.Lock()
	t.closed = true
	src := t.src
	t.mu.Unlock()
	if src != nil {
		_ = src.Close()
	}
	<-t.done
	return err
}

// PackCapture packs, NOW, the session the box's last run worked in — by the
// pack its harness's script makes once the harness exits ([carrier.Pack]), its
// digest kept as one the box's copy holds — and streams it, described, as
// [Runner.OpenCapture] streams the one the script left, from a place of its own
// (below); with release, which removes what the pack left in the box for its
// reader once that reader is done with it, and is never nil beside a reader.
// For a job that never reached that pack: one killed before its script could
// pack, one lost with its engine whose box is still there, one whose own pack
// failed — each over — and one still WORKING where a box would not end it
// ([sandbox.SessionTake.Pack]), which working says. A job a stop, a loss or its
// turn's cap ended is taken as its script packed it on its way out (end.go),
// and packed here only where that pack never landed. A zero description and a
// nil reader where there is nothing to pack: a harness this runner carries none
// for, a run whose record names no session, a harness that wrote no transcript.
//
// THE RUNNER'S OWN PACK, NEVER ONE A CALLER SCRIPTS: every pack of a box's
// session keeps its digest there, or the box later takes its own archive for
// another box's and restores it over the copy that went on past it (carry.go).
//
// A PACK THAT FAILS IS AN ERROR, never "no session": a box without tar or
// gzip, an OpenCode that would not export, a tar that broke — where the record
// names a session the box holds — is a session that was not kept, and the
// capture stamps it refused with what the pack said, so a later launch knows
// the job's session is missing rather than reading an older capture as the
// job's last. The pack says which it is by its exit ([packNothing]).
//
// A JOB STILL WORKING is packed as it stands, by the rule a pack beside a
// working harness keeps (Claude Code's [claudeHeldTar]): a file its harness
// rewrote, shrank, replaced or removed while tar read it fails the pack, and so
// does any file but a transcript written since the pack began; a transcript or
// a subagent's thread it only appended to is packed as tar first saw it — its
// last line perhaps one its harness had not finished writing, which the scrub
// keeps as the text it is and the restore cuts back. A JOB THAT IS OVER is
// packed as the run's own end packs it: its harness is gone, and a pack that
// held it to the working rule would only wait on the box's clock and refuse a
// file that a process its harness started, outliving it, writes as it packs.
// An OpenCode export leaves out the message still being written, either way.
//
// EVERY PACK IN A STAGING DIRECTORY OF ITS OWN ([Paths.SessionPackStaging]),
// named here for that pack alone: two reads of one job at once — a stop's and
// a collection's — may each pack it, and two packs in one directory clear each
// other's files and can move each other's archives.
//
// AND EVERY PACK FOR ITS OWN READ ALONE, of a job that is over as much as of
// one still working: its archive is moved from its staging directory to the
// pack's own place beside it ([Paths.SessionPackHeld]), streamed from there —
// the second pass of a Claude Code take opening it there again ([scrubTar]) —
// and removed by release, NEVER MOVED OVER THE CAPTURE. A Claude Code take
// reads its archive twice and opens it again by its path for the second pass,
// so a read streaming a path that something else moves an archive over is
// refused whenever that move falls between its passes, the archive changed
// under it. Over one capture, two reads of a job that is over each moved their
// own archive there and streamed what it held when they opened it; the job's
// own end pack moves the job's final archive there the moment its harness
// exits, which can come while this pack runs, a pack beside a working harness
// taking seconds to check what it read; and moved there after that, the
// archive of a job as it stood replaced the final one, read by a read of the
// job once over as everything it did, the calls after it neither in its
// session nor ever told. So the capture is the job's own script's alone, moved
// into place once as its harness exits, and a read that streams it — the job
// left its archive — streams that one archive, which nothing moves over while
// it reads it ([Runner.OpenCapture]). Which of two reads' archives is kept, if
// either is, is the run's record's to say (internal/sandbox's takeCapture).
//
// THE BOX'S OWN ENVIRONMENT: the pack runs in a login shell with the box's bin
// directory first on its PATH, as the run's wrapper runs, but not with the
// run's environment, which no row keeps; a harness the seat's environment
// moved somewhere its image's profile does not is a pack that finds nothing.
func (r *Runner) PackCapture(ctx context.Context, box sandbox.Sandbox,
	working bool) (sandbox.SessionArchive, io.ReadCloser, func(context.Context), error) {

	carries, ok := r.harness.(carrier)
	if !ok {
		return sandbox.SessionArchive{}, nil, nil, nil
	}
	pack := uuid.NewString()
	paths := PathsFor(box)
	staged, dest := paths.SessionPackStaging(pack), paths.SessionPackHeld(pack)
	release := func(ctx context.Context) { r.dropPack(ctx, box, staged, dest) }
	rec, packed, err := r.packNow(ctx, box, func(paths Paths, session string) string {
		return carries.Pack(paths, session, staged, dest, working)
	})
	if err != nil || !packed {
		release(ctx)
		return sandbox.SessionArchive{}, nil, nil, err
	}
	described, err := r.archiveAt(ctx, box, dest, rec)
	if err == nil && described == nil {
		err = fmt.Errorf("executor: %s's session %s was packed and left no archive behind", r.harness.Name(), rec.id)
	}
	if err != nil {
		release(ctx)
		return sandbox.SessionArchive{}, nil, nil, err
	}
	body, err := box.OpenFile(ctx, described.Path)
	if err != nil {
		release(ctx)
		return sandbox.SessionArchive{}, nil, nil, fmt.Errorf("executor: opening the session the run packed: %w", err)
	}
	return *described, body, release, nil
}

// dropPack removes what a pack on demand or a save left for its read once that
// read is done with it — the archive it kept for the read
// ([Paths.SessionPackHeld], [Paths.SessionSave]), of a job over or still
// working, and the staging directory a pack on demand's script removes itself
// unless it was cut short — files, the last of them named in what it logs.
//
// PAST ctx'S CANCELLATION, AND WITHIN ITS DEADLINE ([releaseContext]): ctx may
// be the very thing that ended the read, and a cleanup that inherits a dead
// context does nothing; but the release runs on its read's close path, which
// the read waits on, so a box that stops answering once the archive is read
// holds the read no longer than its take's own time. Files it could not remove
// are said in the log and left to the clear before the box's next run
// ([Runner.ClearArtifacts]), which removes every pack's and every save's by
// their patterns.
func (r *Runner) dropPack(ctx context.Context, box sandbox.Sandbox, files ...string) {
	ctx, cancel := releaseContext(ctx, r.now())
	defer cancel()
	res, err := box.Exec(ctx, "rm -rf "+shellQuoteAll(files), sandbox.ExecOptions{})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	if err != nil {
		log.WarnContext(ctx, "harness_pack_not_removed", "harness", r.harness.Name(), "path", files[len(files)-1],
			"error", err.Error(), "detail", "the archive a pack made for its read was not removed once "+
				"read; the clear before the box's next run removes it")
	}
}

// releaseContext is the context a pack's release runs under ([Runner.dropPack]),
// made at now, by the runner's clock ([Runner.now]): past ctx's cancellation,
// and never past ctx's own deadline — the take's,
// which leaves the outcome stamp and the box's pause their room after it
// (internal/sandbox's takeContext) — unless that leaves less than
// [packReleaseFloor], which it is given instead, nor past the pack's own bound
// ([sandbox.SessionPackWait]), which bounds a release made under no deadline.
// A fresh pack's bound on every release let a box that stopped answering once
// its archive was read hold the read two minutes past the take's time, into
// what it leaves the stamp and the pause.
func releaseContext(ctx context.Context, now time.Time) (context.Context, context.CancelFunc) {
	deadline := now.Add(sandbox.SessionPackWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if floor := now.Add(packReleaseFloor); deadline.Before(floor) {
		deadline = floor
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
}

// packReleaseFloor is the least a pack's release is given ([releaseContext]),
// where the take it was made for has less time left, or none.
//
// FIVE SECONDS, and a judgment: the release is one `rm` of a file and a
// directory, which a box that answers at all answers in a fraction of a second
// — a local exec in milliseconds, a container's or an e2b sandbox's command in
// a few hundred — and past it the box is one that has stopped answering, whose
// leftovers the clear before its next run removes. It comes out of the half
// minute the take leaves its outcome stamp, a write the coordination store
// answers in milliseconds.
const packReleaseFloor = 5 * time.Second

// PackSave packs, NOW, a SAVE of the session the box's run is still working in,
// for taker — the holder of the lease the save is taken under, the taking
// process's incarnation (`<node>:<uuid>`) — by its harness's save pack
// ([saver]), into a place of that save's own, named after its taker
// ([Paths.SessionSave]), and nowhere else, its digest kept in the box as every
// pack's is; and describes it there, for the take that names it to stream
// ([sandbox.SessionTake.Save]), which removes it once it is done with it.
// Packed first and taken after, so whatever the take reads for its scrub — the
// tokens renewed in the box, a login exported as it ran — is read once the
// session is packed.
//
// NEVER THE JOB'S FINAL ARCHIVE: a save is packed beside a harness still
// working, and [Paths.SessionCapture] is what every read of a job that is over
// takes as its last (its collection, a stop's, a loss's) — a save moved there
// was collected as the final archive of a job whose own exit pack failed. And
// each save's own place, so two processes that save one box never share a
// file, and two saves of one taker at once — the drain's beside one the waiter
// asked for — each hand their take the archive they made: one path per taker,
// each moved its archive over the other's, and a Claude Code take, which opens
// its archive again by its path for its second pass ([scrubTar]), was refused
// wherever the other's move fell between its passes. HELD TO THE RULE OF A PACK
// BESIDE A WORKING HARNESS ([claudeHeldTar]): a file the harness rewrote under
// the pack fails it. A SAVE THAT FAILS LEAVES NOTHING: no archive of its own,
// its staging directory gone, and every other save's archive as it was. EVERY
// SAVE IN A STAGING DIRECTORY OF ITS OWN ([Paths.SessionSaveStaging]), named
// here for that save alone, as a pack on demand's is: two saves of one taker
// at once never clear each other's lists or archive. A save packed and never
// taken stays in the box until the clear before its next run, which removes
// every save's files.
//
// A zero description where there is nothing to save: a harness whose session
// the engine packs none of beside its run (OpenCode's, [saver]), a run whose
// record names no session, a harness that wrote no transcript. An error where a
// save could not be made, saying why — the check that refused it, for a file
// the harness wrote under the pack — or where taker names none.
func (r *Runner) PackSave(ctx context.Context, box sandbox.Sandbox, taker string) (sandbox.SessionArchive, error) {
	saves, ok := r.harness.(saver)
	if !ok {
		return sandbox.SessionArchive{}, nil
	}
	if taker == "" {
		return sandbox.SessionArchive{}, errors.New("executor: a save names no taker, and a save is packed " +
			"to a place named after its taker")
	}
	pack := uuid.NewString()
	paths := PathsFor(box)
	staged, dest := paths.SessionSaveStaging(taker, pack), paths.SessionSave(taker, pack)
	rec, packed, err := r.packNow(ctx, box, func(paths Paths, session string) string {
		return saves.PackSave(paths, session, staged, dest)
	})
	if err != nil || !packed {
		return sandbox.SessionArchive{}, err
	}
	described, err := r.archiveAt(ctx, box, dest, rec)
	if err == nil && described == nil {
		err = fmt.Errorf("executor: %s's session %s was saved and left no archive behind", r.harness.Name(), rec.id)
	}
	if err != nil {
		r.dropPack(ctx, box, staged, dest)
		return sandbox.SessionArchive{}, err
	}
	return *described, nil
}

// openSave streams the save at path ([Runner.PackSave], [Paths.SessionSave]),
// described, as [Runner.OpenCapture] streams the job's capture, with release,
// which removes it once its take is done with it — a nil reader and a zero
// description where nothing is there (a save already taken) or the record
// names no session, the save removed either way. A path that is no save of
// the box's ([savePath]) is refused unread: a take of a save streams that save
// and no other file of the box.
func (r *Runner) openSave(ctx context.Context, box sandbox.Sandbox, path string) (sandbox.SessionArchive,
	io.ReadCloser, func(context.Context), error) {

	paths := PathsFor(box)
	if !savePath(paths, path) {
		return sandbox.SessionArchive{}, nil, nil, fmt.Errorf("executor: %q is no save a runner packed in the "+
			"box, and a take of a save streams one and nothing else", path)
	}
	release := func(ctx context.Context) { r.dropPack(ctx, box, path) }
	rec, refusal, err := r.readSessionRecord(ctx, box, paths)
	var described *sandbox.SessionArchive
	if err == nil && refusal == "" {
		described, err = r.archiveAt(ctx, box, path, rec)
	}
	if err != nil || described == nil {
		release(ctx)
		return sandbox.SessionArchive{}, nil, nil, err
	}
	body, err := box.OpenFile(ctx, described.Path)
	if err != nil {
		release(ctx)
		return sandbox.SessionArchive{}, nil, nil, fmt.Errorf("executor: opening the session the run saved: %w", err)
	}
	return *described, body, release, nil
}

// savePath reports whether path names a save in the box paths lay out
// ([Paths.SessionSave]): in its work directory, after a taker's tag
// ([saveTag]) and a name minted for one save, and nothing past them.
func savePath(paths Paths, path string) bool {
	rest, ok := strings.CutPrefix(path, paths.WorkDir()+"/session-save.")
	if !ok {
		return false
	}
	tag, pack, ok := strings.Cut(rest, ".")
	if !ok || len(tag) != 2*saveTagBytes || strings.Trim(tag, "0123456789abcdef") != "" || len(pack) != 36 {
		return false
	}
	_, err := uuid.Parse(pack)
	return err == nil
}

// packNow runs pack — a harness's script that packs a session now, beside or
// after its harness — for the session the box's record names, in a login shell
// with the box's bin directory first on its PATH as the run's wrapper runs it,
// within [sandbox.SessionPackWait]: packed, where the pack made its archive;
// not, with no error, where there is nothing to pack — a record that names no
// session, a harness that wrote no transcript ([packNothing]); and an error
// where the pack failed, with what it said ([packFailed]).
func (r *Runner) packNow(ctx context.Context, box sandbox.Sandbox,
	pack func(paths Paths, session string) string) (sessionRecord, bool, error) {

	paths := PathsFor(box)
	rec, refusal, err := r.readSessionRecord(ctx, box, paths)
	if err != nil {
		return sessionRecord{}, false, fmt.Errorf("executor: reading which session the run worked in: %w", err)
	}
	if refusal != "" || rec.id == "" {
		return sessionRecord{}, false, nil
	}
	script := "PATH=" + sandbox.ShellQuote(paths.BinDir()) + `:"$PATH"; export PATH` + "\n" + pack(paths, rec.id)
	res, err := box.Exec(ctx, "sh -lc "+sandbox.ShellQuote(script),
		sandbox.ExecOptions{TimeoutSec: sandbox.SessionPackWait.Seconds()})
	if err != nil {
		return sessionRecord{}, false, fmt.Errorf("executor: packing %s's session %s: %w",
			r.harness.Name(), rec.id, err)
	}
	switch res.ExitCode {
	case 0:
		return rec, true, nil
	case packNothing:
		return sessionRecord{}, false, nil
	}
	return sessionRecord{}, false, fmt.Errorf("executor: %s's session %s could not be packed (exit %d): %s",
		r.harness.Name(), rec.id, res.ExitCode, firstLine(strings.TrimSpace(res.Stderr), packDetailLimit))
}

// readSessionRecord is the box's record of the session its run worked in
// ([Paths.SessionRecord]), read and parsed — or why it could not be read, a
// refusal of a path that holds no file the engine reads, which names no
// session.
func (r *Runner) readSessionRecord(ctx context.Context, box sandbox.Sandbox, paths Paths) (sessionRecord, string, error) {
	recorded, refusal, err := readWhole(ctx, box, paths.SessionRecord(), "the record of the session its harness ran in")
	if err != nil || refusal != "" {
		return sessionRecord{}, refusal, err
	}
	return parseSessionRecord(recorded), "", nil
}

// packDetailLimit is how much of the first line of a failed pack's error stream
// its error carries.
//
// FIVE HUNDRED AND TWELVE BYTES: the line names the session, and then why — the
// check that refused the pack ([claudeHeldTar]) or tar's own word, either
// naming a file under the session's directories, where a subagent's thread,
// nested, runs past a hundred bytes — which a cut at a transcript line's
// [transcriptDetailLimit] left off behind the engine's own hundred; and the
// error is stamped on the capture inside the 1 KiB its reason is held to
// (executorkeep.UnkeptReasonMaxBytes), with the engine's own words around it,
// which half of that leaves room for.
const packDetailLimit = 512

// The exits a pack made on demand ends with ([carrier.Pack]) beside 0, the
// session packed: packNothing, a session there is nothing of to pack — no
// transcript its harness wrote, no session its harness holds — and packFailed,
// one there is and the pack could not make, its error stream saying why.
const (
	packFailed  = 2
	packNothing = 3
)

// packDigest is the part of every pack — the run's own as its harness exits,
// one made on demand, a save — that records the digest of the archive it made
// at staged/capture as one of the session the shell variable session names that
// the box's own copy holds ([Paths.SessionDigests]), and reads that line back,
// BEFORE the archive is moved to where its read reads it: failing, its reason
// written to staged/error, where the line is not there after.
//
// A PACK REPORTS ITS ARCHIVE MADE ONLY ONCE ITS DIGEST IS RECORDED. Every pack
// keeps its digest so the box never takes an archive of its own for another
// box's and restores it over the copy that went on past it (carry.go). The
// packs used to move their archive into place, report it made and only then
// append its digest, a write nothing checked: on a full disk the archive stood
// where its read reads it with no digest of it in the box. So the digest is
// recorded first, of the archive as it will be moved, and the move is made only
// once the line is there — which a digest recorded and its archive then not
// moved never makes false, the box's copy holding that archive's session
// either way. READ BACK, never taken on the append's word: a write a full disk
// lost can be one the writer was told landed, as [claudeHeldTar]'s lists are
// read back for. A box without cksum or grep, which every box's userland has,
// packs nothing it cannot record.
func packDigest(paths Paths, session, staged string) string {
	q := sandbox.ShellQuote
	return fmt.Sprintf(`{ d=$(cksum < %[1]s 2> /dev/null) && %[2]s || { printf '%%s\n' %[3]s > %[4]s 2> /dev/null; false; }; }`,
		q(staged+"/capture"), recordDigest(paths, session, "d"), q(digestUnrecorded), q(staged+"/error"))
}

// recordDigest is the part of a box's script that records the line `<session>
// <digest>` — the values of the shell variables session and digest name — in
// the box's list of the archives its copy holds ([Paths.SessionDigests]) and
// reads it back: true only where the digest is not empty and the line is
// there after. READ BACK, never taken on the append's word: a write a full
// disk lost can be one its writer was told landed. What every pack records
// before it moves its archive into place ([packDigest]), and every restore
// before its run continues what it restored ([claudeSessionGate],
// [openCodeSessionGate]).
func recordDigest(paths Paths, session, digest string) string {
	return fmt.Sprintf(`[ -n "$%[2]s" ] && { printf '%%s %%s\n' "$%[1]s" "$%[2]s" >> %[3]s; } 2> /dev/null && grep -qxF "$%[1]s $%[2]s" %[3]s 2> /dev/null`,
		session, digest, sandbox.ShellQuote(paths.SessionDigests()))
}

// digestUnrecorded is why a pack whose digest the box did not record failed
// ([packDigest]).
const digestUnrecorded = "the archive's digest could not be recorded in the box's list of the archives " +
	"its copy of the session holds — taken by the box's cksum, read back by its grep — so the archive was " +
	"not moved into place"

// restoreDigestUnrecorded is why a run asked to continue a session carried into
// its box starts one of its own where the carried archive's digest could not
// be recorded once it was restored ([recordDigest]): continued without it, the
// box's copy would go on past an archive the box does not know it holds, and
// that archive, carried in again, would be restored over it.
const restoreDigestUnrecorded = "its archive's digest could not be recorded in this box's list of the " +
	"archives its copy of the session holds, so the copy restored from it is not continued"

// keepDigest records digest — `<crc> <length>`, as POSIX cksum prints it for its
// standard input — as the digest of an archive of session the box's own copy
// holds ([Paths.SessionDigests]): the line a run's script writes for an archive
// it packed or restored, written here for the archive a capture kept.
func keepDigest(ctx context.Context, box sandbox.Sandbox, paths Paths, session, digest string) error {
	if !sandbox.ValidSessionID(session) {
		return fmt.Errorf("executor: %q is not a session id a digest may be kept under", session)
	}
	res, err := box.Exec(ctx, "printf '%s\\n' "+sandbox.ShellQuote(session+" "+digest)+" >> "+
		sandbox.ShellQuote(paths.SessionDigests()), sandbox.ExecOptions{})
	switch {
	case err != nil:
		return fmt.Errorf("executor: recording the kept archive's digest in the box: %w", err)
	case res.ExitCode != 0:
		return fmt.Errorf("executor: recording the kept archive's digest in the box: exit %d: %s",
			res.ExitCode, firstLine(res.Stderr, transcriptDetailLimit))
	}
	return nil
}

// cksum is POSIX cksum's digest of what was written to it, as `cksum` prints
// it for its standard input: `<crc> <length>`.
//
// THE CRC POSIX.1 SPECIFIES, which is not hash/crc32's: polynomial 0x04C11DB7
// taken most significant bit first, over the bytes and then over their count —
// least significant byte first, and no more bytes than the count needs — and
// complemented. It is the digest a box's scripts take with the `cksum` every
// image has, so it is the one the box's restore compares an archive by.
type cksum struct {
	crc uint32
	n   int64
}

// cksumTable is the CRC's byte table, most significant bit first.
var cksumTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

func (c *cksum) Write(p []byte) (int, error) {
	for _, b := range p {
		c.crc = c.crc<<8 ^ cksumTable[byte(c.crc>>24)^b]
	}
	c.n += int64(len(p))
	return len(p), nil
}

// String is the digest so far: the CRC folded over the length, complemented,
// beside the length.
func (c *cksum) String() string {
	crc := c.crc
	for n := c.n; n > 0; n >>= 8 {
		crc = crc<<8 ^ cksumTable[byte(crc>>24)^byte(n)]
	}
	return fmt.Sprintf("%d %d", ^crc, c.n)
}
