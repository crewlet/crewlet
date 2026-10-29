package compact

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// ExpungePoll is the interval between two passes of the EXPUNGE loop: one
// walk a git node a minute of the partial index that holds only the
// repositories with an expunge open or a purge requested
// (code_repos_expunging_idx) — a handful in a fleet of any size, so a pass's
// listing is a few rows — and so a person's purge is taken up within a minute
// of its request by the repository's maintainer, and an expunge a crash left
// open is finished from its rows within a minute of the node's return.
const ExpungePoll = time.Minute

// ExpungeInputByteCap is the most PACK bytes one expunge's begin rewrites at a
// cut of cut: code.MaxExpungeOutputs/2 cuts — 16 GiB at the nominal cut
// (code.CompactPackBytes) — [InputByteCap]'s half, for its reason: the
// builds' own count is the authority on how many outputs they wrote, any two
// consecutive outputs of one build sum past a cut, and a build that wrote
// more than one record takes is refused before any upload and halves the
// repository's next cycle ([Compactor.expungeCapFailed]). A repository whose
// packs to rewrite pass it is expunged in as many cycles as it takes, each a
// begin and a commit of its own.
func ExpungeInputByteCap(cut int64) int64 { return code.MaxExpungeOutputs / 2 * cut }

// ExpungeReport is what one pass of the expunge loop did: how many
// repositories its listing named, how many of them this node maintains, and
// how many of those expunged, were refused or failed.
type ExpungeReport struct {
	Listed, Maintained, Expunged, Refused, Failed int
}

// The stages of crewlet.code.expunge.duration.
const (
	stageScan   = "scan"
	stageBuild  = "build"
	stageUpload = "upload"
	stageBegin  = "begin"
	stageRescue = "rescue"
	stageCommit = "commit"
)

// The reasons an expunge's line gives beside the committer's refusals and the
// compaction's own (reasonMemory, reasonStoreFull, reasonOutputs,
// reasonBundleTooLarge): the repository's drop epoch moved past the begin a
// cycle made, and a copy whose objects/ still holds a begin's inputs.
const (
	reasonEpoch  = "epoch"
	reasonInputs = "inputs_leaving"
)

// errNothingKept is a build whose inputs' every object the rest holds and
// whose fallback, which keeps nothing, wrote nothing either: an input set no
// pack holds a byte of, which no correct copy is.
var errNothingKept = errors.New("compact: the expunge's inputs hold no object to keep or drop")

// ExpungePass is one pass of the EXPUNGE loop:
//
//  1. LIST every repository with an expunge open or a purge requested — the
//     `expunge_due` read, every page, by id, at STALE for the compaction
//     listing's reason (the router carries this node's own writes as a floor
//     on every code read it sends), each with its live objects;
//  2. MAINTAINERS, judged against ONE LISTING of who serves git and what each
//     git node advertises, by the PUBLICATION'S charge — the charge the other
//     two loops are placed by, so every writer of a repository's packs runs
//     on one node — a placement this node cannot read expunging nothing that
//     pass, logged once;
//  3. THE STORE'S ROOM, read once a pass, and only while a repository is held
//     back for a full store ([Compactor.releaseHeld]);
//  4. every listed repository this node maintains, by id, until the next
//     tick would begin: one held back for a full store skipped until the room
//     is there, and every other EXPUNGED ([Compactor.Expunge]) — one cycle
//     each, one at a time.
//
// A listing that fails is the pass's error. The pass is logged
// code_expunge_pass, at DEBUG unless it expunged, refused or failed something.
func (c *Compactor) ExpungePass(ctx context.Context) (ExpungeReport, error) {
	began := c.now()
	var report ExpungeReport
	listed, err := c.listExpunges(ctx)
	if err != nil {
		return report, err
	}
	report.Listed = len(listed)
	c.forgetExpunges(listed)
	maintained, unknown := c.maintainedOfExpunges(listed)
	var released map[string]bool
	if unknown != nil {
		c.log.Warn("code_expunge_maintainer_unknown", "listed", len(listed), "error", unknown,
			"detail", "who maintains the listed repositories could not be read, so this pass expunges nothing")
	} else if released, err = c.releaseHeld(ctx, expungeLoop); err != nil {
		c.log.Warn("code_expunge_store_unread", "error", err,
			"detail", "the object store's room could not be read, so every repository held back for a full "+
				"store waits for the next pass")
	}
	until := began.Add(ExpungePoll)
	for _, cand := range listed {
		if unknown != nil || ctx.Err() != nil || !c.now().Before(until) {
			break
		}
		if (c.isHeld(expungeLoop, cand.ID) && !released[cand.ID]) || !maintained[cand.ID] {
			continue
		}
		report.Maintained++
		switch out, _ := c.Expunge(ctx, cand); out {
		case OutcomeApplied, OutcomePending, OutcomeUnknown:
			report.Expunged++
		case OutcomeRefused:
			report.Refused++
		case OutcomeFailed:
			report.Failed++
		}
	}
	log := c.log.Debug
	if report.Expunged+report.Refused+report.Failed > 0 {
		log = c.log.Info
	}
	log("code_expunge_pass", "listed", report.Listed, "maintained", report.Maintained,
		"expunged", report.Expunged, "refused", report.Refused, "failed", report.Failed,
		"took", c.now().Sub(began))
	return report, nil
}

