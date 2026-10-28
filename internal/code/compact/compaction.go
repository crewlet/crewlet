package compact

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
)

// errOutputs and errBundleTooLarge are a build this node refuses before any
// byte of it is stored: more outputs than one compaction records, and an
// output past the bundle rule even at the smaller cut a rebuild takes.
var (
	errOutputs        = errors.New("compact: the build wrote more packs than one compaction records")
	errBundleTooLarge = errors.New("compact: an output's bundle is past the bound every stored pack is held to")
)

// Compact plans and runs ONE compaction of cand — a repository this node is the
// home of, as the `compaction_due` listing named it — and answers what became
// of it: the unit a suite drives, and what every pass runs one at a time.
// Nothing is retried inside it: every failure is the next poll's (a moment's
// state) or remembered until something changes (a definite one), and every
// outcome is counted and logged. Its error is the one that failed or refused
// it, nil for every other outcome.
//
// Throughout, cand.ID is the REPOSITORY and the compaction's own id is minted
// at its build — two ids of two things, never one name for both.
//
//  1. SYNC at session, floored at the repository's version the listing read:
//     never linearizable, because a barrier a compaction would cost the code
//     log up to some 31 000 records a day at 250 000 pushes buys nothing — the
//     committer holds every input to its own snapshot, and a plan from an
//     older one is refused stale. The repository's home holds it already in
//     the ordinary case: its reads are asked of it too (ADR-0059).
//  2. PLAN ([plan]) over the incarnation's live packs, capped at the
//     repository's cap ([Compactor.capFor]) and then at [InputByteCap] of the
//     cut the inputs take ([bounds.cutFor]). Not due is remembered at the
//     listing's version and the cap, never an admission's refusal.
//  3. ADMIT the build to the maintenance lane ([cache.Cache.AdmitCompaction]):
//     busy is the next poll's; a charge the node's memory cannot hold is
//     refused `memory` and never remembered, since the budget is read again
//     every pass.
//  4. BUILD in a scratch named by a fresh compaction id, leaning on a store of
//     exactly the packs it merges, linked from the incarnation
//     ([Compactor.pack]), so its cost follows the roll and never the history
//     beside it; lay each output out; hold the build's
//     own COUNT to what one compaction records — past it nothing is uploaded
//     and the repository's cap is halved — and every output to the BUNDLE
//     RULE, rebuilding ONCE at the densest offender's own ratio, the plan
//     capped again at that cut's [InputByteCap]; a second miss halves the cap
//     too. Then the whole proposal is held to every rule its committer will
//     (code.CompactionProposal.ValidateUnstored), before any byte is stored.
//     THE ADMISSION IS RELEASED HERE, once the last git process has ended:
//     the uploads, the proposal and the adoption run no git, and holding the
//     slot and the charge through an upload the store paces would idle the
//     node's maintenance for every other repository (M3-decisions M3-O29,
//     M3-O40). The scratch, and its pin of the incarnation, stay to the end.
//  5. UPLOAD every output at once ([uploadAll]); a store with no room is
//     refused `store_full` and the repository held back until it has the room
//     ([Compactor.releaseHeld]), and outputs stored drop the repository's own
//     hold, whatever the store ([Compactor.stored]).
//  6. PROPOSE one compaction through the estate's router, under its write
//     attempt, and answer what the committer decided: applied adopts the
//     outputs into this node's cache by link — so the node that built them
//     downloads none — and forgets the repository's not-due memory.
//
// THE COMPACTOR'S LIFETIME BOUNDS IT, whoever asked: [Compactor.Stop] ends the
// compaction in flight whether a pass or a caller of its own started it.
func (c *Compactor) Compact(ctx context.Context, cand code.CompactionCandidate) (Outcome, error) {
	c.compacting.Lock()
	defer c.compacting.Unlock()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	defer context.AfterFunc(c.life, func() { cancel(context.Cause(c.life)) })()
	began := c.now()
	out, err := c.compact(ctx, cand, began)
	c.counted(out, began)
	return out, err
}

