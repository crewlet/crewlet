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

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// ExpungePoll is the interval between two passes of the EXPUNGE loop: a purge
// is taken up, and an expunge a crash left open finished, within a minute. The
// listing stays small at that rate: its OPEN arm walks a partial index
// (code_repos_expunging_idx), and its WEEKLY arm (code_repos_expunge_due_idx)
// holds only repositories not JUDGED within gitlet.ExpungeInterval, a clean
// scan being a record of its own (gitlet.ExpungeScan, [Compactor.judgedClean]),
// so a history is walked at most once an ExpungeInterval on any node.
const ExpungePoll = time.Minute

// ExpungeInputByteCap is the most PACK bytes one expunge's begin rewrites at a
// cut of cut: gitlet.MaxExpungeOutputs/2 cuts (16 GiB at
// gitlet.CompactPackBytes), halved for [InputByteCap]'s reason: any two
// consecutive outputs of one build sum past a cut, and a build that wrote more
// than one record takes is refused before any upload and halves the next
// cycle's cap ([Compactor.expungeCapFailed]). A repository past it is expunged
// over several cycles.
func ExpungeInputByteCap(cut int64) int64 { return gitlet.MaxExpungeOutputs / 2 * cut }

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

// The expunge's own reasons, beside the committer's refusals and the
// compaction's (reasonMemory, reasonStoreFull, ...): the drop epoch moved past
// a cycle's begin, and a copy whose objects/ still holds a begin's inputs.
const (
	reasonEpoch  = "epoch"
	reasonInputs = "inputs_leaving"
)

// errNothingKept is a build that wrote nothing even in its keep-nothing
// fallback: inputs no pack holds a byte of, which no correct copy is.
var errNothingKept = errors.New("compact: the expunge's inputs hold no object to keep or drop")

