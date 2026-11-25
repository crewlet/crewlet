package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ---- create_work_item -------------------------------------------------- //

type createWorkItem struct {
	deps WorkDeps

	// pages checks a decision's page evidence — see [evidenceLookup]. Nil
	// on a company with no Doclet.
	pages PageReader
}

var _ tools.SeatCallable = (*createWorkItem)(nil)

func (t *createWorkItem) Name() string { return CreateWorkItemTool }

func (t *createWorkItem) Description() string {
	return "File a new work item. Search with " + ListWorkItemsTool + " first — a " +
		"duplicate costs somebody a triage turn. Name an assignee and it " +
		"goes straight to their queue; leave it empty and it goes to the " +
		"project's default assignee, or lands in triage — where the " +
		"team's lead is told about it — when the project names none. The " +
		"answer's `assignee` says which. Work you file for yourself wakes " +
		"nobody, your lead included. Name " +
		"`ask` to file the item AS a question to that person — the title " +
		"is the question — with `decision` when they have to choose; do so " +
		"whenever the work waits on somebody's decision, your lead's too."
}

func (t *createWorkItem) Parameters() map[string]any {
	return t.deps.ops.param(scheduleInto(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title": map[string]any{
				"type":        "string",
				"description": "One line saying what the work is.",
			},
			"body": map[string]any{
				"type": "string",
				"description": "The full description in markdown: what is " +
					"wanted, why, and how anyone would know it is done.",
			},
			"type": map[string]any{
				"type": "string",
				"description": "A task type from your workspace's own " +
					"catalogue. `task` if you are unsure.",
			},
			"project": map[string]any{
				"type": "string",
				"description": "The project key. Defaults to the parent's " +
					"for a subtask, and to your team's otherwise.",
			},
			"assignee": map[string]any{
				"type": "string",
				"description": "A seat's handle, from " + LookupColleagueTool + ". Omit " +
					"and the project's default assignee takes it (" + worklet.DescribeProjectTool + " " +
					"names them), else it waits in triage for the lead to route.",
			},
			"priority": map[string]any{
				"type":        "string",
				"description": "One of " + words.Or(worklet.Priorities) + ". Default none.",
			},
			"status": map[string]any{
				"type": "string",
				"description": "Where it starts: one of " + words.Or(worklet.Statuses) +
					". Default todo — name another only when the work is " +
					"already under way or already done as it is filed.",
			},
			"parent": map[string]any{
				"type": "string",
				"description": "The item this belongs under — a subtask " +
					"lives in its parent's project. " + itemRef,
			},
			"fields": map[string]any{
				"type": "object",
				"description": "Custom fields, keyed by SLUG — read them with " +
					worklet.GetWorkCatalogueTool + " or " + worklet.DescribeProjectTool + ". A project may " +
					"REQUIRE some for this type, and the create is refused " +
					"naming any that are missing.",
			},
			"unit": map[string]any{
				"type": "string",
				"description": "The team this work belongs to, by its id or " +
					"its name. Defaults to the team that owns `project`, " +
					"which is almost always right — name another only when " +
					"the work belongs to a different team than the project " +
					"it sits in.",
			},
			"waiting_on": map[string]any{
				"type": "array",
				"description": "Items this one is blocked BY. " + itemRefs +
					" A plain list here, unlike " + UpdateWorkItemTool + ": a new item " +
					"has no dependencies to replace.",
				"items": map[string]any{"type": "string"},
			},
			"labels": map[string]any{
				"type": "array",
				"description": "Tags this project declares — read them with " +
					worklet.DescribeProjectTool + ". A label the project has not declared " +
					"is refused, because a typo would otherwise become a " +
					"grouping nobody can filter on twice.",
				"items": map[string]any{"type": "string"},
			},
			"labels_create_missing": map[string]any{
				"type": "boolean",
				"description": "Declare any label in `labels` this project " +
					"does not have, then file. Say true only when you MEANT " +
					"to add a grouping — the answer lists what it created.",
			},
			"ask": map[string]any{
				"type": "string",
				"description": "A colleague's handle: the item IS a question " +
					"they owe an answer to, and its title is the question. " +
					"They are woken asking for one and follow the item, and " +
					"their answer wakes you. Filed in one record with the " +
					"item, so the question cannot be lost between two writes. " +
					"An agent always asks a person with a `decision`.",
			},
			"decision": decisionSchema(),
		},
		"required": []any{"title"},
	}, false))
}