// compact is [Compactor.Compact]'s body.
func (c *Compactor) compact(ctx context.Context, cand code.CompactionCandidate, began time.Time) (Outcome, error) {
	s, err := c.o.Cache.Sync(ctx, cand.ID, statelog.FloorAt(statelog.ReadSession, cand.Version))
	if err != nil {
		return c.failed(cand, "sync", err)
	}
	defer s.Release()

	cap := c.capFor(cand.ID)
	p := plan(c.o.Cache.Packs(s), cap, c.bounds)
	if !p.due {
		c.remember(cand, cap)
		return OutcomeNotDue, nil
	}
	cut := c.bounds.cutFor(p)
	if at := InputByteCap(cut); at < cap {
		p, cap = p.recap(at), at
	}
	limits := c.o.Limits()
	adm, err := c.o.Cache.AdmitCompaction(s, p.objects(), limits)
	switch {
	case errors.Is(err, gitproc.ErrBusy):
		c.log.Debug("code_compaction_busy", "repo", cand.Name, "error", err)
		return OutcomeBusy, err
	case errors.Is(err, cache.ErrNoMemory):
		// NEVER REMEMBERED: the node's budget is read again at every pass —
		// a container resized in place, a maintainer run that ended — so the
		// next poll plans again and is admitted once the budget holds the
		// charge. The cache logged it once for the incarnation.
		c.refused(reasonMemory)
		return OutcomeRefused, err
	case err != nil:
		return c.failed(cand, "admit", err)
	}
	defer adm.Release()

	b, out, err := c.buildChecked(ctx, cand, s, adm, p, cut, cap, limits)
	if err != nil {
		return out, err
	}
	defer b.sc.Remove()
	adm.Release()

	if out, err := c.upload(ctx, cand, &b); err != nil {
		return out, err
	}
	if err := c.fault(ctx, codefault.CompactAfterUpload, 0); err != nil {
		return c.failed(cand, "propose", err)
	}
	res, err := c.o.Estate.Compact(ctx, b.proposal)
	if err != nil {
		return c.failed(cand, "propose", err)
	}
	if err := c.fault(ctx, codefault.CompactAfterPropose, 0); err != nil {
		return c.failed(cand, "adopt", err)
	}
	return c.answered(ctx, cand, b, res, began)
}

// built is one build of a compaction, laid out and checked: the scratch that
// holds its outputs, the plan it merged and the proposal that names them.
type built struct {
	sc       *cache.Scratch
	plan     planned
	outs     []output
	proposal code.CompactionProposal
}

// buildChecked builds p at cut and holds the result to the build's own count
// and to the bundle rule, rebuilding once where an output passes the rule (see
// [Compactor.Compact], step 4), and then to every rule of the proposal. cap is
// the pack bytes p was capped at, which a failure halves. On an error the
// scratch is gone, and the outcome is the compaction's.
func (c *Compactor) buildChecked(ctx context.Context, cand code.CompactionCandidate, s cache.Synced,
	adm *cache.Compacting, p planned, cut, cap int64, limits code.Limits) (built, Outcome, error) {

	b, err := c.build(ctx, cand.ID, s, adm, p, cut, limits)
	if err != nil {
		return built{}, c.failedOnly(cand, "build", err), err
	}
	if out, err := c.countHolds(cand, b, cap); err != nil {
		return built{}, out, err
	}
	if ratio := pastBound(b.outs, c.bounds.maxBundle); ratio > 0 {
		// ONE REBUILD, in the same reserved slot, at the densest offender's
		// own ratio, the plan capped again at that cut's byte cap.
		b.sc.Remove()
		cut = int64(float64(c.bounds.cut) / ratio)
		if at := InputByteCap(cut); at < cap {
			p, cap = p.recap(at), at
		}
		if b, err = c.build(ctx, cand.ID, s, adm, p, cut, limits); err != nil {
			return built{}, c.failedOnly(cand, "build", err), err
		}
		if out, err := c.countHolds(cand, b, cap); err != nil {
			return built{}, out, err
		}
		if pastBound(b.outs, c.bounds.maxBundle) > 0 {
			b.sc.Remove()
			c.capFailed(cand, b.plan, cap, reasonBundleTooLarge, len(b.outs))
			return built{}, OutcomeFailed, errBundleTooLarge
		}
	}
	if err := b.proposal.ValidateUnstored(); err != nil {
		// THE BUILDER'S OWN BUG — every rule above passed and one of its
		// committer's did not — refused before any byte is stored.
		b.sc.Remove()
		c.log.Error("code_compaction_invalid", "repo", cand.Name, "compaction", b.proposal.Compaction.CompactionID,
			"error", err)
		return built{}, OutcomeFailed, err
	}
	return b, "", nil
}

