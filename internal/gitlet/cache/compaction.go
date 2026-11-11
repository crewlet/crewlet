package cache

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// ErrSuperseded is a read of what a swap carries — a copy's live packs, its
// dropping ones — asked of a Synced whose incarnation a swap has since
// replaced: the swap moved that state into the copy now serving, so the pinned
// one holds none of it, and an answer from it would read as a repository
// holding no pack. A pass that meets it is busy: its next sync pins the copy
// serving and reads what that holds.
var ErrSuperseded = errors.New("cache: a swap replaced the copy the sync pinned")

// WHAT A COMPACTION ASKS OF THE CACHE. A repository's maintainer merges its
// small packs (internal/gitlet/compact), and the cache is where it reads them:
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
// (gitlet.PackRow.Full) — the objects its row counts: what a compaction's plan,
// and a publication's, weighs a pack by — and the position its content dates
// from (gitlet.PackRow.Content), which a purge's cycle chooses its inputs by:
// zero for a pack this node adopted whose row no round has served it since,
// read as older than every request (livePack.content). Nothing else a
// snapshot's row carries — its object, part, kind, state and the position it
// was added at stay zero (livePack.row). Copied under the entry's lock, so a
// round moving the set meanwhile changes nothing handed out; nil for a Synced
// this cache did not hand out.
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
//
// NEVER FROM A COPY A SWAP REPLACED: a swap moves the live set into the copy
// serving and leaves the one s pins none, so an answer from it would be a
// repository holding no pack — a compaction planned over nothing, a weekly
// expunge's lasting clean scan of packs it never read. Refused
// [ErrSuperseded] there, which a pass answers busy, its next sync pinning the
// copy serving. [ErrInvalid] for a Synced this cache did not hand out.
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
