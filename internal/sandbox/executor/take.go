package executor

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A run's session, TAKEN out of its box to be kept (sandbox.SessionTaker).
//
// Three roads in, one out. The archive the run's script packed when its harness
// exited ([Runner.OpenCapture]) is taken as it is; a job that never packed
// (killed, lost with its engine, still working) is packed now by the script's
// own pack ([Runner.PackCapture]); a SAVE, packed mid-job to its own place
// ([Runner.PackSave]), is taken from that place alone by the take that names it
// ([sandbox.SessionTake.Save]), never read as the final archive, never packed
// here. Every archive leaves the box through ONE scrub ([scrubArchive]): the
// engine's values taken out, every file redacted, still an archive its harness
// restores.

// The box is told what was kept: it digests every archive of its session copy
// ([Paths.SessionDigests]) and a restore never writes a held archive over its
// copy (carry.go). The scrub changes the bytes, so the kept archive would read
// as another box's and overwrite the copy that went on past it; its digest is
// therefore recorded beside the pack's ([sandbox.TakenSession.Keep]).

var _ sandbox.SessionTaker = (*Runner)(nil)

// SessionTakesAtOnce is how many takes this process scrubs at once, each
// holding a slot of [sessionTakes] from before it asks its box for anything
// to its scrub's and its meter's end. 29: a take at its base holds at most
// 11.64 MiB ([takeBaseHeldBytes]); 29 times over is 337.5 MiB, within the
// base tier's 340.6 MiB (64 reads at [baseTierReadBytes], the package doc's
// "A collection's read of a thread"): so the takes at their base hold no
// more than every read of the tier does.
const SessionTakesAtOnce = 29

// takeBaseHeldBytes is what a take at its base holds at most, its scrub's and
// its meter's at once, 11.64 MiB: each part's live high-water, the scrub's
// 8.00 MiB (BenchmarkATakeAtItsBase, its spool's gzip writer among it) and the
// meter's 3.53 (BenchmarkAMeterAtItsBase), the most of 30 runs each, one a
// process, on a two-core slot, live bytes under a stop-the-world GC at every
// 1% of growth, to the KiB; and 1% of each beside it, what that sampling can
// miss: a buffer allocated and dropped within 1% of the heap's growth.
const (
	scrubBaseLiveBytes = 8189 << 10
	meterBaseLiveBytes = 3610 << 10
	scrubBaseHeldBytes = scrubBaseLiveBytes + scrubBaseLiveBytes/100
	meterBaseHeldBytes = meterBaseLiveBytes + meterBaseLiveBytes/100
	takeBaseHeldBytes  = scrubBaseHeldBytes + meterBaseHeldBytes
)

// takeLargeHeldBytes is what the large slot adds for a take, past what
// [takeBaseHeldBytes] counts: its scrub at the plaintext's bound and the
// longest value it can hold, less the scrub's base, beside its meter's line
// past the base's charge, at 1.25 bytes a charged byte (the package doc's THE
// COEFFICIENTS); the meter's reader and the strings a charge covers are its
// base whatever the line.
func takeLargeHeldBytes() int64 {
	return scrubHeldBytes(scrubPlainMax, scrubLongestMax) - scrubBaseHeldBytes +
		(threadLineMax-collectionBaseLineCharge)*5/4
}

// The takes at their base hold no more than the box host's reads for its peers
// at [baseTierReadBytes] each: a negative constant does not compile.
const _ = uint(queue.ConcurrentAnswers*baseTierReadBytes - SessionTakesAtOnce*takeBaseHeldBytes)

// sessionTakes holds a slot for each take in flight ([SessionTakesAtOnce]).
var sessionTakes = make(chan struct{}, SessionTakesAtOnce)

// errTakeWait is a take whose wait for a slot of [sessionTakes] ended with its
// ctx: refused, as a box read whose wait runs out is, its box asked nothing.
var errTakeWait = errors.New("executor: the wait for a session take's slot ended")

