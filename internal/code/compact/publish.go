package compact

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/textcut"
)

// PublishPoll is the interval between two passes of the PUBLICATION loop: one
// indexed listing a git node a minute, as [Poll] is the compaction loop's.
const PublishPoll = time.Minute

// The publication's cadence — when a repository's tail is worth a layer
// ([Due]). Each states its reason, and the ones marked PROVISIONAL are
// re-derived by BenchmarkMidx's arms in internal/code/cache (M3-decisions
// M3-O39): the old value and the new one in the commit that measures them.
const (
	// PublishPacks is a tail worth a publication by its PACKS: a pack outside
	// the chain costs every request that reaches it a lookup of its own
	// index, 0.06–0.09 ms a pack measured at 100 MB–1 GB — the own-mode
	// index's reason for cache.MidxPacks, moved to the layer that replaces
	// it. PROVISIONAL: BenchmarkMidx's stale arms on the pin.
	PublishPacks = 64
	// PublishShare is a tail worth a publication by its SHARE of the
	// repository's objects: the tail's objects are walked unbitmapped by every
	// clone and fetch that reaches them — 0.09 s at a sixteenth of a 219 679
	// object history outside the chain, against 0.004 s inside it and 1.1 s
	// with no index (git 2.56.0) — so a SMALL repository is published once
	// its tail holds a sixteenth of its objects. A share alone does not bound
	// a large one's tail: PublishTailObjects does. PROVISIONAL: BenchmarkMidx's
	// share= arm.
	PublishShare = 16
	// PublishTailObjects is a tail worth a publication by its OBJECTS, the
	// ABSOLUTE bound: a tail's cost is its objects, never its share — every
	// clone, the installer's canary and the node's tip commit-graph walk every
	// object outside the chain, at about 6.5 µs an object on git 2.56.0 and at
	// most gitproc.WalkPerObject, plus gitproc.TransferBytesPerObject of a
	// clone's memory each — so this many objects cost a clone at most a
	// second of walk and some 21 MiB. A sixteenth of the largest repository
	// the defaults admit, some 60 million objects, is 3.75 million: 24–41 s of
	// walk added to every clone of it, while compaction keeps such a tail to a
	// few dozen packs and PublishPacks never fires there. A repository is due
	// at whichever of the share and this bound its tail reaches first.
	// PROVISIONAL: BenchmarkMidx's tail= arm re-derives it where the walk is
	// not under gitproc.WalkPerObject.
	PublishTailObjects = int64(time.Second / gitproc.WalkPerObject)
	// PublishDebounce is how long a burst of pushes stands still before its
	// tail is published once, rather than a layer for each of its pushes —
	// cache.MidxDebounce's reason. KEPT.
	PublishDebounce = 10 * time.Second
	// PublishMaxWait is how long a repository pushed to without a pause waits
	// for its publication at the most, so it is published as often as one
	// that pauses — cache.MidxMaxWait's reason, and its anchor: counted from
	// when the TAIL began waiting, never from the last publication alone,
	// which PublishMinInterval already paces. KEPT.
	PublishMaxWait = 10 * time.Minute
	// PublishMinInterval is the least time between two publications of one
	// repository, and between two REPAIRS — cache.MidxMinInterval's reason: a
	// layer's build walks the whole history. PROVISIONAL: max(10 minutes, 10
	// × BenchmarkMidx's step=publish at 5 GB).
	PublishMinInterval = 10 * time.Minute
	// PublishDutyCycle holds a repository's publications to at most a tenth
	// of the time of the node that runs them: the next waits this many times
	// the last one's build, where that is longer than PublishMinInterval —
	// cache.MidxDutyCycle's reason. KEPT.
	PublishDutyCycle = 10
)

// Due reports whether c's tail should be published at now, from its row and
// tailSince — when this node's publication loop first listed the tail —
// alone, before anything is synced:
//
//   - the tail holds a pack: the live packs are past the published ones;
//   - it is WORTH a publication: the repository was never published, or its
//     tail holds PublishPacks packs, or a PublishShare-th of the repository's
//     objects or PublishTailObjects objects, whichever is fewer;
//   - it has SETTLED: it stood still PublishDebounce since its last push, or
//     the tail has waited PublishMaxWait — since the later of tailSince and
//     the last publication, which every pack of a tail it left behind came
//     after;
//   - the DUTY CYCLE allows it: PublishMinInterval, or PublishDutyCycle times
//     the last publication's build, whichever is longer, since the last
//     publication.
//
// THE CEILING RUNS FROM THE TAIL'S START, NEVER FROM THE LAST PUBLICATION
// ALONE: PublishMaxWait is PublishMinInterval, so a ceiling counted from the
// last publication would have passed whenever the duty cycle allowed one, and
// the debounce would hold nothing back — a burst of pushes into a repository
// idle since its last publication would be published at its first pack, its
// other packs left in the tail for a whole duty cycle, as long as a
// publication's build allows (hours for the largest). The row holds no clock
// of the tail's own, and the listing a pass reads names a repository only
// while it holds a tail, so a pass's first sighting of it is the tail's start
// to within one PublishPoll — a memory of this node's, which a restart or a
// new maintainer starts again, costing at most one ceiling more.
//
// The tail's counts are the last publication's recount against the live ones
// (code.PublicationCandidate), so a tail may hold a pack pushed again
// byte-identical to a published one and nothing to publish, which only the
// plan tells.
func Due(c code.PublicationCandidate, tailSince, now time.Time) bool {
	tailPacks, tailObjects := c.LivePacks-c.PublishedPacks, c.LiveObjects-c.PublishedObjects
	never := c.PublishedAt.IsZero()
	waiting := tailSince
	if c.PublishedAt.After(waiting) {
		waiting = c.PublishedAt
	}
	switch {
	case tailPacks <= 0:
		return false
	case !never && tailPacks < PublishPacks && tailObjects < min(c.LiveObjects/PublishShare, PublishTailObjects):
		return false
	case now.Sub(c.LastPushAt) < PublishDebounce && now.Sub(waiting) < PublishMaxWait:
		return false
	case !never && now.Sub(c.PublishedAt) < max(PublishMinInterval, PublishDutyCycle*c.PublishTook):
		return false
	}
	return true
}

