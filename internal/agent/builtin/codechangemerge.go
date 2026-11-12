package builtin

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE MERGE: code_merge_change merges a live change into its target by its
// method, from the head its caller read, as ONE ENGINE WRITE
// (gitlet.WriteMergeChange) the repository's Gitlet node builds and lands as one
// merge record its committer decides (M5-D10, M5-D11) — or, on a target whose
// gate requires a check, puts in that target's queue (M5-D9). It answers one
// of three things:
//
//   - merged: the commit the target moved to and where the record landed;
//   - queued: the change's place in its target's queue, from 1 — the queue's
//     landing is the merge, and a `merged` wake says when it lands;
//   - not_ready (mcp.RefusalNotReady): EVERY precondition of the target's
//     merge gate the change does not meet, each naming whom it waits on —
//     never only the first, so a caller learns in one answer everything
//     between the change and its merge, and a ready_to_merge wake follows
//     once the last is met (M5-D31). Nothing is merged or queued.
//
// Every other refusal of the merge is the committer's or the Gitlet node's, in
// its class ([mergeRefusalClass]).
//
// # Close the work item as step two
//
// Asked to (`close_task`), a DIRECT merge's change's work item is closed by a
// second write of the same operation's step ([stepTwo], statelog.StepOpID),
// once the merge applied or is pending — so the same call made again makes
// the same close, which the tracker's ledger collapses — and a close that
// fails leaves the merge merged and says why beside it ("merged; ENG-42 not
// closed: <reason>", M5-D27): the merge is the call's gesture and the close
// its consequence, and one refused is no reason to report the other undone. A
// QUEUED merge records the ask on its entry, and its landing closes the item
// as the enqueuer.
//
// # The source branch
//
// `delete_branch` is the caller's; unsaid, a source in a seat's namespace
// (refs/heads/seats/…) is deleted with the merge unless a gate over the
// target keeps it (delete_source: none, gitlet.GateRules.KeepsSource), and every
// other source is kept (Q4): a seat's branch is the engine's working copy,
// and a person's is theirs. The default is the Gitlet node's to decide, on the
// snapshot it judges the merge on ([deleteSource]), and its answer says what
// it decided (`deleted_branch`).
//
// THE DEQUEUE: code_dequeue_change takes a change's live entry out of its
// target's queue — the entry the call's one read of the change names
// ([entryOf]) — by one dequeue record its committer decides (gitlet.Dequeue),
// at the ask of its enqueuer, the change's owner, a maintainer of the
// repository or a person.

// The merge tools' names: each is its authority row's action.
const (
	CodeMergeChangeTool   = string(authz.ActionCodeMergeChange)
	CodeDequeueChangeTool = string(authz.ActionCodeDequeueChange)
)

// The verbs a merge's and a dequeue's operations are named by (m3-w1's op
// rule; M5's verbs).
const (
	mergeVerb = "code-merge"
	queueVerb = "code-queue"
)

type codeMergeChange struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeMergeChange)(nil)

func (t *codeMergeChange) Name() string { return CodeMergeChangeTool }

func (t *codeMergeChange) Description() string {
	return "Merge a change into its target from the head you read (`expected_head`): merged at once, or — where " +
		"the target's gate requires a check — queued, answered with its place, and merged when its checks pass. " +
		"A change its gate does not allow yet is refused `not_ready`, naming every precondition left and who " +
		"clears it; a ready_to_merge wake follows, so end the turn rather than ask again."
}

func (t *codeMergeChange) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":          changeRepoParam(),
			"change":        changeParam(),
			"expected_head": map[string]any{"type": "string", "description": "The change's head, as you read it."},
			"method": map[string]any{"type": "string", "enum": spelled(gitlet.MergeMethods()),
				"description": "A merge commit (the default), one squashed commit, or a fast-forward of the target."},
			"delete_branch": map[string]any{"type": "boolean", "description": "Delete the change's branch as it " +
				"merges. Unsaid: a seat's branch is deleted unless the target's rule keeps it; any other is kept."},
			"close_task": map[string]any{"type": "boolean", "description": "Close the change's work item once it " +
				"merges. Default false."},
			"override_reason": map[string]any{"type": "string", "description": "A person's reason to merge past " +
				"the target's merge gate — merged directly, audited. A seat cannot override."},
		},
		"required": []string{"change", "expected_head"},
	})
}

