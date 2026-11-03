package gittest

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// DAMAGE, ONE IMPLEMENTATION. Every case a test damages a repository's files
// with — the pack, its index and reverse index, the multi-pack-index with its
// bitmap and reverse index, the commit-graph — is written here once, every
// byte offset read from the file's own header and table of contents, never
// assumed: gitproc's format table is held to what git says of each case
// (classify_test.go), the git cache's check, scrub and repair are held to
// what they find of the same cases (internal/code/cache), and the code host's
// requests are held to what they answer over them (internal/code/host). A
// second copy in one of those suites would be a case the others no longer
// measure — ADR-0008's rule, one implementation of a shared rule.
//
// A case takes the repository's directory and, where it damages one object's
// bytes or one entry of a table, the object ids it names (each case's doc
// says which). The PACK of a case is the one whose index holds the first id;
// a case naming "every" pack, index or reverse index takes all of the
// repository's.
//
// A copy is measured in one of two SHAPES ([Bare], [Served]), because git
// answers the same damage differently on each: a copy before its first
// maintenance holds only packs, and every git process reads a pack body
// through its index; a copy the maintainer has served from also holds a
// bitmapped multi-pack-index and a commit-graph, and there upload-pack's
// pack-objects copies a stored object to the client as it is stored —
// damaged bytes from any pack, a damaged entry header from the bitmap's
// preferred pack ([PreferredPack]), which single reuse sends without reading
// it — and walks read commits from the graph, so no process names that
// damage at all.

// DamageCase is one damage [Damage] does to a repository's files; its value
// is the case's name in the measurements every suite is held to.
type DamageCase string

// The damage of a pack — the stored bytes of a push — and of its index and
// reverse index, the stored files beside it.
const (
	// PackRemoved removes the pack holding ids[0]: its .pack, .idx and .rev.
	PackRemoved DamageCase = "A"
	// IdxTruncated truncates the index of the pack holding ids[0] to 100
	// bytes.
	IdxTruncated DamageCase = "B"
	// PackFlipped flips eight bytes in the middle of the pack holding
	// ids[0].
	PackFlipped DamageCase = "C"
	// PackTruncated truncates the pack holding ids[0] to a third.
	PackTruncated DamageCase = "D"
	// EntryBytes flips six of the compressed bytes of ids[0]'s pack entry.
	EntryBytes DamageCase = "G"
	// EntryType sets ids[0]'s pack entry's type to 5, which git reserves.
	EntryType DamageCase = "G1"
	// EntrySize adds one to ids[0]'s pack entry's stated size.
	EntrySize DamageCase = "S1"
	// EntryTail zeroes the last four bytes of ids[0]'s pack entry: its
	// compressed stream's own checksum.
	EntryTail DamageCase = "S2"
	// IdxOffsetsPastPack sets every offset of EVERY pack index to 0x000fffff,
	// past the end of each pack.
	IdxOffsetsPastPack DamageCase = "M"
	// IdxOffset sets ids[0]'s offset in its pack's index to ids[1]'s, an
	// object of the same pack: a lookup of ids[0] reads ids[1].
	IdxOffset DamageCase = "IO"
	// IdxTrailer flips one byte of the trailer of the index of the pack
	// holding ids[0]: the SHA-1 of every byte before it.
	IdxTrailer DamageCase = "IT"
	// RevTruncated truncates EVERY pack's reverse index to 20 bytes.
	RevTruncated DamageCase = "R1"
	// RevSignature overwrites EVERY pack's reverse index's signature.
	RevSignature DamageCase = "R2"
	// RevVersion sets EVERY pack's reverse index's version to 9.
	RevVersion DamageCase = "R3"
	// RevHashID sets EVERY pack's reverse index's hash id to 7.
	RevHashID DamageCase = "R4"
	// RevAppended appends four bytes to EVERY pack's reverse index.
	RevAppended DamageCase = "R5"
	// RevSwapped swaps the first two positions of the reverse index of the
	// pack holding ids[0]: every position still in range, two in the wrong
	// order.
	RevSwapped DamageCase = "R6"
	// RevOverwritten overwrites every position of the reverse index of the
	// pack holding ids[0] with 0xff: positions past its objects.
	RevOverwritten DamageCase = "L"
	// RevTrailer flips one byte of the trailer of the reverse index of the
	// pack holding ids[0].
	RevTrailer DamageCase = "RT"
	// DeltaData mangles the zlib header of ids[0]'s data, an OFS_DELTA.
	DeltaData DamageCase = "P"
	// DeltaBase sets the first byte of ids[0]'s base offset, an OFS_DELTA's,
	// to 0xff: past the pack's start.
	DeltaBase DamageCase = "P2"
	// DeltaBaseData mangles the zlib header of ids[1]'s data, the base
	// ids[0] is a delta on.
	DeltaBaseData DamageCase = "P3"
)