// PublishReport is what one pass of the publication loop did: how many
// repositories its listing named and how many this node's cache holds a
// refused layer of, how many of them this node maintains, how many a
// publication was planned for, and how many of those were published, were
// refused or failed.
type PublishReport struct {
	Listed, Damaged, Maintained, Due, Published, Refused, Failed int
}

// PublishPass is one pass of the PUBLICATION loop:
//
//  1. LIST every repository holding a pack no published layer covers — the
//     `publication_due` read, every page, LEAST RECENTLY PUBLISHED FIRST, at
//     STALE for the compaction listing's reason (the router carries this
//     node's own writes as a floor on every code read it sends) — and every
//     repository whose copy on this node holds a layer this node refused
//     (cache.Cache.Damaged), which only its maintainer's REPAIR replaces: an
//     idle repository whose layer the store lost is in no due listing.
//  2. MAINTAINERS, judged against ONE LISTING of who serves git and what each
//     git node advertises ([Compactor.maintainedOfPublications]), for every
//     repository the pass may publish, by the publication's charge
//     (cache.Cache.PublishCharge), as the compaction loop judges its own: a
//     placement this node cannot read publishes nothing that pass, logged
//     once.
//  3. THE STORE'S ROOM, read once a pass, and only while a repository is held
//     back for a full store and the pass may publish
//     ([Compactor.releaseHeld]).
//  4. Every repository in that order, until the next tick would begin: one a
//     plan found nothing to publish in at its version is skipped unless it is
//     damaged; one held back for a full store is skipped until the room is
//     there; a REPAIR is due at once, paced by PublishMinInterval since its
//     last publication and since this node last built one of it; any other
//     is held to [Due] — each judged by its row, and by when this node first
//     listed its tail, before anything is synced; one this node does not
//     maintain is skipped; and every other is PUBLISHED ([Compactor.Publish]),
//     one at a time.
//
// A listing that fails is the pass's error. The pass is logged
// code_publication_pass, at DEBUG unless it published, refused or failed
// something.
func (c *Compactor) PublishPass(ctx context.Context) (PublishReport, error) {
	began := c.now()
	var report PublishReport
	listed, err := c.listPublications(ctx)
	if err != nil {
		return report, err
	}
	report.Listed = len(listed)
	tails := c.sightTails(listed, began)
	damaged := map[string]bool{}
	named := map[string]bool{}
	for _, cand := range listed {
		named[cand.ID] = true
	}
	for _, ref := range c.o.Cache.Damaged() {
		damaged[ref.ID] = true
		if !named[ref.ID] {
			listed = append(listed, code.PublicationCandidate{ID: ref.ID, Name: ref.Name, LivePacks: ref.LivePacks,
				LiveObjects: ref.LiveObjects, Version: ref.Position})
		}
	}
	report.Damaged = len(damaged)
	c.forgetPublications(listed)
	maintained, unknown := c.maintainedOfPublications(listed, damaged)
	var released map[string]bool
	if unknown != nil {
		c.log.Warn("code_publication_maintainer_unknown", "listed", len(listed), "error", unknown,
			"detail", "who maintains the listed repositories could not be read, so this pass publishes nothing")
	} else if released, err = c.releaseHeld(ctx, publicationLoop); err != nil {
		c.log.Warn("code_publication_store_unread", "error", err,
			"detail", "the object store's room could not be read, so every repository held back for a full "+
				"store waits for the next pass")
	}
	until := began.Add(PublishPoll)
	for _, cand := range listed {
		now := c.now()
		if unknown != nil || ctx.Err() != nil || !now.Before(until) {
			break
		}
		repair := damaged[cand.ID]
		switch {
		case !repair && c.nothingAt(cand),
			c.isHeld(publicationLoop, cand.ID) && !released[cand.ID],
			repair && !c.repairDue(cand, now),
			!repair && !Due(cand, tails[cand.ID], now),
			!maintained[cand.ID]:
			continue
		}
		report.Maintained++
		out, _ := c.Publish(ctx, cand)
		switch out {
		case OutcomeNotDue, OutcomeWaiting:
		case OutcomeApplied, OutcomePending, OutcomeUnknown:
			report.Due++
			report.Published++
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
	if report.Published+report.Refused+report.Failed > 0 {
		log = c.log.Info
	}
	log("code_publication_pass", "listed", report.Listed, "damaged", report.Damaged,
		"maintained", report.Maintained, "due", report.Due, "published", report.Published,
		"refused", report.Refused, "failed", report.Failed, "took", c.now().Sub(began))
	return report, nil
}

// maintainedOfPublications is which repositories of the pass's listing this
// node maintains, off ONE LISTING of who serves git and what each git node
// advertises, as the compaction loop judges its own ([Compactor.maintainedOf]):
// asked of every repository the pass may publish — each but one a plan found
// nothing to publish in at its version, which is no repair — each by the
// charge it is placed by ([Compactor.placedBy]). An error is UNKNOWN for every
// one of them, never "maintains none". The rules that turn on the clock — a
// repair's pace, [Due] — are left to the pass, which reads the clock at each
// repository, so a repository due part way through it is one it judged.
func (c *Compactor) maintainedOfPublications(listed []code.PublicationCandidate,
	damaged map[string]bool) (map[string]bool, error) {

	ask := map[string]int64{}
	for _, cand := range listed {
		if damaged[cand.ID] || !c.nothingAt(cand) {
			ask[cand.ID] = c.placedBy(cand.LiveObjects)
		}
	}
	if len(ask) == 0 {
		return nil, nil
	}
	return c.o.Maintainers.MaintainerOf(ask)
}

// listPublications is every repository the `publication_due` listing names,
// every page.
func (c *Compactor) listPublications(ctx context.Context) ([]code.PublicationCandidate, error) {
	q := code.ReadQuery{Kind: code.ReadPublicationDue, Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	var out []code.PublicationCandidate
	for {
		a, err := c.o.Estate.Read(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("compact: list the repositories a publication may be due in: %w", err)
		}
		out = append(out, a.Publish...)
		if a.Next == "" {
			return out, nil
		}
		q.Cursor = a.Next
	}
}

// forgetPublications drops the publication loop's memories of a repository the
// pass no longer names — a plan that found nothing to publish, a hold for a
// full store, the last build of one — so each is bounded by the repositories
// with a tail or a refused layer: one whose tail a publication covered, or
// that was deleted, is planned from its rows if it comes back.
func (c *Compactor) forgetPublications(named []code.PublicationCandidate) {
	in := make(map[string]bool, len(named))
	for _, cand := range named {
		in[cand.ID] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.nothing {
		if !in[id] {
			delete(c.nothing, id)
		}
	}
	for id := range c.held[publicationLoop].repos {
		if !in[id] {
			delete(c.held[publicationLoop].repos, id)
		}
	}
	for id := range c.built {
		if !in[id] {
			delete(c.built, id)
		}
	}
}

// sightTails records at, the pass's start, as the first sighting of the tail
// of every repository the due listing names that this node had not sighted
// yet, forgets every sighting of one it no longer names — its tail published,
// or itself deleted — so that its next tail is sighted afresh, and answers
// every sighting: what [Due]'s ceiling runs from. A repository listed without
// a pause, its tail never emptying, keeps its sighting, which its last
// publication then bounds from below. The due listing alone is sighted, never
// a repository listed for a refused layer alone, which holds no tail.
func (c *Compactor) sightTails(listed []code.PublicationCandidate, at time.Time) map[string]time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]time.Time, len(listed))
	for _, cand := range listed {
		since, ok := c.tails[cand.ID]
		if !ok {
			since = at
		}
		out[cand.ID] = since
	}
	c.tails = out
	return maps.Clone(out)
}

// nothingAt reports whether a plan of cand found nothing to publish at the
// version the listing names — every input such a plan reads but the copy's
// installation, which only a REFUSED layer changes the answer for, and a
// refused layer makes the repository a repair the memory never skips.
func (c *Compactor) nothingAt(cand code.PublicationCandidate) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.nothing[cand.ID]
	return ok && v == cand.Version
}

