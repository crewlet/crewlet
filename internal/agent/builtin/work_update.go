package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// updateDependencyFailure explains an update whose PATCH landed and whose
// dependency change did not all land ([dependencyFailure]'s reason). It
// prescribes a call carrying only the dependencies; the whole call reworded is a
// new operation setting every field again, refused as stale with `if_match`.
func updateDependencyFailure(item worklet.TaskDetail, why string) string {
	return fmt.Sprintf("The change to %s WAS made, but its dependency change "+
		"did not finish: %s. Do not report the dependencies as set. Set them "+
		"with "+UpdateWorkItemTool+" on %s giving only the same waiting_on and "+
		"blocking: it records whichever of them are still missing, and says so "+
		"if one cannot be.", item.Named(), why, item.Address())
}

// ---- update_work_item -------------------------------------------------- //

type updateWorkItem struct {
	itemTarget
	deps WorkDeps
}

var _ tools.SeatCallable = (*updateWorkItem)(nil)

func (t *updateWorkItem) Name() string { return UpdateWorkItemTool }

func (t *updateWorkItem) Description() string {
	return "Change a work item: its status, assignee, priority, title, " +
		"description, labels, checklists, or whether you watch it. Only the " +
		"fields you pass are changed. An assignment can carry a `reason` the " +
		"new assignee reads. Moving an item you keep into `in_review` names its " +
		"`reviewer`. Say WHY you closed something with " +
		worklet.CommentOnWorkTool + " — an item that went to `cancelled` with no " +
		"word is one somebody has to reconstruct. Closing as a duplicate " +
		"also names the item that survives, in `duplicate_of`."
}

func (t *updateWorkItem) Parameters() map[string]any {
	return t.deps.ops.param(scheduleInto(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"item": map[string]any{
				"type":        "string",
				"description": "The item to change. " + itemRef,
			},
			"status":   map[string]any{"type": "string", "description": "One of " + words.Or(worklet.Statuses) + "."},
			"assignee": map[string]any{"type": "string", "description": "A seat's handle, or \"\" to unassign."},
			"reviewer": reviewerParam,
			"reason": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Only with `assignee` or `reviewer`: one line "+
					"saying why the work is theirs now, at most %d "+
					"characters. It is what the new assignee is woken with "+
					"and what the item's history shows beside the hand-off.",
					MaxAssignmentReason),
			},
			"checklist": checklistSchema(),
			"priority":  map[string]any{"type": "string", "description": "One of " + words.Or(worklet.Priorities) + "."},
			"title":     map[string]any{"type": "string"},
			"body":      map[string]any{"type": "string", "description": "Replaces the description."},
			"labels": map[string]any{
				"type": "array",
				"description": "REPLACES the item's labels. Each must be a " +
					"tag this project declares — " + worklet.DescribeProjectTool + " lists them.",
				"items": map[string]any{"type": "string"},
			},
			"labels_create_missing": map[string]any{
				"type": "boolean",
				"description": "Declare any label in `labels` this project " +
					"does not have, then write. Say true only when you MEANT " +
					"to add a grouping.",
			},
			"duplicate_of": map[string]any{
				"type": "string",
				"description": "Closing this as a duplicate: the item that " +
					"survives. Set the status as well — the link records WHY, " +
					"and the status records that it is closed. " + itemRef +
					" null or \"\" takes this item's duplicate link off.",
			},
			"watch": map[string]any{
				"type": "boolean",
				"description": "True to follow this item, false to stop. " +
					"Stopping sticks: you will not be re-subscribed by being " +
					"assigned it, though a direct @-mention still reaches you.",
			},
			"if_match": map[string]any{
				"type": "integer",
				"description": "The `task.version` from " + GetWorkItemTool + ". Given, the " +
					"edit is REFUSED if anybody changed the item since you " +
					"read it. Omitted, your fields are merged onto the " +
					"current item — which is usually what you want.",
			},
			"routing_unit": map[string]any{
				"type": "string",
				"description": "Point this item at a different team, by its " +
					"id or its name: that team's lead hears that work routes " +
					"to them now. The lead's of the project the item is in " +
					"to set — filing your own work into your own team is " +
					"what `unit` on " + worklet.CreateWorkItemTool + " does.",
			},
			"waiting_on": setArgSchema("The items this one is blocked BY. " + itemRefs +
				" A pull request under the company's GitHub owners, by its URL, blocks it " +
				"until it merges or closes, when you are told to update the item. When it " +
				"covers a question you asked a person on this item, withdraw that question " +
				"in the same step with " + WithdrawAskTool + " and `replaced_by`."),
			"blocking":     setArgSchema("The items blocked BY this one. " + itemRefs),
			"linked":       setArgSchema("Related items, with no blocking meaning. " + itemRefs),
			"linked_pages": setArgSchema("Knowledge-base pages this item references, by page id."),
			"linked_entries": setArgSchema("Entries of the company's atlas this item touches (the " +
				"servers, services, databases, domains or accounts the work changes), each by its id " +
				"as " + AtlasSearchTool + " answers it, or by its exact name or key."),
			"fields": map[string]any{
				"type": "object",
				"description": "Custom fields, keyed by SLUG — read them with " +
					worklet.GetWorkCatalogueTool + " or " + worklet.DescribeProjectTool + ". A value is " +
					"checked against the field's own declaration and refused " +
					"naming the rule, never rounded or coerced to fit; null " +
					"clears a field.",
			},
			"dependency_note": map[string]any{
				"type": "string",
				"description": "One line saying WHY, recorded on every " +
					"dependency this call adds. Skipped on removals.",
			},
		},
		"required": []any{"item"},
	}, true))
}