// The damage of a multi-pack-index — derived by the node from its packs — and
// of its bitmap and reverse index.
const (
	// MidxRidx overwrites the multi-pack-index's reverse index (its RIDX
	// chunk) with 0xff, every position past its objects.
	MidxRidx DamageCase = "E"
	// MidxSignature overwrites the multi-pack-index's signature.
	MidxSignature DamageCase = "F"
	// MidxTruncated truncates the multi-pack-index to 20 bytes.
	MidxTruncated DamageCase = "F2"
	// MidxVersion sets the multi-pack-index's version to 9.
	MidxVersion DamageCase = "F3"
	// MidxHashVersion sets the multi-pack-index's hash version to 7.
	MidxHashVersion DamageCase = "F4"
	// MidxNoChunks sets the multi-pack-index's chunk count to 0.
	MidxNoChunks DamageCase = "F5"
	// MidxFirstChunkZeroed zeroes the id of the multi-pack-index's first
	// chunk.
	MidxFirstChunkZeroed DamageCase = "F6"
	// MidxPackNamesMissing renames the multi-pack-index's PNAM chunk.
	MidxPackNamesMissing DamageCase = "F7"
	// MidxFanoutMissing renames the multi-pack-index's OIDF chunk.
	MidxFanoutMissing DamageCase = "F8"
	// MidxLookupMissing renames the multi-pack-index's OIDL chunk.
	MidxLookupMissing DamageCase = "F8L"
	// MidxOffsetsMissing renames the multi-pack-index's OOFF chunk.
	MidxOffsetsMissing DamageCase = "F8O"
	// MidxPackNamesOrder makes the multi-pack-index's first pack name sort
	// after every other.
	MidxPackNamesOrder DamageCase = "F9"
	// MidxChunkPastEnd sets the multi-pack-index's second chunk's offset to
	// 2^40.
	MidxChunkPastEnd DamageCase = "F10"
	// MidxChunkMisaligned moves the multi-pack-index's second chunk's offset
	// one byte on.
	MidxChunkMisaligned DamageCase = "F11"
	// MidxChunkTwice names the multi-pack-index's first chunk's id again in
	// its second row.
	MidxChunkTwice DamageCase = "F12"
	// MidxFanoutSize moves the offset of the chunk the multi-pack-index's
	// table lists after its OIDF chunk four bytes on: the table still
	// aligned, and the fanout, whose size git reads off that offset, four
	// bytes longer than its 256 entries.
	MidxFanoutSize DamageCase = "F13"
	// MidxFanout sets the multi-pack-index's first fanout entry to
	// 0xffffffff.
	MidxFanout DamageCase = "FO"
	// MidxOffset sets ids[0]'s object offset in the multi-pack-index to
	// ids[1]'s, an object of the same pack: a lookup of ids[0] through the
	// index reads ids[1].
	MidxOffset DamageCase = "MO"
	// MidxTrailer flips one byte of the multi-pack-index's trailer — which
	// also names its bitmap, so the bitmap is orphaned.
	MidxTrailer DamageCase = "MT"
	// BitmapSignature overwrites the multi-pack-index's bitmap's signature.
	BitmapSignature DamageCase = "K1"
	// BitmapTruncated truncates the multi-pack-index's bitmap to 30 bytes.
	BitmapTruncated DamageCase = "K2"
	// BitmapVersion sets the multi-pack-index's bitmap's version to 9.
	BitmapVersion DamageCase = "K3"
	// BitmapBody flips eight bytes in the middle of the multi-pack-index's
	// bitmap.
	BitmapBody DamageCase = "K4"
	// BitmapTrailer flips one byte of the multi-pack-index's bitmap's
	// trailer.
	BitmapTrailer DamageCase = "KT"
	// BitmapEntries flips the lowest bit of the first literal word of every
	// commit bitmap the multi-pack-index's bitmap stores whole (one stored
	// as the XOR of another changes through that one): a well-formed bitmap
	// naming one object more or fewer for every commit it covers, so every
	// walk that reads it answers wrong — what BitmapBody does where its
	// flipped bytes land in a bitmap a walk reads, which in a small history
	// they need not.
	BitmapEntries DamageCase = "K5"
)

// The damage of a commit-graph — derived by the node from its packs.
const (
	// GraphSignature overwrites the commit-graph's signature.
	GraphSignature DamageCase = "I"
	// GraphTruncated truncates the commit-graph to 20 bytes.
	GraphTruncated DamageCase = "I2"
	// GraphVersion sets the commit-graph's version to 9.
	GraphVersion DamageCase = "I3"
	// GraphHashVersion sets the commit-graph's hash version to 7.
	GraphHashVersion DamageCase = "I4"
	// GraphFirstChunkZeroed zeroes the id of the commit-graph's first chunk.
	GraphFirstChunkZeroed DamageCase = "I5"
	// GraphNoChunks sets the commit-graph's chunk count to 0.
	GraphNoChunks DamageCase = "I6"
	// GraphChunkShifted moves the commit-graph's first chunk's offset three
	// bytes on.
	GraphChunkShifted DamageCase = "I7"
	// GraphParentPast sets ids[0]'s first-parent position in the
	// commit-graph past its commits.
	GraphParentPast DamageCase = "I8"
	// GraphParentSelf sets ids[0]'s first-parent position in the
	// commit-graph to its own.
	GraphParentSelf DamageCase = "I9"
	// GraphParentOther sets ids[0]'s first-parent position in the
	// commit-graph to ids[1]'s, another commit of the graph: a walk from
	// ids[0] then reaches ids[1]'s history in its parent's place.
	GraphParentOther DamageCase = "I9b"
	// GraphFanoutMissing renames the commit-graph's OIDF chunk.
	GraphFanoutMissing DamageCase = "I10"
	// GraphGeneration sets the overflow bit of ids[0]'s generation in the
	// commit-graph, which holds no overflow chunk.
	GraphGeneration DamageCase = "I11"
	// GraphDate flips the lowest bit of ids[0]'s commit date in the
	// commit-graph.
	GraphDate DamageCase = "IB"
	// GraphFanout sets the commit-graph's first fanout entry to 0xffffffff.
	GraphFanout DamageCase = "GFO"
	// GraphTrailer flips one byte of the commit-graph's trailer.
	GraphTrailer DamageCase = "GT"
)

