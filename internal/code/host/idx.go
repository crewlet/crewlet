package host

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/crewlet/crewlet/internal/code"
)

// A PACK INDEX, READ FOR ITS OBJECTS: the ids of the push's own pack, which the
// blob scan asks git about — so the scratch's alternate, the whole repository,
// is never listed.

// idxOIDs streams the object ids of a version-2 pack index — the only version
// git writes by default since 1.5.2, and the one the receive configuration
// pins — as `<hex>\n` lines, and reports the object count, read from the
// fan-out table's last entry. The header is code.OpenIdx's to read, the one
// reading of an index's header, which refuses a version-1 index, a bad magic
// or a count above 1<<31 naming the file; the ids follow it. Nothing is held
// whole: the ids are read through a 64 KiB buffer as the caller reads them.
func idxOIDs(path string) (count int64, ids io.ReadCloser, err error) {
	count, f, err := code.OpenIdx(path)
	if err != nil {
		return 0, nil, err
	}
	return count, &idxReader{f: f, r: bufio.NewReaderSize(f, 64<<10), left: count}, nil
}

// idxReader hands out an index's ids as hex lines, an id at a time.
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
			return n, fmt.Errorf("host: read the pack index %s's ids: %w", x.f.Name(), err)
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
