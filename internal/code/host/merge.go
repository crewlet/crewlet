package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/toolname"
)

// A CHANGE'S MERGE IS BUILT HERE AND LANDS AS ONE MERGE RECORD (ADR-0096,
// M5-D11): a merge_change (code.WriteMergeChange) is an engine write — the
// same admission, the same sync to its caller's floor, the same scratch named
// by its push id, the same one budget of git run time, the same self-contained
// pack checked by `index-pack --strict`, the same deadline rule and the same
// landing every push takes (land.go) — whose proposal is a MERGE RECORD
// (code.MergeProposal) rather than a push: the record that marks the change
// merged is the record that moves its target, decided by the repository's
// committer from one snapshot against the change's merge gate.
//
// # Judged before anything is built
//
// The copy is synced to the caller's floor and the change read at it (the
// `change` read, as the caller who asks it): a change that is not there, not
// live, a draft, into another branch or at another head than the caller read
// is refused before anything is built, and so — for a merge its gate holds —
// is one whose readiness, judged with its asker as the merger, names a
// precondition unmet besides the merge its build proves (gate_unmet, every one
// named), a seat's override (not_person), and a seat's merge — or enqueue —
// that whoever its turn works for may not make (not_permitted,
// [writing.mergeCeiling]). Every such refusal is judged
// again at the log's head before it is answered, as every write's is, and a
// change found merged is first looked up as this write's own merge (its push
// id names the merge's push row): found, it is answered from that row, nothing
// built. A target whose merge gate requires a check (code.GateRules.
// QueueTarget, read from the repository's gates only where a rule protects the
// target) takes the change through its QUEUE: the write builds nothing and asks
// the committer to enqueue it (code.QueueProposal), its gate the enqueue's
// decide — unless a person overrides the gate, which merges directly.
//
// # Built onto the tip the copy holds
//
// A fast_forward builds nothing: the target's tip must be an ancestor of the
// head (`merge-base --is-ancestor`), or it is refused not_fast_forward. A merge
// or a squash runs ONE `merge-tree --write-tree -z --allow-unrelated-histories`
// of the tip and the head — the merge a change's mergeability is recorded by,
// so the two say one thing — under code.MergeBuildDeadline: a conflict is
// answered with its paths, nothing stored, and a head the tip already holds is
// nothing_to_change. Then ONE `commit-tree`: a merge commit of the tip and the
// head, authored by WHO MERGED (U43 a: the person, or the seat); one squashed
// commit on the tip, authored by the CHANGE'S OWNER with a `Co-authored-by:`
// trailer for each of its other authors (U43 a); each committed by the engine
// at U18 c's dates — the author's the operation's instant, the committer's that
// or the later parent's (commitDates) — so a retry builds the same commit.
//
// # Recomputed onto a moving target
//
// The committer's decide holds the target to the tip the merge was built onto,
// read from its own snapshot; a target that moved since is refused
// target_moved, and the merge is built again onto the tip a linearizable sync
// finds — at most code.MergeRecomputes times, after which it is answered
// target_moving: a branch moving faster than its merges are built is its
// queue's to land, or a quieter moment's. A merge refused expunging — built
// from a copy at another drop epoch than the repository's, an expunge having
// begun since (M6-D4) — is built again the same way, from a copy a
// linearizable sync brings past the begin, and answered expunging where
// every rebuild meets it: a begin is rare and its sync quick, so one rebuild
// almost always lands it.
//
// EVERY ROUND DRAWS ON ONE BUDGET, AND FITS ONE ATTEMPT. The merge's git is
// code.MergeBuildDeadline across its first build and every rebuild together,
// never that much a round: the router sizes the write's attempt on a Gitlet node
// for ONE build of it (internal/estate's Timing.GitWriteAttemptFor), so five
// rounds of thirty seconds each would run past the attempt on exactly the
// busy target rebuilds exist for — answered unready during a build, which
// sends the walk's runner-up Gitlet node to start the whole merge again, or busy
// at the landing, either after builds and stored bundles nobody proposes. So
// a rebuild is started only where it fits — the round before it as the
// measure of it, since it merges the same change onto a tip moved by a push
// or two: what is left of the budget must hold the git that round ran, and
// what is left of the attempt the waits the attempt holds for one write (the
// share's, and three reads' at a read's budget each), that git, and the
// upload and proposal that round's landing was held to (the deadline rule's)
// — and a merge with no room for one is answered target_moving with its
// rebuilds so far, as is a rebuild whose git runs out the budget the rounds
// before it left: the change is no larger than its first build proved it,
// its target moved under it.

// mergeCarrying is p as its landing proposes it: carrying pack — the bundle
// its landing stored, nil for a merge that stores none — as its record names
// it, and the drop epoch of the copy it was built from (code.MergeProposal.
// DropEpoch), the landing's pinned one, as a push's proposal carries it.
func mergeCarrying(p code.MergeProposal, pack *code.PackRef, epoch int64) code.MergeProposal {
	p.Merge.Pack = pack
	p.DropEpoch = epoch
	return p
}

// mergeJudgement is what a merge's judgement read: the change, the target's
// tip in the copy, whether the target lands its merges through its queue, and
// whether the merge deletes the change's source ([writing.deletesSource]).
type mergeJudgement struct {
	change       *code.ChangeDetail
	tip          string
	queue        bool
	deleteSource bool
}

// mergeVerdict is a refusal a merge's judgement reached, with its sentence and
// the preconditions a gate_unmet names, and whether it may be this write's own
// merge, already landed.
type mergeVerdict struct {
	refusal code.MergeRefusal
	detail  string
	unmet   []code.Precondition
	ours    bool
}

