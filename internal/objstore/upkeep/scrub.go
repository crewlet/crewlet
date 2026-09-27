package upkeep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/crewlet/crewlet/internal/objstore/disk"
	"github.com/crewlet/crewlet/internal/objstore/placement"
)

// ScrubInterval is how long one scrub cycle takes — every chunk this node
// holds read and checked against its name once.
//
// A WEEK, Ceph's osd_deep_scrub_interval, for Ceph's reason: a chunk nobody
// reads can rot unnoticed, and it is noticed in time only if it is found while
// its other copies are still good. Every copy is checked within a week, so rot
// has to strike every copy of one chunk inside the same week to lose it, where
// a store that only checked on read would lose every chunk nobody happened to
// open — and reading faster buys that margin at the cost of disk time serving
// needs.
const ScrubInterval = 7 * 24 * time.Hour

// ScrubFloor and ScrubCeiling bound the scrub's rate, in bytes a second: it
// reads what the node holds over [ScrubInterval], but never slower than the
// floor nor faster than the ceiling.
//
// THE FLOOR, a mebibyte a second, is so a node holding little does not spend a
// week reading at a few bytes a second what it can check in minutes; its cycle
// finishes early and the next starts when the week is up. THE CEILING,
// thirty-two, is a tenth of an SSD's sequential read and a fifth of a spinning
// disk's, so the scrub never competes with serving: a node holding more than
// the ceiling reads in a week — about nineteen tebibytes — takes longer than a
// week per cycle rather than taking the disk from the reads it exists for.
const (
	ScrubFloor   = 1 << 20
	ScrubCeiling = 32 << 20
)

// scrubCursorName is the file in the chunk directory the scrub's cursor
// persists in, so a restart resumes the week rather than starting it again —
// a node restarted every few days would otherwise never check the slots at
// the end.
const scrubCursorName = ".scrub"

// scrubSaveEvery is how often a scrub in progress persists its cursor: a crash
// costs at most this much of the scrub again, and it is a small write a minute.
const scrubSaveEvery = time.Minute

// scrubRetry is how long a scrub waits after a walk the disk refused before it
// tries the same slots again: long enough that a failing disk is not listed in
// a loop, short enough that a transient costs a minute of a week.
const scrubRetry = time.Minute

// ScrubStatus is where the scrub is in its cycle and what it has found.
type ScrubStatus struct {
	// CycleStarted is when the current cycle began, zero before the first.
	CycleStarted time.Time `json:"cycle_started"`

	// Progress is how much of the cycle is done: the slots finished over
	// every slot there is, 0..1.
	Progress float64 `json:"progress"`

	// Verified is how many chunks this cycle has read and found intact,
	// Rotten how many it found no longer matching their names — each
	// removed, for repair to fetch a good copy — and LastRotten when it
	// last found one, in this cycle or any before.
	Verified   int       `json:"verified"`
	Rotten     int       `json:"rotten"`
	LastRotten time.Time `json:"last_rotten,omitzero"`

	// Unreadable is how many chunks this cycle could not read at all — a
	// file that opened and would not read, removed for repair to fetch a
	// good copy as a rotten one is, and one the disk would not even open
	// or would not let go of, which stays — and LastUnreadable when it last
	// found one, in this cycle or any before. Every one is stepped past,
	// so a count that keeps rising is a disk failing chunk by chunk rather
	// than a scrub that stopped.
	Unreadable     int       `json:"unreadable"`
	LastUnreadable time.Time `json:"last_unreadable,omitzero"`

	// Error is what stopped the scrub last — a walk the disk refused, which
	// it retries shortly — empty while it is running.
	Error string `json:"error,omitempty"`
}

// scrubCursor is the scrub's progress as it persists.
type scrubCursor struct {
	// Started is when the cycle began, zero when none has.
	Started time.Time `json:"started"`

	// Next is the first slot not yet finished: every chunk in a slot
	// below it has been checked this cycle.
	Next int `json:"next"`

	// Bytes is what the node held when the cycle began, which its rate is
	// derived from.
	Bytes int64 `json:"bytes"`

	Verified       int       `json:"verified"`
	Rotten         int       `json:"rotten"`
	LastRotten     time.Time `json:"last_rotten,omitzero"`
	Unreadable     int       `json:"unreadable,omitempty"`
	LastUnreadable time.Time `json:"last_unreadable,omitzero"`
}

// finished reports whether there is no cycle in progress.
func (c scrubCursor) finished() bool {
	return c.Started.IsZero() || c.Next >= placement.Slots
}

func (c scrubCursor) status(failure string) ScrubStatus {
	return ScrubStatus{CycleStarted: c.Started, Progress: float64(c.Next) / placement.Slots,
		Verified: c.Verified, Rotten: c.Rotten, LastRotten: c.LastRotten,
		Unreadable: c.Unreadable, LastUnreadable: c.LastUnreadable, Error: failure}
}

