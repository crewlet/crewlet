package codeindex

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"slices"
	"unicode"

	"github.com/crewlet/crewlet/internal/regexlit"
)

// A SEGMENT: one immutable file of a repository's index — a run of blobs, by
// object id, and the trigrams of each. Big-endian throughout:
//
//	header    "CRCX" | version u8 (1) | oid bytes u8 (20) | reserved u16 | blobs u32 | reserved u32 |
//	          unicode [16]                                                             (32 bytes)
//	blobs     blobs × (oid [20] | flags u8)          ascending by oid, no oid twice
//	postings  each trigram's blobs, in the trigram table's order: their ordinals in the blob
//	          table, ascending — the first as a uvarint, each after it as a uvarint of its
//	          distance from the one before
//	trigrams  trigrams × (trigram [3] | offset u40)  ascending by trigram; where its postings
//	          begin in the postings section, each list running to the next one's offset
//	footer    trigrams u32 | reserved u32 | posting bytes u64                      (16 bytes)
//	trailer   SHA-256 of every byte before it [32]
//
// A blob's flags: 1, it holds an ESCAPE (regexlit.HasEscape), so a clause
// holding a folded literal admits it whatever its trigrams say; 2, it is
// SKIPPED (Skip) — recorded so its next tip does not read it again, never in
// a posting, and never a candidate. Any other bit is a file this build did
// not write.
//
// unicode is the version of the Unicode tables the blobs' trigrams and
// escapes were taken with: unicode.Version as Go spells it ("17.0.0"), its
// unused bytes zero. THE TABLES ARE STAMPED because both are derived from
// them — a trigram is of the blob's lowering (bytes.ToLower), an escape is
// what the tables fold with an ASCII letter and lower apart from it — and
// they move with the Go toolchain, each Unicode version giving letters a case
// they lacked: a blob holding one of Unicode 16's Garay capitals was filed by
// a build on 15.0 under the capital's own bytes, which a newer build's plan,
// lowering the searched text first, no longer looks it up by, so even a
// case-sensitive search of text it holds would rule it out. And a segment
// outlives the build that wrote it: kept across restarts, carried across
// incarnations by hard link, and never read again for a blob it holds. So a
// segment stamped with other tables is refused as one this build did not
// write (ErrMalformed), which drops it and builds the index again. A tip map
// holds nothing the tables decide, and is not stamped.
//
// THE TABLE FOLLOWS THE POSTINGS because a merge streams its postings and
// learns each list's offset only as it writes it: the table, and the counts
// that place it, come after (the footer), and the trailer is still one hash
// over everything before it — the cache's run-trailer check, written as the
// file streams, with nothing held but the table.
//
// AN OFFSET IS FIVE BYTES: a segment's postings are a byte or two for each
// distinct trigram of each blob it holds, so even a merge of a repository's
// every segment is far below the tebibyte five bytes name — and a writer that
// would pass it is refused rather than wrapped — while a table entry of eight
// bytes rather than twelve is a third less of every segment's table held
// mapped.

// The format's fixed parts.
const (
	segmentMagic      = "CRCX"
	segmentVersion    = 1
	segmentHeader     = 32
	segmentFooter     = 16
	blobEntryBytes    = oidBytes + 1
	trigramEntryBytes = 8
	trailerBytes      = sha256.Size
	// maxPostingBytes is what a five-byte offset can name.
	maxPostingBytes = 1 << 40
	// unicodeStampAt is where the header's naming of the Unicode tables
	// begins; it runs to the header's end.
	unicodeStampAt = 16
)

// unicodeStamp is the header's naming of the tables this build lowers with
// and finds escapes by.
var unicodeStamp = func() (s [segmentHeader - unicodeStampAt]byte) {
	copy(s[:], unicode.Version)
	return s
}()

// The field holds unicode.Version whole. It is a constant, so a Go whose
// version would not fit fails to compile this package, rather than writing a
// stamp cut short that another version could share.
var _ [segmentHeader - unicodeStampAt - len(unicode.Version)]struct{}

// A blob's flags.
const (
	flagEscape  = 1 << 0
	flagSkipped = 1 << 1
	knownFlags  = flagEscape | flagSkipped
)

// ErrMalformed is a segment or a tip map whose bytes are not the format's: a
// file this build did not write, or one damaged past its trailer's reach. A
// derived file, so the remedy is to drop it and build it again.
var ErrMalformed = errors.New("codeindex: a file this build does not write; drop it and the index is built again")

