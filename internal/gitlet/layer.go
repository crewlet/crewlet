package gitlet

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // git's hashfile trailer IS a SHA-1: the reader compares what git wrote, and trusts no hash of its own to it
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/words"
)

// A published layer's three files, read and checked without git (ADR-0008; the
// package doc's "A layer's files are git's, read without git"), and its bytes
// cut into parts and laid back by one cut, layerPart. Each rule cites the git
// 2.56.0 source it follows (scripts/git-pin.env). What git's writer always
// writes is required exactly: the trailer pins every byte, so a file git wrote
// never fails. What git's reader ignores (an unknown chunk id or option bit) is
// ignored here, since a node installs only a layer of a git no newer than its
// own (LayerRef.Git); TestTheLayerReaderReadsWhatGitWrites holds both. Not git's
// `multi-pack-index verify` or `commit-graph verify`: they read every entry
// (twenty times a trailer's cost for the graph), no bitmap and no row, and add
// only data under a sound trailer, which no store can have changed; the cache's
// canary read through the bitmap catches a chain git cannot read.

// LayerContentType is the content type a published layer's part is stored
// under: not a git file — a part is a cut of the layer's three files back to
// back, and one part may end inside one file and begin another.
const LayerContentType = "application/vnd.crewlet.git-layer-part"

// The sizes of the formats' fixed parts, as git 2.56.0 writes them.
const (
	// midxHeaderBytes is MIDX_HEADER_SIZE: the signature, the version, the
	// object id version, the chunk count, a byte git writes zero and reads
	// nothing from, and the pack count.
	midxHeaderBytes = 12
	// graphHeaderBytes is GRAPH_HEADER_SIZE: the signature, the version, the
	// hash version, the chunk count and the base-graph count.
	graphHeaderBytes = 8
	// bitmapHeaderBytes is bitmap_disk_header at a SHA-1: the signature, the
	// version, the options, the selected commits and the checksum of the
	// multi-pack-index the bitmap belongs to.
	bitmapHeaderBytes = 12 + hashBytes
	// chunkRowBytes is CHUNK_TOC_ENTRY_SIZE: a table of contents' row, a
	// four-byte id and an eight-byte offset.
	chunkRowBytes = 12
	// midxChunkAlign is MIDX_CHUNK_ALIGNMENT, every multi-pack-index chunk's
	// alignment; a commit-graph's chunks are aligned to a byte.
	midxChunkAlign = 4
	// commitDataBytes is graph_data_width: a commit's root tree, its first
	// two parents, and its generation and date.
	commitDataBytes = hashBytes + 16
	// bloomHeaderBytes is BLOOMDATA_CHUNK_HEADER_SIZE: the filters' hash
	// version, the hashes a path takes and the bits an entry takes.
	bloomHeaderBytes = 12
	// lookupRowBytes is BITMAP_LOOKUP_TABLE_TRIPLET_WIDTH: a selected
	// commit's position, its bitmap's offset and its xor row.
	lookupRowBytes = 16
	// pseudoMergeTailBytes is what a bitmap's pseudo-merge table ends with:
	// its merges' count, the commits it names, and two offsets.
	pseudoMergeTailBytes = 24
	// packNameBytes is one PNAM entry of a pack a cache holds: "pack-", its
	// forty hex digits, ".idx" and the NUL that ends it.
	packNameBytes = len("pack-") + gitref.OIDHexLen + len(".idx") + 1
)

// The chunk ids the readers read: midx.h's MIDX_CHUNKID_* and commit-graph.c's
// GRAPH_CHUNKID_*.
const (
	chunkPackNames    = "PNAM"
	chunkFanout       = "OIDF"
	chunkLookup       = "OIDL"
	chunkOffsets      = "OOFF"
	chunkLargeOffsets = "LOFF"
	chunkRevIndex     = "RIDX"
	chunkBitmapped    = "BTMP"
	chunkCommitData   = "CDAT"
	chunkGenerations  = "GDA2"
	chunkGenOverflow  = "GDO2"
	chunkExtraEdges   = "EDGE"
	chunkBloomIndex   = "BIDX"
	chunkBloomData    = "BDAT"
	chunkBase         = "BASE"
)

// The bitmap option bits the reader acts on (pack-bitmap.h's
// pack_bitmap_opts). Any other bit is ignored, as git's reader ignores it.
const (
	// bitmapFullDAG is BITMAP_OPT_FULL_DAG, which git refuses a bitmap
	// without.
	bitmapFullDAG = 0x1
	// bitmapHashCache is BITMAP_OPT_HASH_CACHE: four bytes an object of the
	// multi-pack-index, just before the trailer.
	bitmapHashCache = 0x4
	// bitmapLookupTable is BITMAP_OPT_LOOKUP_TABLE: a row a selected
	// commit, before the hash cache.
	bitmapLookupTable = 0x10
	// bitmapPseudoMerges is BITMAP_OPT_PSEUDO_MERGES: a table sized by its
	// own last eight bytes, before the lookup table.
	bitmapPseudoMerges = 0x20
)

// layerSections names a layer's three files, in the order its bytes hold
// them ([LayerRef.Sections]).
var layerSections = [3]string{"multi-pack-index", "bitmap", "commit-graph layer"}

// formatError is a refusal of a file's bytes, not git's format as these
// readers read it, which [CheckLayer] and [LayLayer] answer as [ErrBadLayer].
// A failed read is never one: it says nothing of what the file holds.
type formatError struct{ err error }

func (e formatError) Error() string { return e.err.Error() }

// formatf is a refusal of a file's bytes: what of the file differs, in a
// sentence.
func formatf(format string, args ...any) error {
	return formatError{fmt.Errorf(format, args...)}
}

// isFormat reports a refusal of the bytes, as against a read that failed.
func isFormat(err error) bool {
	var f formatError
	return errors.As(err, &f)
}

// readAt reads exactly len(p) bytes of what at off. A file that ends sooner is
// a refusal — it does not hold the bytes it was handed as — and any other
// failure is the read's, as it came.
func readAt(r io.ReaderAt, p []byte, off int64, what string) error {
	n, err := r.ReadAt(p, off)
	switch {
	case n == len(p):
		// io.ReaderAt may answer io.EOF beside the last of a file's bytes.
		return nil
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return formatf("%s ends at byte %d, inside the %d bytes at %d it is read as holding", what, off+int64(n),
			len(p), off)
	case err == nil:
		return fmt.Errorf("gitlet: read %s at byte %d: %d bytes of %d and no error, which no reader may answer", what,
			off, n, len(p))
	}
	return fmt.Errorf("gitlet: read %s at byte %d: %w", what, off, err)
}

