package cache

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TAG INDEX (DerivedTags), crewlet/tags: every tag in git's creatordate
// order and by the commit each peels to, levelled inside writeRefs and renamed
// in with packed-refs in one hold (doc.go's "The tag index").

// tagsFile writes the tag index: its declaration's Writer.File.
const tagsFile = "internal/gitlet/cache/tags.go"

// tagsRel is the tag index, relative to its incarnation.
const tagsRel = engineRoot + "/tags"

// Format tags/1, big-endian, written front to back: head, records in listing
// order, u32 offsets (one a record), commit records, counts and trailer. The
// same tags give the same bytes however the file was built.
const (
	// tagsMagic and tagsFormat are the head's eight bytes.
	tagsMagic     = "CTAG"
	tagsFormat    = 1
	tagsHeadBytes = 8
	// tagsCountsBytes is u32 n (records), u32 R (their bytes) and u32 m
	// (commit records); the SHA-256 of every byte before it follows.
	tagsCountsBytes = 12
	tagsTailBytes   = tagsCountsBytes + sha256.Size
	// tagFixedBytes is a record before its name: [20] object, [20] peeled, u8
	// types (object's nibble high, peeled's low; git's 1 to 4), u8 flags (bit 0
	// dated), i64 date, u8 L; then the L bytes after refs/tags/.
	tagFixedBytes = 51
	// tagCommitBytes is a commit record: [20] commit, u32 record index, sorted.
	tagCommitBytes = 24
	tagRefPrefix   = "refs/tags/"
	// tagNameMax is the longest name after refs/tags/ a ref may have.
	tagNameMax = gitref.MaxRefNameBytes - len(tagRefPrefix)
	// tagRecordMax is a record at its longest.
	tagRecordMax = tagFixedBytes + tagNameMax
	// tagsEmptyBytes is an index of no tag.
	tagsEmptyBytes = tagsHeadBytes + tagsTailBytes
	// tagRowsChunk is the records a listing reads in one span.
	tagRowsChunk = 64
)

// errTagsMalformed is a tag index this cache did not write: its head, its
// sizes, a record, its order or its trailer is not the format's.
var errTagsMalformed = errors.New("cache: the tag index is not a file this cache wrote")

// git's numbers for the object types, which a record's type nibbles hold.
var tagTypeNames = [...]gitlet.ObjectType{1: gitlet.ObjectCommit, 2: gitlet.ObjectTree, 3: gitlet.ObjectBlob,
	4: gitlet.ObjectTag}

// typeCode is t's number in a record, 0 for no type git names.
func typeCode(t gitlet.ObjectType) byte {
	for i, n := range tagTypeNames {
		if n != "" && n == t {
			return byte(i)
		}
	}
	return 0
}

const (
	typeCommit = 1
	typeTag    = 4
)

// TagKey is a tag's place in the listing: Date descending, then Ref (whole)
// ascending; an undated tag's Date is 0.
type TagKey struct {
	Date int64
	Ref  string
}

// TagEntry is one tag of the index.
type TagEntry struct {
	Ref, OID, Peeled string
	Type, PeeledType gitlet.ObjectType
	Date             int64
	Dated            bool
}

// tagFacts is what the index keeps of a tag's object: a function of its id.
type tagFacts struct {
	peeled    [20]byte
	typ, ptyp byte
	dated     bool
	date      int64
}

// tagRecord is one record as read: its name is the buffer it was read from.
type tagRecord struct {
	obj   [20]byte
	facts tagFacts
	name  []byte
}

// compareTagKeys orders two tags as the listing does: the later date first,
// then the name (after refs/tags/, so the whole ref) ascending.
func compareTagKeys[A, B ~string | ~[]byte](ad int64, an A, bd int64, bn B) int {
	switch {
	case ad > bd:
		return -1
	case ad < bd:
		return 1
	}
	return compareNames(an, bn)
}

// compareNames compares two names byte by byte, as strings.Compare does.
func compareNames[A, B ~string | ~[]byte](a A, b B) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// decodeTagRecord reads the record at the start of b, holding it to the
// format but for its name's ref grammar (validTagName): its length too.
func decodeTagRecord(b []byte) (tagRecord, int, error) {
	if len(b) < tagFixedBytes {
		return tagRecord{}, 0, fmt.Errorf("%w: a record of %d bytes", errTagsMalformed, len(b))
	}
	l := int(b[50])
	if l < 1 || l > tagNameMax || len(b) < tagFixedBytes+l {
		return tagRecord{}, 0, fmt.Errorf("%w: a record naming %d bytes in %d", errTagsMalformed, l,
			len(b)-tagFixedBytes)
	}
	var r tagRecord
	copy(r.obj[:], b[0:20])
	copy(r.facts.peeled[:], b[20:40])
	r.facts.typ, r.facts.ptyp = b[40]>>4, b[40]&0xf
	flags := b[41]
	r.facts.date = int64(binary.BigEndian.Uint64(b[42:50])) //nolint:gosec // a date is held below to be non-negative
	r.facts.dated = flags == 1
	r.name = b[tagFixedBytes : tagFixedBytes+l]
	same := r.obj == r.facts.peeled
	switch {
	case r.facts.typ < 1 || r.facts.typ > 4 || r.facts.ptyp < 1 || r.facts.ptyp > 3:
		return tagRecord{}, 0, fmt.Errorf("%w: a record of types %d and %d", errTagsMalformed, r.facts.typ,
			r.facts.ptyp)
	case r.facts.typ != typeTag && (r.facts.typ != r.facts.ptyp || !same):
		return tagRecord{}, 0, fmt.Errorf("%w: a record of a %s peeling to another object", errTagsMalformed,
			tagTypeNames[r.facts.typ])
	case r.facts.typ == typeTag && same:
		return tagRecord{}, 0, fmt.Errorf("%w: a tag object peeling to itself", errTagsMalformed)
	case flags > 1:
		return tagRecord{}, 0, fmt.Errorf("%w: a record's flags %#x", errTagsMalformed, flags)
	case r.facts.date < 0 || !r.facts.dated && r.facts.date != 0:
		return tagRecord{}, 0, fmt.Errorf("%w: a record's date %d (dated %t)", errTagsMalformed, r.facts.date,
			r.facts.dated)
	case !utf8.Valid(r.name) || r.name[0] == '/' || r.name[len(r.name)-1] == '/':
		return tagRecord{}, 0, fmt.Errorf("%w: a record's name %q", errTagsMalformed, r.name)
	}
	return r, tagFixedBytes + l, nil
}

// validTagName holds a record's name to the ref grammar: where it is served,
// since a file read whole is held to the trailer only this cache writes.
func validTagName(name []byte) error {
	if !gitref.ValidRef(tagRefPrefix + string(name)) {
		return fmt.Errorf("%w: a record's name %q is no ref", errTagsMalformed, name)
	}
	return nil
}

// appendTagRecord appends the record of the tag name naming obj with facts f.
func appendTagRecord(b []byte, obj *[20]byte, f *tagFacts, name string) []byte {
	b = append(b, obj[:]...)
	b = append(b, f.peeled[:]...)
	b = append(b, f.typ<<4|f.ptyp)
	var flags byte
	if f.dated {
		flags = 1
	}
	b = append(b, flags)
	b = binary.BigEndian.AppendUint64(b, uint64(f.date)) //nolint:gosec // never negative
	b = append(b, byte(len(name)))
	return append(b, name...)
}

