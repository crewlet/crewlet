package cache

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// WHAT A COMPACTION ASKS OF THE CACHE. A repository's home git node merges its
// small packs (internal/code/compact), and the cache is where it reads them:
// the live packs of the incarnation it synced, to plan from ([Cache.Packs]);
// a maintenance slot and the build's memory, to build in
// ([Cache.AdmitCompaction]); a scratch leaning on the incarnation, to build
// into (Cache.NewScratch); and once the outputs' rows have applied, their
// files linked in by adoption (Cache.Adopt), so the node that built them
// downloads none of them back. THE COMPACTOR NEVER WRITES AN INCARNATION: its
// pack-objects writes only into its own scratch, and the inputs leave the
// incarnation as every retired pack does — by the next round's swap (swap.go).

// Packs is the live rows of the incarnation s pins, as the rounds over it left
// them, sorted by pack id: each row's id, its git checksum, its bundle's bytes
// and its two indexes' — which tell its pack section, and whether it is FULL
// (code.PackRow.Full) — and the objects its row counts: what a compaction's
// plan weighs a pack by. Nothing else a snapshot's row carries — its object,
// part, kind, state and position stay zero (livePack.row). Copied under the
// entry's lock, so a round moving the set meanwhile changes nothing handed
// out; nil for a Synced this cache did not hand out.
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
		out = append(out, p.row(id))
	}
	s.entry.mu.Unlock()
	slices.SortFunc(out, func(a, b code.PackRow) int { return strings.Compare(a.PackID, b.PackID) })
	return out
}

// Compacting is one compaction's admission to this node's maintenance lane:
// a maintenance slot reserved (gitproc.Reserve) and the build's memory
// charged against the node's background budget as a RUN of the maintainer's,
// both held until Release. A compaction is BACKGROUND WORK and is held to the
// maintainer's rules — never the slot the maintainer keeps for a person's
// check, never a charge the node's memory cannot hold, never the room a pass
// kept for a repository it held back.
type Compacting struct {
	c    *Cache
	slot *gitproc.Reserved
	mem  int64
	// once makes Release idempotent.
	once sync.Once
}