// sized holds what r holds to exactly size bytes: its last byte there, and no
// byte past it. A file of another size is not the section its row cuts,
// whatever it holds.
func sized(r io.ReaderAt, size int64, what string) error {
	var b [1]byte
	if size > 0 {
		if n, err := r.ReadAt(b[:], size-1); n != 1 {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return formatf("%s is shorter than its %d-byte section", what, size)
			}
			return fmt.Errorf("gitlet: read %s's last byte, at %d: %w", what, size-1, err)
		}
	}
	switch n, err := r.ReadAt(b[:], size); {
	case n > 0:
		return formatf("%s is longer than its %d-byte section", what, size)
	case errors.Is(err, io.EOF):
		return nil
	case err == nil:
		return fmt.Errorf("gitlet: read %s past its %d bytes: nothing and no error, which no reader may answer",
			what, size)
	default:
		return fmt.Errorf("gitlet: read %s past its %d bytes: %w", what, size, err)
	}
}

// trailerOf checks the hashfile r (size bytes) against its trailer by
// [HashTrailer], a piece at a time as p says, stopped with ctx, and answers the
// trailer in hex: a multi-pack-index's or commit-graph layer's name, what a
// bitmap's header names. A short file or a wrong trailer is a refusal; a
// failed read, hold or ctx is answered as it came.
func trailerOf(ctx context.Context, r io.ReaderAt, size int64, what string, p Pieces) (string, error) {
	t, err := HashTrailer(ctx, r, size, sha1.New(), p) //nolint:gosec // git's trailer, compared as git wrote it
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "", formatf("%s ends at byte %d, inside the %d bytes it is read as holding", what, t.Read, size)
	case err != nil:
		return "", fmt.Errorf("gitlet: hash %s: %w", what, err)
	case !t.Sound():
		return "", formatf("%s's trailer, %x, is not the SHA-1 of the %d bytes before it, %x", what, t.Held,
			size-hashBytes, t.Sum)
	}
	return hex.EncodeToString(t.Held), nil
}

// chunkSpan is one chunk of a chunk-format file: its id and where it lies.
type chunkSpan struct {
	id         string
	start, end int64
}

// len is the chunk's bytes.
func (c chunkSpan) len() int64 { return c.end - c.start }

// readChunks reads the table of contents of the chunk-format file what (r,
// size bytes; gitformat-chunk.adoc): n rows from byte at, then the zero row
// that ends it. It holds every rule of chunk-format.c's
// read_table_of_contents (no id zero before the end, none twice, none ending
// before it begins, each start on an align boundary) and, beyond git's reader,
// the shape write_chunkfile gives every file: chunks contiguous from the
// table's end to the trailer. So every byte is header, table, chunk or
// trailer, and a chunk's length is the next row's offset less its own.
//
// The table is read only once it fits the file (FuzzParseMidx): n is a
// header's byte, so the table is at most 3 KiB.
func readChunks(r io.ReaderAt, size, at int64, n int, align int64, what string) ([]chunkSpan, error) {
	tableEnd := at + int64(n+1)*chunkRowBytes
	if tableEnd+hashBytes > size {
		return nil, formatf("%s's table of contents, %d chunks and the row that ends it, runs to byte %d, past the "+
			"%d before its trailer", what, n, tableEnd, size-hashBytes)
	}
	table := make([]byte, tableEnd-at)
	if err := readAt(r, table, at, what); err != nil {
		return nil, err
	}
	offset := func(i int) int64 {
		// An offset past what an int64 holds is past every file, which
		// the comparisons below refuse as such.
		return int64(min(binary.BigEndian.Uint64(table[i*chunkRowBytes+4:]), uint64(1)<<62))
	}
	chunks := make([]chunkSpan, 0, n)
	for i := range n {
		id := table[i*chunkRowBytes : i*chunkRowBytes+4]
		start, end := offset(i), offset(i+1)
		switch {
		case binary.BigEndian.Uint32(id) == 0:
			return nil, formatf("row %d of %s's table of contents ends it, before the %d chunks its header counts",
				i, what, n)
		case slices.ContainsFunc(chunks, func(c chunkSpan) bool { return c.id == string(id) }):
			return nil, formatf("%s's table of contents names the chunk %q twice", what, id)
		case start%align != 0:
			return nil, formatf("%s's chunk %q begins at byte %d, not on a %d-byte boundary", what, id, start, align)
		case i == 0 && start != tableEnd:
			return nil, formatf("%s's first chunk, %q, begins at byte %d, not where its table of contents ends, "+
				"%d", what, id, start, tableEnd)
		case end < start:
			return nil, formatf("%s's chunk %q ends at byte %d, before it begins at %d", what, id, end, start)
		}
		chunks = append(chunks, chunkSpan{id: string(id), start: start, end: end})
	}
	switch last := table[n*chunkRowBytes : n*chunkRowBytes+4]; {
	case binary.BigEndian.Uint32(last) != 0:
		return nil, formatf("%s's table of contents does not end after its %d chunks: row %d names %q", what, n, n,
			last)
	case n == 0 && offset(0) != tableEnd:
		return nil, formatf("%s holds no chunk, and the end of its chunks is byte %d, not where its table of "+
			"contents ends, %d", what, offset(0), tableEnd)
	case offset(n) != size-hashBytes:
		return nil, formatf("%s's chunks end at byte %d, not where its trailer begins, %d", what, offset(n),
			size-hashBytes)
	}
	return chunks, nil
}

// chunkOf is the chunk id names in chunks, if the table holds one.
func chunkOf(chunks []chunkSpan, id string) (chunkSpan, bool) {
	i := slices.IndexFunc(chunks, func(c chunkSpan) bool { return c.id == id })
	if i < 0 {
		return chunkSpan{}, false
	}
	return chunks[i], true
}

// requireChunk is the chunk id names in chunks, or a refusal naming it as
// missing.
func requireChunk(chunks []chunkSpan, id, what string) (chunkSpan, error) {
	c, ok := chunkOf(chunks, id)
	if !ok {
		return chunkSpan{}, formatf("%s has no %s chunk", what, id)
	}
	return c, nil
}

// chunkSized holds chunk c to want bytes: the size git writes it at for the
// count it was read from, which of says.
func chunkSized(c chunkSpan, want int64, what, of string) error {
	if c.len() != want {
		return formatf("%s's %s chunk is %d bytes, not the %d %s", what, c.id, c.len(), want, of)
	}
	return nil
}