// SegmentSourceBytes is the most source one segment is built from: a
// builder takes blobs until it has read this much (Builder.SourceBytes) and
// the segment is written then, so what an index build holds is one segment's
// postings — a byte or two for each distinct trigram of each blob, in lists
// grown by doubling, and a table entry and a list header for each distinct
// trigram of the segment — and the blob being read beside it: ESTIMATED at
// about 256 MiB at its peak (M6's sizing), never measured yet. 64 MiB is
// PROVISIONAL: BenchmarkCodeIndex (build seconds a gigabyte, index bytes a
// source byte, query p50 and p99 at 120k files; code search's measuring
// commit) re-derives it.
const SegmentSourceBytes = 64 << 20

// MaxSegments is the most segments one repository's index keeps: past it the
// smaller ones are merged (MergeSegments), keeping only the blobs the tip
// still names, so a search reads at most this many tables a trigram and an
// index gives back what its branch no longer holds. Merged GEOMETRICALLY —
// the newest segments while each is at least half the one before it — so a
// blob is rewritten a logarithmic number of times over the index's life
// rather than at every tip. 32 is PROVISIONAL, re-derived with
// SegmentSourceBytes by BenchmarkCodeIndex.
const MaxSegments = 32

// MergeNewest says whether the two newest segments of an index are to be
// merged into one, where sizes is what each of its segments holds, oldest
// first: when there are more than MaxSegments, or when the newest holds at
// least half what the one before it does. A writer asks it again after each
// merge, and AFTER EACH SEGMENT IT APPENDS — never once over a batch of
// them. Asked as each is appended, every segment stays more than twice the
// one after it, so an index of k segments' worth of source keeps at most
// 2·log2(k) segments, and a merge grows the segment a blob is in by at
// least half, so each blob is written at most log base 1.5 of k times.
// Asked once over a batch of equal segments, the merged one is always the
// newest and always at least half the one before it, so the batch folds
// into one segment from its end, rewriting what it merged at every step:
// k²/2 segments' worth of writes for k segments' source — 189 for a
// repository of nineteen segments, where asking as each is appended writes
// 62 and keeps three.
func MergeNewest(sizes []int64) bool {
	n := len(sizes)
	return n >= 2 && (n > MaxSegments || 2*sizes[n-1] >= sizes[n-2])
}

// blobEntry is one row of a segment's blob table.
type blobEntry struct {
	oid   OID
	flags byte
}

// Builder gathers one segment: the blobs added to it, ascending by object id,
// and each trigram's postings, encoded as they arrive. Its zero value is an
// empty segment.
//
// A blob's ordinal is its place among those added, which is its place in the
// segment's blob table because blobs arrive in id order — so each posting
// list is written as the blobs come, never sorted. What a builder holds is
// its postings, a uvarint of one or two bytes for each distinct trigram of
// each blob, and its table of distinct trigrams; the blobs' bytes are not
// kept.
type Builder struct {
	blobs []blobEntry
	// lists is each trigram's place in postings.
	lists    map[Trigram]int32
	postings []building
	source   int64
	scratch  []Trigram
}

// building is one trigram's postings as they grow.
type building struct {
	last uint32
	data []byte
}

// next refuses an id that does not follow the last one added: the blob table
// is in id order, and a blob is in a segment once.
func (b *Builder) next(oid OID) error {
	if n := len(b.blobs); n > 0 && oid.compare(b.blobs[n-1].oid) <= 0 {
		return fmt.Errorf("codeindex: blob %s added after %s: a segment's blobs are added in id order, each "+
			"once", oid, b.blobs[n-1].oid)
	}
	if uint64(len(b.blobs)) >= math.MaxUint32 {
		return fmt.Errorf("codeindex: a segment holds at most %d blobs", uint32(math.MaxUint32))
	}
	return nil
}

