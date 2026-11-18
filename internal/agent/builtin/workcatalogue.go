package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The catalogue tools: what a task may BE, and what it may carry.
//
// Reading is every SEAT's: `create_work_item` refuses an undeclared type, and
// a model that cannot read the catalogue can only guess (`Bug`, `bugfix` and
// `BUG` beside `bug`). Writing is the OPERATOR's alone: the declarations are
// the company's vocabulary, and a seat adding a type to make its own create
// succeed would be editing the rules it is judged by, hiding the refusal a
// person needs to see.

// CatalogueWriter is the tracker write side these tools need.
type CatalogueWriter interface {
	WriteTypes(ctx context.Context, opID string, types []worklet.TaskType) (worklet.WriteResult, error)
	WriteFields(ctx context.Context, opID string, fields []worklet.FieldDef) (worklet.WriteResult, error)
}

// CatalogueReader is the read side.
type CatalogueReader interface {
	Catalogue(ctx context.Context, q worklet.CatalogueQuery) (worklet.CatalogueAnswer, error)
}

type getWorkCatalogue struct{ deps WorkDeps }

var _ tools.SeatCallable = (*getWorkCatalogue)(nil)

func (t *getWorkCatalogue) Name() string { return worklet.GetWorkCatalogueTool }

func (t *getWorkCatalogue) Description() string {
	return "The task types this company declares and the custom fields a task " +
		"may carry. " + worklet.CreateWorkItemTool + " refuses a type that is not here, so read " +
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

	// The identity check holds on a read too: outside a turn a seat's
	// registry has no seat, and answering would answer as nobody. The
	// operator surface supplies its own actor.
	if _, err := t.deps.actor(ctx, turn); err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(worklet.GetWorkCatalogueTool, err), nil
	}
	if t.deps.Reader == nil {
		return unconfigured(worklet.GetWorkCatalogueTool), nil
	}
	answer, err := t.deps.Reader.CarriedCatalogue(ctx, worklet.CatalogueQuery{
		Archived: argBool(args, "archived"),
		Level:    seatReadLevel,
	})
	if err != nil {
		return readFailure(ctx, worklet.GetWorkCatalogueTool, err), nil
	}
	return jsonResult(answer)
}

type writeWorkCatalogue struct{ deps WorkDeps }

var _ tools.Callable = (*writeWorkCatalogue)(nil)

func (t *writeWorkCatalogue) Name() string { return worklet.WriteWorkCatalogueTool }