// listExpunges is every repository the `expunge_due` listing names, every
// page.
func (c *Compactor) listExpunges(ctx context.Context) ([]code.ExpungeCandidate, error) {
	q := code.ReadQuery{Kind: code.ReadExpungeDue, Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	var out []code.ExpungeCandidate
	for {
		a, err := c.o.Estate.Read(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("compact: list the repositories an expunge is owed in: %w", err)
		}
		out = append(out, a.Expunge...)
		if a.Next == "" {
			return out, nil
		}
		q.Cursor = a.Next
	}
}

// forgetExpunges drops the expunge loop's hold for a full store of a
// repository the listing no longer names — its purge served, or itself
// deleted — so the holds are bounded by the expunges owed. A halved cap is
// kept for the compactor's life, as a compaction's is: it is a fact about what
// the repository's packs make of a build.
func (c *Compactor) forgetExpunges(listed []code.ExpungeCandidate) {
	named := make(map[string]bool, len(listed))
	for _, cand := range listed {
		named[cand.ID] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.held[expungeLoop].repos {
		if !named[id] {
			delete(c.held[expungeLoop].repos, id)
		}
	}
}

// maintainedOfExpunges is which listed repositories this node maintains, off
// ONE LISTING of who serves git, each by the charge it is placed by
// ([Compactor.placedBy]) — an error is UNKNOWN for every one of them, never
// "maintains none".
func (c *Compactor) maintainedOfExpunges(listed []code.ExpungeCandidate) (map[string]bool, error) {
	if len(listed) == 0 {
		return nil, nil
	}
	ask := make(map[string]int64, len(listed))
	for _, cand := range listed {
		ask[cand.ID] = c.placedBy(cand.LiveObjects)
	}
	return c.o.Maintainers.MaintainerOf(ask)
}

// Expunge runs ONE CYCLE of an expunge of cand — a repository this node
// maintains, as the `expunge_due` listing named it — and answers what became
// of it: the unit a suite drives, and what every pass runs one at a time.
// Nothing is retried inside it; its error is the one that failed or refused
// it, nil for every other outcome.
//
// AN EXPUNGE OPEN IS FINISHED FIRST, from its rows ([Compactor.finish]): the
// listing names its id, the cache's copy at or past the begin holds its
// dropping packs, and the rescue is valid at any position at or after the
// begin — so whatever stopped the cycle that began it, a crash included, the
// next pass on whichever node maintains the repository commits it. A PURGE
// REQUESTED begins a cycle ([Compactor.cycle]) and carries it through its
// commit.
//
// NEVER BESIDE ANOTHER BUILD OF THE SAME REPOSITORY on this node — a
// compaction's or a publication's — since either could plan over a pack the
// begin retires or a layer it drops ([Compactor.claimExpunge]). The claim is
// held from the scan to the commit.
//
// THE COMPACTOR'S LIFETIME BOUNDS IT, whoever asked, as it bounds a
// compaction: [Compactor.Stop] ends the cycle in flight.
func (c *Compactor) Expunge(ctx context.Context, cand code.ExpungeCandidate) (Outcome, error) {
	c.expunging.Lock()
	defer c.expunging.Unlock()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer context.AfterFunc(c.life, func() { cancel(context.Cause(c.life)) })()
	if !c.claimExpunge(cand.ID) {
		c.log.Debug("code_expunge_busy", "repo", cand.Name,
			"detail", "a compaction or a publication of the repository is running on this node")
		c.countedExpunge(OutcomeBusy)
		return OutcomeBusy, errClaimed
	}
	defer c.unclaimExpunge(cand.ID)
	var (
		out Outcome
		err error
	)
	switch grace := expungeGrace(cand); {
	case cand.Open != "":
		out, err = c.finish(ctx, cand, openExpunge{id: cand.Open, request: cand.Request, grace: grace,
			floor: cand.Version})
	case cand.Request != "":
		out, err = c.cycle(ctx, cand, grace)
	default:
		out = OutcomeNotDue
	}
	c.countedExpunge(out)
	return out, err
}

// expungeGrace is how long before its scan a cycle of cand keeps what a ref's
// drop left: none serving a purge — a person asked for what a force-push left
// to go at once (code.ExpungeBegin.Grace) — and code.PruneGrace for an
// expunge on its maintainer's own schedule, which an open expunge with no
// request is.
//
// AN OPEN EXPUNGE IS RESCUED BY WHAT IS OWED NOW: a request asked while an
// expunge on the maintainer's own schedule was open asks for what that
// expunge's grace would keep to go too, so its rescue keeps none of it, and
// the request's own cycle judges the rest.
func expungeGrace(cand code.ExpungeCandidate) time.Duration {
	if cand.Request != "" {
		return 0
	}
	return code.PruneGrace
}

// openExpunge is an expunge whose begin has applied, as its rescue and commit
// read it: its id, the epoch its begin moved the repository to (0 where only
// the rows say it — a cycle resumed), the request it serves, the grace its
// rescue keeps a ref drop's object for, the floor its rescue's sync reads at,
// and the tips its scan kept what they reach of — none where only the rows say
// what the scan was, which costs the rescue a walk of the whole history rather
// than of what moved since.
type openExpunge struct {
	id       string
	epoch    int64
	request  string
	grace    time.Duration
	floor    statelog.Position
	scanTips []string
}

// cycle runs one expunge serving cand's purge, from its scan to its commit:
//
//  1. SCAN: sync at session, floored at the version the listing read, as a
//     compaction does — the committer holds every input to its own snapshot
//     and refuses a plan from an older one — and plan the cycle's inputs
//     ([planExpunge]): the live packs whose content dates from before the
//     request (code.PackRow.Content), oldest first, capped at
//     code.MaxExpungeInputs rows and [ExpungeInputByteCap] of the cut — FULL
//     packs and packs a published layer names among them, since what a purge
//     drops may lie in any pack; the layers over them are the begin's to drop
//     (code.LayersOver, the decide's own reading). Then the drops the grace
//     keeps (the `expunge_tips` read, at the scan's floor, since its instant
//     less the grace — none of a drop's for a purge, whose instant is now).
//  2. ADMIT the builds to the maintenance lane (cache.Cache.AdmitExpunge).
//  3. BUILD in a scratch of the inputs leaning on a store of every other live
//     pack, its refs linked from the copy (cache.Cache.StageExpunge): the KEPT
//     packs, `pack-objects --revs --local` from every tip — the copy's refs and
//     the drops the grace keeps that the scratch holds — which packs what a tip
//     reaches of the inputs and of no other pack; then the DROPPING packs,
//     `pack-objects --stdin-packs` over the inputs less the kept packs and less
//     the rest, which packs what nothing reaches. Every object of the inputs is
//     in the kept, the dropping or the rest packs, so the begin keeps every
//     object: a merge, to every reader. Held to the builds' own count and to
//     the bundle rule, rebuilt ONCE at the densest offender's ratio, as a
//     compaction's build is, and the begin to every rule its committer will
//     (code.ExpungeProposal.ValidateUnstored) — then the admission is released.
//  4. UPLOAD every output at once ([uploadAll]); a store with no room holds the
//     repository back until it has the room, as a compaction is held.
//  5. BEGIN, proposed to the repository's committer, which moves its drop
//     epoch: applied, the kept packs are adopted into this node's cache — so
//     the node that built them downloads none — and the cycle FINISHES
//     ([Compactor.finish]) with the tips its scan kept, which bound its
//     rescue's walk.
func (c *Compactor) cycle(ctx context.Context, cand code.ExpungeCandidate, grace time.Duration) (Outcome, error) {
	began := c.now()
	s, err := c.o.Cache.Sync(ctx, cand.ID, statelog.FloorAt(statelog.ReadSession, cand.Version))
	if err != nil {
		return c.expungeFailed(cand, "sync", err)
	}
	released := false
	defer func() {
		if !released {
			s.Release()
		}
	}()
	cap := c.expungeCapFor(cand.ID)
	p := planExpunge(c.o.Cache.Packs(s), cand.Requested, cap)
	if len(p.inputs) == 0 {
		// THE REQUEST IS SERVED, or about to be: no live pack dates from
		// before it, so the listing that named it read a row a commit has
		// since moved.
		c.log.Debug("code_expunge_not_due", "repo", cand.Name, "request", cand.Request,
			"detail", "no live pack dates from before the request")
		return OutcomeNotDue, nil
	}
	cut := c.bounds.cutFor(p)
	if at := ExpungeInputByteCap(cut); at < cap {
		p, cap = p.recapExpunge(at), at
	}
	drops, err := c.dropTips(ctx, cand.ID, s.Position, c.now().Add(-grace))
	if err != nil {
		return c.expungeFailed(cand, "scan", err)
	}
	c.observeStage(stageScan, began)

	buildBegan := c.now()
	limits := c.o.Limits()
	adm, err := c.o.Cache.AdmitExpunge(s, p.objects(), limits)
	switch {
	case errors.Is(err, gitproc.ErrBusy):
		c.log.Debug("code_expunge_busy", "repo", cand.Name, "error", err)
		return OutcomeBusy, err
	case errors.Is(err, cache.ErrNoMemory):
		// NEVER REMEMBERED: the node's budget is read again at every pass,
		// and the next pass asks whether this node is still the maintainer.
		// The cache logged it once for the incarnation.
		c.log.Warn("code_expunge_refused", "repo", cand.Name, "reason", reasonMemory, "error", err)
		return OutcomeRefused, err
	case err != nil:
		return c.expungeFailed(cand, "admit", err)
	}
	defer adm.Release()
	b, out, err := c.buildExpunge(ctx, cand, s, adm, p, cut, cap, grace, drops, limits)
	if err != nil {
		return out, err
	}
	defer b.sc.Remove()
	adm.Release()
	c.observeStage(stageBuild, buildBegan)
	if err = c.fault(ctx, codefault.ExpungeAfterBuild, 0); err != nil {
		return c.expungeFailed(cand, "build", err)
	}

	uploadBegan := c.now()
	outs := slices.Concat(b.kept, b.dropping)
	if out, err = c.uploadExpunge(ctx, cand, b.begin.ExpungeID, outs, p.size()); err != nil {
		return out, err
	}
	// THE STORED REFS, each now naming its object: the kept first, then the
	// dropping, as the build laid them out and the begin names them.
	b.kept, b.dropping = outs[:len(b.kept)], outs[len(b.kept):]
	b.begin.Add, b.begin.Dropping = refs(b.kept), refs(b.dropping)
	c.observeStage(stageUpload, uploadBegan)

	beginBegan := c.now()
	res, err := c.o.Estate.Expunge(ctx, code.ExpungeProposal{Begin: &b.begin})
	if err != nil {
		return c.expungeFailed(cand, "begin", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		return c.expungeAnswered(cand, b.begin.ExpungeID, "begin", res)
	}
	if len(b.kept) > 0 {
		// THE KEPT PACKS ARE PRESENT AT ONCE, linked from the scratch: they
		// are live, as a compaction's outputs are. The dropping packs are no
		// adoption's — linked into objects/pack they would be packs a push
		// could rely on — and reach the copy's dropping view by the rescue's
		// sync, as every git node's copy gets them.
		if err := c.o.Cache.Adopt(ctx, cand.ID, b.sc, refs(b.kept)...); err != nil {
			c.log.Warn("code_expunge_adopt_failed", "repo", cand.Name, "expunge", b.begin.ExpungeID, "error", err)
		}
	}
	c.log.Info("code_expunge_begun", "repo", cand.Name, "expunge", b.begin.ExpungeID, "request", cand.Request,
		"inputs", len(p.inputs), "kept", len(b.kept), "dropping", len(b.dropping),
		"dropped_layers", len(b.begin.DropLayers), "epoch", b.begin.DropEpoch, "position", res.Position.String())
	c.observeStage(stageBegin, beginBegan)
	if err := c.fault(ctx, codefault.ExpungeAfterBegin, 0); err != nil {
		return c.expungeFailed(cand, "rescue", err)
	}
	b.sc.Remove()
	s.Release()
	released = true
	return c.finish(ctx, cand, openExpunge{id: b.begin.ExpungeID, epoch: b.begin.DropEpoch,
		request: cand.Request, grace: grace, floor: res.Position, scanTips: b.tips})
}

// expungeBuilt is one build of an expunge's begin, laid out and checked: the
// scratch that holds its outputs, its kept and its dropping packs, the tips
// it kept what they reach of, and the begin that names them.
type expungeBuilt struct {
	sc             *cache.Scratch
	kept, dropping []output
	tips           []string
	begin          code.ExpungeBegin
}

// buildExpunge builds p at cut ([Compactor.sortExpunge]) and holds the result
// to the builds' own count and to the bundle rule — rebuilding ONCE, in a
// scratch of its own under an expunge id of its own, at the densest
// offender's ratio, the inputs capped again at that cut's
// [ExpungeInputByteCap] — and then to every rule of the begin. cap is the pack
// bytes p was capped at, which a failure halves. On an error the scratch is
// gone, and the outcome is the expunge's.
func (c *Compactor) buildExpunge(ctx context.Context, cand code.ExpungeCandidate, s cache.Synced,
	adm *cache.Admission, p planned, cut, cap int64, grace time.Duration, drops []string,
	limits code.Limits) (expungeBuilt, Outcome, error) {

	b, err := c.sortExpunge(ctx, s, adm, p, cut, drops, limits)
	if err != nil {
		return expungeBuilt{}, c.expungeFailedOnly(cand, "build", err), err
	}
	if out, holdErr := c.expungeCountHolds(cand, b, p, cap); holdErr != nil {
		return expungeBuilt{}, out, holdErr
	}
	if ratio := pastBound(slices.Concat(b.kept, b.dropping), c.bounds.maxBundle); ratio > 0 {
		b.sc.Remove()
		cut = int64(float64(c.bounds.cut) / ratio)
		if at := ExpungeInputByteCap(cut); at < cap {
			p, cap = p.recapExpunge(at), at
		}
		if b, err = c.sortExpunge(ctx, s, adm, p, cut, drops, limits); err != nil {
			return expungeBuilt{}, c.expungeFailedOnly(cand, "build", err), err
		}
		if out, holdErr := c.expungeCountHolds(cand, b, p, cap); holdErr != nil {
			return expungeBuilt{}, out, holdErr
		}
		if pastBound(slices.Concat(b.kept, b.dropping), c.bounds.maxBundle) > 0 {
			b.sc.Remove()
			c.expungeCapFailed(cand, p, cap, reasonBundleTooLarge, len(b.kept)+len(b.dropping))
			return expungeBuilt{}, OutcomeFailed, errBundleTooLarge
		}
	}
	retiring := make(map[string]bool, len(p.inputs))
	for _, in := range p.inputs {
		retiring[in.checksum] = true
	}
	b.begin = code.ExpungeBegin{RepoID: cand.ID, ExpungeID: b.begin.ExpungeID, RequestID: cand.Request,
		Node: c.o.NodeID, Seen: s.Position, DropEpoch: s.Repo.DropEpoch + 1, Grace: grace, Retire: p.ids(),
		Add: refs(b.kept), Dropping: refs(b.dropping),
		DropLayers: code.LayersOver(c.o.Cache.Chain(s).Rows, retiring)}
	if err := (code.ExpungeProposal{Begin: &b.begin}).ValidateUnstored(); err != nil {
		// THE BUILDER'S OWN BUG — every rule above passed and one of its
		// committer's did not — refused before any byte is stored.
		b.sc.Remove()
		c.log.Error("code_expunge_invalid", "repo", cand.Name, "expunge", b.begin.ExpungeID, "error", err)
		return expungeBuilt{}, OutcomeFailed, err
	}
	return b, "", nil
}

// sortExpunge is one build of p's begin at cut, in a scratch made for it
// under an expunge id minted for it: its tips — the copy's refs as the
// scratch linked them, and every drop of drops the scratch holds — its kept
// packs and its dropping packs, laid out in that order under the id's
// derivation, the kept first. On an error the scratch is removed.
//
// AN INPUT SET THE REST HOLDS WHOLE writes neither kept nor dropping packs —
// every object of the inputs lies in another live pack too — and a begin
// writes at least one output: so the dropping build is made again over the
// inputs less the kept packs alone, every object of it held live by the rest
// as well, which the commit then retires and nothing reaches past it.
func (c *Compactor) sortExpunge(ctx context.Context, s cache.Synced, adm *cache.Admission, p planned, cut int64,
	drops []string, limits code.Limits) (expungeBuilt, error) {

	xid := statelog.NewOpID(c.now(), "")
	sc, err := c.o.Cache.StageExpunge(ctx, s, xid, p.checksums())
	if err != nil {
		return expungeBuilt{}, err
	}
	b, err := c.sortIn(ctx, s, sc, xid, adm, p, cut, drops, limits)
	if err != nil {
		sc.Remove()
		return expungeBuilt{}, err
	}
	return b, nil
}

// sortIn is sortExpunge's build in sc.
func (c *Compactor) sortIn(ctx context.Context, s cache.Synced, sc *cache.Scratch, xid string,
	adm *cache.Admission, p planned, cut int64, drops []string, limits code.Limits) (expungeBuilt, error) {

	refTips, err := sc.RefTips()
	if err != nil {
		return expungeBuilt{}, err
	}
	held, err := c.present(ctx, adm, sc, drops)
	if err != nil {
		return expungeBuilt{}, err
	}
	tips := union(refTips, held)
	var keep strings.Builder
	for _, oid := range tips {
		keep.WriteString(oid + "\n")
	}
	kept, err := c.packOut(ctx, adm, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassMaintenance,
		Args: []string{"pack-objects", "--revs", "--local", "--non-empty",
			"--max-pack-size=" + strconv.FormatInt(cut, 10), "--delta-base-offset", "--quiet", "objects/pack/pack"},
		Stdin:  strings.NewReader(keep.String()),
		Config: CompactConfig(limits.MaxBlobBytes),
		// THE WALK FROM EVERY TIP reads the repository's whole history,
		// whatever few packs the inputs are: a deadline sized by them alone
		// would cut a large repository's walk short.
		Deadline: gitproc.PackWorkDeadline(p.packBytes()) + gitproc.HistoryWalk(s.Repo.LiveObjects),
	})
	if err != nil {
		return expungeBuilt{}, fmt.Errorf("compact: pack what a ref reaches of %d packs: %w", len(p.inputs), err)
	}
	rest, err := sc.StorePacks()
	if err != nil {
		return expungeBuilt{}, err
	}
	dropping, err := c.packDropping(ctx, adm, sc, p, kept, rest, cut, limits)
	if err != nil {
		return expungeBuilt{}, err
	}
	if len(kept)+len(dropping) == 0 {
		if dropping, err = c.packDropping(ctx, adm, sc, p, nil, nil, cut, limits); err != nil {
			return expungeBuilt{}, err
		}
		if len(dropping) == 0 {
			return expungeBuilt{}, errNothingKept
		}
	}
	b := expungeBuilt{sc: sc, tips: tips, begin: code.ExpungeBegin{ExpungeID: xid}}
	if b.kept, err = layOutputs(sc, xid, 0, kept); err != nil {
		return expungeBuilt{}, err
	}
	if b.dropping, err = layOutputs(sc, xid, len(kept), dropping); err != nil {
		return expungeBuilt{}, err
	}
	return b, nil
}

// packDropping packs p's inputs less the packs excluded — the kept packs and
// the rest, both excluded, so the name-hash walk `--stdin-packs` makes stops
// at their objects rather than running the history (doc.go's NOTHING BUT THE
// INPUTS IN REACH) — and answers what it printed.
func (c *Compactor) packDropping(ctx context.Context, adm *cache.Admission, sc *cache.Scratch, p planned,
	kept, rest []string, cut int64, limits code.Limits) ([]string, error) {

	var list strings.Builder
	for _, in := range p.inputs {
		list.WriteString("pack-" + in.checksum + ".pack\n")
	}
	for _, cs := range slices.Concat(kept, rest) {
		list.WriteString("^pack-" + cs + ".pack\n")
	}
	printed, err := c.packOut(ctx, adm, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassMaintenance,
		Args: []string{"pack-objects", "--stdin-packs", "--non-empty",
			"--max-pack-size=" + strconv.FormatInt(cut, 10), "--delta-base-offset", "--quiet", "objects/pack/pack"},
		Stdin:    strings.NewReader(list.String()),
		Config:   CompactConfig(limits.MaxBlobBytes),
		Deadline: gitproc.PackWorkDeadline(p.packBytes()),
	})
	if err != nil {
		return nil, fmt.Errorf("compact: pack what no ref reaches of %d packs: %w", len(p.inputs), err)
	}
	return printed, nil
}

// packOut runs one pack-objects of an expunge in the admission's reserved slot
// and answers the checksums it printed, one an output, in the order it wrote
// them — none where `--non-empty` found nothing to pack.
func (c *Compactor) packOut(ctx context.Context, adm *cache.Admission, cmd gitproc.Cmd) ([]string, error) {
	res, err := adm.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	var printed []string
	lines := bufio.NewScanner(bytes.NewReader(res.Stdout))
	for lines.Scan() {
		cs := strings.TrimSpace(lines.Text())
		if !gitref.ValidOID(cs) {
			return nil, fmt.Errorf("compact: pack-objects printed %q where it names a pack it wrote", cs)
		}
		printed = append(printed, cs)
	}
	return printed, lines.Err()
}

// present is every one of oids the scratch holds, in their order: ONE
// `cat-file --batch-check` over them. A drop's object a purge has already
// taken out of the repository is no tip — a walk from it would refuse the
// whole build — and nothing else it reached is either: what it reached that a
// ref still reaches, the ref keeps.
func (c *Compactor) present(ctx context.Context, adm *cache.Admission, sc *cache.Scratch,
	oids []string) ([]string, error) {

	if len(oids) == 0 {
		return nil, nil
	}
	var in strings.Builder
	for _, oid := range oids {
		in.WriteString(oid + "\n")
	}
	res, err := adm.Run(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassMaintenance,
		Args:     []string{"cat-file", "--batch-check=%(objectname)"},
		Stdin:    strings.NewReader(in.String()),
		Deadline: gitproc.ReadDeadline + gitproc.HistoryWalk(int64(len(oids))),
	})
	if err != nil {
		return nil, fmt.Errorf("compact: read which of %d dropped objects the copy holds: %w", len(oids), err)
	}
	var out []string
	lines := bufio.NewScanner(bytes.NewReader(res.Stdout))
	for lines.Scan() {
		line := lines.Text()
		if oid, missing := strings.CutSuffix(line, " missing"); missing {
			if !gitref.ValidOID(oid) {
				return nil, fmt.Errorf("compact: cat-file answered %q for a dropped object", line)
			}
			continue
		}
		if !gitref.ValidOID(line) {
			return nil, fmt.Errorf("compact: cat-file answered %q for a dropped object", line)
		}
		out = append(out, line)
	}
	return out, lines.Err()
}

