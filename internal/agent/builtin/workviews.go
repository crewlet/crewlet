package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The saved-view tools, and they are the OPERATOR's alone.
//
// A view is furniture: a name, a shape and a filter, arranged so a person
// finds the same question tomorrow. A seat's job is the work rather than the
// furniture around it, and a seat that could rearrange a shared board would be
// one more thing a founder has to supervise for no delivery — so these are
// registered by [OperatorTools] and by nothing else.
//
// # Why the container is one string
//
// `container=project:ENG`, exactly as [tracker.ParseQuery] reads it, because a
// caller that lists a strip and then lists the work in it must not have to
// write the same container two ways. The parse is shared for that reason.

// ViewWriter is the tracker write side these tools need.
//
// ViewPrior is the read a save is decided on: a save replaces a view whole by
// id, so who may make one is a question about the view it OVERWRITES as well
// as the one it writes, and the write refuses a prior that no longer holds —
// see [tracker.ViewPrior].
type ViewWriter interface {
	ViewPrior(ctx context.Context, id string) (tracker.ViewPrior, error)
	WriteView(ctx context.Context, opID string, view tracker.View,
		prior tracker.ViewPrior) (tracker.WriteResult, error)
}

// ViewReader is the read side.
type ViewReader interface {
	Views(ctx context.Context, q tracker.ViewQuery) (tracker.ViewListing, error)
}

type listWorkViews struct{ deps WorkDeps }

var _ tools.SeatCallable = (*listWorkViews)(nil)

func (t *listWorkViews) Name() string { return tracker.ListWorkViewsTool }

func (t *listWorkViews) Description() string {
	return "List the saved views on a project, unit, person or the whole " +
		"workspace, as YOUR strip: the shared views, your own personal ones, " +
		"and your pins first. Every container has a list, a board and a " +
		"calendar without anybody saving one; what comes back beyond those " +
		"is what somebody arranged. To run one, pass its `id` as `view` to " +
		"list_work_items — its `params` are in the query grammar's own " +
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

	// WHOSE STRIP IS THE CALLER'S, and never an argument. The tool took a
	// free `viewer` once, so any caller rendered anybody's personal views
	// and pins by naming them — an arrangement nobody else sees is that
	// person's, and the read had no rule at all. The viewer is the name
	// the caller's OWN pins and personal views are written under, which is
	// the actor's handle: the same value `set_pins` and `save_work_view`
	// write with, so a strip shows exactly what its reader arranged.
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.ListWorkViewsTool), nil
	}
	if t.deps.Reader == nil {
		return unconfigured(tracker.ListWorkViewsTool), nil
	}
	container, refusal := parseContainerArg(argString(args, "container"))
	if refusal != "" {
		return failed(refusal), nil
	}
	listing, err := t.deps.Reader.Views(ctx, tracker.ViewQuery{
		Container: container,
		Viewer:    actor.Handle,
		// THE SEAT'S OWN LEVEL, like every other tool read here: a
		// caller that saves a view and then lists the strip sees the
		// view it just saved — see [seatReadLevel].
		Level: seatReadLevel,
	})
	if err != nil {
		return readFailed(tracker.ListWorkViewsTool, err), nil
	}
	return jsonResult(map[string]any{
		"count": len(listing.Views), "views": listing.Views,
		"read_level": listing.Level, "complete": listing.Complete,
	})
}

type saveWorkView struct{ deps WorkDeps }

var _ tools.Callable = (*saveWorkView)(nil)

func (t *saveWorkView) Name() string { return tracker.SaveWorkViewTool }

func (t *saveWorkView) Description() string {
	return "Save a view — a named query with a shape — on a project, unit, " +
		"person or the workspace. Pass an existing view's `id` to replace " +
		"it, or omit it to create one. `params` is the QUERY GRAMMAR, not " +
		"this surface's arguments: four of list_work_items' arguments are " +
		"spelled differently there, and the view is refused if any key " +
		"does not parse."
}

func (t *saveWorkView) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type":        "string",
				"description": "The view to replace. Omit to create a new one.",
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
				// THE ENGINE'S OWN CLOSED SET, read from it rather than
				// copied. This was a literal naming three of the four the
				// validator took: a seat could not save a timeline view
				// although the refusal named it as one of them and the
				// tracker ships a builtin timeline — the shape was reachable
				// by reading and unreachable by writing, with the tool's own
				// schema as the only thing saying otherwise.
				"enum": tracker.ViewTypeNames(),
			},
			"params": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "The query, in the grammar's own parameter " +
					"names — which list_work_items spells differently for " +
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
					"so pin that instead with set_pins.",
			},
			"icon": map[string]any{"type": "string"},
		},
		"required": []string{"container", "name", "type"},
	}
}