// readFanout reads the OID fanout chunk c of what: 256 cumulative counts, the
// i-th the ids whose first byte is at most i, so never decreasing, the last
// the whole count — midx.c's midx_read_oid_fanout and commit-graph.c's
// graph_read_oid_fanout. It answers that count.
func readFanout(r io.ReaderAt, c chunkSpan, what string) (int64, error) {
	if err := chunkSized(c, fanoutBytes, what, "a fanout of 256 counts is"); err != nil {
		return 0, err
	}
	var fanout [fanoutBytes]byte
	if err := readAt(r, fanout[:], c.start, what); err != nil {
		return 0, err
	}
	for i := 1; i < 256; i++ {
		prev, cur := binary.BigEndian.Uint32(fanout[4*(i-1):]), binary.BigEndian.Uint32(fanout[4*i:])
		if prev > cur {
			return 0, formatf("%s's fanout decreases at entry %d, from %d to %d", what, i, prev, cur)
		}
	}
	return int64(binary.BigEndian.Uint32(fanout[fanoutBytes-4:])), nil
}

// MidxInfo is what a multi-pack-index file says about itself.
type MidxInfo struct {
	// Version is 1 or 2: git writes version 1 unless midx.version says 2,
	// and `multi-pack-index compact` writes 2; either is read.
	Version int
	// Packs are the PNAM chunk's index names, "pack-<checksum>.idx", in the
	// chunk's order: sorted in a version 1 file, any order in a version 2.
	Packs []string
	// Objects is the OIDF chunk's last fanout entry: the objects the file
	// indexes — a layer's own, never its base's.
	Objects int64
	// Checksum is the trailer: the SHA-1 of every byte before it, 40 hex —
	// a layer's name.
	Checksum string
	// Chunks are the chunk ids present, in table order (git 2.56.0 writes
	// PNAM, OIDF, OIDL, OOFF, LOFF where an offset needs it, RIDX, BTMP).
	Chunks []string
}

// ParseMidx reads a multi-pack-index file of size bytes (gitformat-pack.adoc;
// midx.c's load_multi_pack_index_one): signature "MIDX", version 1 or 2,
// object id version 1 (SHA-1, Gitlet's format), the table of contents
// (readChunks), and PNAM, OIDF, OIDL, OOFF, RIDX and BTMP at the sizes their
// counts give: git writes RIDX and BTMP exactly when it writes a bitmap
// (midx-write.c's write_midx_internal), and Gitlet publishes no
// multi-pack-index without one, so at least one pack and one object too.
// Every PNAM entry is "pack-<forty hex>.idx", the one name a cache gives a
// pack, strictly ascending in version 1 as git's reader requires. The trailer
// is recomputed by [HashTrailer] as p says, stopped with ctx. A refusal names
// what differs; a failed read, hold or ctx is answered as it came
// ([CheckLayer] says why the two are apart).
func ParseMidx(ctx context.Context, r io.ReaderAt, size int64, p Pieces) (MidxInfo, error) {
	const what = "the multi-pack-index"
	if size < midxHeaderBytes+hashBytes {
		return MidxInfo{}, formatf("%s is %d bytes, under its %d-byte header and %d-byte trailer", what, size,
			midxHeaderBytes, hashBytes)
	}
	var head [midxHeaderBytes]byte
	if err := readAt(r, head[:], 0, what); err != nil {
		return MidxInfo{}, err
	}
	switch {
	case string(head[:4]) != "MIDX":
		return MidxInfo{}, formatf("%s begins %q, not %q", what, head[:4], "MIDX")
	case head[4] != 1 && head[4] != 2:
		return MidxInfo{}, formatf("%s is version %d, and git 2.56.0 reads versions 1 and 2", what, head[4])
	case head[5] != 1:
		return MidxInfo{}, formatf("%s's object ids are version %d, not 1 (SHA-1, Gitlet's object format)",
			what, head[5])
	}
	version, packs := int(head[4]), int64(binary.BigEndian.Uint32(head[8:12]))
	chunks, err := readChunks(r, size, midxHeaderBytes, int(head[6]), midxChunkAlign, what)
	if err != nil {
		return MidxInfo{}, err
	}
	required := map[string]chunkSpan{}
	for _, id := range []string{chunkPackNames, chunkFanout, chunkLookup, chunkOffsets, chunkRevIndex,
		chunkBitmapped} {
		if required[id], err = requireChunk(chunks, id, what); err != nil {
			return MidxInfo{}, err
		}
	}
	objects, err := readFanout(r, required[chunkFanout], what)
	switch {
	case err != nil:
		return MidxInfo{}, err
	case packs == 0:
		return MidxInfo{}, formatf("%s names no pack, and git writes none over no pack", what)
	case objects == 0:
		return MidxInfo{}, formatf("%s indexes no object, and git writes no bitmap over none", what)
	}
	for _, c := range []struct {
		id   string
		want int64
		of   string
	}{
		{chunkLookup, hashBytes * objects, "its objects' ids are"},
		{chunkOffsets, 8 * objects, "its objects' pack ids and offsets are"},
		{chunkRevIndex, 4 * objects, "its objects' bitmap positions are"},
		{chunkBitmapped, 8 * packs, "its packs' bitmap ranges are"},
		{chunkPackNames, (int64(packNameBytes)*packs + midxChunkAlign - 1) / midxChunkAlign * midxChunkAlign,
			"its packs' names are, padded to a multiple of four"},
	} {
		if err = chunkSized(required[c.id], c.want, what, c.of); err != nil {
			return MidxInfo{}, err
		}
	}
	if large, ok := chunkOf(chunks, chunkLargeOffsets); ok && large.len()%8 != 0 {
		return MidxInfo{}, formatf("%s's %s chunk is %d bytes, not a whole number of 8-byte offsets", what,
			chunkLargeOffsets, large.len())
	}
	// The names are read only now that their chunk is the size the count
	// gives, inside the file the table already fitted.
	pnam := required[chunkPackNames]
	raw := make([]byte, pnam.len())
	if err = readAt(r, raw, pnam.start, what); err != nil {
		return MidxInfo{}, err
	}
	names := make([]string, 0, packs)
	for i := range int(packs) {
		entry := raw[i*packNameBytes : (i+1)*packNameBytes]
		name := entry[:packNameBytes-1]
		switch {
		case entry[packNameBytes-1] != 0 || !packIndexName(name):
			return MidxInfo{}, formatf("%s's pack name %d, %q, is not \"pack-<forty hex digits>.idx\" and its NUL",
				what, i, entry)
		case version == 1 && i > 0 && names[i-1] >= string(name):
			return MidxInfo{}, formatf("%s is version 1, whose packs are named in ascending order, and %s follows %s",
				what, name, names[i-1])
		}
		names = append(names, string(name))
	}
	if pad := raw[int(packs)*packNameBytes:]; slices.ContainsFunc(pad, func(b byte) bool { return b != 0 }) {
		return MidxInfo{}, formatf("%s pads its pack names with %q, not NULs", what, pad)
	}
	checksum, err := trailerOf(ctx, r, size, what, p)
	if err != nil {
		return MidxInfo{}, err
	}
	ids := make([]string, len(chunks))
	for i, c := range chunks {
		ids[i] = c.id
	}
	return MidxInfo{Version: version, Packs: names, Objects: objects, Checksum: checksum, Chunks: ids}, nil
}