// takeSessionSlot takes a slot of [sessionTakes] within ctx, the take's own
// wait. A take that waits says so as its wait ends
// (executor_session_take_waited): that the slots are contended, and where it
// ran out waiting, why it was refused.
func takeSessionSlot(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", errTakeWait, err)
	}
	select {
	case sessionTakes <- struct{}{}:
		return func() { <-sessionTakes }, nil
	default:
	}
	start := time.Now()
	took, err := takeSlot(ctx, sessionTakes)
	log.InfoContext(ctx, "executor_session_take_waited", "took", err == nil,
		"waited_ms", time.Since(start).Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errTakeWait, err)
	}
	return took, nil
}

// TakeSession streams the session the box's last run worked in, as a capture
// keeps it ([sandbox.SessionTaker]): packed now where take says so, and else
// the archive the run's script left, or — where it left none — one packed now;
// or, for a take that names a save, that save as [Runner.PackSave] left it,
// packed by nothing here; scrubbed of take's values and redacted, a piece at a
// time, within the bounds take's archive bound sets ([scrubBounds]). The zero
// value where the box holds no session: a harness this runner carries none
// for, a run whose record names none, one whose harness wrote nothing to pack,
// or a save no longer there. An error where it holds one that cannot be taken:
// a pack that failed, an archive no box continues ([sandbox.UnrestorableError])
// or one past take's bound, each refused before a byte of it is read, or a
// save named by a path that is no save's.
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
	if take.Spool.Dir == "" || take.Spool.Bytes <= 0 || take.Spool.Held == nil {
		return sandbox.TakenSession{}, errors.New("executor: a take names no spool for its scrub, which " +
			"spools on its process's own disk in the room the take holds")
	}
	// ONE OF [SessionTakesAtOnce], held by the scrub's goroutine to its end,
	// and given back here where none starts.
	freeSlot, slotErr := takeSessionSlot(ctx)
	if slotErr != nil {
		return sandbox.TakenSession{}, slotErr
	}
	scrubbingStarted := false
	defer func() {
		if !scrubbingStarted {
			freeSlot()
		}
	}()
	// A job that is over is taken as its script left it, or packed now by the
	// same pack under the rule its own end packs by when it did not (crashed,
	// killed mid-pack, lost with its engine); a job still working is packed as
	// it stands, held to the rule for a harness that may still write
	// (take.Pack says which). A SAVE is its own archive and nothing else: never
	// the job's capture, never packed here.
	//
	// Every read streams an archive nothing moves over meanwhile: the take's
	// own pack ([Paths.SessionPackHeld]) or save ([Paths.SessionSave]),
	// released once both scrub passes have read it or it is refused unread; or
	// the job's capture, which only its script writes, once
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
		archive, body, release, err = r.openSave(ctx, box, take.Save, take.Session)
	default:
		if !take.Pack {
			archive, body, err = r.openCapture(ctx, box, take.Session)
		}
		if err == nil && body == nil {
			archive, body, release, err = r.packCapture(ctx, box, take.Pack, take.Session)
		}
	}
	if err != nil || body == nil {
		return sandbox.TakenSession{}, err
	}
	// NOR IS ONE ITS SCRIPT OR A SAVE PACKED read, whatever its size: it is
	// refused before a byte of it is streamed ([sandbox.UnrestorableError]).
	if refused := r.unrestorable(archive); refused != nil {
		_ = body.Close()
		release(ctx)
		return sandbox.TakenSession{}, refused
	}
	if archive.Size > take.MaxBytes {
		_ = body.Close()
		release(ctx)
		return sandbox.TakenSession{}, fmt.Errorf("executor: the session's archive is %d bytes, past the %d "+
			"a session is kept at: %w", archive.Size, take.MaxBytes, executorkeep.ErrSessionTooLarge)
	}
	// ONE PASS, the scrub and its meter: held to the base, past it the one
	// large slot both share while either holds a piece past it (the package
	// doc's "A collection's read of a thread"). Each one's wait for it ends
	// with the take's reader, as the scrub's reads of the box do
	// ([takenBody]); the meter's apart, as it walks on past the scrub's end.
	large := new(passBudget)
	scrubbing, stopScrub := context.WithCancel(ctx)
	metering, stopMeter := context.WithCancel(ctx)
	remembered := &memoUse{memo: r.memo, scope: take.Scope}
	// WHAT THE SCRUB TAKES OUT of what the meter counts, added back to its
	// count once the scrub ends (scrubtally.go).
	tally := newScrubTally(carries, archive.SessionID)
	bounds := plainBounds(scrubPlainMax).gatedBy(scrubbing, large).remembering(remembered).
		spooling(spoolPlace{dir: take.Spool.Dir, room: take.Spool.Bytes, held: take.Spool.Held}).tallying(tally)
	scrub := newScrubber(take.Scrub)
	archive.RunStart = keptRunStart(scrub, archive.RunStart)
	// WHERE A CHAT FILE'S RUN BEGAN, checked against the raw bytes the scrub
	// reads, the only place they exist: the kept archive is rewritten.
	prefix := newPrefixCheck(archive.Prefix)
	bounds = bounds.checking(prefix)
	read, write := io.Pipe()
	out := &takenBody{PipeReader: read, done: make(chan struct{}), stopScrub: stopScrub, stopMeter: stopMeter}
	if !out.reading(body) {
		_ = body.Close()
	}
	sum := &cksum{}
	// WHAT THE SESSION'S LAST REQUEST CARRIED, and what its harness counts it
	// at, read off the archive as it is kept (measure.go), beside the take and
	// within its context, never holding it up.
	meter := newContextMeter(metering, carries, archive.SessionID, archive.Version, large)
	scrubbingStarted = true
	go func() {
		defer close(out.done)
		defer freeSlot()
		defer stopScrub()
		// THE TAKE ENDS WITH ITS METER, which may still wait for the slot.
		defer func() {
			<-meter.done
			stopMeter()
		}()
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
		scrubErr := plainTooLarge(scrubContained(ctx, func() error {
			return scrubArchive(carries.layout(), body, reopen, io.MultiWriter(write, sum, meter), scrub, bounds)
		}))
		logMemoUse(ctx, remembered)
		out.finished(scrubErr, sum.String())
		// THE METER ENDS BEFORE THE READER DOES, so a caller that has read the
		// take to its end and asks what it measured waits on nothing but the
		// meter's last line.
		meter.finish(scrubErr, tally.taken)
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
	runLines := func() int64 {
		<-out.done
		if !out.whole() {
			return 0
		}
		return prefix.lines()
	}
	return sandbox.TakenSession{Archive: archive, Body: out, Keep: keep, Context: meter.context,
		Count: meter.count, CountProvider: meter.countProvider, Next: meter.next, RunLines: runLines,
		ResumeRead: r.resumeRead(archive.Version)}, nil
}