// rememberNothing is a plan of cand that found nothing to publish at its
// version, or a layer too large at it: skipped until the version moves.
func (c *Compactor) rememberNothing(cand code.PublicationCandidate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nothing[cand.ID] = cand.Version
}

// repairDue reports whether a REPAIR of cand may be built at now: at most one
// a PublishMinInterval, since the repository's last publication and since
// this node last built one of it — so a store that keeps losing what is
// published costs a repository one publication a PublishMinInterval, never
// one a pass.
func (c *Compactor) repairDue(cand code.PublicationCandidate, now time.Time) bool {
	c.mu.Lock()
	last := c.built[cand.ID]
	c.mu.Unlock()
	if !cand.PublishedAt.IsZero() && now.Sub(cand.PublishedAt) < PublishMinInterval {
		return false
	}
	return last.IsZero() || now.Sub(last) >= PublishMinInterval
}

// errLayerTooLarge and errNoBitmap are publications this node refuses before
// any byte of them is stored: a layer past what a layer's parts hold, and a
// build git wrote no bitmap of, absorbing one more layer, or over the chain's
// root.
var (
	errLayerTooLarge = errors.New("compact: the layer would weigh more than a layer's parts hold")
	errNoBitmap      = errors.New("compact: git wrote the layer without a bitmap")
)

// Publish plans and runs ONE publication of cand — a repository this node
// maintains, as the `publication_due` listing named it or its cache found it
// damaged — and answers what became of it: the unit a suite drives, and what
// every pass runs one at a time. Nothing is retried inside it, as in
// [Compactor.Compact]; its error is the one that failed or refused it.
//
// Throughout, cand.ID is the REPOSITORY and the publication's own id — the
// compaction record's, its scratch's and every repack output's derivation —
// is minted at its build.
//
//  1. SYNC at session, floored at the repository's version the listing read,
//     for the compaction's reason: the committer holds the chain and every
//     pack to its own snapshot (decide rules 6a–6g).
//  2. PLAN ([planLayer]) over the copy's chain (cache.Cache.Chain) and its
//     live packs: nothing to publish is remembered at the listing's version,
//     a layer still at the installer answers `waiting` and is planned again
//     at the next pass, and a layer too large is refused and remembered. A
//     layer to build RESERVES every pack it covers ([Compactor.reserve]):
//     from then on a compaction of the repository runs beside it and merges
//     only the packs pushed since, which the layer neither names nor needs —
//     so a publication of minutes never holds back the merges that keep its
//     own repository under the pack bound. Until then, its sync and its
//     plan, a compaction of the repository answers busy; and a publication
//     asked while a compaction of its repository is running answers busy
//     itself, since its plan would cover that merge's inputs
//     ([Compactor.claimPublication]).
//  3. ADMIT the build to the maintenance lane (cache.Cache.AdmitPublication),
//     charged at the largest of its steps': busy is the next poll's, and a
//     charge the node's memory cannot hold is refused `memory` and NEVER
//     remembered — the budget is read again at every pass, and the next pass
//     asks whether this node is still the maintainer, which is how a larger
//     node takes the repository over (M3-decisions M3-O26).
//  4. BUILD in a scratch of its own whose chain files name EXACTLY the layers
//     it is written over (cache.Cache.NewLayerScratch): the repack where the
//     plan has one — built over a store of exactly its inputs, as a
//     compaction's is, and gathered into that scratch ([Compactor.repack]) —
//     held to a compaction's count and bundle rules; the
//     multi-pack-index layer with its bitmap over every pack the layer
//     covers; and its commit-graph layer with every changed-path filter. Then
//     the layer is laid out (code.LayLayer) and the whole proposal held to
//     every rule its committer will, before any byte is stored. THE ADMISSION
//     IS RELEASED HERE, once the build's last git step has ended — the
//     uploads, the proposal and the adoption run no git, and holding a
//     publication's charge through an upload the store paces would idle the
//     node's maintenance for every other repository (M3-decisions M3-O29,
//     M3-O40). The scratch, and its pin of the incarnation, stay to the end.
//  5. UPLOAD every part of the layer and every repack output at once
//     ([uploadAll]); a store with no room refuses the publication `store_full`
//     and holds the repository back until it has the room.
//  6. PROPOSE one compaction carrying the layer, and answer what the
//     committer decided: applied adopts the outputs and the layer into this
//     node's cache by link — so the node that built them downloads none —
//     and forgets the repository's memory.
func (c *Compactor) Publish(ctx context.Context, cand code.PublicationCandidate) (Outcome, error) {
	c.publishing.Lock()
	defer c.publishing.Unlock()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer context.AfterFunc(c.life, func() { cancel(context.Cause(c.life)) })()
	began := c.now()
	if !c.claimPublication(cand.ID) {
		c.log.Debug("code_publication_busy", "repo", cand.Name,
			"detail", "a compaction of the repository is running on this node")
		c.countedPublication(OutcomeBusy, began)
		return OutcomeBusy, errClaimed
	}
	defer c.unclaimPublication(cand.ID)
	out, err := c.publish(ctx, cand, began)
	c.countedPublication(out, began)
	return out, err
}