// runMerge is a merge_change's steps from its sync on ([writing.mergeRounds]),
// and its rebuilds recorded once, as it ended ([writing.recordRecomputes]).
func (wr *writing) runMerge(ctx context.Context) (code.EngineWriteResult, error) {
	res, err := wr.mergeRounds(ctx)
	wr.recordRecomputes(res, err)
	return res, err
}

// The outcomes crewlet.code.merge.recomputes records a merge under beside a
// refusal's own code, which is its outcome where it was refused.
const (
	// mergeMerged is a merge whose record applied or is pending — or one
	// found already landed.
	mergeMerged = "merged"
	// mergeQueued is a change its target's queue took rather than merged.
	mergeQueued = "queued"
)

// recordRecomputes observes how many times one merge_change was built again
// onto a target that moved under it (code.MergeAnswer.Recomputes) on
// crewlet.code.merge.recomputes, by how it ended: merged, queued, a refusal's
// own code — target_moving where code.MergeRecomputes rebuilds, or the merge's
// one budget, ran out — unknown, unready (another Gitlet node is asked, and
// records its own), refused (an argument the build found invalid, or an
// object it named missing) or failed. ONCE A MERGE, from the count its answer
// carries, so the distribution is merges rather than rounds: a branch moving
// under its merges is the mean a merge took (the sum over the count, exact
// where the count boundaries put 0 and 1 in one bucket) climbing towards
// code.MergeRecomputes, which target_moving at the cap names, where
// crewlet.code.merge.outcomes' committer counts only target_moved records and
// cannot tell four rebuilds that landed from four merges that each rebuilt
// once.
func (wr *writing) recordRecomputes(res code.EngineWriteResult, err error) {
	if wr.s.o.Metrics == nil {
		return
	}
	recomputes := 0
	if res.Merge != nil {
		recomputes = res.Merge.Recomputes
	}
	wr.s.o.Metrics.ObserveValue(metrics.CodeMergeRecomputes, float64(recomputes),
		metrics.Attrs{"outcome": mergeOutcome(res, err)})
}

// mergeOutcome is how crewlet.code.merge.recomputes names a merge that ended
// so.
func mergeOutcome(res code.EngineWriteResult, err error) string {
	switch {
	case err != nil:
		return writeOutcome(res, err)
	case res.Merge != nil && res.Merge.Refusal != "":
		return string(res.Merge.Refusal)
	case res.Refusal != "":
		return string(res.Refusal)
	case res.Merge != nil && res.Merge.Queued:
		return mergeQueued
	case res.Outcome == statelog.OutcomeApplied, res.Outcome == statelog.OutcomePending:
		return mergeMerged
	case res.Outcome == statelog.OutcomeUnknown:
		return writeUnknown
	}
	return writeRefused
}

// mergeRounds is a merge_change's steps from its sync on: judged, built,
// landed — and built again onto a target that moved under it, or from a copy
// an expunge's begin overtook, at most code.MergeRecomputes times, every
// round drawing on the merge's ONE budget of git and started only where it
// fits what is left of that budget and of the write's attempt
// (roomToRebuild).
func (wr *writing) mergeRounds(ctx context.Context) (code.EngineWriteResult, error) {
	s := wr.s
	fresh := wr.fresh()
	// ONE BUDGET FOR EVERY ROUND (code.MergeBuildDeadline): charged by each
	// round's git and never given afresh, so the merge's git is the one
	// build its attempt was sized for.
	wr.left = s.buildBudget(wr.w.Kind)
	var again *code.MergeAnswer
	for recomputes := 0; ; recomputes++ {
		syncStart := s.now()
		j, res, done, err := wr.judgeMerge(ctx, fresh)
		wr.syncTook += s.now().Sub(syncStart)
		if done || err != nil {
			return withRecomputes(res, recomputes), err
		}
		if j.queue {
			res, err = wr.enqueue(ctx, j)
			return withRecomputes(res, recomputes), err
		}
		before := wr.left
		res, refused, err := wr.buildAndLandMerge(ctx, j)
		if errors.Is(err, errBuildSpent) {
			if recomputes == 0 {
				return wr.spent(), nil
			}
			// A REBUILD RAN OUT WHAT THE ROUNDS BEFORE IT LEFT: the change
			// built within its budget once; its target kept moving.
			return withRecomputes(wr.notRebuilt(again, recomputes, fmt.Sprintf("its rebuilds ran out the %s of "+
				"git a merge is given", s.buildBudget(wr.w.Kind))), recomputes), nil
		}
		if err != nil || refused == nil {
			return withRecomputes(res, recomputes), err
		}
		again = refused
		if recomputes == code.MergeRecomputes {
			return withRecomputes(wr.notRebuilt(again, recomputes+1, ""), recomputes), nil
		}
		if why := wr.roomToRebuild(ctx, before-wr.left); why != "" {
			return withRecomputes(wr.notRebuilt(again, recomputes+1, why), recomputes), nil
		}
		// THE TIP THE DECIDE HELD is at or past the log's head as this node
		// reads it next: built again onto what a linearizable sync finds.
		fresh = statelog.Freshness{Level: statelog.ReadLinearizable}
	}
}

// notRebuilt is the answer to a merge built builds times and not again, why
// saying what stopped it ("" for code.MergeRecomputes rebuilds spent): the
// last record's own refusal where it was expunging — the copies every build
// met — and target_moving where its target moved under each build.
func (wr *writing) notRebuilt(last *code.MergeAnswer, builds int, why string) code.EngineWriteResult {
	if last != nil && last.Refusal == code.MergeRefusalExpunging {
		return code.EngineWriteResult{Merge: last}
	}
	stopped := ""
	if why != "" {
		stopped = ", and " + why
	}
	return code.EngineWriteResult{Merge: &code.MergeAnswer{Refusal: code.MergeRefusalTargetMoving,
		Detail: fmt.Sprintf("%s moved under this merge's build %d times running%s: it moves faster than a merge "+
			"is built onto it — merge again once it is quieter", wr.w.Ref, builds, stopped)}}
}