func (t *codeMergeChange) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeMergeChange) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.writeCaller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	name, number, refusal := changeArg(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	head := strings.TrimSpace(argString(args, "expected_head"))
	if !gitref.ValidOID(head) {
		return failed(fmt.Sprintf("%s needs `expected_head`: the change's head as %s answered it, a whole commit "+
			"id — the merge lands the head you read, and no later one.", tool, CodeGetChangeTool)), nil
	}
	method := gitlet.MergeMethod(strings.TrimSpace(argString(args, "method")))
	if method == "" {
		method = gitlet.MergeMethodMerge
	}
	if !method.Valid() {
		return failed(fmt.Sprintf("%s: `method` %q is not one of %s.", tool, method,
			words.Or(gitlet.MergeMethods()))), nil
	}
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number, false)
	if refusal != nil {
		return *refusal, nil
	}
	if !cv.change.State.Live() {
		// THE COMMITTER'S OWN WORD, refused before a merge is built: a
		// change the call's read found closed or merged is the
		// change_not_open its committer would answer, in the class and
		// under the word every other merge refusal takes.
		return mergeRefused(tool, cv.g.ref(), strings.TrimPrefix(cv.change.TargetRef, branchRoot),
			gitlet.MergeRefusalChangeNotOpen, fmt.Sprintf("%s is %s, and only a live change merges", cv.g.ref(),
				cv.change.State), nil), nil
	}
	repo := cv.g.repo
	op := opIDFor(cv.g.c.actor, tool, mergeVerb, repo.ID, args)
	w := gitlet.EngineWrite{Kind: gitlet.WriteMergeChange, RepoID: repo.ID, Principal: writer(ctx, turn, cv.g.c),
		OpID: op, Fresh: t.deps.writeFresh(turn, cv.g.c, repo), Ref: cv.change.TargetRef, ExpectedHead: head,
		Change: cv.change.Number, Method: method, DeleteSource: deleteSource(args),
		Override: strings.TrimSpace(argString(args, "override_reason")), CloseTask: argBool(args, "close_task"),
		Ceiling: cv.g.c.write}
	if err := w.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, cv.g.c.actor, repo.Name, err), nil
	}
	res, err := t.deps.Writer.GitWrite(ctx, w)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, cv.g.c, repo, op, err), nil
	}
	commit(turn, res.Outcome, types.CommittedCode)
	out := codeMergeAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Change: cv.g.ref(), Target: strings.TrimPrefix(w.Ref, branchRoot), OpID: cv.g.c.actor.Operation}
	m := res.Merge
	switch {
	case m != nil && m.Refusal != "":
		return mergeRefused(tool, cv.g.ref(), out.Target, m.Refusal, m.Detail, m.Unmet), nil
	case res.Refusal == gitlet.WriteConflict:
		return conflictedWrite(ctx, turn, cv.g.c, tool, codeBranchAnswer{Repo: repo.Name, RenamedTo: repo.Redirect,
			RedirectUntil: repo.RedirectUntil, Branch: out.Target, OpID: out.OpID}, res)
	case res.Refusal != "":
		return refusedBy(tools.RefusalInvalid, writeRefusalError(string(res.Refusal), res.Detail),
			fmt.Sprintf("%s merged nothing: %s (%s). The same call is refused again; change what it asks for.",
				tool, res.Detail, res.Refusal)), nil
	case res.Outcome == statelog.OutcomeUnknown:
		return unknownWrite(cv.g.c.actor, tool, fmt.Sprintf("the merge of %s landed", cv.g.ref()), op, false,
			unknownNext(false, sameCall(cv.g.c.actor, tool), fmt.Sprintf("Read %s with %s", cv.g.ref(),
				CodeGetChangeTool), "the same merge would be asked twice")), nil
	case res.Outcome != statelog.OutcomeApplied && res.Outcome != statelog.OutcomePending:
		return faulted(ctx, tool, fmt.Errorf("builtin: Gitlet answered a merge with no outcome and no "+
			"refusal: %+v", res), fmt.Sprintf("%s could not tell what became of the merge: %s.", tool,
			faultSaid)), nil
	case m != nil && m.Queued:
		out.State, out.Outcome, out.EntryID, out.Place, out.Position = mergeQueued, res.Outcome, m.EntryID, m.Place,
			positionText(res.Position)
		out.CloseTask = w.CloseTask
	default:
		out.State, out.Outcome, out.Commit, out.Position = mergeMerged, res.Outcome, res.Commit,
			positionText(res.Position)
		out.DeletedBranch = m != nil && m.DeleteSource
		if w.CloseTask {
			out.WorkItem, out.Residue = t.closeTask(ctx, cv, op)
		}
	}
	return newForeignRead(turn, cv.g.c.asker.Seat).page(ctx, tool, out, "")
}