// setArgSchema is the shape every set-valued argument takes: a model's
// likeliest reading, a bare list, silently drops every entry it did not repeat.
func setArgSchema(what string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": what + " Pass {\"add\": [...], \"remove\": [...]} to change part of the set, or {\"set\": [...]} to replace it entirely. Never a bare list.",
		"properties": map[string]any{
			"add":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"remove": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"set": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "REPLACES the whole set. Everything not listed is removed.",
			},
		},
	}
}

func (t *updateWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *updateWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return unattributed(UpdateWorkItemTool, err), nil
	}
	if t.deps.Writer == nil || t.deps.Reader == nil {
		return unconfigured(UpdateWorkItemTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, UpdateWorkItemTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	writer := t.deps.Writer(actor)

	ref := strings.TrimSpace(argString(args, "item"))
	if ref == "" {
		return failed(UpdateWorkItemTool + " needs an `item` — a key like ENG-42, or an id."), nil
	}
	before, err := t.deps.Reader.Task(ctx, ref, worklet.DetailWants{}, seatRead)
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("There is no work item %q.",
			clip(ref))), nil
	case err != nil:
		return readFailure(ctx, UpdateWorkItemTool, err), nil
	}

	patch, kind, refusal := patchFromArgs(args, actor, t.deps.now(), t.deps.zone())
	if refusal != "" {
		return failed(refusal), nil
	}
	if refused := t.deps.checkWaitOnPerson(ctx, actor, before, patch); refused != nil {
		return *refused, nil
	}
	if patch.Body != nil {
		// See [unfence].
		body := unfence(turn, *patch.Body)
		patch.Body = &body
	}
	// One operation per distinct call ([Actor.callKey]). Derived before the patch
	// is built: a checklist gesture mints its list and item ids from it.
	opID := opIDFor(actor, t.Name(), "update", before.Task.ID, args)
	reason, refusal := assignmentReason(args)
	if refusal != "" {
		return failed(refusal), nil
	}
	checklist, refusal := t.deps.checklistIntent(ctx, args, opID)
	if refusal != "" {
		return failed(refusal), nil
	}
	if checklist != nil {
		patch.Checklist = checklist
		if kind == worklet.ChangeFields {
			kind = worklet.ChangeChecklist
		}
	}
	// The model's own precondition: omitted it merges.
	ifMatch := uint64(max(argInt(args, "if_match", 0), 0))
	if patch.Assignee != nil {
		// The create's check; patchFromArgs holds no roster.
		//nolint:govet // shadow: `x, refusal := f()` declares x too; see .golangci.yml
		assignee, refusal := t.deps.resolveHandle(ctx, UpdateWorkItemTool,
			"`assignee`", *patch.Assignee)
		if refusal != "" {
			return failed(refusal), nil
		}
		patch.Assignee = &assignee
	}
	if refused := t.deps.holdReview(ctx, actor, before, args, &patch, &kind); refused != nil {
		return *refused, nil
	}
	// The re-route is its own kind and gate: `routed` carries one delta, and the
	// new unit's lead hears it as an ORDINARY candidate, not a fallback, so it
	// reaches them on a task that still has an assignee.
	if raw, held := args["routing_unit"]; held {
		unit := strings.TrimSpace(argString(map[string]any{"v": raw}, "v"))
		// Not at the gate: the task's project comes from the row just read.
		if refusal := t.deps.mayWrite(ctx, authz.ActionWorkRoute,
			authz.Object{
				Kind: authz.KindProject, Container: before.Task.Project,
			}); refusal != nil {

			return *refusal, nil
		}
		// The chart's key lands, as for the filed unit: a rename strands other
		// spellings.
		if unit != "" && t.deps.Units != nil {
			resolved, found := t.deps.Units.ResolveUnit(unit)
			if !found {
				return failed(fmt.Sprintf("This company has no team %q. A task "+
					"routed at a team nobody has reaches nobody at all.",
					clip(unit))), nil
			}
			unit = resolved.Key
		}
		patch.RoutingUnit = &unit
		if kind == worklet.ChangeFields {
			kind = worklet.ChangeRouted
		}
	}
	// Inert edges ride the patch; dependencies are a sequence (an authored edge
	// here, a mirrored entry on another item).
	inert, relationRefusal := t.deps.inertRelations(ctx, turn, UpdateWorkItemTool, args,
		before.Task)
	if relationRefusal != nil {
		return *relationRefusal, nil
	}
	if inert != nil {
		patch.Relate = inert
		if kind == worklet.ChangeFields {
			kind = worklet.ChangeRelations
		}
	}
	change, dependencyRefusal := t.deps.dependencyChange(ctx, UpdateWorkItemTool, args, before.Task)
	if dependencyRefusal != nil {
		return *dependencyRefusal, nil
	}
	// Before the patch: a call that cannot write its dependency change writes
	// nothing.
	if !change.Empty() && t.deps.Dependencies == nil {
		return unconfigured(UpdateWorkItemTool), nil
	}
	// Labels last among the checks: declaring them is the call's first PUBLISH,
	// and a later refusal would leave them declared. Against the task's HOME
	// project.
	var declared []string
	var caveats []caveat.Caveat
	if patch.Tags != nil {
		var labelRefusal *tools.Result
		if declared, caveats, labelRefusal = t.deps.declareLabels(ctx, actor, labelWrite{
			tool:   t.Name(),
			before: "changing " + before.Named(),
			notMade: fmt.Sprintf("Nothing it asked of %s was written by this "+
				"call.", before.Named()),
			then: "makes the change",
			twice: "applies the change a second time, and is refused as " +
				"stale if it names `if_match`",
			unvouched: "it answers with the change where this node still " +
				"holds its record",
			look: fmt.Sprintf("the change may have landed where this node "+
				"cannot see it: read %s with "+GetWorkItemTool+" to see whether it "+
				"is there rather than making it any other way", before.Address()),
		}, args, before.Task.Project, *patch.Tags); labelRefusal != nil {
			return *labelRefusal, nil
		}
	}

	answer := withOperation(ReceiptOf(map[string]any{
		"labels_created": declared,
	}, before), actor)
	var (
		patchOutcome statelog.Outcome
		patchAt      statelog.Position
		patchVersion int64
	)
	// Skipped for a dependency-only call: an empty patch still stamps a version
	// and writes a `fields` history row that changed nothing.
	if !patch.Empty() {
		got, err := writer.UpdateTask(ctx, opID, before.Task.ID,
			before.Task.Project, ifMatch, patch, kind,
			worklet.Wake{
				Kind:   kind,
				Before: before.Task,
				After:  patched(before.Task, patch),
				// The history row and the wake already carry the excerpt.
				Excerpt: reason,
			}.Notify(t.deps.Leads))
		if err != nil {
			return writeFailure(ctx, actor, UpdateWorkItemTool, err), nil
		}
		if got.Outcome == statelog.OutcomeUnknown {
			// Its `version` least of all is reported (as `if_match` it may name a
			// version the item never had). The dependencies wait: the same call
			// again answers this write first and writes them after, once.
			return updateUnknown(actor, opID, got, before,
				!change.Empty()), nil
		}
		t.deps.settle(ctx, got.Position)
		answer["outcome"], answer["version"] = string(got.Outcome), got.Version
		answer["position"] = positionOf(got.Position)
		patchOutcome, patchAt, patchVersion = got.Outcome, got.Position, got.Version
		// A change the engine made to a typed value must be told.
		caveats = append(caveats, got.Caveats...)
	}
	if !change.Empty() {
		result, err := t.deps.Dependencies(actor).Depend(ctx,
			opIDFor(actor, t.Name(), "depend", before.Task.ID, args), change, t.deps.Leads)
		_, patchRan := answer["outcome"]
		switch {
		case err != nil && patchRan:
			// The patch landed: "the change was NOT made" would be false.
			return caveated(answer, caveats, partial(answer, DependenciesFailedKey,
				caveat.DependenciesNotRecorded(before.Address(), updateDependencyFailure(
					before, writeTold(ctx, actor, UpdateWorkItemTool, err)))))
		case err != nil:
			return writeFailure(ctx, actor, UpdateWorkItemTool, err), nil
		}
		t.deps.settle(ctx, result.Position)
		// Both records when the patch appended one ([joinDepend]), else the
		// sequence alone, where version zero is no `if_match` condition.
		if patchRan {
			outcome, at, version := joinDepend(patchOutcome, patchAt,
				patchVersion, result)
			answer["outcome"], answer["version"] = string(outcome), version
			answer["position"] = positionOf(at)
		} else {
			answer["outcome"], answer["version"] = string(result.Outcome), result.Version
			answer["position"] = positionOf(result.Position)
		}
		caveats = append(caveats, result.Caveats...)
		// A dependency whose mirror lost its race is durable on the authoring
		// side and repaired by the Worklet duty.
		if len(result.OneSided) > 0 {
			answer["dependencies_pending_mirror"] = len(result.OneSided)
		}
	}
	return caveated(answer, caveats)
}

