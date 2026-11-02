package worklet

import "strings"

// ---- the unit seam ------------------------------------------------------ //
//
// A unit reference stored here is a STRING, and it is the unit's KEY — the
// unit's `id` in the company document, which a write mints into the stored
// document and nothing changes in place, so a team keeps one key for its whole
// life. A caller may NAME a unit either way — a board's own column hands back
// the key, a person or a model types the name — so every question about a
// unit — what to store, what to display, which rows to match, whose lead to
// wake — goes through one resolution, and the running organisation is the only
// thing that can perform it.

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
	// spellings (its key or its name), however it is cased — and false
	// where the chart has no such unit.
	ResolveUnit(ref string) (ChartUnit, bool)
}

// ChartUnit is one unit as the chart answers it.
//
// FOUR ANSWERS FROM ONE RESOLUTION, because the callers ask in four
// directions and a seam that answered one of them would be resolved twice:
// a write stores the Key, a screen renders the Name, a wake reaches the Lead,
// and a filter or a board resolves whatever a caller typed to the Key.
type ChartUnit struct {
	// Key is the unit's key — `org.Unit.Key`: its id where the document
	// gave it one, its name where it did not. IT IS WHAT EVERY WRITE HERE
	// STORES, and what a board column is keyed on.
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
// useful — a row keyed on a team renders the team's current name.
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

// taskUnits renders a task's two unit references against the chart, or nil
// for a task that names neither.
//
// NIL FOR NEITHER rather than a pair of empty refs: "this work belongs to no
// team" is what the document's two empty strings already say, and repeating
// it as an object on every answer would put a rendering of nothing in front
// of every reader — see [TaskDetail.Units].
//
// THE ROUTING HALF IS RESOLVED SEPARATELY even though it usually equals the
// filed one: the two diverge exactly when somebody re-routes an item, which
// is the case a screen draws both halves for.
func taskUnits(units Units, task Task) *TaskUnits {
	filed, routing := strings.TrimSpace(task.FiledUnit), strings.TrimSpace(task.RoutingUnit)
	if filed == "" && routing == "" {
		return nil
	}
	out := TaskUnits{}
	out.Filed, _ = resolveUnit(units, filed)
	out.Routing, _ = resolveUnit(units, routing)
	return &out
}

// CanonicalContainer is a container addressed the way the engine keys one.
//
// A CONTAINER IS AN ADDRESS, and every kind but one is already canonical by
// construction: a project key is upper-cased wherever it is minted — which is
// exactly this rule, spelled at each parser — and a person is a handle. A UNIT
// is the one with two spellings, so the same team addressed by its key and by
// its name was two strips: a view saved from one was invisible from the other,
// and neither surface could tell that from a container nobody has saved a view
// in.
//
// A REFERENCE THE CHART CANNOT RESOLVE IS LEFT ALONE, for the reason
// [unitKeys] leaves one alone: a strip saved against a team since dissolved
// is still that strip.
func CanonicalContainer(units Units, container Container) Container {
	if units == nil || container.Kind != ContainerUnit {
		return container
	}
	if unit, found := units.ResolveUnit(container.ID); found {
		container.ID = unit.Key
	}
	return container
}

// unitKeys is the key a row holds for each unit these references name.
//
// A WRITE STORES A UNIT'S KEY — the tools resolve whatever was typed through
// the chart, and a project's unit is the chart's own — so a filter resolves
// each reference the same way and compares the key: a team's id, its name
// and either in any case all narrow to its work.
//
// EXACT VALUES RATHER THAN A FOLDED COMPARISON. Both columns this filters
// have an index over their stored text (`tracker_tasks_filed_unit_idx`,
// `tracker_projects_unit_idx`), and a `COLLATE NOCASE` comparison stops a
// planner seeking one — so the CHART absorbs the case.
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
func unitKeys(units Units, refs []string) []string {
	out := make([]string, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		if units != nil {
			if unit, found := units.ResolveUnit(ref); found {
				ref = strings.TrimSpace(unit.Key)
			}
		}
		if ref != "" && !seen[ref] {
			seen[ref] = true
			out = append(out, ref)
		}
	}
	return out
}