// plainTooLarge is a take's scrub error as the take refuses it: past the
// plaintext the take's scrub reads ([scrubPlainMax]), a session too large to
// keep ([executorkeep.ErrSessionPlainTooLarge]), whose stamp says so as it
// says the archive's own bound, never as a take that failed.
func plainTooLarge(err error) error {
	if errors.Is(err, errScrubBound) {
		return fmt.Errorf("%w: %w", executorkeep.ErrSessionPlainTooLarge, err)
	}
	return err
}

// logMemoUse logs what a take's scrub did with the process's memo, where it
// met a text of a chunk or more: the figures its bounds are tuned by
// ([scrubMemoEntries]).
func logMemoUse(ctx context.Context, use *memoUse) {
	if use.memo == nil || use.n.hits+use.n.misses == 0 {
		return
	}
	entries, evicted := use.memo.size()
	log.InfoContext(ctx, "executor_session_scrub_memo", "hits", use.n.hits, "misses", use.n.misses,
		"diverged", use.n.diverged, "entries", entries, "evicted", evicted)
}

// scrubContained runs scrub, the take's scrub of a box's archive, a panic in it
// the take's error rather than the end of the engine.
//
// THE SCRUB RUNS ON A GOROUTINE OF THE TAKE'S OWN, which nothing above it can
// recover: a defect a box's text reaches in a redaction or a rewrite would end
// the engine's process, every seat the node holds with it, for one session the
// box's code wrote. So the panic is contained here, told to whoever observes
// contained panics (panics.Recovered) under the take's scope, its stack logged,
// and the take refused with the panic's message, which the capture's stamp
// carries as the reason the session was not kept. The pipe the take writes to
// ends on that error, so no reader takes the archive for whole.
func scrubContained(ctx context.Context, scrub func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			contained := panics.Recovered(panics.ScopeSessionTake, r)
			log.ErrorContext(ctx, "session_scrub_panicked", "panic", fmt.Sprint(r), "stack", contained.Stack)
			err = fmt.Errorf("executor: the session's scrub failed, and the session was not kept: %w",
				contained.Err())
		}
	}()
	return scrub()
}