// layOutputs lays out the packs a build printed, in the order it printed them,
// under the rewrite id's derived ids from the from-th on: each output's
// sections' sizes from its files and its object count from its index
// (code.LayBundle). Every one it printed must be a pack the scratch holds.
func layOutputs(sc *cache.Scratch, id string, from int, printed []string) ([]output, error) {
	outs := make([]output, len(printed))
	for n, cs := range printed {
		path := filepath.Join(sc.Dir, "objects", "pack", "pack-"+cs+".pack")
		ref, err := code.LayBundle(code.DeriveOutputPackID(id, from+n), path, cs)
		if err != nil {
			return nil, fmt.Errorf("compact: lay out output %d: %w", from+n, err)
		}
		outs[n] = output{path: path, ref: *ref}
	}
	return outs, nil
}

// expungeCountHolds holds a begin's build to its own count — the kept and
// dropping packs together at most what one begin records
// (code.MaxExpungeOutputs) — and past it nothing is uploaded, the scratch is
// removed and the repository's cap is halved.
func (c *Compactor) expungeCountHolds(cand code.ExpungeCandidate, b expungeBuilt, p planned,
	cap int64) (Outcome, error) {

	n := len(b.kept) + len(b.dropping)
	if n <= code.MaxExpungeOutputs {
		return "", nil
	}
	b.sc.Remove()
	c.expungeCapFailed(cand, p, cap, reasonOutputs, n)
	return OutcomeFailed, errOutputs
}