// entry is r as the index answers it.
func (r *tagRecord) entry() TagEntry {
	return TagEntry{Ref: tagRefPrefix + string(r.name), OID: hex.EncodeToString(r.obj[:]),
		Peeled: hex.EncodeToString(r.facts.peeled[:]), Type: tagTypeNames[r.facts.typ],
		PeeledType: tagTypeNames[r.facts.ptyp], Date: r.facts.date, Dated: r.facts.dated}
}

// tagWriter writes a tag index front to back: the head, the records, the
// offsets, the commit records, the counts and the trailer.
type tagWriter struct {
	f       io.Writer
	sum     hash.Hash
	w       *bufio.Writer
	off     int64
	offsets []uint32
	records int64
	commits int
	last    [tagCommitBytes]byte
	buf     []byte
}

func newTagWriter(f io.Writer) (*tagWriter, error) {
	t := &tagWriter{f: f, sum: sha256.New()}
	t.w = bufio.NewWriterSize(io.MultiWriter(f, t.sum), copyBuffer)
	head := binary.BigEndian.AppendUint32([]byte(tagsMagic), tagsFormat)
	return t, t.write(head)
}

func (t *tagWriter) write(b []byte) error {
	n, err := t.w.Write(b)
	t.off += int64(n)
	return err
}

// record writes one record's bytes.
func (t *tagWriter) record(raw []byte) error {
	if t.off > int64(^uint32(0)) {
		return fmt.Errorf("cache: a tag index past %d bytes of records", ^uint32(0))
	}
	t.offsets = append(t.offsets, uint32(t.off))
	return t.write(raw)
}

// endRecords writes the offsets once every record is written.
func (t *tagWriter) endRecords() error {
	t.records = t.off - tagsHeadBytes
	for _, o := range t.offsets {
		t.buf = binary.BigEndian.AppendUint32(t.buf[:0], o)
		if err := t.write(t.buf); err != nil {
			return err
		}
	}
	return nil
}

// commit writes one commit record, in (commit, index) order.
func (t *tagWriter) commit(c *[20]byte, idx uint32) error {
	t.buf = binary.BigEndian.AppendUint32(append(t.buf[:0], c[:]...), idx)
	if t.commits > 0 && bytes.Compare(t.buf, t.last[:]) <= 0 {
		return fmt.Errorf("cache: the tag index's commit records out of order at %x", t.buf)
	}
	copy(t.last[:], t.buf)
	t.commits++
	return t.write(t.buf)
}

// finish writes the counts and the trailer, and answers the file's size.
func (t *tagWriter) finish() (int64, error) {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(t.offsets))) //nolint:gosec // at most MaxRefsPerRepo
	b = binary.BigEndian.AppendUint32(b, uint32(t.records))         //nolint:gosec // held in record
	b = binary.BigEndian.AppendUint32(b, uint32(t.commits))         //nolint:gosec // at most the records
	if err := t.write(b); err != nil {
		return 0, err
	}
	if err := t.w.Flush(); err != nil {
		return 0, err
	}
	n, err := t.f.Write(t.sum.Sum(nil))
	return t.off + int64(n), err
}

// tagCounts is a file's counts, held to its size.
type tagCounts struct {
	n, m    int
	records int64
}

// readTagCounts reads r's head and counts and holds them to its size.
func readTagCounts(r io.ReaderAt, size int64) (tagCounts, error) {
	if size < tagsEmptyBytes {
		return tagCounts{}, fmt.Errorf("%w: %d bytes", errTagsMalformed, size)
	}
	var head [tagsHeadBytes]byte
	if _, err := r.ReadAt(head[:], 0); err != nil {
		return tagCounts{}, fmt.Errorf("%w: reading its head: %w", errTagsMalformed, err)
	}
	if string(head[:4]) != tagsMagic || binary.BigEndian.Uint32(head[4:]) != tagsFormat {
		return tagCounts{}, fmt.Errorf("%w: its head is %q", errTagsMalformed, head[:])
	}
	var tail [tagsCountsBytes]byte
	if _, err := r.ReadAt(tail[:], size-tagsTailBytes); err != nil {
		return tagCounts{}, fmt.Errorf("%w: reading its counts: %w", errTagsMalformed, err)
	}
	c := tagCounts{n: int(binary.BigEndian.Uint32(tail[0:4])), records: int64(binary.BigEndian.Uint32(tail[4:8])),
		m: int(binary.BigEndian.Uint32(tail[8:12]))}
	switch {
	case c.n > gitlet.MaxRefsPerRepo || c.m > c.n:
		return tagCounts{}, fmt.Errorf("%w: it counts %d tags and %d commit records", errTagsMalformed, c.n, c.m)
	case tagsHeadBytes+c.records+4*int64(c.n)+tagCommitBytes*int64(c.m)+tagsTailBytes != size:
		return tagCounts{}, fmt.Errorf("%w: its counts (%d tags in %d bytes, %d commit records) are not its "+
			"%d bytes", errTagsMalformed, c.n, c.records, c.m, size)
	case c.n == 0 && c.records != 0 || c.records < int64(c.n)*(tagFixedBytes+1):
		return tagCounts{}, fmt.Errorf("%w: %d tags in %d bytes", errTagsMalformed, c.n, c.records)
	}
	return c, nil
}

// tagScan is what a read of a whole tag file hands each part to: each record
// in order, the end of the records, and each commit record. An error a
// callback answers ends the read as it is.
type tagScan struct {
	begin      func(c tagCounts)
	record     func(i int, r *tagRecord, raw []byte) error
	recordsEnd func(n int) error
	commit     func(c *[20]byte, idx uint32) error
}