// askFor is the question a create is filed as, or nil when it asks nobody. The
// title is the question and the ask's body; an ask with no text renders as
// nothing in the thread and in `asked_of_me`.
func (t *createWorkItem) askFor(ctx context.Context, actor Actor, task worklet.Task,
	args map[string]any) (*worklet.Comment, *tools.Result) {

	asked := strings.TrimSpace(argString(args, "ask"))
	rawDecision, decided := args["decision"]
	switch {
	case asked == "" && decided:
		return nil, refusalOf(failed(worklet.CreateWorkItemTool + ": `decision` needs `ask` " +
			"— the handle of the person who has to choose. A decision asked " +
			"of nobody wakes nobody."))
	case asked == "":
		return nil, nil
	}
	resolved, refusal := t.deps.resolveHandle(ctx, CreateWorkItemTool, "`ask`", asked)
	if refusal != "" {
		return nil, refusalOf(failed(refusal))
	}
	if refused := t.deps.checkAskOfPerson(CreateWorkItemTool, actor, resolved, decided); refused != nil {
		return nil, refused
	}
	ask := &worklet.Comment{
		// Derived like every comment's: a re-run files one question.
		ID:         commentID(actor, CreateWorkItemTool, task.ID, args),
		Task:       task.ID,
		Author:     actor.Handle,
		AuthorKind: actor.Kind,
		Body:       task.Title,
		Ask:        resolved,
		CreatedAt:  task.CreatedAt,
	}
	if decided {
		decision, refusal := t.deps.readDecision(ctx, CreateWorkItemTool,
			actor, rawDecision, t.pages)
		if refusal != nil {
			return nil, refusal
		}
		ask.Decision = decision
	}
	return ask, nil
}

// scheduleInto merges the scheduling parameters into a tool's schema, so create
// and update take the same spellings.
func scheduleInto(schema map[string]any, update bool) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	for key, value := range scheduleSchema(update) {
		props[key] = value
	}
	return schema
}

