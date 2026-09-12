package tracker

import (
	"encoding/json"
	"strings"
	"testing"
)

// seats is a chart seam that resolves exactly the handles it holds.
type seats map[string]string

func (s seats) ResolveSeat(ref string) (string, bool) {
	handle, held := s[strings.ToLower(strings.TrimSpace(ref))]
	return handle, held
}

// tasks is the decide's own task lookup, as a settlement supplies it.
func tasks(byRef map[string]string) func(string) (string, bool) {
	return func(ref string) (string, bool) {
		id, held := byRef[strings.ToUpper(strings.TrimSpace(ref))]
		return id, held
	}
}

func world() fieldRefs {
	return fieldRefs{
		world: seats{"ana": "ana", "ana engineer": "ana"},
		task:  tasks(map[string]string{"ENG-7": "id-7"}),
	}
}

func num(v float64) *float64 { return &v }

// THE WHOLE COERCION TABLE, one case per rule, with no database anywhere.
//
// It is a table because the RULES are a table: the design states them as one
// paragraph of nine clauses, and a behaviour discovered from a failing board
// instead of read from here is a rule nobody re-checks. None of it existed —
// a number field took "about 7 hours" and stored 7, a checkbox took the string
// "false" and stored it set, a url took a bare hostname, and a date-only field
// took a timestamp and kept the time it does not have.
func TestTheCoercionTable(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		field FieldDef
		in    string

		// want is the canonical value, or "" when the case refuses.
		want string
		// refused names what the refusal has to say, so a caller's next
		// attempt is right rather than another guess.
		refused []string
		warns   string
	}{
		// NUMBER: refused, never rounded.
		{
			name:  "a number is a number",
			field: FieldDef{Slug: "effort", Type: FieldNumber},
			in:    `7`, want: `7`,
		}, {
			name:  "a number written as text is normalised",
			field: FieldDef{Slug: "effort", Type: FieldNumber},
			in:    `"7"`, want: `7`,
		}, {
			name:    "a number is not salvaged out of prose",
			field:   FieldDef{Slug: "effort", Type: FieldNumber},
			in:      `"about 7 hours"`,
			refused: []string{"effort", "is not one"},
		}, {
			name:    "below the minimum is refused naming it",
			field:   FieldDef{Slug: "effort", Type: FieldNumber, Config: FieldConfig{Min: num(1)}},
			in:      `0`,
			refused: []string{"minimum", "1"},
		}, {
			name:    "above the maximum is refused naming it",
			field:   FieldDef{Slug: "effort", Type: FieldNumber, Config: FieldConfig{Max: num(10)}},
			in:      `11`,
			refused: []string{"maximum", "10"},
		}, {
			name: "more decimals than the precision is REFUSED, not rounded",
			field: FieldDef{Slug: "effort", Type: FieldNumber,
				Config: FieldConfig{Precision: 2}},
			in:      `3.14159`,
			refused: []string{"2 decimal", "3.14159"},
		}, {
			name: "at the declared precision it lands",
			field: FieldDef{Slug: "effort", Type: FieldNumber,
				Config: FieldConfig{Precision: 2}},
			in: `3.14`, want: `3.14`,
		},

		// CHECKBOX: a bool only.
		{
			name:  "a checkbox takes a bool",
			field: FieldDef{Slug: "urgent", Type: FieldCheckbox},
			in:    `false`, want: `false`,
		}, {
			name:    "a checkbox refuses the string false",
			field:   FieldDef{Slug: "urgent", Type: FieldCheckbox},
			in:      `"false"`,
			refused: []string{"urgent", "true or false"},
		},

		// DATE: truncation warns, a missing time refuses.
		{
			name:  "a date-only field takes a date",
			field: FieldDef{Slug: "ship", Type: FieldDate},
			in:    `"2026-03-04"`, want: `"2026-03-04"`,
		}, {
			name:  "a timestamp on a date-only field is truncated, and says so",
			field: FieldDef{Slug: "ship", Type: FieldDate},
			in:    `"2026-03-04T09:30:00Z"`, want: `"2026-03-04"`,
			warns: "2026-03-04",
		}, {
			name:    "a time field refuses a bare date rather than inventing midnight",
			field:   FieldDef{Slug: "ship", Type: FieldDate, Config: FieldConfig{Time: true}},
			in:      `"2026-03-04"`,
			refused: []string{"carries no time", "T09:00:00Z"},
		}, {
			name:  "a time field keeps the time",
			field: FieldDef{Slug: "ship", Type: FieldDate, Config: FieldConfig{Time: true}},
			in:    `"2026-03-04T09:30:00Z"`, want: `"2026-03-04T09:30:00Z"`,
		}, {
			name:    "a date that is not one is refused",
			field:   FieldDef{Slug: "ship", Type: FieldDate},
			in:      `"next tuesday"`,
			refused: []string{"is not one", "2026-03-04"},
		},

		// OPTIONS: any spelling in, the id out.
		{
			name: "an option slug becomes the option's id",
			field: FieldDef{Slug: "sev", Type: FieldDropdown, Config: FieldConfig{
				Options: []Option{{ID: "o1", Slug: "high", Name: "High"}},
			}},
			in: `"high"`, want: `"o1"`,
		}, {
			name: "an option NAME becomes the same id",
			field: FieldDef{Slug: "sev", Type: FieldDropdown, Config: FieldConfig{
				Options: []Option{{ID: "o1", Slug: "high", Name: "High"}},
			}},
			in: `"High"`, want: `"o1"`,
		}, {
			name: "an option nobody declared is refused with the list",
			field: FieldDef{Slug: "sev", Type: FieldDropdown, Config: FieldConfig{
				Options: []Option{{ID: "o1", Slug: "high", Name: "High"}},
			}},
			in:      `"critical"`,
			refused: []string{"no option", "high"},
		},

		// URL and EMAIL.
		{
			name:    "a url needs a scheme",
			field:   FieldDef{Slug: "doc", Type: FieldURL},
			in:      `"example.com/spec"`,
			refused: []string{"no scheme", "https://"},
		}, {
			name:  "a url with one lands",
			field: FieldDef{Slug: "doc", Type: FieldURL},
			in:    `"https://example.com/spec"`, want: `"https://example.com/spec"`,
		}, {
			name:    "an email must parse",
			field:   FieldDef{Slug: "contact", Type: FieldEmail},
			in:      `"not an address"`,
			refused: []string{"contact", "is not one"},
		}, {
			name:  "an email keeps the ADDRESS and drops the display name",
			field: FieldDef{Slug: "contact", Type: FieldEmail},
			in:    `"Ana <ana@example.com>"`, want: `"ana@example.com"`,
		},

		// RELATIONSHIP and PEOPLE: resolved, or refused.
		{
			name:  "a relationship stores the task's id",
			field: FieldDef{Slug: "parent-epic", Type: FieldRelationship},
			in:    `"ENG-7"`, want: `"id-7"`,
		}, {
			name:    "a relationship to nothing is refused",
			field:   FieldDef{Slug: "parent-epic", Type: FieldRelationship},
			in:      `"ENG-999"`,
			refused: []string{"no such item"},
		}, {
			name:  "a people field resolves a name to one handle",
			field: FieldDef{Slug: "reviewer", Type: FieldPeople},
			in:    `"Ana Engineer"`, want: `"ana"`,
		}, {
			name:    "a people field refuses what names nobody",
			field:   FieldDef{Slug: "reviewer", Type: FieldPeople},
			in:      `"whoever"`,
			refused: []string{"reviewer", "not one colleague"},
		},

		// NULL clears anything, and is decided before the type is.
		{
			name:  "null clears a field of any type",
			field: FieldDef{Slug: "effort", Type: FieldNumber, Config: FieldConfig{Min: num(5)}},
			in:    `null`, want: `null`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := coerceField(c.field, json.RawMessage(c.in), world())
			if len(c.refused) > 0 {
				if err == nil {
					t.Fatalf("%s was accepted as %s", c.in, got.Value)
				}
				for _, want := range c.refused {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the refusal is %q and does not name %q — a "+
							"refusal a caller cannot act on is one they guess "+
							"against", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("%s was refused: %v", c.in, err)
			}
			if string(got.Value) != c.want {
				t.Errorf("%s became %s, want %s", c.in, got.Value, c.want)
			}
			switch {
			case c.warns == "" && len(got.Warnings) > 0:
				t.Errorf("an exact value warned: %v", got.Warnings)
			case c.warns != "" && len(got.Warnings) == 0:
				t.Error("a value the engine changed said nothing — a " +
					"truncation nobody is told about is a value silently " +
					"replaced")
			case c.warns != "" && !strings.Contains(got.Warnings[0], c.warns):
				t.Errorf("the warning is %q and does not name what was stored",
					got.Warnings[0])
			}
		})
	}
}