// publish is [Compactor.Publish]'s body.
func (c *Compactor) publish(ctx context.Context, cand code.PublicationCandidate, began time.Time) (Outcome,
	error) {

	s, err := c.o.Cache.Sync(ctx, cand.ID, statelog.FloorAt(statelog.ReadSession, cand.Version))
	if err != nil {
		return c.publicationFailed(cand, "sync", err)
	}
	defer s.Release()

	chain, live := c.o.Cache.Chain(s), c.o.Cache.Packs(s)
	cap := c.capFor(cand.ID)
	p, verdict := planLayer(chain, live, cap, c.bounds, 0)
	if verdict == layerDue && p.base > 0 {
		// NEVER BUILT OVER A BASE THE STORE LOST: this node installed its
		// own layers from the files it wrote, so its copy holds a layer the
		// store may no longer hold — and a layer written over it would be one
		// no node that fetches its chain can install either.
		held, err := c.heldBase(ctx, chain.Rows, p.base)
		if err != nil {
			return c.publicationFailed(cand, "store", err)
		}
		if held < p.base {
			c.log.Warn("code_publication_base_lost", "repo", cand.Name, "layer", chain.Rows[held].Layer.LayerID,
				"detail", "the object store no longer holds every part of a layer this node holds; it is "+
					"replaced, with every layer above it")
			chain = lostFrom(chain, held)
			p, verdict = planLayer(chain, live, cap, c.bounds, 0)
		}
	}
	switch verdict {
	case layerWaiting:
		c.log.Debug("code_publication_waiting", "repo", cand.Name,
			"detail", "a layer of the chain is still at this node's installer; the next pass plans again")
		return OutcomeWaiting, nil
	case layerNothing:
		c.rememberNothing(cand)
		return OutcomeNotDue, nil
	case layerTooLarge:
		c.rememberNothing(cand)
		c.publishRefusal(reasonLayerTooLarge)
		c.log.Warn("code_publication_refused", "repo", cand.Name, "refusal", reasonLayerTooLarge,
			"objects", p.objects, "estimate", layerEstimate(p.objects),
			"most", code.MaxLayerParts*code.LayerPartBytes)
		return OutcomeRefused, errLayerTooLarge
	}
	c.reserve(cand.ID, p)
	limits := c.o.Limits()
	adm, err := c.o.Cache.AdmitPublication(s, p.objects, p.repack.objects(), limits)
	switch {
	case errors.Is(err, gitproc.ErrBusy):
		c.log.Debug("code_publication_busy", "repo", cand.Name, "error", err)
		return OutcomeBusy, err
	case errors.Is(err, cache.ErrNoMemory):
		// NEVER REMEMBERED (M3-decisions M3-O26): the node's budget is read
		// again at every pass, and the next pass asks whether this node is
		// still the maintainer — how a larger git node takes the repository
		// over. The cache logged it once for the incarnation.
		c.publishRefusal(reasonMemory)
		return OutcomeRefused, err
	case err != nil:
		return c.publicationFailed(cand, "admit", err)
	}
	defer adm.Release()
	c.mu.Lock()
	c.built[cand.ID] = c.now()
	c.mu.Unlock()

	b, out, err := c.buildLayer(ctx, cand, s, adm, chain, live, p, cap, limits)
	if err != nil {
		return out, err
	}
	defer b.sc.Remove()
	adm.Release()

	if out, err := c.uploadLayer(ctx, cand, &b); err != nil {
		return out, err
	}
	if err := c.fault(ctx, codefault.PublishAfterUpload, 0); err != nil {
		return c.publicationFailed(cand, "propose", err)
	}
	res, err := c.o.Estate.Compact(ctx, b.proposal)
	if err != nil {
		return c.publicationFailed(cand, "propose", err)
	}
	if err := c.fault(ctx, codefault.PublishAfterPropose, 0); err != nil {
		return c.publicationFailed(cand, "adopt", err)
	}
	return c.publicationAnswered(ctx, cand, b, res, began)
}

