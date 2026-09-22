package tracker

import "strings"

// ---- the unit seam ------------------------------------------------------ //
//
// A unit reference stored here is a STRING, and the same unit answers to two
// of them: its id where the chart gave it one, and its name where it did not.
// Which one a row holds depends on when it was written, because a task's
// `filed_unit` is a record of what was true and nothing rewrites it. So every
// question about a unit — what to store, what to display, which rows to
// match, whose lead to wake — goes through one resolution, and the chart is
// the only thing that can perform it.

// Units is what a CHART can answer about a unit.
//
// Defined here and satisfied by the caller, because the tracker holds no org:
// the applier may not read one — two nodes briefly on different epochs would
// write different rows — so a unit reference is resolved at READ and at WRITE
// time by whoever has the chart, and every surface that renders or filters
// one has to reach the same answer.
//
// A NIL RESOLVER IS MEANINGFUL and is what a process with no loaded chart
// has: a stored unit then renders raw with `resolved: false`, which is
// honest, rather than as an empty unit, which is a claim the row names none;
// and a filter matches the reference literally rather than widening.
type Units interface {
	// ResolveUnit answers the unit a reference names — either of its
	// spellings, however it is cased — and false where the chart has no
	// such unit.
	ResolveUnit(ref string) (ChartUnit, bool)
}

// ChartUnit is one unit as the chart answers it.
//
// THREE ANSWERS FROM ONE RESOLUTION, because the callers ask in three
// directions and a seam that answered one of them would be resolved twice:
// a write stores the Key, a screen renders the Name, and a wake reaches the
// Lead.
type ChartUnit struct {
	// Key is the unit's durable identity — `org.Unit.Key`: its id where
	// the chart gave it one, its name where it did not. IT IS WHAT EVERY
	// WRITE HERE STORES, so that what a row holds survives the rename the
	// id exists for.
	Key string

	// Name is what a person reads: the unit's name as the chart spells it,
	// whatever spelling the reference used.
	Name string

	// Lead is who hears about this unit's work — the EFFECTIVE lead, so a
	// unit that declares none but sits under a unit that does reports the
	// seat who actually hears.
	Lead LeadRef
}

// LeadRef is who leads a unit.
type LeadRef struct {
	Handle string     `json:"handle,omitempty"`
	Kind   AuthorKind `json:"kind,omitempty"`
}

// UnitRef is a stored unit reference as a reader renders it.
//
// RESOLVED IS A FIELD rather than an absence, because "this row names a unit
// the chart no longer has" is the finding `work_projects_report` exists to
// surface, and a nil unit would be indistinguishable from a row that names
// none.
//
// KEY IS WHAT THE ROW HOLDS, not what the chart would write today: these rows
// are records, and a reader showing a spelling the row does not carry cannot
// be used to find it again. Name is the chart's, which is what makes the pair
// useful — a row filed under a name before an id was added still renders the
// team's current name.
type UnitRef struct {
	Key      string `json:"key,omitempty"`
	Name     string `json:"name,omitempty"`
	Resolved bool   `json:"resolved"`
}

// resolveUnit renders a stored unit reference against the chart.
//
// A ROW THAT NAMES NO UNIT IS RESOLVED, because naming none is a valid state
// — the finding is a row naming one the chart does not have, and conflating
// the two would report every unfiled project as orphaned.
func resolveUnit(units Units, stored string) (UnitRef, LeadRef) {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return UnitRef{Resolved: true}, LeadRef{}
	}
	if units == nil {
		return UnitRef{Key: stored}, LeadRef{}
	}
	unit, found := units.ResolveUnit(stored)
	return UnitRef{Key: stored, Name: unit.Name, Resolved: found}, unit.Lead
}

// CanonicalContainer is a container addressed the way the engine keys one.
//
// A CONTAINER IS AN ADDRESS, and every kind but one is already canonical by
// construction: a project key is upper-cased wherever it is minted — which is
// exactly this rule, spelled at each parser — and a person is a handle. A UNIT
// is the one with two spellings, so the same team addressed by its id and by
// its name was TWO strips: a view saved from one was invisible from the other,
// and neither surface could tell that from a container nobody has saved a view
// in.
//
// A REFERENCE THE CHART CANNOT RESOLVE IS LEFT ALONE, for the reason
// [unitSpellings] leaves one alone: a strip saved against a team since
// dissolved is still that strip, and the read matches both spellings anyway.
func CanonicalContainer(units Units, container Container) Container {
	if units == nil || container.Kind != ContainerUnit {
		return container
	}
	if unit, found := units.ResolveUnit(container.ID); found {
		container.ID = unit.Key
	}
	return container
}

// unitSpellings is every spelling a row may hold for the units these
// references name.
//
// THE READ HALF OF THE TWO SPELLINGS. A write stores a unit's key, and rows
// written before the chart gave that unit an id hold its name for ever — so a
// filter that compared against the one string somebody typed would answer
// with half the team's work, silently, exactly when an id is added. Each
// reference is resolved through the chart and contributes the SET its unit
// answers to.
//
// EXACT VALUES RATHER THAN A FOLDED COMPARISON. Both columns this filters
// have an index over their stored text (`tracker_tasks_filed_unit_idx`,
// `tracker_projects_unit_idx`), and a `COLLATE NOCASE` comparison stops a
// planner seeking one — so the CHART absorbs the case, resolving whatever was
// typed, and what comes back out is the unit's own two spellings.
//
// A REFERENCE THE CHART CANNOT RESOLVE KEEPS ITS LITERAL, which is the honest
// answer rather than an error or a widening: a stored unit may legitimately
// name a team the chart no longer has, and those rows are still that team's
// work. A reference naming nothing at all then matches nothing, which is what
// a filter for a team nobody has should do.
//
// A BLANK REFERENCE CONTRIBUTES NOTHING — it cannot arrive from the query
// grammar, where an empty value carries no filter at all, and an empty set
// here is what the callers check before building a clause.
func unitSpellings(units Units, refs []string) []string {
	out := make([]string, 0, len(refs)*2)
	seen := make(map[string]bool, len(refs)*2)
	add := func(value string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, value)
	}
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		unit, found := ChartUnit{}, false
		if units != nil {
			unit, found = units.ResolveUnit(ref)
		}
		if !found {
			add(ref)
			continue
		}
		add(unit.Key)
		add(unit.Name)
	}
	return out
}
