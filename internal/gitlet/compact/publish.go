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
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
)

// PublishPoll is the interval between two passes of the PUBLICATION loop: one
// indexed listing a Gitlet node a minute, as [Poll] is the compaction loop's.
const PublishPoll = time.Minute

// The publication's cadence ([Due]). The MEASURED values are BenchmarkMidx's
// arms (internal/gitlet/cache's layers_bench_test.go; package doc,
// "Measured"), over gittest.History at 100 MB and a gigabyte.
const (
	// PublishPacks is a tail worth a publication by its PACKS: a pack outside
	// the chain costs every request that reaches it a walk of its own commits
	// and trees (cache.MidxPacks's reason). MEASURED (stale=k/mode=chain;
	// step=publish/layer=tail): a pack outside the chain costs a clone's count
	// 0.14 ms at 100 MB and 0.07 ms at the gigabyte, against a publication of
	// 0.66 s and 9.9 s, so publishing every M pushes, sixteen requests a push,
	// is cheapest at M = √(2T/qδ): 25 and 129. Sixty-four, near their
	// geometric middle (57), is a JUDGMENT CALL costing each end a quarter to
	// a half past its least, where either least costs the other end nearly
	// three times its own; past a minute's build the duty cycle binds first.
	PublishPacks = 64
	// PublishShare is a tail worth a publication by its SHARE of the
	// repository's objects: a tail's objects are walked, never read off the
	// bitmap, and packed by every clone that reaches them. MEASURED (share=…:
	// a clone with the last 1, 4 and 16 of 64 pushes outside a published
	// root): a sixty-fourth added nothing a run shows at 100 MB (0.64 s) or
	// the gigabyte (6.53 s against 6.60), a sixteenth a quarter, and a quarter
	// more than doubled it, so the sixty-fourth is the largest share adding at
	// most a tenth to a clone at both sizes. A share alone does not bound a
	// large repository's tail: PublishTailObjects does.
	PublishShare = 64
	// PublishTailObjects is the ABSOLUTE bound on a tail's OBJECTS: every
	// clone, the installer's canary and the node's tip commit-graph walk every
	// object outside the chain, at most gitproc.WalkPerObject each plus
	// gitproc.TransferBytesPerObject of a clone's memory, so this many cost a
	// clone at most a second of walk and some 21 MiB. A sixty-fourth of the
	// largest repository the defaults admit would add 10 s to every clone, a
	// tail compaction keeps under PublishPacks. Due at whichever of the share
	// and this its tail reaches first. MEASURED (tail=…, at the gigabyte): the
	// canary walks a tail object in 2.7–3.3 µs, the tip's write in 1.1–2.8,
	// and a clone pays 11.0 µs, packing included, gitproc.WalkPerObject's own
	// figure; the clone at the bound ran 1.23 s past one with no tail.
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
	// repository, and between two REPAIRS: a layer's build walks the whole
	// history (cache.MidxMinInterval's reason). TEN MINUTES, the larger of ten
	// minutes and ten times a publication at 5 GB: step=publish/layer=tail
	// took 0.66 s at 100 MB and 9.9 s at the gigabyte, the walk growing faster
	// than the bytes, so about a minute at 5 GB by extrapolation (the 5 GB
	// arm, under CREWLET_BENCH_LARGE, has not run there). A JUDGMENT CALL, and a
	// safe one: past a minute's build PublishDutyCycle binds anyway.
	PublishMinInterval = 10 * time.Minute
	// PublishDutyCycle holds a repository's publications to at most a tenth
	// of its node's time: the next waits this many times the last one's
	// build, where that is longer than PublishMinInterval (cache.MidxDutyCycle's
	// reason). KEPT. Every build this node begins is held to it, landed or not
	// ([Compactor.paced]): its time is spent whatever its record becomes.
	PublishDutyCycle = 10
)

