package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The saved-view tools, the operator's alone (registered only by
// [OperatorTools]): a view is furniture arranged so a person finds the same
// question tomorrow, and a seat that could rearrange a shared board is one
// more thing a founder supervises for no delivery.
//
// The container is one string, `container=project:ENG`, as [worklet.ParseQuery]
// reads it, so a caller listing a strip and then its work writes it one way.

// ViewWriter is the tracker write side these tools need. ViewPrior is the read
// a save is decided on: a save replaces a view whole by id, so it is decided on
// the view it overwrites too, and the write refuses a prior that no longer
// holds ([worklet.ViewPrior]). DeleteView writes against the same prior: who
// may delete a view is who may replace it ([worklet.Writer.DeleteView]).
type ViewWriter interface {
	ViewPrior(ctx context.Context, id string) (worklet.ViewPrior, error)
	WriteView(ctx context.Context, opID string, view worklet.View,
		prior worklet.ViewPrior) (worklet.WriteResult, error)
	DeleteView(ctx context.Context, opID, id string,
		prior worklet.ViewPrior) (worklet.WriteResult, error)
}

type listWorkViews struct{ deps WorkDeps }

var _ tools.SeatCallable = (*listWorkViews)(nil)

func (t *listWorkViews) Name() string { return worklet.ListWorkViewsTool }

func (t *listWorkViews) Description() string {
	return "List the saved views on a project, unit, person or the whole " +
		"workspace, as YOUR strip: the shared views, your own personal ones, " +
		"and your pins first. Every container has a list, a board and a " +
		"calendar without anybody saving one; what comes back beyond those " +
		"is what somebody arranged. To run one, pass its `id` as `view` to " +
		ListWorkItemsTool + " — its `params` are in the query grammar's own " +
		"names and are not this tool's arguments."
}

func (t *listWorkViews) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"container": containerParameter(),
		},
		"required": []string{"container"},
	}
}

func (t *listWorkViews) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *listWorkViews) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	// The strip is the caller's, never an argument: personal views and pins
	// are nobody else's to read. The viewer is the actor's handle, the name
	// `set_pins` and `save_work_view` write under.
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(worklet.ListWorkViewsTool, err), nil
	}
	if t.deps.Reader == nil {
		return unconfigured(worklet.ListWorkViewsTool), nil
	}
	container, refusal := parseContainerArg(argString(args, "container"))
	if refusal != "" {
		return failed(refusal), nil
	}
	// Read whole or not at all ([WorkReader.CarriedViews]): save_work_view
	// replaces a listed view whole with what the model re-sends, so a view
	// holding a field a newer build added is refused here rather than shown
	// short and saved back without it.
	listing, err := t.deps.Reader.CarriedViews(ctx, worklet.ViewQuery{
		Container: container,
		Viewer:    actor.Handle,
		// The chart, so `unit:engineering` and `unit:eng` are one strip
		// ([worklet.Units]).
		Units: t.deps.Units,
		// A caller that saves a view then lists the strip sees it
		// ([seatReadLevel]).
		Level: seatReadLevel,
	})
	if err != nil {
		return readFailure(ctx, worklet.ListWorkViewsTool, err), nil
	}
	// A view's name and filter are words whoever saved it wrote (ADR-0040),
	// and the row records its owner, not who named it: a shared view is
	// anybody's who may keep one in its container.
	read := newForeignRead(turn, actor.Handle)
	if len(listing.Views) > 0 {
		namesBy(read)
	}
	return read.answer(ctx, worklet.ListWorkViewsTool, map[string]any{
		"count": len(listing.Views), "views": listing.Views,
		"read_level": listing.Level, "complete": listing.Complete,
	}, "Narrow it with `container`.")
}

type saveWorkView struct{ deps WorkDeps }

var _ tools.Callable = (*saveWorkView)(nil)

func (t *saveWorkView) Name() string { return worklet.SaveWorkViewTool }

func (t *saveWorkView) Description() string {
	return "Save a view — a named query with a shape — on a project, unit, " +
		"person or the workspace. Pass an existing view's `id` to replace " +
		"it, or omit it to create one. `params` is the QUERY GRAMMAR, not " +
		"this surface's arguments: four of " + ListWorkItemsTool + "' arguments are " +
		"spelled differently there, and the view is refused if any key " +
		"does not parse."
}