// expungeCapFor is the pack bytes repoID's next cycle is capped at: its
// halved cap where a build of it wrote too many outputs or too large a bundle,
// else [ExpungeInputByteCap] of the nominal cut.
func (c *Compactor) expungeCapFor(repoID string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cap, ok := c.expungeCaps[repoID]; ok {
		return cap
	}
	return ExpungeInputByteCap(c.bounds.cut)
}

// expungeCapFailed halves cand's cap after a build of p, capped at cap, failed
// for reason — half what the build rewrote, floored at its lightest input,
// since one pack is a cycle — and logs it.
func (c *Compactor) expungeCapFailed(cand code.ExpungeCandidate, p planned, cap int64, reason string,
	outputs int) {

	floor := p.inputs[0].packBytes
	was := min(cap, p.packBytes())
	c.mu.Lock()
	c.expungeCaps[cand.ID] = max(was/2, floor)
	next := c.expungeCaps[cand.ID]
	c.mu.Unlock()
	c.log.Warn("code_expunge_failed", "repo", cand.Name, "step", "build", "reason", reason,
		"inputs", len(p.inputs), "outputs", outputs, "most_outputs", code.MaxExpungeOutputs,
		"input_bytes", p.packBytes(), "next_cap", next, "at_floor", was <= floor)
}

