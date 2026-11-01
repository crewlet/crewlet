package mirror

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/crewlet/crewlet/internal/code"
)

// openIdxOIDs is every object id of the pack index at path, as hex lines, an
// id at a time — what a `cat-file --batch-check` over one output reads as its
// standard input, so the scratch's alternate, the whole repository, is never
// listed. The header is read by code.OpenIdx, the one reading of it; nothing
// is held whole.
func openIdxOIDs(path string) (io.ReadCloser, error) {
	count, f, err := code.OpenIdx(path)
	if err != nil {
		return nil, err
	}
	return &idxReader{f: f, r: bufio.NewReaderSize(f, 64<<10), left: count}, nil
}

// idxReader hands out an index's ids as hex lines.
type idxReader struct {
	f    *os.File
	r    *bufio.Reader
	left int64
	line []byte   // what of the current line a short Read did not take
	buf  [41]byte // the current line: forty hex digits and a newline
}

func (x *idxReader) Read(p []byte) (int, error) {
	n := 0
	var raw [20]byte
	for n < len(p) {
		if len(x.line) > 0 {
			k := copy(p[n:], x.line)
			x.line, n = x.line[k:], n+k
			continue
		}
		if x.left == 0 {
			break
		}
		if _, err := io.ReadFull(x.r, raw[:]); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return n, fmt.Errorf("mirror: read the pack index %s's ids: %w", x.f.Name(), err)
		}
		x.left--
		hex.Encode(x.buf[:40], raw[:])
		x.buf[40] = '\n'
		x.line = x.buf[:]
	}
	if n == 0 && x.left == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (x *idxReader) Close() error { return x.f.Close() }