func (t *saveWorkView) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type": "string",
				"description": "The view to replace: an id " + worklet.ListWorkViewsTool + " " +
					"lists, or that a save answered. Omit to create a new " +
					"one, whose id is minted and answered back.",
			},
			"container": containerParameter(),
			"name": map[string]any{
				"type":        "string",
				"description": "The tab's label.",
			},
			"type": map[string]any{
				"type": "string",
				"description": "The shape: `list` for what is there, " +
					"`board` for what is moving, `calendar` for what is due, " +
					"`timeline` for how it lies against a date axis, " +
					"`table` for one field per column, compared down it. " +
					"Saving a table with `removed: true` in `params` is a " +
					"trash listing, which is what the builtin `trash` tab is.",
				// The engine's own closed set, read rather than copied, so the
				// schema never omits a shape the validator takes.
				"enum": worklet.ViewTypeNames(),
			},
			"params": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "The query, in the grammar's own parameter " +
					"names — which " + ListWorkItemsTool + " spells differently for " +
					"four of its arguments: " + AliasSentence() + ". A " +
					"custom field is `f.<ref>`. Everything else — assignee, " +
					"type, priority, due, updated, created, reporter, " +
					"watcher, unit, sort, limit, preset — is " +
					"the same word. Refused if any key does not parse, and " +
					"`view`, `cursor`, `read_level`, `max_lag_seconds`, " +
					"`max_lag_seq` and `min_position` are refused " +
					"outright: they are about the reader rather than the " +
					"rows.",
			},
			"personal": map[string]any{
				"type": "boolean",
				"description": "True makes the view YOURS: it appears in " +
					"your own strip and nobody else's. Omit to share it on " +
					"its container. A personal view is always the caller's " +
					"own — there is no naming somebody else's.",
			},
			"protected": map[string]any{
				"type": "boolean",
				"description": "On a personal view, true stops anyone but " +
					"you changing it.",
			},
			"default": map[string]any{
				"type": "boolean",
				"description": "True makes it the container's landing tab, " +
					"taking that from whichever view held it. Only a SHARED " +
					"view can be: nobody but its owner sees a personal one, " +
					"so pin that instead with " + worklet.SetPinsTool + ".",
			},
			"icon": map[string]any{"type": "string"},
		},
		"required": []string{"container", "name", "type"},
	})
}