// heldBase is how many of rows[:base], from the root, the object store still
// holds every part of — base where it holds them all — or the store's error
// where it could not be read: one Stat a part, the first part the store lacks
// or holds at another size ending the walk.
//
// WHY THE MAINTAINER ASKS: every other git node fetches a layer's parts and
// refuses one the store lost (`missing`), but the maintainer installs its own
// layer from the files it wrote (cache.Cache.AdoptLayer), so its copy holds a
// layer the store may have lost since — a restore, which carries no layer, or
// a bucket's own deletion — and its own copy alone would never say so. Built
// over it, a new layer is one no node fetching the chain can install either,
// so a publication is held to the store's copy of its base before anything is
// built: a handful of requests a publication, against a build that walks the
// repository's whole history. A part whose bytes the store changed in place
// is no size of another, and is found by the nodes that fetch it (`corrupt`),
// whose repair waits for this node's own copy to meet it.
func (c *Compactor) heldBase(ctx context.Context, rows []code.LayerRow, base int) (int, error) {
	for i, row := range rows[:base] {
		for _, part := range row.Layer.Parts {
			info, err := c.o.Objects.Stat(ctx, part.Object)
			switch {
			case errors.Is(err, objstore.ErrNotFound):
				return i, nil
			case err != nil:
				return 0, fmt.Errorf("compact: read whether the store holds part %s of layer %s: %w", part.Object,
					row.Layer.LayerID, err)
			case info.Size != part.Size:
				return i, nil
			}
		}
	}
	return base, nil
}

// lostFrom is chain as this node must plan over it once the store lost a part
// of its layer at index i: installed below it alone, and that layer refused
// `missing` — so the plan replaces it with every layer above it, as it
// replaces a layer its own copy refused.
func lostFrom(chain cache.Chain, i int) cache.Chain {
	refused := maps.Clone(chain.Refused)
	if refused == nil {
		refused = map[string]string{}
	}
	refused[chain.Rows[i].Layer.LayerID] = "missing"
	return cache.Chain{Rows: chain.Rows, Installed: min(chain.Installed, i), Refused: refused}
}

// layerBuilt is one publication's build, laid out and checked: the scratch
// that holds it, the plan it was built from, its repack's outputs, the paths
// of the layer's three files (the graph's "" where it carries none) and the
// proposal that names them.
type layerBuilt struct {
	sc       *cache.Scratch
	plan     layerPlan
	outs     []output
	paths    [3]string
	proposal code.CompactionProposal
}

// buildLayer builds p in a scratch of its own — its repack held to a
// compaction's count and bundle rules, rebuilt ONCE at the densest offender's
// ratio as a compaction's is — and, where git wrote the layer without a
// bitmap, planned and built ONCE more over one more layer of the chain. On an
// error the scratch is gone, and the outcome is the publication's.
func (c *Compactor) buildLayer(ctx context.Context, cand code.PublicationCandidate, s cache.Synced,
	adm *cache.Admission, chain cache.Chain, live []code.PackRow, p layerPlan, cap int64,
	limits code.Limits) (layerBuilt, Outcome, error) {

	b, out, err := c.buildLayerChecked(ctx, cand, s, adm, chain, p, cap, limits)
	if errors.Is(err, errNoBitmap) && p.base > 0 {
		// A TAIL HOLDING NO COMMIT — an annotated tag's pack, a push of
		// blobs — gives git nothing to bitmap, and a layer without a bitmap
		// switches off every bitmap beneath it: absorbed into one more
		// layer, the layer holds that layer's commits.
		if more, _ := planLayer(chain, live, cap, c.bounds, 1); more.base < p.base {
			// The layer absorbed is a published one: its packs, which the
			// reservation gains, are none a compaction's plan merges.
			c.reserve(cand.ID, more)
			b, out, err = c.buildLayerChecked(ctx, cand, s, adm, chain, more, cap, limits)
		}
	}
	if errors.Is(err, errNoBitmap) {
		c.publishRefusal(reasonNoBitmap)
		c.log.Warn("code_publication_failed", "repo", cand.Name, "step", "build", "reason", reasonNoBitmap,
			"detail", "git wrote the layer without a bitmap: no pack it covers holds a commit")
		return layerBuilt{}, OutcomeFailed, err
	}
	return b, out, err
}

// buildLayerChecked builds p once — and its repack once more where an output
// passes the bundle rule — and holds the proposal to every rule its committer
// will. On an error the scratch is gone.
func (c *Compactor) buildLayerChecked(ctx context.Context, cand code.PublicationCandidate, s cache.Synced,
	adm *cache.Admission, chain cache.Chain, p layerPlan, cap int64, limits code.Limits) (layerBuilt, Outcome,
	error) {

	began := c.now()
	cut := c.bounds.cut
	if p.repack.due {
		cut = c.bounds.cutFor(p.repack)
		if at := InputByteCap(cut); at < cap {
			p.repack, cap = p.repack.recap(at), at
		}
	}
	b, err := c.writeLayer(ctx, cand.ID, s, adm, chain, p, cut, limits)
	if err != nil {
		return layerBuilt{}, c.publicationFailedOnly(cand, "build", err), err
	}
	if out, err := c.layerCountHolds(cand, b, cap); err != nil {
		return layerBuilt{}, out, err
	}
	if ratio := pastBound(b.outs, c.bounds.maxBundle); ratio > 0 {
		// ONE REBUILD, in the same reserved slot, at the densest offender's
		// own ratio, the repack capped again at that cut's byte cap.
		b.sc.Remove()
		cut = int64(float64(c.bounds.cut) / ratio)
		if at := InputByteCap(cut); at < cap {
			p.repack, cap = p.repack.recap(at), at
		}
		if b, err = c.writeLayer(ctx, cand.ID, s, adm, chain, p, cut, limits); err != nil {
			return layerBuilt{}, c.publicationFailedOnly(cand, "build", err), err
		}
		if out, err := c.layerCountHolds(cand, b, cap); err != nil {
			return layerBuilt{}, out, err
		}
		if pastBound(b.outs, c.bounds.maxBundle) > 0 {
			b.sc.Remove()
			c.layerCapFailed(cand, b.plan.repack, cap, reasonBundleTooLarge, len(b.outs))
			return layerBuilt{}, OutcomeFailed, errBundleTooLarge
		}
	}
	if err := c.fault(ctx, codefault.PublishAfterBuild, 0); err != nil {
		b.sc.Remove()
		return layerBuilt{}, c.publicationFailedOnly(cand, "build", err), err
	}
	layer := b.proposal.Compaction.Layer
	layer.TookMs = c.now().Sub(began).Milliseconds()
	if err := b.proposal.ValidateUnstored(); err != nil {
		// THE BUILDER'S OWN BUG — every rule above passed and one of its
		// committer's did not — refused before any byte is stored.
		b.sc.Remove()
		c.log.Error("code_publication_invalid", "repo", cand.Name, "publication", b.proposal.Compaction.CompactionID,
			"error", err)
		return layerBuilt{}, OutcomeFailed, err
	}
	return b, "", nil
}

