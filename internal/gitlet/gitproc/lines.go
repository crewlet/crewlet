package gitproc

import (
	"bytes"
	"errors"
)

// MaxLine is the longest line [Lines] hands over, and the longest stderr line
// the classifier reads whole: 64 KiB. The longest line git writes for any
// command read by line is an object id with a path (rev-list --objects), and a
// path is at most PATH_MAX, 4 KiB on Linux; the ref grammar bounds a ref far
// below that.
const MaxLine = 64 << 10

// ErrPartialLine reports output that ended inside a line: git ends every line
// it writes, so a remainder is a process that died mid-line, which Run has
// already reported.
var ErrPartialLine = errors.New("gitproc: git's output ended inside a line")

// ErrLineTooLong reports a line past [MaxLine].
var ErrLineTooLong = errors.New("gitproc: git wrote a line longer than gitproc.MaxLine")

// Lines is a Cmd.Stdout that hands each complete line, without its "\n", to fn
// as it arrives; or, made by [NewRecords], each NUL-terminated record (-z, -Z).
//
// It is the shape for every output that grows with a push or a repository
// (cat-file --batch-check over a want list, show-index of a push's pack),
// which must never be captured ([CaptureMax]). fn's slice is valid only
// during the call.
//
// An error from fn ends the run: Write returns it, the pool closes the pipe
// under git, git exits on its next write, and Run answers [ErrOutput] wrapping
// it, so a scan stops at its first offender rather than reading on.
type Lines struct {
	sep     byte
	fn      func(line []byte) error
	pending []byte
	err     error
}

// NewLines is a Lines handing each line to fn.
func NewLines(fn func(line []byte) error) *Lines {
	return &Lines{sep: '\n', fn: fn}
}

// NewRecords is a Lines handing fn each record git ends with a NUL (-z, -Z),
// under every rule of [NewLines]: [MaxLine], [ErrLineTooLong], [ErrPartialLine].
func NewRecords(fn func(rec []byte) error) *Lines {
	return &Lines{sep: 0, fn: fn}
}

// Write splits p into lines, carrying a partial one into the next Write.
func (l *Lines) Write(p []byte) (int, error) {
	if l.err != nil {
		return 0, l.err
	}
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, l.sep)
		if i < 0 {
			if len(l.pending)+len(p) > MaxLine {
				l.err = ErrLineTooLong
				return 0, l.err
			}
			l.pending = append(l.pending, p...)
			break
		}
		line := p[:i]
		if len(l.pending) > 0 {
			if len(l.pending)+i > MaxLine {
				l.err = ErrLineTooLong
				return 0, l.err
			}
			l.pending = append(l.pending, line...)
			line = l.pending
		} else if i > MaxLine {
			l.err = ErrLineTooLong
			return 0, l.err
		}
		if err := l.fn(line); err != nil {
			l.err = err
			return 0, err
		}
		l.pending = l.pending[:0]
		p = p[i+1:]
	}
	return n, nil
}

// Close reports output that did not end in its separator ([ErrPartialLine]) or the
// first error fn returned. Call it after Run.
func (l *Lines) Close() error {
	if l.err != nil {
		return l.err
	}
	if len(l.pending) > 0 {
		return ErrPartialLine
	}
	return nil
}
