package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S MERGE IS BUILT HERE AND LANDS AS ONE MERGE RECORD (ADR-0066,
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
// named), and a seat's override (not_person). Every such refusal is judged
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
// queue's to land, or a quieter moment's.

// mergeCarrying is p carrying pack — the bundle its landing stored, nil for a
// merge that stores none — as its record names it.
func mergeCarrying(p code.MergeProposal, pack *code.PackRef) code.MergeProposal {
	p.Merge.Pack = pack
	return p
}

// mergeJudgement is what a merge's judgement read: the change, the target's
// tip in the copy, and whether the target lands its merges through its queue.
type mergeJudgement struct {
	change *code.ChangeDetail
	tip    string
	queue  bool
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

// runMerge is a merge_change's steps from its sync on: judged, built, landed —
// and built again onto a target that moved under it, at most
// code.MergeRecomputes times.
func (wr *writing) runMerge(ctx context.Context) (code.EngineWriteResult, error) {
	s := wr.s
	fresh := wr.fresh()
	for recomputes := 0; ; recomputes++ {
		syncStart := s.now()
		j, res, done, err := wr.judgeMerge(ctx, fresh)
		wr.syncTook += s.now().Sub(syncStart)
		if done || err != nil {
			return withRecomputes(res, recomputes), err
		}
		if j.queue {
			res, err := wr.enqueue(ctx, j)
			return withRecomputes(res, recomputes), err
		}
		res, moved, err := wr.buildAndLandMerge(ctx, j)
		if err != nil || !moved {
			return withRecomputes(res, recomputes), err
		}
		if recomputes == code.MergeRecomputes {
			return withRecomputes(code.EngineWriteResult{Merge: &code.MergeAnswer{
				Refusal: code.MergeRefusalTargetMoving,
				Detail: fmt.Sprintf("%s moved under this merge's build %d times running: it moves faster than a "+
					"merge is built onto it — merge again once it is quieter", wr.w.Ref, recomputes+1)}},
				recomputes), nil
		}
		// THE TIP THE DECIDE HELD is at or past the log's head as this node
		// reads it next: built again onto what a linearizable sync finds.
		fresh = statelog.Freshness{Level: statelog.ReadLinearizable}
	}
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
			if res, found, err := wr.outcome(ctx); err != nil || found {
				if found && res.Outcome != "" {
					res.Merge = &code.MergeAnswer{}
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

// mergeVerdict judges the merge on the synced copy, in the order its record's
// decide reads its rules: the repository's status, the change as the caller
// read it, a seat's override, the target's tip, and — for a merge its gate
// holds — whether its target is a queue's and whether its readiness, with its
// asker as the merger, is met but for the merge its build proves.
func (wr *writing) mergeVerdict(ctx context.Context, gone bool) (mergeJudgement, mergeVerdict, error) {
	s, w := wr.s, wr.w
	repo := wr.synced.Repo
	if gone {
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalRepoDeleted,
			detail: code.RepoGone{ID: w.RepoID, Retention: s.o.DeletedRetention()}.Detail()}, nil
	}
	switch reason, words := s.statusRefusal(repo); reason {
	case code.RefusalRepoDeleted:
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalRepoDeleted, detail: words}, nil
	case code.RefusalRepoArchived:
		return mergeJudgement{}, mergeVerdict{refusal: code.MergeRefusalRepoArchived, detail: words}, nil
	}
	floor := statelog.FloorAt(statelog.ReadSession, wr.synced.Position)
	ans, err := s.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadChange, Asker: w.Principal, Repo: repo.Name,
		Change: w.Change, Fresh: floor})
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
	if w.Override != "" {
		return j, mergeVerdict{}, nil
	}
	if j.queue, err = wr.queueTarget(ctx, floor); err != nil || j.queue {
		return j, mergeVerdict{}, err
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

// queueTarget reports a target whose merges land through its queue: one some
// rule protects, whose gates — read from the repository's detail, which its
// row never carries — require a check. A target no rule protects has no gate,
// and is never read for one.
func (wr *writing) queueTarget(ctx context.Context, floor statelog.Freshness) (bool, error) {
	w, repo := wr.w, wr.synced.Repo
	protected := false
	for _, rule := range repo.Protections {
		if pattern, ok := gitref.ParsePattern(rule.Pattern); !ok || pattern.Match(w.Ref) {
			// A RULE THIS BUILD CANNOT READ is read with its gate, whose
			// judgement refuses it: never a target taken for unprotected.
			protected = true
			break
		}
	}
	if !protected {
		return false, nil
	}
	ans, err := wr.s.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadRepo, Asker: w.Principal, Repo: repo.Name,
		Gates: true, Fresh: floor})
	if err != nil {
		return false, err
	}
	if ans.Repo == nil {
		return false, fmt.Errorf("host: the read of repository %s's gates answered no repository", repo.Name)
	}
	rules := make([]code.Protection, len(ans.Repo.Gates))
	for i, g := range ans.Repo.Gates {
		rules[i] = code.Protection{PushRule: code.PushRule{Pattern: g.Pattern}, Gate: &g.Gate}
	}
	gate, err := code.GateRulesOf(rules, w.Ref, "")
	if err != nil {
		return false, err
	}
	return gate.QueueTarget(), nil
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
			HeadOID: w.ExpectedHead, Method: w.Method, DeleteSource: w.DeleteSource, CloseTask: w.CloseTask,
			AutoRevert: w.AutoRevert}})
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
			Queued: true}}, nil
	case res.Refusal != "":
		return code.EngineWriteResult{Position: res.Position, Merge: &code.MergeAnswer{EntryID: entry,
			Refusal: res.Refusal, Detail: res.Detail, Unmet: res.Unmet}}, nil
	}
	return code.EngineWriteResult{Outcome: res.Outcome, Position: res.Position, Merge: &code.MergeAnswer{
		Queued: true, EntryID: res.EntryID, Place: res.Place}}, nil
}

