package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A run's session, TAKEN out of its box to be kept (sandbox.SessionTaker).
//
// TWO ROADS IN, ONE OUT. The archive a run's script packed when its harness
// exited ([Runner.OpenCapture]) is taken as it is; a job that never got to pack
// — stopped, cut off at its turn's cap while it still ran, lost with its engine
// with its box left behind — is packed now, by the very pack its script runs
// ([Runner.PackCapture]). Either way the archive leaves the box through ONE
// scrub ([scrubArchive]): every value the engine put in the box taken out, and
// every file redacted, each read and written as what it is, so what is kept is
// the session minus the secrets and still the archive its harness restores.
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
// keeps it ([sandbox.SessionTaker]): packed now where take says so, scrubbed of
// take's values and redacted, a piece at a time. The zero value where the box
// holds no session: a harness this runner carries none for, a run whose record
// names none, or one whose harness wrote nothing to pack.
func (r *Runner) TakeSession(ctx context.Context, box sandbox.Sandbox,
	take sandbox.SessionTake) (sandbox.TakenSession, error) {

	carries, ok := r.harness.(carrier)
	if !ok {
		return sandbox.TakenSession{}, nil
	}
	open := r.OpenCapture
	if take.Pack {
		open = r.PackCapture
	}
	archive, body, err := open(ctx, box)
	if err != nil || body == nil {
		return sandbox.TakenSession{}, err
	}
	scrub := newScrubber(take.Scrub)
	read, write := io.Pipe()
	out := &takenBody{PipeReader: read, done: make(chan struct{})}
	if !out.reading(body) {
		_ = body.Close()
	}
	sum := &cksum{}
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
		scrubErr := scrubArchive(carries.layout(), body, reopen, io.MultiWriter(write, sum), scrub)
		out.finished(scrubErr, sum.String())
		_ = write.CloseWithError(scrubErr)
	}()
	paths := PathsFor(box)
	keep := func(ctx context.Context) error {
		digest, digestErr := out.digest()
		if digestErr != nil {
			return digestErr
		}
		return keepDigest(ctx, box, paths, archive.SessionID, digest)
	}
	return sandbox.TakenSession{Archive: archive, Body: out, Keep: keep}, nil
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
// pack its harness's script makes once the harness exits ([carrier.Pack]), into
// [Paths.SessionCapture], its digest kept as one the box's copy holds — and
// streams it, described, as [Runner.OpenCapture] streams the one the script
// left. For a job that never reached that pack: one a person stopped, one its
// turn's cap ended while it still ran, one lost with its engine whose box is
// still there. A zero description and a nil reader where there is nothing to
// pack: a harness this runner carries none for, a run whose record names no
// session, a harness that wrote no transcript.
//
// THE RUNNER'S OWN PACK, NEVER ONE A CALLER SCRIPTS: every pack of a box's
// session keeps its digest there, or the box later takes its own archive for
// another box's and restores it over the copy that went on past it (carry.go).
//
// A JOB STILL RUNNING is packed as it stands: a transcript's last line may be
// one its harness had not finished writing, and an OpenCode export leaves out
// the message still being written. A pack the box could not make — no tar or
// gzip, an OpenCode that would not export — is said on the log, and the
// archive the box holds, if any, is the one streamed: the run's script's own,
// packed at its harness's exit, which [Runner.ClearArtifacts] cleared from
// every run before this one.
//
// THE BOX'S OWN ENVIRONMENT: the pack runs in a login shell with the box's bin
// directory first on its PATH, as the run's wrapper runs, but not with the
// run's environment, which no row keeps; a harness the seat's environment
// moved somewhere its image's profile does not is a pack that finds nothing.
func (r *Runner) PackCapture(ctx context.Context, box sandbox.Sandbox) (sandbox.SessionArchive, io.ReadCloser, error) {
	carries, ok := r.harness.(carrier)
	if !ok {
		return sandbox.SessionArchive{}, nil, nil
	}
	paths := PathsFor(box)
	recorded, refusal, err := readWhole(ctx, box, paths.SessionRecord(), "the record of the session its harness ran in")
	if err != nil {
		return sandbox.SessionArchive{}, nil, fmt.Errorf("executor: reading which session the run worked in: %w", err)
	}
	if refusal != "" {
		return sandbox.SessionArchive{}, nil, nil
	}
	rec := parseSessionRecord(recorded)
	if rec.id == "" {
		return sandbox.SessionArchive{}, nil, nil
	}
	script := "PATH=" + shellQuote(paths.BinDir()) + `:"$PATH"; export PATH` + "\n" + carries.Pack(paths, rec.id)
	res, err := box.Exec(ctx, "sh -lc "+shellQuote(script),
		sandbox.ExecOptions{TimeoutSec: sandbox.SessionPackWait.Seconds()})
	if err != nil {
		return sandbox.SessionArchive{}, nil, fmt.Errorf("executor: packing %s's session %s: %w",
			r.harness.Name(), rec.id, err)
	}
	if said := strings.TrimSpace(res.Stderr); said != "" || res.ExitCode != 0 {
		log.WarnContext(ctx, "harness_session_pack_said", "harness", r.harness.Name(), "session", rec.id,
			"exit", res.ExitCode, "said", firstLine(said, transcriptDetailLimit),
			"detail", "the session was packed on demand and the pack said something, which is "+
				"usually why there is no archive to keep; the archive the box holds, if any, is kept")
	}
	return r.OpenCapture(ctx, box)
}

// keepDigest records digest — `<crc> <length>`, as POSIX cksum prints it for its
// standard input — as the digest of an archive of session the box's own copy
// holds ([Paths.SessionDigests]): the line a run's script writes for an archive
// it packed or restored, written here for the archive a capture kept.
func keepDigest(ctx context.Context, box sandbox.Sandbox, paths Paths, session, digest string) error {
	if !validSessionID(session) {
		return fmt.Errorf("executor: %q is not a session id a digest may be kept under", session)
	}
	res, err := box.Exec(ctx, "printf '%s\\n' "+shellQuote(session+" "+digest)+" >> "+
		shellQuote(paths.SessionDigests()), sandbox.ExecOptions{})
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
