package worklet

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/words"
)

// What a declaration may say.
//
// A type, field or option resolves by id, by slug and BY NAME (a model writes
// `severity: High` after reading it off a board), so names colliding under
// [NormName] are refused like slugs: one lookup could not tell them apart,
// and `OptionIDs` would keep whichever came last.
//
// A config knob its type cannot read (`Precision` on a checkbox, `Time` on a
// number) is refused: it is stored, replicated and read by nothing, while the
// caller believes they configured something. Knobs are checked in pairs too:
// a `tracking` list on a manual progress field is read by nothing.

// NormName is the one normalisation a name is compared under, by the
// collision check and by the applier's `name_norm` column alike: two that
// differed would pass a write the index then treats as a duplicate.
func NormName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// MaxUnit is how long a number field's unit may be, in bytes: a unit is
// rendered inline beside every value ("8 h", "3 story points"), and anything
// longer is a label.
const MaxUnit = 16

// MaxOptionName bounds an option's own label.
const MaxOptionName = 64

// The closed sets a [FieldConfig] draws from, slices rather than a switch so a
// refusal can list what is allowed.
var (
	// ProgressModes are how a progress field is filled.
	ProgressModes = []string{"manual", "auto"}

	// TrackingSources are what an automatic progress field counts.
	TrackingSources = []string{"subtasks", "checklists", "asked_comments"}

	// RollupSources are the relation a rollup gathers across.
	RollupSources = []string{"waiting_on", "blocking", "linked", "children"}

	// RollupFields are the built-in columns a rollup may gather; a field
	// ID is also accepted.
	RollupFields = []string{
		"due", "start", "points", "estimate_min", "spend_tokens", "status_group",
	}

	// RollupOps are the aggregations.
	RollupOps = []string{"sum", "avg", "min", "max", "count", "earliest", "latest"}

	// DateRollupOps apply to a date source and nothing else.
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
		return invalid("field %s's unit is %d bytes and the maximum "+
			"is %d — a unit is rendered beside every value on every row, and "+
			"anything longer is a label", f.Slug, len(c.Unit), MaxUnit)
	case c.Precision != 0 && !numeric:
		return unusable(f, "a precision", "number, progress and rollup")
	case c.Precision < 0 || c.Precision > MaxPrecision:
		return invalid("field %s declares %d decimal places and the "+
			"range is 0 to %d", f.Slug, c.Precision, MaxPrecision)
	case (c.Min != nil || c.Max != nil) && !numeric:
		return unusable(f, "a minimum or maximum", "number, progress and rollup")
	case c.Min != nil && c.Max != nil && *c.Min > *c.Max:
		// Refused at the declaration rather than at every write that
		// would fail against it.
		return invalid("field %s has a minimum of %v above its "+
			"maximum of %v, so no value could satisfy it", f.Slug, *c.Min, *c.Max)
	case c.Time && f.Type != FieldDate:
		return unusable(f, "a time flag", "date")
	case c.Multi && !MultiValued(f.Type) && f.Type != FieldDropdown:
		// [MultiValued] decides which types hold several, plus the
		// dropdown, the one type whose declaration chooses.
		return unusable(f, "a multi flag", "dropdown, labels, people and relationship")
	}
	if err := checkProgress(f); err != nil {
		return err
	}
	return checkRollup(f)
}