// ExpungePass is one pass of the EXPUNGE loop. It LISTS the `expunge_due`
// read's two arms (the weekly one least recently judged first), every page, at
// STALE for the compaction listing's reason; judges MAINTAINERS off ONE listing
// of who serves git, each by its maintenance's charge ([Compactor.PlacedBy]) so
// every writer of a repository's packs runs on one node, a placement it cannot
// read expunging nothing that pass; reads the store's room only while a
// repository is held back for a full store ([Compactor.releaseHeld]); then
// EXPUNGES each maintained repository not held back ([Compactor.Expunge]), one
// cycle each in listing order, until the next tick would begin. A listing that
// fails is the pass's error; code_expunge_pass logs at DEBUG unless something
// was expunged, refused or failed.
func (c *Compactor) ExpungePass(ctx context.Context) (ExpungeReport, error) {
	began := c.now()
	var report ExpungeReport
	listed, err := c.listExpunges(ctx, began.Add(-gitlet.ExpungeInterval))
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
// page: its open arm, and its weekly arm of the repositories last judged at
// or before before.
func (c *Compactor) listExpunges(ctx context.Context, before time.Time) ([]gitlet.ExpungeCandidate, error) {
	q := gitlet.ReadQuery{Kind: gitlet.ReadExpungeDue, ScannedBefore: before,
		Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	var out []gitlet.ExpungeCandidate
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

// forgetExpunges drops the full-store hold of every repository the listing no
// longer names, so the holds are bounded by the listing. A halved cap is kept
// for the compactor's life, as a compaction's is: it is a fact about what the
// repository's packs make of a build.
func (c *Compactor) forgetExpunges(listed []gitlet.ExpungeCandidate) {
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

// judgedClean records that cand's scheduled scan, reading the repository at
// seen, found no expunge due: ONE RECORD on its subject (gitlet.ExpungeScan),
// whose apply moves expunge_scanned_at so the weekly arm leaves it out for
// gitlet.ExpungeInterval on every node. Shared fleet state, never this
// process's memory, which a restart or a maintainer reshuffle loses; by the
// record's instant, never the row's version, which a push moves while a grace
// runs out by the clock alone (package doc, "The expunge"). A record that does
// not apply leaves the repository listed, and the next pass scans it again.
func (c *Compactor) judgedClean(ctx context.Context, cand gitlet.ExpungeCandidate, seen statelog.Position) {
	scan := gitlet.ExpungeScan{RepoID: cand.ID, ScanID: statelog.NewOpID(c.now(), ""), Node: c.o.NodeID, Seen: seen}
	res, err := c.o.Estate.Expunge(ctx, gitlet.ExpungeProposal{Scan: &scan})
	switch {
	case err != nil:
		c.log.Warn("code_expunge_scan_unrecorded", "repo", cand.Name, "scan", scan.ScanID, "error", err,
			"detail", "the clean scan's record could not be appended, so the repository stays listed and the "+
				"next pass scans it again")
	case res.Outcome == "":
		c.log.Debug("code_expunge_scan_unrecorded", "repo", cand.Name, "scan", scan.ScanID,
			"reason", string(res.Refusal), "detail", res.Detail)
	}
}

// maintainedOfExpunges is which listed repositories this node maintains, off
// ONE LISTING of who serves git, each by the charge it is placed by
// ([Compactor.PlacedBy]) — an error is UNKNOWN for every one of them, never
// "maintains none".
func (c *Compactor) maintainedOfExpunges(listed []gitlet.ExpungeCandidate) (map[string]bool, error) {
	if len(listed) == 0 {
		return nil, nil
	}
	ask := make(map[string]int64, len(listed))
	for _, cand := range listed {
		ask[cand.ID] = c.PlacedBy(cand.LiveObjects)
	}
	return c.o.Maintainers.MaintainerOf(ask)
}

// Expunge runs ONE CYCLE of an expunge of cand, a repository this node
// maintains as the `expunge_due` listing named it, and answers its outcome;
// nothing is retried, and the error is the one that failed or refused it, else
// nil. An expunge OPEN is finished first, from its rows ([Compactor.finish]),
// so the next maintainer resumes whatever stopped it; a purge, or a scheduled
// repository found due, runs [Compactor.cycle]. Never beside a publication of
// the repository on this node, which could plan over a pack or layer the begin
// drops, nor planned beside its compaction, which could retire a chosen input
// ([Compactor.claimExpunge]); compactions resume beside it, leaving its inputs
// out, once it has planned ([expungeClaim]), since a hot repository reaches
// gitlet.MaxPacksPerRepo in minutes. [Compactor.Stop] ends the cycle in flight,
// whoever asked.
func (c *Compactor) Expunge(ctx context.Context, cand gitlet.ExpungeCandidate) (Outcome, error) {
	c.expunging.Lock()
	defer c.expunging.Unlock()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	//nolint:contextcheck // c.life only cancels ctx, which stays the caller's: Stop must end the cycle however long the caller's ctx lives.
	defer context.AfterFunc(c.life, func() { cancel(context.Cause(c.life)) })()
	if !c.claimExpunge(cand.ID) {
		c.log.Debug("code_expunge_busy", "repo", cand.Name,
			"detail", "a compaction, a publication or an expunge of the repository is running on this node")
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
	default:
		out, err = c.cycle(ctx, cand, grace)
	}
	c.countedExpunge(out)
	return out, err
}

// expungeGrace is how long before its scan a cycle of cand keeps what a ref's
// drop left: none for a purge (gitlet.ExpungeBegin.Grace), gitlet.PruneGrace
// on the maintainer's schedule. An open expunge is rescued by what is owed
// now: a request asked while a scheduled expunge was open wants that grace's
// objects gone too, and the request's own cycle judges the rest.
func expungeGrace(cand gitlet.ExpungeCandidate) time.Duration {
	if cand.Request != "" {
		return 0
	}
	return gitlet.PruneGrace
}

// openExpunge is an expunge whose begin has applied, as its rescue and commit
// read it. floor is the position the rescue's sync reads at. epoch is 0 and
// scanTips nil where only the rows say it (a cycle resumed), which costs the
// rescue a walk of the whole history rather than of what moved since.
type openExpunge struct {
	id       string
	epoch    int64
	request  string
	grace    time.Duration
	floor    statelog.Position
	scanTips []string
}

// cycle runs one expunge of cand, a purge or a scheduled one, from its scan
// to its begin, then finishes it ([Compactor.finish]; package doc, "The expunge"):
//
//  1. SCAN: sync at session, floored at the listing's version as a compaction
//     does, read the live packs once and plan: a purge its pre-request packs
//     ([planExpunge]); the schedule RESERVES every eligible pack ([scheduled])
//     through its walk ([Compactor.due]), then narrows to its plan.
//  2. ADMIT (cache.Cache.AdmitExpunge) and BUILD the kept and dropping packs
//     ([Compactor.buildExpunge]), then release the admission.
//  3. UPLOAD every output ([uploadAll]); a full store holds the repository back.
//  4. BEGIN; applied, adopt every output from the scratch so this node
//     downloads none, and finish with the scan's tips, which bound the rescue.
func (c *Compactor) cycle(ctx context.Context, cand gitlet.ExpungeCandidate, grace time.Duration) (Outcome, error) {
	began := c.now()
	if cand.Request == "" && cand.LiveBytes < c.bounds.expungeMin {
		// NEVER DUE, FROM ITS ROW ALONE: what no tip reaches is at most its
		// live pack bytes, so it is judged with no sync, admission or walk, and
		// a repository no cache holds is never downloaded to learn it.
		c.judgedClean(ctx, cand, cand.Version)
		c.log.Debug("code_expunge_not_due", "repo", cand.Name, "live_bytes", cand.LiveBytes,
			"detail", "its live bytes are under the least an expunge frees")
		return OutcomeNotDue, nil
	}
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
	// THE LIVE PACKS, READ ONCE: merges run beside the cycle, and a merge's
	// swap leaves the copy s pins no live set to read again. A swap before even
	// this read is refused (cache.ErrSuperseded) rather than read as no pack,
	// which would judge clean a repository never weighed; the next pass syncs
	// past it.
	live, err := c.o.Cache.Packs(s)
	switch {
	case errors.Is(err, cache.ErrSuperseded):
		c.log.Debug("code_expunge_busy", "repo", cand.Name, "reason", reasonSuperseded, "error", err)
		return OutcomeBusy, err
	case err != nil:
		return c.expungeFailed(cand, "packs", err)
	}
	// planCapped caps a plan at its cut's bytes and RESERVES its inputs
	// ([expungeClaim]): from here the repository's compactions on this node
	// merge beside the cycle, leaving out every input the plan chose.
	var cut int64
	planCapped := func(q planned) planned {
		cut = c.bounds.cutFor(q)
		if at := ExpungeInputByteCap(cut); at < cap {
			q, cap = q.recapExpunge(at), at
		}
		c.reserveExpunge(cand.ID, q)
		return q
	}
	var (
		p        planned
		eligible []pack
	)
	by := c.now().Add(-gitlet.PruneGrace)
	if cand.Request != "" {
		p = planExpunge(live, requestedBefore(cand.Requested), cap)
		if len(p.inputs) == 0 {
			// THE REQUEST IS SERVED, or about to be: the listing read a row a
			// commit has since moved.
			c.log.Debug("code_expunge_not_due", "repo", cand.Name, "request", cand.Request,
				"detail", "no live pack dates from before the request")
			return OutcomeNotDue, nil
		}
		p = planCapped(p)
	} else {
		eligible = scheduled(live, by)
		if len(eligible) == 0 {
			c.judgedClean(ctx, cand, s.Position)
			c.log.Debug("code_expunge_not_due", "repo", cand.Name,
				"detail", "every live pack arrived within the grace, which keeps it whole")
			return OutcomeNotDue, nil
		}
		// EVERY ELIGIBLE PACK RESERVED through the walk, which reads their
		// indexes from the copy s pins and plans among them: a merge retiring
		// one would leave the walk an index gone and the begin an input no
		// longer live. The young packs merges are for stay free.
		c.reserveExpunge(cand.ID, planned{inputs: eligible})
	}
	drops, err := c.keptTips(ctx, cand.ID, s.Position, c.now().Add(-grace))
	if err != nil {
		return c.expungeFailed(cand, "scan", err)
	}
	limits := c.o.Limits()
	if cand.Request == "" {
		freed, out, err := c.due(ctx, cand, s, live, eligible, drops) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if out != "" {
			return out, err
		}
		p = planExpunge(live, arrivedBy(by, freed), cap)
		if len(p.inputs) == 0 {
			// Every file that would free a byte is past what one begin names
			// (gitlet.MaxExpungeInputs rows).
			c.judgedClean(ctx, cand, s.Position)
			c.log.Debug("code_expunge_not_due", "repo", cand.Name,
				"detail", "no pack holding what no tip reaches is one a single begin can name")
			return OutcomeNotDue, nil
		}
		// NARROWED to the plan's inputs: every other eligible pack may merge.
		p = planCapped(p)
	}
	c.observeStage(stageScan, began)

	buildBegan := c.now()
	adm, err := c.o.Cache.AdmitExpunge(s, p.objects(), limits)
	switch {
	case errors.Is(err, gitproc.ErrBusy):
		c.log.Debug("code_expunge_busy", "repo", cand.Name, "error", err)
		return OutcomeBusy, err
	case errors.Is(err, cache.ErrNoMemory):
		// NEVER REMEMBERED: the budget is read again at every pass, which
		// also asks again whether this node maintains the repository.
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
	if err = c.fault(ctx, gitletfault.ExpungeAfterBuild, 0); err != nil {
		return c.expungeFailed(cand, "build", err)
	}

	uploadBegan := c.now()
	outs := slices.Concat(b.kept, b.dropping)
	if out, err = c.uploadExpunge(ctx, cand, b.begin.ExpungeID, outs, p.size()); err != nil {
		return out, err
	}
	// The stored refs, each now naming its object, kept first as the begin
	// names them.
	b.kept, b.dropping = outs[:len(b.kept)], outs[len(b.kept):]
	b.begin.Add, b.begin.Dropping = refs(b.kept), refs(b.dropping)
	c.observeStage(stageUpload, uploadBegan)

	beginBegan := c.now()
	res, err := c.o.Estate.Expunge(ctx, gitlet.ExpungeProposal{Begin: &b.begin})
	if err != nil {
		return c.expungeFailed(cand, "begin", err)
	}
	if res.Outcome != statelog.OutcomeApplied {
		return c.expungeAnswered(cand, b.begin.ExpungeID, "begin", res)
	}
	// ADOPT EVERY OUTPUT before the scratch goes: the kept packs live, the
	// dropping ones into the copy's dropping view (never objects/pack, where a
	// push could rely on one), so the rescue's sync downloads none of what this
	// node just built, up to ExpungeInputByteCap a cycle. A failed adoption
	// costs that round the GETs, never the cycle.
	if len(b.kept) > 0 {
		if err := c.o.Cache.Adopt(ctx, cand.ID, b.sc, refs(b.kept)...); err != nil {
			c.log.Warn("code_expunge_adopt_failed", "repo", cand.Name, "expunge", b.begin.ExpungeID, "error", err)
		}
	}
	if len(b.dropping) > 0 {
		if err := c.o.Cache.AdoptDropping(ctx, cand.ID, b.sc, refs(b.dropping)...); err != nil {
			c.log.Warn("code_expunge_adopt_failed", "repo", cand.Name, "expunge", b.begin.ExpungeID, "error", err)
		}
	}
	c.log.Info("code_expunge_begun", "repo", cand.Name, "expunge", b.begin.ExpungeID, "request", cand.Request,
		"inputs", len(p.inputs), "kept", len(b.kept), "dropping", len(b.dropping),
		"dropped_layers", len(b.begin.DropLayers), "epoch", b.begin.DropEpoch, "position", res.Position.String())
	c.observeStage(stageBegin, beginBegan)
	if err := c.fault(ctx, gitletfault.ExpungeAfterBegin, 0); err != nil {
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
	begin          gitlet.ExpungeBegin
}

// buildExpunge builds p at cut ([Compactor.sortExpunge]) and holds it to the
// builds' own count and the bundle rule, rebuilding ONCE in a fresh scratch at
// the densest offender's ratio with the inputs recapped at that cut's
// [ExpungeInputByteCap], then to every rule of the begin. cap is the pack
// bytes p was capped at, which a failure halves. On an error the scratch is
// gone, and the outcome is the expunge's.
func (c *Compactor) buildExpunge(ctx context.Context, cand gitlet.ExpungeCandidate, s cache.Synced,
	adm *cache.Admission, p planned, cut, cap int64, grace time.Duration, drops []string,
	limits gitlet.Limits) (expungeBuilt, Outcome, error) {

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
	b.begin = gitlet.ExpungeBegin{RepoID: cand.ID, ExpungeID: b.begin.ExpungeID, RequestID: cand.Request,
		Node: c.o.NodeID, Seen: s.Position, DropEpoch: s.Repo.DropEpoch + 1, Grace: grace, Retire: p.ids(),
		Add: refs(b.kept), Dropping: refs(b.dropping),
		DropLayers: gitlet.LayersOver(c.o.Cache.Chain(s).Rows, retiring)}
	if err := (gitlet.ExpungeProposal{Begin: &b.begin}).ValidateUnstored(); err != nil {
		// THE BUILDER'S OWN BUG, refused before any byte is stored.
		b.sc.Remove()
		c.log.Error("code_expunge_invalid", "repo", cand.Name, "expunge", b.begin.ExpungeID, "error", err)
		return expungeBuilt{}, OutcomeFailed, err
	}
	return b, "", nil
}

// sortExpunge is one build of p's begin at cut, in a scratch made for it
// under a fresh expunge id: its tips (the copy's refs and every drop the
// scratch holds), then its kept and dropping packs, laid out under the id's
// derivation, kept first. On an error the scratch is removed.
//
// AN INPUT SET THE REST HOLDS WHOLE writes neither, yet a begin writes at
// least one output: so the dropping build runs again excluding nothing, every
// object of it live elsewhere too, for the commit to retire.
func (c *Compactor) sortExpunge(ctx context.Context, s cache.Synced, adm *cache.Admission, p planned, cut int64,
	drops []string, limits gitlet.Limits) (expungeBuilt, error) {

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
	adm *cache.Admission, p planned, cut int64, drops []string, limits gitlet.Limits) (expungeBuilt, error) {

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
		Config: PackConfig(limits.MaxBlobBytes),
		// THE WALK FROM EVERY TIP reads the whole history, whatever few packs
		// the inputs are: a deadline sized by them alone would cut it short.
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
	b := expungeBuilt{sc: sc, tips: tips, begin: gitlet.ExpungeBegin{ExpungeID: xid}}
	if b.kept, err = layOutputs(sc, xid, 0, kept); err != nil {
		return expungeBuilt{}, err
	}
	if b.dropping, err = layOutputs(sc, xid, len(kept), dropping); err != nil {
		return expungeBuilt{}, err
	}
	return b, nil
}

// packDropping packs p's inputs less kept and rest, excluded by name so
// `--stdin-packs`'s name-hash walk stops at their objects (package doc, "A
// merge makes no reachability judgement"), and answers what it printed.
func (c *Compactor) packDropping(ctx context.Context, adm *cache.Admission, sc *cache.Scratch, p planned,
	kept, rest []string, cut int64, limits gitlet.Limits) ([]string, error) {

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
		Config:   PackConfig(limits.MaxBlobBytes),
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

// present is every one of oids the scratch holds, in order: ONE `cat-file
// --batch-check`. A drop's object an earlier purge took out is no tip, since
// a walk from it would refuse the whole build; whatever it reached that a ref
// still reaches, the ref keeps.
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
// (gitlet.LayBundle). Every one it printed must be a pack the scratch holds.
func layOutputs(sc *cache.Scratch, id string, from int, printed []string) ([]output, error) {
	outs := make([]output, len(printed))
	for n, cs := range printed {
		path := filepath.Join(sc.Dir, "objects", "pack", "pack-"+cs+".pack")
		ref, err := gitlet.LayBundle(gitlet.DeriveOutputPackID(id, from+n), path, cs)
		if err != nil {
			return nil, fmt.Errorf("compact: lay out output %d: %w", from+n, err)
		}
		outs[n] = output{path: path, ref: *ref}
	}
	return outs, nil
}

// expungeCountHolds holds a begin's build to its own count, kept and dropping
// together at most gitlet.MaxExpungeOutputs; past it nothing is uploaded, the
// scratch is removed and the repository's cap is halved.
func (c *Compactor) expungeCountHolds(cand gitlet.ExpungeCandidate, b expungeBuilt, p planned,
	cap int64) (Outcome, error) {

	n := len(b.kept) + len(b.dropping)
	if n <= gitlet.MaxExpungeOutputs {
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
func (c *Compactor) expungeCapFailed(cand gitlet.ExpungeCandidate, p planned, cap int64, reason string,
	outputs int) {

	floor := p.inputs[0].packBytes
	was := min(cap, p.packBytes())
	c.mu.Lock()
	c.expungeCaps[cand.ID] = max(was/2, floor)
	next := c.expungeCaps[cand.ID]
	c.mu.Unlock()
	c.log.Warn("code_expunge_failed", "repo", cand.Name, "step", "build", "reason", reason,
		"inputs", len(p.inputs), "outputs", outputs, "most_outputs", gitlet.MaxExpungeOutputs,
		"input_bytes", p.packBytes(), "next_cap", next, "at_floor", was <= floor)
}

// uploadExpunge stores every output at once, as a compaction's are. A full
// store holds the repository back until it has estimate bytes of room and
// refuses the expunge; any other failure is the next pass's.
func (c *Compactor) uploadExpunge(ctx context.Context, cand gitlet.ExpungeCandidate, id string, outs []output,
	estimate int64) (Outcome, error) {

	uploads := make([]func(context.Context) error, len(outs))
	for i := range outs {
		o := &outs[i]
		uploads[i] = func(ctx context.Context) error { return gitlet.StoreBundle(ctx, c.o.Objects, o.path, &o.ref, nil) }
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
		var bad *gitlet.BadBundle
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
// rows alone where another cycle began it (package doc, "The expunge"):
//
//  1. SYNC at session, floored at open.floor: busy while the copy's objects/
//     still holds the begin's inputs, which hide what the rescue must save
//     (cache.ErrInputsLeaving).
//  2. RESCUE ([Compactor.rescue]) what the refs, kept drops and rows in flight
//     reach of the dropping packs, `--not` the scan's tips where known: valid
//     at any position at or after the begin, so a crash anywhere resumes.
//  3. COMMIT: retire every dropping pack, add the rescue live (its one
//     precondition, its expunge open at its epoch, lets no push starve it)
//     and, applied, adopt the rescue.
func (c *Compactor) finish(ctx context.Context, cand gitlet.ExpungeCandidate, open openExpunge) (Outcome, error) {
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
	dropping, err := c.o.Cache.DroppingPacks(s)
	switch {
	case errors.Is(err, cache.ErrSuperseded):
		c.log.Debug("code_expunge_busy", "repo", cand.Name, "expunge", open.id, "reason", reasonSuperseded,
			"error", err)
		return OutcomeBusy, err
	case err != nil:
		return c.expungeFailed(cand, "dropping packs", err)
	}
	// BEGUN FROM HERE: the copy is past the begin and the dropping packs are
	// read, so compactions here run as though no expunge ran ([expungeClaim]):
	// none takes a dropping pack, and the commit needs only its expunge open.
	c.beganExpunge(cand.ID)
	commit := gitlet.ExpungeCommit{RepoID: cand.ID, ExpungeID: open.id, CommitID: statelog.NewOpID(c.now(), ""),
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
	if err = (gitlet.ExpungeProposal{Commit: &commit}).ValidateUnstored(); err != nil {
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
	if err = c.fault(ctx, gitletfault.ExpungeAfterRescueUpload, 0); err != nil {
		return c.expungeFailed(cand, "commit", err)
	}
	c.observeStage(stageRescue, began)

	commitBegan := c.now()
	res, err := c.o.Estate.Expunge(ctx, gitlet.ExpungeProposal{Commit: &commit})
	if err != nil {
		return c.expungeFailed(cand, "commit", err)
	}
	if err := c.fault(ctx, gitletfault.ExpungeAfterCommit, 0); err != nil {
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
	c.announceExpunged(ctx, types.CodeExpunged{RepoID: cand.ID, RepoName: s.Repo.Name, Request: open.request,
		Expunge: open.id, DroppedObjects: max(droppingObjects-outputObjects(rescue), 0), DroppedBytes: dropped,
		Position: res.Position.String(), StoreUntil: storeUntil(c.now())})
	return OutcomeApplied, nil
}

// storeUntil is the latest a healthy fleet's object store keeps the packs an
// expunge committed at committed retired: gitlet.RetireGrace for a clone
// already reading one, then up to gitlet.RetentionInterval for retire_sweep to
// remove their rows, then up to collect.CollectInterval for the collector.
// collect.PendingGrace adds nothing: it runs from the upload, before the
// begin, and is no longer than RetireGrace.
func storeUntil(committed time.Time) time.Time {
	return committed.UTC().Add(gitlet.RetireGrace + gitlet.RetentionInterval + collect.CollectInterval)
}

// [storeUntil] leaves collect.PendingGrace out only while it is no longer than
// gitlet.RetireGrace: past it this constant overflows and the build fails.
const _ = uint64(gitlet.RetireGrace - collect.PendingGrace)

// announceExpunged publishes an applied expunge commit's account
// (code_expunged) on this node's event stream, bounded by [eventBudget]; a
// stream that does not take it is logged, and the commit stands.
func (c *Compactor) announceExpunged(ctx context.Context, ev types.CodeExpunged) {
	if c.o.Events == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), eventBudget)
	defer cancel()
	e := events.New(ev, events.TraceContext{})
	e.Source = c.o.NodeID
	if err := c.o.Events.Publish(ctx, topics.Event(e.Type), e); err != nil {
		c.log.Warn("code_expunged_unpublished", "repo", ev.RepoName, "expunge", ev.Expunge, "error", err.Error(),
			"detail", "the event log does not hold this expunge; the repository's detail names it as its latest")
	}
}

// rescue builds the rescue of the expunge open on s's repository, where any
// pack is dropping ([Compactor.finish], step 2), and answers its outputs and
// the scratch that holds them — none of either where nothing is dropping, or
// where nothing dropping is reached. On an error the scratch is gone, and the
// outcome is the expunge's.
func (c *Compactor) rescue(ctx context.Context, cand gitlet.ExpungeCandidate, s cache.Synced, open openExpunge,
	cid string, objects int64, dropping []gitlet.PackRow) ([]output, *cache.Scratch, Outcome, error) {

	if len(dropping) == 0 {
		return nil, nil, "", nil
	}
	drops, err := c.keptTips(ctx, cand.ID, s.Position, c.now().Add(-open.grace))
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
		// ONE REBUILD at the densest offender's ratio, in a fresh scratch:
		// the first's outputs are its files.
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
	if err == nil && len(outs) > gitlet.MaxExpungeOutputs {
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
	open openExpunge, cid string, cut, packBytes int64, drops []string, limits gitlet.Limits) ([]output, error) {

	refTips, err := sc.RefTips()
	if err != nil {
		return nil, err
	}
	// THE SCAN'S TIPS ARE HELD TO THE SCRATCH TOO: a `--not` naming an object
	// it lacks would refuse the build, and the check costs a lookup a tip.
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
		Config: PackConfig(limits.MaxBlobBytes),
		// THE WALK IS THE WHOLE HISTORY's where the scan's tips are unknown
		// (a cycle resumed).
		Deadline: gitproc.PackWorkDeadline(packBytes) + gitproc.HistoryWalk(s.Repo.LiveObjects),
	})
	if err != nil {
		return nil, fmt.Errorf("compact: pack what a ref reaches of the dropping packs: %w", err)
	}
	return layOutputs(sc, cid, 0, printed)
}

// rescueInputs is the dropping rows as a plan's packs: what the rescue's cut
// is scaled by, as a build's is by its inputs ([bounds.cutFor]).
func rescueInputs(dropping []gitlet.PackRow) []pack {
	out := make([]pack, len(dropping))
	for i, p := range dropping {
		out[i] = pack{checksum: p.GitChecksum, ids: []string{p.PackID}, objects: p.Objects,
			packBytes: p.Ref().PackBytes(), size: p.Size}
	}
	return out
}

// keptTips is every object an expunge of repoID keeps beside its refs, each
// once: the `expunge_tips` read, every page, at session floored at at so it
// covers every record the copy beside it holds. That is what a ref drop left
// since since and, whatever since is, what rows hold in flight (M6-D2: open or
// draft revision heads, live queue heads, candidates' tips, green rows), so a
// purge still keeps an open change's force-pushed revisions and a pending
// landing's commit.
func (c *Compactor) keptTips(ctx context.Context, repoID string, at statelog.Position,
	since time.Time) ([]string, error) {

	q := gitlet.ReadQuery{Kind: gitlet.ReadExpungeTips, ExpungeTips: &gitlet.ExpungeTipsQuery{RepoID: repoID, Since: since},
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

// due judges whether a scheduled expunge of cand is due, over the live packs
// its scan read, the eligible files among them ([scheduled]) and the tips it
// keeps (drops): it answers what each eligible file would drop by checksum,
// and "" where due or the cycle's outcome where not. live is the scan's
// reading, never s read again, which a merge's swap leaves reading as empty.
//
// DUE per [expungeDue], counting only the ELIGIBLE files: garbage in a pack
// younger than the grace is no byte this cycle could free
// ([Compactor.unreachableIn]). The walk runs under its own admission, which
// sorts nothing (cache.Cache.AdmitExpungeScan, [indexBytes]), released before
// the build is admitted. Found NOT DUE, the repository is recorded judged
// ([Compactor.judgedClean]).
func (c *Compactor) due(ctx context.Context, cand gitlet.ExpungeCandidate, s cache.Synced, live []gitlet.PackRow,
	eligible []pack, drops []string) (map[string]int64, Outcome, error) {

	adm, err := c.o.Cache.AdmitExpungeScan(s, indexBytes(eligible))
	switch {
	case errors.Is(err, gitproc.ErrBusy):
		c.log.Debug("code_expunge_busy", "repo", cand.Name, "error", err)
		return nil, OutcomeBusy, err
	case errors.Is(err, cache.ErrNoMemory):
		c.log.Warn("code_expunge_refused", "repo", cand.Name, "reason", reasonMemory, "error", err)
		return nil, OutcomeRefused, err
	case err != nil:
		return nil, c.expungeFailedOnly(cand, "admit", err), err
	}
	freed, err := c.unreachableIn(ctx, adm, s, eligible, drops)
	adm.Release()
	if err != nil {
		return nil, c.expungeFailedOnly(cand, "scan", err), err
	}
	seen := map[string]bool{}
	var liveBytes, unreachable int64
	for _, row := range live {
		if !seen[row.GitChecksum] {
			seen[row.GitChecksum] = true
			liveBytes += row.Ref().PackBytes()
		}
	}
	for _, n := range freed {
		unreachable += n
	}
	if !expungeDue(unreachable, liveBytes, c.bounds.expungeMin) {
		c.judgedClean(ctx, cand, s.Position)
		c.log.Debug("code_expunge_not_due", "repo", cand.Name, "live_bytes", liveBytes,
			"unreachable_bytes", unreachable, "eligible", len(eligible),
			"detail", "what no tip reaches of the packs older than the grace is under a quarter of the live "+
				"bytes, or under the least an expunge frees; the repository is scanned again in a week")
		return nil, OutcomeNotDue, nil
	}
	c.log.Info("code_expunge_due", "repo", cand.Name, "live_bytes", liveBytes, "unreachable_bytes", unreachable,
		"eligible", len(eligible))
	return freed, "", nil
}

// expungeDue reports whether unreachable bytes of a repository of live pack
// bytes make an expunge on the maintainer's own schedule due: more than
// 1/gitlet.ExpungeShare of them, and at least least.
func expungeDue(unreachable, live, least int64) bool {
	return unreachable >= least && unreachable*gitlet.ExpungeShare > live
}

// union is every object id of a and b, each once, sorted.
func union(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

// expungeAnswered is what the committer's answer makes of a begin or a commit
// that did not apply.
func (c *Compactor) expungeAnswered(cand gitlet.ExpungeCandidate, id, record string,
	res gitlet.ExpungeResult) (Outcome, error) {

	switch res.Outcome {
	case statelog.OutcomePending:
		// A begin that applies is an expunge open, which the next pass's
		// listing names and finishes.
		return OutcomePending, nil
	case statelog.OutcomeUnknown:
		return OutcomeUnknown, nil
	}
	refusal := fmt.Errorf("compact: the %s of expunge %s of %s refused %s: %s", record, id, cand.Name,
		res.Refusal, res.Detail)
	switch res.Refusal {
	case gitlet.RefusalStale:
		c.log.Debug("code_expunge_stale", "repo", cand.Name, "expunge", id, "record", record, "detail", res.Detail)
		return OutcomeStale, refusal
	case gitlet.RefusalBusy:
		c.log.Debug("code_expunge_busy", "repo", cand.Name, "expunge", id, "record", record, "detail", res.Detail)
		return OutcomeBusy, refusal
	case gitlet.RefusalUnavailable:
		return c.expungeFailed(cand, record, refusal)
	}
	c.log.Warn("code_expunge_refused", "repo", cand.Name, "expunge", id, "record", record,
		"reason", string(res.Refusal), "detail", res.Detail)
	return OutcomeRefused, refusal
}

// expungeFailed logs an expunge that failed at step and answers it failed.
func (c *Compactor) expungeFailed(cand gitlet.ExpungeCandidate, step string, err error) (Outcome, error) {
	return c.expungeFailedOnly(cand, step, err), err
}

// expungeFailedOnly logs an expunge that failed at step: failed.
func (c *Compactor) expungeFailedOnly(cand gitlet.ExpungeCandidate, step string, err error) Outcome {
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

// owed is which live packs one expunge cycle takes: a purge's, those whose
// content predates its request's position; the schedule's, those whose newest
// content arrived by its clock less gitlet.PruneGrace and that free bytes,
// freed by checksum as its scan weighed them ([Compactor.unreachableIn]).
type owed struct {
	schedule  bool
	requested statelog.Position
	arrivedBy time.Time
	freed     map[string]int64
}

// requestedBefore is a purge's [owed]: the packs dated before its request.
func requestedBefore(requested statelog.Position) owed { return owed{requested: requested} }

// arrivedBy is the schedule's [owed]: the packs that arrived at or before at
// and free something, by freed.
func arrivedBy(at time.Time, freed map[string]int64) owed {
	return owed{schedule: true, arrivedBy: at, freed: freed}
}

// file is one pack FILE of a repository's live rows (a checksum, all its rows
// together, as a compaction's plan weighs one), with its rows' oldest content
// and newest arrival; unknown where a row's arrival is zero (a pack a cache
// adopted before a round served its row).
type file struct {
	pack
	content, arrived int64
	unknown          bool
}

// filesOf is live's files, in the order their first rows come, each with its
// row ids sorted.
func filesOf(live []gitlet.PackRow) []*file {
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
		if row.Arrived.IsZero() {
			f.unknown = true
		} else {
			f.arrived = max(f.arrived, row.Arrived.UnixNano())
		}
	}
	for _, f := range order {
		slices.Sort(f.ids)
	}
	return order
}

// ripe reports whether the schedule may take f at by: every row's arrival
// known and the newest at or before by. An unknown arrival is passed over,
// the side the grace keeps whole.
func (f *file) ripe(by time.Time) bool { return !f.unknown && f.arrived <= by.UnixNano() }

// scheduled is every file of live the maintainer's own schedule may take at
// by ([file.ripe]), earliest arrival first, the lowest row id breaking a tie:
// the files its scan weighs ([Compactor.due]). PURE in its inputs.
func scheduled(live []gitlet.PackRow, by time.Time) []pack {
	var ripe []*file
	for _, f := range filesOf(live) {
		if f.ripe(by) {
			ripe = append(ripe, f)
		}
	}
	slices.SortFunc(ripe, func(a, b *file) int {
		return cmp.Or(cmp.Compare(a.arrived, b.arrived), strings.Compare(a.ids[0], b.ids[0]))
	})
	out := make([]pack, len(ripe))
	for i, f := range ripe {
		out[i] = f.pack
	}
	return out
}

// planExpunge plans one cycle over live, PURE in its inputs: every FILE
// ([file]) o owes, capped at gitlet.MaxExpungeInputs row ids and byteCap pack
// bytes, never below one file, since one pack rewritten is a cycle. FULL packs
// and packs a published layer names are inputs like any other: what an
// expunge drops may lie in any pack, and it is the one rewrite that takes them.
//
// A PURGE takes every file one of whose rows predates the request
// (gitlet.PackRow.Content; zero, a row no round has served yet, reads as older,
// so an undated pack is rewritten rather than passed over), oldest first. THE
// SCHEDULE takes every ripe file ([file.ripe]) that frees a byte, DENSEST FIRST
// (most freed per byte rewritten, which every Gitlet node downloads again),
// then earliest arrival. The lowest row id breaks a tie.
func planExpunge(live []gitlet.PackRow, o owed, byteCap int64) planned {
	var due []*file
	for _, f := range filesOf(live) {
		switch {
		case !o.schedule && f.content < o.requested.Packed():
			due = append(due, f)
		case o.schedule && f.ripe(o.arrivedBy) && o.freed[f.checksum] > 0:
			due = append(due, f)
		}
	}
	density := func(f *file) float64 { return float64(o.freed[f.checksum]) / float64(max(f.packBytes, 1)) }
	slices.SortFunc(due, func(a, b *file) int {
		if o.schedule {
			return cmp.Or(cmp.Compare(density(b), density(a)), cmp.Compare(a.arrived, b.arrived),
				strings.Compare(a.ids[0], b.ids[0]))
		}
		return cmp.Or(cmp.Compare(a.content, b.content), strings.Compare(a.ids[0], b.ids[0]))
	})
	var out planned
	var ids int
	var packBytes int64
	for _, f := range due {
		if ids+len(f.ids) > gitlet.MaxExpungeInputs {
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

// recapExpunge is p's inputs capped again at maxBytes pack bytes, in order and
// never below the first: what a build at a smaller cut rewrites.
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