// The states a merge's answer names.
const (
	mergeMerged = "merged"
	mergeQueued = "queued"
)

// codeMergeAnswer is what code_merge_change says: the change merged — the
// commit its target moved to — or queued at its place, and, for a merged
// change asked to close its work item, the item closed or why it was not.
type codeMergeAnswer struct {
	Repo          string           `json:"repo"`
	RenamedTo     string           `json:"renamed_to,omitempty"`
	RedirectUntil time.Time        `json:"redirect_until,omitzero"`
	Change        string           `json:"change"`
	Target        string           `json:"target"`
	State         string           `json:"state"`
	Outcome       statelog.Outcome `json:"outcome"`
	Commit        string           `json:"commit,omitempty"`
	DeletedBranch bool             `json:"deleted_branch,omitempty"`
	EntryID       string           `json:"entry_id,omitempty"`
	Place         int              `json:"place,omitempty"`
	// CloseTask is a queued change's recorded ask, its landing's to carry
	// out.
	CloseTask bool `json:"close_task,omitempty"`
	// WorkItem is the work item a merged change's step two closed, by its
	// key; Residue the sentence saying it was not closed and why.
	WorkItem string `json:"work_item,omitempty"`
	Residue  string `json:"residue,omitempty"`
	Position string `json:"position,omitempty"`
	OpID     string `json:"op_id,omitempty"`
}

// deleteSource is the caller's `delete_branch`, nil where it said none: the
// default — a seat's branch deleted unless a gate over the target keeps it,
// and no other ([codeMergeChange]'s doc) — is the Gitlet node's to decide, on
// the snapshot it judges the merge on, reading the gate it reads for the
// merge anyway (gitlet.EngineWrite.DeleteSource), so the call reads nothing
// for it.
func deleteSource(args map[string]any) *bool {
	if _, said := args["delete_branch"]; !said {
		return nil
	}
	deletes := argBool(args, "delete_branch")
	return &deletes
}

// closeTask is a merged change's step two: its work item closed — status
// done — as the caller, under the merge's operation's [stepTwo], and its key;
// or, where it was not, the residue saying why. An item already done is left
// as it is.
func (t *codeMergeChange) closeTask(ctx context.Context, cv conversing, op string) (string, string) {
	item := cv.change.WorkItem
	if item == nil {
		return "", fmt.Sprintf("merged; %s carries no work item to close", cv.g.ref())
	}
	label := cmp.Or(item.Key, item.Ref())
	notClosed := func(why string) (string, string) {
		return "", fmt.Sprintf("merged; %s not closed: %s", label, why)
	}
	switch {
	case item.Backend != types.WorkWorklet:
		return notClosed(fmt.Sprintf("it is a %s item, which this engine does not write — close it there",
			item.Backend))
	case t.deps.Tasks == nil || t.deps.Closer == nil:
		return notClosed("this company runs no Worklet here")
	}
	// THE CLOSE IS WORKLET'S WRITE, DECIDED AS ITS OWN TOOL'S IS: the
	// merge's row admits everybody, and update_work_item's is the Worklet
	// app's (authz.ActionWorkUpdate), so a person who does not use
	// Worklet — whose own update_work_item is refused — must not change an
	// item's status by asking a merge to. Decided for the call's principal
	// as the gate decides that tool, before anything is read.
	if err := Decide(t.deps.Chart)(ctx, authz.ActionWorkUpdate, authz.Object{}); err != nil {
		return notClosed(Refusal(UpdateWorkItemTool, err))
	}
	before, err := t.deps.Tasks.Task(ctx, item.ID, worklet.DetailWants{}, seatRead)
	if err != nil {
		return notClosed(fmt.Sprintf("its read failed (%v)", err))
	}
	if before.Task.Status.Group() == worklet.GroupDone {
		return label, ""
	}
	done := worklet.StatusDone
	after := before.Task
	after.Status = done
	got, err := t.deps.Closer(cv.g.c.actor).UpdateTask(ctx, statelog.StepOpID(op, stepTwo), before.Task.ID,
		before.Task.Project, before.Task.Version, worklet.TaskPatch{Status: &done}, worklet.ChangeStatus,
		worklet.Wake{Kind: worklet.ChangeStatus, Before: before.Task, After: after}.Notify(nil))
	switch {
	case err != nil:
		return notClosed(fmt.Sprintf("its close was refused (%v)", err))
	case got.Outcome == statelog.OutcomeUnknown:
		return notClosed(fmt.Sprintf("nobody can say whether its close landed; read it with %s", worklet.GetWorkItemTool))
	}
	return label, ""
}