// A MULTI-VALUED FIELD IS EVERY MEMBER THROUGH THE TABLE, and the cap is the
// one the rows take.
func TestAMultiValuedFieldCoercesEveryMember(t *testing.T) {
	t.Parallel()
	field := FieldDef{Slug: "areas", Type: FieldLabels, Config: FieldConfig{
		Options: []Option{
			{ID: "o1", Slug: "api", Name: "API"},
			{ID: "o2", Slug: "ui", Name: "UI"},
		},
	}}
	got, err := coerceField(field, json.RawMessage(`["api","UI"]`), world())
	if err != nil {
		t.Fatalf("a list of options was refused: %v", err)
	}
	if string(got.Value) != `["o1","o2"]` {
		t.Errorf("the list became %s, want both option ids — a value stored as "+
			"the slug a person types is invisible to every filter on the field "+
			"it just set", got.Value)
	}
	// ONE BAD MEMBER REFUSES THE WHOLE LIST, because the alternative is a
	// partial set nobody asked for: the caller stated four labels and
	// would be told it worked with three.
	if _, err := coerceField(field, json.RawMessage(`["api","nope"]`), world()); err == nil {
		t.Error("a list carrying an option nobody declared was accepted")
	}
	// AND A BARE VALUE IS ONE MEMBER, which is how a multi-valued field
	// carries exactly one — the same reading fieldRows takes, and the two
	// have to agree or the write and the row disagree about what was set.
	if got, err := coerceField(field, json.RawMessage(`"api"`), world()); err != nil {
		t.Errorf("a single value on a multi field was refused: %v", err)
	} else if string(got.Value) != `"o1"` {
		t.Errorf("a single value became %s", got.Value)
	}
}