// updateUnknown explains an update whose outcome is unknown ([unknownWrite]),
// pointing at the version it read: a read past it can say a change landed.
func updateUnknown(actor Actor, opID string, got worklet.WriteResult,
	before worklet.TaskDetail, dependencies bool) tools.Result {

	look := fmt.Sprintf("Read %s with "+GetWorkItemTool+" (a version past %d means "+
		"something landed, and its history says whether it was this)",
		before.Address(), before.Task.Version)
	next := unknownNext(got.Unvouched, sameCall(actor, UpdateWorkItemTool), look,
		"it is applied a second time, and refused as stale if it names `if_match`")
	if dependencies {
		next += " The dependency changes this call asked for were NOT written: " +
			"it stopped here, before them."
	}
	return unknownWrite(actor, UpdateWorkItemTool,
		fmt.Sprintf("the change to %s landed", before.Named()), opID, got.Unvouched, next)
}

// declareLabels declares the labels a write is about to use that its project
// does not have, only if the caller SAID SO: a meant `regression` and a typo
// `regresion` look identical at the call, and create-on-use would fill the
// filter strip with misspellings. It returns what it created, so a habitual flag
// still shows a typo. Its write derives through [opIDFor], so a repeated call
// declares under the same operation; its refusal is [labelFailure]'s.
func (d WorkDeps) declareLabels(ctx context.Context, actor Actor,
	write labelWrite, args map[string]any, project string,
	labels []string) ([]string, []caveat.Caveat, *tools.Result) {

	if len(labels) == 0 || !argBool(args, "labels_create_missing") {
		return nil, nil, nil
	}
	if d.ProjectWriter == nil {
		return nil, nil, refusalOf(refusedBy(tools.RefusalUnavailable, waitless(), "This build "+
			"cannot declare labels at a write. Ask the project lead to "+
			"declare it, or file without the label."))
	}
	opID := opIDFor(actor, write.tool, "tags", project, args)
	created, caveats, err := d.ProjectWriter(actor).EnsureTags(ctx, opID,
		project, labels)
	if err != nil {
		return nil, nil, refusalOf(labelFailure(ctx, actor, write, opID, project, labels, err))
	}
	// Caveats stay out of the created slugs, where a sentence would read as a tag.
	return created, caveats, nil
}