// mergeRefused answers a merge, an enqueue or a dequeue its committer or its
// Gitlet node refused, in the refusal's class ([mergeRefusalClass]): a gate the
// change does not meet yet as not_ready, naming every unmet precondition and
// whom it waits on. A person's words ride beside the sentence for a model
// wherever the catalog has them ([mergeRefusalWords], [caveat.MergeGateUnmet]).
func mergeRefused(tool, change, target string, refusal gitlet.MergeRefusal, detail string,
	unmet []gitlet.Precondition) tools.Result {

	cause := writeRefusalError(string(refusal), detail)
	if refusal != gitlet.MergeRefusalGateUnmet {
		r := refusedBy(mergeRefusalClass(refusal), cause, fmt.Sprintf("%s: %s (%s).", tool, detail, refusal))
		if words := mergeRefusalWords(tool, refusal); !words.IsZero() {
			r = toPerson(r, words)
		}
		return r
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s does not yet meet %s's merge gate, so nothing was merged or queued. Unmet:", tool,
		change, target)
	for _, p := range unmet {
		fmt.Fprintf(&b, " %s — %s", p.Kind, p.Detail)
		if len(p.WaitsOn) > 0 {
			names := make([]string, len(p.WaitsOn))
			for i, w := range p.WaitsOn {
				names[i] = w.Handle
			}
			fmt.Fprintf(&b, " (waits on %s)", strings.Join(names, ", "))
		}
		b.WriteString(";")
	}
	b.WriteString(" a ready_to_merge wake follows once it does, so end the turn rather than ask again.")
	return toPerson(refusedBy(tools.RefusalNotReady, cause, b.String()), caveat.MergeGateUnmet(gateConditions(unmet)))
}

// gateConditions is unmet as the refusal catalog takes it: each precondition's
// kind and the handles of whoever it waits on, and nothing of its sentence,
// which is written for a model.
func gateConditions(unmet []gitlet.Precondition) []caveat.GateCondition {
	out := make([]caveat.GateCondition, 0, len(unmet))
	for _, p := range unmet {
		c := caveat.GateCondition{Kind: string(p.Kind)}
		for _, party := range p.WaitsOn {
			c.WaitsOn = append(c.WaitsOn, party.Handle)
		}
		out = append(out, c)
	}
	return out
}

// mergeRefusalWords is a person's words for the merge refusals the change
// page's controls leave to the engine (the dashboard's
// MERGE_REFUSAL_SENTENCES draws the engine's words for them): a merge that
// would delete a branch it may not, a repository at its size, and a merge or
// a dequeue asked by or for somebody who may not make it. The gate's own is
// [caveat.MergeGateUnmet]. Every other merge refusal is answered in the
// controls' own words on the change page, by the host's word beside its class,
// and in its code's own sentence everywhere else: the floor ADR-0060 sets.
func mergeRefusalWords(tool string, refusal gitlet.MergeRefusal) caveat.Refusal {
	switch refusal {
	case gitlet.MergeRefusalSourceRefused:
		return caveat.MergeWouldDeleteAKeptBranch()
	case gitlet.MergeRefusalRepoSize:
		return caveat.RepositoryFull()
	case gitlet.MergeRefusalNotPermitted:
		if tool == CodeDequeueChangeTool {
			return caveat.QueueEntryNotYours()
		}
		return caveat.MergeAskedForSomebodyWhoMayNot()
	}
	return caveat.Refusal{}
}

