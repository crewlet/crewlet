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
// method, from the head its caller read, as ONE engine write
// (gitlet.WriteMergeChange) landing one merge record its committer decides
// (M5-D10, M5-D11), or, where the target's gate requires a check, queues it
// (M5-D9). It answers merged (the commit and where the record landed), queued
// (its place from 1; a `merged` wake says when it lands) or not_ready
// (mcp.RefusalNotReady): EVERY unmet precondition of the gate and whom each
// waits on, never only the first, then a ready_to_merge wake once the last is
// met (M5-D31). Every other refusal is the committer's or the Gitlet node's, in
// its class ([mergeRefusalClass]).

// `close_task`: a DIRECT merge's work item is closed by a second write under
// the same operation ([stepTwo], statelog.StepOpID) once the merge applied or
// is pending, so the same call made again makes the same close. A failed close
// leaves the merge merged and says why beside it (M5-D27): the close is the
// merge's consequence, and one refused is no reason to report the other
// undone. A QUEUED merge records the ask on its entry; its landing closes the
// item as the enqueuer.

// `delete_branch`, unsaid: a source under refs/heads/seats/ is deleted unless a
// gate over the target keeps it (gitlet.GateRules.KeepsSource), and any other
// is kept (Q4): a seat's branch is the engine's working copy, a person's is
// theirs. The Gitlet node decides the default ([deleteSource]) and answers it
// as `deleted_branch`.

// THE DEQUEUE: code_dequeue_change takes the change's live entry ([entryOf])
// out of its target's queue by one dequeue record its committer decides
// (gitlet.Dequeue), at the ask of its enqueuer, the change's owner, a
// repository maintainer or a person.

// The merge tools' names: each is its authority row's action.
const (
	CodeMergeChangeTool   = string(authz.ActionCodeMergeChange)
	CodeDequeueChangeTool = string(authz.ActionCodeDequeueChange)
)

// The verbs a merge's and a dequeue's operations are named by ([opIDFor]).
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
		// Refused before a merge is built, as the change_not_open its
		// committer would answer, in that refusal's class and word.
		return mergeRefused(tool, cv.g.ref(), strings.TrimPrefix(cv.change.TargetRef, branchRoot),
			gitlet.MergeRefusalChangeNotOpen, fmt.Sprintf("%s is %s, and only a live change merges", cv.g.ref(),
				cv.change.State), nil), nil
	}
	repo := cv.g.repo
	op := opIDFor(cv.g.c.actor, tool, mergeVerb, repo.ID, args)
	w := written(ctx, turn, cv.g.c, gitlet.EngineWrite{Kind: gitlet.WriteMergeChange, RepoID: repo.ID,
		OpID: op, Fresh: t.deps.writeFresh(turn, cv.g.c, repo), Ref: cv.change.TargetRef, ExpectedHead: head,
		Change: cv.change.Number, Method: method, DeleteSource: deleteSource(args),
		Override: strings.TrimSpace(argString(args, "override_reason")), CloseTask: argBool(args, "close_task")})
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
// default is the Gitlet node's to decide on the snapshot it judges the merge
// on, from the gate it reads anyway (gitlet.EngineWrite.DeleteSource), so the
// call reads nothing for it.
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
	// Decided as update_work_item is (authz.ActionWorkUpdate), before
	// anything is read: the merge's row admits everybody, and a person
	// refused update_work_item must not change an item's status through a
	// merge.
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
// MERGE_REFUSAL_SENTENCES draws on them): a branch deletion refused, a
// repository at its size, and a merge or dequeue by or for somebody who may
// not. The gate's own is [caveat.MergeGateUnmet]; every other refusal takes
// the controls' own words on the change page and its code's own sentence
// elsewhere, the floor ADR-0060 sets.
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

// mergeRefusalClass is a merge refusal's class by [tools.Refusal]'s one rule,
// what would make the same request land, which [pushClass], [changeClass] and
// [conversationClass] follow too, so one state is one class on every code tool.
// A state of the change or repository (closed, merged, draft, archived,
// mirrored, a target moving faster than a merge builds, a full queue) is
// CONFLICT, never INVALID or FORBIDDEN: the caller asked nothing wrong, and the
// same request lands once somebody changes the object. Waiting on the node is
// UNAVAILABLE and code.limits.repo_max_bytes QUOTA_EXCEEDED, as a push's are;
// an entry no queue holds is NOT_FOUND, since no retry revives it; the queue
// records' own refusals (not_green, …) are INVALID, as no tool call meets them.
// Every value is listed (TestEveryMergeRefusalIsClassedByWhatClearsIt walks
// gitlet.MergeRefusals()); one this build does not know is INVALID.
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
	// An entry is its enqueuer's, never a turn's: the committer judges the
	// asker as a party, so the dequeue names no run, as the enqueue does,
	// but keeps the turn's Ceiling, which the committer holds it to beside
	// the seat, and WrittenFor.
	asker.Run = gitlet.RunProvenance{}
	p := gitlet.QueueProposal{OpID: op, Asker: asker, Seen: cv.at, Ceiling: g.c.write,
		WrittenFor: g.c.actor.WrittenFor, Dequeue: &gitlet.Dequeue{RepoID: g.repo.ID, EntryID: entry}}
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

// entryOf is the change's LIVE queue entry as the call's one read named it,
// on its readiness's queue precondition (gitlet.Precondition.Queue), or the
// committer's not_queued refused before anything is asked. Nothing more is
// read: the committer decides at or past that read (gitlet.QueueProposal.Seen)
// and refuses an entry landed or ejected since.
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
