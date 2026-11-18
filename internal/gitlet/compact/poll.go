package compact

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Poll is one pass of the compaction loop: LIST every repository at
// gitlet.CompactMinMerge live packs or more ([Compactor.listDue]); judge who
// maintains them off one listing ([Compactor.maintainedOf]); set the BEHIND
// reading before any memo or compaction, since at the bound a refused push
// moves no version ([Compactor.behindOf]); read the store's room only while a
// repository is held back ([Compactor.releaseHeld]); then compact the listed
// ones in order, one at a time, until the next tick, skipping one not due at
// its version and cap ([memo]), one held back, and one this node does not
// maintain at its [Compactor.PlacedBy] charge. An unreadable placement
// compacts nothing that pass rather than guess, logged once. A failed listing
// is the pass's error and keeps the alarm reading. Logged
// code_compaction_pass, at DEBUG unless it compacted, refused or failed one.
func (c *Compactor) Poll(ctx context.Context) (PollReport, error) {
	began := c.now()
	var report PollReport
	listed, err := c.listDue(ctx)
	if err != nil {
		return report, err
	}
	report.Listed = len(listed)
	c.forget(listed)
	maintained, unknown := c.maintainedOf(listed)
	behind := c.behindOf(listed, maintained, unknown)
	c.setBehind(behind)
	for _, b := range behind {
		report.Behind = append(report.Behind, b.name)
	}
	var released map[string]bool
	if unknown != nil {
		c.log.Warn("code_compaction_maintainer_unknown", "listed", len(listed), "error", unknown,
			"detail", "who maintains the listed repositories could not be read, so this pass compacts nothing")
	} else if released, err = c.releaseHeld(ctx, compactionLoop); err != nil {
		c.log.Warn("code_compaction_store_unread", "error", err,
			"detail", "the object store's room could not be read, so every repository held back for a full "+
				"store waits for the next pass")
	}
	until := began.Add(Poll)
	for _, cand := range listed {
		if unknown != nil || ctx.Err() != nil || !c.now().Before(until) {
			break
		}
		if c.notDueAt(cand) || (c.isHeld(compactionLoop, cand.ID) && !released[cand.ID]) || !maintained[cand.ID] {
			continue
		}
		report.Maintained++
		out, _ := c.Compact(ctx, cand)
		switch out {
		case OutcomeNotDue:
		case OutcomeApplied, OutcomePending, OutcomeUnknown:
			report.Due++
			report.Compacted++
		case OutcomeRefused:
			report.Due++
			report.Refused++
		case OutcomeFailed:
			report.Due++
			report.Failed++
		default:
			report.Due++
		}
	}
	log := c.log.Debug
	if report.Compacted+report.Refused+report.Failed > 0 {
		log = c.log.Info
	}
	log("code_compaction_pass", "listed", report.Listed, "maintained", report.Maintained, "due", report.Due,
		"compacted", report.Compacted, "refused", report.Refused, "failed", report.Failed,
		"behind", len(report.Behind), "took", c.now().Sub(began))
	return report, nil
}

// PlacedBy is the charge a repository of liveObjects live objects is placed
// by: the largest any maintainer step may be admitted at under the current
// epoch's limits (cache.Cache.MaintenanceCharge). One charge for every loop,
// so its packs' three writers (compactions, publications, expunges) land on
// one node whose admissions hold each of them.
func (c *Compactor) PlacedBy(liveObjects int64) int64 {
	return c.o.Cache.MaintenanceCharge(liveObjects, c.o.Limits())
}

