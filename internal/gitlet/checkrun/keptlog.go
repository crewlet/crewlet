package checkrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// errLogPastBound is a kept log's redaction past the bound it is kept to.
var errLogPastBound = errors.New("checkrun: the log's redaction is past its bound")

// errLogUnsettled is a log whose context left no line settled, or whose head's
// redaction was not the stream's.
var errLogUnsettled = errors.New("checkrun: the log's start is not settled")

// errLogChanged is a log whose output was cut, grown or written over while it
// was read ([sandbox.ChangeOf]), or read short of the size its open found:
// what was read may join two versions of it, so none of it is kept.
var errLogChanged = errors.New("checkrun: the log's output changed while it was read")

// errLogNoLine is a log whose redaction, dropped to its bound, holds no line.
var errLogNoLine = errors.New("checkrun: the log's bound holds no line")

// keepLog stores the end of the run's output, redacted, within
// MaxCheckLogBytes as one object through the runner's Logs (the package doc's
// "What leaves the box"), answering what the attempt's code_check_logs row
// names it by: nil and "" for no output or no store, nil and why for a log
// not stored.
func (a *attempt) keepLog(ctx context.Context, box sandbox.Sandbox) (*gitlet.CheckLog, string) {
	return a.keepLogWithin(ctx, box, gitlet.MaxCheckLogBytes)
}

// keepLogWithin is keepLog held to limit bytes, so a case can keep a log of a
// few MiB rather than redact gitlet.MaxCheckLogBytes.
func (a *attempt) keepLogWithin(ctx context.Context, box sandbox.Sandbox, limit int64) (*gitlet.CheckLog,
	string) {
	r := a.r
	if r.o.Logs == nil {
		return nil, ""
	}
	n := limit + redact.LookBack
	l, size, err := openLog(ctx, box, n, limit, 0)
	if err == nil && size == 0 {
		return nil, ""
	}
	var o objstore.Object
	if err == nil {
		o, err = a.putLog(ctx, l)
	}
	if errors.Is(err, errLogPastBound) {
		var whole int64
		if whole, err = l.rest(); err == nil {
			o, err = a.keepAgain(ctx, box, n, limit, whole-limit, size)
		}
	}
	if l != nil {
		_ = l.Close()
	}
	if why := a.logUnkept(ctx, limit, err); why != "" {
		return nil, why
	}
	return &gitlet.CheckLog{Object: o.Key, Hash: o.Hash, Size: o.Size}, ""
}

// keepAgain reads the last n bytes of an output of size bytes again, and puts
// its redaction into the runner's Logs past over bytes more than the first
// read dropped, from the next line's start.
func (a *attempt) keepAgain(ctx context.Context, box sandbox.Sandbox, n, limit, over, size int64) (
	objstore.Object, error) {
	l, again, err := openLog(ctx, box, n, limit, over)
	if err != nil {
		return objstore.Object{}, err
	}
	defer func() { _ = l.Close() }()
	if again != size {
		return objstore.Object{}, logReadError{fmt.Errorf("its output went from %d to %d bytes while it was read",
			size, again)}
	}
	return a.putLog(ctx, l)
}

// putLog puts l into the runner's Logs within l's bound.
func (a *attempt) putLog(ctx context.Context, l *logReader) (objstore.Object, error) {
	return a.r.o.Logs.Put(ctx, l, l.limit, objstore.PutMeta{ContentType: logContentType})
}

// logUnkept is why a log kept to limit was not kept for err, "" for none.
func (a *attempt) logUnkept(ctx context.Context, limit int64, err error) string {
	r := a.r
	switch {
	case err == nil:
		return ""
	case errors.Is(err, objstore.ErrFull):
		return "its log was not kept: the object store is full"
	case errors.Is(err, redact.ErrDecidedEarly):
		r.withheld(withheldLog, sandbox.WithheldRefused)
		return "its log was not kept: its text could not be redacted in one pass"
	case errors.Is(err, errLogUnsettled):
		r.withheld(withheldLog, sandbox.WithheldUnsettled)
		return "its log was not kept: no line of its output's end could be redacted as the whole output is"
	case errors.Is(err, errLogChanged):
		r.withheld(withheldLog, sandbox.WithheldChanged)
		return "its log was not kept: its output changed while it was read"
	case errors.Is(err, errLogNoLine):
		r.withheld(withheldLog, sandbox.WithheldTooLarge)
		return fmt.Sprintf("its log was not kept: its last line, redacted, is past the %d MiB a log keeps",
			limit>>20)
	}
	if re := (logReadError{}); errors.As(err, &re) {
		return "its log could not be read: " + re.Error()
	}
	r.log.WarnContext(ctx, "code_check_log_unkept", "repo_id", a.c.RepoID, "check", a.c.Name,
		"attempt", a.c.Attempt, "error", err.Error())
	return "its log could not be stored"
}

// The pieces of a check's output its runner may withhold
// ([metrics.CodeChecksWithheld]).
const (
	withheldLog  = "log"
	withheldTail = "tail"
)