// packIndexName reports name as the one name a cache gives a pack's index:
// "pack-", the pack's forty lowercase hex digits — never git's zero id, which
// names no file — and ".idx".
func packIndexName(name []byte) bool {
	if len(name) != packNameBytes-1 || !bytes.HasPrefix(name, []byte("pack-")) ||
		!bytes.HasSuffix(name, []byte(".idx")) {
		return false
	}
	oid := name[len("pack-") : len(name)-len(".idx")]
	return gitref.ValidOID(oid) && string(oid) != ZeroOID
}

// BitmapInfo is what a multi-pack bitmap says about itself.
type BitmapInfo struct {
	// Version is 1, the one version git writes and reads.
	Version int
	// Options are the header's option bits (pack-bitmap.h's BITMAP_OPT_*):
	// which tables follow the selected commits' bitmaps.
	Options uint16
	// Midx is the checksum of the multi-pack-index it belongs to — the
	// header's checksum field — 40 hex.
	Midx string
	// Commits is how many commits it selected, each with a bitmap of its
	// own.
	Commits int
	// Checksum is its trailer, 40 hex.
	Checksum string
}

// ParseBitmap reads a multi-pack bitmap of size bytes (pack-bitmap.c's
// load_bitmap_header): signature "BITM", version 1, options with
// BITMAP_OPT_FULL_DAG (git refuses a bitmap without it), the selected commits
// and the checksum of the multi-pack-index it belongs to, and recomputes the
// trailer by [HashTrailer] as p says, stopped with ctx. The rest of git's
// header check, the trailing tables against the file's room, needs the
// multi-pack-index's object count: bitmapTablesFit, run by [CheckLayer] and
// [LayLayer]. A refusal names what differs; a failed read, hold or ctx is
// answered as it came.
func ParseBitmap(ctx context.Context, r io.ReaderAt, size int64, p Pieces) (BitmapInfo, error) {
	const what = "the bitmap"
	if size < bitmapHeaderBytes+hashBytes {
		return BitmapInfo{}, formatf("%s is %d bytes, under its %d-byte header and %d-byte trailer", what, size,
			bitmapHeaderBytes, hashBytes)
	}
	var head [bitmapHeaderBytes]byte
	if err := readAt(r, head[:], 0, what); err != nil {
		return BitmapInfo{}, err
	}
	version, options := binary.BigEndian.Uint16(head[4:6]), binary.BigEndian.Uint16(head[6:8])
	switch {
	case string(head[:4]) != "BITM":
		return BitmapInfo{}, formatf("%s begins %q, not %q", what, head[:4], "BITM")
	case version != 1:
		return BitmapInfo{}, formatf("%s is version %d, and git reads version 1", what, version)
	case options&bitmapFullDAG == 0:
		return BitmapInfo{}, formatf("%s's options, %#x, lack BITMAP_OPT_FULL_DAG, which git requires", what,
			options)
	}
	checksum, err := trailerOf(ctx, r, size, what, p)
	if err != nil {
		return BitmapInfo{}, err
	}
	return BitmapInfo{Version: int(version), Options: options, Midx: hex.EncodeToString(head[12:]),
		Commits: int(binary.BigEndian.Uint32(head[8:12])), Checksum: checksum}, nil
}

// bitmapTablesFit holds a bitmap's trailing tables to the room its file has,
// as git's load_bitmap_header does once it knows the multi-pack-index's object
// count: each table the options name — the hash cache (four bytes an object),
// the lookup table (a row a selected commit) and the pseudo-merge table (its
// size in its own last eight bytes, and its merges' offsets inside it) — fits
// between the header and the table after it.
func bitmapTablesFit(r io.ReaderAt, size int64, b BitmapInfo, objects int64) error {
	const what = "the bitmap"
	end := size - hashBytes
	room := func() int64 { return end - bitmapHeaderBytes }
	if b.Options&bitmapHashCache != 0 {
		if need := 4 * objects; need > room() {
			return formatf("%s is too short to fit its hash cache: %d bytes for the multi-pack-index's %d objects, "+
				"in %d", what, need, objects, room())
		}
		end -= 4 * objects
	}
	if b.Options&bitmapLookupTable != 0 {
		if need := lookupRowBytes * int64(b.Commits); need > room() {
			return formatf("%s is too short to fit its lookup table: %d bytes for its %d commits, in %d", what, need,
				b.Commits, room())
		}
		end -= lookupRowBytes * int64(b.Commits)
	}
	if b.Options&bitmapPseudoMerges != 0 {
		if room() < pseudoMergeTailBytes {
			return formatf("%s is too short to fit its pseudo-merge table's %d-byte end, in %d", what,
				pseudoMergeTailBytes, room())
		}
		var tail [pseudoMergeTailBytes]byte
		if err := readAt(r, tail[:], end-pseudoMergeTailBytes, what); err != nil {
			return err
		}
		table, merges := binary.BigEndian.Uint64(tail[16:]), uint64(binary.BigEndian.Uint32(tail[:4]))
		switch {
		case table > uint64(room()):
			return formatf("%s is too short to fit its %d-byte pseudo-merge table, in %d", what, table, room())
		case 8*merges+pseudoMergeTailBytes > table:
			return formatf("%s's pseudo-merge table, %d bytes, is too short for its %d merges", what, table, merges)
		}
	}
	return nil
}

// GraphInfo is what a commit-graph layer says about itself.
type GraphInfo struct {
	// Commits is the OIDF chunk's last fanout entry: the commits the layer
	// holds — its own, never its bases'.
	Commits int64
	// Bases are the BASE chunk's ids: the layers below it, in chain order,
	// root first — which a commit-graph layer names, as a
	// multi-pack-index layer does not.
	Bases []string
	// BloomVersion is the BDAT header's hash version: 2 for every layer
	// Gitlet writes; 0 with no changed-path filters at all.
	BloomVersion int
	// Checksum is the trailer, 40 hex — the layer's name.
	Checksum string
}