// DamageCases is every [DamageCase], in the order the measurements list
// them.
var DamageCases = []DamageCase{
	PackRemoved, IdxTruncated, PackFlipped, PackTruncated, EntryBytes, EntryType, EntrySize, EntryTail,
	IdxOffsetsPastPack, IdxOffset, IdxTrailer, RevTruncated, RevSignature, RevVersion, RevHashID, RevAppended,
	RevSwapped, RevOverwritten, RevTrailer, DeltaData, DeltaBase, DeltaBaseData,
	MidxRidx, MidxSignature, MidxTruncated, MidxVersion, MidxHashVersion, MidxNoChunks, MidxFirstChunkZeroed,
	MidxPackNamesMissing, MidxFanoutMissing, MidxLookupMissing, MidxOffsetsMissing, MidxPackNamesOrder,
	MidxChunkPastEnd, MidxChunkMisaligned, MidxChunkTwice, MidxFanoutSize, MidxFanout, MidxOffset, MidxTrailer,
	BitmapSignature, BitmapTruncated, BitmapVersion, BitmapBody, BitmapTrailer, BitmapEntries,
	GraphSignature, GraphTruncated, GraphVersion, GraphHashVersion, GraphFirstChunkZeroed, GraphNoChunks,
	GraphChunkShifted, GraphParentPast, GraphParentSelf, GraphParentOther, GraphFanoutMissing, GraphGeneration,
	GraphDate, GraphFanout, GraphTrailer,
}

// Valid reports whether c is one of [DamageCases].
func (c DamageCase) Valid() bool { return slices.Contains(DamageCases, c) }

// ids is how many object ids each case names; a case not listed names none.
var ids = map[DamageCase]int{
	PackRemoved: 1, IdxTruncated: 1, PackFlipped: 1, PackTruncated: 1, EntryBytes: 1, EntryType: 1, EntrySize: 1,
	EntryTail: 1, IdxOffset: 2, IdxTrailer: 1, RevSwapped: 1, RevOverwritten: 1, RevTrailer: 1, DeltaData: 1,
	DeltaBase: 1, DeltaBaseData: 2, MidxOffset: 2, GraphParentPast: 1, GraphParentSelf: 1, GraphParentOther: 2,
	GraphGeneration: 1, GraphDate: 1,
}

// Damage does c to the repository at repo, naming the objects of ids the case
// takes (each case's doc says which), and fails tb on a case that is no
// [DamageCase], a wrong count of ids, or a repository missing the file the
// case damages. Every write is made in place, to the file's own inode, made
// writable first since git writes its files read-only; [DamageInPlace] also
// keeps the file's identity.
func Damage(tb testing.TB, repo string, c DamageCase, ids ...string) {
	tb.Helper()
	d := &damager{tb: tb, repo: repo}
	d.do(c, ids)
}

// DamageInPlace is [Damage] that leaves every file it writes with the
// IDENTITY it had — the same inode, size, modification time and mode — as
// media damage does: what a cache vouches for by its identity, it cannot tell
// was written. It fails tb for a case that changes a file's size or inode (a
// truncation, an append, a removal), which no in-place damage is.
func DamageInPlace(tb testing.TB, repo string, c DamageCase, ids ...string) {
	tb.Helper()
	d := &damager{tb: tb, repo: repo, keep: map[string]os.FileInfo{}}
	d.do(c, ids)
	for path, before := range d.keep {
		after, err := os.Lstat(path)
		if err != nil {
			tb.Fatalf("%s is gone after an in-place damage: %v", path, err)
		}
		if !os.SameFile(before, after) || before.Size() != after.Size() {
			tb.Fatalf("case %s changed %s's inode or size, which no in-place damage does", c, path)
		}
		if err := os.Chmod(path, before.Mode().Perm()); err != nil {
			tb.Fatal(err)
		}
		mtime := before.ModTime()
		if err := os.Chtimes(path, time.Time{}, mtime); err != nil {
			tb.Fatal(err)
		}
	}
}

// damager is one damage's writes: keep, for an in-place damage, every file's
// identity before its first write.
type damager struct {
	tb   testing.TB
	repo string
	keep map[string]os.FileInfo
}

