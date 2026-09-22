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
		Level:    seatReadLevel,
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
		"REPLACES the one it names, and each declaration in it is WHOLE — a " +
		"key you leave out is cleared, `config` and its options included — so " +
		"read get_work_catalogue and send back what it gave you with your " +
		"change in it, never a field composed from scratch. The two lists are " +
		"separate writes: sending only `types` leaves the fields alone, and " +
		"the reverse. The built-in types (task, bug, epic, story, spike, " +
		"chore, milestone) are always available; carry one here only to " +
		"rename it or to archive it."
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
				"items": fieldDeclarationSchema(),
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
			"count": len(types), "outcome": string(result.Outcome), "position": positionOf(result.Position),
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
			"count": len(fields), "outcome": string(result.Outcome), "position": positionOf(result.Position),
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
//
// # ONE GRAMMAR WITH THE READ
//
// The shape here is the shape [tracker.FieldDef] is SERVED in — `applies_to`,
// and the settings nested under `config` — because the only honest edit of a
// declaration is the one `get_work_catalogue` just handed back. It was not:
// the options lived at the TOP level of a tool's field spec while the answer
// carries them under `config`, so an operator who echoed the read to rename
// one field dropped `options` from every field in the list — accepted, since a
// choice field may legitimately declare none, and every stored value keyed by
// an option id then resolved to nothing.
//
// # AND EVERY SETTING, not just the one
//
// Seven of [tracker.FieldConfig]'s keys had no reader here at all, so a number
// field was integer-precision whatever anybody declared, a date could never
// hold a time, and a labels field could never take more than one — while
// [tracker.FieldDef]'s own `applies_to` could not be set either, leaving every
// field applying to every type. Each is a setting the coercion table reads on
// every write and nothing could put there: the refusal a caller was handed —
// "set a value at that precision, or widen the field's own" — named a remedy
// no surface of this engine offered.
//
// WHICH TYPE MAY CARRY WHICH SETTING IS THE TRACKER'S ANSWER, never a second
// one here: its own `checkConfig` refuses a knob a type cannot read, naming
// the knob and the types that use it, and it runs on BOTH write paths — the
// workspace catalogue and a project's own declarations. A gate repeated here
// would be the third rule the coercion table's own comment records going out
// of step with the other two.
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
			AppliesTo:          argStrings(spec, "applies_to"),
			Required:           argBool(spec, "required"),
			RequiredInSubtasks: argBool(spec, "required_in_subtasks"),
			Archived:           argBool(spec, "archived"),
			Pinned:             argBool(spec, "pinned"),
			HideFromAgents:     argBool(spec, "hide_from_agents"),
		}
		if field.ID == "" {
			field.ID = uuid.NewString()
		}
		config, refusal := fieldConfig(spec, field.Slug)
		if refusal != "" {
			return nil, refusal
		}
		field.Config = config
		out = append(out, field)
	}
	return out, ""
}

// fieldConfig reads one declaration's `config` object.
//
// AN ABSENT `config` IS AN EMPTY ONE, which is the rule every other attribute
// of a declaration already follows: `fields` REPLACES the declared set and a
// declaration in it is whole, so an omitted `required` is false and an omitted
// `precision` is zero. There is no patch shape to inherit from — the list is
// the post-state — and a reader that KEPT a setting the caller left out would
// make clearing one impossible, while letting a field composed from scratch
// silently inherit whatever its predecessor held.
func fieldConfig(spec map[string]any, slug string) (tracker.FieldConfig, string) {
	raw, sent := spec["config"]
	if !sent || raw == nil {
		return tracker.FieldConfig{}, ""
	}
	config, ok := raw.(map[string]any)
	if !ok {
		return tracker.FieldConfig{}, fmt.Sprintf("`config` on field %s is the "+
			"settings object get_work_catalogue returns for it, as "+
			`{"precision": 1, "unit": "h"}.`, clip(slug))
	}
	options, refusal := catalogueOptions(config, slug)
	if refusal != "" {
		return tracker.FieldConfig{}, refusal
	}
	out := tracker.FieldConfig{
		Options:  options,
		Unit:     strings.TrimSpace(argString(config, "unit")),
		Time:     argBool(config, "time"),
		Progress: strings.TrimSpace(argString(config, "progress")),
		Multi:    argBool(config, "multi"),
	}
	if value, held := config["precision"]; held && value != nil {
		places, readable := argIntValue(value)
		if !readable {
			return tracker.FieldConfig{}, fmt.Sprintf("`config.precision` on "+
				"field %s is a whole number of decimal places, 0 to %d.",
				clip(slug), tracker.MaxPrecision)
		}
		out.Precision = places
	}
	// THE BOUNDS ARE READ BY PRESENCE, which is why they are pointers on
	// the declaration: a `min` of 0 is a real floor and an absent one is no
	// floor at all, so a reader that could not tell them apart would put a
	// floor of zero under every number field anybody declared.
	low, refusal := fieldBound(config, slug, "min")
	if refusal != "" {
		return tracker.FieldConfig{}, refusal
	}
	high, refusal := fieldBound(config, slug, "max")
	if refusal != "" {
		return tracker.FieldConfig{}, refusal
	}
	out.Min, out.Max = low, high
	return out, ""
}