// scrubRate is the bytes a second a cycle over held bytes reads at.
func scrubRate(held int64) float64 {
	rate := float64(held) / ScrubInterval.Seconds()
	return min(max(rate, ScrubFloor), ScrubCeiling)
}

// Scrub reads and checks every chunk this node holds, in slot order, at a
// paced rate, one cycle every [ScrubInterval] — until ctx ends. A chunk that no
// longer matches its name, or whose bytes will not read, is removed
// ([disk.Store.Verify]) and counted, and the next repair fetches a good copy
// of it from a peer.
//
// ONE PER NODE: a second call while one runs answers an error at once, since
// two would share one cursor and each check what the other had.
func (n *Node) Scrub(ctx context.Context) error {
	if !n.scrubbing.TryLock() {
		return errors.New("objstore/upkeep: this node is already scrubbing")
	}
	defer n.scrubbing.Unlock()
	cur := n.loadCursor(ctx)
	n.showScrub(cur, "")
	// ONE PACER FOR THE LIFE OF THE SCRUB, across windows and cycles: one
	// per window would let the first read of every window go unpaced.
	pace := &pacer{now: n.opts.Now, wait: n.wait}
	for ctx.Err() == nil {
		if cur.finished() {
			if !cur.Started.IsZero() {
				// THE NEXT CYCLE starts a week after this one did, never
				// later than a week from now: a cursor stamped by a clock
				// that has since been set back must not hold the scrub
				// off for longer than a cycle.
				due := min(cur.Started.Add(ScrubInterval).Sub(n.opts.Now()), ScrubInterval)
				if !n.wait(ctx, due) {
					return nil
				}
			}
			held, err := n.heldBytes()
			if err != nil {
				if !n.scrubFailed(ctx, cur, err) {
					return nil
				}
				continue
			}
			cur = scrubCursor{Started: n.opts.Now().UTC(), Bytes: held,
				LastRotten: cur.LastRotten, LastUnreadable: cur.LastUnreadable}
			n.saveCursor(ctx, cur)
			n.showScrub(cur, "")
			log.InfoContext(ctx, "object_scrub_started", "bytes", held,
				"bytes_per_second", int64(scrubRate(held)))
		}
		pace.rate = scrubRate(cur.Bytes)
		if err := n.scrubWindow(ctx, &cur, pace); err != nil {
			if ctx.Err() != nil {
				n.saveCursor(context.WithoutCancel(ctx), cur)
				return nil
			}
			if !n.scrubFailed(ctx, cur, err) {
				return nil
			}
		}
	}
	return nil
}

// scrubWindow checks the chunks from the cursor to the end of its read window
// ([readSlots]), pacing each read, and moves the cursor past them.
//
// ONE BAD CHUNK NEVER STOPS IT. A chunk that will not read is counted and
// stepped past, whatever the disk said about it; only a walk that fails —
// which leaves the window's chunks unknown — and a store released under it
// return, to try the same slots again. A read error once returned here as a
// walk failure did: the retry walked to the same chunk and failed the same
// way every minute, and every slot after it went unchecked for good — the
// rot the scrub exists to find, left for the other copies to catch.
func (n *Node) scrubWindow(ctx context.Context, cur *scrubCursor, pace *pacer) error {
	lo := cur.Next
	hi := (lo/readSlots + 1) * readSlots
	var held []disk.Held
	if err := n.opts.Local.WalkSlots(lo, hi, func(h disk.Held) error {
		held = append(held, h)
		return nil
	}); err != nil {
		return fmt.Errorf("objstore/upkeep: walk slots [%d, %d) to scrub them: %w", lo, hi, err)
	}
	saved := n.opts.Now()
	for _, h := range held {
		// Every slot below this chunk's is finished: the walk is in slot
		// order.
		cur.Next = h.Hash.Slot()
		if !pace.take(ctx, h.Size) {
			return ctx.Err()
		}
		ok, err := n.opts.Local.Verify(h.Hash)
		switch {
		case err == nil && ok:
			cur.Verified++
		case err == nil:
			cur.Rotten++
			cur.LastRotten = n.opts.Now().UTC()
			log.WarnContext(ctx, "object_chunk_rotten", "chunk", string(h.Hash),
				"detail", "the copy no longer matched its hash and was removed; "+
					"repair fetches a good one if the map places it here")
		case errors.Is(err, disk.ErrClosed):
			// RELEASED under the scrub: nothing further can be read, and
			// counting every chunk left as unreadable would say the disk
			// failed when the store only closed.
			return fmt.Errorf("objstore/upkeep: scrub %s: %w", h.Hash, err)
		case errors.Is(err, disk.ErrNotFound) && !errors.Is(err, disk.ErrUnreadable):
			// COLLECTED OR DROPPED since the walk: nothing to check.
		default:
			cur.Unreadable++
			cur.LastUnreadable = n.opts.Now().UTC()
			removed := errors.Is(err, disk.ErrNotFound)
			detail := "the copy could not be read and was removed; repair fetches a good one " +
				"if the map places it here"
			if !removed {
				detail = "the copy could not be read and is still there; the store counts the " +
					"error toward its health, and the scrub goes on past it"
			}
			log.WarnContext(ctx, "object_chunk_unreadable", "chunk", string(h.Hash),
				"error", err, "removed", removed, "detail", detail)
		}
		n.showScrub(*cur, "")
		if n.opts.Now().Sub(saved) >= scrubSaveEvery {
			n.saveCursor(ctx, *cur)
			saved = n.opts.Now()
		}
	}
	cur.Next = hi
	n.saveCursor(ctx, *cur)
	n.showScrub(*cur, "")
	if cur.Next >= placement.Slots {
		log.InfoContext(ctx, "object_scrub_finished", "started", cur.Started,
			"verified", cur.Verified, "rotten", cur.Rotten, "unreadable", cur.Unreadable)
	}
	return nil
}

