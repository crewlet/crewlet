package worklet

import (
	"strings"
	"testing"
)

func numPtr(v float64) *float64 { return &v }

// A NAME IS A RESOLUTION KEY, and two that differ only in case are two rows
// one lookup cannot tell apart.
//
// The sharpest case is an option: [OptionIDs] maps a lowercased name to an
// id, so "High" and "high" would collapse to whichever came last in the slice.
func TestANameCollidesCaseInsensitively(t *testing.T) {
	t.Parallel()
	t.Run("types", func(t *testing.T) {
		t.Parallel()
		_, err := checkTypes([]TaskType{
			{Slug: "bug", Name: "Bug"},
			{Slug: "defect", Name: "bug "},
		})
		if err == nil {
			t.Fatal("two types named \"Bug\" and \"bug \" were both declared")
		}
		if !strings.Contains(err.Error(), "resolved") {
			t.Errorf("the refusal is %q and does not say why a name matters", err)
		}
		// AND AN ARCHIVED ONE'S NAME IS FREE, on the rule its slug
		// already follows: nothing resolves against it.
		if _, err := checkTypes([]TaskType{
			{Slug: "old", Name: "Bug", Archived: true},
			{Slug: "bug", Name: "Bug"},
		}); err != nil {
			t.Errorf("an archived type held its name against a live one: %v", err)
		}
	})
	t.Run("fields", func(t *testing.T) {
		t.Parallel()
		err := checkFields([]FieldDef{
			{ID: "f1", Slug: "sev", Name: "Severity", Type: FieldText},
			{ID: "f2", Slug: "severity", Name: "severity", Type: FieldText},
		})
		if err == nil {
			t.Fatal("two fields named \"Severity\" and \"severity\" were both declared")
		}
	})
	t.Run("options", func(t *testing.T) {
		t.Parallel()
		err := checkFields([]FieldDef{{
			ID: "f1", Slug: "sev", Name: "Severity", Type: FieldDropdown,
			Config: FieldConfig{Options: []Option{
				{ID: "o1", Slug: "high", Name: "High"},
				{ID: "o2", Slug: "urgent", Name: "high"},
			}},
		}})
		if err == nil {
			t.Fatal("two options named \"High\" and \"high\" were both declared " +
				"— OptionIDs keys on the lowercased name, so one of them was " +
				"unreachable and which one moved when the list was reordered")
		}
		if !strings.Contains(err.Error(), "sev") {
			t.Errorf("the refusal is %q and does not name the field", err)
		}
	})
}

// A KNOB ITS TYPE CANNOT READ IS A SETTING STORED, REPLICATED AND APPLIED BY
// NOTHING — and the caller believes they configured something.
func TestAConfigKnobIsRefusedOnATypeThatHasNone(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		field FieldDef
		names []string
	}{
		{
			name: "a precision on a checkbox",
			field: FieldDef{ID: "f1", Slug: "urgent", Name: "Urgent",
				Type: FieldCheckbox, Config: FieldConfig{Precision: 2}},
			names: []string{"precision", "number"},
		}, {
			name: "a unit on a date",
			field: FieldDef{ID: "f1", Slug: "ship", Name: "Ship",
				Type: FieldDate, Config: FieldConfig{Unit: "h"}},
			names: []string{"unit"},
		}, {
			name: "a time flag on a number",
			field: FieldDef{ID: "f1", Slug: "effort", Name: "Effort",
				Type: FieldNumber, Config: FieldConfig{Time: true}},
			names: []string{"time flag", "date"},
		}, {
			name: "a range on a text field",
			field: FieldDef{ID: "f1", Slug: "owner", Name: "Owner",
				Type: FieldText, Config: FieldConfig{Min: numPtr(1)}},
			names: []string{"minimum"},
		}, {
			name: "a rollup on a text field",
			field: FieldDef{ID: "f1", Slug: "owner", Name: "Owner",
				Type:   FieldText,
				Config: FieldConfig{Rollup: &Rollup{Source: "children", Field: "points", Op: "sum"}}},
			names: []string{"rollup"},
		}, {
			name: "a progress mode on a number",
			field: FieldDef{ID: "f1", Slug: "effort", Name: "Effort",
				Type: FieldNumber, Config: FieldConfig{Progress: "auto"}},
			names: []string{"progress"},
		}, {
			name: "a multi flag on a number",
			field: FieldDef{ID: "f1", Slug: "effort", Name: "Effort",
				Type: FieldNumber, Config: FieldConfig{Multi: true}},
			names: []string{"multi"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := checkFields([]FieldDef{c.field})
			if err == nil {
				t.Fatal("the declaration was accepted, so the setting is " +
					"stored and replicated and read by nothing")
			}
			for _, want := range c.names {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal is %q and does not name %q", err, want)
				}
			}
		})
	}
}

