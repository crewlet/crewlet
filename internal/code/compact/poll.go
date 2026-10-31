package compact

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Poll is one pass of the compaction loop:
//
//  1. LIST every repository holding code.CompactMinMerge live packs or more —
//     the `compaction_due` read, every page, MOST LIVE PACKS FIRST, read at
//     STALE: the router carries this node's own compactions as a floor on
//     every code read it sends (estate.CodeSeam.Compact raises it), so the
//     listing sees what this node landed, and a listing a moment behind the
//     head costs at most a repository planned a poll late — a compaction
//     syncs at its repository's own version and its committer holds every
//     input to the rows.
//  2. MAINTAINERS, judged against ONE LISTING of who serves git and what each
//     git node advertises ([Compactor.maintainedOf]) for every listed
//     repository the pass must judge — each it may plan, and each at the
//     bound — so a repository this node does not maintain costs the pass its
//     share of one listing, and one its maintainer remembers as not due a
//     lookup.
//  3. BEHIND: every listed repository at code.MaxPacksPerRepo that this node
//     maintains is what the alarm reading names — judged before the memory and
//     whatever its compaction decides, because at the bound a refused push
//     records nothing, so the repository's version stands still and a reading
//     taken only of what reached a compaction would clear while every push is
//     refused.
//  4. THE STORE'S ROOM, read once a pass, and only while a repository is held
//     back for a full store and the pass may compact ([Compactor.releaseHeld]).
//  5. Every listed repository in the listing's order, until the next tick
//     would begin: skipped when a plan of it was found not due at its version
//     and cap ([memo]), when it is held back for a full store, or when this
//     node is not its MAINTAINER for its maintenance's charge
//     ([Compactor.PlacedBy]) — the charge every loop is placed by, so one node writes all of a repository's
//     packs; and COMPACTED otherwise, one at a time. A placement this node
//     cannot read compacts nothing that pass — a compactor that cannot tell
//     does none of a maintainer's work rather than guess — logged once a pass.
//
// A listing that fails is the pass's error, and the alarm reading is left as
// the last pass found it. The pass is logged code_compaction_pass, at DEBUG
// unless it compacted, refused or failed something.
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

// PlacedBy is the charge a repository of liveObjects live objects is asked
// who maintains it by: the largest any of its maintainer's steps may be
// admitted at under the current epoch's limits
// (cache.Cache.MaintenanceCharge) — one question for every loop, so its
// compactions, its publications and its expunges, the three writers of its
// packs, are placed on one node, and on one whose own admissions hold every
// one of them.
func (c *Compactor) PlacedBy(liveObjects int64) int64 {
	return c.o.Cache.MaintenanceCharge(liveObjects, c.o.Limits())
}

