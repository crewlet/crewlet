package builtin_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// What a model declares is what the tracker judges values against: every
// setting the coercion table reads off a [worklet.FieldDef] must reach the
// declaration, or the remedy a refusal names is unreachable.
//
// These cases are only the mapping: what a declaration does once made is the
// tracker's suite (internal/worklet/coerce_test.go), and a copy here would
// certify nothing but itself. Each case names the value the declaration buys.
func TestEveryFieldSettingReachesTheDeclaration(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		config map[string]any
		want   func(*testing.T, worklet.FieldConfig)
	}{
		{
			// worklet: at precision 1, `0.6` lands and `0.65` is
			// refused naming the rule.
			name:   "precision",
			config: map[string]any{"precision": json.Number("1")},
			want: func(t *testing.T, got worklet.FieldConfig) {
				if got.Precision != 1 {
					t.Errorf("precision = %d, want 1 — at 0 the field refuses "+
						"0.6 and the caller has no way to widen it",
						got.Precision)
				}
			},
		}, {
			// worklet: a value below Min or above Max is refused
			// naming the bound.
			name: "a range",
			config: map[string]any{
				"min": json.Number("1"), "max": json.Number("10"),
			},
			want: func(t *testing.T, got worklet.FieldConfig) {
				if got.Min == nil || *got.Min != 1 {
					t.Errorf("min = %v, want 1", got.Min)
				}
				if got.Max == nil || *got.Max != 10 {
					t.Errorf("max = %v, want 10", got.Max)
				}
			},
		}, {
			// A FLOOR OF ZERO IS A FLOOR: the bounds are pointers so
			// "no minimum" and "not below zero" stay two declarations.
			name:   "a floor of zero",
			config: map[string]any{"min": json.Number("0")},
			want: func(t *testing.T, got worklet.FieldConfig) {
				if got.Min == nil {
					t.Fatal("a minimum of 0 was read as no minimum, so a field " +
						"that refuses negatives cannot be declared")
				}
				if *got.Min != 0 {
					t.Errorf("min = %v, want 0", *got.Min)
				}
			},
		}, {
			// worklet: with Time set a bare date is refused rather
			// than given an invented midnight, and an instant keeps
			// its time instead of being truncated to the day.
			name:   "the time flag",
			config: map[string]any{"time": true},
			want: func(t *testing.T, got worklet.FieldConfig) {
				if !got.Time {
					t.Error("time = false, so a date field can never hold an " +
						"instant and every timestamp is truncated")
				}
			},
		}, {
			// worklet: Multi lets a dropdown hold more than one option.
			name:   "the multi flag",
			config: map[string]any{"multi": true},
			want: func(t *testing.T, got worklet.FieldConfig) {
				if !got.Multi {
					t.Error("multi = false, so this field can only ever hold one")
				}
			},
		}, {
			name:   "the unit",
			config: map[string]any{"unit": " h "},
			want: func(t *testing.T, got worklet.FieldConfig) {
				if got.Unit != "h" {
					t.Errorf("unit = %q, want %q trimmed", got.Unit, "h")
				}
			},
		}, {
			name:   "the progress mode",
			config: map[string]any{"progress": "manual"},
			want: func(t *testing.T, got worklet.FieldConfig) {
				if got.Progress != "manual" {
					t.Errorf("progress = %q, want manual", got.Progress)
				}
			},
		}, {
			// The options sit under `config`, where the read carries
			// them (TestADeclarationRoundTripsFromTheRead).
			name: "the options",
			config: map[string]any{"options": []any{
				map[string]any{"slug": "high", "name": "High"},
			}},
			want: func(t *testing.T, got worklet.FieldConfig) {
				if len(got.Options) != 1 || got.Options[0].Slug != "high" {
					t.Fatalf("options = %+v, want the one declared", got.Options)
				}
				if got.Options[0].ID == "" {
					t.Error("the option was declared with no id, so a rename " +
						"would lose every task that chose it")
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			got := callCatalogue(t, trk, map[string]any{
				"fields": []any{map[string]any{
					"slug": "effort", "name": "Effort", "type": "number",
					"config": c.config,
				}},
			})
			if got.Failed {
				t.Fatalf("write_work_catalogue failed: %q", got.Output)
			}
			if len(trk.declaredFields) != 1 || len(trk.declaredFields[0]) != 1 {
				t.Fatalf("the declaration did not reach the writer: %+v",
					trk.declaredFields)
			}
			c.want(t, trk.declaredFields[0][0].Config)
		})
	}
}