func (t *createWorkItem) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *createWorkItem) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads.
		return unattributed(CreateWorkItemTool, err), nil
	}
	if t.deps.Writer == nil {
		return unconfigured(CreateWorkItemTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, CreateWorkItemTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	writer := t.deps.Writer(actor)

	now := t.deps.now()
	// See [unfence].
	body := unfence(turn, argString(args, "body"))
	task := worklet.Task{
		V:           worklet.DocumentVersion,
		Title:       strings.TrimSpace(argString(args, "title")),
		Body:        body,
		Type:        strings.TrimSpace(argString(args, "type")),
		Project:     strings.ToUpper(strings.TrimSpace(argString(args, "project"))),
		Assignee:    strings.TrimSpace(argString(args, "assignee")),
		Reporter:    actor.Handle,
		Priority:    worklet.Priority(strings.TrimSpace(argString(args, "priority"))),
		Status:      worklet.StatusTodo,
		StatusGroup: worklet.GroupNotStarted,
		Rank:        worklet.RankOrigin,
		Tags:        argStrings(args, "labels"),
		CreatedAt:   now,
		UpdatedAt:   now,
		BodyAuthor:  actor.Handle,
		BodyAt:      now,
	}
	if task.Title == "" {
		return toPerson(failed(worklet.CreateWorkItemTool+" needs a `title`: one line "+
			"saying what the work is."), caveat.TitleMissing()), nil
	}
	if raw, held := args["fields"]; held {
		fields, refusal := fieldMap(raw)
		if refusal != "" {
			return failed(refusal), nil
		}
		task.Fields = fields
	}
	// Due date and size (workschedule.go).
	plan, refusal := readSchedule(args, now, t.deps.zone(), CreateWorkItemTool)
	if refusal != "" {
		return failed(refusal), nil
	}
	plan.applyToTask(&task)
	if task.Type == "" {
		task.Type = worklet.DefaultType
	}
	if task.Priority == "" {
		task.Priority = worklet.PriorityNone
	}
	if !task.Priority.Valid() {
		return failed(fmt.Sprintf("%q is not a priority. The priorities are: %s.",
			clip(string(task.Priority)), words.And(worklet.Priorities))), nil
	}
	// Filing from a board lane is one write, not a create and a move that could
	// fail in between. The applier stamps finish instants from the status's
	// group, so a task filed `done` is done from its first row.
	if raw := strings.TrimSpace(argString(args, "status")); raw != "" {
		status := worklet.Status(raw)
		if !status.Valid() {
			return failed(fmt.Sprintf("%q is not a status. The statuses are: %s.",
				clip(raw), words.And(worklet.Statuses))), nil
		}
		task.Status, task.StatusGroup = status, status.Group()
	}
	if ref := strings.TrimSpace(argString(args, "parent")); ref != "" {
		parent, refused := t.deps.resolveTask(ctx, CreateWorkItemTool, "`parent`", ref)
		if refused != nil {
			return *refused, nil
		}
		task.Parent = &parent.ID
		// A subtask files into its parent's project when the call names none:
		// the caller's own team's would be refused by the tracker.
		if task.Project == "" {
			task.Project = parent.Project
		}
	}
	if task.Project == "" {
		task.Project = t.deps.defaultProject(actor)
		if task.Project == "" {
			return failed(org.NoHomeProject(worklet.CreateWorkItemTool)), nil
		}
	}
	// The operation, and the task's id from it ([createdTaskID]). Its object is
	// the project the CALL names, never a resolved default: the chart and parent
	// can move between attempts, and a second attempt would derive another
	// operation and file a second item. The call's arguments tell new items
	// apart, as a view's do ([viewObjectKey]).
	opID := opIDFor(actor, t.Name(), "create",
		createObjectKey(argString(args, "project")), args)
	task.ID = createdTaskID(actor, t.Name(), argString(args, "project"), args)
	// Open to every seat, unlike update_work_item's re-route (the project
	// lead's). `FiledUnit` is the immutable owning team, `RoutingUnit` the mutable
	// half. Naming none is ordinary: the tracker fills both from the project's
	// unit inside the create's snapshot. The chart's key is stored, not the typed
	// string: `filed_unit` is never rewritten, so verbatim spellings would split
	// one team's work across filters.
	if unit := strings.TrimSpace(argString(args, "unit")); unit != "" {
		if t.deps.Units != nil {
			resolved, found := t.deps.Units.ResolveUnit(unit)
			if !found {
				return failed(fmt.Sprintf("This company has no team %q.",
					clip(unit))), nil
			}
			unit = resolved.Key
		}
		task.FiledUnit, task.RoutingUnit = unit, unit
	}

	// Before the watchers, so the canonical handle is watched.
	assignee, refusal := t.deps.resolveHandle(ctx, CreateWorkItemTool,
		"`assignee`", task.Assignee)
	if refusal != "" {
		return failed(refusal), nil
	}
	task.Assignee = assignee
	// Set at the write: a list built at the wake would read a row that has moved
	// on. A person bound to a seat writes AS it ([PrincipalActor]).
	task.Watchers = handles(actor.Handle, task.Assignee)

	// Checked before anything is published, after the task's id (the ask's id
	// derives from it).
	ask, askRefusal := t.askFor(ctx, actor, task, args)
	if askRefusal != nil {
		return *askRefusal, nil
	}
	// Before any write: an unknown dependency refuses the whole call, leaving
	// no item or labels behind.
	var blockers []string
	for _, ref := range argStrings(args, "waiting_on") {
		id, refused := t.deps.resolveBlocker(ctx, CreateWorkItemTool, "waiting_on", ref)
		if refused != nil {
			return *refused, nil
		}
		blockers = append(blockers, id)
	}
	if len(blockers) > 0 && t.deps.Dependencies == nil {
		return unconfigured(CreateWorkItemTool), nil
	}
	// Before the task (the create refuses undeclared labels), after every
	// refusal above: the declare is the call's first PUBLISH, and a later
	// refusal would leave labels nobody used.
	declared, caveats, labelRefusal := t.deps.declareLabels(ctx, actor, labelWrite{
		tool:    t.Name(),
		before:  "filing the work item",
		notMade: "The work item was NOT filed by this call.",
		then:    "files it",
		twice:   "files a second item",
		unvouched: "it answers with the item where an earlier attempt " +
			"filed it",
		look: "the item may exist on a node this one has not caught up " +
			"with: look for it with " + ListWorkItemsTool + " rather than filing it " +
			"any other way",
	}, args, task.Project, task.Tags)
	if labelRefusal != nil {
		return *labelRefusal, nil
	}
	notify := worklet.Wake{
		Kind: worklet.ChangeCreated, After: task, Comment: ask,
	}.Notify(t.deps.Leads)
	var got worklet.WriteResult
	if ask != nil {
		got, err = writer.CreateTaskAsking(ctx, opID, task, *ask, notify)
	} else {
		got, err = writer.CreateTask(ctx, opID, task, notify)
	}
	if err != nil {
		return writeFailure(ctx, actor, CreateWorkItemTool, err), nil
	}
	if got.Outcome == statelog.OutcomeUnknown {
		// Its key may name a task that never landed.
		return createUnknown(actor, opID, task.ID, got), nil
	}
	t.deps.settle(ctx, got.Position)
	answer := withOperation(receiptItem(map[string]any{
		"id": task.ID, "status": task.Status,
		// The tracker's, not the argument: its snapshot decides the default.
		"assignee": got.Assignee, "outcome": string(got.Outcome), "position": positionOf(got.Position),
		"labels_created": declared, "version": got.Version,
	}, task.ID, got.Key, got.KeyCollision), actor)
	if ask != nil {
		// What an answer names in `answers`.
		answer["asked"], answer["comment_id"] = ask.Ask, ask.ID
		if ask.Decision != nil {
			answer["decision"] = ask.Decision
		}
	}
	caveats = append(caveats, got.Caveats...)
	// After the task: the mirror commit names it.
	if len(blockers) > 0 {
		result, err := t.deps.Dependencies(actor).Depend(ctx,
			opIDFor(actor, t.Name(), "depend", task.ID, args), worklet.DependencyChange{
				Task: task.ID, Project: task.Project, WaitingOnAdd: blockers,
			}, t.deps.Leads)
		if err != nil {
			// The item exists: failing the call would file a second one.
			return ownReceipt(ctx, turn, actor, t.Name(), answer, caveats,
				partial(answer, DependenciesFailedKey, caveat.DependenciesNotRecorded(
					worklet.ItemAddress(task.ID, got.Key, got.KeyCollision),
					dependencyFailure(task.ID, got.Key, got.KeyCollision,
						writeTold(ctx, actor, t.Name(), err)))))
		}
		t.deps.settle(ctx, result.Position)
		// The call answers for both records: the create's position is a floor
		// below the edge, and an `unknown` dependency must not read as landed. The
		// version moves too: the edge is a commit on this item.
		outcome, at, version := joinDepend(got.Outcome, got.Position,
			got.Version, result)
		answer["outcome"], answer["position"] = string(outcome), positionOf(at)
		answer["version"] = version
		caveats = append(caveats, result.Caveats...)
		if len(result.OneSided) > 0 {
			answer["dependencies_pending_mirror"] = len(result.OneSided)
		}
	}
	return ownReceipt(ctx, turn, actor, t.Name(), answer, caveats)
}

// joinDepend folds a dependency sequence that ran AFTER a call's first record
// into that call's one answer: the less certain outcome and the later position
// ([statelog.LessCertain], [statelog.Later]), and the item's later version. A
// caller barriers on the position, so the first record's alone would be a floor
// below the edge and an unknown dependency reported as applied.
//
// The version is THIS item's ([worklet.DependencyResult]'s, zero where it landed
// no commit here), never the sequence's last commit's, which may be another
// item's; where both wrote here the dependency's is newer, else the caller gets
// an `if_match` its own second write moved past.
func joinDepend(outcome statelog.Outcome, at statelog.Position, version int64,
	result worklet.DependencyResult) (statelog.Outcome, statelog.Position, int64) {

	if !result.Wrote() {
		return outcome, at, version
	}
	return statelog.LessCertain(outcome, result.Outcome),
		statelog.Later(at, result.Position), max(version, result.Version)
}

// createUnknown explains a create whose outcome is unknown: a failed result,
// for [unknownWrite]'s reason (a key beside "unknown" reads as the item's).
// What the caller does next is [unknownNext]'s.
func createUnknown(actor Actor, opID, id string, got worklet.WriteResult) tools.Result {
	filed := ""
	if got.Key != "" {
		filed = fmt.Sprintf(" If this attempt filed it, it is %s.",
			worklet.ItemNamed(id, got.Key, got.KeyCollision))
	}
	return unknownWrite(actor, CreateWorkItemTool, "the work item was filed",
		opID, got.Unvouched, filed+" "+unknownNext(got.Unvouched,
			sameCall(actor, CreateWorkItemTool),
			"Look for it with "+ListWorkItemsTool, "it files a second item"))
}

// dependencyFailure explains a create whose ITEM was filed and whose
// dependencies did not all land. Not [writeFailure], whose "NOT made" is false
// of the item and whose "call again" files a second one: update_work_item
// records whatever of `waiting_on` is still missing under its own operation. The
// item's address is [worklet.ItemAddress] (one filed on a key another task holds
// is reached by its id); why it stopped is [writeTold]'s.
func dependencyFailure(id, key string, collision bool, why string) string {
	filed := key + " (" + id + ")"
	if collision {
		filed = worklet.ItemNamed(id, key, collision)
	}
	return fmt.Sprintf("The item WAS filed, as %s, but recording what it "+
		"waits on did not finish: %s. Do NOT call "+worklet.CreateWorkItemTool+" again — that "+
		"files a second item. Set its dependencies with "+UpdateWorkItemTool+" on %s, "+
		"giving the same waiting_on: it records whichever of them are still "+
		"missing, and says so if one cannot be.", filed, why,
		worklet.ItemAddress(id, key, collision))
}

// createObjectKey is the object a create's operation is named for: the project
// the call names, or `new` for none (never the default; see the create).
func createObjectKey(project string) string {
	if key := strings.ToUpper(strings.TrimSpace(project)); key != "" {
		return key
	}
	return "new"
}

// createdTaskID is the id of the task a `create_work_item` call files: a UUIDv5
// over the create's operation id as the work began.
//
// A function of the operation, since a task's id is the subject its create
// arbitrates on: a re-run addresses the task the first run filed, where a fresh
// id per call filed two. As the work began ([Actor.asBegun], [commentID]'s
// reason): a rebased attempt (ADR-0029) re-mints its operations, but the task's
// row has not forgotten the item ([worklet.Writer.CreateTask]).
func createdTaskID(actor Actor, tool, project string, args map[string]any) string {
	opID := opIDFor(actor.asBegun(), tool, "create", createObjectKey(project), args)
	return uuid.NewSHA1(createdTaskNamespace, []byte(opID)).String()
}

// createdTaskNamespace is the uuid namespace a created task's id derives under.
// Fixed: it is durable in every task row.
var createdTaskNamespace = uuid.MustParse("8677bb1c-20e0-4fbe-ab44-846868b79b37")
