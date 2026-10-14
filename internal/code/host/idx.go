package host

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// A PACK INDEX, READ FOR ITS OBJECTS: the ids of the push's own pack, which the
// blob scan asks git about — so the scratch's alternate, the whole repository,
// is never listed.

// idxMagic is a version-2 index's first four bytes; idxV2 its version.
var idxMagic = []byte{0xff, 't', 'O', 'c'}

const idxV2 = 2

// idxOIDs streams the object ids of a version-2 pack index — the only version
// git writes by default since 1.5.2, and the one the receive configuration
// pins — as `<hex>\n` lines, and reports the object count, read from the
// fan-out table's last entry. A version-1 index, a bad magic or a count above
// 1<<31 is refused naming the file. Nothing is held whole: the ids are read
// through a 64 KiB buffer as the caller reads them.
func idxOIDs(path string) (count int64, ids io.ReadCloser, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, fmt.Errorf("host: open the pack index %s: %w", path, err)
	}
	head := make([]byte, 8+256*4)
	if _, err := io.ReadFull(f, head); err != nil {
		_ = f.Close()
		return 0, nil, fmt.Errorf("host: read the pack index %s's header: %w", path, err)
	}
	if !bytes.Equal(head[:4], idxMagic) || binary.BigEndian.Uint32(head[4:8]) != idxV2 {
		_ = f.Close()
		return 0, nil, fmt.Errorf("host: %s is not a version-2 pack index", path)
	}
	n := binary.BigEndian.Uint32(head[8+255*4:])
	if n > 1<<31 {
		_ = f.Close()
		return 0, nil, fmt.Errorf("host: the pack index %s counts %d objects, past 1<<31", path, n)
	}
	return int64(n), &idxReader{f: f, r: bufio.NewReaderSize(f, 64<<10), left: int64(n)}, nil
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