func (t *saveWorkView) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *saveWorkView) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.SaveWorkViewTool), nil
	}
	if t.deps.ViewWriter == nil {
		return unconfigured(tracker.SaveWorkViewTool), nil
	}
	container, refusal := parseContainerArg(argString(args, "container"))
	if refusal != "" {
		return failed(refusal), nil
	}
	// A PERSONAL VIEW IS THE CALLER'S OWN RECORD, under the one name every
	// personal record of theirs is kept under — the name `list_work_views`
	// and the dashboard's strip read it back by. It was a free `owner`
	// handle, so a bound person typing their login saved a view no strip of
	// theirs reads, and one who marked it protected was then refused their
	// own next save, which the tracker checks against the writer's name.
	var owner string
	if argBool(args, "personal") {
		principal, _ := iam.From(ctx)
		if owner = iam.RecordOwner(principal); owner == "" {
			return failed("A personal view is kept on its owner's own " +
				"record, and this credential names nobody who has one. Save " +
				"it shared instead."), nil
		}
	}
	// THE CALLER'S ID WHEN IT HAS ONE, a fresh one when it does not. A
	// verb that always minted would write a second view on every retry of
	// an `unknown` outcome; one that always required an id could not
	// create.
	writer := t.deps.ViewWriter(actor)
	id := strings.TrimSpace(argString(args, "id"))
	var prior tracker.ViewPrior
	if id == "" {
		id = uuid.NewString()
	} else {
		// A SAVE UNDER AN ID IT DID NOT MINT MAY REPLACE SOMEBODY ELSE'S
		// VIEW, so the view it would overwrite is decided on as well as
		// the one it writes. The gate decided the NEW view from the
		// arguments; it could not decide the old one, which is a stored
		// row — and decided on the arguments alone, anybody who may keep
		// a view of their own could take over a project's shared tab or
		// a colleague's personal one by naming its id. Asked here with
		// the same action, on the stored owner and container, as
		// `work.route` is asked on a task's stored project; the write
		// then refuses if the view changes under this decision.
		if prior, err = writer.ViewPrior(ctx, id); err != nil {
			return readFailed(tracker.SaveWorkViewTool, err), nil
		}
		if prior.Held {
			if refused := t.deps.mayWrite(ctx, authz.ActionViewSave,
				viewObject(prior.Owner, prior.Container)); refused != nil {

				return *refused, nil
			}
		}
	}
	view := tracker.View{
		ID:        id,
		Container: container,
		Name:      strings.TrimSpace(argString(args, "name")),
		Type:      tracker.ViewType(strings.TrimSpace(argString(args, "type"))),
		Params:    argStringMap(args, "params"),
		Owner:     owner,
		Protected: argBool(args, "protected"),
		Default:   argBool(args, "default"),
		Icon:      strings.TrimSpace(argString(args, "icon")),
	}
	result, err := writer.WriteView(ctx, "view-"+id, view, prior)
	if err != nil {
		return writeFailed(tracker.SaveWorkViewTool, err), nil
	}
	t.deps.settle(ctx, result.Position)
	return jsonResult(map[string]any{
		"id": id, "outcome": string(result.Outcome), "position": positionOf(result.Position), "version": result.Version,
	})
}

// containerParameter is the one container argument both tools take.
func containerParameter() map[string]any {
	return map[string]any{
		"type": "string",
		"description": "Where the view lives: `workspace`, `project:ENG`, " +
			"`unit:engineering` or `person:ana`.",
	}
}

// parseContainerArg reads the container in the query grammar's own spelling,
// or returns the refusal the model is shown.
//
// A REFUSAL STRING RATHER THAN AN ERROR, which is what every other argument
// reader on this surface returns (`catalogueFields`, `resolveHandle`): both
// callers render this straight into [failed] and
// neither compares it, wraps it or propagates it, so the only reader is the
// model. Shaped as an `error` it was prose no `errors.Is` would ever ask
// about, written in a sentence style the error-string convention forbids.
func parseContainerArg(raw string) (tracker.Container, string) {
	raw = strings.TrimSpace(raw)
	if raw == tracker.ContainerWorkspace {
		return tracker.Container{Kind: tracker.ContainerWorkspace}, ""
	}
	kind, id, found := strings.Cut(raw, ":")
	if !found || !tracker.ValidContainerKind(kind) || id == "" ||
		kind == tracker.ContainerWorkspace {

		return tracker.Container{}, fmt.Sprintf("%q does not name a container. "+
			"Write `workspace`, `project:ENG`, `unit:engineering` or "+
			"`person:ana`.", clip(raw))
	}
	if kind == tracker.ContainerProject {
		id = strings.ToUpper(id)
	}
	return tracker.Container{Kind: kind, ID: id}, ""
}