// roomToRebuild is why the merge cannot be built again — "" where it can —
// judged with the round just landed, whose git ran lastGit, as the measure of
// the next: what is left of the merge's budget of git must hold lastGit, and
// what is left of the write's attempt the waits an engine write's attempt
// holds beside its build (internal/estate's GitWriteAttempt: the share's
// code.GitShareWait, and three at a read's budget each — the sync, the read
// of a moved head as the write's own push, the sync at the log's head a
// refusal is judged at — each this host's syncWait), the build's lastGit,
// and the upload and proposal the last landing was held to (writing.landNeed,
// the deadline rule's). A caller with no deadline holds any.
func (wr *writing) roomToRebuild(ctx context.Context, lastGit time.Duration) string {
	s := wr.s
	if wr.left < lastGit {
		return fmt.Sprintf("its builds left %s of the %s of git a merge is given, under the %s its last ran",
			max(wr.left, 0).Round(time.Millisecond), s.buildBudget(wr.w.Kind), lastGit.Round(time.Millisecond))
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return ""
	}
	need := code.GitShareWait + 3*s.syncWait + lastGit + wr.landNeed
	if left := time.Until(deadline); left < need {
		return fmt.Sprintf("its caller left %s, under the %s another build and landing take",
			left.Round(time.Millisecond), need.Round(time.Millisecond))
	}
	return ""
}

// withRecomputes is res with how many times its merge was built again.
func withRecomputes(res code.EngineWriteResult, n int) code.EngineWriteResult {
	if n == 0 {
		return res
	}
	if res.Merge == nil {
		res.Merge = &code.MergeAnswer{}
	}
	res.Merge.Recomputes = n
	return res
}

// judgeMerge syncs the copy to fresh and judges the merge on it — and, before
// any refusal is answered, once more at the log's head (a linearizable
// re-sync), a change found merged looked up first as this write's own merge.
func (wr *writing) judgeMerge(ctx context.Context, fresh statelog.Freshness) (mergeJudgement,
	code.EngineWriteResult, bool, error) {

	asked := false
	for {
		gone, err := wr.sync(ctx, fresh)
		if err != nil {
			return mergeJudgement{}, code.EngineWriteResult{}, true, err
		}
		j, v, err := wr.mergeVerdict(ctx, gone)
		if err != nil {
			return mergeJudgement{}, code.EngineWriteResult{}, true, err
		}
		if v.refusal == "" {
			return j, code.EngineWriteResult{}, false, nil
		}
		if v.ours && !asked {
			asked = true
			if res, cmds, found, err := wr.outcome(ctx); err != nil || found {
				if found && res.Outcome != "" {
					res.Merge = &code.MergeAnswer{DeleteSource: deletedSource(cmds)}
				}
				return mergeJudgement{}, res, true, err
			}
		}
		if !wr.linear {
			fresh = statelog.Freshness{Level: statelog.ReadLinearizable}
			continue
		}
		return mergeJudgement{}, code.EngineWriteResult{Merge: &code.MergeAnswer{Refusal: v.refusal,
			Detail: v.detail, Unmet: v.unmet}}, true, nil
	}
}

// mergeStatusRefusal is the refusal a repository's row gives a merge and each
// of a queue's steps toward one — the push refusal its status reads
// ([Server.statusRefusal]) as the merge's code: repo_deleted, repo_archived,
// repo_importing or repo_mirrored, in the domain's one sentence for the state
// — or "" for a repository that takes one. THE ONE MAPPING the merge's verdict
// and a queue's chain ([Server.BuildChain]) both read BEFORE they build: the
// decide refuses each from the same row, but only once a scratch, a share unit
// of the seat git-work and a stored pack were spent on a merge it could never
// take.
func (s *Server) mergeStatusRefusal(repo code.RepoRef) (code.MergeRefusal, string) {
	switch reason, words := s.statusRefusal(repo); reason {
	case code.RefusalRepoDeleted:
		return code.MergeRefusalRepoDeleted, words
	case code.RefusalRepoArchived:
		return code.MergeRefusalRepoArchived, words
	case code.RefusalRepoImporting:
		return code.MergeRefusalRepoImporting, words
	case code.RefusalRepoMirrored:
		return code.MergeRefusalRepoMirrored, words
	}
	return "", ""
}