// Add adds the blob oid, whose bytes are blob: its trigrams, and whether it
// holds an escape — or, where Skip says so, the record that it is skipped.
// oid follows every id added before it.
func (b *Builder) Add(oid OID, blob []byte) error {
	if err := b.next(oid); err != nil {
		return err
	}
	if Skip(int64(len(blob)), blob) {
		b.blobs = append(b.blobs, blobEntry{oid: oid, flags: flagSkipped})
		return nil
	}
	ordinal := uint32(len(b.blobs))
	var flags byte
	if regexlit.HasEscape(blob) {
		flags = flagEscape
	}
	b.blobs = append(b.blobs, blobEntry{oid: oid, flags: flags})
	if b.lists == nil {
		b.lists = map[Trigram]int32{}
	}
	b.scratch = appendTrigrams(b.scratch[:0], lower(blob))
	for _, t := range b.scratch {
		i, ok := b.lists[t]
		if !ok {
			b.lists[t] = int32(len(b.postings))
			b.postings = append(b.postings, building{last: ordinal,
				data: binary.AppendUvarint(nil, uint64(ordinal))})
			continue
		}
		p := &b.postings[i]
		p.data = binary.AppendUvarint(p.data, uint64(ordinal-p.last))
		p.last = ordinal
	}
	b.source += int64(len(blob))
	return nil
}

// AddUnread records the blob oid, of size bytes, as skipped without reading
// it: what a caller does with a blob its size alone puts past MaxFileBytes.
// One within that bound is refused: whether it is binary is read from its
// bytes, through Add.
func (b *Builder) AddUnread(oid OID, size int64) error {
	if !Skip(size, nil) {
		return fmt.Errorf("codeindex: blob %s is %d bytes, within the %d the index holds: add it with its bytes",
			oid, size, MaxFileBytes)
	}
	if err := b.next(oid); err != nil {
		return err
	}
	b.blobs = append(b.blobs, blobEntry{oid: oid, flags: flagSkipped})
	return nil
}

// Len is how many blobs b holds, skipped ones included.
func (b *Builder) Len() int { return len(b.blobs) }

// SourceBytes is how many bytes of blob b has indexed: what a caller bounds a
// segment by.
func (b *Builder) SourceBytes() int64 { return b.source }

// WriteSegment writes b's segment to w.
func WriteSegment(w io.Writer, b *Builder) error {
	keys := make([]Trigram, 0, len(b.lists))
	for t := range b.lists {
		keys = append(keys, t)
	}
	slices.Sort(keys)
	out, err := newSegmentWriter(w, b.blobs)
	if err != nil {
		return err
	}
	for _, t := range keys {
		out.list(t, b.postings[b.lists[t]].data)
	}
	return out.close()
}

// segmentWriter is a segment streamed to dst: buffered, hashed as it goes,
// the trigram table held until the postings are written.
type segmentWriter struct {
	dst      io.Writer
	w        *bufio.Writer
	sum      hash.Hash
	table    []byte
	written  uint64
	trigrams uint32
	last     Trigram
	err      error
}

// newSegmentWriter starts a segment of blobs on dst: its header and blob
// table.
func newSegmentWriter(dst io.Writer, blobs []blobEntry) (*segmentWriter, error) {
	if uint64(len(blobs)) > math.MaxUint32 {
		return nil, fmt.Errorf("codeindex: %d blobs, past the %d a segment holds", len(blobs),
			uint32(math.MaxUint32))
	}
	h := sha256.New()
	out := &segmentWriter{dst: dst, w: bufio.NewWriterSize(io.MultiWriter(dst, h), 64<<10), sum: h}
	header := make([]byte, segmentHeader)
	copy(header, segmentMagic)
	header[4], header[5] = segmentVersion, oidBytes
	binary.BigEndian.PutUint32(header[8:], uint32(len(blobs)))
	copy(header[unicodeStampAt:], unicodeStamp[:])
	out.put(header)
	var entry [blobEntryBytes]byte
	for _, b := range blobs {
		copy(entry[:], b.oid[:])
		entry[oidBytes] = b.flags
		out.put(entry[:])
	}
	return out, nil
}

func (o *segmentWriter) put(b []byte) {
	if o.err == nil {
		_, o.err = o.w.Write(b)
	}
}