// withheld counts a piece of a check's output the runner did not keep or show,
// and why, in the reasons a box read of a run's gives ([sandbox.WithheldReason]).
func (r *Runner) withheld(piece string, reason sandbox.WithheldReason) {
	r.o.Metrics.Add(metrics.CodeChecksWithheld, 1, metrics.Attrs{"piece": piece, "reason": string(reason)})
}

// logReadError is a log the box would not hand over, saying why.
type logReadError struct{ err error }

func (e logReadError) Error() string { return e.err.Error() }
func (e logReadError) Unwrap() error { return e.err }

// openLog opens the last n bytes of the run's output as a log kept to limit
// bytes, over dropped bytes of its redaction past where the log begins (to the
// next line's start), and answers the output's size; a nil reader where it is
// empty.
func openLog(ctx context.Context, box sandbox.Sandbox, n, limit, over int64) (*logReader, int64, error) {
	rc, size, err := box.OpenTail(ctx, outputFile, n)
	if err != nil {
		return nil, 0, logReadError{err}
	}
	if size == 0 {
		_ = rc.Close()
		return nil, 0, nil
	}
	l := &logReader{rc: rc, src: rc, limit: limit, drop: over, toLine: over > 0, piece: make([]byte, 32<<10),
		expect: min(n, size)}
	if size > n {
		head := make([]byte, min(n, 2*redact.LookBack))
		k, err := io.ReadFull(rc, head)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			_ = rc.Close()
			return nil, 0, logReadError{err}
		}
		head = head[:k]
		end, _ := redact.SettledEnd(string(head), redact.LookBack)
		if end == "" {
			_ = rc.Close()
			return nil, 0, errLogUnsettled
		}
		whole := redact.Secrets(string(head))
		l.want = []byte(whole[:len(whole)-len(end)])
		l.drop += int64(len(l.want))
		l.src = io.MultiReader(bytes.NewReader(head), rc)
	}
	l.s = redact.NewStream(&l.out)
	return l, size, nil
}

// logReader is a run's output read as its kept log: redacted by s, the
// redaction dropped up to where the log begins (drop bytes, to a line's start
// where toLine), the dropped bytes checked against want, and at most limit
// bytes handed on, past which it fails errLogPastBound; read whole, expect
// bytes of one version of the output, or it fails errLogChanged.
type logReader struct {
	rc     io.ReadCloser
	src    io.Reader
	s      *redact.Stream
	out    bytes.Buffer
	piece  []byte
	ended  bool
	want   []byte
	drop   int64
	toLine bool
	// dropped is how many bytes of the redaction were dropped, atLine whether
	// the last dropped was a line's end.
	dropped       int64
	atLine        bool
	served, limit int64
	read, expect  int64
	err           error
	closed        bool
}

func (l *logReader) Read(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	for {
		if err := l.fill(); err != nil {
			l.err = err
			return 0, err
		}
		if err := l.dropHeld(); err != nil {
			l.err = err
			return 0, err
		}
		if l.out.Len() > 0 || l.ended {
			break
		}
	}
	switch {
	case l.out.Len() == 0 && l.served == 0:
		l.err = errLogNoLine
		return 0, l.err
	case l.out.Len() == 0:
		return 0, io.EOF
	case l.served >= l.limit:
		l.err = errLogPastBound
		return 0, l.err
	}
	k, _ := l.out.Read(p[:min(int64(len(p)), l.limit-l.served)])
	l.served += int64(k)
	return k, nil
}

// fill redacts the output on until the redaction holds something or the
// output has ended.
func (l *logReader) fill() error {
	for l.out.Len() == 0 && !l.ended {
		k, err := l.src.Read(l.piece)
		l.read += int64(k)
		if k > 0 {
			if _, werr := l.s.Write(l.piece[:k]); werr != nil {
				return werr
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			l.ended = true
			if uerr := l.unchanged(); uerr != nil {
				return uerr
			}
			return l.s.Close()
		case err != nil:
			return logReadError{err}
		}
	}
	return nil
}

// unchanged is nil where the output was read whole, as one version of it.
func (l *logReader) unchanged() error {
	change, err := sandbox.ChangeOf(l.rc)
	switch {
	case err != nil:
		return logReadError{err}
	case l.read != l.expect, change != sandbox.FileUnchanged:
		return errLogChanged
	}
	return nil
}

// dropHeld drops what the redaction holds before where the log begins.
func (l *logReader) dropHeld() error {
	for l.out.Len() > 0 && (l.dropped < l.drop || l.toLine && !l.atLine) {
		b, _ := l.out.ReadByte()
		if l.dropped < int64(len(l.want)) && b != l.want[l.dropped] {
			return errLogUnsettled
		}
		l.dropped++
		l.atLine = b == '\n'
	}
	return nil
}

// rest is how long the redaction is past where the log begins, read to its
// end: what was handed on and all that follows.
func (l *logReader) rest() (int64, error) {
	n := l.served + int64(l.out.Len())
	l.out.Reset()
	for !l.ended {
		if err := l.fill(); err != nil {
			return 0, err
		}
		n += int64(l.out.Len())
		l.out.Reset()
	}
	return n, nil
}

// Close closes the box's stream once.
func (l *logReader) Close() error {
	if l.closed {
		return nil
	}
	l.closed = true
	return l.rc.Close()
}
