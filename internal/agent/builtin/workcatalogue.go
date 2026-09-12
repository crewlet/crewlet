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

// The catalogue tools: what a task may BE, and what it may carry.
//
// # Reading is a SEAT's, writing is not
//
// `get_work_catalogue` is in every seat's registry, because `create_work_item`
// refuses a type the company has not declared and a model that cannot read the
// catalogue can only guess — which is precisely how `Bug`, `bugfix` and `BUG`
// came to sit beside `bug` in companies whose engines did not check. Telling
// the model what exists is cheaper than refusing it repeatedly.
//
// Writing is the OPERATOR's alone. A declaration is the company's vocabulary:
// a seat adding a type to make its own create succeed is a seat editing the
// rules it is being judged by, and the refusal it was working around is the
// signal a person needs to see.

// CatalogueWriter is the tracker write side these tools need.
type CatalogueWriter interface {
	WriteTypes(ctx context.Context, opID string, types []tracker.TaskType) (tracker.WriteResult, error)
	WriteFields(ctx context.Context, opID string, fields []tracker.FieldDef) (tracker.WriteResult, error)
}

// CatalogueReader is the read side.
type CatalogueReader interface {
	Catalogue(ctx context.Context, q tracker.CatalogueQuery) (tracker.CatalogueAnswer, error)
}

type getWorkCatalogue struct{ deps WorkDeps }

var _ tools.SeatCallable = (*getWorkCatalogue)(nil)

func (t *getWorkCatalogue) Name() string { return tracker.GetWorkCatalogueTool }

func (t *getWorkCatalogue) Description() string {
	return "The task types this company declares and the custom fields a task " +
		"may carry. create_work_item refuses a type that is not here, so read " +
		"this before inventing one."
}

func (t *getWorkCatalogue) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"archived": map[string]any{
				"type": "boolean",
				"description": "True also lists retired types and fields. " +
					"Default false — no new work is filed under either.",
			},
		},
	}
}

func (t *getWorkCatalogue) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *getWorkCatalogue) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	// THE IDENTITY CHECK IS ON A READ TOO, exactly as it is on
	// list_work_items beside it: outside a turn a seat's registry has no
	// seat, and a tool that answered anyway would be answering as nobody.
	// The operator surface supplies its own actor, so this succeeds there.
	if _, err := t.deps.actor(ctx, turn); err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.GetWorkCatalogueTool), nil
	}
	if t.deps.Reader == nil {
		return unconfigured(tracker.GetWorkCatalogueTool), nil
	}
	answer, err := t.deps.Reader.Catalogue(ctx, tracker.CatalogueQuery{
		Archived: argBool(args, "archived"),
		Level:    statelog.ReadSession,
	})
	if err != nil {
		return failed(readFailure(tracker.GetWorkCatalogueTool, err)), nil
	}
	return jsonResult(answer)
}

type writeWorkCatalogue struct{ deps WorkDeps }

var _ tools.Callable = (*writeWorkCatalogue)(nil)

func (t *writeWorkCatalogue) Name() string { return tracker.WriteWorkCatalogueTool }

func (t *writeWorkCatalogue) Description() string {
	return "Declare the company's task types or its custom fields. Each list " +
		"REPLACES the one it names, so send the whole set — read " +
		"get_work_catalogue first. The two are separate writes: sending only " +
		"`types` leaves the fields alone, and the reverse. The built-in types " +
		"(task, bug, epic, story, spike, chore, milestone) are always " +
		"available; carry one here only to rename it or to archive it."
}

func (t *writeWorkCatalogue) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"types": map[string]any{
				"type":        "array",
				"description": "Replaces the declared task types.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"slug":        map[string]any{"type": "string", "description": "Lower-case; what a task's `type` holds."},
						"name":        map[string]any{"type": "string"},
						"plural":      map[string]any{"type": "string"},
						"icon":        map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
						"archived": map[string]any{
							"type": "boolean",
							"description": "True takes no new work and leaves " +
								"the tasks already filed under it alone.",
						},
					},
					"required": []string{"slug", "name"},
				},
			},
			"fields": map[string]any{
				"type": "array",
				"description": "Replaces the workspace's custom-field declarations. " +
					"A project declares its own beside these.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id": map[string]any{
							"type": "string",
							"description": "Stable for the life of the field — " +
								"values are keyed by it. Omit on a NEW field and " +
								"one is minted; send the existing id to edit one.",
						},
						"slug":        map[string]any{"type": "string", "description": "Lower-case; what `f.<slug>` resolves."},
						"name":        map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
						"type": map[string]any{
							"type": "string",
							"enum": fieldTypeNames(),
						},
						"required":             map[string]any{"type": "boolean"},
						"required_in_subtasks": map[string]any{"type": "boolean"},
						"archived": map[string]any{
							"type": "boolean",
							"description": "ONE-WAY. Its values leave the value " +
								"table; restoring means a new field with a new id.",
						},
						"pinned":           map[string]any{"type": "boolean"},
						"hide_from_agents": map[string]any{"type": "boolean"},
						"options": map[string]any{
							"type":        "array",
							"description": "Only for dropdown, labels and relationship.",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"id":       map[string]any{"type": "string", "description": "Omit on a new option and one is minted."},
									"slug":     map[string]any{"type": "string"},
									"name":     map[string]any{"type": "string"},
									"color":    map[string]any{"type": "string"},
									"archived": map[string]any{"type": "boolean"},
								},
								"required": []string{"slug", "name"},
							},
						},
					},
					"required": []string{"slug", "name", "type"},
				},
			},
		},
	}
}

