package cache

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// ErrSuperseded refuses a read of what a swap carries (a copy's live or
// dropping packs) from a Synced whose incarnation a swap has replaced: that
// copy holds none of it, so an answer would read as a repository holding no
// pack (a compaction planned over nothing, an expunge's clean scan of packs
// it never read). A pass answers it busy; its next sync pins the copy serving.
var ErrSuperseded = errors.New("cache: a swap replaced the copy the sync pinned")

// What a compaction (internal/gitlet/compact) asks of the cache: the live
// packs of its synced incarnation to plan from ([Cache.Packs]); a maintenance
// slot and memory to build in ([Cache.AdmitCompaction], background.go); a
// scratch whose store links exactly the packs it merges
// (Cache.NewMergeScratch), so the build's name-hash walk ends at the roll's
// edge rather than running the repository's history; and, once the outputs'
// rows apply, their files linked in (Cache.Adopt), so the builder downloads
// none back. THE COMPACTOR NEVER WRITES AN INCARNATION: pack-objects writes
// only its scratch, and the inputs leave by the next round's swap (swap.go).

// Packs is the live rows of the incarnation s pins, sorted by pack id, with
// what a compaction's or publication's plan weighs a pack by: id, checksum,
// bundle and index bytes (whether FULL, gitlet.PackRow.Full), object count,
// and the content position a purge picks inputs by (gitlet.PackRow.Content;
// zero, older than every request, for an adopted pack no round has served
// since). Other row fields stay zero (livePack.row). Copied under the entry's
// lock; it may be past s.Position, since a round applies in place, and the
// committer refuses an input no longer live. Never a row an expunge left
// dropping: decide refuses it stale (gitlet's liveInput), the merge's store
// holds none, and no layer may name one (gitlet's rule 6g). [ErrSuperseded]
// once a swap replaced s's copy, which holds no pack; [ErrInvalid] for a
// Synced this cache did not hand out.
func (c *Cache) Packs(s Synced) ([]gitlet.PackRow, error) {
	if err := c.handedOut(s, "a compaction's plan"); err != nil {
		return nil, err
	}
	s.entry.mu.Lock()
	if s.inc.successor != nil {
		s.entry.mu.Unlock()
		return nil, fmt.Errorf("%w: %s, read for its live packs", ErrSuperseded, s.Dir)
	}
	out := make([]gitlet.PackRow, 0, len(s.inc.live))
	for id, p := range s.inc.live {
		if !p.dropping {
			out = append(out, p.row(id))
		}
	}
	s.entry.mu.Unlock()
	slices.SortFunc(out, func(a, b gitlet.PackRow) int { return strings.Compare(a.PackID, b.PackID) })
	return out, nil
}