func (t *saveWorkView) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *saveWorkView) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(worklet.SaveWorkViewTool, err), nil
	}
	if t.deps.ViewWriter == nil {
		return unconfigured(worklet.SaveWorkViewTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, worklet.SaveWorkViewTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	container, refusal := parseContainerArg(argString(args, "container"))
	if refusal != "" {
		return failed(refusal), nil
	}
	// A personal view is the caller's own record, under the name
	// [iam.RecordOwner] gives every personal record of theirs, which
	// `list_work_views`, the dashboard's strip and the tracker's protected
	// check all read; a free `owner` argument saves one no strip reads.
	var owner string
	if argBool(args, "personal") {
		principal, _ := iam.From(ctx)
		if owner = iam.RecordOwner(principal); owner == "" {
			return failed("A personal view is kept on its owner's own " +
				"record, and this credential names nobody who has one. Save " +
				"it shared instead."), nil
		}
	}
	// A unit container is stored under the team's key, as a project's is
	// upper-cased: a view saved under one spelling would be missing from the
	// strip asked for under the other.
	container = worklet.CanonicalContainer(t.deps.Units, container)
	writer := t.deps.ViewWriter(actor)
	// The caller's id, else one derived from the operation: the id is the
	// subject the write arbitrates on, so a per-call mint would make every
	// retry (a brought-back `op_id`, an `Idempotency-Key`, a re-run turn) a
	// second view ([createdViewID], as [createdTaskID]).
	id := strings.TrimSpace(argString(args, "id"))
	// The operation is the call's ([opIDFor]), not the view's: one fixed id
	// per view would let the broker's duplicate window collapse a second edit
	// into the first, answered `applied` without landing.
	opID := opIDFor(actor, t.Name(), "view", viewObjectKey(id), args)
	var prior worklet.ViewPrior
	if id == "" {
		id = createdViewID(actor, t.Name(), args)
	} else {
		// A save under an existing id may replace somebody else's view.
		// The gate decided the new view from the arguments; the stored
		// one is decided here, on its owner and container (as
		// `work.route` is on a task's stored project), or anybody could
		// take over a shared tab by naming its id. The write refuses if
		// the view changes under this decision.
		if prior, err = writer.ViewPrior(ctx, id); err != nil {
			return readFailure(ctx, worklet.SaveWorkViewTool, err), nil
		}
		if !prior.Held {
			// An id is only ever one this engine minted ([createdViewID]);
			// creating under one nobody holds would make whatever the
			// caller typed a primary key and write subject.
			return refused(tools.RefusalNotFound, fmt.Sprintf(
				worklet.SaveWorkViewTool+": there is no view %q to replace. Name a "+
					"view "+worklet.ListWorkViewsTool+" lists, or omit `id` to create a "+
					"new one — its id is minted and answered back.",
				clip(id))), nil
		}
		if denied := t.deps.mayWrite(ctx, authz.ActionViewSave,
			viewObject(prior.Owner, prior.Container)); denied != nil {

			return *denied, nil
		}
	}
	view := worklet.View{
		ID:        id,
		Container: container,
		Name:      strings.TrimSpace(argString(args, "name")),
		Type:      worklet.ViewType(strings.TrimSpace(argString(args, "type"))),
		Params:    argStringMap(args, "params"),
		Owner:     owner,
		Protected: argBool(args, "protected"),
		Default:   argBool(args, "default"),
		Icon:      strings.TrimSpace(argString(args, "icon")),
	}
	result, err := writer.WriteView(ctx, opID, view, prior)
	if err != nil {
		return writeFailure(ctx, actor, worklet.SaveWorkViewTool, err), nil
	}
	if result.Outcome == statelog.OutcomeUnknown {
		// Never the id of a view nobody can say was saved: handed back, it
		// names a view that may not exist.
		where := container.Kind
		if container.ID != "" {
			where += ":" + container.ID
		}
		return unknownWrite(actor, worklet.SaveWorkViewTool,
			fmt.Sprintf("the view %q was saved", view.Name), opID,
			result.Unvouched, unknownNext(result.Unvouched,
				sameCall(actor, worklet.SaveWorkViewTool),
				fmt.Sprintf("List the views in %s with "+worklet.ListWorkViewsTool, where),
				"it saves a second view")), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(withOperation(map[string]any{
		"id": id, "outcome": string(result.Outcome), "position": positionOf(result.Position), "version": result.Version,
	}, actor))
}

type deleteWorkView struct{ deps WorkDeps }

var _ tools.Callable = (*deleteWorkView)(nil)

func (t *deleteWorkView) Name() string { return worklet.DeleteWorkViewTool }

func (t *deleteWorkView) Description() string {
	return "Delete a saved view: it leaves every strip, the list of views " +
		"and every pin, and running it is refused from then on. Name it by " +
		"the `id` " + worklet.ListWorkViewsTool + " lists. Whoever may change " +
		"the view may delete it, and a protected view only its owner. The " +
		"views every container has without anybody saving one (list, board, " +
		"calendar and the rest) have no id and cannot be deleted."
}

func (t *deleteWorkView) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type": "string",
				"description": "The view to delete: an id " + worklet.ListWorkViewsTool +
					" lists, or that a save answered.",
			},
		},
		"required": []string{"id"},
	})
}

