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

// Poll is one pass:
//
//  1. LIST every repository holding code.CompactMinMerge live packs or more —
//     the `compaction_due` read, every page, MOST LIVE PACKS FIRST, read at
//     STALE: the router carries this node's own compactions as a floor on
//     every code read it sends (estate.CodeSeam.Compact raises it), so the
//     listing sees what this node landed, and a listing a moment behind the
//     head costs at most a repository planned a poll late — a compaction
//     syncs at its repository's own version and its committer holds every
//     input to the rows.
//  2. BEHIND: every listed repository at code.MaxPacksPerRepo that this node
//     is home for is what the alarm reading names — asked before the memory
//     and whatever its compaction decides, because at the bound a refused push
//     records nothing, so the repository's version stands still and a reading
//     taken only of what reached a compaction would clear while every push is
//     refused.
//  3. THE STORE'S ROOM, read once a pass, and only while a repository is held
//     back for a full store ([Compactor.releaseHeld]).
//  4. Every listed repository in the listing's order, until the next tick
//     would begin: skipped when a plan of it was found not due at its version
//     and cap ([memo]), when it is held back for a full store, or when this
//     node is not its home; and COMPACTED otherwise, one at a time. A
//     placement this node cannot read skips every remaining repository — a
//     compactor that cannot tell compacts nothing rather than guess — logged
//     once a pass.
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
	behind := c.behindOf(listed)
	c.setBehind(behind)
	for _, b := range behind {
		report.Behind = append(report.Behind, b.name)
	}
	released, err := c.releaseHeld(ctx)
	if err != nil {
		c.log.Warn("code_compaction_store_unread", "error", err,
			"detail", "the object store's room could not be read, so every repository held back for a full "+
				"store waits for the next pass")
	}
	until := began.Add(Poll)
	for _, cand := range listed {
		if ctx.Err() != nil || !c.now().Before(until) {
			break
		}
		if c.notDueAt(cand) || (c.isHeld(cand.ID) && !released[cand.ID]) {
			continue
		}
		home, err := c.o.Homes.IsHome(cand.ID)
		if err != nil {
			c.log.Warn("code_compaction_home_unknown", "repo", cand.Name, "error", err,
				"detail", "who serves git could not be read, so this pass compacts nothing more")
			break
		}
		if !home {
			continue
		}
		report.Home++
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
	log("code_compaction_pass", "listed", report.Listed, "home", report.Home, "due", report.Due,
		"compacted", report.Compacted, "refused", report.Refused, "failed", report.Failed,
		"behind", len(report.Behind), "took", c.now().Sub(began))
	return report, nil
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

// forget drops the memories of a repository the listing no longer names — a
// not-due plan, a hold for a full store — so each is bounded by the
// repositories the company holds at CompactMinMerge packs or more: one that
// fell below it, or was deleted, is planned from its rows if it comes back. A
// halved CAP is kept for the compactor's life (M3-decisions M3-O29): it is a
// fact about what the repository's packs make of a build, which a repository
// compacted below the listing's floor does not change, and forgotten it would
// cost the repository a failed build each time it grew back.
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
	for id := range c.held {
		if !named[id] {
			delete(c.held, id)
		}
	}
}