// do runs case c.
func (d *damager) do(c DamageCase, oids []string) {
	tb := d.tb
	tb.Helper()
	if !c.Valid() {
		tb.Fatalf("%q is no damage case: gittest.DamageCases lists them", c)
	}
	if len(oids) != ids[c] {
		tb.Fatalf("damage case %s names %d object ids, and was given %d", c, ids[c], len(oids))
	}
	switch c {
	case PackRemoved:
		stem := d.packOf(oids[0])
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			if err := os.Remove(stem + ext); err != nil {
				tb.Fatal(err)
			}
		}
	case IdxTruncated:
		d.truncate(d.packOf(oids[0])+".idx", 100)
	case PackFlipped:
		pack := d.packOf(oids[0]) + ".pack"
		mid := d.size(pack) / 2
		d.overwrite(pack, mid, mangled(d.bytesAt(pack, mid, 8)))
	case PackTruncated:
		pack := d.packOf(oids[0]) + ".pack"
		d.truncate(pack, d.size(pack)/3)
	case EntryBytes:
		stem := d.packOf(oids[0])
		_, data := d.entryHeader(stem, oids[0])
		d.overwrite(stem+".pack", data+4, mangled(d.bytesAt(stem+".pack", data+4, 6)))
	case EntryType:
		stem := d.packOf(oids[0])
		start, _ := d.entryHeader(stem, oids[0])
		b := d.bytesAt(stem+".pack", start, 1)[0]
		d.overwrite(stem+".pack", start, []byte{b&0x8f | 5<<4})
	case EntrySize:
		stem := d.packOf(oids[0])
		start, _ := d.entryHeader(stem, oids[0])
		b := d.bytesAt(stem+".pack", start, 1)[0]
		if b&0x0f == 0x0f {
			tb.Fatalf("%s's entry's size nibble is full: %#x", oids[0], b)
		}
		d.overwrite(stem+".pack", start, []byte{b + 1})
	case EntryTail:
		stem := d.packOf(oids[0])
		_, end := PackEntry(tb, stem+".idx", oids[0])
		d.overwrite(stem+".pack", end-4, []byte{0, 0, 0, 0})
	case IdxOffsetsPastPack:
		for _, stem := range d.packs() {
			idx := stem + ".idx"
			n := d.idxObjects(idx)
			d.overwrite(idx, idxOffsets(n), bytes.Repeat([]byte{0x00, 0x0f, 0xff, 0xff}, int(n)))
		}
	case IdxOffset:
		stem := d.packOf(oids[0])
		if other := d.packOf(oids[1]); other != stem {
			tb.Fatalf("%s and %s are in different packs: an index's offsets name its own pack's objects", oids[0], oids[1])
		}
		idx := stem + ".idx"
		n := d.idxObjects(idx)
		at, from := idxOffsets(n)+4*d.idxPosition(idx, oids[0]), idxOffsets(n)+4*d.idxPosition(idx, oids[1])
		d.overwrite(idx, at, d.bytesAt(idx, from, 4))
	case IdxTrailer:
		d.flipTrailer(d.packOf(oids[0]) + ".idx")
	case RevTruncated, RevSignature, RevVersion, RevHashID, RevAppended:
		for _, stem := range d.packs() {
			rev := stem + ".rev"
			switch c {
			case RevTruncated:
				d.truncate(rev, 20)
			case RevSignature:
				d.overwrite(rev, 0, []byte{0xde, 0xad, 0xbe, 0xef})
			case RevVersion:
				d.overwrite(rev, 7, []byte{9})
			case RevHashID:
				d.overwrite(rev, 11, []byte{7})
			case RevAppended:
				d.appendTo(rev, []byte{0, 0, 0, 0})
			}
		}
	case RevSwapped:
		rev := d.packOf(oids[0]) + ".rev"
		if d.size(rev) < revHeader+8+2*trailer {
			tb.Fatalf("%s holds fewer than two positions", rev)
		}
		first, second := d.bytesAt(rev, revHeader, 4), d.bytesAt(rev, revHeader+4, 4)
		if bytes.Equal(first, second) {
			tb.Fatalf("%s's first two positions are equal", rev)
		}
		d.overwrite(rev, revHeader, append(slices.Clone(second), first...))
	case RevOverwritten:
		rev := d.packOf(oids[0]) + ".rev"
		body := d.size(rev) - revHeader - 2*trailer
		d.overwrite(rev, revHeader, bytes.Repeat([]byte{0xff}, int(body)))
	case RevTrailer:
		d.flipTrailer(d.packOf(oids[0]) + ".rev")
	case DeltaData:
		stem := d.packOf(oids[0])
		at := d.deltaData(stem, oids[0])
		d.overwrite(stem+".pack", at, mangled(d.bytesAt(stem+".pack", at, 2)))
	case DeltaBase:
		stem := d.packOf(oids[0])
		_, data := d.entryHeader(stem, oids[0])
		d.overwrite(stem+".pack", data, []byte{0xff})
	case DeltaBaseData:
		stem := d.packOf(oids[1])
		_, at := d.entryHeader(stem, oids[1])
		d.overwrite(stem+".pack", at, mangled(d.bytesAt(stem+".pack", at, 2)))
	default:
		d.derived(c, oids)
	}
}

