package tracker

import (
	"fmt"
	"slices"
	"strings"
)

// What a DECLARATION may say, and why a name is checked as carefully as a slug.
//
// # The name is a resolution key, not a label
//
// A type, a field and an option are each resolvable three ways — by id, by
// slug, and BY NAME — which is what lets a model write `severity: High` after
// reading "High" off a board. So two declarations whose names differ only in
// case are two rows one lookup cannot tell apart, and the resolution picks
// whichever the index reached first: the same collision the slug rule exists
// to prevent, through the door nobody closed.
//
// The schema has been carrying the column for it all along — `name_norm`, with
// an index annotated "the case-insensitive collision rule" — and the rule was
// never written. So "Bug" and "bug " both landed, and `OptionIDs` mapped both
// to one key, silently keeping whichever option came last in the slice: a
// value written by name resolved differently after somebody reordered the
// list.
//
// # And a config knob on a type that has none is a caller who believes they
// configured something
//
// `Precision` on a checkbox, `Time` on a number, a `Rollup` on a text field:
// each is a setting stored, replicated and read by nothing. The options check
// already refuses its own version of this — "options belong to the three types
// that have them" — and the rest of the struct was unchecked, so every other
// knob was accepted anywhere.

// NormName is the one normalisation a name is compared under.
//
// ONE FUNCTION, because there were three inline copies — the applier writes
// `name_norm` for types, for fields and for options — and the check added
// beside them would have been a fourth. A collision rule that normalises one
// way and a column that normalises another is a rule that passes a write the
// index then treats as a duplicate.
func NormName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// MaxUnit is how long a number field's unit may be.
//
// 16 BYTES, because a unit is rendered INLINE beside every value on every row
// — "8 h", "3 story points" — and anything longer is a label rather than a
// unit. It is the design's own figure.
const MaxUnit = 16

// MaxOptionName bounds an option's own label.
const MaxOptionName = 64

// The closed sets a [FieldConfig] draws from. Each is a named slice rather
// than a switch, so a refusal can LIST what was allowed — a refusal naming the
// set is one a caller's next attempt satisfies, and one that does not is a
// guess.
var (
	// ProgressModes are how a progress field is filled.
	ProgressModes = []string{"manual", "auto"}

	// TrackingSources are what an automatic progress field counts.
	TrackingSources = []string{"subtasks", "checklists", "asked_comments"}

	// RollupSources are the relation a rollup gathers across.
	RollupSources = []string{"waiting_on", "blocking", "linked", "children"}

	// RollupFields are the built-in columns a rollup may gather. A field
	// ID is also accepted, which is why this is not the whole set.
	RollupFields = []string{
		"due", "start", "points", "estimate_min", "spend_tokens", "status_group",
	}

	// RollupOps are the aggregations.
	RollupOps = []string{"sum", "avg", "min", "max", "count", "earliest", "latest"}

	// DateRollupOps apply to a DATE source and to nothing else: the
	// earliest of a set of numbers is its minimum, under a word that says
	// something else.
	DateRollupOps = []string{"earliest", "latest"}
)

