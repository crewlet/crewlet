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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// WHAT A SCHEDULED CYCLE'S OWN INPUTS CAN DROP: the scan on the maintainer's
// own schedule weighs the unreachable bytes of exactly the files a cycle may
// take — those whose newest content arrived code.PruneGrace or longer ago —
// never of the whole repository. Weighed over the whole repository, garbage
// in a YOUNG pack — one a compaction merged with a later push, whose output
// arrives when its newest input did — made the repository due while every
// pack the cycle could take held none of it, and the cycle rewrote, stored
// and had every Gitlet node download the old, garbage-free packs to free
// nothing, every week.
//
// ONE WALK, every file at once: `rev-list --objects --no-object-names` from
// every ref of the copy and every drop and row in flight the scan keeps, read
// through the copy's published bitmaps where they cover it, each object it
// prints looked up in each file's own .idx — the version-2 index git wrote,
// its ids sorted and its 256-entry fan-out narrowing the search — and marked
// reached there. What a file's index names and the walk never printed is what
// the cycle drops of it, each object weighed at the bytes it takes in that
// file: the distance from its offset to the next object's, the last one's to
// the pack's trailer. An object two eligible files both hold is weighed in
// each, since a cycle retiring both frees both copies.
//
// HELD IN MEMORY BY THE FILES, never by the history: an id, a weight and a
// mark an object of the eligible files, and one file's index read at a time
// beside them ([indexBytes]), which the scan's admission charges beside room
// for the walk (cache.Cache.AdmitExpungeScan); the walk's own output is
// streamed through, a line at a time.

// scanBytesPerObject is what the scan holds an object of the packs it weighs
// for as long as its walk runs: its id (20 bytes), its weight (8) and its
// mark (1). readBytesPerObject is what reading one pack's index holds an
// object beside that, one pack at a time: the file itself (twenty-eight
// bytes an object, code.IndexBytesFor) and its offsets sorted to weigh them
// (sixteen).
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

// packIndex is one eligible file's index as the scan reads it: its object ids
// in index order (twenty bytes each), each object's bytes in the pack, the
// fan-out that narrows a lookup, and which objects the walk reached.
type packIndex struct {
	checksum string
	oids     []byte
	weights  []int64
	fanout   [256]uint32
	reached  []bool
}

// readPackIndex reads the version-2 index at path of a pack of packBytes
// bytes — a stored pack's, which carries no 64-bit offset table
// (code.IndexBytesFor) — refusing one whose header, size, fan-out or offsets
// are not what git writes for that pack.
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
	if int64(len(b)) != code.IndexBytesFor(n) {
		return nil, bad("is %d bytes, and an index of %d objects with no 64-bit offsets is %d", len(b), n,
			code.IndexBytesFor(n))
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
	// A COPY of the ids alone, so the file's crc and offset columns go with
	// the read.
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

// reachWriter is the walk's standard output, read a line at a time as git
// writes it: each line one object id, marked in every file.
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

// unreachableIn is, by checksum, the bytes of each of files that no tip of the
// copy s pins reaches — its refs, and tips, the drops and rows in flight the
// scan keeps — ONE walk in adm's slot (this file's head).
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
