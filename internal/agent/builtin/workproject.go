package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/worklet"
)

// `write_project` — the project settings a TOOL owns, and who owns each.
//
// ONE TOOL over two writers: policy and tag set are two objects on two subjects
// so a seat's tag add never contends with a lead's field edit
// (internal/worklet/project.go), but "change this project" is one thought to a
// caller. The tool does both writes and reports both.
//
// A SEAT holds it for one facet, `tags.add`: `create_work_item` refuses a label
// the project has not declared, so without it a seat could never use `labels`.
// Every other facet is gated inside, and the refusals name who can.

// ProjectWriter is the tracker write side this tool needs: both verbs in one
// interface, since the seam is what a project settings tool needs, not what one
// tracker subject accepts.
type ProjectWriter interface {
	WriteProject(ctx context.Context, opID, key string, edit worklet.ProjectEdit,
		authority worklet.ProjectAuthority) (worklet.WriteResult, error)
	WriteTags(ctx context.Context, opID, project string, edit worklet.TagEdit,
		authority worklet.TagAuthority) (worklet.WriteResult, error)

	// EnsureTags is the INLINE half of the declare rule, for the create and
	// update tools: a seat that said `labels_create_missing` declares what its
	// write is about to use in one append, before the task's own.
	EnsureTags(ctx context.Context, opID, project string, tags []string) (
		[]string, []caveat.Caveat, error)
}

type writeProject struct{ deps WorkDeps }

var _ tools.SeatCallable = (*writeProject)(nil)

func (t *writeProject) Name() string { return worklet.WriteProjectTool }