// mergeVerdict judges the merge on the synced copy, in the order its record's
// decide reads its rules: the repository's status, the change as the caller
// read it, a seat's override, the target's tip, and — for a merge its gate
// holds — whether its target is a queue's and whether its readiness, with its
// asker as the merger, is met but for the merge its build proves. The change
// is read as its DETAIL ALONE (code.ReadQuery.DetailOnly): a merge reads no
// revision, and this read is made again at the log's head before a refusal
// and on every rebuild, each a page of revisions' facts shipped for nothing.
func (wr *writing) mergeVerdict(ctx context.Context, gone bool) (mergeJudgement, mergeVerdict, error) {
	s, w := wr.s, wr.w
	repo := wr.synced.Repo
	if gone {
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalRepoDeleted,
			detail: code.RepoGone{ID: w.RepoID, Retention: s.o.DeletedRetention()}.Detail()}, nil
	}
	if refusal, words := s.mergeStatusRefusal(repo); refusal != "" {
		return mergeJudgement{}, mergeVerdict{refusal: refusal, detail: words}, nil
	}
	floor := statelog.FloorAt(statelog.ReadSession, wr.synced.Position)
	ans, err := s.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadChange, Asker: w.Principal, Repo: repo.Name,
		Change: w.Change, DetailOnly: true, Fresh: floor})
	switch {
	case errors.Is(err, code.ErrChangeNotFound):
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalChangeNotFound, detail: fmt.Sprintf(
			"repository %s holds no change %s", repo.Name, code.ChangeRef(repo.Name, w.Change))}, nil
	case err != nil:
		return mergeJudgement{}, mergeVerdict{}, err
	case ans.Change == nil:
		return mergeJudgement{}, mergeVerdict{}, fmt.Errorf("host: the read of %s answered no change",
			code.ChangeRef(repo.Name, w.Change))
	}
	c := ans.Change
	named := code.ChangeRef(repo.Name, c.Number)
	switch {
	case c.State == code.ChangeStateMerged:
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalChangeNotOpen, ours: true,
			detail: fmt.Sprintf("%s is merged already, at %s", named, shortID(c.MergeOID))}, nil
	case c.State == code.ChangeStateClosed:
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalChangeNotOpen,
			detail: fmt.Sprintf("%s is closed, and only a live change merges", named)}, nil
	case c.State == code.ChangeStateDraft:
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalChangeDraft, detail: fmt.Sprintf(
			"%s is a draft: its owner, a principal that maintains the repository or a person marks it ready "+
				"first", named)}, nil
	case c.TargetRef != w.Ref:
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalStale, detail: fmt.Sprintf("%s merges "+
			"into %s, not the %s you read: read it again", named, c.TargetRef, w.Ref)}, nil
	case c.Head != w.ExpectedHead:
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalHeadMoved, detail: fmt.Sprintf("%s's "+
			"head is %s, not the %s you read: read it again", named, shortID(c.Head), shortID(w.ExpectedHead))}, nil
	case w.Override != "" && w.Principal.Kind != code.PrincipalOperator:
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalNotPerson, detail: fmt.Sprintf("only a "+
			"person merges past a merge gate, and this merge of %s overrides it as seat %s", named,
			w.Principal.Seat)}, nil
	}
	tip, err := wr.head(ctx)
	if err != nil {
		return mergeJudgement{}, mergeVerdict{}, err
	}
	if tip == "" {
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalStale, detail: fmt.Sprintf("%s merges "+
			"into %s, which does not exist", named, w.Ref)}, nil
	}
	j := mergeJudgement{change: c, tip: tip}
	gate := &mergeGate{wr: wr, floor: floor}
	if w.Ceiling != nil {
		if v, err := wr.mergeCeiling(ctx, gate, named); err != nil || v.refusal != "" { //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			return mergeJudgement{}, v, err
		}
	}
	if j.deleteSource, err = wr.deletesSource(ctx, gate, c.SourceRef); err != nil {
		return mergeJudgement{}, mergeVerdict{}, err
	}
	if w.Override != "" {
		return j, mergeVerdict{}, nil
	}
	rules, err := gate.rules(ctx)
	if err != nil {
		return mergeJudgement{}, mergeVerdict{}, err
	}
	if j.queue = rules.QueueTarget(); j.queue {
		return j, mergeVerdict{}, nil
	}
	if c.Readiness == nil {
		return mergeJudgement{}, mergeVerdict{}, fmt.Errorf("host: the read of live change %s answered no "+
			"readiness", named)
	}
	var unmet []code.Precondition
	for _, p := range c.Readiness.Preconditions {
		// THE MERGE ITS BUILD PROVES, against the tip it builds onto: the
		// one precondition the record's decide does not judge either.
		if p.State != code.PreconditionMet && p.Kind != code.PreconditionMergeable {
			unmet = append(unmet, p)
		}
	}
	if len(unmet) > 0 {
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalGateUnmet, unmet: unmet,
			detail: fmt.Sprintf("%s does not meet %s's merge gate: %s", named, w.Ref, unmet[0].Detail)}, nil
	}
	return j, mergeVerdict{}, nil
}

// mergeCeiling is why a seat's merge may not be made for whoever its turn
// works for (code.EngineWrite.Ceiling, M3-decisions U40 (a)), as a refusal,
// or none where it may — judged here, from this node's chart and the synced
// row, as every engine write's ceiling is (write.go's rules), and before the
// merge is built or enqueued, since an enqueue is the gate (M5-D8) and its
// landing lands it as its enqueuer:
//
//   - THE SEAT'S OWN WORK — a target under its own namespace, or any target
//     of a repository its seat owns (code.Grant.OwnWork) — is steered by
//     whoever woke the turn, nobody included, as every write's is;
//   - NOBODY (code.WriteCeilingNobody) steers nothing else;
//   - a COLLEAGUE SEAT is held to the merge authority the colleague itself
//     holds into the target (M5-D2) — code.MergeAuthority, the one rule a
//     readiness's merge_authority term is judged by, judged here for the
//     colleague from this node's chart, the synced row and the target's gate
//     this merge reads anyway ([mergeGate]) — so a seat never merges for a
//     colleague a merge the colleague could not make, and one the colleague
//     may not read (code.Grant.MayRead) is refused as one it may not merge.
//     No read of the change is made again as the colleague: the term reads
//     nothing of the change but its target, which the write names and the
//     caller's own read already held to the change's.
func (wr *writing) mergeCeiling(ctx context.Context, gate *mergeGate, named string) (mergeVerdict, error) {
	s, w, repo := wr.s, wr.w, wr.synced.Repo
	roster := s.o.Roster()
	if roster == nil {
		return mergeVerdict{}, wr.unready(code.UnreadyNoChart, fmt.Sprintf("%s holds no chart yet to judge "+
			"whom seat %q's merge is made for; a Gitlet node holding one builds it", s.o.NodeID, w.Principal.Seat))
	}
	if code.GrantOf(w.Principal, repo, roster).OwnWork(code.Command{Ref: w.Ref}) {
		return mergeVerdict{}, nil
	}
	if w.Ceiling.Kind != code.WriteCeilingSeat {
		return mergeVerdict{refusal: code.MergeRefusalNotPermitted, detail: fmt.Sprintf("this work is for "+
			"whoever started its turn, who may change nothing of this company's code; a merge of %s into %s is "+
			"not this seat's own work — its own namespace (%s<its handle>/) and the repositories it owns",
			named, w.Ref, code.SeatNamespaceRoot)}, nil
	}
	colleague := code.Principal{Kind: code.PrincipalAgent, Seat: w.Ceiling.Seat}
	if !code.GrantOf(colleague, repo, roster).MayRead() {
		return mergeVerdict{refusal: code.MergeRefusalNotPermitted, detail: fmt.Sprintf("this work is for "+
			"seat %s, whose ask started its turn and who may not read %s, so may not merge it", w.Ceiling.Seat,
			named)}, nil
	}
	rules, err := gate.rules(ctx)
	if err != nil {
		return mergeVerdict{}, err
	}
	if p := code.MergeAuthority(colleague, repo, w.Ref, rules, roster); p.State != code.PreconditionMet {
		return mergeVerdict{refusal: code.MergeRefusalNotPermitted, detail: fmt.Sprintf("this work is for "+
			"seat %s, whose ask started its turn, and it may not make this merge itself: %s",
			w.Ceiling.Seat, p.Detail)}, nil
	}
	return mergeVerdict{}, nil
}