// layerCountHolds holds a publication's repack to its own count, as
// [Compactor.countHolds] holds a compaction's: past what one record retires
// and adds, nothing is uploaded and the repository's cap is halved.
func (c *Compactor) layerCountHolds(cand code.PublicationCandidate, b layerBuilt, cap int64) (Outcome, error) {
	if n := len(b.outs); !b.plan.repack.due || (n <= code.MaxCompactOutputs && n <= len(b.plan.repack.ids())) {
		return "", nil
	}
	b.sc.Remove()
	c.layerCapFailed(cand, b.plan.repack, cap, reasonOutputs, len(b.outs))
	return OutcomeFailed, errOutputs
}

// layerCapFailed halves cand's cap after a publication's repack of p, capped
// at cap, failed for reason — [Compactor.capFailed]'s rule, the cap the two
// loops share (a fact about what the repository's packs make of a build) —
// remembers the repository as having nothing to publish at its version where
// the cap was already at its floor, and counts and logs it.
func (c *Compactor) layerCapFailed(cand code.PublicationCandidate, p planned, cap int64, reason string,
	outputs int) {

	atFloor := c.halveCap(cand.ID, p, cap)
	if atFloor {
		c.rememberNothing(cand)
	}
	c.publishRefusal(reason)
	c.log.Warn("code_publication_failed", "repo", cand.Name, "step", "build", "reason", reason,
		"inputs", len(p.inputs), "outputs", outputs, "most_outputs", code.MaxCompactOutputs,
		"input_bytes", p.packBytes(), "next_cap", c.capFor(cand.ID), "at_floor", atFloor)
}

// writeLayer writes p's layer in a scratch named by a fresh publication id —
// its repack at cut where it has one — and lays it out into the publication
// its committer would be asked to decide. On an error the scratch is removed.
func (c *Compactor) writeLayer(ctx context.Context, repoID string, s cache.Synced, adm *cache.Admission,
	chain cache.Chain, p layerPlan, cut int64, limits code.Limits) (layerBuilt, error) {

	cid := statelog.NewOpID(c.now(), "")
	sc, err := c.o.Cache.NewLayerScratch(ctx, s, cid, p.base)
	if err != nil {
		return layerBuilt{}, err
	}
	b := layerBuilt{sc: sc, plan: p}
	if err := c.writeLayerIn(ctx, &b, s, cid, adm, chain, cut, limits); err != nil {
		sc.Remove()
		return layerBuilt{}, err
	}
	b.proposal = code.CompactionProposal{Compaction: code.Compaction{
		RepoID: repoID, CompactionID: cid, Node: c.o.NodeID, Seen: s.Position, Add: refs(b.outs),
		Layer: b.proposal.Compaction.Layer}}
	if p.repack.due {
		b.proposal.Compaction.Retire = p.repack.ids()
	}
	return b, nil
}