// list writes t's postings, encoded as the format has them; t rises from one
// call to the next, and a trigram no blob holds is written as nothing.
func (o *segmentWriter) list(t Trigram, encoded []byte) {
	if o.err != nil || len(encoded) == 0 {
		return
	}
	if o.trigrams > 0 && t <= o.last {
		o.err = fmt.Errorf("codeindex: trigram %v written after %v: a segment's trigrams rise", t, o.last)
		return
	}
	if o.written+uint64(len(encoded)) > maxPostingBytes {
		o.err = fmt.Errorf("codeindex: a segment's postings past %d bytes, more than its offsets name: "+
			"cut it into smaller segments", uint64(maxPostingBytes))
		return
	}
	o.table = append(o.table, byte(t>>16), byte(t>>8), byte(t),
		byte(o.written>>32), byte(o.written>>24), byte(o.written>>16), byte(o.written>>8), byte(o.written))
	o.put(encoded)
	o.written += uint64(len(encoded))
	o.trigrams++
	o.last = t
}

// close writes the trigram table, the footer and the trailer — the trailer
// straight to the destination, past the hash, since it is no part of what it
// sums.
func (o *segmentWriter) close() error {
	o.put(o.table)
	footer := make([]byte, segmentFooter)
	binary.BigEndian.PutUint32(footer, o.trigrams)
	binary.BigEndian.PutUint64(footer[8:], o.written)
	o.put(footer)
	if o.err != nil {
		return o.err
	}
	if err := o.w.Flush(); err != nil {
		return err
	}
	_, err := o.dst.Write(o.sum.Sum(nil))
	return err
}

// appendPostings appends ordinals, ascending, encoded as a posting list.
func appendPostings(dst []byte, ordinals []uint32) []byte {
	var prev uint32
	for i, o := range ordinals {
		if i == 0 {
			dst = binary.AppendUvarint(dst, uint64(o))
		} else {
			dst = binary.AppendUvarint(dst, uint64(o-prev))
		}
		prev = o
	}
	return dst
}

// Segment is a segment's bytes as a search and a merge read them: a mapped
// file, or bytes in memory. Its trailer is the caller's to have checked —
// the cache's run-trailer check — and OpenSegment holds its sections to its
// length, so no read lands outside it; a posting list is held to the blob
// table as it is read.
type Segment struct {
	blobs    []byte
	postings []byte
	table    []byte
	count    int
	trigrams int
	// escapes is the ordinals of the blobs that hold an escape, ascending.
	escapes []uint32
}

// OpenSegment is data, a whole segment, as a search reads it.
func OpenSegment(data []byte) (*Segment, error) {
	if len(data) < segmentHeader+segmentFooter+trailerBytes || string(data[:4]) != segmentMagic {
		return nil, fmt.Errorf("%w: no CRCX header", ErrMalformed)
	}
	if data[4] != segmentVersion || data[5] != oidBytes {
		return nil, fmt.Errorf("%w: version %d of %d-byte ids, and this build writes version %d of %d",
			ErrMalformed, data[4], data[5], segmentVersion, oidBytes)
	}
	if binary.BigEndian.Uint16(data[6:]) != 0 || binary.BigEndian.Uint32(data[12:]) != 0 {
		return nil, fmt.Errorf("%w: a reserved field of the header is set", ErrMalformed)
	}
	if stamp := data[unicodeStampAt:segmentHeader]; !bytes.Equal(stamp, unicodeStamp[:]) {
		return nil, fmt.Errorf("%w: its trigrams and escapes were taken with the Unicode tables of %q, and "+
			"this build's are %s's", ErrMalformed, bytes.TrimRight(stamp, "\x00"), unicode.Version)
	}
	count := uint64(binary.BigEndian.Uint32(data[8:]))
	foot := data[len(data)-trailerBytes-segmentFooter:]
	trigrams := uint64(binary.BigEndian.Uint32(foot))
	postings := binary.BigEndian.Uint64(foot[8:])
	if binary.BigEndian.Uint32(foot[4:]) != 0 {
		return nil, fmt.Errorf("%w: a reserved field of the footer is set", ErrMalformed)
	}
	blobsEnd := segmentHeader + count*blobEntryBytes
	size := uint64(len(data))
	if postings > size || blobsEnd > size {
		return nil, fmt.Errorf("%w: its sections run past its %d bytes", ErrMalformed, len(data))
	}
	postingsEnd := blobsEnd + postings
	tableEnd := postingsEnd + trigrams*trigramEntryBytes
	if tableEnd+segmentFooter+trailerBytes != size {
		return nil, fmt.Errorf("%w: its sections do not add up to its %d bytes", ErrMalformed, len(data))
	}
	s := &Segment{
		blobs:    data[segmentHeader:blobsEnd],
		postings: data[blobsEnd:postingsEnd],
		table:    data[postingsEnd:tableEnd],
		count:    int(count),
		trigrams: int(trigrams),
	}
	for i := range s.count {
		flags := s.flags(i)
		if flags&^knownFlags != 0 || flags == flagEscape|flagSkipped {
			return nil, fmt.Errorf("%w: blob %d is flagged %#x", ErrMalformed, i, flags)
		}
		if i > 0 && s.OID(i).compare(s.OID(i-1)) <= 0 {
			return nil, fmt.Errorf("%w: its blobs do not rise by id", ErrMalformed)
		}
		if flags&flagEscape != 0 {
			s.escapes = append(s.escapes, uint32(i))
		}
	}
	// Each list holds a byte at least, so the offsets rise from zero within
	// the postings; the trigrams rise with them.
	for i := range s.trigrams {
		t, off := s.entry(i)
		switch {
		case i == 0 && off != 0:
			return nil, fmt.Errorf("%w: its first posting list does not begin its postings", ErrMalformed)
		case off >= postings:
			return nil, fmt.Errorf("%w: a posting list begins past its postings", ErrMalformed)
		case i > 0:
			prev, prevOff := s.entry(i - 1)
			if t <= prev || off <= prevOff {
				return nil, fmt.Errorf("%w: its trigram table does not rise", ErrMalformed)
			}
		}
	}
	if s.trigrams == 0 && postings != 0 {
		return nil, fmt.Errorf("%w: postings no trigram names", ErrMalformed)
	}
	return s, nil
}