// A RANGE NO VALUE SATISFIES is a field nothing can ever be written to.
func TestAnImpossibleRangeIsRefusedAtTheDeclaration(t *testing.T) {
	t.Parallel()
	err := checkFields([]FieldDef{{
		ID: "f1", Slug: "effort", Name: "Effort", Type: FieldNumber,
		Config: FieldConfig{Min: numPtr(10), Max: numPtr(1)},
	}})
	if err == nil {
		t.Fatal("a minimum above its maximum was declared — every write to " +
			"that field would fail, and the declaration is where it shows")
	}
	if !strings.Contains(err.Error(), "no value") {
		t.Errorf("the refusal is %q and does not say what is impossible", err)
	}
	// AND A PRECISION PAST WHAT THE STORAGE HAS.
	if err := checkFields([]FieldDef{{
		ID: "f1", Slug: "effort", Name: "Effort", Type: FieldNumber,
		Config: FieldConfig{Precision: MaxPrecision + 1},
	}}); err == nil {
		t.Error("a precision past the maximum was declared, so the field " +
			"promises an exactness the storage does not have")
	}
}

// THE ROLLUP DRAWS FROM THREE CLOSED SETS, and the two pairings that make
// sense of each other.
func TestTheRollupSetsAndTheirPairings(t *testing.T) {
	t.Parallel()
	rollup := func(source, field, op string) error {
		return checkFields([]FieldDef{{
			ID: "f1", Slug: "total", Name: "Total", Type: FieldRollup,
			Config: FieldConfig{Rollup: &Rollup{Source: source, Field: field, Op: op}},
		}})
	}
	// A WELL-FORMED ROLLUP IS STILL REFUSED: this build gathers nothing
	// (checkRollup). The pairing rules run first, so an operator learns
	// every other mistake in the same pass.
	if err := rollup("children", "points", "sum"); err == nil {
		t.Fatal("a rollup was accepted and nothing computes one")
	} else if !strings.Contains(err.Error(), "gathers nothing") {
		t.Errorf("the refusal is %q and does not say why", err)
	}
	for _, c := range []struct {
		name              string
		source, field, op string
		says              string
	}{
		{"an aggregation nobody has", "children", "points", "median",
			`isn't an aggregation. Use "sum", "avg", "min", "max", "count", "earliest" or "latest"`},
		{"the earliest of a number", "children", "points", "earliest", "applies to dates"},
		{"the sum of some dates", "children", "due", "sum", `Use "earliest", "latest" or "count"`},
		{"the sum of some words", "children", "status_group", "sum", "count"},
		{"nothing to gather across", "", "points", "sum", "gather across"},
		{"nothing to gather", "children", "", "sum", "no field"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := rollup(c.source, c.field, c.op)
			if err == nil {
				t.Fatal("the rollup was accepted")
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Errorf("the refusal is %q and does not say %q", err, c.says)
			}
		})
	}
}

// AN AUTOMATIC PROGRESS FIELD IS REFUSED, because nothing fills one
// (checkProgress). The sources it would count are still a closed set, checked
// first.
func TestAnAutomaticProgressFieldIsRefusedWhileNothingFillsOne(t *testing.T) {
	t.Parallel()
	err := checkFields([]FieldDef{{
		ID: "f1", Slug: "done", Name: "Done", Type: FieldProgress,
		Config: FieldConfig{Progress: "auto", Tracking: []string{"subtasks"}},
	}})
	if err == nil {
		t.Fatal("an automatic progress field was accepted and nothing fills one")
	}
	if !strings.Contains(err.Error(), "computes no automatic progress") {
		t.Errorf("the refusal is %q and does not say why", err)
	}
	// A MANUAL ONE STILL LANDS: only the automatic setting is refused.
	if err := checkFields([]FieldDef{{
		ID: "f1", Slug: "done", Name: "Done", Type: FieldProgress,
		Config: FieldConfig{Progress: "manual"},
	}}); err != nil {
		t.Errorf("a manual progress field was refused: %v", err)
	}
	if err := checkFields([]FieldDef{{
		ID: "f1", Slug: "done", Name: "Done", Type: FieldProgress,
		Config: FieldConfig{Progress: "auto", Tracking: []string{"vibes"}},
	}}); err == nil {
		t.Error("a tracking source nobody has was declared")
	}
}