// labelWrite is the write a call was about to make when it declared its labels
// first ([WorkDeps.declareLabels]), in the words an answer about that
// declaration needs.
type labelWrite struct {
	// tool is the CALLING tool, which every answer names: the declaration is a
	// step of its call, never a call to write_project.
	tool string
	// before is what the call stopped before doing, a gerund phrase: "filing
	// the work item", "changing ENG-4".
	before string
	// notMade says that this call did not make that write.
	notMade string
	// then is what the same call made again does once the declaration is
	// answered: "files it", "makes the change".
	then string
	// twice is what a DIFFERENT call does where an earlier attempt already made
	// the write: "files a second item".
	twice string
	// unvouched is what the same call made again does with its OWN write where
	// this node cannot vouch for the declaration: every step of one call carries
	// the call's mint instant, so it cannot vouch for that write either. A
	// clause: "it answers with the item where …".
	unvouched string
	// look is the read that says whether that write is there, a clause: "look
	// for it with list_work_items".
	look string
}

// labelFailure explains a create or an update refused, or stopped, at the inline
// declaration of its labels, under the CALLING tool and in terms of the write it
// did NOT make (never [writeFailure] under write_project).
//
// The same call finishes it ([opIDFor], [sameCall]). An unvouched declaration is
// the exception: the repeat stops at that step until it reaches this node, so the
// answer prescribes declaring the tags with write_project (harmless: it states
// the same tags), and warns that the repeat's own write shares the call's instant
// ([Actor.OperationSince]) and may answer `unknown` again.
func labelFailure(ctx context.Context, actor Actor, write labelWrite, opID, project string,
	labels []string, err error) tools.Result {

	var clash *worklet.TagClash
	var full *worklet.TagsFull
	switch {
	case errors.As(err, &clash):
		// [worklet.ErrTaken]: another tag carries the label.
		return refusedBy(tools.RefusalExists, err, fmt.Sprintf("%s was refused, "+
			"and nothing was written: to use "+
			"the label %s it has to declare it in %s, where the tag %s is "+
			"already labelled %q — two tags nobody could tell apart. If they "+
			"mean the same thing, give `labels` %s instead of %s; if not, "+
			"declare %s in %s yourself under a label of its own ("+worklet.WriteProjectTool+" "+
			"on %s with tags_add: [{\"slug\": %q, \"label\": \"<a label %s "+
			"does not use>\"}]). Then make this call again.", write.tool,
			clash.Slug, clash.Project, clash.Other.Slug, clash.Other.Label,
			clash.Other.Slug, clash.Slug, clash.Slug, clash.Project,
			clash.Project, clash.Slug, clash.Project))
	case errors.As(err, &full):
		// [worklet.ErrState]: the label is fine; it lands once a tag is archived.
		return refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s was refused, "+
			"and nothing was written: to use "+
			"the label %s it has to declare it in %s, which already keeps %d "+
			"tags, the most a project may. Use a tag %s already has "+
			"("+worklet.DescribeProjectTool+" lists them) or leave %s out of `labels`; or have "+
			"%s's lead archive tags it no longer files under ("+worklet.WriteProjectTool+"'s "+
			"tags_archive), then make this call again.", write.tool, full.Slug,
			full.Project, worklet.MaxTagsPerProject, full.Project, full.Slug,
			full.Project))
	case errors.Is(err, worklet.ErrStepUnresolved), errors.Is(err, estate.ErrOutcomeUnknown):
		// A declaration no data node answered is this case too: the same
		// operation again settles it.
		unvouched := errors.Is(err, worklet.ErrStepUnvouched)
		why := "the write's acknowledgement was lost"
		switch {
		case unvouched:
			why = "this node's operation ledger may have lost the record of " +
				"this operation, so this node cannot tell"
		case errors.Is(err, estate.ErrOutcomeUnknown):
			// Fixed words, as [Unanswered]'s: the router's error names every node.
			why = "a data node it was sent to did not answer"
		}
		if actor.Operation != "" {
			opID = actor.Operation
		}
		stopped := fmt.Sprintf("%s stopped before %s: it first declares "+
			"whichever of its labels (%s) %s does not have yet, and whether "+
			"that declaration landed is unknown (%s; operation %s). %s",
			write.tool, write.before, strings.Join(labels, ", "), project, why,
			opID, write.notMade)
		// The cause is the unknown step ([writeCause]), not a refusal of what
		// was asked.
		cause := writeCause(actor, err)
		again := sameCall(actor, write.tool)
		switch {
		case again == "":
			return refusedBy(tools.RefusalUnavailable, cause, stopped+" Making "+
				"the call again is a new operation: it declares the labels again — harmless, since that "+
				"states the same tags — then "+write.then+".")
		case unvouched:
			adds := make([]string, len(labels))
			for i, label := range labels {
				adds[i] = fmt.Sprintf("{\"slug\": %q}", label)
			}
			// An `op_id` travels, so another node can be asked.
			elsewhere := ""
			if actor.Operation != "" {
				elsewhere = ", or make the same call with that `op_id` through " +
					"another node's operator MCP, whose ledger may reach back " +
					"that far"
			}
			return refusedBy(tools.RefusalUnavailable, cause, fmt.Sprintf("%s The "+
				"same call made here stops at this step "+
				"until the declaration reaches this node, and never if it did "+
				"not land. Declaring a tag again is harmless, so declare them "+
				"first: "+worklet.WriteProjectTool+" on %s with tags_add: [%s] states the same "+
				"tags as a new operation, and changes nothing if the first "+
				"declaration landed. Then %s. That repeat skips the declaration, "+
				"but its own write dates from the same instant, so this node "+
				"cannot vouch for it either: %s, and otherwise answers `unknown` "+
				"again — which then means %s%s. Do not reword it: if an earlier "+
				"attempt at this call got past this step, a different call %s.",
				stopped, project, strings.Join(adds, ", "), again,
				write.unvouched, write.look, elsewhere, write.twice))
		}
		return refusedBy(tools.RefusalUnavailable, cause, fmt.Sprintf("%s %s: that "+
			"is the same operation — it answers the "+
			"declaration with what landed, then %s, once. Do not reword it: if "+
			"an earlier attempt at this call got past this step, a different call %s. "+
			"Do not report it as done until a call answers without this error.",
			stopped, capitalize(again), write.then, write.twice))
	}
	if reused := reusedOperation(actor, write.tool, err); reused != "" {
		return refusedBy(tools.RefusalInvalid, err, reused)
	}
	// The error's class as every write reads it ([writeFailure]); a fault is
	// told in fixed words ([toldOf]).
	failure := writeFailure(ctx, actor, write.tool, err)
	return tools.Result{Failed: true, Cause: failure.Cause,
		Output: fmt.Sprintf("%s was refused before %s: declaring its labels in "+
			"%s did not land (%s). Nothing was written — do not report it as "+
			"done.", write.tool, write.before, project, toldOf(failure, err))}
}