func (t *writeWorkCatalogue) Description() string {
	return "Declare the company's task types or its custom fields. Each list " +
		"REPLACES the one it names, and each declaration in it is WHOLE — a " +
		"key you leave out is cleared, `config` and its options included — so " +
		"read " + worklet.GetWorkCatalogueTool + " and send back what it gave you with your " +
		"change in it (for `types`, its `declared_types`, the company's own), " +
		"never a field composed from scratch. The two lists are " +
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
		return unattributed(worklet.WriteWorkCatalogueTool, err), nil
	}
	if t.deps.CatalogueWriter == nil {
		return unconfigured(worklet.WriteWorkCatalogueTool), nil
	}
	_, hasTypes := args["types"]
	_, hasFields := args["fields"]
	if !hasTypes && !hasFields {
		return failed("Send `types`, `fields` or both — each REPLACES the list " +
			"it names, and a call that sends neither changes nothing."), nil
	}
	writer := t.deps.CatalogueWriter(actor)
	out := map[string]any{}
	// THE CALL ANSWERS FOR BOTH RECORDS at the top, and each list keeps its
	// own detail beneath it — see [callOutcome].
	var call callOutcome

	// BOTH LISTS ARE READ BEFORE EITHER IS WRITTEN, so a malformed `fields`
	// cannot fail the call after `types` has already been replaced.
	var types []worklet.TaskType
	var fields []worklet.FieldDef
	if hasTypes {
		var refusal string
		if types, refusal = catalogueTypes(args); refusal != "" {
			return failed(refusal), nil
		}
	}
	if hasFields {
		var refusal string
		if fields, refusal = catalogueFields(args); refusal != "" {
			return failed(refusal), nil
		}
	}

	// TWO WRITES, NEVER ONE: two objects on two subjects (see
	// internal/worklet/catalogue.go). A refused second write after a landed
	// first is reported as exactly that ([partlyWritten]): the first IS replaced.
	if hasTypes {
		// Derived from the call, so the call made again replaces the list
		// once ([opIDFor]).
		opID := opIDFor(actor, t.Name(), "types", "workspace", args)
		result, err := writer.WriteTypes(ctx, opID, types)
		if err != nil {
			return writeFailure(ctx, actor, worklet.WriteWorkCatalogueTool, err), nil
		}
		if result.Outcome == statelog.OutcomeUnknown {
			// THE FIELDS WAIT FOR IT: half a receipt beside half a doubt is a
			// call the reader must take apart; repeating all of it is harmless.
			next := restateNext("Read the catalogue with " + worklet.GetWorkCatalogueTool)
			if hasFields {
				next = "The fields were NOT written: the call stopped here, " +
					"before them. " + next
			}
			return unknownWrite(actor, worklet.WriteWorkCatalogueTool,
				"the types were written", opID, result.Unvouched, next), nil
		}
		t.deps.settle(ctx, result.Position)
		call.add(result.Result)
		out["types"] = map[string]any{
			"count": len(types), "outcome": string(result.Outcome), "position": positionOf(result.Position),
			"version": result.Version,
		}
	}
	if hasFields {
		opID := opIDFor(actor, t.Name(), "fields", "workspace", args)
		result, err := writer.WriteFields(ctx, opID, fields)
		if err != nil {
			failure := writeFailure(ctx, actor, worklet.WriteWorkCatalogueTool, err)
			if hasTypes {
				return partlyWritten(failure, "The types were replaced", "the fields",
					call), nil
			}
			return failure, nil
		}
		if result.Outcome == statelog.OutcomeUnknown {
			next := restateNext("Read the catalogue with " + worklet.GetWorkCatalogueTool)
			if hasTypes {
				next = "The types WERE written. " + next
			}
			return unknownWrite(actor, worklet.WriteWorkCatalogueTool,
				"the fields were written", opID, result.Unvouched, next), nil
		}
		t.deps.settle(ctx, result.Position)
		call.add(result.Result)
		out["fields"] = map[string]any{
			"count": len(fields), "outcome": string(result.Outcome), "position": positionOf(result.Position),
			"version": result.Version,
		}
	}
	call.stamp(out)
	return jsonResult(out)
}

