package builtin

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE MERGE: code_merge_change merges a live change into its target by its
// method, from the head its caller read, as ONE ENGINE WRITE
// (code.WriteMergeChange) the repository's git node builds and lands as one
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
// Every other refusal of the merge is the committer's or the git node's, in
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
// target keeps it (delete_source: none, code.GateRules.KeepsSource), and every
// other source is kept (Q4): a seat's branch is the engine's working copy,
// and a person's is theirs.
//
// THE DEQUEUE: code_dequeue_change takes a change's live entry out of its
// target's queue — one dequeue record its committer decides (code.Dequeue),
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
			"method": map[string]any{"type": "string", "enum": spelled(code.MergeMethods()),
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
	method := code.MergeMethod(strings.TrimSpace(argString(args, "method")))
	if method == "" {
		method = code.MergeMethodMerge
	}
	if !method.Valid() {
		return failed(fmt.Sprintf("%s: `method` %q is not one of %s.", tool, method,
			strings.Join(spelled(code.MergeMethods()), ", "))), nil
	}
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number)
	if refusal != nil {
		return *refusal, nil
	}
	if !cv.change.State.Live() {
		return refusedBy(tools.RefusalConflict, fmt.Errorf("code: %s is %s", cv.g.ref(), cv.change.State),
			fmt.Sprintf("%s: %s is %s, and only a live change merges.", tool, cv.g.ref(), cv.change.State)), nil
	}
	repo := cv.g.repo
	deleteSource, refusal := t.deleteSource(ctx, cv, args)
	if refusal != nil {
		return *refusal, nil
	}
	op := opIDFor(cv.g.c.actor, tool, mergeVerb, repo.ID, args)
	w := code.EngineWrite{Kind: code.WriteMergeChange, RepoID: repo.ID, Principal: writer(ctx, turn, cv.g.c),
		OpID: op, Fresh: t.deps.writeFresh(turn, cv.g.c, repo), Ref: cv.change.TargetRef, ExpectedHead: head,
		Change: cv.change.Number, Method: method, DeleteSource: deleteSource,
		Override: strings.TrimSpace(argString(args, "override_reason")), CloseTask: argBool(args, "close_task"),
		Ceiling: cv.g.c.write}
	if err := w.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, cv.g.c.actor, repo.Name, err), nil
	}
	res, err := t.deps.Writer.GitWrite(ctx, w)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, cv.g.c, repo, op, err), nil
	}
	out := codeMergeAnswer{Repo: repo.Name, RenamedTo: repo.Redirect, RedirectUntil: repo.RedirectUntil,
		Change: cv.g.ref(), Target: strings.TrimPrefix(w.Ref, branchRoot), OpID: cv.g.c.actor.Operation}
	m := res.Merge
	switch {
	case m != nil && m.Refusal != "":
		return mergeRefused(tool, cv.g.ref(), out.Target, m.Refusal, m.Detail, m.Unmet), nil
	case res.Refusal == code.WriteConflict:
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
		return faulted(ctx, tool, fmt.Errorf("builtin: the code host answered a merge with no outcome and no "+
			"refusal: %+v", res), fmt.Sprintf("%s could not tell what became of the merge: %s.", tool,
			faultSaid)), nil
	case m != nil && m.Queued:
		out.State, out.Outcome, out.EntryID, out.Place, out.Position = mergeQueued, res.Outcome, m.EntryID, m.Place,
			positionText(res.Position)
		out.CloseTask = w.CloseTask
	default:
		out.State, out.Outcome, out.Commit, out.Position = mergeMerged, res.Outcome, res.Commit,
			positionText(res.Position)
		out.DeletedBranch = deleteSource
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

// deleteSource is whether the merge deletes the change's source branch: the
// caller's `delete_branch`, or — unsaid — a seat's branch unless a gate over
// the target keeps it, and no other ([codeMergeChange]'s doc). The gates are
// read as the git node reads them for the merge (code.ReadRepo with its
// gates), at the caller's floor.
func (t *codeMergeChange) deleteSource(ctx context.Context, cv conversing, args map[string]any) (bool,
	*tools.Result) {

	if _, said := args["delete_branch"]; said {
		return argBool(args, "delete_branch"), nil
	}
	return seatSourceDeleted(ctx, cv.g, cv.change.SourceRef, cv.change.TargetRef)
}

// seatSourceDeleted is whether a merge of source into target deletes source
// where its caller said nothing: a seat's branch unless a gate over the
// target keeps it, and no other (Q4) — read as the git node reads the gates
// for the merge (code.ReadRepo with its gates), at the caller's floor. A
// merge's default and a revert's step two take it alike.
func seatSourceDeleted(ctx context.Context, g changeRead, source, target string) (bool, *tools.Result) {
	if !strings.HasPrefix(source, gitref.SeatsRoot) {
		return false, nil
	}
	answer, err := g.deps.read(ctx, g.c, code.ReadQuery{Kind: code.ReadRepo, Repo: g.repo.Name, Gates: true,
		Fresh: seatRead})
	if err != nil {
		r := g.deps.failed(ctx, g.turn, g.tool, g.c.asker, g.repo, err)
		return false, &r
	}
	if answer.Repo == nil {
		r := faulted(ctx, g.tool, fmt.Errorf("builtin: the code host answered repository %s's gates with no "+
			"repository", g.repo.Name), fmt.Sprintf("%s could not read %s's merge rules: %s.", g.tool,
			g.repo.Name, faultSaid))
		return false, &r
	}
	rules := make([]code.Protection, len(answer.Repo.Gates))
	for i, rg := range answer.Repo.Gates {
		rules[i] = code.Protection{PushRule: code.PushRule{Pattern: rg.Pattern}, Gate: &rg.Gate}
	}
	gate, err := code.GateRulesOf(rules, target, "")
	if err != nil {
		r := faulted(ctx, g.tool, err, fmt.Sprintf("%s could not read %s's merge rules: %s.", g.tool,
			g.repo.Name, faultSaid))
		return false, &r
	}
	return !gate.KeepsSource, nil
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
	case item.Backend != types.WorkNative:
		return notClosed(fmt.Sprintf("it is a %s item, which this engine does not write — close it there",
			item.Backend))
	case t.deps.Tasks == nil || t.deps.Closer == nil:
		return notClosed("this company runs no native tracker here")
	}
	// THE CLOSE IS THE TRACKER'S WRITE, DECIDED AS ITS OWN TOOL'S IS: the
	// merge's row admits everybody, and update_work_item's is the tracker
	// app's (authz.ActionWorkUpdate), so a person who does not use the
	// tracker — whose own update_work_item is refused — must not change an
	// item's status by asking a merge to. Decided for the call's principal
	// as the gate decides that tool, before anything is read.
	if err := Decide(t.deps.Chart)(ctx, authz.ActionWorkUpdate, authz.Object{}); err != nil {
		return notClosed(Refusal(UpdateWorkItemTool, err))
	}
	before, err := t.deps.Tasks.Task(ctx, item.ID, tracker.DetailWants{}, seatRead)
	if err != nil {
		return notClosed(fmt.Sprintf("its read failed (%v)", err))
	}
	if before.Task.Status.Group() == tracker.GroupDone {
		return label, ""
	}
	done := tracker.StatusDone
	after := before.Task
	after.Status = done
	got, err := t.deps.Closer(cv.g.c.actor).UpdateTask(ctx, statelog.StepOpID(op, stepTwo), before.Task.ID,
		before.Task.Project, before.Task.Version, tracker.TaskPatch{Status: &done}, tracker.ChangeStatus,
		tracker.Wake{Kind: tracker.ChangeStatus, Before: before.Task, After: after}.Notify(nil))
	switch {
	case err != nil:
		return notClosed(fmt.Sprintf("its close was refused (%v)", err))
	case got.Outcome == statelog.OutcomeUnknown:
		return notClosed(fmt.Sprintf("nobody can say whether its close landed; read it with %s", tracker.GetWorkItemTool))
	}
	return label, ""
}