// listDue is every repository the `compaction_due` listing names, every page.
// A listing pages by the code host's page bounds, so a company of the most
// repositories it may declare is one or two reads.
func (c *Compactor) listDue(ctx context.Context) ([]code.CompactionCandidate, error) {
	q := code.ReadQuery{Kind: code.ReadCompactionDue, MinPacks: c.bounds.minMerge,
		Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	var out []code.CompactionCandidate
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

// forget drops the compaction loop's memories of a repository the listing no
// longer names — a not-due plan, a hold for a full store — so each is bounded
// by the repositories the company holds at CompactMinMerge packs or more: one
// that fell below it, or was deleted, is planned from its rows if it comes
// back. A halved CAP is kept for the compactor's life (M3-decisions M3-O29):
// it is a fact about what the repository's packs make of a build, which a
// repository compacted below the listing's floor does not change, and
// forgotten it would cost the repository a failed build each time it grew
// back.
func (c *Compactor) forget(listed []code.CompactionCandidate) {
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
// LISTING of who serves git and what each git node advertises: asked of every
// repository the pass must judge — each at the pack bound, whose alarm it
// raises whatever its plan, and each it may plan — each by the charge it is
// placed by ([Compactor.PlacedBy]), and of no other, so a repository its
// maintainer remembers as not due costs the pass a lookup and nothing more. An
// error is UNKNOWN for every one of them, never "maintains none".
//
// ONE LISTING, NEVER ONE A REPOSITORY: only a repository's maintainer ever
// plans it, so only its maintainer remembers it as not due, and on every other
// git node every listed repository is judged at every pass — a pass asking who
// maintains a repository once a repository read the presence view, decoded
// every node's lease and sorted the git nodes ten thousand times a minute on a
// node maintaining an eighth of ten thousand repositories, to learn it
// maintained none of the rest.
func (c *Compactor) maintainedOf(listed []code.CompactionCandidate) (map[string]bool, error) {
	ask := map[string]int64{}
	for _, cand := range listed {
		if cand.LivePacks >= code.MaxPacksPerRepo || !c.notDueAt(cand) {
			ask[cand.ID] = c.PlacedBy(cand.LiveObjects)
		}
	}
	if len(ask) == 0 {
		return nil, nil
	}
	return c.o.Maintainers.MaintainerOf(ask)
}

// behindOf is every listed repository at the pack bound this node maintains,
// as maintained judged them. With who maintains them unknown, one keeps its
// place in the reading where the last pass named it — unknown is not "no
// longer behind", and an alarm cleared by a placement this node could not read
// would go silent exactly while every push to the repository is refused.
func (c *Compactor) behindOf(listed []code.CompactionCandidate, maintained map[string]bool,
	unknown error) []behindRepo {

	c.mu.Lock()
	last := make(map[string]bool, len(c.behind))
	for _, b := range c.behind {
		last[b.id] = true
	}
	c.mu.Unlock()
	var out []behindRepo
	for _, cand := range listed {
		if cand.LivePacks < code.MaxPacksPerRepo {
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
func (c *Compactor) notDueAt(cand code.CompactionCandidate) bool {
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

// releaseHeld reads, once, whether the object store now has room for any
// repository loop l holds back since the store refused an upload of it for
// room (objstore.ErrFull), and answers the ones this pass may build again.
//
// NEVER RETRIED AS A BLIP, which is the full store's contract: a build refused
// for room builds nothing and uploads nothing again until the store shows the
// room. Room comes back when an operator makes it or the collector deletes
// what nothing names — neither of which moves a repository's version, so the
// hold is NEVER a memory at the version, which would keep an idle repository
// from ever compacting, or publishing, again. On a store that holds a byte
// reservation (the fleet's own bucket on the broker), a repository is released
// once the reservation's room left holds what its build would store, the
// smallest first, each counted against the room the next is held to. A
// provider's bucket reserves nothing and can say nothing of its room, so the
// only way to learn of it is a write: one held repository — the smallest — is
// let through once [collect.CollectInterval] has passed since the loop's last
// refusal, the soonest the collector, the one thing in the engine that frees
// room in such a bucket, can have made any; its upload stored, every hold of
// every loop is released ([Compactor.stored]). So a full provider's bucket
// costs a node one build a loop an hour, never one a repository a poll. A
// failed read of the room holds every repository back for the pass. On either
// store, a repository's own hold in a loop goes once that loop's own upload of
// it is stored: the refusal it was held back for is answered, and a later
// refusal sets an estimate of its own.
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
// ITS OWN HOLD GOES, WHATEVER THE STORE: the refusal loop l held repoID back
// for is answered by these very uploads, stored, and a refusal of a later
// build of it sets an estimate of its own. Kept, the estimate of the build a
// full store first refused went on holding the repository back for as long as
// the loop's listing named it — on a store that holds a reservation nothing
// else drops a hold of a repository still listed, and one that keeps
// code.CompactMinMerge full packs, or that a capped roll leaves listed, is in
// the compaction's listing for good — so once the room fell back below that
// first build's size, as the uploads just stored and a day of retired inputs
// make usual, every later build of it, however small, was skipped at every
// pass, until its pushes were refused compaction_behind; and the stale
// estimate was charged against the room every other held repository is
// released by. Another loop's hold of it stays: its estimate is another
// build's.
//
// THE REST, by what the store says of its room. On a provider's bucket, which
// says nothing of it, a write it takes is the one way this node learns the
// room came back, so every repository any loop holds back for it is
// released. On a store that holds a reservation the room is read every pass
// instead ([Compactor.releaseHeld]) — one build's uploads taken say nothing of
// a larger one's — and no other hold is released here.
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