// uploadExpunge stores every output of a build at once, as a compaction's are
// stored. A store with no room holds the repository back until it has room —
// estimate the bytes the build would store — and refuses the expunge; any
// other failure is the next pass's.
func (c *Compactor) uploadExpunge(ctx context.Context, cand code.ExpungeCandidate, id string, outs []output,
	estimate int64) (Outcome, error) {

	uploads := make([]func(context.Context) error, len(outs))
	for i := range outs {
		o := &outs[i]
		uploads[i] = func(ctx context.Context) error { return code.StoreBundle(ctx, c.o.Objects, o.path, &o.ref, nil) }
	}
	err := uploadAll(ctx, uploads)
	if full, ok := storeFull(err); ok {
		c.holdFull(expungeLoop, cand.ID, estimate)
		c.log.Warn("code_expunge_refused", "repo", cand.Name, "expunge", id, "reason", reasonStoreFull,
			"bytes", estimate, "error", err,
			"detail", fmt.Sprintf("the object store has no room for this expunge's packs; it is built again "+
				"once %s", full.Remedy))
		return OutcomeRefused, err
	}
	if err != nil {
		var bad *code.BadBundle
		if errors.As(err, &bad) {
			c.log.Error("code_expunge_rejected", "repo", cand.Name, "expunge", id, "object", bad.Object.String(),
				"error", bad.Err)
		}
		return c.expungeFailed(cand, "upload", err)
	}
	c.stored(ctx, expungeLoop, cand.ID)
	return "", nil
}

