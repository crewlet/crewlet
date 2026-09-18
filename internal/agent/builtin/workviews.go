package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
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
type ViewWriter interface {
	WriteView(ctx context.Context, opID string, view tracker.View) (tracker.WriteResult, error)
}

// ViewReader is the read side.
type ViewReader interface {
	Views(ctx context.Context, q tracker.ViewQuery) (tracker.ViewListing, error)
}

type listWorkViews struct{ deps WorkDeps }

var _ tools.Callable = (*listWorkViews)(nil)

func (t *listWorkViews) Name() string { return tracker.ListWorkViewsTool }

func (t *listWorkViews) Description() string {
	return "List the saved views on a project, unit, person or the whole " +
		"workspace. Every container has a list, a board and a calendar " +
		"without anybody saving one; what comes back beyond those is what " +
		"somebody arranged. To run one, pass its `id` as `view` to " +
		"list_work_items — its `params` are in the query grammar's own " +
		"names and are not this tool's arguments."
}

func (t *listWorkViews) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"container": containerParameter(),
			"viewer": map[string]any{
				"type": "string",
				"description": "Whose strip to render: their personal views " +
					"and their pins first. Omit for the shared strip.",
			},
		},
		"required": []string{"container"},
	}
}

func (t *listWorkViews) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	if t.deps.Reader == nil {
		return unconfigured(tracker.ListWorkViewsTool), nil
	}
	container, refusal := parseContainerArg(argString(args, "container"))
	if refusal != "" {
		return failed(refusal), nil
	}
	listing, err := t.deps.Reader.Views(ctx, tracker.ViewQuery{
		Container: container,
		Viewer:    strings.TrimSpace(argString(args, "viewer")),
		// THE SEAT'S OWN LEVEL, like every other tool read here: a
		// caller that saves a view and then lists the strip sees the
		// view it just saved — see [seatReadLevel].
		Level: seatReadLevel,
	})
	if err != nil {
		return failed(readFailure(tracker.ListWorkViewsTool, err)), nil
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
					"watcher, unit, goal, sprint, sort, limit, preset — is " +
					"the same word. Refused if any key does not parse, and " +
					"`view`, `cursor`, `read_level`, `max_lag_seconds`, " +
					"`max_lag_seq` and `min_position` are refused " +
					"outright: they are about the reader rather than the " +
					"rows.",
			},
			"owner": map[string]any{
				"type": "string",
				"description": "A handle makes the view PERSONAL: it appears " +
					"in that person's strip and nobody else's. Omit to share it.",
			},
			"protected": map[string]any{
				"type":        "boolean",
				"description": "True stops anyone but the owner changing it.",
			},
			"default": map[string]any{
				"type": "boolean",
				"description": "True makes it the container's landing tab, " +
					"taking that from whichever view held it.",
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
	// THE CALLER'S ID WHEN IT HAS ONE, a fresh one when it does not. A
	// verb that always minted would write a second view on every retry of
	// an `unknown` outcome; one that always required an id could not
	// create.
	id := strings.TrimSpace(argString(args, "id"))
	if id == "" {
		id = uuid.NewString()
	}
	view := tracker.View{
		ID:        id,
		Container: container,
		Name:      strings.TrimSpace(argString(args, "name")),
		Type:      tracker.ViewType(strings.TrimSpace(argString(args, "type"))),
		Params:    argStringMap(args, "params"),
		Owner:     strings.TrimSpace(argString(args, "owner")),
		Protected: argBool(args, "protected"),
		Default:   argBool(args, "default"),
		Icon:      strings.TrimSpace(argString(args, "icon")),
	}
	result, err := t.deps.ViewWriter(actor).WriteView(ctx, "view-"+id, view)
	if err != nil {
		return failed(writeFailure(tracker.SaveWorkViewTool, err)), nil
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
// reader on this surface returns (`goalTargets`, `catalogueFields`,
// `resolveHandle`): both callers render this straight into [failed] and
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
