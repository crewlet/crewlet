package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// `write_project` — the project settings a TOOL owns, and who owns each.
//
// # Why one verb over two writers
//
// A project's policy and its tag set are two objects on two subjects, so that
// every seat's tag add never contends with a lead's field edit (see
// internal/tracker/project.go). They are ONE TOOL anyway, because the split is
// about arbitration and a caller's question is not: "change something about
// this project" is one thought, and a surface that made it two would leave a
// model choosing between `write_project` and `write_project_tags` on grounds it
// cannot see. The tool does two writes and reports both.
//
// # Why a SEAT holds it
//
// For exactly one facet: `tags.add`. A tag is how work is grouped for a week,
// declaring one is open to every seat by design, and `create_work_item` refuses
// a label the project has not declared — so a seat without this verb is a seat
// that can never use the `labels` argument on the tools it does hold. Every
// other facet is gated inside, and the refusals name who can.

// ProjectWriter is the tracker write side this tool needs.
//
// BOTH VERBS IN ONE INTERFACE although they are two objects, because this is
// the consumer's interface and the consumer calls both — the seam is "what a
// project settings tool needs", not "what one tracker subject accepts".
type ProjectWriter interface {
	WriteProject(ctx context.Context, opID, key string, edit tracker.ProjectEdit,
		authority tracker.ProjectAuthority) (tracker.WriteResult, error)
	WriteTags(ctx context.Context, opID, project string, edit tracker.TagEdit,
		authority tracker.TagAuthority) (tracker.WriteResult, error)

	// EnsureTags is the INLINE half of the declare rule, used by the
	// create and update tools rather than by this one: a seat that said
	// `labels_create_missing` declares what its write is about to use in
	// one append, before the task's own.
	EnsureTags(ctx context.Context, opID, project string, tags []string) (
		[]string, []string, error)
}

// LeadsProject reports whether a handle leads the unit that owns a project.
//
// A SEAM RATHER THAN A CHART, for the reason [Leads] is one: the answer is a
// fact about the company's configuration, this package holds none, and a lead
// relation derived here would be a second opinion about the hierarchy.
//
// NIL RESOLVES FALSE, which refuses every action naming what is missing — the
// safe direction, and the one an operator can diagnose: a surface that wired
// no lookup loses the verb rather than opening it to everybody.
type LeadsProject func(ctx context.Context, actor, project string) bool

type writeProject struct {
	deps  WorkDeps
	leads LeadsProject
}

var _ tools.SeatCallable = (*writeProject)(nil)

func (t *writeProject) Name() string { return tracker.WriteProjectTool }

func (t *writeProject) Description() string {
	return "Change a project's own settings. `tags` is the one part any seat " +
		"may add to — declare a label before filing work under it, because " +
		"create_work_item refuses one this project does not have. Renaming or " +
		"archiving a tag, declaring project fields and setting the default " +
		"assignee are the project lead's; archiving " +
		"the project itself takes a person. A project's name, purpose and " +
		"owning unit come from the org chart and are not writable here."
}

func (t *writeProject) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{
				"type": "string",
				"description": "The project key, e.g. ENG. Defaults to your " +
					"own unit's project.",
			},
			"tags_add": map[string]any{
				"type": "array",
				"description": "Declare these tags. Naming one that already " +
					"exists is not an error. Any seat may do this.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"slug": map[string]any{
							"type": "string",
							"description": "Lower-case letters, digits and " +
								"hyphens; what a task's `labels` holds.",
						},
						"label": map[string]any{
							"type":        "string",
							"description": "What a person reads. Defaults to the slug.",
						},
						"color":       map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
					},
					"required": []any{"slug"},
				},
			},
			"tags_rename": map[string]any{
				"type": "object",
				"description": "Slug to new LABEL. The slug never changes — " +
					"it is what every task already filed under it holds. " +
					"The project lead's.",
				"additionalProperties": map[string]any{"type": "string"},
			},
			"tags_archive": map[string]any{
				"type": "array",
				"description": "Slugs that take no new work. One-way, and the " +
					"tasks already under them keep them. The project lead's.",
				"items": map[string]any{"type": "string"},
			},
			"fields": map[string]any{
				"type": "array",
				"description": "REPLACES this project's own field " +
					"declarations — send the whole set, read describe_project " +
					"first. These sit beside the workspace's, they do not " +
					"replace them. Each declaration is whole too: a key left " +
					"out is cleared, `config` and its options included. The " +
					"project lead's.",
				"items": fieldDeclarationSchema(),
			},
			"default_assignee": map[string]any{
				"type": "string",
				"description": "Who unassigned work lands on. Empty means " +
					"triage. The project lead's.",
			},
			"archived": map[string]any{
				"type": "boolean",
				"description": "Stops the project taking new work. Takes a " +
					"person's own credential, not a seat's.",
			},
		},
	}
}