// finish rescues and commits the expunge open on cand's repository, from the
// rows alone where the cycle that began it is not the one asking:
//
//  1. SYNC at session, floored at the begin's position — or at the version
//     the listing read where the expunge was found open — so the copy holds
//     every dropping pack the begin left: refused busy where the copy's
//     objects/ still holds the begin's inputs, which would hide from the
//     rescue every object it must save (cache.ErrInputsLeaving).
//  2. RESCUE, where any pack is dropping, in a scratch of the dropping packs
//     leaning on the copy's live packs, its refs linked from the copy
//     (cache.Cache.StageRescue): `pack-objects --revs --local` from every ref
//     and every drop the grace keeps, `--not` the tips the scan kept where the
//     cycle still holds them — what a tip reaches of the dropping packs alone,
//     which only a push landed between the scan and the begin can reach.
//     VALID AT ANY POSITION AT OR AFTER THE BEGIN: a push checked after the
//     begin relies on no dropping object — its receiver's view leaves them
//     out — and one checked before it is refused `expunging` (the drop
//     epoch), so nothing that lands after the begin reaches a dropping object
//     the refs at the rescue's sync did not; and so a crash anywhere resumes
//     from the rows, the scan's tips unknown and the walk the whole history's.
//  3. COMMIT, proposed to the repository's committer: the dropping packs, every
//     one, retired into code_packs_retired for code.RetireGrace, the rescue
//     added live — its one precondition its expunge, open at its epoch, so no
//     push starves it — and, applied, the rescue adopted.
func (c *Compactor) finish(ctx context.Context, cand code.ExpungeCandidate, open openExpunge) (Outcome, error) {
	began := c.now()
	s, err := c.o.Cache.Sync(ctx, cand.ID, statelog.FloorAt(statelog.ReadSession, open.floor))
	if err != nil {
		return c.expungeFailed(cand, "sync", err)
	}
	defer s.Release()
	epoch := s.Repo.DropEpoch
	switch {
	case open.epoch != 0 && epoch != open.epoch:
		c.log.Debug("code_expunge_stale", "repo", cand.Name, "expunge", open.id, "reason", reasonEpoch,
			"epoch", epoch, "want", open.epoch)
		return OutcomeStale, fmt.Errorf("compact: %s is at drop epoch %d and expunge %s began %d", cand.Name,
			epoch, open.id, open.epoch)
	case s.DropEpoch != epoch:
		c.log.Debug("code_expunge_busy", "repo", cand.Name, "expunge", open.id, "reason", reasonInputs,
			"detail", "the copy's objects/ still holds the inputs of the begin; the next pass syncs again")
		return OutcomeBusy, cache.ErrInputsLeaving
	}
	dropping := c.o.Cache.DroppingPacks(s)
	commit := code.ExpungeCommit{RepoID: cand.ID, ExpungeID: open.id, CommitID: statelog.NewOpID(c.now(), ""),
		RequestID: open.request, Node: c.o.NodeID, Seen: s.Position, DropEpoch: epoch}
	var droppedBytes, droppingObjects int64
	for _, p := range dropping {
		commit.Retire = append(commit.Retire, p.PackID)
		droppedBytes += p.Size
		droppingObjects += p.Objects
	}
	rescue, sc, out, err := c.rescue(ctx, cand, s, open, commit.CommitID, droppingObjects, dropping)
	if err != nil {
		return out, err
	}
	if sc != nil {
		defer sc.Remove()
	}
	commit.Add = refs(rescue)
	if err = (code.ExpungeProposal{Commit: &commit}).ValidateUnstored(); err != nil {
		c.log.Error("code_expunge_invalid", "repo", cand.Name, "expunge", open.id, "commit", commit.CommitID,
			"error", err)
		return OutcomeFailed, err
	}
	if out, err = c.uploadExpunge(ctx, cand, open.id, rescue, outputBytes(rescue)); err != nil {
		return out, err
	}
	for i, o := range rescue {
		commit.Add[i] = o.ref
	}
	if err = c.fault(ctx, codefault.ExpungeAfterRescueUpload, 0); err != nil {
		return c.expungeFailed(cand, "commit", err)
	}
	c.observeStage(stageRescue, began)

	commitBegan := c.now()
	res, err := c.o.Estate.Expunge(ctx, code.ExpungeProposal{Commit: &commit})
	if err != nil {
		return c.expungeFailed(cand, "commit", err)
	}
	if err := c.fault(ctx, codefault.ExpungeAfterCommit, 0); err != nil {
		return c.expungeFailed(cand, "adopt", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		return c.expungeAnswered(cand, open.id, "commit", res)
	}
	if len(rescue) > 0 {
		if err := c.o.Cache.Adopt(ctx, cand.ID, sc, commit.Add...); err != nil {
			c.log.Warn("code_expunge_adopt_failed", "repo", cand.Name, "expunge", open.id, "error", err)
		}
	}
	dropped := max(droppedBytes-outputBytes(rescue), 0)
	if c.o.Metrics != nil {
		c.o.Metrics.Add(metrics.CodeExpungeDroppedBytes, uint64(dropped), nil)
	}
	c.observeStage(stageCommit, commitBegan)
	c.log.Info("code_expunge_committed", "repo", cand.Name, "expunge", open.id, "request", open.request,
		"retired", len(commit.Retire), "rescued", len(rescue), "dropped_bytes", dropped,
		"position", res.Position.String(), "resumed", open.scanTips == nil)
	return OutcomeApplied, nil
}

// rescue builds the rescue of the expunge open on s's repository, where any
// pack is dropping ([Compactor.finish], step 2), and answers its outputs and
// the scratch that holds them — none of either where nothing is dropping, or
// where nothing dropping is reached. On an error the scratch is gone, and the
// outcome is the expunge's.
func (c *Compactor) rescue(ctx context.Context, cand code.ExpungeCandidate, s cache.Synced, open openExpunge,
	cid string, objects int64, dropping []code.PackRow) ([]output, *cache.Scratch, Outcome, error) {

	if len(dropping) == 0 {
		return nil, nil, "", nil
	}
	drops, err := c.dropTips(ctx, cand.ID, s.Position, c.now().Add(-open.grace))
	if err != nil {
		return nil, nil, c.expungeFailedOnly(cand, "rescue", err), err
	}
	limits := c.o.Limits()
	adm, err := c.o.Cache.AdmitExpunge(s, objects, limits)
	switch {
	case errors.Is(err, gitproc.ErrBusy):
		c.log.Debug("code_expunge_busy", "repo", cand.Name, "error", err)
		return nil, nil, OutcomeBusy, err
	case errors.Is(err, cache.ErrNoMemory):
		c.log.Warn("code_expunge_refused", "repo", cand.Name, "reason", reasonMemory, "error", err)
		return nil, nil, OutcomeRefused, err
	case err != nil:
		return nil, nil, c.expungeFailedOnly(cand, "admit", err), err
	}
	defer adm.Release()
	sc, err := c.o.Cache.StageRescue(ctx, s, cid)
	if errors.Is(err, cache.ErrInputsLeaving) {
		return nil, nil, OutcomeBusy, err
	}
	if err != nil {
		return nil, nil, c.expungeFailedOnly(cand, "rescue", err), err
	}
	var packBytes int64
	for _, p := range dropping {
		packBytes += p.Ref().PackBytes()
	}
	cut := c.bounds.cutFor(planned{inputs: rescueInputs(dropping)})
	outs, err := c.rescueIn(ctx, s, sc, adm, open, cid, cut, packBytes, drops, limits)
	if err == nil && pastBound(outs, c.bounds.maxBundle) > 0 {
		// ONE REBUILD, at the densest offender's ratio, in a scratch of its
		// own: the first's outputs are its files.
		ratio := pastBound(outs, c.bounds.maxBundle)
		sc.Remove()
		if sc, err = c.o.Cache.StageRescue(ctx, s, cid); err == nil {
			outs, err = c.rescueIn(ctx, s, sc, adm, open, cid, int64(float64(c.bounds.cut)/ratio), packBytes, drops,
				limits)
		}
		if err == nil && pastBound(outs, c.bounds.maxBundle) > 0 {
			err = errBundleTooLarge
		}
	}
	if err == nil && len(outs) > code.MaxExpungeOutputs {
		err = errOutputs
	}
	if err != nil {
		if sc != nil {
			sc.Remove()
		}
		return nil, nil, c.expungeFailedOnly(cand, "rescue", err), err
	}
	adm.Release()
	return outs, sc, "", nil
}

// rescueIn is one build of a rescue in sc at cut: its tips — the copy's refs
// as the scratch linked them and every drop of drops the copy holds — `--not`
// the scan's tips where the cycle holds them, laid out under cid's
// derivation.
func (c *Compactor) rescueIn(ctx context.Context, s cache.Synced, sc *cache.Scratch, adm *cache.Admission,
	open openExpunge, cid string, cut, packBytes int64, drops []string, limits code.Limits) ([]output, error) {

	refTips, err := sc.RefTips()
	if err != nil {
		return nil, err
	}
	// THE SCAN'S TIPS ARE HELD TO THE SCRATCH TOO: each is in a live pack —
	// what a scan kept is — but a `--not` naming an object the scratch lacks
	// would refuse the build, and an existence check costs a lookup a tip.
	held, err := c.present(ctx, adm, sc, slices.Concat(drops, open.scanTips))
	if err != nil {
		return nil, err
	}
	scanned := map[string]bool{}
	for _, oid := range open.scanTips {
		scanned[oid] = true
	}
	var tips, not []string
	for _, oid := range held {
		if scanned[oid] {
			not = append(not, oid)
		} else {
			tips = append(tips, oid)
		}
	}
	var in strings.Builder
	for _, oid := range union(refTips, tips) {
		in.WriteString(oid + "\n")
	}
	if len(not) > 0 {
		in.WriteString("--not\n")
		for _, oid := range union(nil, not) {
			in.WriteString(oid + "\n")
		}
	}
	printed, err := c.packOut(ctx, adm, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassMaintenance,
		Args: []string{"pack-objects", "--revs", "--local", "--non-empty",
			"--max-pack-size=" + strconv.FormatInt(cut, 10), "--delta-base-offset", "--quiet", "objects/pack/pack"},
		Stdin:  strings.NewReader(in.String()),
		Config: CompactConfig(limits.MaxBlobBytes),
		// THE WALK IS THE WHOLE HISTORY's where the scan's tips are not
		// known — a cycle resumed — and what moved since the scan otherwise.
		Deadline: gitproc.PackWorkDeadline(packBytes) + gitproc.HistoryWalk(s.Repo.LiveObjects),
	})
	if err != nil {
		return nil, fmt.Errorf("compact: pack what a ref reaches of the dropping packs: %w", err)
	}
	return layOutputs(sc, cid, 0, printed)
}