func (t *writeProject) Description() string {
	return "Change a project's own settings. `tags` is the one part any seat " +
		"may add to — declare a label before filing work under it, because " +
		worklet.CreateWorkItemTool + " refuses one this project does not have. Renaming or " +
		"archiving a tag, declaring project fields, setting the default " +
		"assignee, the target date and the stuck window are the project lead's; archiving " +
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
					"declarations — send the whole set, read " + worklet.DescribeProjectTool + " " +
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
			"target_date": map[string]any{
				"type": []string{"string", "null"},
				"description": "When the project is meant to be finished: " +
					"a date (2026-12-18), or an instant, which is stored as " +
					"the day it falls on on the company's clock. Empty or " +
					"null clears it. The project lead's.",
			},
			"archived": map[string]any{
				"type": "boolean",
				"description": "Stops the project taking new work. The " +
					"project lead's, and a person's own gesture: an agent " +
					"is refused it even where it leads the project.",
			},
			"stuck_after_hours": map[string]any{
				"type":    []string{"integer", "null"},
				"minimum": 0,
				"maximum": worklet.MaxStuckAfterHours,
				"description": fmt.Sprintf("How many hours an item in progress "+
					"here may go unchanged before it reads stuck: %d to %d. "+
					"Null or 0 restores the default of %d. The project lead's.",
					worklet.MinStuckAfterHours, worklet.MaxStuckAfterHours,
					worklet.DefaultStuckAfterHours),
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
		return unattributed(worklet.WriteProjectTool, err), nil
	}
	if t.deps.ProjectWriter == nil {
		return unconfigured(worklet.WriteProjectTool), nil
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
	// THE DEFAULT ASSIGNEE IS RESOLVED AGAINST THE CHART: a typo would route
	// a whole project's triage to nobody, every wake dropped in silence. The
	// empty string means triage and is left unresolved.
	if edit.DefaultAssignee != nil && *edit.DefaultAssignee != "" {
		who, unknown := t.deps.resolveHandle(ctx, worklet.WriteProjectTool,
			"`default_assignee`", *edit.DefaultAssignee)
		if unknown != "" {
			return failed(unknown), nil
		}
		edit.DefaultAssignee = &who
	}
	if tagEdit.Empty() && edit.Empty() {
		return failed("This call changes nothing. Send `tags_add`, " +
			"`tags_rename`, `tags_archive`, `fields`, " +
			"`default_assignee`, `target_date`, `stuck_after_hours` or " +
			"`archived`."), nil
	}
	// THE AUTHORITY IS RESOLVED ONCE, before either write, so the two halves
	// cannot be decided on different answers. And PER FACET: adding a label is
	// every colleague's (the tool's own gate), renaming or archiving one or any
	// policy edit is the project lead's, and archiving the project is that AND
	// never an agent's.
	project := authz.Object{Kind: authz.KindProject, Container: key}
	var authority worklet.ProjectAuthority
	if !edit.Empty() || len(tagEdit.Rename) > 0 || len(tagEdit.Archive) > 0 {
		if refusal := t.deps.mayWrite(ctx, authz.ActionProjectPolicy,
			project); refusal != nil {

			return *refusal, nil
		}
		authority.Policy = true
	}
	if edit.Archived != nil {
		if refusal := t.deps.mayWrite(ctx, authz.ActionProjectArchive,
			project); refusal != nil {

			return *refusal, nil
		}
		authority.Archive = true
	}
	writer := t.deps.ProjectWriter(actor)
	out := map[string]any{"project": key}
	// WHAT THE CALL TELLS ITS CALLER beside the change, from both halves.
	var caveats []caveat.Caveat
	// THE CALL ANSWERS FOR BOTH RECORDS at the top, and each facet keeps its
	// own detail beneath it — see [callOutcome].
	var call callOutcome

	// THE TAGS FIRST: the policy half may archive the project, and a tag
	// declared into an archived project reads as a mistake. Each write under an
	// operation derived from the call ([opIDFor]), its verb telling the two
	// apart, so a retry writes each record once.
	if !tagEdit.Empty() {
		opID := opIDFor(actor, t.Name(), "tags", key, args)
		result, err := writer.WriteTags(ctx, opID, key,
			tagEdit, worklet.TagAuthority{Policy: authority.Policy})
		if err != nil {
			return writeFailure(ctx, actor, worklet.WriteProjectTool, err), nil
		}
		if result.Outcome == statelog.OutcomeUnknown {
			// See [writeWorkCatalogue]: the second half waits for the
			// first, and the whole call is harmless to repeat.
			next := restateNext(fmt.Sprintf("Read %s with "+worklet.DescribeProjectTool, key))
			if !edit.Empty() {
				next = "The policy change was NOT written: the call stopped " +
					"here, before it. " + next
			}
			return unknownWrite(actor, worklet.WriteProjectTool,
				fmt.Sprintf("%s's tag change landed", key), opID,
				result.Unvouched, next), nil
		}
		t.deps.settle(ctx, result.Position)
		call.add(result.Result)
		tags := map[string]any{
			"outcome": string(result.Outcome), "position": positionOf(result.Position), "version": result.Version,
		}
		caveats = append(caveats, result.Caveats...)
		out["tags"] = tags
	}
	if !edit.Empty() {
		opID := opIDFor(actor, t.Name(), "policy", key, args)
		result, err := writer.WriteProject(ctx, opID, key,
			edit, authority)
		if err != nil {
			failure := writeFailure(ctx, actor, worklet.WriteProjectTool, err)
			if !tagEdit.Empty() {
				return partlyWritten(failure, "The tag change was written",
					"the project settings", call), nil
			}
			return failure, nil
		}
		if result.Outcome == statelog.OutcomeUnknown {
			next := restateNext(fmt.Sprintf("Read %s with "+worklet.DescribeProjectTool, key))
			if !tagEdit.Empty() {
				next = "The tag change WAS written. " + next
			}
			return unknownWrite(actor, worklet.WriteProjectTool,
				fmt.Sprintf("%s's policy change landed", key), opID,
				result.Unvouched, next), nil
		}
		t.deps.settle(ctx, result.Position)
		call.add(result.Result)
		policy := map[string]any{
			"outcome": string(result.Outcome), "position": positionOf(result.Position), "version": result.Version,
		}
		// THE TARGET DATE'S TRUNCATION IS SAID: an instant stored as a day is
		// the caller's value changed, and `applied` alone reads as verbatim.
		caveats = append(caveats, result.Caveats...)
		out["policy"] = policy
	}
	call.stamp(out)
	// ONE LIST AT THE TOP, whichever half raised it ([caveated]): a list
	// nested under a half is one the act route cannot take off a receipt.
	return caveated(out, caveats)
}

// projectTagEdit reads the three tag facets a caller sent.
func projectTagEdit(args map[string]any) (worklet.TagEdit, string) {
	var edit worklet.TagEdit
	if raw, held := argval.List(args["tags_add"]); held {
		for i, entry := range raw {
			spec, ok := entry.(map[string]any)
			if !ok {
				return worklet.TagEdit{}, fmt.Sprintf(
					"Tag %d of `tags_add` is not an object.", i+1)
			}
			edit.Add = append(edit.Add, worklet.Tag{
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
			text, ok := argval.String(label)
			if !ok || label == nil {
				return worklet.TagEdit{}, fmt.Sprintf(
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
// omitting it means leave it alone, or declaring a field would clear the default.
func projectPolicyEdit(args map[string]any) (worklet.ProjectEdit, string) {
	var edit worklet.ProjectEdit
	if _, held := args["fields"]; held {
		fields, refusal := catalogueFields(args)
		if refusal != "" {
			return worklet.ProjectEdit{}, refusal
		}
		edit.Fields = &fields
	}
	if _, held := args["default_assignee"]; held {
		who := strings.TrimSpace(argString(args, "default_assignee"))
		edit.DefaultAssignee = &who
	}
	// NULL AND THE EMPTY STRING BOTH CLEAR the target ("no target" is a
	// setting); the tracker coerces anything else, or refuses it.
	if raw, held := args["target_date"]; held {
		target := ""
		if raw != nil {
			text, ok := argval.String(raw)
			if !ok {
				return worklet.ProjectEdit{}, "`target_date` is a date as a " +
					"string, 2026-12-18 — or null to clear it."
			}
			target = strings.TrimSpace(text)
		}
		edit.TargetDate = &target
	}
	if _, held := args["archived"]; held {
		archived := argBool(args, "archived")
		edit.Archived = &archived
	}
	// NULL AND 0 BOTH CLEAR the window back to the default; the tracker
	// refuses a whole number outside its bounds.
	if raw, held := args["stuck_after_hours"]; held {
		hours := 0
		if raw != nil {
			whole, ok := argIntValue(raw)
			if !ok {
				return worklet.ProjectEdit{}, fmt.Sprintf("`stuck_after_hours` is a "+
					"whole number of hours, %d to %d, or null for the default of %d.",
					worklet.MinStuckAfterHours, worklet.MaxStuckAfterHours,
					worklet.DefaultStuckAfterHours)
			}
			hours = whole
		}
		edit.StuckAfterHours = &hours
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