// catalogueTypes reads the declared types a caller sent.
func catalogueTypes(args map[string]any) ([]worklet.TaskType, string) {
	raw, ok := argval.List(args["types"])
	if !ok {
		return nil, "`types` is a list of type declarations."
	}
	out := make([]worklet.TaskType, 0, len(raw))
	for i, entry := range raw {
		fields, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("Type %d is not an object.", i+1)
		}
		out = append(out, worklet.TaskType{
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
// An absent id is minted, since a NEW field has none to send; a sent id is
// kept exactly, which is what lets an edit reach the field it names (and the
// archive rule hold).
//
// The shape is the one [worklet.FieldDef] is SERVED in (`applies_to`, the
// settings under `config`), every [worklet.FieldConfig] key included, because
// the only honest edit of a declaration is the one `get_work_catalogue` just
// handed back. Which type may carry which setting is the tracker's answer
// alone (its `checkConfig`, on both write paths); a gate repeated here would
// be a third rule to drift from the other two.
func catalogueFields(args map[string]any) ([]worklet.FieldDef, string) {
	raw, ok := argval.List(args["fields"])
	if !ok {
		return nil, "`fields` is a list of field declarations."
	}
	out := make([]worklet.FieldDef, 0, len(raw))
	for i, entry := range raw {
		spec, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("Field %d is not an object.", i+1)
		}
		field := worklet.FieldDef{
			ID:                 strings.TrimSpace(argString(spec, "id")),
			Slug:               strings.TrimSpace(argString(spec, "slug")),
			Name:               strings.TrimSpace(argString(spec, "name")),
			Description:        argString(spec, "description"),
			Type:               worklet.FieldType(strings.TrimSpace(argString(spec, "type"))),
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
// An absent `config` is an empty one, like every other attribute of a
// declaration: `fields` is the post-state, not a patch, so a reader that kept
// an omitted setting would make clearing one impossible and let a field
// composed from scratch inherit its predecessor's.
func fieldConfig(spec map[string]any, slug string) (worklet.FieldConfig, string) {
	raw, sent := spec["config"]
	if !sent || raw == nil {
		return worklet.FieldConfig{}, ""
	}
	config, ok := raw.(map[string]any)
	if !ok {
		return worklet.FieldConfig{}, fmt.Sprintf("`config` on field %s is the "+
			"settings object "+worklet.GetWorkCatalogueTool+" returns for it, as "+
			`{"precision": 1, "unit": "h"}.`, clip(slug))
	}
	options, refusal := catalogueOptions(config, slug)
	if refusal != "" {
		return worklet.FieldConfig{}, refusal
	}
	out := worklet.FieldConfig{
		Options:  options,
		Unit:     strings.TrimSpace(argString(config, "unit")),
		Time:     argBool(config, "time"),
		Progress: strings.TrimSpace(argString(config, "progress")),
		Multi:    argBool(config, "multi"),
	}
	if value, held := config["precision"]; held && value != nil {
		places, readable := argIntValue(value)
		if !readable {
			return worklet.FieldConfig{}, fmt.Sprintf("`config.precision` on "+
				"field %s is a whole number of decimal places, 0 to %d.",
				clip(slug), worklet.MaxPrecision)
		}
		out.Precision = places
	}
	// The bounds are read by presence (pointers on the declaration): a `min`
	// of 0 is a real floor and an absent one is none.
	low, refusal := fieldBound(config, slug, "min")
	if refusal != "" {
		return worklet.FieldConfig{}, refusal
	}
	high, refusal := fieldBound(config, slug, "max")
	if refusal != "" {
		return worklet.FieldConfig{}, refusal
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
func catalogueOptions(config map[string]any, slug string) ([]worklet.Option, string) {
	raw, held := argval.List(config["options"])
	if !held {
		return nil, ""
	}
	out := make([]worklet.Option, 0, len(raw))
	for i, entry := range raw {
		fields, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf("Option %d of field %s is not an object.",
				i+1, clip(slug))
		}
		option := worklet.Option{
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
// take one (this and `write_project`) describe it: one function, so the two
// cannot document the same argument two ways.
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

// fieldConfigSchema is what a field's TYPE lets it be configured with: every
// key a served declaration can carry, spelled as the read spells it, so the
// object a caller edits is the one it was handed. [worklet.FieldConfig]'s
// `rollup` and `tracking` are left out: the tracker refuses both (this build
// computes neither), so no read hands one back.
func fieldConfigSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"description": "The field's own settings, exactly as " + worklet.GetWorkCatalogueTool + " " +
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
					"\"8 h\", so at most %d bytes.", worklet.MaxUnit),
			},
			"precision": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("number, progress and rollup only: "+
					"how many decimal places a value may carry, 0 to %d. "+
					"Default 0, which is whole numbers only. A value carrying "+
					"more is REFUSED naming the rule, never rounded.",
					worklet.MaxPrecision),
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
				"enum": toAny(worklet.ProgressModes),
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
	out := make([]string, 0, len(worklet.FieldTypes))
	for _, t := range worklet.FieldTypes {
		out = append(out, string(t))
	}
	return out
}