// rescueInputs is the dropping rows as a plan's packs: what the rescue's cut
// is scaled by, as a build's is by its inputs ([bounds.cutFor]).
func rescueInputs(dropping []code.PackRow) []pack {
	out := make([]pack, len(dropping))
	for i, p := range dropping {
		out[i] = pack{checksum: p.GitChecksum, ids: []string{p.PackID}, objects: p.Objects,
			packBytes: p.Ref().PackBytes(), size: p.Size}
	}
	return out
}

// dropTips is every object a ref drop of repoID left since since — the
// `expunge_tips` read, every page, at session floored at at, so the drops of
// every record the copy it is read beside holds are in it — each once.
func (c *Compactor) dropTips(ctx context.Context, repoID string, at statelog.Position,
	since time.Time) ([]string, error) {

	q := code.ReadQuery{Kind: code.ReadExpungeTips, ExpungeTips: &code.ExpungeTipsQuery{RepoID: repoID, Since: since},
		Fresh: statelog.FloorAt(statelog.ReadSession, at)}
	var out []string
	for {
		a, err := c.o.Estate.Read(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("compact: read the objects %s's ref drops left: %w", repoID, err)
		}
		out = append(out, a.Tips...)
		if a.Next == "" {
			return union(nil, out), nil
		}
		q.Cursor = a.Next
	}
}