// fieldBound reads `config.min` or `config.max`.
func fieldBound(config map[string]any, slug, key string) (*float64, string) {
	raw, sent := config[key]
	if !sent || raw == nil {
		return nil, ""
	}
	value, readable := argFloatValue(raw)
	if !readable {
		edge := "largest"
		if key == "min" {
			edge = "smallest"
		}
		return nil, fmt.Sprintf("`config.%s` on field %s is a number — the %s "+
			"a value on this field may be. Leave it out for no %s at all.",
			key, clip(slug), edge, key)
	}
	return &value, ""
}

// catalogueOptions reads `config.options` — the choices of a dropdown, a
// labels or a relationship field.
func catalogueOptions(config map[string]any, slug string) ([]tracker.Option, string) {
	raw, held := config["options"].([]any)
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

// fieldDeclarationSchema is ONE custom-field declaration, as both tools that
// take one describe it.
//
// ONE FUNCTION, because there are two callers and they had already drifted:
// this tool described an option's own keys while `write_project` declared
// `options` as a list of bare objects, so the same argument was documented to
// a model twice and only one of the two said what an option is.
func fieldDeclarationSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id": map[string]any{
				"type": "string",
				"description": "Stable for the life of the field — values are " +
					"keyed by it. Omit on a NEW field and one is minted; send " +
					"the existing id to edit one.",
			},
			"slug":        map[string]any{"type": "string", "description": "Lower-case; what `f.<slug>` resolves."},
			"name":        map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"type": map[string]any{
				"type": "string",
				"enum": toAny(fieldTypeNames()),
			},
			"applies_to": map[string]any{
				"type": "array",
				"description": "The type slugs that carry this field. Empty " +
					"means every type. A task of a type it does not apply to " +
					"cannot hold a value for it, and cannot be required to.",
				"items": map[string]any{"type": "string"},
			},
			"required":             map[string]any{"type": "boolean"},
			"required_in_subtasks": map[string]any{"type": "boolean"},
			"archived": map[string]any{
				"type": "boolean",
				"description": "ONE-WAY. Its values leave the value table; " +
					"restoring means a new field with a new id.",
			},
			"pinned":           map[string]any{"type": "boolean"},
			"hide_from_agents": map[string]any{"type": "boolean"},
			"config":           fieldConfigSchema(),
		},
		"required": []any{"slug", "name", "type"},
	}
}

// fieldConfigSchema is what a field's TYPE lets it be configured with.
//
// EVERY KEY A SERVED DECLARATION CAN CARRY, spelled as the read spells it, so
// the object a caller edits is the object it was handed. The two keys of
// [tracker.FieldConfig] missing here are the two no stored declaration may
// hold either: a `rollup` block and a `tracking` list both say a value keeps
// itself up to date, and the tracker refuses both because this build computes
// neither — so a read can never hand one back, and offering them would spend a
// round on an argument whose every value is refused.
func fieldConfigSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"description": "The field's own settings, exactly as get_work_catalogue " +
			"returns them. Each key belongs to particular field TYPES and is " +
			"refused by name on any other. WHOLE, like the declaration around " +
			"it: a key left out is cleared.",
		"properties": map[string]any{
			"options": map[string]any{
				"type": "array",
				"description": "dropdown, labels and relationship only. A value " +
					"stores the option's ID, so renaming one keeps every task " +
					"that chose it.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":       map[string]any{"type": "string", "description": "Omit on a new option and one is minted."},
						"slug":     map[string]any{"type": "string"},
						"name":     map[string]any{"type": "string"},
						"color":    map[string]any{"type": "string"},
						"archived": map[string]any{"type": "boolean"},
					},
					"required": []any{"slug", "name"},
				},
			},
			"unit": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("number, progress and rollup only. "+
					"Rendered inline beside every value on every row, as "+
					"\"8 h\", so at most %d bytes.", tracker.MaxUnit),
			},
			"precision": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("number, progress and rollup only: "+
					"how many decimal places a value may carry, 0 to %d. "+
					"Default 0, which is whole numbers only. A value carrying "+
					"more is REFUSED naming the rule, never rounded.",
					tracker.MaxPrecision),
			},
			"min": map[string]any{
				"type": "number",
				"description": "number, progress and rollup only: the smallest " +
					"a value may be. Leave it out for no floor — 0 is a floor.",
			},
			"max": map[string]any{
				"type": "number",
				"description": "number, progress and rollup only: the largest a " +
					"value may be. A minimum above its maximum is refused.",
			},
			"time": map[string]any{
				"type": "boolean",
				"description": "date only. True holds a time of day as well as " +
					"a day, and a bare date is then refused rather than given " +
					"an invented midnight. False truncates a timestamp to its " +
					"date and says so in the result's warnings.",
			},
			"progress": map[string]any{
				"type": "string",
				"enum": toAny(tracker.ProgressModes),
				"description": "progress only. `manual` is a number somebody " +
					"writes; `auto` is refused, because this build computes no " +
					"automatic progress and the bar would show whatever was " +
					"last typed into it.",
			},
			"multi": map[string]any{
				"type": "boolean",
				"description": "dropdown only as a declaration — labels, people " +
					"and relationship already hold several. True lets one task " +
					"carry more than one value, each its own filterable row.",
			},
		},
	}
}

// fieldTypeNames renders the closed set for the tool schema.
func fieldTypeNames() []string {
	out := make([]string, 0, len(tracker.FieldTypes))
	for _, t := range tracker.FieldTypes {
		out = append(out, string(t))
	}
	return out
}