// AND A TRACKING LIST IS REFUSED ON ITS OWN TYPE: only the refused automatic
// mode reads it, so beside `progress: manual` it counts nothing.
func TestATrackingListIsRefusedWhileNothingCountsIt(t *testing.T) {
	t.Parallel()
	err := checkFields([]FieldDef{{
		ID: "f1", Slug: "done", Name: "Done", Type: FieldProgress,
		Config: FieldConfig{Progress: "manual", Tracking: []string{"subtasks"}},
	}})
	if err == nil {
		t.Fatal("a tracking list was accepted beside a manual mode, so the " +
			"sources are stored, replicated and counted by nothing")
	}
	for _, want := range []string{"done", "subtasks", "nothing counts them"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is %q and does not name %q", err, want)
		}
	}
	// A LIST WITH NO MODE AT ALL IS THE SAME FACT: the default reads it no
	// better.
	if err := checkFields([]FieldDef{{
		ID: "f1", Slug: "done", Name: "Done", Type: FieldProgress,
		Config: FieldConfig{Tracking: []string{"checklists"}},
	}}); err == nil {
		t.Error("a tracking list with no mode was accepted")
	}
	// AND THE FIELD WITHOUT ONE STILL LANDS: only the list is refused.
	if err := checkFields([]FieldDef{{
		ID: "f1", Slug: "done", Name: "Done", Type: FieldProgress,
		Config: FieldConfig{Progress: "manual"},
	}}); err != nil {
		t.Errorf("a manual progress field carrying no tracking was refused: %v", err)
	}
	// THE SOURCE NAMES ARE STILL CHECKED FIRST, so an unknown source is
	// reported rather than the list.
	err = checkFields([]FieldDef{{
		ID: "f1", Slug: "done", Name: "Done", Type: FieldProgress,
		Config: FieldConfig{Progress: "manual", Tracking: []string{"vibes"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "the sources are") {
		t.Errorf("the refusal is %v and does not name the closed set first", err)
	}
}

// THE APPLIER AND THE CHECK NORMALISE A NAME THE SAME WAY, or a collision the
// write admits is one the `name_norm` index then treats as a duplicate.
func TestOneNormalisationForEveryNameColumn(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want string }{
		{"Bug", "bug"},
		{" bug ", "bug"},
		{"BUG", "bug"},
		{"", ""},
	} {
		if got := NormName(c.in); got != c.want {
			t.Errorf("NormName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// THE NAME RULE COVERS THE BUILTINS TOO, because what resolves is the
// effective set: a catalogue adds to the shipped types, so `defect` named
// "Bug" collides with the shipped `bug`.
func TestATypeNameCollidesWithABuiltinToo(t *testing.T) {
	t.Parallel()
	if _, err := checkTypes([]TaskType{{Slug: "defect", Name: "Bug"}}); err == nil {
		t.Fatal("a declared type took the name of a builtin, so resolving " +
			"\"Bug\" picks whichever row was read first")
	}
	// AND RENAMING A BUILTIN IS STILL FREE: a declared type carrying a
	// builtin's slug replaces it rather than joining it.
	if _, err := checkTypes([]TaskType{{Slug: "bug", Name: "Defect"}}); err != nil {
		t.Errorf("renaming a builtin was refused: %v — a company's own word "+
			"for `bug` is exactly what the override is for", err)
	}
	// AND SO IS A NAME NOBODY SHIPS.
	if _, err := checkTypes([]TaskType{{Slug: "incident", Name: "Incident"}}); err != nil {
		t.Errorf("a fresh type was refused: %v", err)
	}
}