// deletesSource is whether this merge deletes the change's source branch: its
// caller's word where it said one (code.EngineWrite.DeleteSource), and
// otherwise THE DEFAULT, decided here on the snapshot the merge is judged on
// — a source in a seat's namespace is deleted unless a gate over the target
// keeps it (code.GateRules.KeepsSource, from the gate this judgement reads
// anyway wherever a rule protects the target), and every other source is
// kept (Q4): a seat's branch is the engine's working copy, and a person's is
// theirs. Decided here rather than by the caller, so a merge from a seat's
// branch costs its caller no read of the gates, and the rule a merge deletes
// by is the one its own judgement read.
func (wr *writing) deletesSource(ctx context.Context, gate *mergeGate, source string) (bool, error) {
	if said := wr.w.DeleteSource; said != nil {
		return *said, nil
	}
	if !strings.HasPrefix(source, gitref.SeatsRoot) {
		return false, nil
	}
	rules, err := gate.rules(ctx)
	if err != nil {
		return false, err
	}
	return !rules.KeepsSource, nil
}

// deletedSource reports a merge's push row deleting the change's source: the
// second ref a merge's push names, there only where the merge deleted it
// (code.Merge's push).
func deletedSource(cmds []code.Command) bool {
	return len(cmds) > 1 && cmds[1].New == code.ZeroOID
}

// mergeGate is the gate one judgement of a merge holds its target to — every
// rule of the repository matching it, with its merge gate, combined with
// code.merge_by_floor on this node's epoch (code.GateRulesOf) — read at most
// once a judgement, and only where something asks it: whether the target is a
// queue's, the merge authority of the colleague a seat's merge is made for,
// and whether a merge from a seat's branch deletes it by default. Read where the judgement's floor holds the synced copy, so each
// question is asked of the snapshot the merge is judged on.
type mergeGate struct {
	wr    *writing
	floor statelog.Freshness
	read  *code.GateRules
}

// rules is the target's gate: the repository's gates — which its row never
// carries — read from its detail only where a rule protects the target. A
// target no rule protects has no gate, and is never read for one.
func (m *mergeGate) rules(ctx context.Context) (code.GateRules, error) {
	if m.read != nil {
		return *m.read, nil
	}
	wr := m.wr
	w, repo := wr.w, wr.synced.Repo
	floor := wr.s.limits().MergeByFloor
	protected := false
	for _, rule := range repo.Protections {
		if pattern, ok := gitref.ParsePattern(rule.Pattern); !ok || pattern.Match(w.Ref) {
			// A RULE THIS BUILD CANNOT READ is read with its gate, whose
			// judgement refuses it: never a target taken for unprotected.
			protected = true
			break
		}
	}
	var rules []code.Protection
	if protected {
		ans, err := wr.s.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadRepo, Asker: w.Principal,
			Repo: repo.Name, Gates: true, Fresh: m.floor})
		if err != nil {
			return code.GateRules{}, err
		}
		if ans.Repo == nil {
			return code.GateRules{}, fmt.Errorf("host: the read of repository %s's gates answered no repository",
				repo.Name)
		}
		rules = make([]code.Protection, len(ans.Repo.Gates))
		for i, g := range ans.Repo.Gates {
			rules[i] = code.Protection{PushRule: code.PushRule{Pattern: g.Pattern}, Gate: &g.Gate}
		}
	}
	gate, err := code.GateRulesOf(rules, w.Ref, floor)
	if err != nil {
		return code.GateRules{}, err
	}
	m.read = &gate
	return gate, nil
}

