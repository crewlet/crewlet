package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
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
		"somebody arranged. Use the `params` of a view with list_work_items " +
		"to run it."
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
	container, err := parseContainerArg(argString(args, "container"))
	if err != nil {
		return failed(err.Error()), nil
	}
	listing, err := t.deps.Reader.Views(ctx, tracker.ViewQuery{
		Container: container,
		Viewer:    strings.TrimSpace(argString(args, "viewer")),
		// SESSION, like every other tool read here: a caller that saves
		// a view and then lists the strip sees the view it just saved.
		Level: statelog.ReadSession,
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
		"it, or omit it to create one. The parameters are list_work_items' " +
		"own, so save what you just ran."
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
					"`board` for what is moving, `calendar` for what is due.",
				"enum": []string{
					string(tracker.ViewList), string(tracker.ViewBoard),
					string(tracker.ViewCalendar),
				},
			},
			"params": map[string]any{
				"type":                 "object",
				"additionalProperties": map[string]any{"type": "string"},
				"description": "The query, in list_work_items' own parameter " +
					"names. Refused if it does not parse.",
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
	container, err := parseContainerArg(argString(args, "container"))
	if err != nil {
		return failed(err.Error()), nil
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
		"id": id, "outcome": string(result.Outcome), "version": result.Version,
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

// parseContainerArg reads the container in the query grammar's own spelling.
func parseContainerArg(raw string) (tracker.Container, error) {
	raw = strings.TrimSpace(raw)
	if raw == tracker.ContainerWorkspace {
		return tracker.Container{Kind: tracker.ContainerWorkspace}, nil
	}
	kind, id, found := strings.Cut(raw, ":")
	if !found || !tracker.ValidContainerKind(kind) || id == "" ||
		kind == tracker.ContainerWorkspace {

		return tracker.Container{}, fmt.Errorf("%q does not name a container. "+
			"Write `workspace`, `project:ENG`, `unit:engineering` or "+
			"`person:ana`.", clip(raw))
	}
	if kind == tracker.ContainerProject {
		id = strings.ToUpper(id)
	}
	return tracker.Container{Kind: kind, ID: id}, nil
}