// patchFromArgs builds the patch and the change kind, or the refusal to show the
// model. The kind is decided here, not derived from the patch: a change with
// several fields is about ONE thing (a status move that set an assignee is a
// status change); the order below is that judgement, most specific first.
func patchFromArgs(args map[string]any, actor Actor, now time.Time,
	loc *time.Location) (worklet.TaskPatch, worklet.ChangeKind, string) {

	var patch worklet.TaskPatch
	kind := worklet.ChangeFields

	// Scheduling first, so a typed-date refusal comes before anything else.
	plan, refusal := readSchedule(args, now, loc, UpdateWorkItemTool)
	if refusal != "" {
		return patch, kind, refusal
	}
	if refusal = readWait(args, now, loc, &plan, UpdateWorkItemTool); refusal != "" {
		return patch, kind, refusal
	}
	plan.applyToPatch(&patch)

	if v, held := args["title"]; held {
		title := strings.TrimSpace(argString(map[string]any{"v": v}, "v"))
		patch.Title = &title
	}
	if _, held := args["body"]; held {
		body := argString(args, "body")
		patch.Body = &body
	}
	if _, held := args["assignee"]; held {
		assignee := strings.TrimSpace(argString(args, "assignee"))
		patch.Assignee = &assignee
		kind = worklet.ChangeAssignee
	}
	if raw := strings.TrimSpace(argString(args, "priority")); raw != "" {
		priority := worklet.Priority(raw)
		if !priority.Valid() {
			return patch, kind, fmt.Sprintf("%q is not a priority. The "+
				"priorities are: %s.", clip(raw), words.And(worklet.Priorities))
		}
		patch.Priority = &priority
		// The kind stays `fields`: `prioritised` is somebody writing YOUR OWN
		// priority list, not in [worklet.ChangeKind.TaskCommit], so on a task it
		// would wake nobody.
	}
	if raw := strings.TrimSpace(argString(args, "status")); raw != "" {
		status := worklet.Status(raw)
		if !status.Valid() {
			return patch, kind, fmt.Sprintf("%q is not a status. The statuses "+
				"are: %s.", clip(raw), words.And(worklet.Statuses))
		}
		patch.Status = &status
		kind = worklet.ChangeStatus
	}
	if _, held := args["labels"]; held {
		labels := argStrings(args, "labels")
		patch.Tags = &labels
		if kind == worklet.ChangeFields {
			kind = worklet.ChangeTags
		}
	}
	if watch, held := argval.Bool(args["watch"]); held {
		// A gesture about one person, never the set: [worklet.TaskPatch]'s
		// collections are carried whole, so `watchers: [me]` would remove
		// everybody else. The writer resolves it inside its own snapshot. A person
		// bound to a seat writes AS that seat; the credential ([Actor.OperatorID])
		// is never an address.
		patch.Watch = &worklet.WatchIntent{Handle: actor.Handle, Watch: watch}
		if kind == worklet.ChangeFields {
			kind = worklet.ChangeWatchers
		}
	}
	if raw, held := args["fields"]; held {
		fields, refusal := fieldMap(raw)
		if refusal != "" {
			return patch, kind, refusal
		}
		patch.Fields = &fields
	}
	return patch, kind, ""
}