// THE KEY IS RESOLVED THROUGH THE SAME THREE TIERS as every other lookup here,
// and a field nothing declares passes through rather than failing the write.
func TestFieldKeysResolveAndForeignOnesPassThrough(t *testing.T) {
	t.Parallel()
	declared := map[string]FieldDef{
		"f1": {ID: "f1", Slug: "severity", Name: "Severity", Type: FieldText},
	}
	got, _, err := coerceFields(declared, map[string]json.RawMessage{
		"severity": json.RawMessage(`"high"`),
		"unknown":  json.RawMessage(`"whatever"`),
	}, "bug", world())
	if err != nil {
		t.Fatalf("coerceFields: %v", err)
	}
	if _, held := got["f1"]; !held {
		t.Errorf("the slug did not resolve to the field's id: %v — a value "+
			"keyed by the slug is one a rename re-points", got)
	}
	if _, held := got["unknown"]; !held {
		t.Error("a field nothing declares was dropped — the applier stores it " +
			"as a foreign value by design, and refusing it here makes a " +
			"rolling upgrade reject writes it is meant to carry")
	}
	// AN ARCHIVED FIELD IS NOT RESOLVABLE BY SLUG, because its values are
	// hidden from every index: setting one by the spelling a person
	// remembers writes a value nothing can ever see.
	declared["f1"] = FieldDef{ID: "f1", Slug: "severity", Type: FieldText, Archived: true}
	got, _, err = coerceFields(declared, map[string]json.RawMessage{
		"severity": json.RawMessage(`"high"`),
	}, "bug", world())
	if err != nil {
		t.Fatalf("coerceFields: %v", err)
	}
	if _, held := got["f1"]; held {
		t.Error("an archived field resolved by slug, so the value lands hidden")
	}
}

// TWO SPELLINGS OF ONE FIELD, DISAGREEING, IS REFUSED rather than resolved in
// some order: a map has no order to appeal to, and whichever won would
// silently discard the other.
func TestOneFieldSetTwiceUnderTwoSpellingsIsRefused(t *testing.T) {
	t.Parallel()
	declared := map[string]FieldDef{
		"f1": {ID: "f1", Slug: "severity", Name: "Severity", Type: FieldText},
	}
	_, _, err := coerceFields(declared, map[string]json.RawMessage{
		"severity": json.RawMessage(`"high"`),
		"Severity": json.RawMessage(`"low"`),
	}, "bug", world())
	if err == nil {
		t.Fatal("one field set twice with two different values was accepted")
	}
	if !strings.Contains(err.Error(), "severity") {
		t.Errorf("the refusal is %q and does not name the field", err)
	}
	// THE SAME VALUE TWICE IS NOT A CONFLICT, because nothing is
	// discarded — a caller that sent both spellings of one setting meant
	// one setting.
	if _, _, err := coerceFields(declared, map[string]json.RawMessage{
		"severity": json.RawMessage(`"high"`),
		"Severity": json.RawMessage(`"high"`),
	}, "bug", world()); err != nil {
		t.Errorf("two spellings agreeing on one value were refused: %v", err)
	}
}