// writeLayerIn is writeLayer's build in b.sc: the repack, the
// multi-pack-index layer, the commit-graph layer, and the layer laid out.
func (c *Compactor) writeLayerIn(ctx context.Context, b *layerBuilt, s cache.Synced, cid string,
	adm *cache.Admission, chain cache.Chain, cut int64, limits code.Limits) error {

	if b.plan.repack.due {
		outs, err := c.repack(ctx, s, b.sc, cid, adm, b.plan.repack, cut, limits)
		if err != nil {
			return err
		}
		b.outs = outs
	}
	packs := b.plan.layerPacks(b.outs)
	var stdin strings.Builder
	var packBytes int64
	sizes := map[string]int64{}
	for _, m := range b.plan.members {
		sizes[m.checksum] = m.packBytes
	}
	for _, o := range b.outs {
		sizes[o.ref.GitChecksum] = o.ref.PackBytes()
	}
	for _, cs := range packs {
		stdin.WriteString("pack-" + cs + ".idx\n")
		packBytes += sizes[cs]
	}
	baseArg, baseID := "none", ""
	if b.plan.base > 0 {
		baseID = chain.Rows[b.plan.base-1].Layer.LayerID
		baseArg = baseID
	}
	res, err := adm.Run(ctx, gitproc.Cmd{
		Dir: b.sc.Dir, Class: gitproc.ClassMaintenance,
		Args: []string{"multi-pack-index", "write", "--incremental", "--bitmap", "--stdin-packs",
			"--no-write-chain-file", "--base=" + baseArg, "--no-progress"},
		Stdin:  strings.NewReader(stdin.String()),
		Config: []gitproc.KV{{Key: "pack.writeBitmapLookupTable", Value: "true"}},
		// THE BITMAP WALKS EVERY COMMIT and reads the objects they reach,
		// the base's among them: a deadline sized by the layer's packs
		// alone would cut the walk of a large repository's history short.
		Deadline: gitproc.PackWorkDeadline(b.plan.historyBytes) + gitproc.HistoryWalk(b.plan.historyObjects),
	})
	if err != nil {
		return fmt.Errorf("compact: write the multi-pack-index layer over %d packs: %w", len(packs), err)
	}
	id := strings.TrimSpace(string(res.Stdout))
	if !gitref.ValidOID(id) {
		return fmt.Errorf("compact: multi-pack-index write printed %q where it names the layer it wrote",
			textcut.Ellipsis(id, 120))
	}
	layers := filepath.Join(b.sc.Dir, "objects", "pack", "multi-pack-index.d")
	b.paths[0] = filepath.Join(layers, "multi-pack-index-"+id+".midx")
	b.paths[1] = filepath.Join(layers, "multi-pack-index-"+id+".bitmap")
	if _, err := os.Lstat(b.paths[1]); errors.Is(err, os.ErrNotExist) {
		return errNoBitmap
	} else if err != nil {
		return fmt.Errorf("compact: read whether the layer has its bitmap: %w", err)
	}
	baseGraphs, err := graphChain(b.sc.Dir)
	if err != nil {
		return err
	}
	if _, err := adm.Run(ctx, gitproc.Cmd{
		Dir: b.sc.Dir, Class: gitproc.ClassMaintenance,
		Args:   []string{"commit-graph", "write", "--stdin-packs", "--split=no-merge", "--changed-paths", "--no-progress"},
		Stdin:  strings.NewReader(stdin.String()),
		Config: []gitproc.KV{{Key: "commitGraph.changedPathsVersion", Value: "2"}},
		// EVERY FILTER IS COMPUTED (no --max-new-filters): the layer is
		// written once for the fleet.
		Deadline: gitproc.PackWorkDeadline(packBytes) + gitproc.HistoryWalk(b.plan.objects),
	}); err != nil {
		return fmt.Errorf("compact: write the commit-graph layer over %d packs: %w", len(packs), err)
	}
	graphs, err := graphChain(b.sc.Dir)
	if err != nil {
		return err
	}
	switch {
	case len(graphs) == len(baseGraphs)+1 && slices.Equal(graphs[:len(baseGraphs)], baseGraphs):
		b.paths[2] = filepath.Join(b.sc.Dir, "objects", "info", "commit-graphs", "graph-"+graphs[len(graphs)-1]+".graph")
	case !slices.Equal(graphs, baseGraphs):
		return fmt.Errorf("compact: the commit-graph write left the chain %v over the base's %v: no layer "+
			"over exactly that base", graphs, baseGraphs)
	}
	ref, err := code.LayLayer(ctx, b.paths[0], b.paths[1], b.paths[2], baseGraphs, code.Pieces{})
	if err != nil {
		return fmt.Errorf("compact: lay out layer %s: %w", id, err)
	}
	if !slices.Equal(ref.Packs, packs) {
		return fmt.Errorf("compact: layer %s names the packs %v, and the build wrote it over %v", id, ref.Packs,
			packs)
	}
	ref.BaseID, ref.Replaces, ref.Git = baseID, slices.Clone(b.plan.replaces), adm.GitVersion().String()
	b.proposal.Compaction.Layer = &ref
	return nil
}

// repack builds a publication's repack of p — the non-full packs its layer
// gathers, at cut — in a MERGE'S SCRATCH OF ITS OWN, leaning on a store of
// exactly p's inputs (cache.Cache.NewMergeScratch) as a compaction's build
// does ([Compactor.pack]); lays its outputs out under the publication cid's
// derived ids, holding the merge's scratch to exactly what the build printed
// ([lay]); and gathers them into the layer's scratch sc
// (cache.Scratch.Gather), where the multi-pack-index layer names them and the
// adoption finds them. The merge's scratch is removed before it answers: sc's
// links are the same inodes.
//
// NEVER IN THE LAYER'S SCRATCH, which links every pack of the copy: there
// `--stdin-packs`, filling in its name hashes, walked from the inputs' commits
// down the repository's whole history, as a compaction's build over the
// incarnation did — about 3.5 µs an object of the history on the node pin, a
// parsed object kept for each — while the repack's charge and its deadline
// follow its inputs, so past a few million objects every publication that
// repacked ran out its repack's deadline and failed, at every pass that
// planned it.
func (c *Compactor) repack(ctx context.Context, s cache.Synced, sc *cache.Scratch, cid string,
	adm *cache.Admission, p planned, cut int64, limits code.Limits) ([]output, error) {

	ms, err := c.o.Cache.NewMergeScratch(ctx, s, statelog.NewOpID(c.now(), ""), p.checksums())
	if err != nil {
		return nil, err
	}
	defer ms.Remove()
	printed, err := c.pack(ctx, adm, ms, p, cut, limits)
	if err != nil {
		return nil, err
	}
	outs, err := lay(ms, cid, printed)
	if err != nil {
		return nil, err
	}
	if err := sc.Gather(ms, printed); err != nil {
		return nil, fmt.Errorf("compact: gather the repack's %d outputs into the layer's scratch: %w", len(printed),
			err)
	}
	for i := range outs {
		outs[i].path = filepath.Join(sc.Dir, "objects", "pack", filepath.Base(outs[i].path))
	}
	return outs, nil
}