// scanTagFile reads the tag file at path front to back, held to the format,
// its keys strictly ascending, its trailer and its identity, else
// errTagsMalformed; it answers the identity and the counts.
func scanTagFile(path string, s tagScan) (fileID, tagCounts, error) {
	f, err := os.Open(path)
	if err != nil {
		return fileID{}, tagCounts{}, fmt.Errorf("%w: %w", errTagsMalformed, err)
	}
	defer func() { _ = f.Close() }() // read-only
	info, err := f.Stat()
	if err != nil {
		return fileID{}, tagCounts{}, fmt.Errorf("%w: %w", errTagsMalformed, err)
	}
	id := idOf(info)
	c, err := readTagCounts(f, info.Size())
	if err != nil {
		return id, c, err
	}
	if s.begin != nil {
		s.begin(c)
	}
	// HASHED AS CONSUMED, never as the buffer fills: the trailer is held to
	// the bytes before it.
	sum := sha256.New()
	r := bufio.NewReaderSize(f, copyBuffer)
	read := func(n int) ([]byte, error) {
		b, perr := r.Peek(n)
		if perr != nil {
			return nil, fmt.Errorf("%w: reading %d bytes: %w", errTagsMalformed, n, perr)
		}
		sum.Write(b)
		_, _ = r.Discard(n) // what Peek answered is in the buffer
		return b, nil
	}
	if _, err = read(tagsHeadBytes); err != nil {
		return id, c, err
	}
	// commitPeeled marks each record peeling to a commit, each taken once by
	// a commit record.
	commitPeeled := make([]byte, c.n/8+1)
	var lastName []byte
	var lastDate int64
	var at int64
	// One record and one commit for the whole read: a callback's pointer to
	// either would otherwise move it to the heap once a tag.
	var rec tagRecord
	var commit [20]byte
	for i := range c.n {
		fixed, perr := r.Peek(tagFixedBytes)
		if perr != nil {
			return id, c, fmt.Errorf("%w: reading record %d: %w", errTagsMalformed, i, perr)
		}
		raw, perr := r.Peek(tagFixedBytes + int(fixed[50]))
		if perr != nil {
			return id, c, fmt.Errorf("%w: reading record %d: %w", errTagsMalformed, i, perr)
		}
		var size int
		var derr error
		if rec, size, derr = decodeTagRecord(raw); derr != nil {
			return id, c, derr
		}
		raw = raw[:size]
		if at += int64(size); at > c.records {
			return id, c, fmt.Errorf("%w: record %d ends past the records", errTagsMalformed, i)
		}
		if i > 0 && compareTagKeys(lastDate, lastName, rec.facts.date, rec.name) >= 0 {
			return id, c, fmt.Errorf("%w: record %d (%s) is not after the one before it", errTagsMalformed, i,
				rec.name)
		}
		if rec.facts.ptyp == typeCommit {
			commitPeeled[i/8] |= 1 << (i % 8)
		}
		if s.record != nil {
			if err = s.record(i, &rec, raw); err != nil {
				return id, c, err
			}
		}
		lastDate, lastName = rec.facts.date, append(lastName[:0], rec.name...)
		sum.Write(raw)
		_, _ = r.Discard(size)
	}
	if at != c.records {
		return id, c, fmt.Errorf("%w: its records end at %d of %d bytes", errTagsMalformed, at, c.records)
	}
	if s.recordsEnd != nil {
		if err = s.recordsEnd(c.n); err != nil {
			return id, c, err
		}
	}
	var last uint32
	for i := range c.n {
		b, rerr := read(4)
		if rerr != nil {
			return id, c, rerr
		}
		o := binary.BigEndian.Uint32(b)
		if i == 0 && o != tagsHeadBytes || i > 0 && o <= last || int64(o) >= tagsHeadBytes+c.records {
			return id, c, fmt.Errorf("%w: offset %d is %d", errTagsMalformed, i, o)
		}
		last = o
	}
	var prev [tagCommitBytes]byte
	for i := range c.m {
		b, rerr := read(tagCommitBytes)
		if rerr != nil {
			return id, c, rerr
		}
		idx := binary.BigEndian.Uint32(b[20:])
		if i > 0 && bytes.Compare(prev[:], b) >= 0 || int(idx) >= c.n || commitPeeled[idx/8]&(1<<(idx%8)) == 0 {
			return id, c, fmt.Errorf("%w: commit record %d names record %d", errTagsMalformed, i, idx)
		}
		commitPeeled[idx/8] &^= 1 << (idx % 8)
		copy(prev[:], b)
		if s.commit != nil {
			copy(commit[:], b[:20])
			if err = s.commit(&commit, idx); err != nil {
				return id, c, err
			}
		}
	}
	if slices.ContainsFunc(commitPeeled, func(b byte) bool { return b != 0 }) {
		return id, c, fmt.Errorf("%w: a record peeling to a commit has no commit record", errTagsMalformed)
	}
	if _, err = read(tagsCountsBytes); err != nil {
		return id, c, err
	}
	want := sum.Sum(nil)
	trailer := make([]byte, sha256.Size)
	if _, err = io.ReadFull(r, trailer); err != nil {
		return id, c, fmt.Errorf("%w: reading its trailer: %w", errTagsMalformed, err)
	}
	if !bytes.Equal(trailer, want) {
		return id, c, fmt.Errorf("%w: its trailer is %x, and its bytes hash to %x", errTagsMalformed, trailer, want)
	}
	if _, err = r.ReadByte(); !errors.Is(err, io.EOF) {
		return id, c, fmt.Errorf("%w: bytes past its trailer", errTagsMalformed)
	}
	if now := lstatID(path); now != id {
		return id, c, fmt.Errorf("%w: it changed while it was read", errTagsMalformed)
	}
	return id, c, nil
}

// TagIndex is an incarnation's tag index, open for one read: valid until Close.
// Read through ReadAt on the open file, never a mapping, so a file renamed
// over or trashed under the read keeps its inode for it.
type TagIndex struct {
	r io.ReaderAt
	f *os.File
	tagCounts
	// fail hands the first error a method meets to the cache, once.
	fail func(error)
}

// openTagIndex is the index r holds, size bytes, its counts held to its size.
func openTagIndex(r io.ReaderAt, size int64) (*TagIndex, error) {
	c, err := readTagCounts(r, size)
	if err != nil {
		return nil, err
	}
	return &TagIndex{r: r, tagCounts: c}, nil
}

// Len is how many tags the index holds.
func (x *TagIndex) Len() int { return x.n }

// Close closes the file.
func (x *TagIndex) Close() error {
	if x.f == nil {
		return nil
	}
	return x.f.Close()
}

// failed hands err to the cache once and answers it.
func (x *TagIndex) failed(err error) error {
	if x.fail != nil {
		x.fail(err)
		x.fail = nil
	}
	return err
}

func (x *TagIndex) offsetsAt() int64  { return tagsHeadBytes + x.records }
func (x *TagIndex) commitsAt() int64  { return x.offsetsAt() + 4*int64(x.n) }
func (x *TagIndex) recordsEnd() int64 { return tagsHeadBytes + x.records }

// readAt reads len(b) bytes at off, a short read malformed.
func (x *TagIndex) readAt(b []byte, off int64) error {
	if _, err := x.r.ReadAt(b, off); err != nil {
		return fmt.Errorf("%w: reading %d bytes at %d: %w", errTagsMalformed, len(b), off, err)
	}
	return nil
}

// offset is record i's offset, held inside the records.
func (x *TagIndex) offset(i int) (int64, error) {
	var b [4]byte
	if err := x.readAt(b[:], x.offsetsAt()+4*int64(i)); err != nil {
		return 0, err
	}
	o := int64(binary.BigEndian.Uint32(b[:]))
	if o < tagsHeadBytes || o+tagFixedBytes > x.recordsEnd() {
		return 0, fmt.Errorf("%w: offset %d is %d, outside the records", errTagsMalformed, i, o)
	}
	return o, nil
}

// recordAt is the record at off, its name held to the ref grammar.
func (x *TagIndex) recordAt(off int64) (tagRecord, error) {
	b := make([]byte, min(int64(tagRecordMax), x.recordsEnd()-off))
	if err := x.readAt(b, off); err != nil {
		return tagRecord{}, err
	}
	r, _, err := decodeTagRecord(b)
	if err == nil {
		err = validTagName(r.name)
	}
	return r, err
}