// scrubFailed records a failure and waits [scrubRetry], reporting whether the
// scrub should go on.
func (n *Node) scrubFailed(ctx context.Context, cur scrubCursor, err error) bool {
	log.WarnContext(ctx, "object_scrub_failed", "error", err, "slot", cur.Next,
		"detail", "trying the same slots again shortly")
	n.showScrub(cur, err.Error())
	return n.wait(ctx, scrubRetry)
}

func (n *Node) showScrub(cur scrubCursor, failure string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.status.Scrub = cur.status(failure)
}

// heldBytes is how many bytes of chunks this node holds.
func (n *Node) heldBytes() (int64, error) {
	var total int64
	err := n.opts.Local.WalkSlots(0, placement.Slots, func(h disk.Held) error {
		total += h.Size
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("objstore/upkeep: measure what this node holds: %w", err)
	}
	return total, nil
}

// cursorPath is where the scrub's cursor persists.
func (n *Node) cursorPath() string {
	return filepath.Join(n.opts.Local.Root(), scrubCursorName)
}

// loadCursor reads the persisted cursor. None, or one this build cannot read,
// starts a cycle afresh — the safe direction, since it checks more rather than
// less.
func (n *Node) loadCursor(ctx context.Context) scrubCursor {
	raw, err := os.ReadFile(n.cursorPath())
	if errors.Is(err, fs.ErrNotExist) {
		return scrubCursor{}
	}
	var cur scrubCursor
	if err == nil {
		err = json.Unmarshal(raw, &cur)
	}
	if err == nil && (cur.Next < 0 || cur.Next > placement.Slots || cur.Bytes < 0) {
		err = fmt.Errorf("slot %d and %d bytes are not a cursor", cur.Next, cur.Bytes)
	}
	if err != nil {
		log.WarnContext(ctx, "object_scrub_cursor_unread", "error", err,
			"detail", "starting a new scrub cycle")
		return scrubCursor{}
	}
	return cur
}

// saveCursor persists the cursor: written beside it, synced and renamed over
// it, so a crash leaves the old cursor or the new one. A cursor that cannot be
// saved is logged and the scrub goes on — losing it costs checking slots
// again, never skipping them.
func (n *Node) saveCursor(ctx context.Context, cur scrubCursor) {
	if err := writeCursor(n.cursorPath(), cur); err != nil {
		log.WarnContext(ctx, "object_scrub_cursor_unsaved", "error", err,
			"detail", "a restart will scrub these slots again")
	}
}

func writeCursor(path string, cur scrubCursor) error {
	raw, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	next := path + ".next"
	f, err := os.OpenFile(next, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(next, path)
}

// pacer spaces reads so they average no more than rate bytes a second.
type pacer struct {
	rate float64
	now  func() time.Time
	wait func(ctx context.Context, d time.Duration) bool
	next time.Time
}

// take waits until a read of n bytes is due, reporting false if ctx ended
// first. Called BEFORE the read, so the time the read itself takes counts
// toward its share; time spent idle or behind earns no credit, so the scrub
// never bursts past its rate to catch up.
func (p *pacer) take(ctx context.Context, n int64) bool {
	now := p.now()
	if p.next.Before(now) {
		p.next = now
	}
	due := p.next.Sub(now)
	p.next = p.next.Add(time.Duration(float64(n) / p.rate * float64(time.Second)))
	return p.wait(ctx, due)
}