// listDue is every page of the `compaction_due` listing, most live packs
// first, read at STALE: the router floors every code read at this node's own
// compactions (estate.CodeSeam.Compact), and a listing behind the head costs
// at most a repository planned a poll late, since a compaction syncs at its
// repository's version and its committer holds every input to the rows.
// Gitlet's page bounds make the largest company one or two reads.
func (c *Compactor) listDue(ctx context.Context) ([]gitlet.CompactionCandidate, error) {
	q := gitlet.ReadQuery{Kind: gitlet.ReadCompactionDue, MinPacks: c.bounds.minMerge,
		Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	var out []gitlet.CompactionCandidate
	for {
		a, err := c.o.Estate.Read(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("compact: list the repositories a compaction may be due in: %w", err)
		}
		out = append(out, a.Due...)
		if a.Next == "" {
			return out, nil
		}
		q.Cursor = a.Next
	}
}

// forget drops the compaction loop's memories (a not-due plan, a full-store
// hold) of a repository the listing no longer names, bounding them by the
// repositories listed; one that comes back is planned from its rows. A halved
// CAP is kept for the compactor's life (M3-decisions M3-O29): it is a fact of
// the repository's packs, which falling below the listing's floor does not
// change, and forgotten it would cost a failed build each time it grew back.
func (c *Compactor) forget(listed []gitlet.CompactionCandidate) {
	named := make(map[string]bool, len(listed))
	for _, cand := range listed {
		named[cand.ID] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.notDue {
		if !named[id] {
			delete(c.notDue, id)
		}
	}
	for id := range c.held[compactionLoop].repos {
		if !named[id] {
			delete(c.held[compactionLoop].repos, id)
		}
	}
}

// maintainedOf is which listed repositories this node maintains, off ONE
// listing of who serves git and what each Gitlet node advertises, asked only
// of those the pass must judge (each at the pack bound, whose alarm it raises
// whatever its plan, and each not remembered as not due), each at its
// [Compactor.PlacedBy] charge. An error is UNKNOWN for all, never "maintains
// none".
//
// ONE LISTING, NEVER ONE A REPOSITORY: only a maintainer remembers a
// repository as not due, so every other Gitlet node judges every listed one
// each pass, and a lookup per repository (the presence view read, every lease
// decoded, the nodes sorted) ran ten thousand times a minute to learn it
// maintained none of them.
func (c *Compactor) maintainedOf(listed []gitlet.CompactionCandidate) (map[string]bool, error) {
	ask := map[string]int64{}
	for _, cand := range listed {
		if cand.LivePacks >= gitlet.MaxPacksPerRepo || !c.notDueAt(cand) {
			ask[cand.ID] = c.PlacedBy(cand.LiveObjects)
		}
	}
	if len(ask) == 0 {
		return nil, nil
	}
	return c.o.Maintainers.MaintainerOf(ask)
}

// behindOf is every listed repository at the pack bound this node maintains.
// With maintainers unknown, one the last pass named stays: an alarm cleared by
// a placement this node could not read would go silent exactly while every
// push to the repository is refused.
func (c *Compactor) behindOf(listed []gitlet.CompactionCandidate, maintained map[string]bool,
	unknown error) []behindRepo {

	c.mu.Lock()
	last := make(map[string]bool, len(c.behind))
	for _, b := range c.behind {
		last[b.id] = true
	}
	c.mu.Unlock()
	var out []behindRepo
	for _, cand := range listed {
		if cand.LivePacks < gitlet.MaxPacksPerRepo {
			continue
		}
		if (unknown == nil && maintained[cand.ID]) || (unknown != nil && last[cand.ID]) {
			out = append(out, behindRepo{id: cand.ID, name: cand.Name, packs: cand.LivePacks})
		}
	}
	return out
}

// setBehind is the alarm reading's half from this pass, and the gauge.
func (c *Compactor) setBehind(behind []behindRepo) {
	c.mu.Lock()
	c.behind = behind
	c.mu.Unlock()
	if c.o.Metrics != nil {
		c.o.Metrics.Set(metrics.CodeCompactBehind, float64(len(behind)), nil)
	}
}

// notDueAt reports whether a plan of cand found it not due at the version the
// listing names and at its cap now — every input a plan reads ([plan]).
func (c *Compactor) notDueAt(cand gitlet.CompactionCandidate) bool {
	cap := c.capFor(cand.ID)
	c.mu.Lock()
	defer c.mu.Unlock()
	m, ok := c.notDue[cand.ID]
	return ok && m.version == cand.Version && m.cap == cap
}

// isHeld reports whether repoID is held back for a full store in loop l.
func (c *Compactor) isHeld(l loop, repoID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.held[l].repos[repoID]
	return ok
}

// releaseHeld reads once whether the object store now has room for any
// repository loop l holds back for objstore.ErrFull, and answers the ones this
// pass may build again (package doc, "The uploads, and a store with no room").
// The hold is never a memory at the version, which room coming back does not
// move. A store with a byte reservation releases holds smallest first while
// the room left holds their estimates. A provider's bucket says nothing of its
// room, so its smallest hold goes through once [collect.CollectInterval] (the
// collector frees such room, nothing else) has passed since the loop's last
// refusal, and its upload stored releases every hold ([Compactor.stored]): one
// build a loop an hour, never one a repository a poll. A failed read holds
// every repository back for the pass.
func (c *Compactor) releaseHeld(ctx context.Context, l loop) (map[string]bool, error) {
	c.mu.Lock()
	type hold struct {
		id       string
		estimate int64
	}
	var held []hold
	for id, estimate := range c.held[l].repos {
		held = append(held, hold{id, estimate})
	}
	fullAt := c.held[l].fullAt
	c.mu.Unlock()
	if len(held) == 0 {
		return nil, nil
	}
	slices.SortFunc(held, func(a, b hold) int {
		return cmp.Or(cmp.Compare(a.estimate, b.estimate), cmp.Compare(a.id, b.id))
	})
	usage, reserves, err := c.o.Objects.Usage(ctx)
	if err != nil {
		return nil, err
	}
	released := map[string]bool{}
	if !reserves {
		if c.now().Sub(fullAt) >= collect.CollectInterval {
			released[held[0].id] = true
		}
		return released, nil
	}
	room := usage.MaxBytes - usage.Bytes
	for _, h := range held {
		if h.estimate > room {
			break
		}
		released[h.id] = true
		room -= h.estimate
	}
	return released, nil
}

// holdFull holds repoID back in loop l for a full store: its build would
// store about estimate bytes.
func (c *Compactor) holdFull(l loop, repoID string, estimate int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held[l].repos[repoID] = estimate
	c.held[l].fullAt = c.now()
}

// stored is a build of loop l — a compaction's outputs, or a publication's
// layer and repack outputs — of repoID whose uploads the store took.
//
// ITS OWN HOLD GOES, WHATEVER THE STORE: these uploads answer the refusal it
// was held for. Kept, the first build's estimate would hold back every later,
// smaller build of a repository still listed until its pushes were refused
// compaction_behind. Another loop's hold stays: its estimate is another
// build's. On a provider's bucket a write taken is the only sign room came
// back, so every hold of every loop goes; a reservation's room is read every
// pass instead ([Compactor.releaseHeld]), since one build's uploads say nothing
// of a larger one's.
func (c *Compactor) stored(ctx context.Context, l loop, repoID string) {
	c.mu.Lock()
	delete(c.held[l].repos, repoID)
	holding := false
	for _, h := range c.held {
		holding = holding || len(h.repos) > 0
	}
	c.mu.Unlock()
	if !holding {
		return
	}
	if _, reserves, err := c.o.Objects.Usage(ctx); err != nil || reserves {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.held {
		clear(c.held[i].repos)
		c.held[i].fullAt = time.Time{}
	}
}