// Rows calls each with every tag past after (nil: from the newest), in order,
// until it answers false.
func (x *TagIndex) Rows(after *TagKey, each func(TagEntry) bool) error {
	start := 0
	if after != nil {
		name, ok := strings.CutPrefix(after.Ref, tagRefPrefix)
		if !ok {
			return fmt.Errorf("cache: a tags listing past %q, which is no tag", after.Ref)
		}
		lo, hi := 0, x.n
		for lo < hi {
			mid := int(uint(lo+hi) >> 1)
			off, err := x.offset(mid)
			if err != nil {
				return x.failed(err)
			}
			r, err := x.recordAt(off)
			if err != nil {
				return x.failed(err)
			}
			if compareTagKeys(r.facts.date, r.name, after.Date, name) <= 0 {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		start = lo
	}
	for i := start; i < x.n; {
		k := min(tagRowsChunk, x.n-i)
		offs := make([]byte, 4*(k+1))
		if i+k == x.n {
			offs = offs[:4*k]
		}
		if err := x.readAt(offs, x.offsetsAt()+4*int64(i)); err != nil {
			return x.failed(err)
		}
		bounds := make([]int64, k+1)
		for j := range k {
			bounds[j] = int64(binary.BigEndian.Uint32(offs[4*j:]))
		}
		bounds[k] = x.recordsEnd()
		if i+k < x.n {
			bounds[k] = int64(binary.BigEndian.Uint32(offs[4*k:]))
		}
		for j := range k {
			if bounds[j] < tagsHeadBytes || bounds[j+1] <= bounds[j] || bounds[j+1] > x.recordsEnd() {
				return x.failed(fmt.Errorf("%w: offsets %d and %d are %d and %d", errTagsMalformed, i+j, i+j+1,
					bounds[j], bounds[j+1]))
			}
		}
		span := make([]byte, bounds[k]-bounds[0])
		if err := x.readAt(span, bounds[0]); err != nil {
			return x.failed(err)
		}
		for j := range k {
			raw := span[bounds[j]-bounds[0] : bounds[j+1]-bounds[0]]
			r, size, err := decodeTagRecord(raw)
			if err == nil && size != len(raw) {
				err = fmt.Errorf("%w: record %d is %d bytes between offsets %d apart", errTagsMalformed, i+j, size,
					len(raw))
			}
			if err == nil {
				err = validTagName(r.name)
			}
			if err != nil {
				return x.failed(err)
			}
			if !each(r.entry()) {
				return nil
			}
		}
		i += k
	}
	return nil
}

// Commit is the tags naming commit oid, at most most of them newest first, and
// how many more: at most 2·⌈log2(m+1)⌉ + 2·most reads, however many tags one
// commit has.
func (x *TagIndex) Commit(oid string, most int) (refs []string, more int, err error) {
	var want [20]byte
	if !gitref.ValidOID(oid) {
		return nil, 0, nil
	}
	if _, err = hex.Decode(want[:], []byte(oid)); err != nil {
		return nil, 0, nil //nolint:nilerr // ValidOID held it to hex: no tag names what is no id
	}
	most = max(most, 0)
	block := func(at, count int) ([]byte, error) {
		b := make([]byte, tagCommitBytes*count)
		return b, x.readAt(b, x.commitsAt()+tagCommitBytes*int64(at))
	}
	// THE LOWER BOUND, each probe reading the most+1 records from it: the
	// bound is a probed index, so its block is in hand when the search ends.
	var found []byte
	lo, hi := 0, x.m
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		b, rerr := block(mid, min(most+1, x.m-mid))
		if rerr != nil {
			return nil, 0, x.failed(rerr)
		}
		if bytes.Compare(b[:20], want[:]) < 0 {
			lo = mid + 1
		} else {
			hi, found = mid, b
		}
	}
	if lo == x.m || found == nil || !bytes.Equal(found[:20], want[:]) {
		return nil, 0, nil
	}
	run := 0
	for run*tagCommitBytes < len(found) && bytes.Equal(found[run*tagCommitBytes:run*tagCommitBytes+20], want[:]) {
		run++
	}
	if run*tagCommitBytes == len(found) && lo+run < x.m {
		// THE UPPER BOUND, only for a run longer than the block.
		ulo, uhi := lo+run, x.m
		for ulo < uhi {
			mid := int(uint(ulo+uhi) >> 1)
			b, rerr := block(mid, 1)
			if rerr != nil {
				return nil, 0, x.failed(rerr)
			}
			if bytes.Compare(b[:20], want[:]) <= 0 {
				ulo = mid + 1
			} else {
				uhi = mid
			}
		}
		run = ulo - lo
	}
	take := min(most, run)
	for j := range take {
		idx := int(binary.BigEndian.Uint32(found[j*tagCommitBytes+20:]))
		if idx >= x.n {
			return nil, 0, x.failed(fmt.Errorf("%w: a commit record names record %d of %d", errTagsMalformed, idx,
				x.n))
		}
		off, oerr := x.offset(idx)
		if oerr != nil {
			return nil, 0, x.failed(oerr)
		}
		r, rerr := x.recordAt(off)
		if rerr != nil {
			return nil, 0, x.failed(rerr)
		}
		if r.facts.ptyp != typeCommit || r.facts.peeled != want {
			return nil, 0, x.failed(fmt.Errorf("%w: a commit record of %s names a tag of %x", errTagsMalformed, oid,
				r.facts.peeled))
		}
		refs = append(refs, tagRefPrefix+string(r.name))
	}
	return refs, run - take, nil
}

// Tags is the tag index of the incarnation s holds, open for one read and
// closed by its caller; false where it keeps none a read can trust, which
// asks a round to build it.
func (c *Cache) Tags(s Synced) (*TagIndex, bool) {
	if c.handedOut(s, "a read of the tag index") != nil {
		return nil, false
	}
	e, inc := s.entry, s.inc
	path := filepath.Join(inc.dir, filepath.FromSlash(tagsRel))
	for range 2 {
		e.mu.Lock()
		if inc.derived[DerivedTags].Step != StepTags {
			c.askTags(e, inc)
			e.mu.Unlock()
			return nil, false
		}
		e.mu.Unlock()
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close() // read-only
			continue
		}
		id := idOf(info)
		e.mu.Lock()
		vouched := vouchedAt(inc, tagsRel, id)
		e.mu.Unlock()
		if !vouched {
			// A round may have renamed the next index in between: once more.
			_ = f.Close() // read-only
			continue
		}
		x, err := openTagIndex(f, info.Size())
		if err != nil {
			_ = f.Close() // read-only
			c.tagsUnreadable(e, inc, id, err)
			return nil, false
		}
		x.f = f
		x.fail = func(err error) { c.tagsUnreadable(e, inc, id, err) }
		return x, true
	}
	// No vouched file stands behind the record, and a delta moving no tag
	// would never level it: set it aside and ask a round.
	e.mu.Lock()
	defer e.mu.Unlock()
	if inc.derived[DerivedTags].Step == StepTags && !vouchedAt(inc, tagsRel, lstatID(path)) {
		c.asideTags(inc)
		c.askTags(e, inc)
	}
	return nil, false
}

// askTags queues a round at the copy's own position to build inc's tag
// index, at most one queued and none within TagsRetry of a failed step.
// e.mu is held.
func (c *Cache) askTags(e *entry, inc *incarnation) {
	if e.current != inc || !inc.converged || e.dead || e.verifying > 0 || c.isStopped() ||
		c.now().Sub(inc.tagsFailed) < TagsRetry {
		return
	}
	fresh := statelog.FloorAt(statelog.ReadSession, inc.at)
	if e.next == nil {
		e.next = &round{fresh: fresh}
	} else {
		e.next.fresh = merge(e.next.fresh, fresh)
	}
	c.start(e)
}

