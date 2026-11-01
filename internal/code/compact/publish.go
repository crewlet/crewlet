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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
)

// PublishPoll is the interval between two passes of the PUBLICATION loop: one
// indexed listing a git node a minute, as [Poll] is the compaction loop's.
const PublishPoll = time.Minute

// The publication's cadence — when a repository's tail is worth a layer
// ([Due]). Each states its reason; the ones MEASURED are BenchmarkMidx's arms
// in internal/code/cache — git 2.56.0, one bounded run of each arm
// (-benchtime 1x, one count) on two E-cores of an Intel Core Ultra 7 265U,
// 2026-10-29, over gittest.History's composition at 100 MB and a gigabyte.
const (
	// PublishPacks is a tail worth a publication by its PACKS: a pack outside
	// the chain costs every request that reaches it a walk of its own
	// commits and trees — the own-mode index's reason for cache.MidxPacks,
	// moved to the layer that replaces it. SIXTY-FOUR, MEASURED
	// (stale=k/mode=chain, pushes of 4 KiB beside a published root; and
	// step=publish/layer=tail, the publication over sixty-four of them):
	// counting a clone's objects through the chain costs about 0.14 ms a
	// pack outside it at 100 MB and 0.07 ms at the gigabyte, against a
	// publication of 0.66 s and 9.9 s — so a repository published every M
	// pushes, each of its sixteen requests a push paying M/2 packs' walk, is
	// cheapest at M = √(2T/qδ): 25 at 100 MB and 129 at the gigabyte. A
	// JUDGMENT CALL between the two: sixty-four, near their geometric middle
	// (57), costs each about a quarter to a half past its own least, where
	// either least would cost the other end nearly three times its own; past
	// a minute's build the duty cycle binds before it anyway.
	PublishPacks = 64
	// PublishShare is a tail worth a publication by its SHARE of the
	// repository's objects: the tail's objects are walked, never read off the
	// bitmap, and packed by every clone that reaches them — so a SMALL
	// repository is published once its tail holds a sixty-fourth of its
	// objects. SIXTY-FOURTH, MEASURED (share=…: a clone of main with the
	// history's last 1, 4 and 16 of its 64 pushes outside a published root):
	// a sixty-fourth added nothing a run shows to a clone at 100 MB (0.64 s
	// either way) and at the gigabyte (6.53 s against 6.60), a sixteenth a
	// quarter (0.79 against 0.64 s; 8.40 against 6.60 s), and a quarter more
	// than doubled it (1.42 s; 14.7 s) — so the largest share whose tail adds
	// at most a tenth to a clone at both sizes is the sixty-fourth. 16, the
	// provisional share this replaced, priced a tail by a count of its
	// objects (0.09 s at a sixteenth of 219 679 objects), not by the clone
	// that also packs them. A share alone does not bound a large repository's
	// tail: PublishTailObjects does.
	PublishShare = 64
	// PublishTailObjects is a tail worth a publication by its OBJECTS, the
	// ABSOLUTE bound: a tail's cost is its objects, never its share — every
	// clone, the installer's canary and the node's tip commit-graph walk every
	// object outside the chain, at most gitproc.WalkPerObject an object, plus
	// gitproc.TransferBytesPerObject of a clone's memory each — so this many
	// objects cost a clone at most a second of walk and some 21 MiB. A
	// sixty-fourth of the largest repository the defaults admit, some 60
	// million objects, is 940 000: 10 s of walk added to every clone of it,
	// while compaction keeps such a tail to a few dozen packs and PublishPacks
	// never fires there. A repository is due at whichever of the share and
	// this bound its tail reaches first. MEASURED (tail=…, at the gigabyte:
	// a quarter of it, it, and four times it outside a published root): the
	// canary walks a tail object in 2.7–3.3 µs and the tip's write in
	// 1.1–2.8, and a clone pays 11.0 µs for each, packing included (the line
	// through the three clones: 7.28, 7.83 and 11.0 s), which is
	// gitproc.WalkPerObject's own figure, so the bound stays a second over it
	// — the clone at the bound ran 1.23 s past the chained clone with no tail
	// (6.60 s) in its one run.
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
	// layer's build walks the whole history. TEN MINUTES, the larger of ten
	// minutes and ten times a publication at 5 GB, as cache.MidxMinInterval
	// is set beside its run: a later publication (step=publish/layer=tail)
	// took 0.66 s at 100 MB and 9.9 s at the gigabyte, its walk of the
	// history growing faster than the bytes, which at the gigabyte's growth
	// is about a minute at 5 GB — ten minutes within that extrapolation's
	// error, since the 5 GB arm (under CREWLET_BENCH_LARGE) has not run on
	// the machine these did. A JUDGMENT CALL on that figure, and a safe one:
	// past a minute's build PublishDutyCycle holds the repository to a tenth
	// of the maintainer's time whatever this says.
	PublishMinInterval = 10 * time.Minute
	// PublishDutyCycle holds a repository's publications to at most a tenth
	// of the time of the node that runs them: the next waits this many times
	// the last one's build, where that is longer than PublishMinInterval —
	// cache.MidxDutyCycle's reason. KEPT. EVERY BUILD this node begins is
	// held to it, the ones that do not land as well as the ones that do
	// ([Compactor.paced]): a build's time is spent whatever its record
	// becomes.
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
//     repository the pass may publish, by its maintenance's charge
//     ([Compactor.PlacedBy]), as the compaction loop judges its own: a
//     placement this node cannot read publishes nothing that pass, logged
//     once.
//  3. THE STORE'S ROOM, read once a pass, and only while a repository is held
//     back for a full store and the pass may publish
//     ([Compactor.releaseHeld]).
//  4. Every repository in that order, until the next tick would begin: one a
//     plan found nothing to publish in at its version is skipped unless it is
//     damaged; one held back for a full store is skipped until the room is
//     there; one whose last build here did not land waits out that build's
//     pace ([Compactor.paced]); a REPAIR is due at once, paced by
//     PublishMinInterval since its last publication; any other is held to
//     [Due] — each judged by its row, and by when this node first listed its
//     tail, before anything is synced; one this node does not maintain is
//     skipped; and every other is PUBLISHED ([Compactor.Publish]), one at a
//     time.
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

// maintainedOfPublications is which repositories of the pass's listing this
// node maintains, off ONE LISTING of who serves git and what each git node
// advertises, as the compaction loop judges its own ([Compactor.maintainedOf]):
// asked of every repository the pass may publish — each but one a plan found
// nothing to publish in at its version, which is no repair — each by the
// charge it is placed by ([Compactor.PlacedBy]). An error is UNKNOWN for every
// one of them, never "maintains none". The rules that turn on the clock — a
// repair's pace, [Due] — are left to the pass, which reads the clock at each
// repository, so a repository due part way through it is one it judged.
func (c *Compactor) maintainedOfPublications(listed []code.PublicationCandidate,
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
// full store, the pace of a build that did not land — so each is bounded by
// the repositories with a tail or a refused layer: one whose tail a
// publication covered, or that was deleted, is planned from its rows if it
// comes back.
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
// a PublishMinInterval since the repository's last publication — so a store
// that keeps losing what is published costs a repository one publication a
// PublishMinInterval, never one a pass. A repair this node built that did not
// land is held back by that build's pace, as every build is
// ([Compactor.paced]).
func (c *Compactor) repairDue(cand code.PublicationCandidate, now time.Time) bool {
	return cand.PublishedAt.IsZero() || now.Sub(cand.PublishedAt) >= PublishMinInterval
}

// paced reports whether this node's last build of a publication of repoID —
// one that did not land: it failed, was refused, or its record is not known
// applied — holds the repository back at now: until PublishMinInterval, or
// PublishDutyCycle times that build's length, whichever is longer, has passed
// since it began. The duty cycle [Due] holds a repository's publications to
// by its row, which only a record that applies moves.
//
// EVERY BUILD THIS NODE BEGINS IS PACED: a build that did not land moves no
// row, and the due listing is least recently published first, so without this
// the repository it failed for was the first candidate of every pass — and
// where its build ran past PublishPoll, as the largest repositories' builds
// do, the pass ended after it and the ticker's buffered tick began the next at
// once, rebuilding that one repository back to back for as long as it failed:
// every other repository the node maintains never published again, each tail
// past PublishTailObjects served unbitmapped, and a maintenance slot and up to
// the publication's whole charge spent on builds that failed. Paced, a
// repository whose publication keeps failing costs its maintainer at most a
// tenth of its time, and the pass goes on to the next.
//
// A NODE-LOCAL PACE, NEVER A MEMORY AT THE VERSION: a refusal for memory is
// no build (its admission refused it before any began) and is still never
// remembered (M3-decisions M3-O26), and a full store's hold stays its own
// ([Compactor.releaseHeld]) — a build the store refused for room waits for
// both, the room and its pace, so a reservation whose room comes and goes
// around a build's estimate never has the repository rebuilt at every pass.
// A restart forgets it, costing one build more.
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
//     node takes the repository over (M3-decisions M3-O26). Once admitted —
//     never before, so a refused attempt asks the store nothing — the base
//     the layer is written over is held to the object store's copy, its parts
//     asked at once, and a base the store lost is replaced with every layer
//     above it, that plan's charge held in the same slot in place of the
//     first's ([Compactor.admitOverItsBase]).
//  4. BUILD the repack first, where the plan has one — over a store of
//     exactly its inputs, as a compaction's is ([Compactor.repack]) — and
//     hold it to a compaction's count and bundle rules BEFORE the layer is
//     written over its outputs ([Compactor.repackChecked]); then, in a
//     scratch of its own whose chain files name EXACTLY the layers it is
//     written over (cache.Cache.NewLayerScratch), the repack's outputs
//     gathered into it ([gather]), the multi-pack-index layer with its
//     bitmap over every pack the layer covers, and its commit-graph layer
//     with every changed-path filter, each written once. Then
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
//
// EVERY BUILD IT BEGINS IS PACED until one lands: its start and its length
// are kept, and the publication pass holds the repository back by them
// whatever became of the build ([Compactor.paced]) — the duty cycle a landed
// publication's row holds it to.
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
	if err = c.fault(ctx, codefault.PublishAfterUpload, 0); err != nil {
		return c.publicationFailed(cand, "propose", err)
	}
	res, err := c.o.Estate.Compact(ctx, b.proposal)
	if err != nil {
		return c.publicationFailed(cand, "propose", err)
	}
	if err = c.fault(ctx, codefault.PublishAfterPropose, 0); err != nil {
		return c.publicationFailed(cand, "adopt", err)
	}
	return c.publicationAnswered(ctx, cand, b, res, began)
}

// buildable answers a plan that builds nothing — a layer still at this node's
// installer, `waiting` and planned again at the next pass; nothing to publish,
// remembered at the listing's version; a layer too large, refused and
// remembered — and "" for a layer to build.
func (c *Compactor) buildable(cand code.PublicationCandidate, p layerPlan, verdict layerVerdict) (Outcome, error) {
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
	return "", nil
}

// admitOverItsBase admits a publication of p to the maintenance lane
// (cache.Cache.AdmitPublication, charged at the largest of its steps'; a
// refusal is [Compactor.admissionRefused]'s) and then holds the base it is
// written over to the object store's copy ([Compactor.heldBase]), answering
// the admission, and the chain and the plan the build runs over — or a nil
// admission, and what became of the publication.
//
// NEVER BUILT OVER A BASE THE STORE LOST: this node installed its own layers
// from the files it wrote, so its copy holds a layer the store may no longer
// hold — and a layer written over it would be one no node that fetches its
// chain can install either. Such a base is replaced with every layer above it:
// the plan is made again over the layers below the loss ([lostFrom]), held to
// what a plan builds again, reserved, and its charge held in the admission's
// own slot in place of the first's (cache.Admission.RechargePublication) — a
// larger layer, or a repack its gathered packs now need, may pass the first
// plan's charge, and a release and a second admission would wake the
// maintainer between them, which could take the slot and leave the repair
// busy at every pass a busy node gives it.
//
// THE STORE IS ASKED ONCE THE ADMISSION IS GRANTED: an attempt refused busy (a
// moment's state, asked again at the next pass) or for memory (never
// remembered, asked again at every pass) asks it nothing, where it asked for
// every part of the base at every attempt before. The admission is the
// node's, in memory; the questions hold its slot for about one first byte.
func (c *Compactor) admitOverItsBase(ctx context.Context, cand code.PublicationCandidate, s cache.Synced,
	chain cache.Chain, live []code.PackRow, p layerPlan, cap int64, limits code.Limits) (*cache.Admission,
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
func (c *Compactor) admissionRefused(cand code.PublicationCandidate, err error) (Outcome, error) {
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
	}
	return c.publicationFailed(cand, "admit", err)
}

// heldBase is how many of rows[:base], from the root, the object store still
// holds every part of — base where it holds them all — or the store's error
// where it could not be read: the answer a walk of the parts in chain order
// gives, the first part the store lacks or holds at another size ending it at
// that part's layer, and the first it could not read failing it.
//
// WHY THE MAINTAINER ASKS: every other git node fetches a layer's parts and
// refuses one the store lost (`missing`), but the maintainer installs its own
// layer from the files it wrote (cache.Cache.AdoptLayer), so its copy holds a
// layer the store may have lost since — a restore, which carries no layer, or
// a bucket's own deletion — and its own copy alone would never say so. Built
// over it, a new layer is one no node fetching the chain can install either,
// so a publication is held to the store's copy of its base before anything is
// built. A part whose bytes the store changed in place is no size of another,
// and is found by the nodes that fetch it (`corrupt`), whose repair waits for
// this node's own copy to meet it.
//
// THE PARTS ARE ASKED AT ONCE, cache.FetchConcurrency at a time — the width a
// sync already holds against the same store. On S3, the production store,
// every request waits a first byte of 20 to 100 ms: asked one after another,
// a large repository's chain of ten to twenty layers, its root several parts
// of a gibibyte, cost 0.3 to 2 s an attempt, and the most a base may hold —
// code.MaxPublishedLayers layers of code.MaxLayerParts parts, 512 — up to 50 s,
// a large share of a small repository's whole publication and of a pass cut at
// PublishPoll. At once, it costs about one first byte for every sixteen parts.
// The answer is the walk's: every part up to the first that is not held or not
// read is asked to the end, and none past it is begun once that one is known.
func (c *Compactor) heldBase(ctx context.Context, rows []code.LayerRow, base int) (int, error) {
	type part struct {
		layer int
		part  code.LayerPart
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
	proposal code.CompactionProposal
}

// buildLayer builds p in a scratch of its own — its repack first, held to a
// compaction's count and bundle rules before the layer is written over its
// outputs ([Compactor.repackChecked]) — and, where git wrote the layer without
// a bitmap, planned and built ONCE more over one more layer of the chain. On
// an error the scratch is gone, and the outcome is the publication's.
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

// buildLayerChecked builds p once and holds the proposal to every rule its
// committer will. On an error the scratch is gone.
func (c *Compactor) buildLayerChecked(ctx context.Context, cand code.PublicationCandidate, s cache.Synced,
	adm *cache.Admission, chain cache.Chain, p layerPlan, cap int64, limits code.Limits) (layerBuilt, Outcome,
	error) {

	began := c.now()
	b, out, err := c.writeLayer(ctx, cand, s, adm, chain, p, cap, limits)
	if err != nil {
		return layerBuilt{}, out, err
	}
	if err = c.fault(ctx, codefault.PublishAfterBuild, 0); err != nil {
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

// writeLayer writes p's layer under a fresh publication id and lays it out into
// the publication its committer would be asked to decide: its repack first,
// where it has one, built and held to its rules in a merge's scratch of its
// own ([Compactor.repackChecked]); then the layer's scratch, the repack's
// outputs gathered into it ([gather]); then the layer over them
// ([Compactor.writeLayerIn]). On an error nothing it made is left, and the
// outcome is the publication's.
func (c *Compactor) writeLayer(ctx context.Context, cand code.PublicationCandidate, s cache.Synced,
	adm *cache.Admission, chain cache.Chain, p layerPlan, cap int64, limits code.Limits) (layerBuilt, Outcome,
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
	b.proposal = code.CompactionProposal{Compaction: code.Compaction{
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
	ref, err := code.LayLayer(ctx, b.paths[0], b.paths[1], b.paths[2], baseGraphs, code.Pieces{})
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

// repackChecked builds a publication's repack of p — the non-full packs its
// layer gathers — and holds it to a compaction's rules BEFORE THE LAYER IS
// WRITTEN over its outputs: cut as a compaction's build is
// ([bounds.cutFor]), the plan capped again at that cut's [InputByteCap]; its
// own COUNT held to what one record retires and adds — past it, nothing is
// uploaded and the repository's cap is halved; and every output to the
// BUNDLE RULE, built ONCE more at the densest offender's own ratio in a fresh
// merge's scratch, the plan capped again at that cut's byte cap, a second miss
// halving the cap too. It answers the repack that passed, whose merge's
// scratch is the caller's to remove once its outputs are gathered; on an error
// nothing it made is left, and the outcome is the publication's.
//
// THE RULES READ THE REPACK'S OUTPUTS ALONE, so they are held before the
// multi-pack-index layer and its bitmap — a walk of the repository's whole
// history, minutes and up to the publication's whole charge at the largest
// repository — and the commit-graph layer are written: held after them, an
// output past either rule threw a finished bitmap away, and the bundle rule's
// rebuild wrote the whole layer twice.
func (c *Compactor) repackChecked(ctx context.Context, cand code.PublicationCandidate, s cache.Synced, cid string,
	adm *cache.Admission, p planned, cap int64, limits code.Limits) (repacked, Outcome, error) {

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
func (c *Compactor) repackCountHolds(cand code.PublicationCandidate, rp repacked, cap int64) (Outcome, error) {
	if n := len(rp.outs); n <= code.MaxCompactOutputs && n <= len(rp.plan.ids()) {
		return "", nil
	}
	rp.ms.Remove()
	c.layerCapFailed(cand, rp.plan, cap, reasonOutputs, len(rp.outs))
	return OutcomeFailed, errOutputs
}

// repack builds a publication's repack of p — the non-full packs its layer
// gathers, at cut — in a MERGE'S SCRATCH OF ITS OWN, leaning on a store of
// exactly p's inputs (cache.Cache.NewMergeScratch) as a compaction's build
// does ([Compactor.pack]), and lays its outputs out under the publication
// cid's derived ids, holding the merge's scratch to exactly what the build
// printed ([lay]). On an error the merge's scratch is gone; otherwise it is
// the caller's.
//
// NEVER IN THE LAYER'S SCRATCH, which links every pack of the copy: there
// `--stdin-packs`, filling in its name hashes, walked from the inputs' commits
// down the repository's whole history, as a compaction's build over the
// incarnation did — about 3.5 µs an object of the history on the node pin, a
// parsed object kept for each — while the repack's charge and its deadline
// follow its inputs, so past a few million objects every publication that
// repacked ran out its repack's deadline and failed, at every pass that
// planned it.
func (c *Compactor) repack(ctx context.Context, s cache.Synced, cid string, adm *cache.Admission, p planned,
	cut int64, limits code.Limits) (repacked, error) {

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