// Len is how many blobs s holds, skipped ones included.
func (s *Segment) Len() int { return s.count }

// OID is blob i's object id.
func (s *Segment) OID(i int) OID {
	var o OID
	copy(o[:], s.blobs[i*blobEntryBytes:])
	return o
}

// Skipped reports whether blob i is skipped: past MaxFileBytes, or binary.
func (s *Segment) Skipped(i int) bool { return s.flags(i)&flagSkipped != 0 }

func (s *Segment) flags(i int) byte { return s.blobs[i*blobEntryBytes+oidBytes] }

// Find is the place of the blob oid in s, or false where s does not hold it.
func (s *Segment) Find(oid OID) (int, bool) {
	lo, hi := 0, s.count
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if s.OID(mid).compare(oid) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < s.count && s.OID(lo) == oid {
		return lo, true
	}
	return 0, false
}

// entry is trigram table entry i: the trigram and where its postings begin.
func (s *Segment) entry(i int) (Trigram, uint64) {
	e := s.table[i*trigramEntryBytes:]
	t := Trigram(uint32(e[0])<<16 | uint32(e[1])<<8 | uint32(e[2]))
	off := uint64(e[3])<<32 | uint64(e[4])<<24 | uint64(e[5])<<16 | uint64(e[6])<<8 | uint64(e[7])
	return t, off
}

// list is entry i's posting list, encoded.
func (s *Segment) list(i int) []byte {
	_, from := s.entry(i)
	to := uint64(len(s.postings))
	if i+1 < s.trigrams {
		_, to = s.entry(i + 1)
	}
	return s.postings[from:to]
}