// THE DECIMAL COUNT IS THE ONE A PERSON TYPED, not the binary approximation.
func TestPrecisionCountsTheDecimalsSomebodyWrote(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   float64
		want int
	}{{7, 0}, {0.1, 1}, {3.14, 2}, {2.5, 1}, {1e21, 0}} {
		if got := decimalsOf(c.in); got != c.want {
			t.Errorf("decimalsOf(%v) = %d, want %d — counting the float64's "+
				"own expansion refuses 0.1 on a one-place field", c.in, got, c.want)
		}
	}
}

// WHATEVER THE WRITE ACCEPTS, THE APPLIER STORES AS THE WRITE MEANT IT — and
// a multi-valued type decomposes on BOTH sides.
//
// Three places decide what a field value is: [MultiValued], which the query
// reads to render a grouping; [fieldRows], which the applier uses to write one
// row per member; and the table here. A rule written independently in the
// third is how it stops matching the first two, and it did so immediately —
// gating on `Config.Multi` alone refused a one-element list on a `people`
// field, which is exactly what this tree already writes and the row layer
// already accepts.
//
// # The one asymmetry, and why it is not a disagreement
//
// The write REFUSES a list on a single-valued type and the applier accepts
// one. That is the salvage contract rather than a mismatch: the applier may
// not refuse anything — a value it cannot handle would stop that task's every
// later change on every node — so it stores what arrives, and the check that a
// `number` holds one number lives at the only place that can make it. What
// must never differ is the other direction, which is what this asserts: a
// value the write said yes to has to reach the rows the write intended.
func TestTheApplierStoresWhatTheWriteAccepted(t *testing.T) {
	t.Parallel()
	options := []Option{
		{ID: "o1", Slug: "one", Name: "One"},
		{ID: "o2", Slug: "two", Name: "Two"},
	}
	for _, kind := range FieldTypes {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			field := FieldDef{ID: "f1", Slug: "f", Type: kind,
				Config: FieldConfig{Options: options, Precision: 2}}
			raw := json.RawMessage(listFixture(kind))
			got, err := coerceField(field, raw, world())
			if err != nil {
				if MultiValued(kind) {
					t.Fatalf("a multi-valued type refused a list: %v — the "+
						"query renders it as holding several and the applier "+
						"writes one row each", err)
				}
				return // The asymmetry above: refused here, salvaged there.
			}
			rows, err := fieldRows(field, got.Value)
			if err != nil {
				t.Fatalf("the applier refused the coerced value %s: %v",
					got.Value, err)
			}
			if len(rows) != 2 {
				t.Errorf("a two-member list coerced to %s and the applier "+
					"wrote %d rows — a value the write said yes to has to "+
					"reach the rows the write intended", got.Value, len(rows))
			}
		})
	}
}

// listFixture is a two-member list of whatever this type accepts.
func listFixture(kind FieldType) string {
	switch kind {
	case FieldNumber, FieldProgress, FieldRollup:
		return `[1,2]`
	case FieldCheckbox:
		return `[true,false]`
	case FieldDate:
		return `["2026-03-04","2026-03-05"]`
	case FieldRelationship:
		return `["ENG-7","ENG-7"]`
	case FieldPeople:
		return `["ana","ana"]`
	case FieldURL:
		return `["https://a.example.com","https://b.example.com"]`
	case FieldEmail:
		return `["a@example.com","b@example.com"]`
	}
	return `["one","two"]`
}

// NO ROSTER DEGRADES RATHER THAN REFUSING, which is the rule [Writer.Leads]
// already states for the other seam of this kind: a build with no chart cannot
// turn a name into a handle, and failing every write that touches a people
// field would refuse them for a reason unrelated to what the caller asked.
func TestAPeopleFieldWithoutARosterPassesThrough(t *testing.T) {
	t.Parallel()
	field := FieldDef{Slug: "reviewer", Type: FieldPeople}
	got, err := coerceField(field, json.RawMessage(`"ana"`), fieldRefs{})
	if err != nil {
		t.Fatalf("a people field was refused by a writer with no chart: %v", err)
	}
	if string(got.Value) != `"ana"` {
		t.Errorf("the value became %s, want what was sent", got.Value)
	}
	// AND WITH ONE, IT IS CHECKED — so the degradation is the absence of
	// a seam rather than the absence of a rule.
	if _, err := coerceField(field, json.RawMessage(`"whoever"`), world()); err == nil {
		t.Error("a build WITH a roster accepted a handle nobody has")
	}
}