// ParseGraph reads a commit-graph file of size bytes (gitformat-commit-graph;
// commit-graph.c's parse_commit_graph): signature "CGPH", version 1, hash
// version 1 (SHA-1), at least git's smallest commit-graph, the table of
// contents (readChunks), OIDF, OIDL and CDAT at the sizes the commit count
// gives, the optional GDA2, GDO2, EDGE and BIDX with BDAT at theirs
// (bloomVersion), and a BASE chunk of exactly the base graphs the header
// counts (baseGraphs). It recomputes the trailer by [HashTrailer] as p says,
// stopped with ctx. A refusal names what differs; a failed read, hold or ctx
// is answered as it came.
func ParseGraph(ctx context.Context, r io.ReaderAt, size int64, p Pieces) (GraphInfo, error) {
	const what = "the commit-graph layer"
	// graph_min_size: a header, the rows of three chunks and of the one that
	// ends the table, a fanout and a trailer.
	const smallest = graphHeaderBytes + 4*chunkRowBytes + fanoutBytes + hashBytes
	if size < smallest {
		return GraphInfo{}, formatf("%s is %d bytes, under the %d of git's smallest", what, size, smallest)
	}
	var head [graphHeaderBytes]byte
	if err := readAt(r, head[:], 0, what); err != nil {
		return GraphInfo{}, err
	}
	switch {
	case string(head[:4]) != "CGPH":
		return GraphInfo{}, formatf("%s begins %q, not %q", what, head[:4], "CGPH")
	case head[4] != 1:
		return GraphInfo{}, formatf("%s is version %d, and git reads version 1", what, head[4])
	case head[5] != 1:
		return GraphInfo{}, formatf("%s's hash is version %d, not 1 (SHA-1, Gitlet's object format)", what,
			head[5])
	}
	n, bases := int(head[6]), int64(head[7])
	if fits := int64(graphHeaderBytes+(n+1)*chunkRowBytes+fanoutBytes) + hashBytes; size < fits {
		return GraphInfo{}, formatf("%s is %d bytes, too small to hold %d chunks, a fanout and a trailer (%d)",
			what, size, n, fits)
	}
	chunks, err := readChunks(r, size, graphHeaderBytes, n, 1, what)
	if err != nil {
		return GraphInfo{}, err
	}
	required := map[string]chunkSpan{}
	for _, id := range []string{chunkFanout, chunkLookup, chunkCommitData} {
		if required[id], err = requireChunk(chunks, id, what); err != nil {
			return GraphInfo{}, err
		}
	}
	commits, err := readFanout(r, required[chunkFanout], what)
	if err != nil {
		return GraphInfo{}, err
	}
	if err = chunkSized(required[chunkLookup], hashBytes*commits, what, "its commits' ids are"); err != nil {
		return GraphInfo{}, err
	}
	if err = chunkSized(required[chunkCommitData], commitDataBytes*commits, what,
		"its commits' data are"); err != nil {
		return GraphInfo{}, err
	}
	if c, ok := chunkOf(chunks, chunkGenerations); ok {
		if err = chunkSized(c, 4*commits, what, "its commits' corrected dates are"); err != nil {
			return GraphInfo{}, err
		}
	}
	for _, w := range []struct {
		id    string
		width int64
	}{{chunkGenOverflow, 8}, {chunkExtraEdges, 4}} {
		if c, ok := chunkOf(chunks, w.id); ok && c.len()%w.width != 0 {
			return GraphInfo{}, formatf("%s's %s chunk is %d bytes, not a whole number of %d-byte entries", what,
				w.id, c.len(), w.width)
		}
	}
	info := GraphInfo{Commits: commits}
	if info.BloomVersion, err = bloomVersion(r, chunks, commits, what); err != nil {
		return GraphInfo{}, err
	}
	if info.Bases, err = baseGraphs(r, chunks, bases, what); err != nil {
		return GraphInfo{}, err
	}
	if info.Checksum, err = trailerOf(ctx, r, size, what, p); err != nil {
		return GraphInfo{}, err
	}
	return info, nil
}

// bloomVersion is the hash version of a commit-graph's changed-path filters —
// 0 where it carries none — read from the BIDX and BDAT chunks, which git
// writes both or neither: BIDX a filter's end a commit, the last of them the
// bytes of the filters BDAT holds after its header.
func bloomVersion(r io.ReaderAt, chunks []chunkSpan, commits int64, what string) (int, error) {
	index, hasIndex := chunkOf(chunks, chunkBloomIndex)
	data, hasData := chunkOf(chunks, chunkBloomData)
	switch {
	case hasIndex != hasData:
		return 0, formatf("%s carries one of the changed-path chunks %s and %s without the other, and git writes "+
			"both or neither", what, chunkBloomIndex, chunkBloomData)
	case !hasData:
		return 0, nil
	}
	if err := chunkSized(index, 4*commits, what, "its commits' filter ends are"); err != nil {
		return 0, err
	}
	if data.len() < bloomHeaderBytes {
		return 0, formatf("%s's %s chunk is %d bytes, under its %d-byte header", what, chunkBloomData, data.len(),
			bloomHeaderBytes)
	}
	var version [4]byte
	if err := readAt(r, version[:], data.start, what); err != nil {
		return 0, err
	}
	if commits > 0 {
		var last [4]byte
		if err := readAt(r, last[:], index.end-4, what); err != nil {
			return 0, err
		}
		if filters := int64(binary.BigEndian.Uint32(last[:])); filters != data.len()-bloomHeaderBytes {
			return 0, formatf("%s's filters end at byte %d of the %d its %s chunk holds after its header", what,
				filters, data.len()-bloomHeaderBytes, chunkBloomData)
		}
	}
	return int(binary.BigEndian.Uint32(version[:])), nil
}

// baseGraphs is the ids a commit-graph layer's BASE chunk names, root first:
// exactly the bases its header counts, the chunk absent when it counts none.
func baseGraphs(r io.ReaderAt, chunks []chunkSpan, bases int64, what string) ([]string, error) {
	base, hasBase := chunkOf(chunks, chunkBase)
	switch {
	case !hasBase && bases > 0:
		return nil, formatf("%s's header counts %d base graphs, and it carries no %s chunk", what, bases, chunkBase)
	case hasBase && bases == 0:
		return nil, formatf("%s's header counts no base graph, and it carries a %s chunk", what, chunkBase)
	case !hasBase:
		return nil, nil
	}
	if err := chunkSized(base, hashBytes*bases, what, "its base graphs' ids are"); err != nil {
		return nil, err
	}
	raw := make([]byte, base.len())
	if err := readAt(r, raw, base.start, what); err != nil {
		return nil, err
	}
	ids := make([]string, bases)
	for i := range ids {
		ids[i] = hex.EncodeToString(raw[i*hashBytes : (i+1)*hashBytes])
	}
	return ids, nil
}

// layerFiles is what a layer's three files say of themselves, by the one
// reading [CheckLayer] and [LayLayer] share.
type layerFiles struct {
	midx MidxInfo
	// packs are the checksums of the packs its multi-pack-index names,
	// sorted.
	packs  []string
	bitmap BitmapInfo
	// graph is the zero GraphInfo where the layer carries no commit-graph
	// layer.
	graph GraphInfo
}