// derived runs a case of a multi-pack-index's, its bitmap's or a
// commit-graph's.
func (d *damager) derived(c DamageCase, oids []string) {
	tb := d.tb
	tb.Helper()
	midx := filepath.Join(d.repo, "objects", "pack", "multi-pack-index")
	graph := filepath.Join(d.repo, "objects", "info", "commit-graph")
	switch c {
	case MidxRidx:
		start, end := MidxChunk(tb, midx, "RIDX")
		d.overwrite(midx, start, bytes.Repeat([]byte{0xff}, int(end-start)))
	case MidxSignature:
		d.overwrite(midx, 0, []byte{0xde, 0xad, 0xbe, 0xef})
	case MidxTruncated:
		d.truncate(midx, 20)
	case MidxVersion:
		d.overwrite(midx, 4, []byte{9})
	case MidxHashVersion:
		d.overwrite(midx, 5, []byte{7})
	case MidxNoChunks:
		d.overwrite(midx, 6, []byte{0})
	case MidxFirstChunkZeroed:
		d.overwrite(midx, ChunkRow(MidxHeader, 0), []byte{0, 0, 0, 0})
	case MidxPackNamesMissing:
		d.renameChunk(midx, MidxHeader, "PNAM", "XNAM")
	case MidxFanoutMissing:
		d.renameChunk(midx, MidxHeader, "OIDF", "XIDF")
	case MidxLookupMissing:
		d.renameChunk(midx, MidxHeader, "OIDL", "XIDL")
	case MidxOffsetsMissing:
		d.renameChunk(midx, MidxHeader, "OOFF", "XOFF")
	case MidxPackNamesOrder:
		start, _ := MidxChunk(tb, midx, "PNAM")
		d.overwrite(midx, start, []byte("z")) // "pack-…" sorts after every other "pack-…"
	case MidxChunkPastEnd:
		d.tableOffset(midx, MidxHeader, 1, func(uint64) uint64 { return 1 << 40 })
	case MidxChunkMisaligned:
		d.tableOffset(midx, MidxHeader, 1, func(off uint64) uint64 { return off + 1 })
	case MidxChunkTwice:
		first := ChunkRow(MidxHeader, 0)
		d.overwrite(midx, ChunkRow(MidxHeader, 1), d.bytesAt(midx, first, 4))
	case MidxFanoutSize:
		d.tableOffset(midx, MidxHeader, d.chunkRow(midx, MidxHeader, "OIDF")+1, func(off uint64) uint64 { return off + 4 })
	case MidxFanout:
		start, _ := MidxChunk(tb, midx, "OIDF")
		d.overwrite(midx, start, []byte{0xff, 0xff, 0xff, 0xff})
	case MidxOffset:
		offsets, _ := MidxChunk(tb, midx, "OOFF")
		at := offsets + 8*int64(d.lookupPosition(midx, MidxHeader, oids[0]))
		from := offsets + 8*int64(d.lookupPosition(midx, MidxHeader, oids[1]))
		entry, other := d.bytesAt(midx, at, 8), d.bytesAt(midx, from, 8)
		if !bytes.Equal(entry[:4], other[:4]) {
			tb.Fatalf("%s and %s are in different packs of the multi-pack-index: an offset names its own pack's object",
				oids[0], oids[1])
		}
		d.overwrite(midx, at+4, other[4:])
	case MidxTrailer:
		d.flipTrailer(midx)
	case BitmapSignature:
		d.overwrite(d.bitmap(), 0, []byte{0xde, 0xad, 0xbe, 0xef})
	case BitmapTruncated:
		d.truncate(d.bitmap(), 30)
	case BitmapVersion:
		d.overwrite(d.bitmap(), 4, []byte{0, 9})
	case BitmapBody:
		bitmap := d.bitmap()
		mid := d.size(bitmap) / 2
		d.overwrite(bitmap, mid, mangled(d.bytesAt(bitmap, mid, 8)))
	case BitmapTrailer:
		d.flipTrailer(d.bitmap())
	case BitmapEntries:
		bitmap := d.bitmap()
		for _, at := range d.bitmapEntryWords(bitmap) {
			// A literal word is stored big-endian: its lowest bit is in its
			// last byte.
			d.overwrite(bitmap, at+7, []byte{d.bytesAt(bitmap, at+7, 1)[0] ^ 0x01})
		}
	case GraphSignature:
		d.overwrite(graph, 0, []byte{0xde, 0xad, 0xbe, 0xef})
	case GraphTruncated:
		d.truncate(graph, 20)
	case GraphVersion:
		d.overwrite(graph, 4, []byte{9})
	case GraphHashVersion:
		d.overwrite(graph, 5, []byte{7})
	case GraphFirstChunkZeroed:
		d.overwrite(graph, ChunkRow(GraphHeader, 0), []byte{0, 0, 0, 0})
	case GraphNoChunks:
		d.overwrite(graph, 6, []byte{0})
	case GraphChunkShifted:
		d.tableOffset(graph, GraphHeader, 0, func(off uint64) uint64 { return off + 3 })
	case GraphParentPast:
		d.overwrite(graph, d.graphCommit(graph, oids[0])+20, []byte{0x00, 0xff, 0xff, 0xff})
	case GraphParentSelf, GraphParentOther:
		parent := oids[0]
		if c == GraphParentOther {
			parent = oids[1]
		}
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], d.lookupPosition(graph, GraphHeader, parent))
		d.overwrite(graph, d.graphCommit(graph, oids[0])+20, b[:])
	case GraphFanoutMissing:
		d.renameChunk(graph, GraphHeader, "OIDF", "XIDF")
	case GraphGeneration:
		start, _ := GraphChunk(tb, graph, "GDA2")
		at := start + 4*int64(d.lookupPosition(graph, GraphHeader, oids[0]))
		d.overwrite(graph, at, []byte{0x80 | d.bytesAt(graph, at, 1)[0]})
	case GraphDate:
		// The entry's last eight bytes are the generation (30 bits) and
		// the date (34 bits); the last byte is the date's lowest.
		at := d.graphCommit(graph, oids[0]) + graphEntry - 1
		d.overwrite(graph, at, []byte{d.bytesAt(graph, at, 1)[0] ^ 1})
	case GraphFanout:
		start, _ := GraphChunk(tb, graph, "OIDF")
		d.overwrite(graph, start, []byte{0xff, 0xff, 0xff, 0xff})
	case GraphTrailer:
		d.flipTrailer(graph)
	default:
		tb.Fatalf("damage case %s has no body", c)
	}
}

// The layouts the cases read: a hashfile's trailer, a reverse index's header,
// a commit-graph's commit-data entry (the root tree, two parent positions,
// the generation and date).
const (
	trailer    = 20
	revHeader  = 12
	graphEntry = 36
)

// idxOffsets is where the four-byte offsets table of a version-2 pack index
// of n objects begins: a magic and a version, a 256-entry fanout, the sorted
// names, their CRCs.
func idxOffsets(n int64) int64 { return 8 + 256*4 + n*24 }

