package builtin

import (
	"context"
	"errors"
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
	return "The projects this company files work into, with how much work " +
		"each holds — waiting (todo), started (active), done and closed — " +
		"who leads it and when it is meant to be finished. " +
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
				"type": "string",
				"description": "Narrows to the projects one unit of the org " +
					"chart owns, by that unit's id or its name.",
			},
			"archived": map[string]any{
				"type": "string",
				"enum": tracker.ArchivedModeNames(),
				"description": "Which set: `false` for the live projects, " +
					"`only` for the retired ones alone, `true` for both. " +
					"Default `false` — no new work is filed into a retired " +
					"project.",
			},
			"sort": map[string]any{
				"type": "string",
				// THE ENGINE'S OWN LIST, rendered rather than retyped:
				// a description naming a key the parse refuses is a
				// refusal a model cannot act on. No `enum`, because the
				// leading `-` doubles the set and a fourteen-entry
				// enumeration teaches a model less than the sentence.
				"description": "Orders the whole company's projects before " +
					"the page is taken, so `-active` is the most started work " +
					"anywhere rather than the most of one page, and `target` " +
					"the soonest target date (projects with none last). One of " +
					strings.Join(tracker.ProjectSortNames(), ", ") +
					", each optionally with a leading `-` for descending. " +
					"Default " + string(tracker.ProjectSortKey) + ".",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": "At most 50, which is also the default. A " +
					"company with more projects than that answers with " +
					"`total` beside `truncated`: narrow with q or unit.",
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
	// THE SAME GRAMMAR THE SCREEN'S `archived=` AND `sort=` ARE, parsed by
	// the one function that owns them — see [tracker.ParseProjectQuery]. A
	// model that spelled one wrong is told what is accepted rather than
	// quietly answered about a different set.
	q, err := tracker.ParseProjectQuery(tracker.MapParams(args))
	if err != nil {
		// CLIPPED, because the refusal quotes the model's OWN argument
		// back at it and a smuggled newline breaks the render — see
		// [clip], which is also why it is not shortened.
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return failed(clip(err.Error())), nil
	}
	// THE SEAT'S OWN PAGE, which is smaller than the listing's own cap and
	// has to be: two hundred rows encode at ≈ 93 KiB and [jsonAnswer]
	// REFUSES a tool answer past [ToolAnswerBytes] rather than cutting it,
	// so the tracker's screen-sized cap reached a model as advice to narrow
	// and no projects at all. A page with `total` beside it is the honest
	// answer — see [tracker.MaxProjectsPerToolAnswer].
	if q.Limit <= 0 || q.Limit > tracker.MaxProjectsPerToolAnswer {
		q.Limit = tracker.MaxProjectsPerToolAnswer
	}
	q.Units = t.deps.Units
	// THE SEAT'S OWN LEVEL, like every other read here — see
	// [seatReadLevel] for why it is a name and not a literal.
	q.Level = seatReadLevel
	listing, err := reader.Projects(ctx, q)
	if err != nil {
		return readFailure(tracker.ListProjectsTool, err), nil
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
		// THE CALLER'S OWN, which is what a model omitting the argument
		// meant — and the one project it is certain to be asking about.
		project = t.deps.defaultProject(actor)
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
	switch {
	case errors.Is(err, tracker.ErrNoProject):
		// THE READER'S OWN SENTENCE, which names the nearest keys. This
		// used to go through [readFailure], which told a model that
		// typed a key wrong that the tracker could not be read and that
		// it must not conclude the project does not exist — the one
		// conclusion it needed to draw.
		return refused(tools.RefusalNotFound, clip(err.Error())), nil
	case errors.Is(err, tracker.ErrNoType):
		return failed(clip(err.Error())), nil
	case err != nil:
		return readFailure(tracker.DescribeProjectTool, err), nil
	}
	return jsonResult(detail)
}