// readLayer reads a layer's three files, sizes[i] bytes each (graph nil
// exactly when the layer has no commit-graph layer), and holds each to its
// size and the three to each other and the chain below: the bitmap belongs to
// the multi-pack-index and its tables fit it, every pack is named once, the
// changed-path filters are version 2, the one Gitlet writes (version 1 or none
// would have every node's log and blame read the slow way, or misread a path
// the version 1 hash gets wrong), and the BASE chunk is exactly baseGraphs, the
// GraphIDs of the layers below that carry one, root first: git reads a chain
// only over the bases each layer names. [LayLayer] and [CheckLayer] share this
// one comparison; fields name the size fields as the caller states them. A
// refusal of the bytes is [ErrBadLayer] naming the field; a failed read, hold
// or ctx (trailers hashed as p says) is answered as it came.
func readLayer(ctx context.Context, midx, bitmap, graph io.ReaderAt, sizes [3]int64, fields [3]string,
	baseGraphs []string, p Pieces) (layerFiles, error) {

	bad := func(field string, err error) error {
		if !isFormat(err) {
			return err
		}
		return fmt.Errorf("%w: %s: %w", ErrBadLayer, field, err)
	}
	for i, r := range [3]io.ReaderAt{midx, bitmap, graph} {
		if r == nil {
			continue
		}
		if err := sized(r, sizes[i], "the "+layerSections[i]); err != nil {
			return layerFiles{}, bad(fields[i], err)
		}
	}
	var f layerFiles
	var err error
	if f.midx, err = ParseMidx(ctx, midx, sizes[0], p); err != nil {
		return layerFiles{}, bad("midx", err)
	}
	if f.bitmap, err = ParseBitmap(ctx, bitmap, sizes[1], p); err != nil {
		return layerFiles{}, bad("bitmap", err)
	}
	if f.bitmap.Midx != f.midx.Checksum {
		return layerFiles{}, bad("bitmap.midx", formatf("the bitmap belongs to the multi-pack-index %s, and the "+
			"layer's is %s", f.bitmap.Midx, f.midx.Checksum))
	}
	if err = bitmapTablesFit(bitmap, sizes[1], f.bitmap, f.midx.Objects); err != nil {
		return layerFiles{}, bad("bitmap", err)
	}
	f.packs = make([]string, len(f.midx.Packs))
	for i, name := range f.midx.Packs {
		f.packs[i] = strings.TrimSuffix(strings.TrimPrefix(name, "pack-"), ".idx")
	}
	slices.Sort(f.packs)
	for i := 1; i < len(f.packs); i++ {
		if f.packs[i] == f.packs[i-1] {
			return layerFiles{}, bad("packs", formatf("the multi-pack-index names the pack %s twice", f.packs[i]))
		}
	}
	if graph == nil {
		return f, nil
	}
	if f.graph, err = ParseGraph(ctx, graph, sizes[2], p); err != nil {
		return layerFiles{}, bad("graph", err)
	}
	if f.graph.BloomVersion != 2 {
		return layerFiles{}, bad("graph.filters", formatf("the commit-graph layer's changed-path filters are "+
			"version %d (0: none), not 2", f.graph.BloomVersion))
	}
	if !slices.Equal(f.graph.Bases, baseGraphs) {
		return layerFiles{}, bad("graph.bases", formatf("the commit-graph layer is written over the layers %s, and "+
			"the chain below it is %s", words.And(f.graph.Bases), words.And(baseGraphs)))
	}
	return f, nil
}

// CheckLayer holds a layer's three files, as split from its parts, to its row
// and the chain below it, the ONE rule (ADR-0008) the maintainer's adoption and
// every Gitlet node's install apply before git is handed them: the row
// ([LayerRef.Check]), each file exactly its section (graph nil or empty where
// the row has none), the trailers the row's LayerID and GraphID, the packs,
// objects and commits the row's, and the rest of readLayer. Each trailer is
// hashed as p says, ctx checked before every read (internal/gitlet/cache's
// CheckPieceBytes): the largest root layer is some 2.6 GB. A refusal is
// [ErrBadLayer] naming the field, never retried: the parts passed the store's
// hash check ([FetchLayerPart]), so the fault is the publisher's and its next
// publication replaces the layer. A failed read, hold or ctx is answered as it
// came, since refusing the layer for a disk's failure has every node refetch it.
func CheckLayer(ctx context.Context, row LayerRef, midx, bitmap, graph io.ReaderAt, baseGraphs []string,
	p Pieces) error {

	bad := func(field, format string, args ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrBadLayer, field, fmt.Sprintf(format, args...))
	}
	if err := row.Check(); err != nil {
		return fmt.Errorf("%w: %w", ErrBadLayer, err)
	}
	switch {
	case midx == nil || bitmap == nil:
		return fmt.Errorf("gitlet: check layer %s: its multi-pack-index and its bitmap are both read, and one was "+
			"handed as none", row.LayerID)
	case row.GraphBytes > 0 && graph == nil:
		return fmt.Errorf("gitlet: check layer %s: its row carries a %d-byte commit-graph layer, and none was handed",
			row.LayerID, row.GraphBytes)
	case row.GraphBytes == 0 && graph != nil:
		if err := sized(graph, 0, "the commit-graph layer"); err != nil {
			if !isFormat(err) {
				return err
			}
			return fmt.Errorf("%w: graph_bytes: %w", ErrBadLayer, err)
		}
		graph = nil
	}
	f, err := readLayer(ctx, midx, bitmap, graph, row.Sections(),
		[3]string{"midx_bytes", "bitmap_bytes", "graph_bytes"}, baseGraphs, p)
	if err != nil {
		return err
	}
	switch {
	case f.midx.Checksum != row.LayerID:
		return bad("layer_id", "the multi-pack-index's trailer is %s, and the row names %s", f.midx.Checksum,
			row.LayerID)
	case !slices.Equal(f.packs, row.Packs):
		return bad("packs", "%s", packsDiffer(f.packs, row.Packs))
	case f.midx.Objects != row.Objects:
		return bad("objects", "the multi-pack-index indexes %d objects, and the row says %d", f.midx.Objects,
			row.Objects)
	case graph == nil:
		return nil
	case f.graph.Checksum != row.GraphID:
		return bad("graph_id", "the commit-graph layer's trailer is %s, and the row names %s", f.graph.Checksum,
			row.GraphID)
	case f.graph.Commits != row.Commits:
		return bad("commits", "the commit-graph layer holds %d commits, and the row says %d", f.graph.Commits,
			row.Commits)
	}
	return nil
}

// packsDiffer says how two sorted pack lists differ, naming the first pack one
// holds and the other does not.
func packsDiffer(file, row []string) string {
	i := 0
	for i < len(file) && i < len(row) && file[i] == row[i] {
		i++
	}
	if i < len(row) && (i == len(file) || row[i] < file[i]) {
		return fmt.Sprintf("the multi-pack-index names %d packs and the row %d: %s is the row's and not the "+
			"multi-pack-index's", len(file), len(row), row[i])
	}
	return fmt.Sprintf("the multi-pack-index names %d packs and the row %d: %s is the multi-pack-index's and not "+
		"the row's", len(file), len(row), file[i])
}

