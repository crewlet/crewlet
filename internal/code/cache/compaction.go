package cache

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
)

// WHAT A COMPACTION ASKS OF THE CACHE. A repository's maintainer merges its
// small packs (internal/code/compact), and the cache is where it reads them:
// the live packs of the incarnation it synced, to plan from ([Cache.Packs]);
// a maintenance slot and the build's memory, to build in
// ([Cache.AdmitCompaction], background.go); a scratch to build into, leaning
// on a store of exactly the packs it merges, linked from the incarnation
// (Cache.NewMergeScratch), so the build's walk for its name hashes ends at the
// roll's edge rather than running the repository's history; and once the
// outputs' rows have applied, their files linked in by adoption (Cache.Adopt),
// so the node that built them downloads none of them back. THE COMPACTOR
// NEVER WRITES AN INCARNATION: its pack-objects writes only into its own
// scratch, and the inputs leave the incarnation as every retired pack does —
// by the next round's swap (swap.go).

// Packs is the live rows of the incarnation s pins, as the rounds over it left
// them, sorted by pack id: each row's id, its git checksum, its bundle's bytes
// and its two indexes' — which tell its pack section, and whether it is FULL
// (code.PackRow.Full) — and the objects its row counts: what a compaction's
// plan, and a publication's, weighs a pack by. Nothing else a snapshot's row
// carries — its object, part, kind, state and position stay zero
// (livePack.row). Copied under the entry's lock, so a round moving the set
// meanwhile changes nothing handed out; nil for a Synced this cache did not
// hand out.
//
// NEVER A ROW AN EXPUNGE'S BEGIN LEFT DROPPING: no compaction takes one as an
// input — its decide refuses it stale (code's liveInput), so a plan naming
// one would be refused at every poll until the expunge commits — and the
// compaction's build reads its inputs through its merge's store, links of
// exactly them from the incarnation's objects/pack (Cache.NewMergeScratch),
// where no dropping pack lies: a merge naming one is refused [ErrInvalid].
// Nor is one a publication's: a dropping pack is no pack a layer may name, and
// the committer refuses a publication while the repository's expunge is open
// (code's rule 6g).
//
// AS THE INCARNATION HOLDS THEM, which may be past s.Position: a round that
// adds packs applies in place on the incarnation s pins. A plan from them is
// judged by the committer against the rows of its own snapshot, which refuses
// an input that is no longer live, so a plan a little newer than its Seen is
// at worst one the next poll makes again.
func (c *Cache) Packs(s Synced) []code.PackRow {
	if c.handedOut(s, "a compaction's plan") != nil {
		return nil
	}
	s.entry.mu.Lock()
	out := make([]code.PackRow, 0, len(s.inc.live))
	for id, p := range s.inc.live {
		if !p.dropping {
			out = append(out, p.row(id))
		}
	}
	s.entry.mu.Unlock()
	slices.SortFunc(out, func(a, b code.PackRow) int { return strings.Compare(a.PackID, b.PackID) })
	return out
}