// remove is a handle set minus one handle, order preserved.
func remove(all []string, handle string) []string {
	out := make([]string, 0, len(all))
	for _, one := range all {
		if one != handle {
			out = append(out, one)
		}
	}
	return out
}

// appendMissing adds the handle when `add`, and otherwise leaves the set.
func appendMissing(all []string, handle string, add bool) []string {
	if !add {
		return all
	}
	return append(all, handle)
}

// patched is the task as the write will leave it, for the wake's snapshot:
// applied here, since a read after the write would race other writers.
func patched(task worklet.Task, patch worklet.TaskPatch) worklet.Task {
	// The writer's own merge, never a copy, which would miss new fields and have
	// [worklet.TaskDeltas] report changes that changed nothing.
	task = worklet.Patched(task, patch)

	if patch.Watch != nil {
		// Applied to the snapshot this tool read: the durable sets are the
		// writer's, and this is only the wake's recipients; without it a new
		// watcher would miss the wake announcing it.
		task.Watchers = appendMissing(remove(task.Watchers, patch.Watch.Handle),
			patch.Watch.Handle, patch.Watch.Watch)
		task.Muted = appendMissing(remove(task.Muted, patch.Watch.Handle),
			patch.Watch.Handle, !patch.Watch.Watch)
	}
	if patch.Checklist != nil {
		// The writer's own function, so the wake's delta and recipients follow
		// the gesture. One this read cannot resolve leaves the snapshot as read;
		// the decide resolves it.
		if lists, err := worklet.ApplyChecklist(task.Checklists, *patch.Checklist); err == nil {
			task.Checklists = lists
		}
	}
	return task
}
