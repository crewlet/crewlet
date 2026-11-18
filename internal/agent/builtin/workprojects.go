package builtin

import (
	"context"
	"errors"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The project tools: where work is filed, and what each project expects of it.
//
// A seat's, as the catalogue is: `create_work_item` refuses an unknown
// project, an undeclared type and an empty required field, and a model that
// cannot read those can only guess. `describe_project` is how a seat learns
// any project but its own (the turn-start prefetch covers only that): its
// statuses, which fields each type requires, and who leads the owning unit.
//
// Neither writes, and no companion does: a project's name, purpose and owning
// unit are chart-owned, written only by the epoch apply, and its field
// declarations are a lead's, through the operator surface.

// ProjectReader is the tracker read side these tools need.
type ProjectReader interface {
	Projects(ctx context.Context, q worklet.ProjectQuery) (
		worklet.ProjectListing, error)
	// CarriedProject is one project as describe_project hands it to a model,
	// whose field declarations write_project has it send back whole: so it
	// is read whole or refused where this node's build would drop part of it
	// (estate's tracker.project_carried), never shown short and written back.
	CarriedProject(ctx context.Context, q worklet.ProjectDetailQuery) (
		worklet.ProjectDetail, error)
}

type listProjects struct{ deps WorkDeps }

var _ tools.SeatCallable = (*listProjects)(nil)

func (t *listProjects) Name() string { return worklet.ListProjectsTool }

func (t *listProjects) Description() string {
	return "The projects this company files work into, with how much work " +
		"each holds — waiting (todo), started (active), done and closed — " +
		"who leads it and when it is meant to be finished. " +
		worklet.CreateWorkItemTool + " refuses a project that is not here."
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
				"enum": worklet.ArchivedModeNames(),
				"description": "Which set: `false` for the live projects, " +
					"`only` for the retired ones alone, `true` for both. " +
					"Default `false` — no new work is filed into a retired " +
					"project.",
			},
			"sort": map[string]any{
				"type": "string",
				// The engine's own list, rendered: a key the parse refuses
				// is a refusal a model cannot act on. No `enum`: the
				// leading `-` doubles the set, and fourteen entries teach
				// less than the sentence.
				"description": "Orders the whole company's projects before " +
					"the page is taken, so `-active` is the most started work " +
					"anywhere rather than the most of one page, and `target` " +
					"the soonest target date (projects with none last). One of " +
					words.Or(worklet.ProjectSortNames()) +
					", each optionally with a leading `-` for descending. " +
					"Default " + string(worklet.ProjectSortKey) + ".",
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

	// Identity is checked on a read too, as get_work_catalogue does: outside
	// a turn a seat's registry has no seat, and the tool would answer as nobody.
	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(worklet.ListProjectsTool, err), nil
	}
	reader, ok := t.deps.Reader.(ProjectReader)
	if !ok || t.deps.Reader == nil {
		return unconfigured(worklet.ListProjectsTool), nil
	}
	// The screen's `archived=` and `sort=` grammar, parsed by its owner
	// [worklet.ParseProjectQuery]: a misspelling is told what is accepted,
	// never answered about a different set.
	q, err := worklet.ParseProjectQuery(worklet.MapParams(args))
	if err != nil {
		// Clipped: the refusal quotes the model's own argument, and a
		// smuggled newline breaks the render (see [clip]).
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return failed(clip(err.Error())), nil
	}
	// The seat's page, below the listing's own cap: [jsonAnswer] refuses an
	// answer past [ToolAnswerBytes] rather than cutting it (200 rows ≈ 93 KiB),
	// so the screen's cap would answer with no projects. A page with `total`
	// is the honest answer (see [worklet.MaxProjectsPerToolAnswer]).
	if q.Limit <= 0 || q.Limit > worklet.MaxProjectsPerToolAnswer {
		q.Limit = worklet.MaxProjectsPerToolAnswer
	}
	q.Units = t.deps.Units
	// The seat's own level; [seatReadLevel] says why a name, not a literal.
	q.Level = seatReadLevel
	listing, err := reader.Projects(ctx, q)
	if err != nil {
		return readFailure(ctx, worklet.ListProjectsTool, err), nil
	}
	// Name and purpose are words whoever set them wrote; the row records
	// only the last changer (ADR-0040).
	read := newForeignRead(turn, actor.Handle)
	if len(listing.Projects) > 0 {
		namesBy(read)
	}
	return read.answer(ctx, worklet.ListProjectsTool, listing,
		"Ask for fewer with `limit`, or narrow with `unit` or `archived`.")
}

type describeProject struct{ deps WorkDeps }

var _ tools.SeatCallable = (*describeProject)(nil)

func (t *describeProject) Name() string { return worklet.DescribeProjectTool }

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
				"description": "The project key, as " + worklet.ListProjectsTool + " reports it. " +
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
		return unattributed(worklet.DescribeProjectTool, err), nil
	}
	reader, ok := t.deps.Reader.(ProjectReader)
	if !ok || t.deps.Reader == nil {
		return unconfigured(worklet.DescribeProjectTool), nil
	}
	project := strings.TrimSpace(argString(args, "project"))
	if project == "" {
		// Omitted means the caller's own project.
		project = t.deps.defaultProject(actor)
	}
	if project == "" {
		return failed("Name a project — this seat's unit owns none, so there " +
			"is no default to fall back on. " + worklet.ListProjectsTool + " reports the keys."), nil
	}
	detail, err := reader.CarriedProject(ctx, worklet.ProjectDetailQuery{
		Project: project,
		ForType: strings.TrimSpace(argString(args, "for_type")),
		Units:   t.deps.Units,
		Level:   seatReadLevel,
	})
	switch {
	case errors.Is(err, worklet.ErrNoProject):
		// The reader's own sentence, naming the nearest keys: [readFailure]
		// would tell a model that mistyped a key not to conclude the
		// project is missing, the one conclusion it needs.
		return refusedBy(tools.RefusalNotFound, err, clip(err.Error())), nil
	case errors.Is(err, worklet.ErrNoType):
		// A `for_type` the project does not file is the argument to change;
		// the reader's sentence names the types it does.
		return refusedBy(tools.RefusalInvalid, err, clip(err.Error())), nil
	case err != nil:
		return readFailure(ctx, worklet.DescribeProjectTool, err), nil
	}
	// Name, purpose, statuses and labels are words whoever set the policy
	// wrote, with no writer recorded (ADR-0040); the catalogue's types and
	// fields are company configuration, only neutralised.
	read := newForeignRead(turn, actor.Handle)
	namesBy(read)
	return read.answer(ctx, worklet.DescribeProjectTool, detail,
		"Narrow it with `for_type`.")
}