// keptRunStart is where an archive's run began in its session as the take
// keeps it ([sandbox.SessionArchive.RunStart]): the message it names spelled as
// the kept archive spells that message's id — every message's `id` passes the
// rewrite of a string under a key that is no credential's ([keptString]) — so
// the kept session holds the message under the id the start names. Where that
// rewrite leaves no message id ([sandbox.ValidMessageID]: a value the take names
// in it, a shape of a secret), the start is dropped, never the capture: the
// capture is kept naming no start, as one whose run recorded none.
//
// A LINE COUNT IS NO TEXT THE SCRUB REWRITES, and passes as it is; it is the
// take's own, checked against the raw archive ([prefixCheck]).
func keptRunStart(scrub scrubber, start sandbox.RunStart) sandbox.RunStart {
	if start.After == "" {
		return start
	}
	kept := keptString(scrub, start.After)
	if !sandbox.ValidMessageID(kept) {
		return sandbox.RunStart{Lines: start.Lines}
	}
	return sandbox.RunStart{After: kept, Lines: start.Lines}
}

// prefixCheck checks a box's word on where a chat file's run began
// ([sandbox.LinePrefix]) against the raw bytes of the archive's one regular
// member, as the scrub's first reading reads them: the POSIX cksum of its
// first Lines lines, through the last one's break, fed at most once a byte
// whatever the reading reads again ([scrubBounds.checking]). Nil, or for a
// zero prefix, it checks nothing.
type prefixCheck struct {
	want sandbox.LinePrefix
	sum  cksum
	// at is how many of the member's bytes were fed, breaks how many line
	// breaks.
	at, breaks int64
	// fed is the index of the regular member fed, -1 before one; others
	// says the archive holds another.
	fed    int
	others bool
}

// newPrefixCheck checks want, nil where want names no lines.
func newPrefixCheck(want sandbox.LinePrefix) *prefixCheck {
	if want.Lines <= 0 {
		return nil
	}
	return &prefixCheck{want: want, fed: -1}
}

// member is body, the member at index of a reading, feeding c its bytes where
// it is the archive's one regular member.
func (c *prefixCheck) member(index int, h *tar.Header, body io.Reader) io.Reader {
	if c == nil || h.Typeflag != tar.TypeReg {
		return body
	}
	if c.fed >= 0 && c.fed != index {
		c.others = true
		return body
	}
	c.fed = index
	return &prefixFeed{r: body, check: c}
}

// feed hands c the member's bytes p, which begin at offset pos.
func (c *prefixCheck) feed(pos int64, p []byte) {
	if pos+int64(len(p)) <= c.at || c.breaks == c.want.Lines || c.at >= c.want.Bytes {
		return
	}
	if pos > c.at {
		// A GAP THE READING SKIPPED: no reading skips bytes it never read,
		// so this is no prefix this check saw whole.
		c.others = true
		return
	}
	p = p[c.at-pos:]
	for i, b := range p {
		if b == '\n' {
			c.breaks++
			if c.breaks == c.want.Lines {
				p = p[:i+1]
				break
			}
		}
	}
	_, _ = c.sum.Write(p)
	c.at += int64(len(p))
}

// lines is the prefix's line count where the member held exactly it, whole
// and as the box digested it; 0 otherwise.
func (c *prefixCheck) lines() int64 {
	if c == nil || c.others || c.breaks != c.want.Lines || c.at != c.want.Bytes ||
		c.sum.String() != fmt.Sprintf("%d %d", c.want.CRC, c.want.Bytes) {
		return 0
	}
	return c.want.Lines
}