// build runs one pack-objects over p at cut in a scratch named by a fresh
// compaction id, and lays its outputs out into the compaction its committer
// would be asked to decide. On an error the scratch is removed.
func (c *Compactor) build(ctx context.Context, repoID string, s cache.Synced, adm *cache.Compacting, p planned,
	cut int64, limits code.Limits) (built, error) {

	cid := statelog.NewOpID(c.now(), "")
	sc, err := c.o.Cache.NewMergeScratch(ctx, s, cid, p.checksums())
	if err != nil {
		return built{}, err
	}
	printed, err := c.pack(ctx, adm, sc, p, cut, limits)
	if err == nil {
		err = c.fault(ctx, codefault.CompactAfterBuild, 0)
	}
	var outs []output
	if err == nil {
		outs, err = lay(sc, cid, printed)
	}
	if err != nil {
		sc.Remove()
		return built{}, err
	}
	return built{sc: sc, plan: p, outs: outs, proposal: code.CompactionProposal{Compaction: code.Compaction{
		RepoID: repoID, CompactionID: cid, Node: c.o.NodeID, Seen: s.Position, Retire: p.ids(),
		Add: refs(outs)}}}, nil
}

// countHolds holds a build to its own count — THE AUTHORITY on how many packs
// it wrote, which no cut bounds alone ([InputByteCap]): more than one
// compaction records (code.MaxCompactOutputs), or more than it retires, and
// nothing is uploaded, the scratch is removed and the repository's cap is
// halved, so the next poll plans a smaller roll rather than the same one.
func (c *Compactor) countHolds(cand code.CompactionCandidate, b built, cap int64) (Outcome, error) {
	if n := len(b.outs); n <= code.MaxCompactOutputs && n <= len(b.proposal.Compaction.Retire) {
		return "", nil
	}
	b.sc.Remove()
	c.capFailed(cand, b.plan, cap, reasonOutputs, len(b.outs))
	return OutcomeFailed, errOutputs
}

// capFailed halves cand's cap after a build of p, capped at cap, failed for
// reason — HALF WHAT THE BUILD MERGED, which a cap far above a small roll's
// bytes would otherwise halve to no effect, the next plan the same roll —
// floored at its two lightest inputs, and remembered as not due at the floor,
// where a plan of the same inputs would fail the same way until the
// repository moves; and counts and logs it.
func (c *Compactor) capFailed(cand code.CompactionCandidate, p planned, cap int64, reason string, outputs int) {
	floor := p.inputs[0].packBytes + p.inputs[1].packBytes
	atFloor := c.halve(cand.ID, min(cap, p.packBytes()), floor)
	if atFloor {
		c.remember(cand, c.capFor(cand.ID))
	}
	c.refused(reason)
	c.log.Warn("code_compaction_failed", "repo", cand.Name, "step", "build", "reason", reason,
		"inputs", len(p.inputs), "outputs", outputs, "most_outputs", code.MaxCompactOutputs,
		"input_bytes", p.packBytes(), "next_cap", c.capFor(cand.ID), "at_floor", atFloor)
}

// upload stores every output of b at once, each hit at the CompactUpload
// point before every read the store makes of it. A store with no room holds
// the repository back until it has room and refuses the compaction
// `store_full`; any other failure is the next poll's.
func (c *Compactor) upload(ctx context.Context, cand code.CompactionCandidate, b *built) (Outcome, error) {
	uploads := make([]func(context.Context) error, len(b.outs))
	for i := range b.outs {
		o := &b.outs[i]
		uploads[i] = func(ctx context.Context) error {
			return code.StoreBundle(ctx, c.o.Objects, o.path, &o.ref, func(ctx context.Context, at int64) error {
				return c.fault(ctx, codefault.CompactUpload, at)
			})
		}
	}
	err := uploadAll(ctx, uploads)
	if full, ok := storeFull(err); ok {
		estimate := b.plan.size()
		c.holdFull(cand.ID, estimate)
		c.refused(reasonStoreFull)
		c.log.Warn("code_compaction_refused", "repo", cand.Name, "compaction", b.proposal.Compaction.CompactionID,
			"refusal", reasonStoreFull, "bytes", estimate, "error", err,
			"detail", fmt.Sprintf("the object store has no room for this compaction's outputs; it is planned "+
				"again once %s", full.Remedy))
		return OutcomeRefused, err
	}
	if err != nil {
		var bad *code.BadBundle
		if errors.As(err, &bad) {
			c.log.Error("code_compaction_rejected", "repo", cand.Name,
				"compaction", b.proposal.Compaction.CompactionID, "object", bad.Object.String(), "error", bad.Err)
		}
		return c.failed(cand, "upload", err)
	}
	for i, o := range b.outs {
		b.proposal.Compaction.Add[i] = o.ref
	}
	c.stored(ctx, cand.ID)
	return "", nil
}