// buildAndLandMerge is one round's build and landing: the merge built in its
// scratch onto j.tip under the merge's budget and one unit of the node's seat
// git-work share, then the deadline rule and the one landing with its record.
// moved reports a record refused target_moved: the merge is the caller's to
// build again.
func (wr *writing) buildAndLandMerge(ctx context.Context, j mergeJudgement) (res code.EngineWriteResult,
	moved bool, err error) {

	s, w := wr.s, wr.w
	wr.cmd = code.Command{Ref: w.Ref, Old: j.tip}
	sc, err := s.o.Cache.NewScratch(ctx, wr.synced, wr.pushID)
	if err != nil {
		if errors.Is(err, cache.ErrStopped) {
			return code.EngineWriteResult{}, false, s.writesStopping()
		}
		return code.EngineWriteResult{}, false, fmt.Errorf("host: a scratch for merge %s: %w", wr.pushID, err)
	}
	defer sc.Remove()

	buildStart := s.now()
	commit, res, done, err := wr.buildMergeShared(ctx, sc, j)
	wr.buildTook += s.now().Sub(buildStart)
	if done || err != nil {
		return res, false, err
	}
	if err := s.fault(ctx, codefault.MergeAfterBuild, 0); err != nil {
		return code.EngineWriteResult{}, false, fmt.Errorf("host: merge %s after its build: %w", wr.pushID, err)
	}
	wr.cmd.New = commit
	if res, refused, err := wr.deadlineRule(ctx, sc); refused || err != nil {
		return res, false, err
	}
	wr.landed = true
	landStart := s.now()
	l, err := s.land(ctx, landing{Synced: wr.synced, Scratch: sc, Commands: []code.Command{wr.cmd},
		Principal: w.Principal, Via: code.ViaMerge, PushID: wr.pushID, ReceivedAt: s.now(),
		Merge: &code.MergeProposal{OpID: w.OpID, Asker: w.Principal, Seen: wr.synced.Position,
			Merge: code.Merge{RepoID: w.RepoID, MergeID: wr.pushID, ChangeID: j.change.ID, Method: w.Method,
				TargetRef: w.Ref, BaseOID: j.tip, HeadOID: w.ExpectedHead, CommitOID: commit,
				DeleteSource: w.DeleteSource, Override: w.Override, Node: s.o.NodeID, ReceivedAt: s.now()}}})
	wr.landTook += s.now().Sub(landStart)
	if err != nil {
		return code.EngineWriteResult{}, false, err
	}
	if l.Merge == nil {
		// REFUSED BY THE LANDING BEFORE ITS PROPOSAL — the store full or
		// unavailable, the pack past a bound — as a push's is.
		return code.EngineWriteResult{Command: l.Commands[0]}, false, nil
	}
	m := l.Merge
	if m.Refusal == code.MergeRefusalTargetMoved {
		return code.EngineWriteResult{}, true, nil
	}
	res = code.EngineWriteResult{Outcome: m.Outcome, Position: m.Position, Merge: &code.MergeAnswer{
		Refusal: m.Refusal, Detail: m.Detail, Unmet: m.Unmet}}
	switch m.Outcome {
	case statelog.OutcomeApplied:
		res.Commit = m.MergeOID
	case statelog.OutcomePending:
		res.Commit = commit
	}
	return res, false, nil
}

// buildMergeShared is a merge's build under one unit of the node's seat
// git-work share and its kind's budget of git run time
// (code.MergeBuildDeadline): a fast-forward's ancestry, or a merge's or a
// squash's merge, commit and pack. Past code.GitShareWait the share is
// unready, about this node; a build that runs its whole budget is too_large.
func (wr *writing) buildMergeShared(ctx context.Context, sc *cache.Scratch, j mergeJudgement) (string,
	code.EngineWriteResult, bool, error) {

	s, w := wr.s, wr.w
	release, err := s.o.Share.Take(ctx, code.SeatWorkWrite)
	if err != nil {
		if code.AnswerDue(ctx) {
			err = wr.unready(code.UnreadyShare, fmt.Sprintf("%s's seat git work held its whole share until the "+
				"merge's answer fell due, its asker's attempt spent; another git node builds it", s.o.NodeID))
		}
		return "", code.EngineWriteResult{}, true, err
	}
	defer release()
	wr.metered, wr.left = true, s.buildBudget(w.Kind)
	commit, res, done, err := wr.buildMerge(ctx, sc, j)
	wr.metered = false
	if errors.Is(err, errBuildSpent) {
		return "", wr.spent(), true, nil
	}
	return commit, res, done, err
}