// prefixFeed is a member's body feeding its check every byte read through it,
// at its offset in the member.
type prefixFeed struct {
	r     io.Reader
	check *prefixCheck
	pos   int64
}

func (f *prefixFeed) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	f.check.feed(f.pos, p[:n])
	f.pos += int64(n)
	return n, err
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
	// stopScrub ends the scrub's wait for a large slot ([passBudget]), and
	// stopMeter the meter's, where the scrub did not write the whole take:
	// the meter of one that did walks its tail whatever the reader does.
	stopScrub, stopMeter context.CancelFunc

	mu     sync.Mutex
	src    io.Closer
	closed bool
	// sum is the digest of everything the scrub wrote, set once it wrote
	// the whole archive; failed is why it did not.
	sum    string
	failed error
}

// whole reports whether the scrub wrote the whole archive.
func (t *takenBody) whole() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sum != ""
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
	t.stopScrub()
	if !t.whole() {
		t.stopMeter()
	}
	<-t.done
	return err
}

// PackCapture packs, NOW, the session the box's last run worked in, by the pack
// its harness's script makes ([carrier.Pack]), and streams it, described, as
// [Runner.OpenCapture] streams the script's own; release removes what the pack
// left in the box and is never nil beside a reader. It is for a job that never
// reached its script's pack (killed, lost with its engine, its pack failed) and
// one still WORKING ([sandbox.SessionTake.Pack]); a job a stop, a loss or its
// cap ended is packed here only where its end pack (end.go) never landed. A
// zero description and nil reader mean nothing to pack. The runner's own pack,
// never a caller's script: every pack keeps its digest in the box, or the box
// takes its own archive for another's and restores it over its newer copy
// (carry.go).
func (r *Runner) PackCapture(ctx context.Context, box sandbox.Sandbox,
	working bool) (sandbox.SessionArchive, io.ReadCloser, func(context.Context), error) {
	return r.packCapture(ctx, box, working, "")
}