func (t *writeProject) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *writeProject) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.WriteProjectTool), nil
	}
	if t.deps.ProjectWriter == nil {
		return unconfigured(tracker.WriteProjectTool), nil
	}
	key := strings.TrimSpace(argString(args, "project"))
	if key == "" {
		key = t.deps.defaultProject(actor)
	}
	if key == "" {
		return failed("Name the `project` to change — your seat's unit owns " +
			"none, so there is no default."), nil
	}
	tagEdit, refusal := projectTagEdit(args)
	if refusal != "" {
		return failed(refusal), nil
	}
	edit, refusal := projectPolicyEdit(args)
	if refusal != "" {
		return failed(refusal), nil
	}
	// THE DEFAULT ASSIGNEE IS RESOLVED AGAINST THE CHART, because it is
	// where every unassigned task in the project lands: a typo there
	// routes a whole project's triage to somebody who does not exist, and
	// every wake to them is dropped in silence.
	//
	// THE EMPTY STRING IS NOT A HANDLE. It is a real setting here — it
	// means triage — so it is left alone rather than resolved.
	if edit.DefaultAssignee != nil && *edit.DefaultAssignee != "" {
		who, unknown := t.deps.resolveHandle(tracker.WriteProjectTool,
			"`default_assignee`", *edit.DefaultAssignee)
		if unknown != "" {
			return failed(unknown), nil
		}
		edit.DefaultAssignee = &who
	}
	if tagEdit.Empty() && edit.Empty() {
		return failed("This call changes nothing. Send `tags_add`, " +
			"`tags_rename`, `tags_archive`, `fields`, " +
			"`default_assignee` or `archived`."), nil
	}
	// THE AUTHORITY IS RESOLVED ONCE, before either write, so a call that
	// holds both facets cannot land the tag half and then be refused the
	// policy half on a different answer to the same question.
	// EITHER AUTHORITY IS ENOUGH, and the operator half is why the lookup
	// below is not the whole answer: it resolves the lead from the ORG
	// CHART by handle, and a token nobody bound is in no chart at all — so
	// for an unbound operator it answers false whatever they lead. See
	// [tracker.ProjectAuthority].
	//
	// ASKED ABOUT [Actor.Record], which is the person behind the
	// credential: a bound founder's own seat leads their projects, and
	// asked about the token's name instead the lookup matched nobody and
	// fell through to `Operator` for an authority their seat actually
	// holds — so a lead-only edit refused for a seat that leads the
	// project read as a person's override in the history.
	lead := t.leads != nil && t.leads(ctx, actor.Record(), key)
	person := actor.Kind.Person()
	writer := t.deps.ProjectWriter(actor)
	out := map[string]any{"project": key}

	// THE TAGS FIRST, because the policy half may archive the project and
	// a tag declared into an archived project is the one order that reads
	// as a mistake. Two writes, never one — two objects on two subjects.
	if !tagEdit.Empty() {
		result, err := writer.WriteTags(ctx, statelog.NewOpID(time.Now(), "tags-"+key), key,
			tagEdit, tracker.TagAuthority{Lead: lead, Operator: person})
		if err != nil {
			return failed(writeFailure(actor, tracker.WriteProjectTool, err)), nil
		}
		t.deps.settle(ctx, result.Position)
		tags := map[string]any{
			"outcome": string(result.Outcome), "position": positionOf(result.Position), "version": result.Version,
		}
		if len(result.Warnings) > 0 {
			tags["warnings"] = result.Warnings
		}
		out["tags"] = tags
	}
	if !edit.Empty() {
		result, err := writer.WriteProject(ctx, statelog.NewOpID(time.Now(), "policy-"+key), key,
			edit, tracker.ProjectAuthority{Lead: lead, Operator: person})
		if err != nil {
			return failed(writeFailure(actor, tracker.WriteProjectTool, err)), nil
		}
		t.deps.settle(ctx, result.Position)
		out["policy"] = map[string]any{
			"outcome": string(result.Outcome), "position": positionOf(result.Position), "version": result.Version,
		}
	}
	return jsonResult(out)
}

// projectTagEdit reads the three tag facets a caller sent.
func projectTagEdit(args map[string]any) (tracker.TagEdit, string) {
	var edit tracker.TagEdit
	if raw, held := args["tags_add"].([]any); held {
		for i, entry := range raw {
			spec, ok := entry.(map[string]any)
			if !ok {
				return tracker.TagEdit{}, fmt.Sprintf(
					"Tag %d of `tags_add` is not an object.", i+1)
			}
			edit.Add = append(edit.Add, tracker.Tag{
				Slug:        strings.TrimSpace(argString(spec, "slug")),
				Label:       strings.TrimSpace(argString(spec, "label")),
				Color:       strings.TrimSpace(argString(spec, "color")),
				Description: argString(spec, "description"),
			})
		}
	}
	if raw, held := args["tags_rename"].(map[string]any); held {
		edit.Rename = make(map[string]string, len(raw))
		for slug, label := range raw {
			text, ok := label.(string)
			if !ok {
				return tracker.TagEdit{}, fmt.Sprintf(
					"`tags_rename[%s]` is the tag's new LABEL, as a string.",
					clip(slug))
			}
			edit.Rename[slug] = text
		}
	}
	if _, held := args["tags_archive"]; held {
		edit.Archive = argStrings(args, "tags_archive")
	}
	return edit, ""
}

// projectPolicyEdit reads the three policy facets a caller sent.
//
// PRESENCE DECIDES, never the value: `default_assignee: ""` means triage and
// omitting it means leave it alone, and a reader that could not tell them apart
// would clear a project's default every time somebody declared a field.
func projectPolicyEdit(args map[string]any) (tracker.ProjectEdit, string) {
	var edit tracker.ProjectEdit
	if _, held := args["fields"]; held {
		fields, refusal := catalogueFields(args)
		if refusal != "" {
			return tracker.ProjectEdit{}, refusal
		}
		edit.Fields = &fields
	}
	if _, held := args["default_assignee"]; held {
		who := strings.TrimSpace(argString(args, "default_assignee"))
		edit.DefaultAssignee = &who
	}
	if _, held := args["archived"]; held {
		archived := argBool(args, "archived")
		edit.Archived = &archived
	}
	return edit, ""
}

// toAny widens a string set for a JSON Schema `enum`.
func toAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