// `applies_to` SCOPES A FIELD TO ITS TYPES: dropped, every field applies to
// every type and a `severity` is required of a chore.
func TestAppliesToReachesTheDeclaration(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	got := callCatalogue(t, trk, map[string]any{
		"fields": []any{map[string]any{
			"slug": "severity", "name": "Severity", "type": "dropdown",
			"applies_to": []any{"bug", "incident"},
		}},
	})
	if got.Failed {
		t.Fatalf("write_work_catalogue failed: %q", got.Output)
	}
	if want := []string{"bug", "incident"}; !equalStrings(
		trk.declaredFields[0][0].AppliesTo, want) {

		t.Fatalf("applies_to = %v, want %v — without it the field is on every "+
			"type the company declares", trk.declaredFields[0][0].AppliesTo, want)
	}
}

// THE READ IS THE EDIT: a declaration handed back by `get_work_catalogue` is
// one this tool accepts unchanged. Otherwise echoing the read to rename one
// field silently drops every choice field's options (a choice field may
// declare none), and every stored value keyed by an option id resolves to
// nothing.
func TestADeclarationRoundTripsFromTheRead(t *testing.T) {
	t.Parallel()
	served := worklet.FieldDef{
		ID: "f1", Slug: "severity", Name: "Severity", Type: worklet.FieldDropdown,
		AppliesTo: []string{"bug"},
		Config: worklet.FieldConfig{
			Multi: true,
			Options: []worklet.Option{
				{ID: "o1", Slug: "high", Name: "High"},
				{ID: "o2", Slug: "low", Name: "Low"},
			},
		},
	}
	encoded, err := json.Marshal([]worklet.FieldDef{served})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var handedBack []any
	if err := json.Unmarshal(encoded, &handedBack); err != nil {
		t.Fatalf("decode: %v", err)
	}

	trk := newFakeTracker()
	if got := callCatalogue(t, trk, map[string]any{
		"fields": handedBack,
	}); got.Failed {
		t.Fatalf("the answer's own shape was refused as an argument: %q", got.Output)
	}
	back := trk.declaredFields[0][0]
	if back.ID != served.ID || back.Slug != served.Slug || back.Type != served.Type {
		t.Fatalf("the declaration came back as %+v, want %+v", back, served)
	}
	if len(back.Config.Options) != 2 {
		t.Fatalf("options = %+v — echoing the read dropped the field's "+
			"choices, and every value keyed by one resolves to nothing",
			back.Config.Options)
	}
	if back.Config.Options[0].ID != "o1" || back.Config.Options[1].ID != "o2" {
		t.Errorf("the option ids were re-minted: %+v", back.Config.Options)
	}
	if !back.Config.Multi || !equalStrings(back.AppliesTo, served.AppliesTo) {
		t.Errorf("multi = %v and applies_to = %v, want the served declaration",
			back.Config.Multi, back.AppliesTo)
	}
}

// A DECLARATION IS WHOLE, as the list around it is: an omitted `config`
// CLEARS the field's settings, as an omitted `required` is false; keeping
// them would make clearing a setting impossible. The tool's description says so.
func TestAnOmittedConfigClearsTheFieldsSettings(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	got := callCatalogue(t, trk, map[string]any{
		"fields": []any{map[string]any{
			"id": "f1", "slug": "effort", "name": "Effort", "type": "number",
		}},
	})
	if got.Failed {
		t.Fatalf("write_work_catalogue failed: %q", got.Output)
	}
	if got := trk.declaredFields[0][0].Config; got.Precision != 0 ||
		got.Min != nil || got.Max != nil || got.Time || got.Multi ||
		got.Unit != "" || got.Progress != "" || len(got.Options) != 0 {

		t.Fatalf("config = %+v, want the empty one — a declaration that kept "+
			"what it left out could never clear a setting", got)
	}
	// AND THE TOOL SAYS SO: a rule a model cannot read it breaks.
	if described := catalogueTool(t, trk).Description(); !strings.Contains(
		described, "cleared") {

		t.Errorf("the description does not say an omitted key is cleared: %q",
			described)
	}
}