// packCapture is [Runner.PackCapture] of a minting harness's job bound to
// bound ([sandbox.SessionTake.Session]), which PackCapture names none of.
func (r *Runner) packCapture(ctx context.Context, box sandbox.Sandbox, working bool,
	bound string) (sandbox.SessionArchive, io.ReadCloser, func(context.Context), error) {

	// Nothing to pack: a harness this runner carries none for, a record that
	// names no session, or a harness that wrote no transcript.
	carries, ok := r.harness.(carrier)
	if !ok {
		return sandbox.SessionArchive{}, nil, nil, nil
	}
	// Every pack has a staging directory of its own: two reads of one job at
	// once (a stop's and a collection's) in one directory clear each other's
	// files and move each other's archives. Its archive then moves to its own
	// held place ([Paths.SessionPackHeld]), NEVER OVER THE CAPTURE: a path
	// another archive is moved over between a read's passes ([scrubTar] opens
	// it again) refuses the read, and a partial archive of a still-working job
	// moved there after the end pack would replace the final one and lose the
	// calls after it. The capture is its script's alone ([Runner.OpenCapture]);
	// which of two reads' archives is kept is internal/sandbox's takeCapture.
	pack := uuid.NewString()
	paths := PathsFor(box)
	staged, dest := paths.SessionPackStaging(pack), paths.SessionPackHeld(pack)
	release := func(ctx context.Context) { r.dropPack(ctx, box, staged, dest) }
	// A pack that fails is an error, never "no session": the capture is
	// stamped refused so a later launch does not read an older capture as the
	// last; the pack says which by its exit ([packNothing]). A working job is
	// packed by the rule for a pack beside a live harness ([claudeHeldTar]): a
	// file changed other than appended fails it, and an appended transcript is
	// packed as tar saw it, its last line perhaps unfinished, which the scrub
	// keeps as text and the restore cuts back. A job over is packed as its own
	// end packs it, or the working rule would refuse files a process its
	// harness outlived still writes. An OpenCode export leaves out the message
	// still being written, either way. The pack runs in a login shell with the
	// box's bin first on PATH, not the run's environment, which no row keeps.
	rec, packed, err := r.packNow(ctx, box, bound, func(paths Paths, rec sessionRecord) (string, error) {
		return carries.Pack(paths, rec.id, staged, dest, working), nil
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

// dropPack removes what a pack on demand or a save left for its read once the
// read is done: the archive kept for it ([Paths.SessionPackHeld],
// [Paths.SessionSave]) and the staging directory the pack's script removes
// itself unless cut short.
//
// It runs past ctx's cancellation, within its deadline ([releaseContext]): ctx
// may be what ended the read, and a cleanup on a dead context does nothing; but
// release is on the read's close path, so an unresponsive box holds the read no
// longer than its take's own time. Files not removed are logged and left to
// [Runner.ClearArtifacts] before the box's next run.
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
// — a local exec in milliseconds, a container's command in a few hundred —
// and past it the box is one that has stopped answering, whose
// leftovers the clear before its next run removes. It comes out of the half
// minute the take leaves its outcome stamp, a write the coordination store
// answers in milliseconds.
const packReleaseFloor = 5 * time.Second

// PackSave packs, NOW, a SAVE of the session the box's run is still working in,
// for taker (the lease holder's incarnation, `<node>:<uuid>`), by its harness's
// save pack ([saver]) into a place of that save's own ([Paths.SessionSave]),
// and describes it for the take that names it ([sandbox.SessionTake.Save]) to
// stream and remove. A zero description means nothing to save (no [saver], a
// record that names no session, a harness that holds none of it yet); an error
// says why it could not be made, wrapping [sandbox.ErrSaveNoRoom] where the box
// lacks room ([packNoRoom]), an [sandbox.UnrestorableError] where no box of
// this build continues the session ([Runner.packNow]), or that taker names
// none. Packed first so what the take's scrub reads (tokens renewed in the box)
// is read once the session is packed.
func (r *Runner) PackSave(ctx context.Context, box sandbox.Sandbox, taker, session string) (sandbox.SessionArchive,
	error) {
	saves, ok := r.harness.(saver)
	if !ok {
		return sandbox.SessionArchive{}, nil
	}
	if taker == "" {
		return sandbox.SessionArchive{}, errors.New("executor: a save names no taker, and a save is packed " +
			"to a place named after its taker")
	}
	// Never the job's final archive: [Paths.SessionCapture] is what every read
	// of a job that is over takes as its last, so a save moved there was
	// collected as the final archive of a job whose exit pack failed. Each save
	// has its own place and staging directory: with one path per taker, two
	// saves at once (the drain's and the waiter's) move over each other and
	// refuse a Claude Code take, which reopens its archive by path ([scrubTar]).
	// Held to the rule for a pack beside a working harness ([claudeHeldTar]); a
	// failed save leaves nothing, and an untaken one is removed by
	// [Runner.ReleaseSave] or the clear before the next run.
	pack := uuid.NewString()
	paths := PathsFor(box)
	staged, dest := paths.SessionSaveStaging(taker, pack), paths.SessionSave(taker, pack)
	rec, packed, err := r.packNow(ctx, box, session, func(paths Paths, rec sessionRecord) (string, error) {
		return saves.PackSave(paths, rec, staged, dest)
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

// Saves reports whether this runner packs saves of a session its run still
// works in ([Runner.PackSave]): only a runner of a harness that is a [saver],
// Claude Code's and OpenCode's. Asked before anything else of a save, so a run
// on a harness that packs none costs its node no slot, no box call and no read
// of the store.
func (r *Runner) Saves() bool {
	_, ok := r.harness.(saver)
	return ok
}

// Streamed is how far the event stream of the job in box has run, in bytes:
// the size of the file the wrapper redirects the harness's stdout into
// ([Output.Stdout]), Claude Code's stream-json and OpenCode's event log, read
// by its size alone, as [Runner.archiveAt] reads an archive's. That stream,
// and not the session's transcript, because it is the one file every step of
// the run moves: a subagent's calls and results are printed on it too, where
// they reach no line of the main thread's transcript, so a session whose only
// work is a subagent's still reads as moved. Zero where the job has written
// none, a missing file reading as empty.
func (r *Runner) Streamed(ctx context.Context, box sandbox.Sandbox) (int64, error) {
	tail, err := box.ReadTail(ctx, r.harness.Output(PathsFor(box)).Stdout, 0)
	if err != nil {
		return 0, fmt.Errorf("executor: reading how far %s's event stream has run: %w", r.harness.Name(), err)
	}
	return tail.Size, nil
}

// ReleaseSave removes the save at path ([Runner.PackSave]) from box, for a
// save no take will read: one its saver gave up on between its pack and its
// take, which no take's release removes. Within the bound a take's release has
// ([Runner.dropPack]), past ctx's cancellation, which is often what gave the
// save up; a box that does not answer it leaves the file to the clear before
// its next run, said in the log. A path that is no save of the box's
// ([savePath]) is refused and nothing is removed: a release removes one save's
// archive, never another file of the box.
func (r *Runner) ReleaseSave(ctx context.Context, box sandbox.Sandbox, path string) error {
	if !savePath(PathsFor(box), path) {
		return fmt.Errorf("executor: %q is no save a runner packed in the box, and a release removes a save "+
			"and nothing else", path)
	}
	r.dropPack(ctx, box, path)
	return nil
}

// openSave streams the save at path ([Runner.PackSave], [Paths.SessionSave]),
// described, as [Runner.OpenCapture] streams the job's capture, with release,
// which removes it once its take is done with it — a nil reader and a zero
// description where nothing is there (a save already taken) or the record
// names no session, the save removed either way. A path that is no save of
// the box's ([savePath]) is refused unread: a take of a save streams that save
// and no other file of the box.
func (r *Runner) openSave(ctx context.Context, box sandbox.Sandbox, path, bound string) (sandbox.SessionArchive,
	io.ReadCloser, func(context.Context), error) {

	paths := PathsFor(box)
	if !savePath(paths, path) {
		return sandbox.SessionArchive{}, nil, nil, fmt.Errorf("executor: %q is no save a runner packed in the "+
			"box, and a take of a save streams one and nothing else", path)
	}
	release := func(ctx context.Context) { r.dropPack(ctx, box, path) }
	rec, refusal, err := r.readSessionRecord(ctx, box, paths, bound)
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
// after its harness — for the session the box's record names, handed that
// record as the box holds it, in a login shell with the box's bin directory
// first on its PATH as the run's wrapper runs it, within
// [sandbox.SessionPackWait]: packed, where the pack made its archive; not, with
// no error, where there is nothing to pack — a record that names no session, a
// harness that wrote no transcript ([packNothing]); and an error where no box
// of this build continues the session the record names
// ([sandbox.UnrestorableError]) or pack refused the record, each before any
// script ran, saying why, where the pack failed,
// with what it said ([packFailed]), or where a save's box lacked the room to
// pack it, wrapping [sandbox.ErrSaveNoRoom] ([packNoRoom]).
func (r *Runner) packNow(ctx context.Context, box sandbox.Sandbox, bound string,
	pack func(paths Paths, rec sessionRecord) (string, error)) (sessionRecord, bool, error) {

	paths := PathsFor(box)
	rec, refusal, err := r.readSessionRecord(ctx, box, paths, bound)
	if err != nil {
		return sessionRecord{}, false, fmt.Errorf("executor: reading which session the run worked in: %w", err)
	}
	if refusal != "" || rec.id == "" {
		return sessionRecord{}, false, nil
	}
	// A SESSION NO BOX CONTINUES IS NOT PACKED, its release read off the record.
	if refused := r.unrestorable(r.recorded(rec)); refused != nil {
		return sessionRecord{}, false, refused
	}
	packs, err := pack(paths, rec)
	if err != nil {
		return sessionRecord{}, false, err
	}
	script := "PATH=" + shellword.Quote(paths.BinDir()) + `:"$PATH"; export PATH` + "\n" + packs
	res, err := box.Exec(ctx, "sh -lc "+shellword.Quote(script),
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
	case packNoRoom:
		return sessionRecord{}, false, fmt.Errorf("%w: %s", sandbox.ErrSaveNoRoom,
			firstLine(strings.TrimSpace(res.Stderr), packDetailLimit))
	}
	return sessionRecord{}, false, fmt.Errorf("executor: %s's session %s could not be packed (exit %d): %s",
		r.harness.Name(), rec.id, res.ExitCode, firstLine(strings.TrimSpace(res.Stderr), packDetailLimit))
}

// readSessionRecord is the box's record of the session its run worked in
// ([Paths.SessionRecord]), read and parsed — or why it could not be read, a
// refusal of a path that holds no file the engine reads, which names no
// session. A minting harness's session is the one bound to its job, bound,
// held to what its box says ([Runner.boundRecord]).
func (r *Runner) readSessionRecord(ctx context.Context, box sandbox.Sandbox, paths Paths,
	bound string) (sessionRecord, string, error) {

	rec, refusal, err := readRecordedSession(ctx, box, paths)
	if err != nil || refusal != "" || !r.MintsSession() {
		return rec, refusal, err
	}
	rec, err = r.boundRecord(ctx, box, paths, rec, bound)
	return rec, "", err
}

// readRecordedSession is [Runner.readSessionRecord] as the box records it,
// whoever's harness minted the id it names.
func readRecordedSession(ctx context.Context, box sandbox.Sandbox, paths Paths) (sessionRecord, string, error) {
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
// session packed: packNothing, a session there is nothing of to pack (no
// transcript its harness wrote, no session its harness holds); packFailed,
// one there is and the pack could not make, its error stream saying why; and
// packNoRoom, a save its box lacks the memory to pack now ([openCodeSaveRoom]).
const (
	packFailed  = 2
	packNothing = 3
	packNoRoom  = 4
)

// packDigest is the part of every pack (the run's own, on demand, a save) that
// records the digest of the archive at staged/capture as one of the session the
// shell variable session names that the box's copy holds
// ([Paths.SessionDigests]), and reads the line back, BEFORE the archive is moved
// to where its read reads it; failing, its reason goes to staged/error.
// A pack reports its archive made only once its digest is recorded, so the box
// never restores an archive of its own over a newer copy (carry.go). Recording
// after the move left, on a full disk, an archive in place with no digest. Read
// back, never taken on the append's word: a write a full disk lost can be one
// its writer was told landed ([claudeHeldTar]'s lists are read back likewise).
// A digest kept for an archive then not moved is harmless. A box without cksum
// or grep packs nothing.
func packDigest(paths Paths, session, staged string) string {
	q := shellword.Quote
	return fmt.Sprintf(`{ d=$(cksum < %[1]s 2> /dev/null) && %[2]s || { printf '%%s\n' %[3]s > %[4]s 2> /dev/null; false; }; }`,
		q(staged+"/capture"), recordDigest(paths, session), q(digestUnrecorded), q(staged+"/error"))
}

// recordDigest is the part of a box's script that records the line `<session>
// <digest>` — the values of the shell variable session names and of d, which
// every caller's cksum fills — in the box's list of the archives its copy
// holds ([Paths.SessionDigests]) and reads it back: true only where the digest
// is not empty and the line is there after. READ BACK, never taken on the append's word: a write a full
// disk lost can be one its writer was told landed. What every pack records
// before it moves its archive into place ([packDigest]), and every restore
// before its run continues what it restored ([claudeSessionGate],
// [openCodeSessionGate]).
func recordDigest(paths Paths, session string) string {
	return fmt.Sprintf(`[ -n "$d" ] && { printf '%%s %%s\n' "$%[1]s" "$d" >> %[2]s; } 2> /dev/null && grep -qxF "$%[1]s $d" %[2]s 2> /dev/null`,
		session, shellword.Quote(paths.SessionDigests()))
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
	res, err := box.Exec(ctx, "printf '%s\\n' "+shellword.Quote(session+" "+digest)+" >> "+
		shellword.Quote(paths.SessionDigests()), sandbox.ExecOptions{})
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