// MaxPrecision is how many decimal places a number field may declare: what a
// float64 carries exactly for a tracker's magnitudes. Past it the declaration
// promises an exactness the storage does not have.
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
		return invalid("field %s is filled %q and the modes are: %s",
			f.Slug, clip(c.Progress), strings.Join(ProgressModes, ", "))
	}
	seen := map[string]bool{}
	for _, source := range c.Tracking {
		switch {
		case !slices.Contains(TrackingSources, source):
			return invalid("field %s tracks %q and the sources "+
				"are: %s", f.Slug, clip(source),
				strings.Join(TrackingSources, ", "))
		case seen[source]:
			return invalid("field %s tracks %q twice", f.Slug, source)
		}
		seen[source] = true
	}
	if c.Progress == "auto" {
		// This build computes no automatic progress: the field would hold
		// whatever was last typed while calling itself automatic, worse
		// than a manual one because nobody knows to keep it up to date.
		// Computing it means a per-task aggregate on every read, a
		// decision about a board's cost rather than missing code.
		return invalid("field %s is filled automatically and this "+
			"build computes no automatic progress — the value would be "+
			"whatever somebody last typed into it. Set `progress: manual` "+
			"and write the number, or leave the field out", f.Slug)
	}
	if len(c.Tracking) > 0 {
		// Only the refused `auto` mode reads a tracking list, so on a
		// manual field it is read by nothing. Checked after the mode, so
		// a field declaring both is told about the setting to change first.
		return invalid("field %s tracks %s and nothing counts "+
			"them — a tracking list is what an AUTOMATIC progress field "+
			"counts, and this build computes no automatic progress. Drop "+
			"`tracking` and write the number", f.Slug,
			strings.Join(c.Tracking, ", "))
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
	// This build gathers no rollup (a read-time aggregate over a relation),
	// so it is refused, for [checkProgress]'s reason: a field that calls itself
	// gathered and is not is worse than a plain number. The shape checks run
	// first, so an operator learns every other mistake in one validate.
	switch {
	case r.Source == "":
		return invalid("field %s is a rollup and names nothing to "+
			"gather across — the sources are %s, or a relationship field's id",
			f.Slug, strings.Join(RollupSources, ", "))
	case r.Field == "":
		return invalid("field %s is a rollup and names no field to "+
			"gather — the built-in ones are %s, or a field's id",
			f.Slug, strings.Join(RollupFields, ", "))
	case !slices.Contains(RollupOps, r.Op):
		return invalid("field %s rolls up with %q, which isn't an "+
			"aggregation. Use %s", f.Slug, clip(r.Op), words.Or(RollupOps))
	}
	// `earliest` of numbers is `min` under another word; a sum of dates is
	// microseconds nobody meant.
	dateField := r.Field == "due" || r.Field == "start"
	if slices.Contains(DateRollupOps, r.Op) && !dateField {
		return invalid("field %s takes the %s of %q, and %s applies "+
			"to dates — `due` and `start`. Over anything else it is `min` or "+
			"`max`", f.Slug, r.Op, r.Field, r.Op)
	}
	if dateField && !slices.Contains(DateRollupOps, r.Op) && r.Op != "count" {
		return invalid("field %s takes the %s of %q, which is a "+
			"date — a sum or an average of dates is a number of microseconds "+
			"nobody meant. Use %s",
			f.Slug, r.Op, r.Field, words.Or(append(slices.Clone(DateRollupOps), "count")))
	}
	// A status group is a word: the only aggregation is a count.
	if r.Field == "status_group" && r.Op != "count" {
		return invalid("field %s takes the %s of the status group, "+
			"which is a word — the only aggregation over words is `count`",
			f.Slug, r.Op)
	}
	return invalid("field %s declares a rollup and this build "+
		"gathers nothing — the value would be whatever somebody last typed "+
		"into it, under a name that says it was gathered. Drop the `rollup` "+
		"block and write the number, or leave the field out", f.Slug)
}

// unusable is the shared refusal for a knob on a type that has none.
func unusable(f *FieldDef, what, types string) error {
	return invalid("field %s is a %s and carries %s, which only a "+
		"%s field uses — a setting its type cannot read is one stored, "+
		"replicated and applied by nothing", f.Slug, f.Type, what, types)
}

// checkNames refuses two declarations whose names collide under [NormName],
// since a name is a resolution key. An archived declaration's name is free,
// as its slug is: nothing resolves against it.
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
			return invalid("%s %s and %s are both named %q, and a "+
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