// mergeRefused answers a merge, an enqueue or a dequeue its committer or its
// git node refused, in the refusal's class ([mergeRefusalClass]): a gate the
// change does not meet yet as not_ready, naming every unmet precondition and
// whom it waits on.
func mergeRefused(tool, change, target string, refusal code.MergeRefusal, detail string,
	unmet []code.Precondition) tools.Result {

	cause := writeRefusalError(string(refusal), detail)
	if refusal != code.MergeRefusalGateUnmet {
		return refusedBy(mergeRefusalClass(refusal), cause, fmt.Sprintf("%s: %s (%s).", tool, detail, refusal))
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
	return refusedBy(tools.RefusalNotReady, cause, b.String())
}

// mergeRefusalClass is the class a merge refusal is answered in, by what its
// caller does next: read again (a head, a target or a change that moved),
// wait (a committer busy, a target still moving), ask somebody else (a seat
// asking a person's override, an asker the repository does not let), or
// change the call.
func mergeRefusalClass(r code.MergeRefusal) tools.Refusal {
	switch r {
	case code.MergeRefusalGateUnmet:
		return tools.RefusalNotReady
	case code.MergeRefusalStale, code.MergeRefusalHeadMoved, code.MergeRefusalTargetMoved:
		return tools.RefusalStaleVersion
	case code.MergeRefusalTargetMoving, code.MergeRefusalBusy, code.MergeRefusalCompactionBehind,
		code.MergeRefusalChangeNotOpen, code.MergeRefusalChangeDraft, code.MergeRefusalQueueFull,
		code.MergeRefusalNotQueued:
		return tools.RefusalConflict
	case code.MergeRefusalAlreadyQueued:
		return tools.RefusalExists
	case code.MergeRefusalNotPerson, code.MergeRefusalNotPermitted:
		return tools.RefusalForbidden
	case code.MergeRefusalRepoDeleted, code.MergeRefusalChangeNotFound:
		return tools.RefusalNotFound
	case code.MergeRefusalUnavailable, code.MergeRefusalNotApplied:
		return tools.RefusalUnavailable
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
	cv, refusal := t.deps.conversation(ctx, turn, tool, c, args, name, number)
	if refusal != nil {
		return *refusal, nil
	}
	g := cv.g
	entry, refusal := t.entryOf(ctx, cv)
	if refusal != nil {
		return *refusal, nil
	}
	op := opIDFor(g.c.actor, tool, queueVerb, g.repo.ID, args)
	asker := writer(ctx, turn, g.c)
	// AN ENTRY IS ITS ENQUEUER'S, NEVER A TURN'S: the committer judges the
	// asker as a party, so the dequeue names no run, as the enqueue does.
	asker.Run = code.RunProvenance{}
	p := code.QueueProposal{OpID: op, Asker: asker, Seen: cv.at,
		Dequeue: &code.Dequeue{RepoID: g.repo.ID, EntryID: entry}}
	if err := p.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, g.c.actor, g.repo.Name, err), nil
	}
	res, err := t.deps.Writer.Queue(ctx, p)
	if err != nil {
		return t.deps.failedWrite(ctx, turn, tool, g.c, g.repo, op, err), nil
	}
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
		Change: g.ref(), Target: target, State: string(code.QueueDequeued), Outcome: res.Outcome, EntryID: entry,
		Position: positionText(res.Position), OpID: g.c.actor.Operation}
	return newForeignRead(turn, g.c.asker.Seat).page(ctx, tool, out, "")
}