// A SETTING ITS TYPE CANNOT READ IS REFUSED BY THE TRACKER, reached only
// because the tool forwards the key; a second gate here would drift from it.
// This asserts the forwarding: internal/worklet/config_test.go owns the
// refusal, and stays green even if this tool swallowed the key.
func TestASettingReachesTheGateThatRefusesIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		fieldType string
		config    map[string]any
		carries   func(worklet.FieldConfig) bool
	}{
		{
			name: "a precision on a text field", fieldType: "text",
			config:  map[string]any{"precision": json.Number("2")},
			carries: func(c worklet.FieldConfig) bool { return c.Precision == 2 },
		}, {
			name: "options on a number field", fieldType: "number",
			config: map[string]any{"options": []any{
				map[string]any{"slug": "high", "name": "High"},
			}},
			carries: func(c worklet.FieldConfig) bool { return len(c.Options) == 1 },
		}, {
			name: "a time flag on a number field", fieldType: "number",
			config:  map[string]any{"time": true},
			carries: func(c worklet.FieldConfig) bool { return c.Time },
		}, {
			name: "a range on a text field", fieldType: "text",
			config:  map[string]any{"min": json.Number("1")},
			carries: func(c worklet.FieldConfig) bool { return c.Min != nil },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			got := callCatalogue(t, trk, map[string]any{
				"fields": []any{map[string]any{
					"slug": "thing", "name": "Thing", "type": c.fieldType,
					"config": c.config,
				}},
			})
			if got.Failed {
				t.Fatalf("write_work_catalogue failed: %q", got.Output)
			}
			if !c.carries(trk.declaredFields[0][0].Config) {
				t.Fatalf("the setting never reached the declaration (%+v), so "+
					"the gate that refuses it on this type can never fire",
					trk.declaredFields[0][0].Config)
			}
		})
	}
}

// AND THE TRACKER'S REFUSAL REACHES THE MODEL, rather than a tool-shaped
// sentence that says less than the gate did.
func TestTheDeclarationRefusalReachesTheCaller(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.writeErr = errTrackerRefusedTheField
	got := callCatalogue(t, trk, map[string]any{
		"fields": []any{map[string]any{
			"slug": "owner", "name": "Owner", "type": "text",
			"config": map[string]any{"precision": json.Number("2")},
		}},
	})
	if !got.Failed {
		t.Fatal("a refused declaration was reported as written")
	}
	if !strings.Contains(got.Output, "precision") ||
		!strings.Contains(got.Output, "number, progress and rollup") {

		t.Fatalf("the tracker's refusal did not reach the caller: %q", got.Output)
	}
}

// A MALFORMED SETTING IS REFUSED BY NAME, never read as its zero: an
// unreadable `precision` read as 0 is a valid precision, declared silently.
func TestAnUnreadableSettingIsRefusedByName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		config any
		names  []string
	}{
		{"a precision that is not a number",
			map[string]any{"precision": "two"}, []string{"config.precision", "effort"}},
		{"a fractional precision",
			map[string]any{"precision": json.Number("1.5")}, []string{"config.precision"}},
		{"a minimum that is not a number",
			map[string]any{"min": "one"}, []string{"config.min", "effort"}},
		{"a maximum that is not a number",
			map[string]any{"max": "ten"}, []string{"config.max"}},
		{"a config that is not an object", "precision 1", []string{"config", "effort"}},
		{"an option that is not an object",
			map[string]any{"options": []any{"high"}}, []string{"Option 1", "effort"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			got := callCatalogue(t, trk, map[string]any{
				"fields": []any{map[string]any{
					"slug": "effort", "name": "Effort", "type": "number",
					"config": c.config,
				}},
			})
			if !got.Failed {
				t.Fatalf("an unreadable setting was accepted: %+v",
					trk.declaredFields)
			}
			for _, want := range c.names {
				if !strings.Contains(got.Output, want) {
					t.Errorf("the refusal is %q and does not name %q", got.Output, want)
				}
			}
			if len(trk.declaredFields) != 0 {
				t.Error("a refused declaration still reached the writer")
			}
		})
	}
}

// ONE GRAMMAR, TWO TOOLS: `write_project`'s declarations go through the same
// reader, so its settings arrive the same way.
func TestAProjectDeclaresFieldsInTheSameGrammar(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistry(t, trk, chartLeads)
	got := callWork(t, reg, worklet.WriteProjectTool, map[string]any{
		"project": "ENG",
		"fields": []any{map[string]any{
			"slug": "effort", "name": "Effort", "type": "number",
			"applies_to": []any{"bug"},
			"config": map[string]any{
				"precision": json.Number("1"), "min": json.Number("0"),
				"max": json.Number("40"), "unit": "h",
			},
		}},
	})
	if got.Failed {
		t.Fatalf("write_project failed: %q", got.Output)
	}
	if len(trk.projectEdits) != 1 || trk.projectEdits[0].Fields == nil {
		t.Fatalf("the declaration did not reach the writer: %+v", trk.projectEdits)
	}
	field := (*trk.projectEdits[0].Fields)[0]
	if field.Config.Precision != 1 || field.Config.Unit != "h" {
		t.Errorf("config = %+v, want the precision and unit as declared",
			field.Config)
	}
	if field.Config.Min == nil || *field.Config.Min != 0 ||
		field.Config.Max == nil || *field.Config.Max != 40 {

		t.Errorf("the range did not reach the declaration: %+v", field.Config)
	}
	if !equalStrings(field.AppliesTo, []string{"bug"}) {
		t.Errorf("applies_to = %v, want [bug]", field.AppliesTo)
	}
}