// tagsUnreadable sets inc's tag index aside where a read found the file at id,
// still in place, not as this cache wrote it: record cleared, vouch dropped,
// the scrub and a round asked, logged once a file.
func (c *Cache) tagsUnreadable(e *entry, inc *incarnation, id fileID, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !c.tagsDamaged(inc, id) {
		return
	}
	inc.tagsFailed = time.Time{}
	c.logTagsUnreadable(e, inc, id, err)
	c.askTags(e, inc)
}

// tagsDamaged sets inc's tag index aside and asks the scrub where the file at
// id, found not as written, is still the one vouched. e.mu is held.
func (c *Cache) tagsDamaged(inc *incarnation, id fileID) bool {
	if !vouchedAt(inc, tagsRel, id) {
		return false
	}
	c.asideTags(inc)
	c.scrubAsked(inc)
	return true
}

// asideTags sets inc's tag index aside: no read is handed it, and a file still
// there stays counted as the next build's source of facts. e.mu is held.
func (c *Cache) asideTags(inc *incarnation) {
	if id := lstatID(filepath.Join(inc.dir, filepath.FromSlash(tagsRel))); id.present {
		c.setDerived(inc, DerivedTags, derivedState{Bytes: id.size}, true)
	} else {
		c.setDerived(inc, DerivedTags, derivedState{}, false)
	}
	unvouch(inc, tagsRel)
}

// logTagsUnreadable logs the tag index at id unreadable, once a file; e.mu is
// held.
func (c *Cache) logTagsUnreadable(e *entry, inc *incarnation, id fileID, err error) {
	if inc.tagsBad == id {
		return
	}
	inc.tagsBad = id
	c.log.Warn("code_cache_tags_unreadable", "repo", e.id, "incarnation", inc.n, "error", err)
}

// tagStep is what a round's tag step leaves for the placement: a staged index,
// the index in place found level, or an index to set aside; failed is why it
// wrote none.
type tagStep struct {
	staged string
	bytes  int64
	level  fileID
	aside  bool
	failed error
}

// discard removes a staged index the round did not place, and gives its
// bytes back.
func (s *tagStep) discard(c *Cache) {
	if s.staged == "" {
		return
	}
	_ = removeStaged(s.staged, nil) // a round that failed: tmp/ is tidied
	c.tmpBytes.Add(-s.bytes)
	s.staged = ""
}

// peelFacts is what the round's peel read of the tags it sets: each annotated
// tag's peel (packed-refs' own map) and every object's peeled type.
type peelFacts struct {
	peeled map[string]string
	types  map[string]gitlet.ObjectType
}

// refsLeft is the packed-refs a round leaves: the file it staged, or the one
// in place, and the digest it has.
type refsLeft struct{ path, digest string }

// tagsDue reports whether a round's writeRefs levels inc's tag index: always
// on a full round, on a delta where it moves a tag and the index is served,
// and otherwise once TagsRetry has passed since a step failed. e.mu is held.
func (c *Cache) tagsDue(inc *incarnation, snap snapshot) bool {
	switch {
	case snap.full:
		return true
	case inc.derived[DerivedTags].Step == StepTags:
		return slices.ContainsFunc(snap.refs, func(r gitlet.RefRow) bool { return isTag(r.Ref) })
	}
	return c.now().Sub(inc.tagsFailed) >= TagsRetry
}

// tagStepEnds reports a failure of the tag step that is the ROUND's: damage
// git named, a packed-refs not ours, or the round's end, each of which the
// round's repair, its full read or its end answers.
func tagStepEnds(ctx context.Context, err error) bool {
	_, damaged := DamageReason(err)
	return damaged || len(hintsOf(err)) > 0 || errors.Is(err, errRefsNotOurs) || ctx.Err() != nil
}

// levelTags brings inc's tag index level with the refs the round leaves:
// merged, found level or built whole. A failure tagStepEnds names fails the
// round; any other is the step's own, logged and answered in the step.
func (c *Cache) levelTags(ctx context.Context, e *entry, inc *incarnation, snap snapshot, left refsLeft,
	pf peelFacts, by gitproc.Repairer, refsMove bool) (tagStep, error) {
	path := filepath.Join(inc.dir, filepath.FromSlash(tagsRel))
	e.mu.Lock()
	servable := inc.derived[DerivedTags].Step == StepTags
	id := lstatID(path)
	vouched := vouchedAt(inc, tagsRel, id)
	e.mu.Unlock()

	// aside: the index served now can no longer be, should the step fail.
	aside := servable && refsMove
	var source map[[20]byte]tagFacts
	unreadable := func(err error) {
		aside = servable
		// Never served again, however the rest of the round ends.
		e.mu.Lock()
		c.tagsDamaged(inc, id)
		c.logTagsUnreadable(e, inc, id, err)
		e.mu.Unlock()
	}
	switch {
	case !snap.full && servable && vouched:
		st, err := c.mergeTags(ctx, e, inc, snap, path, pf, by)
		switch {
		case err == nil:
			return st, nil
		case !errors.Is(err, errTagsMalformed):
			return c.tagStepFailed(ctx, e, inc, err, aside)
		}
		unreadable(err)
	case id.present:
		level, facts, err := checkTagFile(path, snap)
		switch {
		case err != nil:
			unreadable(err)
		case level:
			if err = c.tagKnob(ctx, inc, 0); err != nil {
				return c.tagStepFailed(ctx, e, inc, err, aside)
			}
			return tagStep{level: id}, nil
		default:
			source = facts
			aside = servable
		}
	}
	st, err := c.buildTags(ctx, e, inc, snap, left, pf, by, source)
	if err != nil {
		return c.tagStepFailed(ctx, e, inc, err, aside)
	}
	return st, nil
}

// tagStepFailed answers a failure of the tag step: the round's, or the
// step's own, logged.
func (c *Cache) tagStepFailed(ctx context.Context, e *entry, inc *incarnation, err error, aside bool) (tagStep,
	error) {
	if tagStepEnds(ctx, err) {
		return tagStep{}, err
	}
	c.log.Warn("code_cache_tags_failed", "repo", e.id, "incarnation", inc.n, "error", err)
	return tagStep{failed: err, aside: aside}, nil
}

// checkTagFile reads the tag file at path whole: whether a full round's
// snapshot names exactly its tags at their objects, and every record's facts
// by object id.
func checkTagFile(path string, snap snapshot) (bool, map[[20]byte]tagFacts, error) {
	var tags []gitlet.RefRow
	if snap.full {
		lo, _ := slices.BinarySearchFunc(snap.refs, tagRefPrefix, func(r gitlet.RefRow, p string) int {
			return strings.Compare(r.Ref, p)
		})
		hi := lo
		for hi < len(snap.refs) && isTag(snap.refs[hi].Ref) {
			hi++
		}
		tags = snap.refs[lo:hi]
	}
	matched := make([]bool, len(tags))
	level := snap.full
	facts := map[[20]byte]tagFacts{}
	var hexed [40]byte
	_, _, err := scanTagFile(path, tagScan{record: func(_ int, r *tagRecord, _ []byte) error {
		facts[r.obj] = r.facts
		if !level {
			return nil
		}
		i, found := slices.BinarySearchFunc(tags, r.name, func(row gitlet.RefRow, name []byte) int {
			return compareNames(row.Ref[len(tagRefPrefix):], name)
		})
		hex.Encode(hexed[:], r.obj[:])
		if !found || matched[i] || tags[i].OID != string(hexed[:]) {
			level = false
			return nil
		}
		matched[i] = true
		return nil
	}})
	if err != nil {
		return false, nil, err
	}
	return level && !slices.Contains(matched, false), facts, nil
}