// mergeRefusalClass is the class a merge refusal is answered in, by
// internal/mcp's one rule for every domain ([tools.Refusal]'s doc) — what the
// refusal is about and what would make the same request land — which a push's
// ([pushClass]), a change's ([changeClass]) and a conversation's
// ([conversationClass]) follow too, so one state is one class on every code
// tool:
//
//   - NOT_READY: a gate the change does not meet yet, which its people
//     clear and a ready_to_merge wake reports;
//   - STALE_VERSION: a head, a target or a change that moved since its
//     caller read it — read again;
//   - CONFLICT: the change or its repository in a state that refuses the
//     merge until somebody changes it — a change closed or merged, a draft its
//     owner or a person marks ready, a repository archived or a pull mirror —
//     a merge that lost its race to a target moving faster than a merge is
//     built onto it, and a queue holding all it may, its contents at their
//     cap until it lands what it holds;
//   - QUOTA_EXCEEDED: a repository the merge would take past
//     code.limits.repo_max_bytes, as a push's repo_size is, which no
//     permission lifts and no retry clears;
//   - UNAVAILABLE: what waiting on the node clears, as a push's is, and in
//     the same class wherever Gitlet answers it — a committer busy,
//     a repository its compaction has not caught up with, an import
//     finishing, an expunge that began, a pack the merge's own builder
//     uploaded too long ago (it builds it again), a log that refused or a
//     record applied nowhere;
//   - FORBIDDEN: a rule refusing the caller — a seat asking a person's
//     override, an asker the repository or the change does not let;
//   - NOT_FOUND: a repository, a change or a queue entry that is not there —
//     a dequeue of a change no queue holds live among them, which a re-read
//     or a retry never brings back;
//   - EXISTS: a change its target's queue already holds;
//   - INVALID: the request itself, which only another call clears — a queue
//     target asked directly, a target with no queue asked to queue, a source
//     branch the merger may not delete — and the queue's own records'
//     refusals (not_green, not_red, no_landing, record_full), which its home
//     Gitlet node answers and no tool's call is ever refused.
//
// NEVER INVALID OR FORBIDDEN FOR A STATE: a merge of a closed change or into
// an archived repository asks nothing wrong of a caller who may ask it, so
// "send something different" and "you may not" both mislead its reader; the
// object refuses it, and the same request lands once somebody changes that
// (or, for a change merged, never). Every value is listed, so a refusal the
// domain adds is examined here
// (TestEveryMergeRefusalIsClassedByWhatClearsIt walks gitlet.MergeRefusals());
// one off the wire this build does not know is invalid, the class that says
// nothing more specific.
func mergeRefusalClass(r gitlet.MergeRefusal) tools.Refusal {
	switch r {
	case gitlet.MergeRefusalGateUnmet:
		return tools.RefusalNotReady
	case gitlet.MergeRefusalStale, gitlet.MergeRefusalHeadMoved, gitlet.MergeRefusalTargetMoved:
		return tools.RefusalStaleVersion
	case gitlet.MergeRefusalChangeNotOpen, gitlet.MergeRefusalChangeDraft, gitlet.MergeRefusalRepoArchived,
		gitlet.MergeRefusalRepoMirrored, gitlet.MergeRefusalTargetMoving, gitlet.MergeRefusalQueueFull:
		return tools.RefusalConflict
	case gitlet.MergeRefusalRepoSize:
		return tools.RefusalQuotaExceeded
	case gitlet.MergeRefusalBusy, gitlet.MergeRefusalCompactionBehind, gitlet.MergeRefusalRepoImporting,
		gitlet.MergeRefusalExpunging, gitlet.MergeRefusalUploadExpired, gitlet.MergeRefusalUnavailable,
		gitlet.MergeRefusalNotApplied:
		return tools.RefusalUnavailable
	case gitlet.MergeRefusalNotPerson, gitlet.MergeRefusalNotPermitted:
		return tools.RefusalForbidden
	case gitlet.MergeRefusalRepoDeleted, gitlet.MergeRefusalChangeNotFound, gitlet.MergeRefusalNotQueued:
		return tools.RefusalNotFound
	case gitlet.MergeRefusalAlreadyQueued:
		return tools.RefusalExists
	case gitlet.MergeRefusalQueueTarget, gitlet.MergeRefusalNoQueue, gitlet.MergeRefusalSourceRefused,
		gitlet.MergeRefusalNotGreen, gitlet.MergeRefusalNotRed, gitlet.MergeRefusalNoLanding,
		gitlet.MergeRefusalRecordFull:
		return tools.RefusalInvalid
	}
	return tools.RefusalInvalid
}