// packs is every pack of the repository, by its path without an extension.
func (d *damager) packs() []string {
	d.tb.Helper()
	found, err := filepath.Glob(filepath.Join(d.repo, "objects", "pack", "pack-*.idx"))
	if err != nil {
		d.tb.Fatal(err)
	}
	if len(found) == 0 {
		d.tb.Fatalf("%s holds no pack", d.repo)
	}
	stems := make([]string, len(found))
	for i, idx := range found {
		stems[i] = strings.TrimSuffix(idx, ".idx")
	}
	slices.Sort(stems)
	return stems
}

// packOf is the pack whose index holds oid.
func (d *damager) packOf(oid string) string {
	d.tb.Helper()
	for _, stem := range d.packs() {
		if d.idxHolds(stem+".idx", oid) {
			return stem
		}
	}
	d.tb.Fatalf("no pack of %s holds %s", d.repo, oid)
	return ""
}

// idxObjects is how many objects the version-2 index at idx holds.
func (d *damager) idxObjects(idx string) int64 {
	d.tb.Helper()
	head := d.bytesAt(idx, 0, 8+256*4)
	if string(head[:4]) != "\xfftOc" || binary.BigEndian.Uint32(head[4:8]) != 2 {
		d.tb.Fatalf("%s is no version-2 pack index", idx)
	}
	return int64(binary.BigEndian.Uint32(head[8+255*4:]))
}

// idxHolds reports whether the version-2 index at idx names oid.
func (d *damager) idxHolds(idx, oid string) bool {
	return d.idxSearch(idx, oid) >= 0
}

// idxPosition is oid's position among the names of the index at idx.
func (d *damager) idxPosition(idx, oid string) int64 {
	d.tb.Helper()
	at := d.idxSearch(idx, oid)
	if at < 0 {
		d.tb.Fatalf("%s holds no %s", idx, oid)
	}
	return at
}

// idxSearch is oid's position among the names of the index at idx, or -1.
func (d *damager) idxSearch(idx, oid string) int64 {
	d.tb.Helper()
	want, err := hex.DecodeString(oid)
	if err != nil || len(want) != 20 {
		d.tb.Fatalf("%q is no object id", oid)
	}
	body := d.read(idx)
	n := d.idxObjects(idx)
	names := int64(8 + 256*4)
	for i := range n {
		if bytes.Equal(body[names+20*i:names+20*i+20], want) {
			return i
		}
	}
	return -1
}

// lookupPosition is oid's position in the OIDL chunk of the chunk-format file
// at path, under a header of header bytes: its position in the multi-pack-index
// or the commit-graph.
func (d *damager) lookupPosition(path string, header int, oid string) uint32 {
	d.tb.Helper()
	want, err := hex.DecodeString(oid)
	if err != nil || len(want) != 20 {
		d.tb.Fatalf("%q is no object id", oid)
	}
	body := d.read(path)
	lookup, end := chunk(d.tb, path, header, "OIDL")
	for i := int64(0); lookup+20*i < end; i++ {
		if bytes.Equal(body[lookup+20*i:lookup+20*i+20], want) {
			return uint32(i)
		}
	}
	d.tb.Fatalf("%s holds no %s", path, oid)
	return 0
}

// graphCommit is where commit oid's CDAT entry begins in the commit-graph at
// path.
func (d *damager) graphCommit(path, oid string) int64 {
	d.tb.Helper()
	data, _ := GraphChunk(d.tb, path, "CDAT")
	return data + graphEntry*int64(d.lookupPosition(path, GraphHeader, oid))
}

// entryHeader is where oid's entry begins in the pack at stem, and where its
// type-and-size header ends: a byte, and a byte more for each seven bits.
func (d *damager) entryHeader(stem, oid string) (start, data int64) {
	d.tb.Helper()
	start, _ = PackEntry(d.tb, stem+".idx", oid)
	data = start
	for d.bytesAt(stem+".pack", data, 1)[0]&0x80 != 0 {
		data++
	}
	return start, data + 1
}

// deltaData is where the compressed data of oid, an OFS_DELTA, begins in the
// pack at stem: past its header and past its base offset.
func (d *damager) deltaData(stem, oid string) int64 {
	d.tb.Helper()
	start, at := d.entryHeader(stem, oid)
	if typ := d.bytesAt(stem+".pack", start, 1)[0] >> 4 & 7; typ != 6 {
		d.tb.Fatalf("%s is stored as type %d, not an OFS_DELTA (6)", oid, typ)
	}
	for d.bytesAt(stem+".pack", at, 1)[0]&0x80 != 0 {
		at++
	}
	return at + 1
}

// bitmap is the multi-pack-index's bitmap file.
func (d *damager) bitmap() string {
	d.tb.Helper()
	found, _ := filepath.Glob(filepath.Join(d.repo, "objects", "pack", "multi-pack-index-*.bitmap"))
	if len(found) != 1 {
		d.tb.Fatalf("%s's multi-pack-index has %d bitmaps, want one", d.repo, len(found))
	}
	return found[0]
}