// Due reports whether c's tail should be published at now, judged from its
// row and tailSince (when this node first listed the tail) before any sync:
// the tail holds a pack; it is WORTH one (never published, PublishPacks
// packs, or the fewer of a PublishShare-th of the objects and
// PublishTailObjects); it has SETTLED (PublishDebounce since the last push,
// or PublishMaxWait since the later of tailSince and the last publication);
// and the DUTY CYCLE allows it (the longer of PublishMinInterval and
// PublishDutyCycle times the last build, since the last publication).
//
// The ceiling never runs from the last publication alone: PublishMaxWait is
// PublishMinInterval, so a burst would be published at its first pack. A tail
// may be a re-pushed published pack, which only the plan finds empty.
func Due(c gitlet.PublicationCandidate, tailSince, now time.Time) bool {
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

// PublishPass is one pass of the PUBLICATION loop. It LISTS every repository
// with a tail (`publication_due`, every page, least recently published first,
// at STALE for the compaction listing's reason) and every one whose copy here
// holds a layer this node refused (cache.Cache.Damaged), which only a REPAIR
// replaces and no due listing names once idle. It judges MAINTAINERS off ONE
// listing ([Compactor.maintainedOfPublications]), publishing nothing when it is
// unread; reads the store's room once ([Compactor.releaseHeld]); and until the
// next tick PUBLISHES ([Compactor.Publish]), one at a time, each repository not
// remembered empty (a repair excepted), held for a full store, paced
// ([Compactor.paced]), short of [Due] or [Compactor.repairDue], or maintained
// elsewhere. A failed listing is the pass's error; code_publication_pass logs
// at DEBUG unless the pass published, refused or failed.
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
			listed = append(listed, gitlet.PublicationCandidate{ID: ref.ID, Name: ref.Name, LivePacks: ref.LivePacks,
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
			c.paced(cand.ID, now),
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

// maintainedOfPublications is which listed repositories this node maintains,
// off ONE listing of who serves git, as [Compactor.maintainedOf] judges the
// compaction loop's: asked of each the pass may publish (a repair, or one not
// remembered empty) by its charge ([Compactor.PlacedBy]). An error is UNKNOWN
// for all of them, never "maintains none". The clock's rules ([Due], a
// repair's pace) stay with the pass, which reads the clock at each repository.
func (c *Compactor) maintainedOfPublications(listed []gitlet.PublicationCandidate,
	damaged map[string]bool) (map[string]bool, error) {

	ask := map[string]int64{}
	for _, cand := range listed {
		if damaged[cand.ID] || !c.nothingAt(cand) {
			ask[cand.ID] = c.PlacedBy(cand.LiveObjects)
		}
	}
	if len(ask) == 0 {
		return nil, nil
	}
	return c.o.Maintainers.MaintainerOf(ask)
}

// listPublications is every repository the `publication_due` listing names,
// every page.
func (c *Compactor) listPublications(ctx context.Context) ([]gitlet.PublicationCandidate, error) {
	q := gitlet.ReadQuery{Kind: gitlet.ReadPublicationDue, Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	var out []gitlet.PublicationCandidate
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

// forgetPublications drops the loop's memories (nothing to publish, a
// full-store hold, a failed build's pace) of every repository the pass no
// longer names, bounding them by the repositories with a tail or a refused
// layer; one that comes back is planned from its rows.
func (c *Compactor) forgetPublications(named []gitlet.PublicationCandidate) {
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

// sightTails answers when this node first sighted each listed repository's
// tail, what [Due]'s ceiling runs from: at for one not sighted before, and a
// sighting is forgotten once the listing no longer names it, so the next tail
// is sighted afresh. The row holds no clock of the tail's own, so this is
// within one PublishPoll of its start; a restart or a new maintainer starts it
// again, costing at most one ceiling more. A tail that never empties keeps its
// sighting, which its last publication bounds from below. Only the due
// listing is sighted, never a repository listed for a refused layer alone.
func (c *Compactor) sightTails(listed []gitlet.PublicationCandidate, at time.Time) map[string]time.Time {
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
func (c *Compactor) nothingAt(cand gitlet.PublicationCandidate) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.nothing[cand.ID]
	return ok && v == cand.Version
}

// rememberNothing is a plan of cand that found nothing to publish at its
// version, or a layer too large at it: skipped until the version moves.
func (c *Compactor) rememberNothing(cand gitlet.PublicationCandidate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nothing[cand.ID] = cand.Version
}

// repairDue reports whether a REPAIR of cand may be built at now: at most one
// a PublishMinInterval since the last publication, so a store that keeps
// losing layers costs one publication an interval, never one a pass. A repair
// that did not land is paced besides ([Compactor.paced]).
func (c *Compactor) repairDue(cand gitlet.PublicationCandidate, now time.Time) bool {
	return cand.PublishedAt.IsZero() || now.Sub(cand.PublishedAt) >= PublishMinInterval
}

// paced reports whether this node's last build of a publication of repoID
// that did not land (failed, refused, or not known applied) holds it back at
// now: until the longer of PublishMinInterval and PublishDutyCycle times that
// build's length has passed since it began, the duty cycle [Due] reads off
// the row, which only an applied record moves. Unpaced, a failing repository,
// least recently published, heads every pass and is rebuilt back to back,
// and no other repository the node maintains is published again.
//
// A NODE-LOCAL PACE, NEVER A MEMORY AT THE VERSION: a memory refusal is no
// build and never remembered (M3-decisions M3-O26), and a full store's hold
// stays its own ([Compactor.releaseHeld]), so a build refused for room waits
// for both. A restart forgets it, costing one build more.
func (c *Compactor) paced(repoID string, now time.Time) bool {
	c.mu.Lock()
	last, ok := c.built[repoID]
	c.mu.Unlock()
	return ok && now.Sub(last.began) < max(PublishMinInterval, PublishDutyCycle*last.took)
}

// errLayerTooLarge and errNoBitmap are publications this node refuses before
// any byte of them is stored: a layer past what a layer's parts hold, and a
// build git wrote no bitmap of, absorbing one more layer, or over the chain's
// root.
var (
	errLayerTooLarge = errors.New("compact: the layer would weigh more than a layer's parts hold")
	errNoBitmap      = errors.New("compact: git wrote the layer without a bitmap")
)

// Publish plans and runs ONE publication of cand (a repository this node
// maintains, listed due or found damaged) and answers what became of it,
// retrying nothing inside, as [Compactor.Compact]. cand.ID is the REPOSITORY;
// the publication's own id is minted at its build. It SYNCS at session,
// floored at the listed version (package doc); PLANS ([planLayer]) and
// RESERVES what the layer covers ([Compactor.reserve]); ADMITS the build and
// holds its base to the store's copy ([Compactor.admitOverItsBase]); BUILDS
// it ([Compactor.buildLayer]), releasing the admission once its last git step
// ends, before the uploads (package doc, "The builds' bounds"); UPLOADS
// ([Compactor.uploadLayer]); and PROPOSES one compaction carrying the layer,
// adopted by link once applied. Every build it begins is paced until one
// lands ([Compactor.paced]).
func (c *Compactor) Publish(ctx context.Context, cand gitlet.PublicationCandidate) (Outcome, error) {
	c.publishing.Lock()
	defer c.publishing.Unlock()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	//nolint:contextcheck // c.life only cancels ctx, which stays the caller's: Stop must end the publication however long the caller's ctx lives.
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
func (c *Compactor) publish(ctx context.Context, cand gitlet.PublicationCandidate, began time.Time) (Outcome,
	error) {

	s, err := c.o.Cache.Sync(ctx, cand.ID, statelog.FloorAt(statelog.ReadSession, cand.Version))
	if err != nil {
		return c.publicationFailed(cand, "sync", err)
	}
	defer s.Release()

	chain := c.o.Cache.Chain(s)
	live, err := c.o.Cache.Packs(s)
	switch {
	case errors.Is(err, cache.ErrSuperseded):
		c.log.Debug("code_publication_busy", "repo", cand.Name, "reason", reasonSuperseded, "error", err)
		return OutcomeBusy, err
	case err != nil:
		return c.publicationFailed(cand, "packs", err)
	}
	cap := c.capFor(cand.ID)
	p, verdict := planLayer(chain, live, cap, c.bounds, 0)
	if out, refusal := c.buildable(cand, p, verdict); out != "" {
		return out, refusal
	}
	c.reserve(cand.ID, p)
	limits := c.o.Limits()
	adm, chain, p, out, err := c.admitOverItsBase(ctx, cand, s, chain, live, p, cap, limits)
	if adm == nil {
		return out, err
	}
	defer adm.Release()

	// THE BUILD IS PACED UNTIL IT LANDS ([Compactor.paced]): marked here,
	// whatever becomes of it, and forgotten once its record applies.
	start := c.now()
	b, out, err := c.buildLayer(ctx, cand, s, adm, chain, live, p, cap, limits)
	c.mu.Lock()
	c.built[cand.ID] = lastBuild{began: start, took: c.now().Sub(start)}
	c.mu.Unlock()
	if err != nil {
		return out, err
	}
	defer b.sc.Remove()
	adm.Release()

	if out, err = c.uploadLayer(ctx, cand, &b); err != nil {
		return out, err
	}
	if err = c.fault(ctx, gitletfault.PublishAfterUpload, 0); err != nil {
		return c.publicationFailed(cand, "propose", err)
	}
	res, err := c.o.Estate.Compact(ctx, b.proposal)
	if err != nil {
		return c.publicationFailed(cand, "propose", err)
	}
	if err = c.fault(ctx, gitletfault.PublishAfterPropose, 0); err != nil {
		return c.publicationFailed(cand, "adopt", err)
	}
	return c.publicationAnswered(ctx, cand, b, res, began)
}

// buildable answers a plan that builds nothing — a layer still at this node's
// installer, `waiting` and planned again at the next pass; nothing to publish,
// remembered at the listing's version; a layer too large, refused and
// remembered — and "" for a layer to build.
func (c *Compactor) buildable(cand gitlet.PublicationCandidate, p layerPlan, verdict layerVerdict) (Outcome, error) {
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
			"most", gitlet.MaxLayerParts*gitlet.LayerPartBytes)
		return OutcomeRefused, errLayerTooLarge
	}
	return "", nil
}

// admitOverItsBase admits a publication of p to the maintenance lane
// (cache.Cache.AdmitPublication; a refusal is [Compactor.admissionRefused]'s),
// then holds its base to the object store's copy ([Compactor.heldBase]),
// answering the admission and the chain and plan the build runs over, or a
// nil admission and the publication's outcome. A base the store lost is
// replaced with every layer above it ([lostFrom]): replanned, reserved, and
// recharged in the SAME slot (cache.Admission.RechargePublication), since a
// release and a second admission would wake the maintainer, which could take
// the slot and leave the repair busy at every pass. The store is asked only
// once admitted, so an attempt refused busy or for memory asks it nothing.
func (c *Compactor) admitOverItsBase(ctx context.Context, cand gitlet.PublicationCandidate, s cache.Synced,
	chain cache.Chain, live []gitlet.PackRow, p layerPlan, cap int64, limits gitlet.Limits) (*cache.Admission,
	cache.Chain, layerPlan, Outcome, error) {

	adm, err := c.o.Cache.AdmitPublication(s, p.base, p.objects, p.repack.objects(), limits)
	if err != nil {
		out, refusal := c.admissionRefused(cand, err)
		return nil, chain, p, out, refusal
	}
	if p.base == 0 {
		return adm, chain, p, "", nil
	}
	held, err := c.heldBase(ctx, chain.Rows, p.base)
	if err != nil {
		adm.Release()
		out, failed := c.publicationFailed(cand, "store", err)
		return nil, chain, p, out, failed
	}
	if held == p.base {
		return adm, chain, p, "", nil
	}
	c.log.Warn("code_publication_base_lost", "repo", cand.Name, "layer", chain.Rows[held].Layer.LayerID,
		"detail", "the object store no longer holds every part of a layer this node holds; it is replaced, "+
			"with every layer above it")
	chain = lostFrom(chain, held)
	p, verdict := planLayer(chain, live, cap, c.bounds, 0)
	if out, refusal := c.buildable(cand, p, verdict); out != "" {
		adm.Release()
		return nil, chain, p, out, refusal
	}
	// The layers it replaces besides are published ones: their packs, which
	// the reservation gains, are none a compaction's plan merges.
	c.reserve(cand.ID, p)
	if err = adm.RechargePublication(s, p.base, p.objects, p.repack.objects(), limits); err != nil {
		adm.Release()
		out, refusal := c.admissionRefused(cand, err)
		return nil, chain, p, out, refusal
	}
	return adm, chain, p, "", nil
}

// admissionRefused is what a publication whose admission — or the charge of
// its plan made again (cache.Admission.RechargePublication) — the node refused
// with err becomes: busy, the next poll's; a charge the node's memory cannot
// hold, refused `memory`; and anything else failed.
func (c *Compactor) admissionRefused(cand gitlet.PublicationCandidate, err error) (Outcome, error) {
	switch {
	case errors.Is(err, gitproc.ErrBusy):
		c.log.Debug("code_publication_busy", "repo", cand.Name, "error", err)
		return OutcomeBusy, err
	case errors.Is(err, cache.ErrNoMemory):
		// NEVER REMEMBERED (M3-decisions M3-O26): the next pass reads the
		// budget again and asks again who maintains it, which is how a larger
		// Gitlet node takes it over. The cache logged it once.
		c.publishRefusal(reasonMemory)
		return OutcomeRefused, err
	}
	return c.publicationFailed(cand, "admit", err)
}

// heldBase is how many of rows[:base], from the root, the object store still
// holds every part of (base where it holds all), or the store's error: what a
// walk in chain order answers, ended at the layer of the first part missing or
// of another size, failed by the first it could not read. The maintainer
// installs its own layers from its files (cache.Cache.AdoptLayer), so its copy
// never shows a layer the store lost since (a restore, a bucket's deletion),
// and a layer built over it no fetching node could install. A part changed in
// place at its size is found by the nodes that fetch it (`corrupt`). Parts are
// asked AT ONCE, cache.FetchConcurrency at a time as a sync is: one by one,
// S3's 20–100 ms first byte cost a full base's 512 parts
// (gitlet.MaxPublishedLayers × gitlet.MaxLayerParts) up to 50 s of a pass cut
// at PublishPoll. None past the first lost or unread part begins once known.
func (c *Compactor) heldBase(ctx context.Context, rows []gitlet.LayerRow, base int) (int, error) {
	type part struct {
		layer int
		part  gitlet.LayerPart
	}
	var parts []part
	for i, row := range rows[:base] {
		for _, p := range row.Layer.Parts {
			parts = append(parts, part{layer: i, part: p})
		}
	}
	// answers[k] is part k's: nil where the store holds it at its size,
	// errPartLost where it does not, and the store's error where it could
	// not say; first is the first part, in chain order, not answered nil.
	answers := make([]error, len(parts))
	var first atomic.Int64
	first.Store(int64(len(parts)))
	var g errgroup.Group
	g.SetLimit(cache.FetchConcurrency)
	for k, pt := range parts {
		if int64(k) > first.Load() {
			break
		}
		g.Go(func() error {
			if int64(k) > first.Load() {
				return nil
			}
			info, err := c.o.Objects.Stat(ctx, pt.part.Object)
			switch {
			case errors.Is(err, objstore.ErrNotFound), err == nil && info.Size != pt.part.Size:
				answers[k] = errPartLost
			case err != nil:
				answers[k] = fmt.Errorf("compact: read whether the store holds part %s of layer %s: %w",
					pt.part.Object, rows[pt.layer].Layer.LayerID, err)
			default:
				return nil
			}
			for at := first.Load(); int64(k) < at; at = first.Load() {
				if first.CompareAndSwap(at, int64(k)) {
					break
				}
			}
			return nil
		})
	}
	_ = g.Wait() // every Go answers nil: each part's answer is in answers.
	for k, answer := range answers {
		switch {
		case answer == nil:
		case errors.Is(answer, errPartLost):
			return parts[k].layer, nil
		default:
			return 0, answer
		}
	}
	return base, nil
}

// errPartLost is a part of a layer the object store no longer holds as its
// row names it: nothing under its key, or something of another size.
var errPartLost = errors.New("compact: the object store no longer holds the part")

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
	proposal gitlet.CompactionProposal
}

// buildLayer builds p in a scratch of its own — its repack first, held to a
// compaction's count and bundle rules before the layer is written over its
// outputs ([Compactor.repackChecked]) — and, where git wrote the layer without
// a bitmap, planned and built ONCE more over one more layer of the chain. On
// an error the scratch is gone, and the outcome is the publication's.
func (c *Compactor) buildLayer(ctx context.Context, cand gitlet.PublicationCandidate, s cache.Synced,
	adm *cache.Admission, chain cache.Chain, live []gitlet.PackRow, p layerPlan, cap int64,
	limits gitlet.Limits) (layerBuilt, Outcome, error) {

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

// buildLayerChecked builds p once and holds the proposal to every rule its
// committer will. On an error the scratch is gone.
func (c *Compactor) buildLayerChecked(ctx context.Context, cand gitlet.PublicationCandidate, s cache.Synced,
	adm *cache.Admission, chain cache.Chain, p layerPlan, cap int64, limits gitlet.Limits) (layerBuilt, Outcome,
	error) {

	began := c.now()
	b, out, err := c.writeLayer(ctx, cand, s, adm, chain, p, cap, limits)
	if err != nil {
		return layerBuilt{}, out, err
	}
	if err = c.fault(ctx, gitletfault.PublishAfterBuild, 0); err != nil {
		b.sc.Remove()
		return layerBuilt{}, c.publicationFailedOnly(cand, "build", err), err
	}
	layer := b.proposal.Compaction.Layer
	layer.TookMs = c.now().Sub(began).Milliseconds()
	if err = b.proposal.ValidateUnstored(); err != nil {
		// THE BUILDER'S OWN BUG — every rule above passed and one of its
		// committer's did not — refused before any byte is stored.
		b.sc.Remove()
		c.log.Error("code_publication_invalid", "repo", cand.Name, "publication", b.proposal.Compaction.CompactionID,
			"error", err)
		return layerBuilt{}, OutcomeFailed, err
	}
	return b, "", nil
}

// layerCapFailed halves cand's cap after a publication's repack of p, capped
// at cap, failed for reason — [Compactor.capFailed]'s rule, the cap the two
// loops share (a fact about what the repository's packs make of a build) —
// remembers the repository as having nothing to publish at its version where
// the cap was already at its floor, and counts and logs it.
func (c *Compactor) layerCapFailed(cand gitlet.PublicationCandidate, p planned, cap int64, reason string,
	outputs int) {

	atFloor := c.halveCap(cand.ID, p, cap)
	if atFloor {
		c.rememberNothing(cand)
	}
	c.publishRefusal(reason)
	c.log.Warn("code_publication_failed", "repo", cand.Name, "step", "build", "reason", reason,
		"inputs", len(p.inputs), "outputs", outputs, "most_outputs", gitlet.MaxCompactOutputs,
		"input_bytes", p.packBytes(), "next_cap", c.capFor(cand.ID), "at_floor", atFloor)
}

// writeLayer writes p's layer under a fresh publication id and lays it out
// into the proposal its committer decides: the repack first, where it has one
// ([Compactor.repackChecked]), its outputs gathered into the layer's scratch
// ([gather]), then the layer over them ([Compactor.writeLayerIn]). On an
// error nothing it made is left, and the outcome is the publication's.
func (c *Compactor) writeLayer(ctx context.Context, cand gitlet.PublicationCandidate, s cache.Synced,
	adm *cache.Admission, chain cache.Chain, p layerPlan, cap int64, limits gitlet.Limits) (layerBuilt, Outcome,
	error) {

	cid := statelog.NewOpID(c.now(), "")
	b := layerBuilt{plan: p}
	var merged *cache.Scratch
	if p.repack.due {
		rp, out, failed := c.repackChecked(ctx, cand, s, cid, adm, p.repack, cap, limits)
		if failed != nil {
			return layerBuilt{}, out, failed
		}
		// The layer's scratch links the outputs' very inodes, so the
		// merge's goes once they are gathered there.
		defer rp.ms.Remove()
		merged, b.outs, b.plan.repack = rp.ms, rp.outs, rp.plan
	}
	sc, err := c.o.Cache.NewLayerScratch(ctx, s, cid, p.base)
	if err != nil {
		return layerBuilt{}, c.publicationFailedOnly(cand, "build", err), err
	}
	b.sc = sc
	if merged != nil {
		err = gather(sc, merged, b.outs)
	}
	if err == nil {
		err = c.writeLayerIn(ctx, &b, adm, chain)
	}
	if err != nil {
		sc.Remove()
		return layerBuilt{}, c.publicationFailedOnly(cand, "build", err), err
	}
	b.proposal = gitlet.CompactionProposal{Compaction: gitlet.Compaction{
		RepoID: cand.ID, CompactionID: cid, Node: c.o.NodeID, Seen: s.Position, Add: refs(b.outs),
		Layer: b.proposal.Compaction.Layer}}
	if b.plan.repack.due {
		b.proposal.Compaction.Retire = b.plan.repack.ids()
	}
	return b, "", nil
}

// writeLayerIn writes b's layer in b.sc over its packs — the members its
// repack did not merge, and the repack's outputs, gathered there already: the
// multi-pack-index layer, the commit-graph layer, and the layer laid out, each
// git step in the admission's reserved slot (the compactor's runBuild).
func (c *Compactor) writeLayerIn(ctx context.Context, b *layerBuilt, adm *cache.Admission, chain cache.Chain) error {
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
	res, err := c.runBuild(ctx, adm, gitproc.Cmd{
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
	if _, statErr := os.Lstat(b.paths[1]); errors.Is(statErr, os.ErrNotExist) {
		return errNoBitmap
	} else if statErr != nil {
		return fmt.Errorf("compact: read whether the layer has its bitmap: %w", statErr)
	}
	baseGraphs, err := graphChain(b.sc.Dir)
	if err != nil {
		return err
	}
	if _, err = c.runBuild(ctx, adm, gitproc.Cmd{
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
		return fmt.Errorf("compact: the commit-graph write left the chain %s over the base's %s: no layer "+
			"over exactly that base", words.And(graphs), words.And(baseGraphs))
	}
	ref, err := gitlet.LayLayer(ctx, b.paths[0], b.paths[1], b.paths[2], baseGraphs, gitlet.Pieces{})
	if err != nil {
		return fmt.Errorf("compact: lay out layer %s: %w", id, err)
	}
	if !slices.Equal(ref.Packs, packs) {
		return fmt.Errorf("compact: layer %s names the packs %s, and the build wrote it over %s", id, words.And(ref.Packs),
			words.And(packs))
	}
	ref.BaseID, ref.Replaces, ref.Git = baseID, slices.Clone(b.plan.replaces), adm.GitVersion().String()
	b.proposal.Compaction.Layer = &ref
	return nil
}

// repacked is a publication's repack built: the merge's scratch holding its
// outputs, the outputs laid out under the publication's derived ids, and the
// plan it was built from — capped again where a cut smaller than the nominal
// one needed it.
type repacked struct {
	ms   *cache.Scratch
	outs []output
	plan planned
}

// repackChecked builds a publication's repack of p (the non-full packs its
// layer gathers) and holds it to a compaction's rules BEFORE THE LAYER IS
// WRITTEN: the rules read the outputs alone, and the layer's bitmap walks the
// whole history, so an output failing them after would waste it. Cut as a
// compaction's build is ([bounds.cutFor]) and capped at that cut's
// [InputByteCap]; past its COUNT (what one record retires and adds) nothing
// is uploaded and the cap halves; past the BUNDLE RULE it is built ONCE more
// at the densest offender's ratio in a fresh merge's scratch, capped again, a
// second miss halving the cap. The passing repack's merge's scratch is the
// caller's to remove once gathered; on an error nothing it made is left, and
// the outcome is the publication's.
func (c *Compactor) repackChecked(ctx context.Context, cand gitlet.PublicationCandidate, s cache.Synced, cid string,
	adm *cache.Admission, p planned, cap int64, limits gitlet.Limits) (repacked, Outcome, error) {

	cut := c.bounds.cutFor(p)
	if at := InputByteCap(cut); at < cap {
		p, cap = p.recap(at), at
	}
	rp, err := c.repack(ctx, s, cid, adm, p, cut, limits)
	if err != nil {
		return repacked{}, c.publicationFailedOnly(cand, "build", err), err
	}
	if out, failed := c.repackCountHolds(cand, rp, cap); failed != nil {
		return repacked{}, out, failed
	}
	ratio := pastBound(rp.outs, c.bounds.maxBundle)
	if ratio == 0 {
		return rp, "", nil
	}
	// ONE REBUILD, in the same reserved slot and a fresh merge's scratch, at
	// the densest offender's own ratio, the repack capped again at that cut's
	// byte cap.
	rp.ms.Remove()
	cut = int64(float64(c.bounds.cut) / ratio)
	if at := InputByteCap(cut); at < cap {
		p, cap = p.recap(at), at
	}
	if rp, err = c.repack(ctx, s, cid, adm, p, cut, limits); err != nil {
		return repacked{}, c.publicationFailedOnly(cand, "build", err), err
	}
	if out, failed := c.repackCountHolds(cand, rp, cap); failed != nil {
		return repacked{}, out, failed
	}
	if pastBound(rp.outs, c.bounds.maxBundle) > 0 {
		rp.ms.Remove()
		c.layerCapFailed(cand, rp.plan, cap, reasonBundleTooLarge, len(rp.outs))
		return repacked{}, OutcomeFailed, errBundleTooLarge
	}
	return rp, "", nil
}

// repackCountHolds holds a publication's repack to its own count, as
// [Compactor.countHolds] holds a compaction's: past what one record retires
// and adds, nothing is uploaded, the merge's scratch is removed and the
// repository's cap is halved.
func (c *Compactor) repackCountHolds(cand gitlet.PublicationCandidate, rp repacked, cap int64) (Outcome, error) {
	if n := len(rp.outs); n <= gitlet.MaxCompactOutputs && n <= len(rp.plan.ids()) {
		return "", nil
	}
	rp.ms.Remove()
	c.layerCapFailed(cand, rp.plan, cap, reasonOutputs, len(rp.outs))
	return OutcomeFailed, errOutputs
}

// repack builds a publication's repack of p at cut in a MERGE'S SCRATCH OF
// ITS OWN, over a store of exactly p's inputs (cache.Cache.NewMergeScratch),
// as a compaction's build does ([Compactor.pack]), and lays its outputs out
// under cid's derived ids, holding the scratch to exactly what the build
// printed ([lay]). On an error the scratch is gone; otherwise it is the
// caller's. NEVER IN THE LAYER'S SCRATCH, which links every pack: there
// `--stdin-packs` walks the whole history while the repack's charge and
// deadline follow its inputs (package doc, "A merge makes no reachability
// judgement").
func (c *Compactor) repack(ctx context.Context, s cache.Synced, cid string, adm *cache.Admission, p planned,
	cut int64, limits gitlet.Limits) (repacked, error) {

	ms, err := c.o.Cache.NewMergeScratch(ctx, s, statelog.NewOpID(c.now(), ""), p.checksums())
	if err != nil {
		return repacked{}, err
	}
	printed, err := c.pack(ctx, adm, ms, p, cut, limits)
	var outs []output
	if err == nil {
		outs, err = lay(ms, cid, printed)
	}
	if err != nil {
		ms.Remove()
		return repacked{}, err
	}
	return repacked{ms: ms, outs: outs, plan: p}, nil
}

// gather links the repack's outputs from the merge's scratch from into the
// layer's scratch sc (cache.Scratch.Gather) — where the multi-pack-index
// layer names them and the adoption finds them, a multi-pack-index naming only
// packs of its own directory — and points every output's path there: from's
// links are the same inodes, and from goes once they are gathered.
func gather(sc, from *cache.Scratch, outs []output) error {
	checksums := make([]string, len(outs))
	for i, o := range outs {
		checksums[i] = o.ref.GitChecksum
	}
	if err := sc.Gather(from, checksums); err != nil {
		return fmt.Errorf("compact: gather the repack's %d outputs into the layer's scratch: %w", len(outs), err)
	}
	for i := range outs {
		outs[i].path = filepath.Join(sc.Dir, "objects", "pack", filepath.Base(outs[i].path))
	}
	return nil
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
func (c *Compactor) uploadLayer(ctx context.Context, cand gitlet.PublicationCandidate, b *layerBuilt) (Outcome,
	error) {

	layer := b.proposal.Compaction.Layer
	hit := func(ctx context.Context, at int64) error { return c.fault(ctx, gitletfault.PublishUpload, at) }
	var parts []gitlet.LayerPart
	uploads := []func(context.Context) error{func(ctx context.Context) error {
		var err error
		parts, err = gitlet.StoreLayer(ctx, c.o.Objects, b.paths, layer.Sections(), hit)
		return err
	}}
	for i := range b.outs {
		o := &b.outs[i]
		uploads = append(uploads, func(ctx context.Context) error {
			return gitlet.StoreBundle(ctx, c.o.Objects, o.path, &o.ref, hit)
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
		var bad *gitlet.BadBundle
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
func (c *Compactor) publicationAnswered(ctx context.Context, cand gitlet.PublicationCandidate, b layerBuilt,
	res gitlet.CompactResult, began time.Time) (Outcome, error) {

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
		// LANDED: the row its record wrote paces the next build ([Due]).
		c.mu.Lock()
		delete(c.nothing, cand.ID)
		delete(c.built, cand.ID)
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
	case gitlet.RefusalStale:
		c.log.Debug("code_publication_stale", "repo", cand.Name, "publication", cid, "detail", res.Detail)
		return OutcomeStale, refusal
	case gitlet.RefusalBusy:
		c.log.Debug("code_publication_busy", "repo", cand.Name, "publication", cid, "detail", res.Detail)
		return OutcomeBusy, refusal
	case gitlet.RefusalUnavailable:
		return c.publicationFailed(cand, "propose", refusal)
	}
	c.log.Warn("code_publication_refused", "repo", cand.Name, "publication", cid, "refusal", string(res.Refusal),
		"detail", res.Detail)
	return OutcomeRefused, refusal
}

// publicationFailed logs a publication that failed at step and answers it
// failed.
func (c *Compactor) publicationFailed(cand gitlet.PublicationCandidate, step string, err error) (Outcome, error) {
	return c.publicationFailedOnly(cand, step, err), err
}

// publicationFailedOnly logs a publication that failed at step: failed.
func (c *Compactor) publicationFailedOnly(cand gitlet.PublicationCandidate, step string, err error) Outcome {
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