// lookup is the encoded posting list of t, or false where no blob of s holds
// it.
func (s *Segment) lookup(t Trigram) ([]byte, bool) {
	lo, hi := 0, s.trigrams
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if got, _ := s.entry(mid); got < t {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < s.trigrams {
		if got, _ := s.entry(lo); got == t {
			return s.list(lo), true
		}
	}
	return nil, false
}

// ordinals reads one posting list of s in order, holding each ordinal to s's
// blob table as it is read — above the one before it, and below the table's
// count — so a list that is not the format's is refused by the read that
// meets it and never followed past the table. The ONE reader of a list:
// decoding one, narrowing a set by one and keeping one decoded as far as a
// lookup has needed it (lookup.go) all read through it. A read that stops
// part way has neither checked nor answered what lies past where it stopped.
type ordinals struct {
	list  []byte
	count uint64
	// at is the ordinal last read, once one has been (began).
	at    uint32
	began bool
	// err is why the read stopped short of the list's end, if it did.
	err error
}

// ordinals is a read of list, a posting list of s, from its start.
func (s *Segment) ordinals(list []byte) ordinals {
	return ordinals{list: list, count: uint64(s.count)}
}

// next reads the list's next ordinal into at, and reports false at the
// list's end, or at bytes that are not the format's, which err then names.
func (o *ordinals) next() bool {
	if o.err != nil || len(o.list) == 0 {
		return false
	}
	v, n := binary.Uvarint(o.list)
	if n <= 0 {
		o.err = fmt.Errorf("%w: a posting list that is not uvarints", ErrMalformed)
		return false
	}
	o.list = o.list[n:]
	if o.began {
		if v == 0 || v > o.count {
			o.err = fmt.Errorf("%w: a posting list that does not rise within its blobs", ErrMalformed)
			return false
		}
		v += uint64(o.at)
	}
	if v >= o.count {
		o.err = fmt.Errorf("%w: a posting names blob %d of %d", ErrMalformed, v, o.count)
		return false
	}
	o.at, o.began = uint32(v), true
	return true
}

// decode appends the ordinals list encodes to dst, held to s's blob table:
// each below its count, each above the one before.
func (s *Segment) decode(list []byte, dst []uint32) ([]uint32, error) {
	o := s.ordinals(list)
	for o.next() {
		dst = append(dst, o.at)
	}
	if o.err != nil {
		return nil, o.err
	}
	return dst, nil
}

// MergeSegments writes to w the one segment holding every blob of segs that
// keep keeps — each once, whichever segments held it, with its trigrams — and
// nothing of any other: how a repository's index sheds the blobs its tip no
// longer names and keeps its segments few. A nil keep keeps every blob.
//
// It writes the bytes a Builder given the kept blobs would, so a merged
// segment is indistinguishable from one built whole — stamped with this
// build's Unicode tables, as every input is: a Segment is made only by
// OpenSegment, which refuses one stamped with others. It streams: what it
// holds is each input's map from its ordinals to the merged ones, the merged
// blob table, the trigram table it writes last, and one trigram's lists at a
// time.
func MergeSegments(w io.Writer, segs []*Segment, keep func(OID) bool) error {
	// THE BLOBS: the inputs' tables merged by id, each id once. A dropped
	// blob maps to no ordinal a segment can hold.
	const dropped = math.MaxUint32
	remaps := make([][]uint32, len(segs))
	for k, s := range segs {
		remaps[k] = make([]uint32, s.count)
	}
	var blobs []blobEntry
	at := make([]int, len(segs))
	for {
		best := -1
		for k, s := range segs {
			if at[k] < s.count && (best < 0 || s.OID(at[k]).compare(segs[best].OID(at[best])) < 0) {
				best = k
			}
		}
		if best < 0 {
			break
		}
		oid, flags := segs[best].OID(at[best]), segs[best].flags(at[best])
		ordinal := uint32(dropped)
		if keep == nil || keep(oid) {
			if uint64(len(blobs)) >= math.MaxUint32 {
				return fmt.Errorf("codeindex: a merge past the %d blobs a segment holds: merge fewer segments "+
					"at once", uint32(math.MaxUint32))
			}
			ordinal = uint32(len(blobs))
			blobs = append(blobs, blobEntry{oid: oid, flags: flags})
		}
		for k, s := range segs {
			if at[k] >= s.count || s.OID(at[k]) != oid {
				continue
			}
			if s.flags(at[k]) != flags {
				return fmt.Errorf("%w: two segments flag blob %s %#x and %#x", ErrMalformed, oid, flags,
					s.flags(at[k]))
			}
			remaps[k][at[k]] = ordinal
			at[k]++
		}
	}
	out, err := newSegmentWriter(w, blobs)
	if err != nil {
		return err
	}
	// THE POSTINGS: the inputs' tables merged by trigram, each list mapped
	// onto the merged ordinals — rising still, since both tables are in id
	// order — and the lists of one trigram merged, by one union kept from
	// trigram to trigram. The next trigram, as the next blob above, is
	// found by a scan of the inputs' heads: k steps for each one written,
	// never for each posting, which is what the lists folded in one at a
	// time cost.
	var u union
	var encoded []byte
	clear(at)
	for {
		var t Trigram
		found := false
		for k, s := range segs {
			if at[k] < s.trigrams {
				if got, _ := s.entry(at[k]); !found || got < t {
					t, found = got, true
				}
			}
		}
		if !found {
			break
		}
		u.reset()
		for k, s := range segs {
			if at[k] >= s.trigrams {
				continue
			}
			if got, _ := s.entry(at[k]); got != t {
				continue
			}
			// Decoded straight into the union and mapped where it lies,
			// the dropped left out.
			from := len(u.buf)
			if u.buf, err = s.decode(s.list(at[k]), u.buf); err != nil {
				return err
			}
			kept := u.buf[:from]
			for _, o := range u.buf[from:] {
				if n := remaps[k][o]; n != dropped {
					kept = append(kept, n)
				}
			}
			u.buf = kept
			u.end()
			at[k]++
		}
		encoded = appendPostings(encoded[:0], u.merged())
		out.list(t, encoded)
	}
	return out.close()
}

// union is ascending lists of ordinals, none holding one twice, merged into
// one ascending list holding each once: a trigram's lists across the
// segments a merge reads, a clause's blobs across its literals.
//
// THE LISTS ARE MERGED IN PAIRS, a round at a time, between two buffers kept
// from one union to the next: k lists of L ordinals in all take L·⌈log₂ k⌉
// steps and, once the buffers have grown, allocate nothing. Folding them in
// one at a time, each into a fresh slice, copies what is merged so far once
// for each list after it — k·L steps and as much garbage: merging 696 163
// postings of 512 blobs allocated 69 bytes a posting from 32 segments and 4.9
// from 2, and allocates 0.37 from either merged in pairs
// (TestAMergeAllocatesForWhatItWritesHoweverManyItMerges).
type union struct {
	// buf is the lists back to back; ends is where each ends in it.
	buf  []uint32
	ends []int
	// spare and spareEnds are what a round merges into.
	spare     []uint32
	spareEnds []int
}

// reset empties u for its next union.
func (u *union) reset() { u.buf, u.ends = u.buf[:0], u.ends[:0] }

// add adds list, ascending, to the union.
func (u *union) add(list []uint32) {
	u.buf = append(u.buf, list...)
	u.end()
}

// end ends the list appended to buf since the one before it ended — what a
// caller decoding a list straight into buf adds it with. An empty list adds
// nothing.
func (u *union) end() {
	last := 0
	if n := len(u.ends); n > 0 {
		last = u.ends[n-1]
	}
	if len(u.buf) > last {
		u.ends = append(u.ends, len(u.buf))
	}
}

// merged is the union of the lists added since u was reset: ascending, each
// ordinal once. It is u's own storage, valid until u is next used.
func (u *union) merged() []uint32 {
	for len(u.ends) > 1 {
		if cap(u.spare) < len(u.buf) {
			// Made afresh, never grown: nothing it holds is read again, and
			// growing it copies what an earlier round merged into it — and,
			// built with the race detector, which does not elide the append
			// of a make that slices.Grow is, allocates the growth twice. As
			// long as buf can hold, which append grows a factor at a time,
			// so the spare too is made a logarithmic number of times however
			// the unions it merges grow from one to the next.
			u.spare = make([]uint32, 0, cap(u.buf))
		}
		u.spare, u.spareEnds = u.spare[:0], u.spareEnds[:0]
		from := 0
		for i := 0; i < len(u.ends); i += 2 {
			if i+1 < len(u.ends) {
				u.spare = appendUnion(u.spare, u.buf[from:u.ends[i]], u.buf[u.ends[i]:u.ends[i+1]])
				from = u.ends[i+1]
			} else {
				u.spare = append(u.spare, u.buf[from:u.ends[i]]...)
				from = u.ends[i]
			}
			u.spareEnds = append(u.spareEnds, len(u.spare))
		}
		u.buf, u.spare = u.spare, u.buf
		u.ends, u.spareEnds = u.spareEnds, u.ends
	}
	if len(u.ends) == 0 {
		return u.buf[:0]
	}
	return u.buf[:u.ends[0]]
}

// appendUnion appends to dst the ascending union of a and b, both ascending
// with no element twice, each element once.
func appendUnion(dst, a, b []uint32) []uint32 {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			dst = append(dst, a[i])
			i++
		case a[i] > b[j]:
			dst = append(dst, b[j])
			j++
		default:
			dst = append(dst, a[i])
			i++
			j++
		}
	}
	dst = append(dst, a[i:]...)
	return append(dst, b[j:]...)
}

// intersect is the ascending intersection of a and b, both ascending; it
// reuses a's storage.
func intersect(a, b []uint32) []uint32 {
	out := a[:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}
