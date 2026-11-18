package compact

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// The expunge's due scan (package doc, "The expunge") weighs the unreachable
// bytes of exactly the files a cycle may take, never the whole repository, in
// ONE walk for every file: `rev-list --objects --no-object-names` from every
// tip the scan keeps, through the copy's bitmaps, each object it prints marked
// reached in each file's own version-2 .idx (sorted ids, 256-entry fan-out).
// What an index names and the walk never printed is what the cycle drops of
// that file, weighed at its offset's distance to the next object's (the last
// one's to the trailer); an object two files hold is weighed in each, since
// retiring both frees both. Memory scales with the files, never the history:
// an id, weight and mark per object, one index read at a time ([indexBytes],
// charged by cache.Cache.AdmitExpungeScan), and the walk's output streamed a
// line at a time.

// scanBytesPerObject is what the scan holds per object of the packs it weighs
// while its walk runs: id (20 bytes), weight (8), mark (1). readBytesPerObject
// is what reading one pack's index adds per object, one pack at a time: the
// file (28, gitlet.IndexBytesFor) and its offsets sorted to weigh them (16).
const (
	scanBytesPerObject = 20 + 8 + 1
	readBytesPerObject = 28 + 16
)

// indexBytes is what a scan weighing files holds while its walk runs: every
// file's ids, weights and marks, and the largest file's read beside them —
// cache.Cache.AdmitExpungeScan's held bytes.
func indexBytes(files []pack) int64 {
	var all, largest int64
	for _, f := range files {
		all += f.objects
		largest = max(largest, f.objects)
	}
	return all*scanBytesPerObject + largest*readBytesPerObject
}

// packIndex is one eligible file's index as the scan reads it: object ids in
// index order (20 bytes each), each object's bytes in the pack, the fan-out
// that narrows a lookup, and which objects the walk reached.
type packIndex struct {
	checksum string
	oids     []byte
	weights  []int64
	fanout   [256]uint32
	reached  []bool
}

// readPackIndex reads the version-2 index at path of a stored pack of
// packBytes bytes, which has no 64-bit offset table (gitlet.IndexBytesFor),
// refusing one whose header, size, fan-out or offsets git would not write.
func readPackIndex(path, checksum string, packBytes int64) (*packIndex, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("compact: read pack %s's index: %w", checksum, err)
	}
	const header, fanoutBytes, hashBytes = 8, 256 * 4, 20
	bad := func(why string, args ...any) error {
		return fmt.Errorf("compact: pack %s's index %s: %s", checksum, path, fmt.Sprintf(why, args...))
	}
	if len(b) < header+fanoutBytes || !bytes.Equal(b[:4], []byte("\377tOc")) ||
		binary.BigEndian.Uint32(b[4:8]) != 2 {
		return nil, bad("is not a version-2 pack index")
	}
	ix := &packIndex{checksum: checksum}
	for i := range ix.fanout {
		ix.fanout[i] = binary.BigEndian.Uint32(b[header+4*i:])
		if i > 0 && ix.fanout[i] < ix.fanout[i-1] {
			return nil, bad("has a fan-out that falls at entry %d", i)
		}
	}
	n := int64(ix.fanout[255])
	if int64(len(b)) != gitlet.IndexBytesFor(n) {
		return nil, bad("is %d bytes, and an index of %d objects with no 64-bit offsets is %d", len(b), n,
			gitlet.IndexBytesFor(n))
	}
	ids := int64(header + fanoutBytes)
	offsets := ids + n*(hashBytes+4)
	trailer := packBytes - hashBytes
	type placed struct {
		at int64
		i  int
	}
	order := make([]placed, n)
	for i := range order {
		at := int64(binary.BigEndian.Uint32(b[offsets+4*int64(i):]))
		if at&(1<<31) != 0 || at < 12 || at >= trailer {
			return nil, bad("places object %d at %d, outside a pack of %d bytes", i, at, packBytes)
		}
		order[i] = placed{at: at, i: i}
	}
	slices.SortFunc(order, func(a, b placed) int { return cmp.Compare(a.at, b.at) })
	ix.weights = make([]int64, n)
	next := trailer
	for j := len(order) - 1; j >= 0; j-- {
		if order[j].at >= next {
			return nil, bad("places two objects at %d", order[j].at)
		}
		ix.weights[order[j].i] = next - order[j].at
		next = order[j].at
	}
	// Cloned, so b's crc and offset columns are not kept alive.
	ix.oids = slices.Clone(b[ids : ids+n*hashBytes])
	ix.reached = make([]bool, n)
	return ix, nil
}