// checkConfig refuses a configuration its own type cannot use.
func checkConfig(f *FieldDef) error {
	c := &f.Config
	numeric := f.Type == FieldNumber || f.Type == FieldProgress || f.Type == FieldRollup
	switch {
	case c.Unit != "" && !numeric:
		return unusable(f, "a unit", "number, progress and rollup")
	case len(c.Unit) > MaxUnit:
		return fmt.Errorf("tracker: field %s's unit is %d bytes and the maximum "+
			"is %d — a unit is rendered beside every value on every row, and "+
			"anything longer is a label", f.Slug, len(c.Unit), MaxUnit)
	case c.Precision != 0 && !numeric:
		return unusable(f, "a precision", "number, progress and rollup")
	case c.Precision < 0 || c.Precision > MaxPrecision:
		return fmt.Errorf("tracker: field %s declares %d decimal places and the "+
			"range is 0 to %d", f.Slug, c.Precision, MaxPrecision)
	case (c.Min != nil || c.Max != nil) && !numeric:
		return unusable(f, "a minimum or maximum", "number, progress and rollup")
	case c.Min != nil && c.Max != nil && *c.Min > *c.Max:
		// A RANGE NO VALUE SATISFIES is a field nothing can ever be
		// written to, refused at the declaration rather than at every
		// write that fails against it.
		return fmt.Errorf("tracker: field %s has a minimum of %v above its "+
			"maximum of %v, so no value could satisfy it", f.Slug, *c.Min, *c.Max)
	case c.Time && f.Type != FieldDate:
		return unusable(f, "a time flag", "date")
	case c.Multi && !MultiValued(f.Type) && f.Type != FieldDropdown:
		// WHICH TYPES HOLD SEVERAL IS [MultiValued]'s ANSWER, plus the
		// one type whose declaration genuinely chooses: a dropdown is
		// single-valued unless it says otherwise, and `labels` is the
		// same field type that always is.
		return unusable(f, "a multi flag", "dropdown, labels, people and relationship")
	}
	if err := checkProgress(f); err != nil {
		return err
	}
	return checkRollup(f)
}

// MaxPrecision is how many decimal places a number field may declare.
//
// SIX, which is the design's own figure and is what a float64 carries exactly
// for the magnitudes a tracker holds. Past it the declaration promises an
// exactness the storage does not have, and the coercion table would refuse
// values for a precision nothing could represent.
const MaxPrecision = 6

// checkProgress is the two knobs only a progress field has.
func checkProgress(f *FieldDef) error {
	c := &f.Config
	if c.Progress == "" && len(c.Tracking) == 0 {
		return nil
	}
	if f.Type != FieldProgress {
		return unusable(f, "a progress mode or tracking list", "progress")
	}
	if c.Progress != "" && !slices.Contains(ProgressModes, c.Progress) {
		return fmt.Errorf("tracker: field %s is filled %q and the modes are: %s",
			f.Slug, clip(c.Progress), strings.Join(ProgressModes, ", "))
	}
	seen := map[string]bool{}
	for _, source := range c.Tracking {
		switch {
		case !slices.Contains(TrackingSources, source):
			return fmt.Errorf("tracker: field %s tracks %q and the sources "+
				"are: %s", f.Slug, clip(source),
				strings.Join(TrackingSources, ", "))
		case seen[source]:
			return fmt.Errorf("tracker: field %s tracks %q twice", f.Slug, source)
		}
		seen[source] = true
	}
	if c.Progress == "auto" {
		// AND THIS BUILD DOES NOT COMPUTE ONE, which is the same
		// objection the refusal below this used to make about tracking
		// nothing: an automatic field reads whatever was last written
		// into it and calls itself automatic, which is worse than a
		// manual field because nobody knows to keep it up to date.
		//
		// Refused rather than accepted and left empty, on the rule the
		// tracking check already stated: a bar that never moves reads as
		// work that never started. The sources it would count —
		// subtasks, checklists, asked comments — are rows a READ would
		// have to aggregate per task, which is a decision about a
		// board's cost rather than a line of missing code.
		return fmt.Errorf("tracker: field %s is filled automatically and this "+
			"build computes no automatic progress — the value would be "+
			"whatever somebody last typed into it. Set `progress: manual` "+
			"and write the number, or leave the field out", f.Slug)
	}
	return nil
}

