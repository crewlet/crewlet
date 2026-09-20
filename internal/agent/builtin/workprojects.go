package builtin

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The project tools: where work is filed, and what each project expects of it.
//
// # Both are a SEAT's, and the reason is the same as the catalogue's
//
// `create_work_item` refuses a project the company does not have, refuses a
// type it has not declared and refuses a required field left empty — and a
// model that cannot READ any of that can only guess. Telling it what exists is
// cheaper than refusing it repeatedly, and a refusal that names the valid
// values is only half an answer if the model had no way to look them up first.
//
// `describe_project` is the one a woken seat reaches for. The turn-start
// prefetch renders a ≤2 KB block from local rows for the seat's OWN project;
// this is how it learns any other one — which statuses exist, which fields are
// required of which type, and who leads the unit that owns it.
//
// # Neither writes, and there is no companion that does
//
// A project's name, purpose and owning unit are CHART-OWNED: they are written
// by the epoch apply from the org chart and by nothing else, so a seat editing
// them would be editing the company's structure through the back door. Its
// field declarations are a lead's, through the operator surface.

// ProjectReader is the tracker read side these tools need.
type ProjectReader interface {
	Projects(ctx context.Context, q tracker.ProjectQuery) (
		tracker.ProjectListing, error)
	Project(ctx context.Context, q tracker.ProjectDetailQuery) (
		tracker.ProjectDetail, error)
}

type listProjects struct{ deps WorkDeps }

var _ tools.SeatCallable = (*listProjects)(nil)

func (t *listProjects) Name() string { return tracker.ListProjectsTool }

func (t *listProjects) Description() string {
	return "The projects this company files work into, with how much open " +
		"work each holds and who leads it. " +
		"create_work_item refuses a project that is not here."
}

func (t *listProjects) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q": map[string]any{
				"type": "string",
				"description": "Narrows by a word in the key, the name or the " +
					"purpose. Omit for every project.",
			},
			"unit": map[string]any{
				"type":        "string",
				"description": "Narrows to the projects one unit of the org chart owns.",
			},
			"archived": map[string]any{
				"type": "boolean",
				"description": "True also lists retired projects. Default " +
					"false — no new work is filed into one.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "At most 200, which is also the default.",
			},
		},
	}
}

func (t *listProjects) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *listProjects) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	// THE IDENTITY CHECK IS ON A READ TOO, exactly as it is on
	// get_work_catalogue beside it: outside a turn a seat's registry has
	// no seat, and a tool that answered anyway would answer as nobody.
	if _, err := t.deps.actor(ctx, turn); err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.ListProjectsTool), nil
	}
	reader, ok := t.deps.Reader.(ProjectReader)
	if !ok || t.deps.Reader == nil {
		return unconfigured(tracker.ListProjectsTool), nil
	}
	listing, err := reader.Projects(ctx, tracker.ProjectQuery{
		Q:        strings.TrimSpace(argString(args, "q")),
		Unit:     strings.TrimSpace(argString(args, "unit")),
		Archived: argBool(args, "archived"),
		Limit:    argInt(args, "limit", 0),
		Units:    t.deps.Units,
		// THE SEAT'S OWN LEVEL, like every other read here — see
		// [seatReadLevel] for why it is a name and not a literal.
		Level: seatReadLevel,
	})
	if err != nil {
		return failed(readFailure(tracker.ListProjectsTool, err)), nil
	}
	return jsonResult(listing)
}

type describeProject struct{ deps WorkDeps }

var _ tools.SeatCallable = (*describeProject)(nil)

func (t *describeProject) Name() string { return tracker.DescribeProjectTool }

func (t *describeProject) Description() string {
	return "Everything one project expects of the work filed into it: the six " +
		"statuses with what each means, the types it files, the custom fields " +
		"grouped by which type they apply to (required ones first, with their " +
		"options), its tags, its default assignee and its lead. Read this " +
		"before filing into a project you have not " +
		"used — a create refused for a missing required field is one round " +
		"this call would have saved."
}

func (t *describeProject) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{
				"type": "string",
				"description": "The project key, as list_projects reports it. " +
					"Omit for your own.",
			},
			"for_type": map[string]any{
				"type": "string",
				"description": "Narrows the fields to the ones that apply to " +
					"one task type, plus the ones that apply to every type. " +
					"Pass the type you are about to file.",
			},
		},
	}
}

func (t *describeProject) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *describeProject) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.DescribeProjectTool), nil
	}
	reader, ok := t.deps.Reader.(ProjectReader)
	if !ok || t.deps.Reader == nil {
		return unconfigured(tracker.DescribeProjectTool), nil
	}
	project := strings.TrimSpace(argString(args, "project"))
	if project == "" {
		// THE SEAT'S OWN, which is what a model omitting the argument
		// meant — and the one project it is certain to be asking about.
		project = t.deps.defaultProject(actor.Handle)
	}
	if project == "" {
		return failed("Name a project — this seat's unit owns none, so there " +
			"is no default to fall back on. list_projects reports the keys."), nil
	}
	detail, err := reader.Project(ctx, tracker.ProjectDetailQuery{
		Project: project,
		ForType: strings.TrimSpace(argString(args, "for_type")),
		Units:   t.deps.Units,
		Level:   seatReadLevel,
	})
	if err != nil {
		return failed(readFailure(tracker.DescribeProjectTool, err)), nil
	}
	return jsonResult(detail)
}
