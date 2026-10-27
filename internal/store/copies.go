package store

import (
	"errors"
	"math"
	"os"
	"slices"
	"sync"
	"time"
)

// A COPY HOLDS ITS FILE'S WRITE-AHEAD LOG FOR ITS WHOLE RUN, and this file is
// what a handle knows of that.
//
// The read transaction that keeps a copy one moment ([DB.vacuumInto]) is also
// what stops every checkpoint at that moment: the -wal beside the live file
// grows by every page the node commits while the copy runs, and the first
// checkpoint after the copy folds them back in. Two things outside this package
// have to know it, and each asks the handle:
//
//   - the `wal_large` alarm reads a large log as a checkpoint that is not
//     happening. A copy's is not happening either, but it folds back when the
//     copy ends, and an operator told that a reader is holding a snapshot open
//     goes looking for something that is not there — so a reading names the
//     copies holding the log ([DB.Copies]);
//   - every judgement of whether a copy has room — the donor snapshot's gate,
//     the fleet backup's, and the `volume_low` alarm saying the next copy
//     would not fit — reserved room for the copy alone, where it lands, while
//     the log it holds back grows on the live file's own volume: the one the
//     node commits to ([DB.CopyHoldsBack]).
//
// # What a copy holds back is measured, not assumed
//
// The log grows by the node's write rate times the copy's run, and the run is
// the file's size over the rate the copy reads it: two rates no constant
// knows. Measured once each way on the pinned driver (two cores, no race
// detector), a copy of a 139 MB replicated estate ran at about 104 MB/s for
// 1.3 s beside a writer committing about 1 500 one-row updates a second, and
// the log grew to 10.0 MB, where Turso's 1 000-frame autocheckpoint keeps it
// under 4.1 MB with nothing holding it, and fell back to 1.2 MB once the copy
// ended. At that copy rate a 44 GB estate's copy runs about seven minutes. So
// every copy MEASURES what it held: the -wal's size when its hold is taken and
// once the copy has returned, before the hold ends, as a share of the file it
// copied ([copies.end]). A copy taken later holds back about that share of the
// file as it stands then: its run scales with the file and the rate it reads
// at does not, and the node's write rate is the one the last copy saw.
//
// THE LAST COPY THAT FINISHED is the measure, because it is the nearest to the
// next one in time and so the best guess at the write rate the next will see;
// a copy that failed changes nothing, having held the log for part of a copy.
// Until a copy has finished through a handle — the first after a node opened
// the file, or after an adoption reopened it — there is no measure, and what a
// copy holds back is left to the margin each judgement keeps beside the copy
// itself. A write burst past the rate the last copy saw is the same, and the
// two alarms name it while it happens.

// walSidecar is the sidecar a database in WAL mode keeps its uncheckpointed
// pages in: the file a copy's hold makes grow.
const walSidecar = "-wal"

// Copy is a copy of a database file in flight, holding its write-ahead log.
type Copy struct {
	// Dest is the file the copy is being written to: the part file beside a
	// backup's destination, or the one in the snapshot directory — which is
	// what tells an operator a backup's copy from a donor snapshot's.
	Dest string

	// Since is when it began holding the log.
	Since time.Time
}

// copies is a handle's account of the copies taken of its file.
type copies struct {
	mu sync.Mutex

	// running is every copy holding the log now, in the order each began
	// holding it.
	running []*Copy

	// heldPerByte is what the last copy to finish held the log back by, as
	// a share of the file it copied, and measured whether a copy has.
	heldPerByte float64
	measured    bool
}

// copyHold is one copy from its hold to its end: the copy as [DB.Copies] lists
// it, and the sizes of the file and its log when the hold was taken, which its
// measure is taken against — sized says both could be read.
type copyHold struct {
	copy      *Copy
	wal, file int64
	sized     bool
}

// begin enters a copy of the file at path, to dest, whose view is held.
func (c *copies) begin(path, dest string) *copyHold {
	h := &copyHold{copy: &Copy{Dest: dest, Since: now()}}
	wal, walErr := fileSize(path + walSidecar)
	file, fileErr := fileSize(path)
	h.wal, h.file, h.sized = wal, file, walErr == nil && fileErr == nil && file > 0
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = append(c.running, h.copy)
	return h
}

// end removes a copy that has returned and, when it copied, takes its measure,
// answering what it held back: how far the log beside the file at path grew
// while the copy held it.
//
// CALLED BEFORE THE HOLD ENDS: once it has, a checkpoint may fold the log back
// in, and the file that showed what the copy held back no longer does.
func (c *copies) end(path string, h *copyHold, copied bool) (held int64) {
	wal, err := fileSize(path + walSidecar)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = slices.DeleteFunc(c.running, func(r *Copy) bool { return r == h.copy })
	if !copied || err != nil || !h.sized {
		return 0
	}
	// NEVER BELOW ZERO: nothing truncates a log a reader holds, and a log
	// that read smaller would be one measured wrong, not one that shrank.
	held = max(wal-h.wal, 0)
	c.heldPerByte, c.measured = float64(held)/float64(h.file), true
	return held
}

// Copies is every copy holding this file's write-ahead log now, the one
// holding it longest first; none on a handle no copy runs through.
func (d *DB) Copies() []Copy {
	if d == nil {
		return nil
	}
	d.copies.mu.Lock()
	defer d.copies.mu.Unlock()
	if len(d.copies.running) == 0 {
		return nil
	}
	out := make([]Copy, len(d.copies.running))
	for i, c := range d.copies.running {
		out[i] = *c
	}
	return out
}

// CopyHoldsBack is the write-ahead log a copy of this file, size bytes now, is
// expected to hold back on the file's own volume while it runs: the share of
// the file the last copy that finished through this handle held back, of size.
// Zero until one has finished — see the file doc for what stands in for the
// measure then — and on a handle that is not open, since a measure taken
// through it is of a file a reopen may since have replaced.
//
// THE CALLER STATES THE SIZE, having just measured it to weigh against a
// volume's free space: read here as well, it would be a second reading of a
// growing file at another instant, beside the one the judgement uses.
func (d *DB) CopyHoldsBack(size int64) int64 {
	if !d.isOpen() || size <= 0 {
		return 0
	}
	d.copies.mu.Lock()
	share, measured := d.copies.heldPerByte, d.copies.measured
	d.copies.mu.Unlock()
	if !measured {
		return 0
	}
	return int64(math.Ceil(share * float64(size)))
}

// fileSize is a file's size, and zero with no error when it does not exist: a
// database with nothing uncheckpointed has no -wal at all, which is a log of
// no bytes rather than one that cannot be read.
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