// checkRollup is the three closed sets a rollup draws from, and the two rules
// that pair them.
func checkRollup(f *FieldDef) error {
	r := f.Config.Rollup
	if r == nil {
		return nil
	}
	if f.Type != FieldRollup {
		return unusable(f, "a rollup", "rollup")
	}
	// AND THIS BUILD GATHERS NOTHING. A rollup is a correlated aggregate
	// over a relation — read-time, per row, over a set the row does not
	// contain — and none of that exists here: the declaration was
	// validated, stored, replicated and snapshotted, and the value stayed
	// whatever somebody typed. Refused for [checkProgress]'s reason: a
	// field that calls itself gathered and is not is worse than a plain
	// number, because nobody knows to maintain it.
	//
	// The checks below run FIRST, so an operator removing the block is
	// told about its other mistakes at the same time rather than one per
	// validate.
	switch {
	case r.Source == "":
		return fmt.Errorf("tracker: field %s is a rollup and names nothing to "+
			"gather across — the sources are %s, or a relationship field's id",
			f.Slug, strings.Join(RollupSources, ", "))
	case r.Field == "":
		return fmt.Errorf("tracker: field %s is a rollup and names no field to "+
			"gather — the built-in ones are %s, or a field's id",
			f.Slug, strings.Join(RollupFields, ", "))
	case !slices.Contains(RollupOps, r.Op):
		return fmt.Errorf("tracker: field %s rolls up with %q and the "+
			"aggregations are: %s", f.Slug, clip(r.Op),
			strings.Join(RollupOps, ", "))
	}
	// A DATE AGGREGATION ON A NUMBER is the earliest of a set of numbers
	// under a word that means something else — and the reverse, a sum of
	// dates, is a number of microseconds nobody meant.
	dateField := r.Field == "due" || r.Field == "start"
	if slices.Contains(DateRollupOps, r.Op) && !dateField {
		return fmt.Errorf("tracker: field %s takes the %s of %q, and %s applies "+
			"to dates — `due` and `start`. Over anything else it is `min` or "+
			"`max`", f.Slug, r.Op, r.Field, r.Op)
	}
	if dateField && !slices.Contains(DateRollupOps, r.Op) && r.Op != "count" {
		return fmt.Errorf("tracker: field %s takes the %s of %q, which is a "+
			"date — a sum or an average of dates is a number of microseconds "+
			"nobody meant. Use %s, or `count`",
			f.Slug, r.Op, r.Field, strings.Join(DateRollupOps, " or "))
	}
	// A STATUS GROUP IS A WORD, so the only thing to do with a set of them
	// is count them.
	if r.Field == "status_group" && r.Op != "count" {
		return fmt.Errorf("tracker: field %s takes the %s of the status group, "+
			"which is a word — the only aggregation over words is `count`",
			f.Slug, r.Op)
	}
	return fmt.Errorf("tracker: field %s declares a rollup and this build "+
		"gathers nothing — the value would be whatever somebody last typed "+
		"into it, under a name that says it was gathered. Drop the `rollup` "+
		"block and write the number, or leave the field out", f.Slug)
}

// unusable is the shared refusal for a knob on a type that has none.
func unusable(f *FieldDef, what, types string) error {
	return fmt.Errorf("tracker: field %s is a %s and carries %s, which only a "+
		"%s field uses — a setting its type cannot read is one stored, "+
		"replicated and applied by nothing", f.Slug, f.Type, what, types)
}

// checkNames refuses two declarations whose names collide case-insensitively.
//
// THE NAME IS A RESOLUTION KEY. A model writes what it read off a board, and
// two rows one lookup cannot tell apart resolve to whichever the index reached
// first — which is not even stable, because it moves when somebody reorders
// the list.
//
// AN ARCHIVED DECLARATION'S NAME IS FREE, on the same rule its slug already
// follows: nothing resolves against it, so the word is available for whatever
// replaces it.
func checkNames(kind string, names []namedDeclaration) error {
	seen := make(map[string]string, len(names))
	for _, one := range names {
		if one.archived {
			continue
		}
		norm := NormName(one.name)
		if norm == "" {
			continue
		}
		if first, clash := seen[norm]; clash {
			return fmt.Errorf("tracker: %s %s and %s are both named %q, and a "+
				"name is how one is resolved — two that differ only in case or "+
				"spacing resolve to whichever row was read first",
				kind, first, one.ident, one.name)
		}
		seen[norm] = one.ident
	}
	return nil
}

// namedDeclaration is one thing a name has to be unique among.
type namedDeclaration struct {
	ident    string
	name     string
	archived bool
}
