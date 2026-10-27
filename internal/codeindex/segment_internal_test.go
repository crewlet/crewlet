package codeindex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// fixture is a blob of the round trip: its id and bytes, or the size of one
// too large to read.
type fixture struct {
	oid    OID
	text   []byte
	unread int64
}

// fixtures hold every kind of blob a segment records.
var fixtures = []fixture{
	{oid: testOID(1), text: []byte("package main\n\nfunc main() { println(\"hi\") }\n")},
	{oid: testOID(2), text: []byte("Straße ſession ÉTÉ\n")},   // an escape
	{oid: testOID(3), text: []byte("PNG\x00\x01binary")},      // binary: skipped
	{oid: testOID(4), unread: MaxFileBytes + 1},               // too large: skipped unread
	{oid: testOID(5), text: []byte("caf\xe9 au lait \xff\n")}, // not UTF-8
	{oid: testOID(6), text: []byte("")},                       // empty: in no posting
	{oid: testOID(7), text: []byte("ab")},                     // shorter than a trigram
}

// built is the segment of the fixtures keep keeps (nil keeps every one), as
// a Builder writes it.
func built(t *testing.T, keep func(OID) bool) []byte {
	t.Helper()
	var b Builder
	for _, f := range fixtures {
		if keep != nil && !keep(f.oid) {
			continue
		}
		var err error
		if f.unread > 0 {
			err = b.AddUnread(f.oid, f.unread)
		} else {
			err = b.Add(f.oid, f.text)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := WriteSegment(&out, &b); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// only keeps the fixtures numbered ns.
func only(ns ...int) func(OID) bool {
	return func(oid OID) bool {
		return slices.ContainsFunc(ns, func(n int) bool { return testOID(n) == oid })
	}
}

// opened is data opened as a segment.
func opened(t *testing.T, data []byte) *Segment {
	t.Helper()
	s, err := OpenSegment(data)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// merged is the merge of the segments data, by keep.
func merged(t *testing.T, keep func(OID) bool, data ...[]byte) []byte {
	t.Helper()
	var segs []*Segment
	for _, d := range data {
		segs = append(segs, opened(t, d))
	}
	var out bytes.Buffer
	if err := MergeSegments(&out, segs, keep); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// postingsByBlob is, for each blob of s any posting names, the trigrams whose
// postings name it, ascending.
func postingsByBlob(t *testing.T, s *Segment) map[OID][]Trigram {
	t.Helper()
	out := map[OID][]Trigram{}
	for i := range s.trigrams {
		tg, _ := s.entry(i)
		ordinals, err := s.decode(s.list(i), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range ordinals {
			out[s.OID(int(o))] = append(out[s.OID(int(o))], tg)
		}
	}
	return out
}

// trailed reports whether data's last 32 bytes are the SHA-256 of the rest:
// the cache's run-trailer check.
func trailed(data []byte) bool {
	sum := sha256.Sum256(data[:len(data)-trailerBytes])
	return bytes.Equal(sum[:], data[len(data)-trailerBytes:])
}

// A SEGMENT ROUND-TRIPS, AND A MERGE KEEPS EXACTLY WHAT ITS KEEP KEEPS.
//
// Every kind of blob a segment records — text, an escape, a binary and a
// too-large one skipped, bytes that are not UTF-8, an empty blob and one
// shorter than a trigram — reads back with its flags and with exactly its
// trigrams (Trigrams) in the postings, under a trailer the cache's run-trailer
// check holds. A merge writes the very bytes a build of the kept blobs writes,
// whatever segments held them and however many held one: a blob two inputs
// share is one blob, a dropped one is in no posting, and a merge of one whole
// segment is that segment.
func TestASegmentRoundTripsAndMergesByTheKeep(t *testing.T) {
	t.Parallel()
	whole := built(t, nil)
	if !trailed(whole) {
		t.Fatal("a segment's last 32 bytes are not the SHA-256 of every byte before them")
	}
	if stamp := string(bytes.TrimRight(whole[unicodeStampAt:segmentHeader], "\x00")); stamp != unicode.Version {
		t.Errorf("a segment names the Unicode tables %q, and this build lowers with %s's", stamp, unicode.Version)
	}
	s := opened(t, whole)
	if s.Len() != len(fixtures) {
		t.Fatalf("the segment holds %d blobs, want %d", s.Len(), len(fixtures))
	}
	byBlob := postingsByBlob(t, s)
	for i, f := range fixtures {
		skipped := f.unread > 0 || Binary(f.text)
		if s.OID(i) != f.oid || s.Skipped(i) != skipped {
			t.Errorf("blob %d is %s skipped %v, want %s skipped %v", i, s.OID(i), s.Skipped(i), f.oid, skipped)
		}
		if at, ok := s.Find(f.oid); !ok || at != i {
			t.Errorf("Find(%s) = %d, %v; want %d, true", f.oid, at, ok, i)
		}
		if want := Trigrams(f.text); !skipped && !slices.Equal(byBlob[f.oid], want) {
			t.Errorf("blob %s's postings name it under %v, and its trigrams are %v", f.oid, byBlob[f.oid], want)
		}
		if skipped && len(byBlob[f.oid]) > 0 {
			t.Errorf("the skipped blob %s is in the postings of %v", f.oid, byBlob[f.oid])
		}
	}
	if !slices.Equal(s.escapes, []uint32{1}) {
		t.Errorf("the blobs flagged for an escape are %v, want the second alone", s.escapes)
	}
	if _, ok := s.Find(testOID(9)); ok {
		t.Error("Find answers a blob the segment does not hold")
	}

	// THE MERGE IS THE BUILD OF WHAT IT KEEPS. Blob 2 is in both inputs.
	a, b := built(t, only(1, 2, 3, 4)), built(t, only(2, 5, 6, 7))
	if got, want := merged(t, only(2, 3, 4, 5, 7), a, b), built(t, only(2, 3, 4, 5, 7)); !bytes.Equal(got, want) {
		t.Error("merging two segments by a keep writes other bytes than building the blobs it keeps")
	}
	if got := merged(t, nil, a, b); !bytes.Equal(got, whole) {
		t.Error("merging two segments keeping everything writes other bytes than building every blob")
	}
	if got := merged(t, nil, whole); !bytes.Equal(got, whole) {
		t.Error("a merge of one whole segment is not that segment")
	}
	if got := merged(t, func(OID) bool { return false }, a, b); opened(t, got).Len() != 0 {
		t.Error("a merge keeping nothing holds a blob")
	}
	empty := merged(t, nil)
	if e := opened(t, empty); e.Len() != 0 || e.trigrams != 0 || !trailed(empty) {
		t.Error("a merge of no segments is not an empty segment")
	}
}

// A SEGMENT THIS BUILD DID NOT WRITE IS REFUSED, never read past its bytes —
// at its open where its structure says so, and where a posting list does,
// by the read that meets it.
//
// One written under other Unicode tables is such a file, though every byte
// of it is well formed: its trigrams were taken over a lowering by those
// tables and its escape flags found by them, and a blob it holds is never
// indexed again, so a newer build reading it would rule out a blob holding a
// letter its tables gave a case after the segment was written.
func TestASegmentThisBuildDidNotWriteIsRefused(t *testing.T) {
	t.Parallel()
	whole := built(t, nil)
	s := opened(t, whole)
	blobsAt := segmentHeader
	tableAt := len(whole) - trailerBytes - segmentFooter - s.trigrams*trigramEntryBytes
	stamped := func(version string) func([]byte) []byte {
		return func(d []byte) []byte {
			clear(d[unicodeStampAt:segmentHeader])
			copy(d[unicodeStampAt:segmentHeader], version)
			return d
		}
	}
	older := stamped("15.0.0")
	if _, err := OpenSegment(older(slices.Clone(whole))); !errors.Is(err, ErrMalformed) ||
		!strings.Contains(err.Error(), `"15.0.0"`) || !strings.Contains(err.Error(), unicode.Version) {
		t.Errorf("a segment lowered with Unicode 15.0.0's tables opens, or its refusal names neither "+
			"version: %v", err)
	}
	for name, damage := range map[string]func([]byte) []byte{
		"cut short":           func(d []byte) []byte { return d[:len(d)-1] },
		"another magic":       func(d []byte) []byte { d[0] = 'X'; return d },
		"another version":     func(d []byte) []byte { d[4] = 2; return d },
		"a reserved bit":      func(d []byte) []byte { d[7] = 1; return d },
		"other tables":        older,
		"no tables named":     stamped(""),
		"a version and bytes": stamped(unicode.Version + "\x00x"),
		"an unknown flag":     func(d []byte) []byte { d[blobsAt+oidBytes] = 0x80; return d },
		"both flags":          func(d []byte) []byte { d[blobsAt+oidBytes] = flagEscape | flagSkipped; return d },
		"blobs out of order": func(d []byte) []byte {
			first := slices.Clone(d[blobsAt : blobsAt+blobEntryBytes])
			copy(d[blobsAt:], d[blobsAt+blobEntryBytes:blobsAt+2*blobEntryBytes])
			copy(d[blobsAt+blobEntryBytes:], first)
			return d
		},
		"trigrams out of order": func(d []byte) []byte {
			first := slices.Clone(d[tableAt : tableAt+trigramEntryBytes])
			copy(d[tableAt:], d[tableAt+trigramEntryBytes:tableAt+2*trigramEntryBytes])
			copy(d[tableAt+trigramEntryBytes:], first)
			return d
		},
		"a count past the file": func(d []byte) []byte {
			binary.BigEndian.PutUint32(d[8:], 1<<31)
			return d
		},
	} {
		if _, err := OpenSegment(damage(slices.Clone(whole))); !errors.Is(err, ErrMalformed) {
			t.Errorf("a segment %s opens: %v", name, err)
		}
	}

	// A POSTING THAT NAMES NO BLOB: the first list's first ordinal made one
	// past the table. The open does not read the postings; every read of
	// that list refuses it.
	d := slices.Clone(whole)
	d[blobsAt+len(fixtures)*blobEntryBytes] = 0x7f
	bad := opened(t, d)
	if _, err := bad.decode(bad.list(0), nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("a posting naming blob 127 of %d decodes: %v", len(fixtures), err)
	}
	var out bytes.Buffer
	if err := MergeSegments(&out, []*Segment{bad}, nil); !errors.Is(err, ErrMalformed) {
		t.Errorf("a merge reads a posting naming no blob: %v", err)
	}
}

// A MERGE ALLOCATES FOR WHAT IT WRITES, HOWEVER MANY SEGMENTS IT MERGES.
//
// A trigram's lists, one from each segment holding it, are merged in pairs
// between two buffers kept from trigram to trigram (union), so what a merge
// allocates is its tables and those buffers, never anything a posting.
// Folded in one at a time, each into a fresh slice, they copied what was
// merged so far once for each segment after it — k·L work and as much
// garbage, which over 32 segments was some seventy bytes allocated a posting
// written. The same 512 blobs of about 2 KiB of words, their ids interleaved
// across the segments, are merged from 2 segments and from 32: both write
// the bytes a build of every blob writes, the 32-way merge allocates what
// the 2-way one does, and each less than it writes.
func TestAMergeAllocatesForWhatItWritesHoweverManyItMerges(t *testing.T) {
	// Not parallel: runtime.MemStats counts every goroutine's allocations,
	// and a parallel case's would be counted as the merge's.
	rng := rand.New(rand.NewPCG(20261027, 3))
	words := make([]string, 300)
	for i := range words {
		w := make([]byte, 3+rng.IntN(8))
		for j := range w {
			w[j] = byte('a' + rng.IntN(26))
		}
		words[i] = string(w)
	}
	const blobs = 512
	texts := make([][]byte, blobs)
	for i := range texts {
		var b bytes.Buffer
		for b.Len() < 2<<10 {
			b.WriteString(words[rng.IntN(len(words))])
			if rng.IntN(12) == 0 {
				b.WriteByte('\n')
			} else {
				b.WriteByte(' ')
			}
		}
		texts[i] = b.Bytes()
	}
	write := func(k, j int) []byte {
		var b Builder
		for i := j; i < blobs; i += k {
			if err := b.Add(testOID(i+1), texts[i]); err != nil {
				t.Fatal(err)
			}
		}
		var out bytes.Buffer
		if err := WriteSegment(&out, &b); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	whole := write(1, 0)
	postings := 0
	for _, trigrams := range postingsByBlob(t, opened(t, whole)) {
		postings += len(trigrams)
	}
	merge := func(k int) uint64 {
		segs := make([]*Segment, k)
		for j := range segs {
			segs[j] = opened(t, write(k, j))
		}
		out := bytes.NewBuffer(make([]byte, 0, len(whole)))
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		err := MergeSegments(out, segs, nil)
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), whole) {
			t.Fatalf("merging the blobs from %d segments writes other bytes than building them whole", k)
		}
		return after.TotalAlloc - before.TotalAlloc
	}
	// Five leaves a list unpaired in two of its rounds.
	two, five, many := merge(2), merge(5), merge(32)
	t.Logf("%d postings in %d bytes; a merge of 2 segments allocated %d bytes, of 5 %d, of 32 %d", postings,
		len(whole), two, five, many)
	if most := max(five, many); most > two+two/8 {
		t.Errorf("merges of 5 and 32 segments allocated %d and %d bytes and one of 2 %d: what a merge allocates "+
			"grows with the segments it merges", five, many, two)
	}
	if limit := uint64(len(whole)); max(two, five, many) > limit {
		t.Errorf("merges writing %d bytes (%d postings) allocated %d (2 segments), %d (5) and %d (32): more "+
			"than they write", len(whole), postings, two, five, many)
	}
}

// A BUILDER TAKES BLOBS IN ID ORDER, EACH ONCE, and reads every blob within
// the bound.
func TestABuilderTakesEachBlobOnceInOrder(t *testing.T) {
	t.Parallel()
	var b Builder
	if err := b.Add(testOID(5), []byte("five")); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(testOID(4), []byte("four")); err == nil {
		t.Error("a blob added after a larger id is taken")
	}
	if err := b.Add(testOID(5), []byte("five")); err == nil {
		t.Error("a blob added twice is taken")
	}
	if err := b.AddUnread(testOID(6), MaxFileBytes); err == nil {
		t.Error("a blob within the bound is recorded skipped unread")
	}
	if err := b.AddUnread(testOID(6), MaxFileBytes+1); err != nil {
		t.Errorf("a blob past the bound is not recorded unread: %v", err)
	}
	if b.Len() != 2 || b.SourceBytes() != int64(len("five")) {
		t.Errorf("the builder holds %d blobs and %d bytes of source, want 2 and 4", b.Len(), b.SourceBytes())
	}
}

// A TIP MAP ROUND-TRIPS, its files in byte order whatever order they came in.
//
// A path need not be UTF-8, a blob may be at two paths, and a skipped one
// keeps its flag; the trailer is the cache's run-trailer check's.
func TestATipMapRoundTrips(t *testing.T) {
	t.Parallel()
	tip := testOID(99)
	entries := []TipEntry{
		{Path: "internal/code/doc.go", Blob: testOID(1)},
		{Path: "README.md", Blob: testOID(2)},
		{Path: "assets/logo.png", Blob: testOID(3), Skipped: true},
		{Path: "caf\xe9.txt", Blob: testOID(4)},
		{Path: "internal/code/doc_test.go", Blob: testOID(1)},
	}
	var out bytes.Buffer
	if err := WriteTipMap(&out, tip, entries); err != nil {
		t.Fatal(err)
	}
	data := out.Bytes()
	if !trailed(data) {
		t.Fatal("a tip map's last 32 bytes are not the SHA-256 of every byte before it")
	}
	m, err := OpenTipMap(data)
	if err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(entries)
	slices.SortFunc(want, func(a, b TipEntry) int { return bytes.Compare([]byte(a.Path), []byte(b.Path)) })
	if m.Tip() != tip || m.Len() != len(want) {
		t.Fatalf("the map is of %s with %d files, want %s with %d", m.Tip(), m.Len(), tip, len(want))
	}
	for i, e := range want {
		if string(m.Path(i)) != e.Path || m.Blob(i) != e.Blob || m.Skipped(i) != e.Skipped {
			t.Errorf("entry %d is %q %s skipped %v, want %q %s skipped %v", i, m.Path(i), m.Blob(i),
				m.Skipped(i), e.Path, e.Blob, e.Skipped)
		}
	}

	for name, bad := range map[string][]TipEntry{
		"a path twice":  {{Path: "a", Blob: testOID(1)}, {Path: "a", Blob: testOID(2)}},
		"an empty path": {{Path: "", Blob: testOID(1)}},
		"a NUL":         {{Path: "a\x00b", Blob: testOID(1)}},
	} {
		if err := WriteTipMap(&bytes.Buffer{}, tip, bad); err == nil {
			t.Errorf("a tip map with %s is written", name)
		}
	}
	for name, damage := range map[string]func([]byte) []byte{
		"cut short":       func(d []byte) []byte { return d[:len(d)-1] },
		"another magic":   func(d []byte) []byte { d[0] = 'X'; return d },
		"another version": func(d []byte) []byte { d[4] = 2; return d },
		"an unknown flag": func(d []byte) []byte { d[tipHeader+12] = 0x80; return d },
		"paths out of order": func(d []byte) []byte {
			// The first two entries' paths, "README.md" and
			// "assets/logo.png", swapped with their lengths.
			e := d[tipHeader:]
			binary.BigEndian.PutUint32(e[8:], uint32(len("assets/logo.png")))
			binary.BigEndian.PutUint64(e[tipEntryBytes:], uint64(len("assets/logo.png")))
			binary.BigEndian.PutUint32(e[tipEntryBytes+8:], uint32(len("README.md")))
			at := tipHeader + len(want)*tipEntryBytes
			copy(d[at:], "assets/logo.pngREADME.md")
			return d
		},
	} {
		if _, err := OpenTipMap(damage(slices.Clone(data))); !errors.Is(err, ErrMalformed) {
			t.Errorf("a tip map %s opens: %v", name, err)
		}
	}
	var none bytes.Buffer
	if err := WriteTipMap(&none, tip, nil); err != nil {
		t.Fatal(err)
	}
	if m, err := OpenTipMap(none.Bytes()); err != nil || m.Len() != 0 || m.Tip() != tip {
		t.Errorf("an empty tip map reads back %v, %v", m, err)
	}
}