// entryOf is the change's LIVE entry in its target's queue, read from the
// target's queue at the caller's floor a page at a time until the change's
// entry is met — at most ⌈code.MaxQueueEntries/code.MaxQueueRowsPerPage⌉
// pages, a dequeue being a rare gesture and a queue rarely deeper than its
// first page — or the refusal of a change no queue holds.
func (t *codeDequeueChange) entryOf(ctx context.Context, cv conversing) (string, *tools.Result) {
	g := cv.g
	q := code.ReadQuery{Kind: code.ReadQueue, Repo: g.repo.Name, Target: cv.change.TargetRef,
		Limit: code.MaxQueueRowsPerPage, Fresh: statelog.FloorAt(statelog.ReadSession, cv.at)}
	for {
		answer, err := g.deps.read(ctx, g.c, q)
		if err != nil {
			r := g.deps.failed(ctx, g.turn, g.tool, g.c.asker, g.repo, err)
			return "", &r
		}
		for _, row := range answer.Queue {
			if row.ChangeID == cv.change.ID {
				return row.EntryID, nil
			}
		}
		if answer.Next == "" {
			r := refusedBy(tools.RefusalConflict, fmt.Errorf("code: %s holds no live queue entry", g.ref()),
				fmt.Sprintf("%s: %s is in no queue of %s, so nothing was taken out.", g.tool, g.ref(),
					strings.TrimPrefix(cv.change.TargetRef, branchRoot)))
			return "", &r
		}
		q.Cursor = answer.Next
	}
}