// enqueue puts the change in its target's queue, at the head its caller read —
// the enqueue's decide its gate — and answers what became of it. The
// enqueue's asker names no run: an entry is its enqueuer's, never a turn's.
func (wr *writing) enqueue(ctx context.Context, j mergeJudgement) (code.EngineWriteResult, error) {
	w := wr.w
	asker := w.Principal
	asker.Run = code.RunProvenance{}
	entry := code.QueueEntryID(w.OpID)
	wr.landed = true
	res, err := wr.s.o.Estate.Queue(ctx, code.QueueProposal{OpID: w.OpID, Asker: asker, Seen: wr.synced.Position,
		Enqueue: &code.Enqueue{RepoID: w.RepoID, EntryID: entry, ChangeID: j.change.ID, TargetRef: w.Ref,
			HeadOID: w.ExpectedHead, Method: w.Method, DeleteSource: j.deleteSource, CloseTask: w.CloseTask}})
	var quiet silence
	switch {
	case errors.Is(err, code.ErrInvalidProposal):
		return code.EngineWriteResult{}, fmt.Errorf("host: the enqueue of %s the engine formed: %w",
			code.ChangeRef(wr.repoName(), w.Change), err)
	case err != nil && ctx.Err() == nil && errors.As(err, &quiet) && !quiet.Silent():
		// NOTHING RAN IT: asked again under its operation, it is the same
		// entry.
		return code.EngineWriteResult{Merge: &code.MergeAnswer{EntryID: entry,
			Refusal: code.MergeRefusalUnavailable, Detail: fmt.Sprintf("no data node took the enqueue of %s: %s; "+
				"merge again", code.ChangeRef(wr.repoName(), w.Change), sentence(err.Error()))}}, nil
	case err != nil:
		// A NODE WENT SILENT, or the caller left: it may have enqueued it.
		wr.s.log.Warn("code_merge_enqueue_unknown", "repo", wr.repoName(), "entry", entry, "error", err)
		return code.EngineWriteResult{Outcome: statelog.OutcomeUnknown, Merge: &code.MergeAnswer{EntryID: entry,
			Queued: true, DeleteSource: j.deleteSource}}, nil
	case res.Refusal != "":
		return code.EngineWriteResult{Position: res.Position, Merge: &code.MergeAnswer{EntryID: entry,
			Refusal: res.Refusal, Detail: res.Detail, Unmet: res.Unmet}}, nil
	}
	return code.EngineWriteResult{Outcome: res.Outcome, Position: res.Position, Merge: &code.MergeAnswer{
		Queued: true, EntryID: res.EntryID, Place: res.Place, DeleteSource: j.deleteSource}}, nil
}

// buildAndLandMerge is one round's build and landing: the merge built in its
// scratch onto j.tip under the merge's budget and one unit of the node's seat
// git-work share, then the deadline rule and the one landing with its record.
// again is the answer of a record refused target_moved or expunging — nil for
// every other: the merge is the caller's to build again.
func (wr *writing) buildAndLandMerge(ctx context.Context, j mergeJudgement) (res code.EngineWriteResult,
	again *code.MergeAnswer, err error) {

	s, w := wr.s, wr.w
	wr.cmd = code.Command{Ref: w.Ref, Old: j.tip}
	sc, err := s.o.Cache.NewScratch(ctx, wr.synced, wr.pushID)
	if err != nil {
		if errors.Is(err, cache.ErrStopped) {
			return code.EngineWriteResult{}, nil, s.writesStopping()
		}
		return code.EngineWriteResult{}, nil, fmt.Errorf("host: a scratch for merge %s: %w", wr.pushID, err)
	}
	defer sc.Remove()

	buildStart := s.now()
	commit, res, done, err := wr.buildMergeShared(ctx, sc, j)
	wr.buildTook += s.now().Sub(buildStart)
	if done || err != nil {
		return res, nil, err
	}
	if err = s.fault(ctx, codefault.MergeAfterBuild, 0); err != nil {
		return code.EngineWriteResult{}, nil, fmt.Errorf("host: merge %s after its build: %w", wr.pushID, err)
	}
	wr.cmd.New = commit
	if res, refused, err := wr.deadlineRule(ctx, sc); refused || err != nil { //nolint:govet // shadow: `res, refused, err := f()` declares refused too; see .golangci.yml
		return res, nil, err
	}
	wr.landed = true
	landStart := s.now()
	l, err := s.land(ctx, landing{Synced: wr.synced, Scratch: sc, Commands: []code.Command{wr.cmd},
		Principal: w.Principal, Via: code.ViaMerge, PushID: wr.pushID, ReceivedAt: s.now(),
		Merge: &code.MergeProposal{OpID: w.OpID, Asker: w.Principal, Seen: wr.synced.Position,
			Merge: code.Merge{RepoID: w.RepoID, MergeID: wr.pushID, ChangeID: j.change.ID, Method: w.Method,
				TargetRef: w.Ref, BaseOID: j.tip, HeadOID: w.ExpectedHead, CommitOID: commit,
				DeleteSource: j.deleteSource, Override: w.Override, Node: s.o.NodeID, ReceivedAt: s.now()}}})
	wr.landTook += s.now().Sub(landStart)
	if err != nil {
		return code.EngineWriteResult{}, nil, err
	}
	if l.Merge == nil {
		// REFUSED BY THE LANDING BEFORE ITS PROPOSAL — the store full or
		// unavailable, the pack past a bound — as a push's is.
		return code.EngineWriteResult{Command: l.Commands[0]}, nil, nil
	}
	m := l.Merge
	if m.Refusal == code.MergeRefusalTargetMoved || m.Refusal == code.MergeRefusalExpunging {
		return code.EngineWriteResult{}, &code.MergeAnswer{Refusal: m.Refusal, Detail: m.Detail}, nil
	}
	res = code.EngineWriteResult{Outcome: m.Outcome, Position: m.Position, Merge: &code.MergeAnswer{
		Refusal: m.Refusal, Detail: m.Detail, Unmet: m.Unmet}}
	switch m.Outcome {
	case statelog.OutcomeApplied:
		res.Commit = m.MergeOID
	case statelog.OutcomePending:
		res.Commit = commit
	}
	if m.Refusal == "" {
		res.Merge.DeleteSource = j.deleteSource
	}
	return res, nil, nil
}