// tagMove is a tag a delta moved: its name after refs/tags/ and its new
// object, "" where it was deleted.
type tagMove struct{ name, oid string }

// tagInsert is a tag a merge writes: its record and its index's place.
type tagInsert struct {
	obj   [20]byte
	facts tagFacts
	name  string
}

// mergeTags stages the index of a delta: the file in place merged with the
// tags the delta moved. An old file found unreadable is errTagsMalformed, and
// nothing is staged.
func (c *Cache) mergeTags(ctx context.Context, e *entry, inc *incarnation, snap snapshot, path string,
	pf peelFacts, by gitproc.Repairer) (tagStep, error) {
	var moves []tagMove
	for _, r := range snap.refs {
		if name, ok := strings.CutPrefix(r.Ref, tagRefPrefix); ok {
			moves = append(moves, tagMove{name: name, oid: r.OID})
		}
	}
	slices.SortFunc(moves, func(a, b tagMove) int { return strings.Compare(a.name, b.name) })
	var objs [][20]byte
	for _, m := range moves {
		if m.oid != "" {
			objs = append(objs, tagOID(m.oid))
		}
	}
	facts, err := c.readTagFacts(ctx, inc, by, uniqueObjs(objs), pf, nil)
	if err != nil {
		return tagStep{}, err
	}
	inserts := make([]tagInsert, 0, len(objs))
	for _, m := range moves {
		if m.oid != "" {
			o := tagOID(m.oid)
			inserts = append(inserts, tagInsert{obj: o, facts: facts[o], name: m.name})
		}
	}
	slices.SortFunc(inserts, func(a, b tagInsert) int {
		return compareTagKeys(a.facts.date, a.name, b.facts.date, b.name)
	})

	var st tagStep
	err = c.stageTags(&st, func(w *tagWriter) error { return mergeTagFile(path, moves, inserts, w) })
	return st, err
}

// mergeTagFile writes into w the tag file at path merged with moves, whose new
// tags are inserts in listing order: one pass, each kept record copied from
// the read buffer, allocating for what moved and four bytes a tag.
func mergeTagFile(path string, moves []tagMove, inserts []tagInsert, w *tagWriter) error {
	// skipped is each old record a move replaces, by its old index, and
	// keptBefore each insert's count of old records kept before it: what
	// re-indexes an old commit record (two short sorted lists).
	var skipped, keptBefore []uint32
	var kept uint32
	next := 0
	var buf []byte
	flush := func(date int64, name []byte, end bool) error {
		for next < len(inserts) && (end ||
			compareTagKeys(inserts[next].facts.date, inserts[next].name, date, name) < 0) {
			in := &inserts[next]
			buf = appendTagRecord(buf[:0], &in.obj, &in.facts, in.name)
			if werr := w.record(buf); werr != nil {
				return werr
			}
			keptBefore = append(keptBefore, kept)
			next++
		}
		return nil
	}
	var insertCommits []tagCommitRec
	// rec is each old commit record re-indexed, one for the whole merge.
	var rec tagCommitRec
	pending := 0
	reindex := func(i uint32) (uint32, bool) {
		s, dropped := slices.BinarySearch(skipped, i)
		if dropped {
			return 0, false
		}
		k := i - uint32(s) //nolint:gosec // s counts skipped records below i
		before, _ := slices.BinarySearchFunc(keptBefore, k, func(kb, k uint32) int {
			if kb <= k {
				return -1
			}
			return 1
		})
		return k + uint32(before), true //nolint:gosec // at most the inserts
	}
	writeInserts := func(upTo *tagCommitRec) error {
		for pending < len(insertCommits) && (upTo == nil || insertCommits[pending].less(upTo)) {
			ic := &insertCommits[pending]
			if werr := w.commit(&ic.commit, ic.idx); werr != nil {
				return werr
			}
			pending++
		}
		return nil
	}
	_, _, serr := scanTagFile(path, tagScan{
		begin: func(old tagCounts) {
			w.offsets = make([]uint32, 0, old.n+len(inserts))
			skipped = make([]uint32, 0, len(moves))
		},
		record: func(i int, r *tagRecord, raw []byte) error {
			if _, moved := slices.BinarySearchFunc(moves, r.name, func(m tagMove, name []byte) int {
				return compareNames(m.name, name)
			}); moved {
				skipped = append(skipped, uint32(i)) //nolint:gosec // at most MaxRefsPerRepo
				return nil
			}
			if werr := flush(r.facts.date, r.name, false); werr != nil {
				return werr
			}
			kept++
			return w.record(raw)
		},
		recordsEnd: func(int) error {
			if werr := flush(0, nil, true); werr != nil {
				return werr
			}
			for j := range inserts {
				if inserts[j].facts.ptyp == typeCommit {
					insertCommits = append(insertCommits, tagCommitRec{commit: inserts[j].facts.peeled,
						idx: keptBefore[j] + uint32(j)}) //nolint:gosec // at most the inserts
				}
			}
			slices.SortFunc(insertCommits, func(a, b tagCommitRec) int { return a.compare(&b) })
			return w.endRecords()
		},
		commit: func(commit *[20]byte, idx uint32) error {
			ni, ok := reindex(idx)
			if !ok {
				return nil
			}
			rec.commit, rec.idx = *commit, ni
			if werr := writeInserts(&rec); werr != nil {
				return werr
			}
			return w.commit(&rec.commit, rec.idx)
		},
	})
	if serr != nil {
		return serr
	}
	return writeInserts(nil)
}

// tagCommitRec is one commit record.
type tagCommitRec struct {
	commit [20]byte
	idx    uint32
}

func (a *tagCommitRec) compare(b *tagCommitRec) int {
	if c := bytes.Compare(a.commit[:], b.commit[:]); c != 0 {
		return c
	}
	switch {
	case a.idx < b.idx:
		return -1
	case a.idx > b.idx:
		return 1
	}
	return 0
}

func (a *tagCommitRec) less(b *tagCommitRec) bool { return a.compare(b) < 0 }

// tagLine is one tag of a build: its name after refs/tags/ and its object.
type tagLine struct {
	name string
	obj  [20]byte
}

// buildTags writes the index whole: every tag the refs the round leaves hold,
// each tag's facts from source by its object where it holds them, else from
// the peel and git.
func (c *Cache) buildTags(ctx context.Context, e *entry, inc *incarnation, snap snapshot, left refsLeft,
	pf peelFacts, by gitproc.Repairer, source map[[20]byte]tagFacts) (tagStep, error) {
	var tags []tagLine
	if snap.full {
		for _, r := range snap.refs {
			if name, ok := strings.CutPrefix(r.Ref, tagRefPrefix); ok {
				tags = append(tags, tagLine{name: name, obj: tagOID(r.OID)})
			}
		}
	} else {
		var err error
		if tags, err = tagLines(left); err != nil {
			return tagStep{}, fmt.Errorf("cache: sync %s: refs: %w", e.id, err)
		}
	}
	objs := make([][20]byte, len(tags))
	for i, t := range tags {
		objs[i] = t.obj
	}
	facts, err := c.readTagFacts(ctx, inc, by, uniqueObjs(objs), pf, source)
	if err != nil {
		return tagStep{}, err
	}
	all := make([]tagInsert, len(tags))
	for i := range tags {
		all[i] = tagInsert{obj: tags[i].obj, facts: facts[tags[i].obj], name: tags[i].name}
	}
	var st tagStep
	err = c.stageTags(&st, func(w *tagWriter) error { return writeTagFile(all, w) })
	return st, err
}

