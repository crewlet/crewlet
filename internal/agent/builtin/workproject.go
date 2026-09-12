package builtin

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
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
		"archiving a tag, declaring project fields, setting the sprint policy " +
		"and setting the default assignee are the project lead's; archiving " +
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
					"replace them. The project lead's.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":                   map[string]any{"type": "string", "description": "Omit for a new field."},
						"slug":                 map[string]any{"type": "string"},
						"name":                 map[string]any{"type": "string"},
						"type":                 map[string]any{"type": "string", "enum": toAny(fieldTypeNames())},
						"required":             map[string]any{"type": "boolean"},
						"required_in_subtasks": map[string]any{"type": "boolean"},
						"archived": map[string]any{
							"type":        "boolean",
							"description": "One-way; values stay on their tasks.",
						},
						"pinned":           map[string]any{"type": "boolean"},
						"hide_from_agents": map[string]any{"type": "boolean"},
						"description":      map[string]any{"type": "string"},
						"options": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "object"},
						},
					},
					"required": []any{"slug", "name", "type"},
				},
			},
			"sprints": map[string]any{
				"type": "object",
				"description": "The sprint policy. Send `enabled: false` to " +
					"stop sprinting — it does not close a running sprint. " +
					"The project lead's.",
				"properties": map[string]any{
					"enabled": map[string]any{
						"type":        "boolean",
						"description": "Default true.",
					},
					"length_days":   map[string]any{"type": "integer"},
					"start_weekday": map[string]any{"type": "integer", "description": "0 is Sunday."},
					"start_minutes": map[string]any{"type": "integer", "description": "Minutes after midnight."},
					"ahead":         map[string]any{"type": "integer", "description": "Unstarted sprints kept minted."},
					"auto_start":    map[string]any{"type": "boolean"},
					"auto_roll":     map[string]any{"type": "boolean"},
					"archive_after": map[string]any{"type": "integer", "description": "Days after close; 0 never."},
					"name_format":   map[string]any{"type": "string"},
					"measure": map[string]any{
						"type": "string",
						"enum": toAny(sprintMeasureNames()),
					},
				},
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
		key = t.deps.defaultProject(actor.Handle)
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
	if tagEdit.Empty() && edit.Empty() {
		return failed("This call changes nothing. Send `tags_add`, " +
			"`tags_rename`, `tags_archive`, `fields`, `sprints`, " +
			"`default_assignee` or `archived`."), nil
	}
	// THE AUTHORITY IS RESOLVED ONCE, before either write, so a call that
	// holds both facets cannot land the tag half and then be refused the
	// policy half on a different answer to the same question.
	lead := t.leads != nil && t.leads(ctx, actor.Handle, key)
	writer := t.deps.ProjectWriter(actor)
	out := map[string]any{"project": key}

	// THE TAGS FIRST, because the policy half may archive the project and
	// a tag declared into an archived project is the one order that reads
	// as a mistake. Two writes, never one — two objects on two subjects.
	if !tagEdit.Empty() {
		result, err := writer.WriteTags(ctx, "tags-"+uuid.NewString(), key,
			tagEdit, tracker.TagAuthority{Lead: lead})
		if err != nil {
			return failed(writeFailure(tracker.WriteProjectTool, err)), nil
		}
		t.deps.settle(ctx, result.Position)
		tags := map[string]any{
			"outcome": string(result.Outcome), "version": result.Version,
		}
		if len(result.Warnings) > 0 {
			tags["warnings"] = result.Warnings
		}
		out["tags"] = tags
	}
	if !edit.Empty() {
		result, err := writer.WriteProject(ctx, "policy-"+uuid.NewString(), key,
			edit, tracker.ProjectAuthority{Lead: lead, Operator: actor.Kind == tracker.AuthorOperator})
		if err != nil {
			return failed(writeFailure(tracker.WriteProjectTool, err)), nil
		}
		t.deps.settle(ctx, result.Position)
		out["policy"] = map[string]any{
			"outcome": string(result.Outcome), "version": result.Version,
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

// projectPolicyEdit reads the four policy facets a caller sent.
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
	if raw, held := args["sprints"]; held {
		spec, ok := raw.(map[string]any)
		if !ok {
			return tracker.ProjectEdit{}, "`sprints` is the sprint policy, as an object."
		}
		policy, refusal := sprintPolicyArg(spec)
		if refusal != "" {
			return tracker.ProjectEdit{}, refusal
		}
		edit.Sprints = &tracker.SprintPolicyEdit{Policy: policy}
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

// sprintPolicyArg reads a sprint policy, or the deliberate absence of one.
func sprintPolicyArg(spec map[string]any) (*tracker.SprintPolicy, string) {
	if enabled, held := spec["enabled"]; held {
		if on, ok := enabled.(bool); ok && !on {
			// OFF IS A NIL POLICY, which is the state a project that
			// never declared sprints is already in — so turning it off
			// and never turning it on are the same row.
			return nil, ""
		}
	}
	policy := &tracker.SprintPolicy{
		LengthDays:   argInt(spec, "length_days", 0),
		StartWeekday: time.Weekday(argInt(spec, "start_weekday", 0)),
		StartMinutes: argInt(spec, "start_minutes", 0),
		Ahead:        argInt(spec, "ahead", 0),
		AutoStart:    argBool(spec, "auto_start"),
		AutoRoll:     argBool(spec, "auto_roll"),
		ArchiveAfter: argInt(spec, "archive_after", 0),
		NameFormat:   strings.TrimSpace(argString(spec, "name_format")),
		Measure:      tracker.SprintMeasure(strings.TrimSpace(argString(spec, "measure"))),
	}
	if policy.LengthDays == 0 {
		// THE DEFAULT IS APPLIED AT THE EDGE rather than left as a zero
		// the writer refuses, because a lead who sent a policy without a
		// length meant the ordinary cadence — and the refusal that zero
		// would otherwise earn teaches nothing.
		policy.LengthDays = tracker.DefaultSprintDays
	}
	return policy, ""
}

// sprintMeasureNames renders the closed set for the tool schema.
func sprintMeasureNames() []string {
	out := make([]string, 0, len(tracker.SprintMeasures))
	for _, m := range tracker.SprintMeasures {
		out = append(out, string(m))
	}
	return out
}

// toAny widens a string set for a JSON Schema `enum`.
func toAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}