// mark marks oid reached where ix holds it.
func (ix *packIndex) mark(oid [20]byte) {
	lo := 0
	if oid[0] > 0 {
		lo = int(ix.fanout[oid[0]-1])
	}
	hi := int(ix.fanout[oid[0]])
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		switch c := bytes.Compare(ix.oids[mid*20:mid*20+20], oid[:]); {
		case c == 0:
			ix.reached[mid] = true
			return
		case c < 0:
			lo = mid + 1
		default:
			hi = mid
		}
	}
}

// unreached is the bytes of ix's objects the walk never reached.
func (ix *packIndex) unreached() int64 {
	var n int64
	for i, r := range ix.reached {
		if !r {
			n += ix.weights[i]
		}
	}
	return n
}

// reachWriter is the walk's standard output, read a line at a time: each line
// one object id, marked in every file.
type reachWriter struct {
	files   []*packIndex
	partial []byte
}

func (w *reachWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.partial = append(w.partial, p...)
			break
		}
		line := p[:i]
		if len(w.partial) > 0 {
			w.partial = append(w.partial, line...)
			line = w.partial
		}
		if err := w.line(line); err != nil {
			return 0, err
		}
		w.partial = w.partial[:0]
		p = p[i+1:]
	}
	return n, nil
}

// line marks the object one line names.
func (w *reachWriter) line(line []byte) error {
	var oid [20]byte
	if len(line) != 40 {
		return fmt.Errorf("compact: rev-list printed %q where it names an object", line)
	}
	if _, err := hex.Decode(oid[:], line); err != nil {
		return fmt.Errorf("compact: rev-list printed %q where it names an object", line)
	}
	for _, ix := range w.files {
		ix.mark(oid)
	}
	return nil
}

// unreachableIn is, by checksum, the bytes of each of files that nothing the
// copy s pins reaches (its refs, and tips: the drops and rows in flight the
// scan keeps), in ONE walk in adm's slot (this file's head).
func (c *Compactor) unreachableIn(ctx context.Context, adm *cache.Admission, s cache.Synced, files []pack,
	tips []string) (map[string]int64, error) {

	w := &reachWriter{files: make([]*packIndex, 0, len(files))}
	for _, f := range files {
		ix, err := readPackIndex(filepath.Join(s.Dir, "objects", "pack", "pack-"+f.checksum+".idx"), f.checksum,
			f.packBytes)
		if err != nil {
			return nil, err
		}
		w.files = append(w.files, ix)
	}
	var in strings.Builder
	for _, oid := range tips {
		in.WriteString(oid + "\n")
	}
	if _, err := adm.Run(ctx, gitproc.Cmd{
		Dir: s.Dir, Class: gitproc.ClassMaintenance,
		Args: []string{"rev-list", "--objects", "--no-object-names", "--use-bitmap-index", "--ignore-missing",
			"--all", "--stdin"},
		Stdin:    strings.NewReader(in.String()),
		Stdout:   w,
		Deadline: gitproc.ReadDeadline + gitproc.HistoryWalk(s.Repo.LiveObjects),
	}); err != nil {
		return nil, fmt.Errorf("compact: walk what %s's tips reach: %w", s.Repo.Name, err)
	}
	if len(w.partial) > 0 {
		return nil, fmt.Errorf("compact: rev-list ended mid-line, on %q", w.partial)
	}
	out := make(map[string]int64, len(w.files))
	for _, ix := range w.files {
		out[ix.checksum] = ix.unreached()
	}
	return out, nil
}