// bitmapEntryWords is the offset of the first literal word of every commit
// bitmap the .bitmap at path stores whole — those whose XOR offset is zero —
// read from the file's own header: its signature, version, options, entry
// count and the index's checksum (32 bytes), the four type bitmaps, then each
// entry's commit position, XOR offset and flags (six bytes) before its EWAH
// bitmap. An EWAH bitmap is its bit size and word count (four bytes each),
// its words — the first a running-length word, whose top 31 bits count the
// literal words after it — and the position of its last running-length word
// (four bytes).
func (d *damager) bitmapEntryWords(path string) []int64 {
	d.tb.Helper()
	b := d.read(path)
	if len(b) < 32 || string(b[:4]) != "BITM" {
		d.tb.Fatalf("%s is no bitmap: its header is %q", path, b[:min(len(b), 4)])
	}
	entries := binary.BigEndian.Uint32(b[8:12])
	at := int64(32)
	ewah := func() (words uint32, first int64) {
		if at+8 > int64(len(b)) {
			d.tb.Fatalf("%s ends inside an EWAH bitmap's header at %d", path, at)
		}
		words, first = binary.BigEndian.Uint32(b[at+4:at+8]), at+8
		at += 8 + 8*int64(words) + 4
		return words, first
	}
	for range 4 {
		ewah()
	}
	var out []int64
	for range entries {
		xor := b[at+4]
		at += 6
		words, first := ewah()
		if xor != 0 {
			continue
		}
		if words < 2 || binary.BigEndian.Uint64(b[first:first+8])>>33 == 0 {
			d.tb.Fatalf("%s's entry at %d holds no literal word after its first run", path, first-14)
		}
		out = append(out, first+8)
	}
	if len(out) == 0 {
		d.tb.Fatalf("%s stores no commit bitmap whole", path)
	}
	return out
}

// renameChunk writes to over the id of chunk from in the table of contents of
// the chunk-format file at path.
func (d *damager) renameChunk(path string, header int, from, to string) {
	d.tb.Helper()
	body := d.read(path)
	for i := range int(body[6]) {
		at := ChunkRow(header, i)
		if string(body[at:at+4]) == from {
			d.overwrite(path, at, []byte(to))
			return
		}
	}
	d.tb.Fatalf("%s has no %s chunk", path, from)
}

// chunkRow is the row of chunk id in the table of contents of the
// chunk-format file at path, failing the test on a file with no such chunk.
func (d *damager) chunkRow(path string, header int, id string) int {
	d.tb.Helper()
	body := d.read(path)
	for i := range int(body[6]) {
		if at := ChunkRow(header, i); string(body[at:at+4]) == id {
			return i
		}
	}
	d.tb.Fatalf("%s has no %s chunk", path, id)
	return 0
}

// tableOffset sets row i's offset in the chunk-format file at path to set's
// answer over it.
func (d *damager) tableOffset(path string, header, i int, set func(uint64) uint64) {
	d.tb.Helper()
	at := ChunkRow(header, i) + 4
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], set(binary.BigEndian.Uint64(d.bytesAt(path, at, 8))))
	d.overwrite(path, at, b[:])
}

// flipTrailer flips the last byte of the file at path: a byte of its
// hashfile trailer.
func (d *damager) flipTrailer(path string) {
	d.tb.Helper()
	at := d.size(path) - 1
	d.overwrite(path, at, []byte{d.bytesAt(path, at, 1)[0] ^ 0xff})
}

// touch records path's identity before its first write, for an in-place
// damage, and makes it writable.
func (d *damager) touch(path string) {
	d.tb.Helper()
	if d.keep != nil {
		if _, ok := d.keep[path]; !ok {
			info, err := os.Lstat(path)
			if err != nil {
				d.tb.Fatal(err)
			}
			d.keep[path] = info
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		d.tb.Fatal(err)
	}
}

func (d *damager) overwrite(path string, at int64, b []byte) {
	d.tb.Helper()
	d.touch(path)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		d.tb.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, at); err != nil {
		d.tb.Fatal(err)
	}
}

func (d *damager) truncate(path string, size int64) {
	d.tb.Helper()
	d.touch(path)
	if err := os.Truncate(path, size); err != nil {
		d.tb.Fatal(err)
	}
}

func (d *damager) appendTo(path string, b []byte) {
	d.tb.Helper()
	d.touch(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		d.tb.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		d.tb.Fatal(err)
	}
}

func (d *damager) read(path string) []byte {
	d.tb.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		d.tb.Fatal(err)
	}
	return body
}

// bytesAt is n bytes of path from at.
func (d *damager) bytesAt(path string, at, n int64) []byte {
	d.tb.Helper()
	body := d.read(path)
	if at < 0 || at+n > int64(len(body)) {
		d.tb.Fatalf("%s is %d bytes, and a case reads %d at %d", path, len(body), n, at)
	}
	return body[at : at+n]
}

func (d *damager) size(path string) int64 {
	d.tb.Helper()
	info, err := os.Stat(path)
	if err != nil {
		d.tb.Fatal(err)
	}
	return info.Size()
}

// mangled is every byte of b XORed with 0xff.
func mangled(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = c ^ 0xff
	}
	return out
}

// THE FIXTURE every damage case was measured on: four commits, the fourth
// adding a 256 KiB file, packed the way a cache holds a repository — commits
// one to three in an OLDER pack, the fourth in a NEWER one — main at the
// fourth.

// Fixture is the measured history, in a work tree of its own.
type Fixture struct {
	Client  *Client
	Work    string
	Commits [4]string
}

// NewFixture commits the measured history in a fresh work tree.
func NewFixture(tb testing.TB) *Fixture {
	tb.Helper()
	f := &Fixture{Client: NewClient(tb)}
	f.Work = f.Client.Init(tb)
	f.Commits[0] = f.Client.Commit(tb, f.Work, map[string]string{"a.txt": "one\n"}, "one")
	f.Commits[1] = f.Client.Commit(tb, f.Work, map[string]string{"a.txt": "two\n"}, "two")
	f.Commits[2] = f.Client.Commit(tb, f.Work, map[string]string{"b.txt": "three\n"}, "three")
	f.Commits[3] = f.Client.Commit(tb, f.Work, map[string]string{"big.txt": BigFile()}, "four")
	return f
}