// buildMergeShared is a merge's build under one unit of the node's seat
// git-work share and what is left of its budget of git run time
// (code.MergeBuildDeadline, every round's together: mergeRounds gives it once):
// a fast-forward's ancestry, or a merge's or a squash's merge, commit and
// pack. Past code.GitShareWait the share is unready, about this node; a build
// that runs out the budget is errBuildSpent, which mergeRounds answers.
func (wr *writing) buildMergeShared(ctx context.Context, sc *cache.Scratch, j mergeJudgement) (string,
	code.EngineWriteResult, bool, error) {

	s := wr.s
	release, err := s.o.Share.Take(ctx, code.SeatWorkWrite)
	if err != nil {
		if code.AnswerDue(ctx) {
			err = wr.unready(code.UnreadyShare, fmt.Sprintf("%s's seat git work held its whole share until the "+
				"merge's answer fell due, its asker's attempt spent; another Gitlet node builds it", s.o.NodeID))
		}
		return "", code.EngineWriteResult{}, true, err
	}
	defer release()
	wr.metered = true
	commit, res, done, err := wr.buildMerge(ctx, sc, j)
	wr.metered = false
	return commit, res, done, err
}

// buildMerge is the merge's build in sc, onto j.tip: see this file's head —
// its one merge (mergeOnto), then its pack.
func (wr *writing) buildMerge(ctx context.Context, sc *cache.Scratch, j mergeJudgement) (string,
	code.EngineWriteResult, bool, error) {

	b, err := wr.newBuilding(sc)
	if err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	commit, res, done, err := b.mergeOnto(ctx, wr.mergeOf(j))
	if done || err != nil {
		return "", res, true, err
	}
	if res, done, err := b.pack(ctx); done || err != nil {
		return "", res, true, err
	}
	return commit, code.EngineWriteResult{}, false, nil
}

// mergeOf is one change's merge as a build makes it: what its commit is
// written from, the target's tip it is built onto, the head it merges and
// how, who merges it and the instant they did — a merge_change's from its
// write and the change its judgement read (writing.mergeOf), a chain entry's
// from the entry (chain.go).
type mergeOf struct {
	change  code.ChainChange
	target  string
	tip     string
	head    string
	method  code.MergeMethod
	merger  code.Principal
	instant time.Time
}

// mergeOf is the merge_change's merge onto j.tip.
func (wr *writing) mergeOf(j mergeJudgement) mergeOf {
	w := wr.w
	return mergeOf{change: code.ChainChangeOf(*j.change), target: w.Ref, tip: j.tip, head: w.ExpectedHead,
		method: w.Method, merger: w.Principal, instant: w.Instant()}
}

// mergeOnto is ONE change's merge onto m.tip in the build's scratch, every
// process charged to what is left of the build's budget: a fast-forward's
// ancestry, its commit the head itself; or a merge's or a squash's one
// merge-tree and one commit-tree, U43 (a)'s identity (mergeIdentity) at
// commitDates of m.instant over the later parent. A head the tip already
// holds, a fast-forward the tip's history refuses, a squash that changes
// nothing and a merge that conflicts are answered as the change's refusal
// (done), nothing committed — though a merge that conflicts has written its
// tree and its marked files into the scratch. It packs nothing: a
// merge_change packs its one commit, a chain every entry's at once.
func (b *building) mergeOnto(ctx context.Context, m mergeOf) (string, code.EngineWriteResult, bool, error) {
	wr := b.wr
	named := code.ChangeRef(wr.repoName(), m.change.Number)
	if m.tip == m.head {
		return "", wr.changeRefused(code.WriteNothingChanged, fmt.Sprintf("%s's head is %s's tip already: it "+
			"has nothing to merge", named, m.target)), true, nil
	}
	if m.method == code.MergeMethodFastForward {
		forward, err := b.ancestor(ctx, m.tip, m.head)
		switch {
		case err != nil:
			return "", code.EngineWriteResult{}, true, err
		case !forward:
			return "", wr.changeRefused(code.WriteNotFastForward, fmt.Sprintf("%s's head %s does not hold %s's "+
				"tip %s, so the branch cannot move to it without dropping what it holds: merge or squash it, or "+
				"rebase it onto its target first", named, shortID(m.head), m.target, shortID(m.tip))), true, nil
		}
		return m.head, code.EngineWriteResult{}, false, nil
	}
	merged, err := b.ancestor(ctx, m.head, m.tip)
	switch {
	case err != nil:
		return "", code.EngineWriteResult{}, true, err
	case merged:
		return "", wr.changeRefused(code.WriteNothingChanged, fmt.Sprintf("%s holds %s's head %s already: it "+
			"has nothing to merge", m.target, named, shortID(m.head))), true, nil
	}
	tip, head := &commitHeader{}, &commitHeader{}
	if err = b.readCommits(ctx, []string{m.tip, m.head}, []*commitHeader{tip, head}); err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	out, err := b.merge(ctx, "", m.tip, m.head)
	if err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	if !out.clean {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		res, err := b.conflicted(ctx, out, fmt.Sprintf("%s does not merge cleanly into %s at %s: %d paths "+
			"conflict; rebase it onto its target (%s) and resolve them in a checkout of the repository ("+
			toolname.CodeCheckout+")", named, m.target, shortID(m.tip), len(out.conflicts)+out.omitted,
			toolname.CodeRebaseChange))
		return "", res, true, err
	}
	if m.method == code.MergeMethodSquash && out.tree == tip.tree {
		return "", wr.changeRefused(code.WriteNothingChanged, fmt.Sprintf("%s changes nothing on %s at %s: no "+
			"commit is made", named, m.target, shortID(m.tip))), true, nil
	}
	ident, message, err := wr.mergeIdentity(m)
	if err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	parents := []string{m.tip}
	parentDate := tip.committed
	if m.method == code.MergeMethodMerge {
		parents = append(parents, m.head)
		parentDate = later(parentDate, head.committed)
	}
	ident.AuthorWhen, ident.CommitterWhen = commitDates(m.instant, parentDate)
	commit, err := b.commitOf(ctx, out.tree, parents, ident, message)
	if err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	return commit, code.EngineWriteResult{}, false, nil
}