// buildMerge is the merge's build in sc, onto j.tip: see this file's head.
func (wr *writing) buildMerge(ctx context.Context, sc *cache.Scratch, j mergeJudgement) (string,
	code.EngineWriteResult, bool, error) {

	w := wr.w
	b, err := wr.newBuilding(sc)
	if err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	named := code.ChangeRef(wr.repoName(), j.change.Number)
	if j.tip == w.ExpectedHead {
		return "", wr.changeRefused(code.WriteNothingChanged, fmt.Sprintf("%s's head is %s's tip already: it "+
			"has nothing to merge", named, w.Ref)), true, nil
	}
	if w.Method == code.MergeMethodFastForward {
		forward, err := b.ancestor(ctx, j.tip, w.ExpectedHead)
		switch {
		case err != nil:
			return "", code.EngineWriteResult{}, true, err
		case !forward:
			return "", wr.changeRefused(code.WriteNotFastForward, fmt.Sprintf("%s's head %s does not hold %s's "+
				"tip %s, so the branch cannot move to it without dropping what it holds: merge or squash it, or "+
				"rebase it onto its target first", named, shortID(w.ExpectedHead), w.Ref, shortID(j.tip))), true, nil
		}
		return w.ExpectedHead, code.EngineWriteResult{}, false, nil
	}
	merged, err := b.ancestor(ctx, w.ExpectedHead, j.tip)
	switch {
	case err != nil:
		return "", code.EngineWriteResult{}, true, err
	case merged:
		return "", wr.changeRefused(code.WriteNothingChanged, fmt.Sprintf("%s holds %s's head %s already: it "+
			"has nothing to merge", w.Ref, named, shortID(w.ExpectedHead))), true, nil
	}
	tip, head := &commitHeader{}, &commitHeader{}
	if err := b.readCommits(ctx, []string{j.tip, w.ExpectedHead}, []*commitHeader{tip, head}); err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	m, err := b.merge(ctx, "", j.tip, w.ExpectedHead)
	if err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	if !m.clean {
		res, err := b.conflicted(ctx, m, fmt.Sprintf("%s does not merge cleanly into %s at %s: %d paths "+
			"conflict; rebase it onto its target (code_rebase_change) and resolve them in a coding box "+
			"(run_sandbox)", named, w.Ref, shortID(j.tip), len(m.conflicts)+m.omitted))
		return "", res, true, err
	}
	if w.Method == code.MergeMethodSquash && m.tree == tip.tree {
		return "", wr.changeRefused(code.WriteNothingChanged, fmt.Sprintf("%s changes nothing on %s at %s: no "+
			"commit is made", named, w.Ref, shortID(j.tip))), true, nil
	}
	ident, message, err := wr.mergeIdentity(j)
	if err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	parents := []string{j.tip}
	parentDate := tip.committed
	if w.Method == code.MergeMethodMerge {
		parents = append(parents, w.ExpectedHead)
		parentDate = later(parentDate, head.committed)
	}
	ident.AuthorWhen, ident.CommitterWhen = commitDates(w.Instant(), parentDate)
	commit, err := b.commitOf(ctx, m.tree, parents, ident, message)
	if err != nil {
		return "", code.EngineWriteResult{}, true, err
	}
	if res, done, err := b.pack(ctx); done || err != nil {
		return "", res, true, err
	}
	return commit, code.EngineWriteResult{}, false, nil
}

// mergeIdentity is a merge's commit's identity and message (U43 a): a merge
// commit authored by WHO MERGED it — the write's principal — and titled by
// the change; a squash authored by the CHANGE'S OWNER, its message the
// change's title and description and a `Co-authored-by:` trailer for every
// other author of it, in the order they first wrote it. Committed by the
// engine either way. A signer that names a part git would not write as given
// is the signer's bug, told before the commit.
func (wr *writing) mergeIdentity(j mergeJudgement) (*gitproc.Identity, string, error) {
	s, w, c := wr.s, wr.w, j.change
	sig, err := s.o.Signer.Sign(w.Principal)
	if err != nil {
		return nil, "", err
	}
	named := code.ChangeRef(wr.repoName(), c.Number)
	id := &gitproc.Identity{AuthorName: sig.AuthorName, AuthorEmail: sig.AuthorEmail,
		CommitterName: sig.CommitterName, CommitterEmail: sig.CommitterEmail}
	var message string
	if w.Method == code.MergeMethodMerge {
		message = fmt.Sprintf("Merge %s: %s\n\nMerges %s into %s.\n", named, c.Title,
			strings.TrimPrefix(c.SourceRef, "refs/heads/"), strings.TrimPrefix(c.TargetRef, "refs/heads/"))
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
			if seen[a.Author] {
				continue
			}
			seen[a.Author] = true
			name, email, err := s.o.Signer.Author(a.Author)
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