// answered is what the committer's answer makes of a compaction whose
// outputs are stored.
func (c *Compactor) answered(ctx context.Context, cand code.CompactionCandidate, b built, res code.CompactResult,
	began time.Time) (Outcome, error) {

	cid := b.proposal.Compaction.CompactionID
	switch res.Outcome {
	case statelog.OutcomeApplied:
		// THE OUTPUTS ARE PRESENT AT ONCE, linked from the scratch — never a
		// GET of what this node just stored — and the inputs leave this
		// node's copy at its next round's swap, as on every other git node.
		// A failed adoption costs that round a GET, never the compaction.
		if err := c.o.Cache.Adopt(ctx, cand.ID, b.sc, b.proposal.Compaction.Add...); err != nil {
			c.log.Warn("code_compaction_adopt_failed", "repo", cand.Name, "compaction", cid, "error", err)
		}
		c.mu.Lock()
		delete(c.notDue, cand.ID)
		c.mu.Unlock()
		c.log.Info("code_compaction_applied", "repo", cand.Name, "compaction", cid,
			"inputs", len(b.plan.inputs), "outputs", len(b.outs), "objects", b.plan.objects(),
			"bytes_in", b.plan.size(), "bytes_out", outputBytes(b.outs), "position", res.Position.String(),
			"took", c.now().Sub(began))
		return OutcomeApplied, nil
	case statelog.OutcomePending:
		// The outputs are named once the record applies, and the collector's
		// should it never: the next listing reads the rows as they are.
		return OutcomePending, nil
	case statelog.OutcomeUnknown:
		return OutcomeUnknown, nil
	}
	if res.Refusal != "" {
		c.refused(string(res.Refusal))
	}
	refusal := fmt.Errorf("compact: compaction %s of %s refused %s: %s", cid, cand.Name, res.Refusal, res.Detail)
	switch res.Refusal {
	case code.RefusalStale:
		c.log.Debug("code_compaction_stale", "repo", cand.Name, "compaction", cid, "detail", res.Detail)
		return OutcomeStale, refusal
	case code.RefusalBusy:
		c.log.Debug("code_compaction_busy", "repo", cand.Name, "compaction", cid, "detail", res.Detail)
		return OutcomeBusy, refusal
	case code.RefusalUnavailable:
		return c.failed(cand, "propose", refusal)
	case code.RefusalPackTooLarge:
		// The builder's check and the decide's disagreeing — a bug — on the
		// same inputs it would make the same outputs, so it is remembered
		// until the repository moves.
		c.remember(cand, c.capFor(cand.ID))
	}
	c.log.Warn("code_compaction_refused", "repo", cand.Name, "compaction", cid, "refusal", string(res.Refusal),
		"detail", res.Detail)
	return OutcomeRefused, refusal
}

// failed logs a compaction that failed at step and answers it failed.
func (c *Compactor) failed(cand code.CompactionCandidate, step string, err error) (Outcome, error) {
	return c.failedOnly(cand, step, err), err
}

// failedOnly logs a compaction that failed at step: failed.
func (c *Compactor) failedOnly(cand code.CompactionCandidate, step string, err error) Outcome {
	c.log.Warn("code_compaction_failed", "repo", cand.Name, "step", step, "error", err)
	return OutcomeFailed
}

// remember is a plan of cand found not due at the version the listing named
// and at cap: skipped until either moves.
func (c *Compactor) remember(cand code.CompactionCandidate, cap int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notDue[cand.ID] = memo{version: cand.Version, cap: cap}
}