// THE SCHEMA NAMES EVERY SETTING A MODEL MAY SEND, the same on both tools: a
// key the schema omits is one no model sends.
func TestTheDeclarationSchemaNamesEverySetting(t *testing.T) {
	t.Parallel()
	catalogue := fieldItems(t, catalogueTool(t, newFakeTracker()).Parameters(),
		worklet.WriteWorkCatalogueTool, "fields")
	project := fieldItems(t, seatTool(t,
		projectRegistry(t, newFakeTracker(), chartLeads),
		worklet.WriteProjectTool).Parameters(), worklet.WriteProjectTool, "fields")

	for _, key := range []string{
		"id", "slug", "name", "description", "type", "applies_to", "required",
		"required_in_subtasks", "archived", "pinned", "hide_from_agents",
		"config",
	} {
		if _, held := properties(t, catalogue)[key]; !held {
			t.Errorf("the field declaration schema does not offer %q", key)
		}
	}
	config, held := properties(t, catalogue)["config"].(map[string]any)
	if !held {
		t.Fatal("`config` is not an object in the schema")
	}
	// Every key a served declaration can carry; not `tracking` or `rollup`,
	// which the tracker refuses outright, so no read hands one back.
	for _, key := range []string{
		"options", "unit", "precision", "min", "max", "time", "progress", "multi",
	} {
		if _, offered := properties(t, config)[key]; !offered {
			t.Errorf("the config schema does not offer %q, so no model will "+
				"send it however completely the reader maps it", key)
		}
	}
	if !sameJSON(t, catalogue, project) {
		t.Error("write_work_catalogue and write_project describe a field " +
			"declaration differently — the reader is one function, so the " +
			"schema has to be one too")
	}
}

// ---- helpers ------------------------------------------------------------ //

// errTrackerRefusedTheField quotes internal/worklet/config.go's `unusable`:
// this package asserts the message reaches a model, not what it says.
var errTrackerRefusedTheField = &refusal{
	"worklet: field owner is a text and carries a precision, which only a " +
		"number, progress and rollup field uses — a setting its type cannot " +
		"read is one stored, replicated and applied by nothing",
}

// refusal is a tracker refusal as the tracker writes one: its sentence,
// marked [worklet.ErrInvalid]. Unmarked, it would be the node's failure.
type refusal struct{ text string }

func (r *refusal) Error() string { return r.text }

func (r *refusal) Unwrap() error { return worklet.ErrInvalid }

// catalogueTool is `write_work_catalogue` as an operator holds it, its only
// registry: a seat declaring fields would edit the rules it is judged by.
// Built from the real constructor, so a tool that stops registering fails here.
func catalogueTool(t *testing.T, trk *fakeTracker) tools.Callable {
	t.Helper()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as,
			CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return trk },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: worklet.AuthorOperator}, nil
			},
		},
		Authorize: builtin.Decide(chartLeads),
	}) {
		if tool.Name() == worklet.WriteWorkCatalogueTool {
			return tool
		}
	}
	t.Fatal("write_work_catalogue is on no surface at all")
	return nil
}

func callCatalogue(t *testing.T, trk *fakeTracker, args map[string]any) tools.Result {
	t.Helper()
	got, err := catalogueTool(t, trk).Call(everyGrant(), args)
	if err != nil {
		t.Fatalf("%s: %v", worklet.WriteWorkCatalogueTool, err)
	}
	return got
}

func seatTool(t *testing.T, reg *tools.Registry, name string) tools.Callable {
	t.Helper()
	entry, held := reg.Lookup(name)
	if !held {
		t.Fatalf("%s is not registered", name)
	}
	return entry.Tool
}

// fieldItems is one tool's `fields` item schema, as a model receives it.
func fieldItems(t *testing.T, schema map[string]any, tool, arg string) map[string]any {
	t.Helper()
	field, held := properties(t, schema)[arg].(map[string]any)
	if !held {
		t.Fatalf("%s has no `%s` argument", tool, arg)
	}
	items, held := field["items"].(map[string]any)
	if !held {
		t.Fatalf("%s declares no item shape for `%s`", tool, arg)
	}
	return items
}

func properties(t *testing.T, schema map[string]any) map[string]any {
	t.Helper()
	out, held := schema["properties"].(map[string]any)
	if !held {
		t.Fatalf("schema %v has no properties", schema)
	}
	return out
}

func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(left) == string(right)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