// union is every object id of a and b, each once, sorted.
func union(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

// expungeAnswered is what the committer's answer makes of a begin or a commit
// that did not apply.
func (c *Compactor) expungeAnswered(cand code.ExpungeCandidate, id, record string,
	res code.ExpungeResult) (Outcome, error) {

	switch res.Outcome {
	case statelog.OutcomePending:
		// The next pass's listing reads the rows as they are: a begin that
		// applies is an expunge open, which the next pass finishes.
		return OutcomePending, nil
	case statelog.OutcomeUnknown:
		return OutcomeUnknown, nil
	}
	refusal := fmt.Errorf("compact: the %s of expunge %s of %s refused %s: %s", record, id, cand.Name,
		res.Refusal, res.Detail)
	switch res.Refusal {
	case code.RefusalStale:
		c.log.Debug("code_expunge_stale", "repo", cand.Name, "expunge", id, "record", record, "detail", res.Detail)
		return OutcomeStale, refusal
	case code.RefusalBusy:
		c.log.Debug("code_expunge_busy", "repo", cand.Name, "expunge", id, "record", record, "detail", res.Detail)
		return OutcomeBusy, refusal
	case code.RefusalUnavailable:
		return c.expungeFailed(cand, record, refusal)
	}
	c.log.Warn("code_expunge_refused", "repo", cand.Name, "expunge", id, "record", record,
		"reason", string(res.Refusal), "detail", res.Detail)
	return OutcomeRefused, refusal
}

// expungeFailed logs an expunge that failed at step and answers it failed.
func (c *Compactor) expungeFailed(cand code.ExpungeCandidate, step string, err error) (Outcome, error) {
	return c.expungeFailedOnly(cand, step, err), err
}

// expungeFailedOnly logs an expunge that failed at step: failed.
func (c *Compactor) expungeFailedOnly(cand code.ExpungeCandidate, step string, err error) Outcome {
	c.log.Warn("code_expunge_failed", "repo", cand.Name, "step", step, "error", err)
	return OutcomeFailed
}

// countedExpunge records one expunge's outcome.
func (c *Compactor) countedExpunge(o Outcome) {
	if c.o.Metrics != nil {
		c.o.Metrics.Add(metrics.CodeExpungeRuns, 1, metrics.Attrs{"outcome": string(o)})
	}
}

// observeStage records how long one stage of an expunge took, from began.
func (c *Compactor) observeStage(stage string, began time.Time) {
	if c.o.Metrics != nil {
		c.o.Metrics.Observe(metrics.CodeExpungeDuration, c.now().Sub(began), metrics.Attrs{"stage": stage})
	}
}

// planExpunge plans one cycle of a purge requested at requested over a
// repository whose live rows are live, its inputs capped at byteCap pack
// bytes — PURE in those: every FILE (a git checksum, all of its rows
// together, as a compaction's plan weighs one) one of whose rows dates its
// content from before the request (code.PackRow.Content: zero, a pack this
// node adopted whose row no round has served yet, is read as older than
// every request, so a pack whose date is not known is rewritten rather than
// passed over), oldest content first, the lowest row id breaking a tie, and
// capped at code.MaxExpungeInputs row ids and at byteCap pack bytes — never
// below one file, since one pack rewritten is a cycle. A FULL pack and a pack
// a published layer names are inputs like any other: what a purge drops may
// lie in any pack, and an expunge is the one rewrite that takes them.
func planExpunge(live []code.PackRow, requested statelog.Position, byteCap int64) planned {
	type file struct {
		pack
		content int64
	}
	files := map[string]*file{}
	var order []*file
	for _, row := range live {
		f := files[row.GitChecksum]
		if f == nil {
			f = &file{pack: pack{checksum: row.GitChecksum, objects: row.Objects, packBytes: row.Ref().PackBytes(),
				size: row.Size}, content: row.Content.Packed()}
			files[row.GitChecksum] = f
			order = append(order, f)
		}
		f.ids = append(f.ids, row.PackID)
		f.content = min(f.content, row.Content.Packed())
	}
	var due []*file
	for _, f := range order {
		slices.Sort(f.ids)
		if f.content < requested.Packed() {
			due = append(due, f)
		}
	}
	slices.SortFunc(due, func(a, b *file) int {
		return cmp.Or(cmp.Compare(a.content, b.content), strings.Compare(a.ids[0], b.ids[0]))
	})
	var out planned
	var ids int
	var packBytes int64
	for _, f := range due {
		if ids+len(f.ids) > code.MaxExpungeInputs {
			continue
		}
		if len(out.inputs) >= 1 && packBytes+f.packBytes > byteCap {
			break
		}
		out.inputs = append(out.inputs, f.pack)
		ids += len(f.ids)
		packBytes += f.packBytes
	}
	out.due = len(out.inputs) > 0
	return out
}

// recapExpunge is p's inputs capped again at maxBytes pack bytes, in their
// order and never below the first — what a build cut smaller than the nominal
// cut rewrites ([ExpungeInputByteCap] of that cut).
func (p planned) recapExpunge(maxBytes int64) planned {
	var out planned
	var packBytes int64
	for _, in := range p.inputs {
		if len(out.inputs) >= 1 && packBytes+in.packBytes > maxBytes {
			break
		}
		out.inputs = append(out.inputs, in)
		packBytes += in.packBytes
	}
	out.due = len(out.inputs) > 0
	return out
}