func (t *deleteWorkView) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *deleteWorkView) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(worklet.DeleteWorkViewTool, err), nil
	}
	if t.deps.ViewWriter == nil {
		return unconfigured(worklet.DeleteWorkViewTool), nil
	}
	actor, denied := t.deps.bindOperation(actor, worklet.DeleteWorkViewTool, args)
	if denied != "" {
		return failed(denied), nil
	}
	id := strings.TrimSpace(argString(args, "id"))
	if id == "" {
		return failed(worklet.DeleteWorkViewTool + ": name the view to delete by " +
			"its `id`, as " + worklet.ListWorkViewsTool + " lists it."), nil
	}
	writer := t.deps.ViewWriter(actor)
	// The stored view is decided on, as for a replacing save: the gate has no
	// object to form from an id, and the write refuses a view that moved or
	// changed hands since.
	prior, err := writer.ViewPrior(ctx, id)
	if err != nil {
		return readFailure(ctx, worklet.DeleteWorkViewTool, err), nil
	}
	if !prior.Held {
		return refused(tools.RefusalNotFound, fmt.Sprintf(
			worklet.DeleteWorkViewTool+": there is no view %q to delete. Name a "+
				"view "+worklet.ListWorkViewsTool+" lists.", clip(id))), nil
	}
	if denied := t.deps.mayWrite(ctx, authz.ActionViewDelete,
		viewObject(prior.Owner, prior.Container)); denied != nil {

		return *denied, nil
	}
	opID := opIDFor(actor, t.Name(), "view", id, args)
	result, err := writer.DeleteView(ctx, opID, id, prior)
	if err != nil {
		return writeFailure(ctx, actor, worklet.DeleteWorkViewTool, err), nil
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return unknownWrite(actor, worklet.DeleteWorkViewTool,
			fmt.Sprintf("the view %s was deleted", clip(id)), opID,
			result.Unvouched, unknownNext(result.Unvouched,
				sameCall(actor, worklet.DeleteWorkViewTool),
				"List the views with "+worklet.ListWorkViewsTool,
				"it deletes nothing twice")), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(withOperation(map[string]any{
		"id": id, "outcome": string(result.Outcome), "position": positionOf(result.Position), "version": result.Version,
	}, actor))
}

// createdViewID is the id a new view is saved under: a UUIDv5 over the
// operation as the work began, for [createdTaskID]'s reasons, so the same
// operation made again (a rebased turn included) addresses the view its first
// attempt saved, and a fresh operation a fresh view.
func createdViewID(actor Actor, tool string, args map[string]any) string {
	opID := opIDFor(actor.asBegun(), tool, "view", viewObjectKey(""), args)
	return uuid.NewSHA1(createdViewNamespace, []byte(opID)).String()
}

// viewObjectKey is the object a save's operation is named for: the view it
// names, or `new` for a new view (whose id derives from the operation), told
// apart from other new views by the digest of the call's arguments.
func viewObjectKey(id string) string {
	if id == "" {
		return "new"
	}
	return id
}

// createdViewNamespace is the uuid namespace a new view's id is derived under.
// Fixed for the life of the format: it is durable in every view row.
var createdViewNamespace = uuid.MustParse("3d5c2f0e-8a4b-4c1d-9e7f-6b2a1c0d9e8f")

// containerParameter is the one container argument both tools take.
func containerParameter() map[string]any {
	return map[string]any{
		"type": "string",
		"description": "Where the view lives: `workspace`, `project:ENG`, " +
			"`unit:engineering` or `person:ana`.",
	}
}

// parseContainerArg reads the container in the query grammar's own spelling,
// or returns the refusal the model is shown. A string rather than an error, as
// every argument reader here (`catalogueFields`, `resolveHandle`): its only
// reader is the model, through [failed], and nothing compares or wraps it.
func parseContainerArg(raw string) (worklet.Container, string) {
	raw = strings.TrimSpace(raw)
	if raw == worklet.ContainerWorkspace {
		return worklet.Container{Kind: worklet.ContainerWorkspace}, ""
	}
	kind, id, found := strings.Cut(raw, ":")
	if !found || !worklet.ValidContainerKind(kind) || id == "" ||
		kind == worklet.ContainerWorkspace {

		return worklet.Container{}, fmt.Sprintf("%q does not name a container. "+
			"Write `workspace`, `project:ENG`, `unit:engineering` or "+
			"`person:ana`.", clip(raw))
	}
	if kind == worklet.ContainerProject {
		id = strings.ToUpper(id)
	}
	return worklet.Container{Kind: kind, ID: id}, ""
}