// graphChain is the commit-graph layers a scratch's commit-graph-chain lists,
// root first — none where it holds no chain file.
func graphChain(dir string) ([]string, error) {
	text, err := os.ReadFile(filepath.Join(dir, "objects", "info", "commit-graphs", "commit-graph-chain"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("compact: read the scratch's commit-graph-chain: %w", err)
	}
	var out []string
	lines := bufio.NewScanner(bytes.NewReader(text))
	for lines.Scan() {
		if line := strings.TrimSpace(lines.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out, lines.Err()
}

// uploadLayer stores every part of b's layer and every repack output AT ONCE
// ([uploadAll]) — each hit at the PublishUpload point before every read the
// store makes of it, by the one closure both kinds of upload are handed — and
// names them in the proposal. A store with no room holds the repository back
// until it has room and refuses the publication `store_full`; any other
// failure is the next poll's.
func (c *Compactor) uploadLayer(ctx context.Context, cand code.PublicationCandidate, b *layerBuilt) (Outcome,
	error) {

	layer := b.proposal.Compaction.Layer
	hit := func(ctx context.Context, at int64) error { return c.fault(ctx, codefault.PublishUpload, at) }
	var parts []code.LayerPart
	uploads := []func(context.Context) error{func(ctx context.Context) error {
		var err error
		parts, err = code.StoreLayer(ctx, c.o.Objects, b.paths, layer.Sections(), hit)
		return err
	}}
	for i := range b.outs {
		o := &b.outs[i]
		uploads = append(uploads, func(ctx context.Context) error {
			return code.StoreBundle(ctx, c.o.Objects, o.path, &o.ref, hit)
		})
	}
	err := uploadAll(ctx, uploads)
	if full, ok := storeFull(err); ok {
		estimate := layerEstimate(b.plan.objects) + b.plan.repack.size()
		c.holdFull(publicationLoop, cand.ID, estimate)
		c.publishRefusal(reasonStoreFull)
		c.log.Warn("code_publication_refused", "repo", cand.Name, "publication", b.proposal.Compaction.CompactionID,
			"refusal", reasonStoreFull, "bytes", estimate, "error", err,
			"detail", fmt.Sprintf("the object store has no room for this publication's layer and outputs; it is "+
				"planned again once %s", full.Remedy))
		return OutcomeRefused, err
	}
	if err != nil {
		var bad *code.BadBundle
		if errors.As(err, &bad) {
			c.log.Error("code_publication_rejected", "repo", cand.Name,
				"publication", b.proposal.Compaction.CompactionID, "object", bad.Object.String(), "error", bad.Err)
		}
		return c.publicationFailed(cand, "upload", err)
	}
	layer.Parts = parts
	for i, o := range b.outs {
		b.proposal.Compaction.Add[i] = o.ref
	}
	if c.o.Metrics != nil {
		c.o.Metrics.Add(metrics.CodePublishBytes, uint64(layer.Size()), metrics.Attrs{"kind": "layer"})
		if n := outputBytes(b.outs); n > 0 {
			c.o.Metrics.Add(metrics.CodePublishBytes, uint64(n), metrics.Attrs{"kind": "outputs"})
		}
	}
	c.stored(ctx, publicationLoop, cand.ID)
	return "", nil
}

// publicationAnswered is what the committer's answer makes of a publication
// whose layer and outputs are stored.
func (c *Compactor) publicationAnswered(ctx context.Context, cand code.PublicationCandidate, b layerBuilt,
	res code.CompactResult, began time.Time) (Outcome, error) {

	cid, layer := b.proposal.Compaction.CompactionID, b.proposal.Compaction.Layer
	switch res.Outcome {
	case statelog.OutcomeApplied:
		// THE OUTPUTS AND THE LAYER ARE PRESENT AT ONCE, linked from the
		// scratch — never a GET of what this node just stored. A failed
		// adoption costs the round or the installer a GET, never the
		// publication.
		if len(b.outs) > 0 {
			if err := c.o.Cache.Adopt(ctx, cand.ID, b.sc, b.proposal.Compaction.Add...); err != nil {
				c.log.Warn("code_publication_adopt_failed", "repo", cand.Name, "publication", cid, "error", err)
			}
		}
		if err := c.o.Cache.AdoptLayer(ctx, cand.ID, b.sc, *layer); err != nil {
			c.log.Warn("code_publication_adopt_failed", "repo", cand.Name, "publication", cid, "layer",
				layer.LayerID, "error", err)
		}
		c.mu.Lock()
		delete(c.nothing, cand.ID)
		c.mu.Unlock()
		c.log.Info("code_publication_applied", "repo", cand.Name, "publication", cid, "layer", layer.LayerID,
			"base", layer.BaseID, "replaces", len(layer.Replaces), "repacked", len(b.proposal.Compaction.Retire),
			"outputs", len(b.outs), "objects", layer.Objects, "bytes", layer.Size(), "position", res.Position.String(),
			"took", c.now().Sub(began))
		return OutcomeApplied, nil
	case statelog.OutcomePending:
		return OutcomePending, nil
	case statelog.OutcomeUnknown:
		return OutcomeUnknown, nil
	}
	if res.Refusal != "" {
		c.publishRefusal(string(res.Refusal))
	}
	refusal := fmt.Errorf("compact: publication %s of %s refused %s: %s", cid, cand.Name, res.Refusal, res.Detail)
	switch res.Refusal {
	case code.RefusalStale:
		c.log.Debug("code_publication_stale", "repo", cand.Name, "publication", cid, "detail", res.Detail)
		return OutcomeStale, refusal
	case code.RefusalBusy:
		c.log.Debug("code_publication_busy", "repo", cand.Name, "publication", cid, "detail", res.Detail)
		return OutcomeBusy, refusal
	case code.RefusalUnavailable:
		return c.publicationFailed(cand, "propose", refusal)
	}
	c.log.Warn("code_publication_refused", "repo", cand.Name, "publication", cid, "refusal", string(res.Refusal),
		"detail", res.Detail)
	return OutcomeRefused, refusal
}

// publicationFailed logs a publication that failed at step and answers it
// failed.
func (c *Compactor) publicationFailed(cand code.PublicationCandidate, step string, err error) (Outcome, error) {
	return c.publicationFailedOnly(cand, step, err), err
}

// publicationFailedOnly logs a publication that failed at step: failed.
func (c *Compactor) publicationFailedOnly(cand code.PublicationCandidate, step string, err error) Outcome {
	c.log.Warn("code_publication_failed", "repo", cand.Name, "step", step, "error", err)
	return OutcomeFailed
}

// countedPublication records one publication's outcome and how long it took.
func (c *Compactor) countedPublication(o Outcome, began time.Time) {
	if c.o.Metrics == nil {
		return
	}
	c.o.Metrics.Add(metrics.CodePublishRuns, 1, metrics.Attrs{"outcome": string(o)})
	c.o.Metrics.Observe(metrics.CodePublishDuration, c.now().Sub(began), metrics.Attrs{"outcome": string(o)})
}

// publishRefusal counts one publication refused by its reason.
func (c *Compactor) publishRefusal(reason string) {
	if c.o.Metrics != nil {
		c.o.Metrics.Add(metrics.CodePublishRefusals, 1, metrics.Attrs{"reason": reason})
	}
}