// writeTagFile writes into w the tag file of tags, in any order: sorted into
// the listing's, its commit records after them.
func writeTagFile(tags []tagInsert, w *tagWriter) error {
	slices.SortFunc(tags, func(a, b tagInsert) int {
		return compareTagKeys(a.facts.date, a.name, b.facts.date, b.name)
	})
	var buf []byte
	var commits []tagCommitRec
	for i := range tags {
		t := &tags[i]
		buf = appendTagRecord(buf[:0], &t.obj, &t.facts, t.name)
		if err := w.record(buf); err != nil {
			return err
		}
		if t.facts.ptyp == typeCommit {
			commits = append(commits, tagCommitRec{commit: t.facts.peeled, idx: uint32(i)}) //nolint:gosec // at most MaxRefsPerRepo
		}
	}
	if err := w.endRecords(); err != nil {
		return err
	}
	slices.SortFunc(commits, func(a, b tagCommitRec) int { return a.compare(&b) })
	for i := range commits {
		if err := w.commit(&commits[i].commit, commits[i].idx); err != nil {
			return err
		}
	}
	return nil
}

// stageTags writes a new index into tmp/, counted until placed or discarded,
// with no fsync: an index a previous process left is read whole before it
// serves, so a lost write costs a build, never a wrong answer.
func (c *Cache) stageTags(st *tagStep, write func(w *tagWriter) error) error {
	f, err := os.CreateTemp(filepath.Join(c.dir, tmpDir), "tags-*")
	if err != nil {
		return fmt.Errorf("cache: staging a tag index: %w", err)
	}
	staged := f.Name()
	var size int64
	w, err := newTagWriter(f)
	if err == nil {
		err = write(w)
	}
	if err == nil {
		size, err = w.finish()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return removeStaged(staged, err)
	}
	st.staged, st.bytes = staged, size
	c.tmpBytes.Add(size)
	return nil
}

// placeTags places what the round's tag step left, in writeRefs' placement
// hold after packed-refs, and answers whether it changed the file or the
// record. e.mu is held.
func (c *Cache) placeTags(e *entry, inc *incarnation, s *tagStep) bool {
	path := filepath.Join(inc.dir, filepath.FromSlash(tagsRel))
	servable := inc.derived[DerivedTags].Step == StepTags
	switch {
	case s.staged != "":
		staged, size := s.staged, s.bytes
		s.staged = ""
		c.tmpBytes.Add(-size)
		err := tagsDir(inc)
		if err != nil {
			err = removeStaged(staged, err)
		} else {
			err = c.place(staged, path)
		}
		if err != nil {
			// A round stages an index only over one behind its refs or unfit.
			c.log.Warn("code_cache_tags_failed", "repo", e.id, "incarnation", inc.n, "error", err)
			s.failed, s.aside = err, servable
			return c.placeTags(e, inc, s)
		}
		vouch(inc, tagsRel, lstatID(path))
		c.setDerived(inc, DerivedTags, derivedState{Bytes: size, Step: StepTags}, true)
		inc.tagsFailed = time.Time{}
		return true
	case s.level.present:
		vouch(inc, tagsRel, s.level)
		inc.tagsFailed = time.Time{}
		if servable && inc.derived[DerivedTags].Bytes == s.level.size {
			return false
		}
		c.setDerived(inc, DerivedTags, derivedState{Bytes: s.level.size, Step: StepTags}, true)
		return true
	case s.aside:
		c.asideTags(inc)
		inc.tagsFailed = c.now()
		return true
	case s.failed != nil:
		inc.tagsFailed = c.now()
	}
	return false
}

// tagLines is every tag of the packed-refs left: each tag line's name and
// object, the file held to the grammar and to left's digest, else
// errRefsNotOurs.
func tagLines(left refsLeft) ([]tagLine, error) {
	f, err := os.Open(left.path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errRefsNotOurs, err)
	}
	defer func() { _ = f.Close() }() // read-only
	sum := sha256.New()
	lines := &refsReader{r: bufio.NewReaderSize(io.TeeReader(f, sum), copyBuffer)}
	if err = lines.header(); err != nil {
		return nil, err
	}
	var out []tagLine
	for {
		line, lerr := lines.next()
		if errors.Is(lerr, io.EOF) {
			break
		}
		if lerr != nil {
			return nil, lerr
		}
		if line.peel || !bytes.HasPrefix(line.ref, tagsPrefix) {
			continue
		}
		var obj [20]byte
		if _, err = hex.Decode(obj[:], line.raw[:gitref.OIDHexLen]); err != nil {
			return nil, fmt.Errorf("%w: a line %q", errRefsNotOurs, line.raw)
		}
		out = append(out, tagLine{name: string(line.ref[len(tagRefPrefix):]), obj: obj})
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != left.digest {
		return nil, fmt.Errorf("%w: it hashes to %s, and this cache wrote %s", errRefsNotOurs, got, left.digest)
	}
	return out, nil
}

// tagOID is a whole object id the rows or git answered, held to the grammar
// already, as bytes.
func tagOID(oid string) [20]byte {
	b, _ := oidBytes(oid) // every caller's id is a ValidOID
	return b
}

// uniqueObjs is objs sorted, each once.
func uniqueObjs(objs [][20]byte) [][20]byte {
	out := slices.Clone(objs)
	slices.SortFunc(out, func(a, b [20]byte) int { return bytes.Compare(a[:], b[:]) })
	return slices.Compact(out)
}