// mergeIdentity is a merge's commit's identity and message (U43 a): a merge
// commit authored by WHO MERGED it — the write's principal, or the enqueuer
// of a chain's entry — and titled by the change; a squash authored by the
// CHANGE'S OWNER, its message the change's title and description and a
// `Co-authored-by:` trailer for every other author of it, in the order they
// first wrote it. Committed by the engine either way. A signer that names a
// part git would not write as given is the signer's bug, told before the
// commit.
func (wr *writing) mergeIdentity(m mergeOf) (*gitproc.Identity, string, error) {
	s, c := wr.s, m.change
	sig, err := s.o.Signer.Sign(m.merger)
	if err != nil {
		return nil, "", err
	}
	named := code.ChangeRef(wr.repoName(), c.Number)
	id := &gitproc.Identity{AuthorName: sig.AuthorName, AuthorEmail: sig.AuthorEmail,
		CommitterName: sig.CommitterName, CommitterEmail: sig.CommitterEmail}
	var message string
	if m.method == code.MergeMethodMerge {
		message = fmt.Sprintf("Merge %s: %s\n\nMerges %s into %s.\n", named, c.Title,
			strings.TrimPrefix(c.SourceRef, "refs/heads/"), strings.TrimPrefix(m.target, "refs/heads/"))
	} else {
		if id.AuthorName, id.AuthorEmail, err = s.o.Signer.Author(c.Owner); err != nil {
			return nil, "", err
		}
		var text strings.Builder
		fmt.Fprintf(&text, "%s (%s)\n", c.Title, named)
		if body := strings.TrimSpace(c.Body); body != "" {
			text.WriteString("\n" + body + "\n")
		}
		trailers := 0
		seen := map[string]bool{c.Owner: true}
		for _, a := range c.Authors {
			if seen[a] {
				continue
			}
			seen[a] = true
			name, email, err := s.o.Signer.Author(a)
			if err != nil {
				return nil, "", err
			}
			if trailers == 0 {
				text.WriteString("\n")
			}
			trailers++
			fmt.Fprintf(&text, "Co-authored-by: %s <%s>\n", name, email)
		}
		message = text.String()
	}
	for _, part := range []struct{ name, value string }{
		{"author name", id.AuthorName}, {"author email", id.AuthorEmail},
		{"committer name", id.CommitterName}, {"committer email", id.CommitterEmail},
	} {
		if why := gitproc.IdentityRefusal(part.value); why != "" {
			return nil, "", fmt.Errorf("host: the signer's %s %q for %s's merge %s", part.name, part.value, named,
				why)
		}
	}
	return id, gitref.WrittenCommitText(message), nil
}

// mergeDecided is the verdict of a merge record's answer (Estate.Merge) as the
// landing reads it, and the answer the write is given: applied from the
// change's row, its commit the one the target moved to; pending at its
// position; refused, with nothing recorded; and — where nobody answered —
// refused unavailable when every node said it ran nothing, so asking again
// lands it, and unknown otherwise, since a node that went silent may have.
func (s *Server) mergeDecided(ctx context.Context, l *landing, proposed []code.Command, res code.MergeResult,
	err error) (verdict, code.MergeResult) {

	id := l.Merge.Merge.MergeID
	var quiet silence
	switch {
	case err != nil && ctx.Err() != nil:
		return verdict{commands: proposed, outcome: types.AuditUnknown},
			code.MergeResult{Outcome: statelog.OutcomeUnknown, MergeID: id}
	case errors.Is(err, code.ErrInvalidProposal):
		s.log.Error("code_merge_invalid", "repo", l.Synced.Repo.Name, "merge", id, "error", err)
		return verdict{commands: proposed, outcome: types.AuditFailed},
			code.MergeResult{MergeID: id, Refusal: code.MergeRefusalUnavailable,
				Detail: "the engine refused its own merge: " + sentence(err.Error())}
	case err != nil && errors.As(err, &quiet) && !quiet.Silent():
		return verdict{commands: proposed, outcome: types.AuditRefused},
			code.MergeResult{MergeID: id, Refusal: code.MergeRefusalUnavailable,
				Detail: fmt.Sprintf("no data node took merge %s: %s; merge again", id, sentence(err.Error()))}
	case err != nil:
		s.log.Warn("code_merge_outcome_unknown", "repo", l.Synced.Repo.Name, "merge", id, "error", err)
		return verdict{commands: proposed, outcome: types.AuditUnknown},
			code.MergeResult{Outcome: statelog.OutcomeUnknown, MergeID: id}
	case res.Refusal != "":
		return verdict{commands: proposed, outcome: types.AuditRefused}, res
	case res.Outcome == statelog.OutcomePending:
		return verdict{commands: proposed, outcome: types.AuditPending, position: res.Position}, res
	case res.Outcome != statelog.OutcomeApplied:
		return verdict{commands: proposed, outcome: types.AuditUnknown}, res
	}
	moved := slices.Clone(proposed)
	for i := range moved {
		moved[i].Result = code.ResultOK
	}
	return verdict{commands: moved, outcome: types.AuditApplied, position: res.Position, applied: true}, res
}