func (t *writeWorkCatalogue) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *writeWorkCatalogue) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.WriteWorkCatalogueTool), nil
	}
	if t.deps.CatalogueWriter == nil {
		return unconfigured(tracker.WriteWorkCatalogueTool), nil
	}
	_, hasTypes := args["types"]
	_, hasFields := args["fields"]
	if !hasTypes && !hasFields {
		return failed("Send `types`, `fields` or both — each REPLACES the list " +
			"it names, and a call that sends neither changes nothing."), nil
	}
	writer := t.deps.CatalogueWriter(actor)
	out := map[string]any{}

	// TWO WRITES, NEVER ONE, because they are two objects on two subjects
	// — see internal/tracker/catalogue.go. A call sending both does them
	// in order, and the second still runs if the first was refused only
	// in the sense that it does NOT: an operator who sent both meant both,
	// and reporting one applied and one refused is the honest shape.
	if hasTypes {
		types, refusal := catalogueTypes(args)
		if refusal != "" {
			return failed(refusal), nil
		}
		result, err := writer.WriteTypes(ctx, "types-"+uuid.NewString(), types)
		if err != nil {
			return failed(writeFailure(tracker.WriteWorkCatalogueTool, err)), nil
		}
		t.deps.settle(ctx, result.Position)
		out["types"] = map[string]any{
			"count": len(types), "outcome": string(result.Outcome),
			"version": result.Version,
		}
	}
	if hasFields {
		fields, refusal := catalogueFields(args)
		if refusal != "" {
			return failed(refusal), nil
		}
		result, err := writer.WriteFields(ctx, "fields-"+uuid.NewString(), fields)
		if err != nil {
			return failed(writeFailure(tracker.WriteWorkCatalogueTool, err)), nil
		}
		t.deps.settle(ctx, result.Position)
		out["fields"] = map[string]any{
			"count": len(fields), "outcome": string(result.Outcome),
			"version": result.Version,
		}
	}
	return jsonResult(out)
}

// catalogueTypes reads the declared types a caller sent.
func catalogueTypes(args map[string]any) ([]tracker.TaskType, string) {
	raw, ok := args["types"].([]any)
	if !ok {
		return nil, "`types` is a list of type declarations."
	}
	out := make([]tracker.TaskType, 0, len(raw))
	for i, entry := range raw {
		fields, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("Type %d is not an object.", i+1)
		}
		out = append(out, tracker.TaskType{
			Slug:        strings.TrimSpace(argString(fields, "slug")),
			Name:        strings.TrimSpace(argString(fields, "name")),
			Plural:      strings.TrimSpace(argString(fields, "plural")),
			Icon:        strings.TrimSpace(argString(fields, "icon")),
			Description: argString(fields, "description"),
			Archived:    argBool(fields, "archived"),
		})
	}
	return out, ""
}

// catalogueFields reads the declared fields a caller sent.
//
// AN ABSENT ID IS MINTED, because a caller declaring a NEW field has no id to
// send and one it invented would be a uuid it has to keep. An id it DOES send
// is kept exactly, which is what lets an edit reach the field it names — and
// what makes the archive rule enforceable at all.
func catalogueFields(args map[string]any) ([]tracker.FieldDef, string) {
	raw, ok := args["fields"].([]any)
	if !ok {
		return nil, "`fields` is a list of field declarations."
	}
	out := make([]tracker.FieldDef, 0, len(raw))
	for i, entry := range raw {
		spec, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("Field %d is not an object.", i+1)
		}
		field := tracker.FieldDef{
			ID:                 strings.TrimSpace(argString(spec, "id")),
			Slug:               strings.TrimSpace(argString(spec, "slug")),
			Name:               strings.TrimSpace(argString(spec, "name")),
			Description:        argString(spec, "description"),
			Type:               tracker.FieldType(strings.TrimSpace(argString(spec, "type"))),
			Required:           argBool(spec, "required"),
			RequiredInSubtasks: argBool(spec, "required_in_subtasks"),
			Archived:           argBool(spec, "archived"),
			Pinned:             argBool(spec, "pinned"),
			HideFromAgents:     argBool(spec, "hide_from_agents"),
		}
		if field.ID == "" {
			field.ID = uuid.NewString()
		}
		options, refusal := catalogueOptions(spec, field.Slug)
		if refusal != "" {
			return nil, refusal
		}
		field.Config.Options = options
		out = append(out, field)
	}
	return out, ""
}

func catalogueOptions(spec map[string]any, slug string) ([]tracker.Option, string) {
	raw, held := spec["options"].([]any)
	if !held {
		return nil, ""
	}
	out := make([]tracker.Option, 0, len(raw))
	for i, entry := range raw {
		fields, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("Option %d of field %s is not an object.",
				i+1, clip(slug))
		}
		option := tracker.Option{
			ID:       strings.TrimSpace(argString(fields, "id")),
			Slug:     strings.TrimSpace(argString(fields, "slug")),
			Name:     strings.TrimSpace(argString(fields, "name")),
			Color:    strings.TrimSpace(argString(fields, "color")),
			Order:    i,
			Archived: argBool(fields, "archived"),
		}
		if option.ID == "" {
			option.ID = uuid.NewString()
		}
		out = append(out, option)
	}
	return out, ""
}

// fieldTypeNames renders the closed set for the tool schema.
func fieldTypeNames() []string {
	out := make([]string, 0, len(tracker.FieldTypes))
	for _, t := range tracker.FieldTypes {
		out = append(out, string(t))
	}
	return out
}