// behindOf is every listed repository at the pack bound this node is home
// for. One whose home cannot be read now keeps its place in the reading where
// the last pass named it — unknown is not "no longer behind", and an alarm
// cleared by a placement this node could not read would go silent exactly
// while every push to the repository is refused.
func (c *Compactor) behindOf(listed []code.CompactionCandidate) []behindRepo {
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
		home, err := c.o.Homes.IsHome(cand.ID)
		if (err == nil && home) || (err != nil && last[cand.ID]) {
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

// isHeld reports whether repoID is held back for a full store.
func (c *Compactor) isHeld(repoID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.held[repoID]
	return ok
}

// releaseHeld reads, once, whether the object store now has room for any
// repository held back since it refused an output for room (objstore.ErrFull),
// and answers the ones this pass may compact again.
//
// NEVER RETRIED AS A BLIP, which is the full store's contract: a compaction
// refused for room builds nothing and uploads nothing again until the store
// shows the room. Room comes back when an operator makes it or the collector
// deletes what nothing names — neither of which moves a repository's version,
// so the hold is NEVER a memory at the version, which would keep an idle
// repository from ever compacting again. On a store that holds a byte
// reservation (the fleet's own bucket on the broker), a repository is
// released once the reservation's room left holds what its compaction would
// store, the smallest first, each counted against the room the next is held
// to. A provider's bucket reserves nothing and can say nothing of its room,
// so the only way to learn of it is a write: one held repository — the
// smallest — is let through once [collect.CollectInterval] has passed since
// the last refusal, the soonest the collector, the one thing in the engine
// that frees room in such a bucket, can have made any; its outputs stored,
// every hold is released ([Compactor.stored]). So a full provider's bucket
// costs a node one build an hour, never one a repository a poll. A failed read
// of the room holds every repository back for the pass. On either store, a
// repository's own hold goes once its own outputs are stored: the refusal it
// was held back for is answered, and a later refusal sets an estimate of its
// own.
func (c *Compactor) releaseHeld(ctx context.Context) (map[string]bool, error) {
	c.mu.Lock()
	type hold struct {
		id       string
		estimate int64
	}
	var holds []hold
	for id, estimate := range c.held {
		holds = append(holds, hold{id, estimate})
	}
	fullAt := c.fullAt
	c.mu.Unlock()
	if len(holds) == 0 {
		return nil, nil
	}
	slices.SortFunc(holds, func(a, b hold) int {
		return cmp.Or(cmp.Compare(a.estimate, b.estimate),
			cmp.Compare(a.id, b.id))
	})
	usage, reserves, err := c.o.Objects.Usage(ctx)
	if err != nil {
		return nil, err
	}
	released := map[string]bool{}
	if !reserves {
		if c.now().Sub(fullAt) >= collect.CollectInterval {
			released[holds[0].id] = true
		}
		return released, nil
	}
	room := usage.MaxBytes - usage.Bytes
	for _, h := range holds {
		if h.estimate > room {
			break
		}
		released[h.id] = true
		room -= h.estimate
	}
	return released, nil
}

// holdFull holds repoID back for a full store: its compaction would store
// about estimate bytes.
func (c *Compactor) holdFull(repoID string, estimate int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held[repoID] = estimate
	c.fullAt = c.now()
}

// stored is a compaction of repoID whose outputs the store took.
//
// ITS OWN HOLD GOES, WHATEVER THE STORE: the refusal it was held back for is
// answered by these very outputs, stored, and a refusal of a later roll of it
// sets an estimate of its own. Kept, the estimate of the roll a full store
// first refused went on holding the repository back for as long as the
// listing named it — on a store that holds a reservation nothing else drops a
// hold of a repository still listed, and one that keeps eight full packs, or
// that a capped roll leaves listed, is listed for good — so once the room fell
// back below that first roll's size, as the outputs just stored and a day of
// retired inputs make usual, every later roll of it, however small, was
// skipped at every pass, until its pushes were refused compaction_behind; and
// the stale estimate was charged against the room every other held repository
// is released by.
//
// THE REST, by what the store says of its room. On a provider's bucket, which
// says nothing of it, a write it takes is the one way this node learns the
// room came back, so every repository held back for it is released. On a store
// that holds a reservation the room is read every pass instead
// ([Compactor.releaseHeld]) — one roll's outputs taken say nothing of a larger
// one's — and no other hold is released here.
func (c *Compactor) stored(ctx context.Context, repoID string) {
	c.mu.Lock()
	delete(c.held, repoID)
	holding := len(c.held) > 0
	c.mu.Unlock()
	if !holding {
		return
	}
	if _, reserves, err := c.o.Objects.Usage(ctx); err != nil || reserves {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.held)
	c.fullAt = time.Time{}
}