// LayLayer reads the three files a build wrote in a scratch (graphPath ""
// when the build's packs held no commit the base's graph layers lack) and
// answers the row's byte fields from them: section sizes, Objects, Commits,
// the packs (as checksums, sorted), LayerID and GraphID. So the publisher
// states nothing git did not write, by [CheckLayer]'s own reading (readLayer,
// with baseGraphs as there), and a build no node would install is refused
// before a byte is stored. The row's own check ([LayerRef.Check]) is the
// decide's; BaseID, Replaces, Parts, Git and TookMs are left for the
// publisher, which holds the packs to its plan. Trailers are hashed as p says,
// stopped with ctx. A refusal of the bytes is [ErrBadLayer]; a failed open,
// read, hold or ctx is answered as it came.
func LayLayer(ctx context.Context, midxPath, bitmapPath, graphPath string, baseGraphs []string,
	p Pieces) (LayerRef, error) {

	var readers [3]io.ReaderAt
	var sizes [3]int64
	for i, path := range [3]string{midxPath, bitmapPath, graphPath} {
		if path == "" {
			if i < 2 {
				return LayerRef{}, fmt.Errorf("gitlet: lay out a layer: no path names its %s, which every layer has",
					layerSections[i])
			}
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return LayerRef{}, fmt.Errorf("gitlet: lay out a layer: %w", err)
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		switch {
		case err != nil:
			return LayerRef{}, fmt.Errorf("gitlet: lay out a layer: %w", err)
		case !info.Mode().IsRegular():
			return LayerRef{}, fmt.Errorf("gitlet: lay out a layer: its %s, %s, is not a regular file",
				layerSections[i], path)
		}
		readers[i], sizes[i] = f, info.Size()
	}
	files, err := readLayer(ctx, readers[0], readers[1], readers[2], sizes, [3]string{"midx", "bitmap", "graph"},
		baseGraphs, p)
	if err != nil {
		return LayerRef{}, err
	}
	return LayerRef{
		LayerID: files.midx.Checksum, GraphID: files.graph.Checksum, Packs: files.packs,
		MidxBytes: sizes[0], BitmapBytes: sizes[1], GraphBytes: sizes[2],
		Objects: files.midx.Objects, Commits: files.graph.Commits,
	}, nil
}

// layerSpan is one run of a layer's bytes inside one of its three files.
type layerSpan struct {
	// section is the file: 0 the multi-pack-index, 1 its bitmap, 2 the
	// commit-graph layer ([LayerRef.Sections]).
	section int
	// offset is where the run begins in that file; bytes how long it is.
	offset, bytes int64
}

// partCut is where one part of a layer lies: its first byte in the layer, its
// bytes, and the runs of the three files they are.
type partCut struct {
	start, size int64
	spans       []layerSpan
}

// layerPart is where part i of a layer whose three files are sections bytes
// lies: every part LayerPartBytes from the layer's first byte, the last
// holding what remains, and a part past the layer's end no byte and no run.
// THE ONE CUT (ADR-0008), its start with it: [StoreLayer] reads each part's
// bytes by it and [FetchLayerPart] writes them back by it, so a part is laid
// back exactly where it was taken from — the place in the layer it starts at
// as much as the runs it is — and no caller computes either of its own.
func layerPart(sections [3]int64, i int) partCut {
	start := int64(i) * LayerPartBytes
	size := max(0, min(LayerPartBytes, sections[0]+sections[1]+sections[2]-start))
	return partCut{start: start, size: size, spans: layerSpans(sections, start, size)}
}

// layerSpans is where the size bytes of a layer that begin at the layer's
// byte start lie in its three files, whose sizes are sections: the runs in
// byte order, none empty, none past the layer's end — the runs of a part
// (layerPart).
func layerSpans(sections [3]int64, start, size int64) []layerSpan {
	var out []layerSpan
	var base int64 // where the current file begins in the layer
	for s, n := range sections {
		lo, hi := max(start, base), min(start+size, base+n)
		if lo < hi {
			out = append(out, layerSpan{section: s, offset: lo - base, bytes: hi - lo})
		}
		base += n
	}
	return out
}

// StoreLayer uploads a layer, the files at paths back to back, sections[i]
// bytes each (paths[2] "" exactly when sections[2] is 0; each file is held to
// its section's size first, as the cut is by sections), as LayerPartBytes
// parts stored as [LayerContentType], ALL AT ONCE, each Put under
// [UploadDeadline] of its size, and answers them in byte order. At once since
// Put mints a key as its upload begins: every key is then at most
// objstore.ReadBase + objstore.PaceFor(LayerPartBytes) old at the slowest legal
// pace (about 8 h 33 min, under objstore.RecordWithin); serial uploads would
// outlive it. The first failure cancels the rest and is the answer as the store
// gave it (an *objstore.Full among them); stored parts are the collector's. hit,
// if not nil, runs before each store read of a part at the LAYER offset it
// begins at, its error failing that upload (a gitletfault point, never imported).
func StoreLayer(ctx context.Context, p Putter, paths [3]string, sections [3]int64,
	hit func(ctx context.Context, offset int64) error) ([]LayerPart, error) {

	var total int64
	for i, n := range sections {
		switch {
		case n < 0:
			return nil, fmt.Errorf("gitlet: store a layer: its %s is %d bytes", layerSections[i], n)
		case (n > 0) != (paths[i] != ""):
			return nil, fmt.Errorf("gitlet: store a layer: its %s is %d bytes and named by the path %q; a file of "+
				"bytes has a path, and only one has", layerSections[i], n, paths[i])
		}
		total += n
	}
	switch {
	case sections[0] < 1 || sections[1] < 1:
		return nil, fmt.Errorf("gitlet: store a layer: a layer is a multi-pack-index and its bitmap at the least, "+
			"and its sections are %s", words.And(sections[:]))
	case total > MaxLayerParts*LayerPartBytes:
		return nil, fmt.Errorf("gitlet: store a layer: it is %d bytes, past the %d one layer's %d parts of %d may "+
			"hold (MaxLayerParts)", total, MaxLayerParts*LayerPartBytes, MaxLayerParts, LayerPartBytes)
	}
	var files [3]*os.File
	for i, path := range paths {
		if path == "" {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("gitlet: store a layer: %w", err)
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		switch {
		case err != nil:
			return nil, fmt.Errorf("gitlet: store a layer: %w", err)
		case info.Size() != sections[i]:
			return nil, fmt.Errorf("gitlet: store a layer: its %s, %s, is %d bytes and its section %d", layerSections[i],
				path, info.Size(), sections[i])
		}
		files[i] = f
	}
	parts := make([]LayerPart, (total+LayerPartBytes-1)/LayerPartBytes)
	g, gctx := errgroup.WithContext(ctx)
	for i := range parts {
		cut := layerPart(sections, i)
		g.Go(func() error {
			obj, err := storePart(gctx, p, files, cut, hit)
			if err != nil {
				return fmt.Errorf("gitlet: store part %d of the layer, its bytes %d to %d: %w", i, cut.start,
					cut.start+cut.size, err)
			}
			parts[i] = LayerPart{Object: obj.Key, Hash: obj.Hash, Size: cut.size}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return parts, nil
}

// storePart uploads the bytes of the part of a layer cut says — the runs it
// cuts them into, read from files — under UploadDeadline of its size, and
// answers the stored object, which holds exactly those bytes.
func storePart(ctx context.Context, p Putter, files [3]*os.File, cut partCut,
	hit func(ctx context.Context, offset int64) error) (objstore.Object, error) {

	ctx, cancel := context.WithTimeout(ctx, UploadDeadline(cut.size))
	defer cancel()
	runs := make([]io.Reader, len(cut.spans))
	for k, s := range cut.spans {
		runs[k] = io.NewSectionReader(files[s.section], s.offset, s.bytes)
	}
	in := io.MultiReader(runs...)
	if hit != nil {
		in = &hitReader{ctx: ctx, hit: hit, r: in, at: cut.start}
	}
	obj, err := p.Put(ctx, in, cut.size, objstore.PutMeta{ContentType: LayerContentType})
	if err != nil {
		return objstore.Object{}, err
	}
	if err = obj.Validate(); err != nil {
		// A store that answers what no row can name: a row naming it
		// would be refused on every node.
		return objstore.Object{}, fmt.Errorf("the store answered an object no row can name: %w", err)
	}
	if obj.Size != cut.size {
		// A file shortened under the upload: the part would be named by
		// bytes that are not the layer's.
		return objstore.Object{}, fmt.Errorf("the store took %d bytes of the part's %d", obj.Size, cut.size)
	}
	return obj, nil
}

// LayerReader is the object store's read of one stored object, verified
// against the row that names it: what a Gitlet node reads a layer's parts by
// ([FetchLayerPart]). *objstore.Store satisfies it, as it does the cache's own
// read of a pack bundle (internal/gitlet/cache's Objects), so the installer
// hands its store to both: the read holds back an object's last byte until
// the whole of it hashes to its row's digest, ends a damaged object short with
// objstore.ErrCorrupt, and answers a missing one objstore.ErrNotFound before
// its first byte.
type LayerReader interface {
	Open(ctx context.Context, o objstore.Object) (io.ReadCloser, error)
}

// FetchLayerPart reads part i of row through r, the store's verified read, and
// writes its runs through buf at their places in files (ordered as
// [LayerRef.Sections]; one the part does not reach may be nil), by the cut
// [StoreLayer] made (layerPart). So a node fetches a layer's parts at once, one
// fetch slot and buffer a part as for a pack, and checks the files with
// [CheckLayer] once all have landed. The part is whole only once its last byte
// is read, which the store hands out only once the part hashes to the row's
// digest; on a failure the caller discards the files. A row [LayerRef.Check]
// refuses is [ErrBadLayer] before any byte moves; a missing part is
// objstore.ErrNotFound and a wrong one objstore.ErrCorrupt, both definite via
// errors.Is; a store failure, or this node's disk failing a write ("on this
// node"), is answered as it came.
func FetchLayerPart(ctx context.Context, r LayerReader, row LayerRef, i int, files [3]io.WriterAt,
	buf []byte) error {

	if err := row.Check(); err != nil {
		return fmt.Errorf("%w: %w", ErrBadLayer, err)
	}
	switch {
	case i < 0 || i >= len(row.Parts):
		return fmt.Errorf("gitlet: fetch part %d of layer %s: the layer has %d parts", i, row.LayerID, len(row.Parts))
	case len(buf) == 0:
		return fmt.Errorf("gitlet: fetch part %d of layer %s: no buffer to copy it through", i, row.LayerID)
	}
	// The part's place by the one cut StoreLayer made it by; its size is the
	// row's part's, which row.Check holds to that cut (every part but the
	// last LayerPartBytes, the parts summing to the sections).
	part, cut := row.Parts[i], layerPart(row.Sections(), i)
	spans := cut.spans
	for _, s := range spans {
		if files[s.section] == nil {
			return fmt.Errorf("gitlet: fetch part %d of layer %s: it holds bytes of the %s, and no file was handed "+
				"for it", i, row.LayerID, layerSections[s.section])
		}
	}
	rc, err := r.Open(ctx, part.Content())
	if err != nil {
		return fmt.Errorf("gitlet: fetch part %d of layer %s (object %s): %w", i, row.LayerID, part.Object, err)
	}
	defer func() { _ = rc.Close() }()
	for _, s := range spans {
		at, left := s.offset, s.bytes
		for left > 0 {
			n, rerr := rc.Read(buf[:min(int64(len(buf)), left)])
			if n > 0 {
				if _, werr := files[s.section].WriteAt(buf[:n], at); werr != nil {
					return fmt.Errorf("gitlet: fetch part %d of layer %s: write its bytes into the %s on this node: %w",
						i, row.LayerID, layerSections[s.section], werr)
				}
				at, left = at+int64(n), left-int64(n)
			}
			switch {
			case rerr == nil:
			case errors.Is(rerr, io.EOF) && left > 0:
				// No verified read ends this way — a short object is
				// ErrCorrupt — but a reader that did has handed over less
				// than the part, which is not the part.
				return fmt.Errorf("gitlet: fetch part %d of layer %s (object %s): it ended %d bytes short of its %d: %w",
					i, row.LayerID, part.Object, left, part.Size, objstore.ErrCorrupt)
			case errors.Is(rerr, io.EOF):
			default:
				return fmt.Errorf("gitlet: fetch part %d of layer %s (object %s): %w", i, row.LayerID, part.Object,
					rerr)
			}
		}
	}
	// The store hands out a part's last byte only once the whole part is
	// verified, so every byte is the row's now; one more read is the end,
	// and anything else is more than the part.
	switch n, rerr := rc.Read(buf[:1]); {
	case n > 0:
		return fmt.Errorf("gitlet: fetch part %d of layer %s (object %s): the store handed over more than its %d "+
			"bytes: %w", i, row.LayerID, part.Object, part.Size, objstore.ErrCorrupt)
	case errors.Is(rerr, io.EOF):
		return nil
	case rerr == nil:
		return fmt.Errorf("gitlet: fetch part %d of layer %s (object %s): the read did not end after its %d bytes",
			i, row.LayerID, part.Object, part.Size)
	default:
		return fmt.Errorf("gitlet: fetch part %d of layer %s (object %s): %w", i, row.LayerID, part.Object, rerr)
	}
}
