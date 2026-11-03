package codeindex

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
)

// A TIP MAP: one immutable file naming, for one tip of a repository, every
// file the tip holds and the blob at it — which of a segment's postings count
// at that tip, since a posting names a blob and never a path. Big-endian
// throughout:
//
//	header   "CRCT" | version u8 (1) | oid bytes u8 (20) | reserved u16 | entries u32 |
//	         reserved u32 | path bytes u64 | tip [20] | reserved u32           (48 bytes)
//	entries  entries × (path offset u64 | path length u32 | flags u8 | reserved [3] | oid [20])
//	                                                  ascending by path, byte for byte; no path twice
//	paths    path bytes — the entries' paths back to back, in the entries' order
//	trailer  SHA-256 of every byte before it [32]
//
// A path is the file's from the repository's root as git names it: any bytes
// but NUL, so not always UTF-8. Flag 1 is the file's blob SKIPPED (Skip), so
// a search counts the file among those it did not read; any other bit is a
// file this build did not write. tip is the commit the map was read at, the
// one a search answers about.

// The format's fixed parts.
const (
	tipMagic      = "CRCT"
	tipVersion    = 1
	tipHeader     = 48
	tipEntryBytes = 36
	tipSkipped    = 1 << 0
)

// TipEntry is one file of a tip: its path, its blob, and whether that blob is
// skipped.
type TipEntry struct {
	Path    string
	Blob    OID
	Skipped bool
}

// WriteTipMap writes to w the tip map of the commit tip holding entries, in
// any order: each path once, none empty or holding a NUL.
func WriteTipMap(w io.Writer, tip OID, entries []TipEntry) error {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b TipEntry) int { return cmp.Compare(a.Path, b.Path) })
	var pathBytes uint64
	for i, e := range sorted {
		switch {
		case e.Path == "" || strings.IndexByte(e.Path, 0) >= 0:
			return fmt.Errorf("codeindex: the path %q names no file git can hold", e.Path)
		case uint64(len(e.Path)) > math.MaxUint32:
			return fmt.Errorf("codeindex: a path of %d bytes, past the %d a tip map holds", len(e.Path),
				uint32(math.MaxUint32))
		case i > 0 && sorted[i-1].Path == e.Path:
			return fmt.Errorf("codeindex: the path %q twice in one tip", e.Path)
		}
		pathBytes += uint64(len(e.Path))
	}
	if uint64(len(sorted)) > math.MaxUint32 {
		return fmt.Errorf("codeindex: %d files, past the %d a tip map holds", len(sorted), uint32(math.MaxUint32))
	}
	h := sha256.New()
	out := bufio.NewWriterSize(io.MultiWriter(w, h), 64<<10)
	header := make([]byte, tipHeader)
	copy(header, tipMagic)
	header[4], header[5] = tipVersion, oidBytes
	binary.BigEndian.PutUint32(header[8:], uint32(len(sorted)))
	binary.BigEndian.PutUint64(header[16:], pathBytes)
	copy(header[24:], tip[:])
	if _, err := out.Write(header); err != nil {
		return err
	}
	var entry [tipEntryBytes]byte
	var off uint64
	for _, e := range sorted {
		clear(entry[:])
		binary.BigEndian.PutUint64(entry[:], off)
		binary.BigEndian.PutUint32(entry[8:], uint32(len(e.Path)))
		if e.Skipped {
			entry[12] = tipSkipped
		}
		copy(entry[16:], e.Blob[:])
		if _, err := out.Write(entry[:]); err != nil {
			return err
		}
		off += uint64(len(e.Path))
	}
	for _, e := range sorted {
		if _, err := out.WriteString(e.Path); err != nil {
			return err
		}
	}
	if err := out.Flush(); err != nil {
		return err
	}
	_, err := w.Write(h.Sum(nil))
	return err
}

// TipMap is a tip map's bytes as a search reads them: a mapped file, or bytes
// in memory. Its trailer is the caller's to have checked — the cache's
// run-trailer check — and OpenTipMap holds every entry to its length, so no
// read lands outside it.
type TipMap struct {
	tip     OID
	count   int
	entries []byte
	paths   []byte
}

// OpenTipMap is data, a whole tip map, as a search reads it.
func OpenTipMap(data []byte) (*TipMap, error) {
	if len(data) < tipHeader+trailerBytes || string(data[:4]) != tipMagic {
		return nil, fmt.Errorf("%w: no CRCT header", ErrMalformed)
	}
	if data[4] != tipVersion || data[5] != oidBytes {
		return nil, fmt.Errorf("%w: version %d of %d-byte ids, and this build writes version %d of %d",
			ErrMalformed, data[4], data[5], tipVersion, oidBytes)
	}
	if binary.BigEndian.Uint16(data[6:]) != 0 || binary.BigEndian.Uint32(data[12:]) != 0 ||
		binary.BigEndian.Uint32(data[44:]) != 0 {
		return nil, fmt.Errorf("%w: a reserved field of the header is set", ErrMalformed)
	}
	count := uint64(binary.BigEndian.Uint32(data[8:]))
	pathBytes := binary.BigEndian.Uint64(data[16:])
	size := uint64(len(data))
	entriesEnd := tipHeader + count*tipEntryBytes
	if pathBytes > size || entriesEnd+pathBytes+trailerBytes != size {
		return nil, fmt.Errorf("%w: its sections do not add up to its %d bytes", ErrMalformed, len(data))
	}
	m := &TipMap{count: int(count), entries: data[tipHeader:entriesEnd],
		paths: data[entriesEnd : entriesEnd+pathBytes]}
	copy(m.tip[:], data[24:])
	// The paths lie back to back in the entries' order, so each one's
	// offset is the lengths before it; they rise, and none is empty.
	var off uint64
	for i := range m.count {
		e := m.entries[i*tipEntryBytes:]
		start, n := binary.BigEndian.Uint64(e), uint64(binary.BigEndian.Uint32(e[8:]))
		switch {
		case start != off || n == 0 || off+n > pathBytes:
			return nil, fmt.Errorf("%w: entry %d's path is not where its entries say", ErrMalformed, i)
		case e[12]&^tipSkipped != 0 || e[13] != 0 || e[14] != 0 || e[15] != 0:
			return nil, fmt.Errorf("%w: entry %d is flagged %#x", ErrMalformed, i, e[12])
		}
		off += n
		if i > 0 && bytes.Compare(m.Path(i-1), m.Path(i)) >= 0 {
			return nil, fmt.Errorf("%w: its paths do not rise", ErrMalformed)
		}
	}
	if off != pathBytes {
		return nil, fmt.Errorf("%w: path bytes no entry names", ErrMalformed)
	}
	return m, nil
}

// Tip is the commit m was read at.
func (m *TipMap) Tip() OID { return m.tip }

// Len is how many files m names.
func (m *TipMap) Len() int { return m.count }

// Path is entry i's path. It is m's own bytes: valid while they are, and
// never written to.
func (m *TipMap) Path(i int) []byte {
	e := m.entries[i*tipEntryBytes:]
	start := binary.BigEndian.Uint64(e)
	return m.paths[start : start+uint64(binary.BigEndian.Uint32(e[8:]))]
}

// Blob is the blob at entry i's path.
func (m *TipMap) Blob(i int) OID {
	var o OID
	copy(o[:], m.entries[i*tipEntryBytes+16:])
	return o
}

// Skipped reports whether entry i's blob is skipped: past MaxFileBytes, or
// binary.
func (m *TipMap) Skipped(i int) bool { return m.entries[i*tipEntryBytes+12]&tipSkipped != 0 }