// Tip is the newest commit.
func (f *Fixture) Tip() string { return f.Commits[3] }

// BigFile is 256 KiB of base64 over a seeded stream, so its bytes, its blob
// and every pack checksum holding it are the same on every run.
func BigFile() string {
	r := rand.New(rand.NewPCG(1, 2))
	raw := make([]byte, 192<<10)
	for i := range raw {
		raw[i] = byte(r.Uint32())
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// Shaped is a copy of a fixture's history: the repository, and its two packs'
// paths without their extension.
type Shaped struct {
	Dir, Older, Newer string
}

// Bare is a fresh copy of f in the shape of a cache before its first
// maintenance: the two packs with their indexes and reverse indexes, and the
// refs.
func Bare(tb testing.TB, f *Fixture) Shaped {
	tb.Helper()
	dir := Empty(tb)
	oc := Pack(tb, f.Client, dir, f.Work, f.Commits[2])
	nc := Pack(tb, f.Client, dir, f.Work, f.Tip(), "^"+f.Commits[2])
	SetRefs(tb, dir, map[string]string{"refs/heads/main": f.Tip()})
	pack := filepath.Join(dir, "objects", "pack")
	return Shaped{Dir: dir, Older: filepath.Join(pack, "pack-"+oc), Newer: filepath.Join(pack, "pack-"+nc)}
}

// Served is a fresh copy of f in the shape every copy a cache has served from
// settles in: [Bare], and the maintainer's two files written over it by the
// git under test ([WriteServed]).
func Served(tb testing.TB, f *Fixture) Shaped {
	tb.Helper()
	s := Bare(tb, f)
	WriteServed(tb, f.Client, s.Dir)
	return s
}

// WriteServed writes the cache maintainer's two files into the repository at
// dir with c's git: the multi-pack-index with its bitmap, the bitmap carrying
// its lookup table, and the commit-graph of every commit its refs reach.
func WriteServed(tb testing.TB, c *Client, dir string) {
	tb.Helper()
	gitDir := "--git-dir=" + dir
	c.Run(tb, "", gitDir, "-c", "core.multiPackIndex=true", "-c", "pack.writeBitmapLookupTable=true",
		"multi-pack-index", "write", "--bitmap", "--no-progress")
	c.Run(tb, "", gitDir, "commit-graph", "write", "--reachable", "--no-progress")
}

// PreferredPack is the pack a served copy's multi-pack-index PREFERS — its
// path without an extension — read from the index as git reads it
// (midx_preferred_pack): the object at the first position of the index's
// pseudo-pack order, its reverse index (the RIDX chunk), is the preferred
// pack's, and that object's row of the object offsets (OOFF) names its pack by
// the pack's position among the index's pack names (PNAM). With
// pack.allowPackReuse=single, a clone is sent that pack's objects as they are
// stored, and every other object as pack-objects packs any object. Which pack
// it is was the writer's choice — for an index written with no
// --preferred-pack, the pack whose .pack has the oldest modification time to
// the second, and among packs of one second whichever the writer listed first
// (an index it replaces in that index's order, then the directory's) — so a
// case that needs the pack reads it here rather than presume one. It fails tb
// on a copy with no multi-pack-index or one with no reverse index, which every
// git at the node floor writes beside a bitmap.
func PreferredPack(tb testing.TB, repo string) string {
	tb.Helper()
	pack := filepath.Join(repo, "objects", "pack")
	midx := filepath.Join(pack, "multi-pack-index")
	body, err := os.ReadFile(midx)
	if err != nil {
		tb.Fatal(err)
	}
	ridx, ridxEnd := MidxChunk(tb, midx, "RIDX")
	offsets, offsetsEnd := MidxChunk(tb, midx, "OOFF")
	names, namesEnd := MidxChunk(tb, midx, "PNAM")
	if ridxEnd-ridx < 4 || ridxEnd > int64(len(body)) {
		tb.Fatalf("%s's reverse index holds no position", midx)
	}
	at := offsets + 8*int64(binary.BigEndian.Uint32(body[ridx:]))
	if at+8 > offsetsEnd || offsetsEnd > int64(len(body)) {
		tb.Fatalf("%s's reverse index names an object past its offsets", midx)
	}
	id := binary.BigEndian.Uint32(body[at:])
	if namesEnd > int64(len(body)) {
		tb.Fatalf("%s's pack names run past its end", midx)
	}
	list := strings.Split(strings.TrimRight(string(body[names:namesEnd]), "\x00"), "\x00")
	if int64(id) >= int64(len(list)) {
		tb.Fatalf("%s's first object is in pack %d of its %d", midx, id, len(list))
	}
	return filepath.Join(pack, strings.TrimSuffix(list[id], ".idx"))
}

// FileIdentity is what a cache's vouch for a file is kept by — its device and
// inode, its size and its modification time — as Lstat reads them, for a
// case that holds a file's identity across an in-place damage.
func FileIdentity(tb testing.TB, path string) [4]int64 {
	tb.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		tb.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		tb.Fatalf("%s's identity is not readable here", path)
	}
	return [4]int64{int64(st.Dev), int64(st.Ino), info.Size(), info.ModTime().UnixNano()} //nolint:unconvert // Dev is int32 on darwin
}