// readTagFacts is the facts of objs: from source where it holds them, else the
// type and peel from pf or a cat-file --batch-check, and the date from a
// cat-file --batch of annotated tags and a log of lightweight tags' commits.
func (c *Cache) readTagFacts(ctx context.Context, inc *incarnation, by gitproc.Repairer, objs [][20]byte,
	pf peelFacts, source map[[20]byte]tagFacts) (map[[20]byte]tagFacts, error) {
	out := make(map[[20]byte]tagFacts, len(objs))
	var need, check, annotated, commits []string
	for _, o := range objs {
		if f, ok := source[o]; ok {
			out[o] = f
			continue
		}
		oid := hex.EncodeToString(o[:])
		need = append(need, oid)
		t, ok := pf.types[oid]
		if !ok {
			check = append(check, oid)
			continue
		}
		f := tagFacts{ptyp: typeCode(t), peeled: o}
		if p, annotatedTag := pf.peeled[oid]; annotatedTag {
			f.typ, f.peeled = typeTag, tagOID(p)
		} else {
			f.typ = f.ptyp
		}
		out[o] = f
	}
	if err := c.tagKnob(ctx, inc, len(need)); err != nil {
		return nil, err
	}
	if len(check) > 0 {
		if err := c.tagTypes(ctx, inc, by, check, out); err != nil {
			return nil, err
		}
	}
	for _, oid := range need {
		f := out[tagOID(oid)]
		switch {
		case f.typ == typeTag:
			annotated = append(annotated, oid)
		case f.typ == typeCommit:
			commits = append(commits, oid)
		}
	}
	if len(annotated) > 0 {
		if err := c.tagDates(ctx, inc, by, annotated, out); err != nil {
			return nil, err
		}
	}
	if len(commits) > 0 {
		if err := c.commitDates(ctx, inc, by, commits, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// tagKnob is the suite's knob over a step about to read reading objects'
// facts through git, none where it sets none.
func (c *Cache) tagKnob(ctx context.Context, inc *incarnation, reading int) error {
	if c.knobs == nil || c.knobs.tagging == nil {
		return nil
	}
	return c.knobs.tagging(ctx, inc.dir, reading)
}

// tagTypes reads each object's peel and its peeled type with one cat-file
// --batch-check of `<oid>^{}`, which peels through every tag.
func (c *Cache) tagTypes(ctx context.Context, inc *incarnation, by gitproc.Repairer, oids []string,
	out map[[20]byte]tagFacts) error {
	var stdin bytes.Buffer
	for _, oid := range oids {
		stdin.WriteString(oid + "^{}\n")
	}
	i := 0
	lines := gitproc.NewLines(func(line []byte) error {
		if i >= len(oids) {
			return errors.New("cache: cat-file answered more lines than it was asked")
		}
		oid := oids[i]
		i++
		peeled, t, err := peelLine(oid, string(line))
		if err != nil {
			return err
		}
		f := tagFacts{peeled: tagOID(peeled), ptyp: typeCode(t)}
		f.typ = f.ptyp
		if peeled != oid {
			f.typ = typeTag
		}
		out[tagOID(oid)] = f
		return nil
	})
	_, err := c.git.Run(ctx, gitproc.Cmd{
		Dir: inc.dir, Dropping: heldView(inc.dir), Config: c.ServeConfig(),
		Args:  []string{"cat-file", "--batch-check=%(objectname) %(objecttype)", "--buffer"},
		Stdin: bytes.NewReader(stdin.Bytes()), Stdout: lines, Class: gitproc.ClassRead, Repairer: by,
		Deadline: gitproc.PackWorkDeadline(int64(stdin.Len())),
	})
	if cerr := lines.Close(); err == nil {
		err = cerr
	}
	if err == nil && i != len(oids) {
		err = fmt.Errorf("cache: cat-file answered %d of %d tags", i, len(oids))
	}
	if err != nil {
		return fmt.Errorf("cache: the tag index: types: %w", err)
	}
	return nil
}

// peelLine reads one line of a `--batch-check=%(objectname) %(objecttype)` of
// `<oid>^{}`: what oid peels to and that object's type.
func peelLine(oid, line string) (string, gitlet.ObjectType, error) {
	if line == oid+"^{} missing" {
		return "", "", fmt.Errorf("%w: %s, which no installed pack holds", gitproc.ErrMissingObject, oid)
	}
	peeled, t, ok := strings.Cut(line, " ")
	typ := gitlet.ObjectType(t)
	if !ok || !gitref.ValidOID(peeled) || !typ.Valid() || typ == gitlet.ObjectTag {
		return "", "", fmt.Errorf("cache: cat-file peeled %s as %q", oid, line)
	}
	return peeled, typ, nil
}

// tagDates reads each annotated tag's tagger date with one cat-file --batch.
func (c *Cache) tagDates(ctx context.Context, inc *incarnation, by gitproc.Repairer, oids []string,
	out map[[20]byte]tagFacts) error {
	var stdin bytes.Buffer
	for _, oid := range oids {
		stdin.WriteString(oid + "\n")
	}
	i := 0
	stream := gitref.NewTagStream(gitref.TagCaps{}, func(t gitref.Tag) error {
		if i >= len(oids) || t.OID != oids[i] {
			return fmt.Errorf("cache: cat-file answered tag %s out of the order asked", t.OID)
		}
		i++
		o := tagOID(t.OID)
		f := out[o]
		if !t.TaggedAt.IsZero() {
			f.dated, f.date = true, t.TaggedAt.Unix()
		}
		out[o] = f
		return nil
	})
	_, err := c.git.Run(ctx, gitproc.Cmd{
		Dir: inc.dir, Dropping: heldView(inc.dir), Config: c.ServeConfig(),
		Args:  []string{"cat-file", "--batch", "--buffer"},
		Stdin: bytes.NewReader(stdin.Bytes()), Stdout: stream, Class: gitproc.ClassRead, Repairer: by,
		Deadline: gitproc.PackWorkDeadline(int64(stdin.Len())),
	})
	if cerr := stream.Close(); err == nil {
		err = cerr
	}
	if err == nil && i != len(oids) {
		err = fmt.Errorf("cache: cat-file answered %d of %d tags", i, len(oids))
	}
	if err != nil {
		return fmt.Errorf("cache: the tag index: tagger dates: %w", err)
	}
	return nil
}

// commitDates reads each lightweight tag's commit's committer date with one
// log, which reads no mailmap or notes; a commit whose date git prints empty
// is undated, as its creatordate is.
func (c *Cache) commitDates(ctx context.Context, inc *incarnation, by gitproc.Repairer, oids []string,
	out map[[20]byte]tagFacts) error {
	var stdin bytes.Buffer
	asked := make(map[string]bool, len(oids))
	for _, oid := range oids {
		stdin.WriteString(oid + "\n")
		asked[oid] = false
	}
	lines := gitproc.NewLines(func(line []byte) error {
		oid, secs, ok := strings.Cut(string(line), " ")
		answered, known := asked[oid]
		switch {
		case !ok || !known || answered:
			return fmt.Errorf("cache: log answered %q", line)
		case len(secs) > maxDateDigits:
			return fmt.Errorf("cache: log dated %s %q", oid, secs)
		}
		asked[oid] = true
		if secs == "" {
			return nil
		}
		n, err := strconv.ParseInt(secs, 10, 64)
		if err != nil || n < 0 || strconv.FormatInt(n, 10) != secs {
			return fmt.Errorf("cache: log dated %s %q", oid, secs)
		}
		o := tagOID(oid)
		f := out[o]
		f.dated, f.date = true, n
		out[o] = f
		return nil
	})
	_, err := c.git.Run(ctx, gitproc.Cmd{
		Dir: inc.dir, Dropping: heldView(inc.dir), Config: c.ServeConfig(),
		Args: []string{"log", "--no-walk=unsorted", "--no-mailmap", "--no-color", "--no-notes", "--stdin",
			"--format=%H %ct"},
		Stdin: bytes.NewReader(stdin.Bytes()), Stdout: lines, Class: gitproc.ClassRead, Repairer: by,
		Deadline: gitproc.PackWorkDeadline(int64(stdin.Len())),
	})
	if cerr := lines.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		for oid, answered := range asked {
			if !answered {
				err = fmt.Errorf("cache: log answered no line for %s", oid)
				break
			}
		}
	}
	if err != nil {
		return fmt.Errorf("cache: the tag index: committer dates: %w", err)
	}
	return nil
}

// maxDateDigits is the most digits of a date in seconds: an int64's.
const maxDateDigits = 19

// tagsDir makes the incarnation's crewlet/ where it is missing, so a tag
// index can be renamed in.
func tagsDir(inc *incarnation) error {
	dir := filepath.Join(inc.dir, engineRoot)
	switch info, err := os.Lstat(dir); {
	case errors.Is(err, fs.ErrNotExist):
		return os.Mkdir(dir, hostbox.DirMode)
	case err != nil:
		return err
	case !info.IsDir():
		return fmt.Errorf("cache: %s is not a directory", dir)
	}
	return nil
}