// AdmitCompaction admits a compaction of s's repository, merging inputs
// objects in all, or refuses it at once, having started nothing:
//
//   - ErrNoMemory: the charge alone is past what this node can give its
//     background work — definite for this node at this memory, logged once an
//     incarnation (code_cache_compaction_refused) and counted
//     (crewlet.code.cache.maintenance.refusals, reason memory, step compact).
//     A git node with more memory compacts it if it becomes the home, and this
//     one once its memory is raised: the budget is read again every
//     maintainer pass, so the caller asks again at its next poll;
//   - gitproc.ErrBusy: no maintenance slot the maintainer may use is free, or
//     the background work in flight — the maintainer's runs, the room a pass
//     kept standing for a repository it held back, a round's index, a check —
//     leaves the build's memory no room: a moment's state;
//   - ErrStopped, and ErrInvalid for a Synced this cache did not hand out or
//     a negative count.
//
// The charge is chargeOver(inputs, CompactBytesPerObject, inputs) — the
// merged packs' objects, with room beside them for one transfer of the
// repository, as every background step is charged — and
// CompactWindowBytes(limits), the build's delta window, which no count of
// objects charges.
//
// ITS PIN, ITS SLOT AND ITS RELEASE are a check's (Cache.AdmitVerify): a slot
// held from the admission to the build's last process, so the build and the
// rebuild its bundle rule may ask for (internal/code/compact) run in it
// without asking the lane again, and the incarnation pinned by s, which its
// holder keeps until the compaction is answered. ITS MEMORY IS A RUN'S, never
// a check's (maintFlight.admitOther): a check is a person's, admitted beside
// the maintainer's runs and leaving its slots alone, while a compaction holds
// a maintenance slot for a whole build as a run does — so it is admitted as a
// run, at the maintainer's limit of maintenanceRuns slots and its
// allowance, beside the room the latest pass kept standing, and counted in
// flight until Release. One run in flight more, so a pass that finds the lane
// full of the maintainer's runs and a compaction starts nothing beside them.
func (c *Cache) AdmitCompaction(s Synced, inputs int64, limits code.Limits) (*Compacting, error) {
	switch {
	case c.isStopped():
		return nil, ErrStopped
	case inputs < 0:
		return nil, fmt.Errorf("%w: a compaction merges %d objects", ErrInvalid, inputs)
	}
	if err := c.handedOut(s, "a compaction's admission"); err != nil {
		return nil, err
	}
	label := cmp.Or(s.Repo.Name, s.entry.id)
	mem := chargeOver(inputs, CompactBytesPerObject, inputs) + CompactWindowBytes(limits)
	if budget := c.maint.budgetNow(); mem > budget {
		c.compactionRefused(s, mem)
		return nil, fmt.Errorf("cache: compact %s: %w: its build needs about %d bytes, and this node can give "+
			"its background work %d of its memory", label, ErrNoMemory, mem, budget)
	}
	slot, err := c.git.Reserve()
	if err != nil {
		return nil, fmt.Errorf("cache: compact %s: %w", label, err)
	}
	lane := c.git.Slots(gitproc.ClassMaintenance)
	switch c.maint.admitStanding(mem, maintenanceRuns(lane), c.maintMemory) {
	case admitted:
		return &Compacting{c: c, slot: slot, mem: mem}, nil
	case pastBudget:
		// The budget was lowered between the two reads: as definite as a
		// charge past it at the first.
		slot.Release()
		c.compactionRefused(s, mem)
		return nil, fmt.Errorf("cache: compact %s: %w: its build needs about %d bytes, and this node can give "+
			"its background work %d of its memory", label, ErrNoMemory, mem, c.maint.budgetNow())
	default:
		slot.Release()
		return nil, fmt.Errorf("cache: compact %s: %w: the maintenance this node is running, and the room it "+
			"keeps for a repository it held back, leave a build of about %d bytes no slot or no room; it is "+
			"asked again at the next poll", label, gitproc.ErrBusy, mem)
	}
}

// compactionRefused logs and counts a compaction of s's repository refused
// for memory, once an incarnation however many polls find it again — the
// incarnation's memoryRefused step stepCompact, which a swap carries.
func (c *Cache) compactionRefused(s Synced, mem int64) {
	e, inc := s.entry, s.inc
	e.mu.Lock()
	first := inc.memoryRefused&stepCompact == 0
	inc.memoryRefused |= stepCompact
	e.mu.Unlock()
	if !first {
		return
	}
	m := c.nodeMemory()
	c.log.Warn("code_cache_compaction_refused", "repo", cmp.Or(s.Repo.Name, e.id), "estimate", mem,
		"budget", c.maint.budgetNow(), "memory", m.Bytes, "memory_from", m.From, "reserved", c.memReserve)
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheMaintenanceRefusals, 1,
			metrics.Attrs{"reason": refusedForMemory, "step": stepCompactName})
	}
}

// Run runs one git process of the build in the reserved slot
// (gitproc.Reserved.Run): a maintenance-class process, one at a time, and
// nothing once Released.
func (a *Compacting) Run(ctx context.Context, cmd gitproc.Cmd) (gitproc.Result, error) {
	return a.slot.Run(ctx, cmd)
}

// Release gives the slot and the charge back, once however often it is
// called — after the build's last git process, before anything the
// compaction does that runs no git (its uploads, its proposal, its adoption)
// — and wakes the maintainer, so a repository held back for the room the
// build took is taken up at once.
func (a *Compacting) Release() {
	a.once.Do(func() {
		a.slot.Release()
		a.c.maint.done(a.mem)
		a.c.kickMaintainer()
	})
}