type codeDequeueChange struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeDequeueChange)(nil)

func (t *codeDequeueChange) Name() string { return CodeDequeueChangeTool }

func (t *codeDequeueChange) Description() string {
	return "Take a queued change out of its target's merge queue: its enqueuer, its owner, a maintainer of the " +
		"repository or a person may. The change stays open, and merges again only when it is merged again."
}

func (t *codeDequeueChange) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type":       "object",
		"properties": map[string]any{"repo": changeRepoParam(), "change": changeParam()},
		"required":   []string{"change"},
	})
}

func (t *codeDequeueChange) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeDequeueChange) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.writeCaller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	name, number, refusal := changeArg(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number, true)
	if refusal != nil {
		return *refusal, nil
	}
	g := cv.g
	entry, refusal := entryOf(cv)
	if refusal != nil {
		return *refusal, nil
	}
	op := opIDFor(g.c.actor, tool, queueVerb, g.repo.ID, args)
	asker := writer(ctx, turn, g.c)
	// AN ENTRY IS ITS ENQUEUER'S, NEVER A TURN'S: the committer judges the
	// asker as a party, so the dequeue names no run, as the enqueue does —
	// but it carries whom the turn works for (the write's ceiling), which
	// the committer holds the dequeue to beside the seat itself.
	asker.Run = gitlet.RunProvenance{}
	p := gitlet.QueueProposal{OpID: op, Asker: asker, Seen: cv.at, Ceiling: g.c.write,
		Dequeue: &gitlet.Dequeue{RepoID: g.repo.ID, EntryID: entry}}
	if err := p.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, g.c.actor, g.repo.Name, err), nil
	}
	res, err := t.deps.Writer.Queue(ctx, p)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, g.c, g.repo, op, err), nil
	}
	commit(turn, res.Outcome, types.CommittedCode)
	target := strings.TrimPrefix(cv.change.TargetRef, branchRoot)
	switch {
	case res.Refusal != "":
		return mergeRefused(tool, g.ref(), target, res.Refusal, res.Detail, res.Unmet), nil
	case res.Outcome == statelog.OutcomeUnknown:
		return unknownWrite(g.c.actor, tool, fmt.Sprintf("%s left its queue", g.ref()), op, false,
			unknownNext(false, sameCall(g.c.actor, tool), fmt.Sprintf("Read %s with %s", g.ref(),
				CodeGetChangeTool), "")), nil
	}
	out := codeMergeAnswer{Repo: g.repo.Name, RenamedTo: g.repo.Redirect, RedirectUntil: g.repo.RedirectUntil,
		Change: g.ref(), Target: target, State: string(gitlet.QueueDequeued), Outcome: res.Outcome, EntryID: entry,
		Position: positionText(res.Position), OpID: g.c.actor.Operation}
	return newForeignRead(turn, g.c.asker.Seat).page(ctx, tool, out, "")
}

// entryOf is the change's LIVE entry in its target's queue as the call's one
// read of the change named it — on its readiness's queue precondition
// (gitlet.Precondition.Queue), which that read resolves over the change's own
// index — or the refusal of a change no queue holds. The dequeue reads
// nothing more: its committer decides it at or past that read
// (gitlet.QueueProposal.Seen), so an entry the queue landed or ejected since is
// the committer's to refuse, from its own snapshot. A change no queue holds
// is the committer's not_queued, refused before anything is asked: not
// found, since an entry that is not live — never queued, landed or ejected —
// never becomes live again by itself.
func entryOf(cv conversing) (string, *tools.Result) {
	if r := cv.change.Readiness; r != nil {
		for _, p := range r.Preconditions {
			if p.Kind == gitlet.PreconditionQueue && p.Queue != nil {
				return p.Queue.EntryID, nil
			}
		}
	}
	g := cv.g
	target := strings.TrimPrefix(cv.change.TargetRef, branchRoot)
	r := mergeRefused(g.tool, g.ref(), target, gitlet.MergeRefusalNotQueued,
		fmt.Sprintf("%s is in no queue of %s, so nothing was taken out", g.ref(), target), nil)
	return "", &r
}
