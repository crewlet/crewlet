package worklet

import "strings"

// ---- the unit seam ------------------------------------------------------ //
//
// A stored unit reference is the unit's KEY: its `id` in the company document,
// minted at write and never changed, so a team keeps one key for life. Callers
// name a unit by key or by name, so every question about one (what to store,
// display or match, whose lead to wake) goes through one resolution, which
// only the running organisation can perform.

// Units is what a CHART can answer about a unit.
//
// The caller satisfies it because the tracker holds no org: the applier may
// not read one (two nodes on different epochs would write different rows), so
// a reference resolves at read and write time by whoever has the chart.
//
// A NIL RESOLVER IS MEANINGFUL, for a process with no loaded chart: a stored
// unit renders raw with `resolved: false` rather than as an empty unit, which
// would claim the row names none, and a filter matches the reference literally.
type Units interface {
	// ResolveUnit answers the unit a reference names, by key or name in any
	// case, and false where the chart has no such unit.
	ResolveUnit(ref string) (ChartUnit, bool)
}

// ChartUnit is one unit as the chart answers it: one resolution serves all
// four callers so none resolves twice. A write stores the Key, a screen
// renders the Name, a wake reaches the Lead, and a filter or a board resolves
// what was typed to the Key.
type ChartUnit struct {
	// Key is `org.Unit.Key`: the unit's id, or its name where the document
	// gave none. EVERY WRITE HERE STORES IT, and a board column is keyed on it.
	Key string

	// Name is the chart's spelling, whatever spelling the reference used.
	Name string

	// Lead is the EFFECTIVE lead: a unit declaring none reports the seat of
	// the unit above it that actually hears.
	Lead LeadRef
}

// LeadRef is who leads a unit.
type LeadRef struct {
	Handle string     `json:"handle,omitempty"`
	Kind   AuthorKind `json:"kind,omitempty"`
}

// UnitRef is a stored unit reference as a reader renders it.
//
// Resolved is a field, not an absence: a row naming a unit the chart no longer
// has is the finding `work_projects_report` surfaces, and a nil unit would
// read as naming none. Key is what the row holds, not what the chart would
// write today, so a reader can find the row again; Name is the chart's
// current one.
type UnitRef struct {
	Key      string `json:"key,omitempty"`
	Name     string `json:"name,omitempty"`
	Resolved bool   `json:"resolved"`
}

// resolveUnit renders a stored unit reference against the chart. A row naming
// no unit is resolved: the finding is a unit the chart lacks, and counting
// none as missing would report every unfiled project as orphaned.
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
// when it names neither rather than a pair of empty refs on every answer (see
// [TaskDetail.Units]). Routing resolves apart from Filed: they diverge exactly
// when an item is re-routed, the case a screen draws both halves for.
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
// Only a unit has two spellings (a project key is upper-cased where minted, a
// person is a handle), so without this one team by key and by name would be
// two strips, a view saved in one invisible from the other. A reference the
// chart cannot resolve is left alone, as in [unitKeys]: a strip saved against
// a dissolved team is still that strip.
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
// A write stores a unit's key, so a filter resolves each reference the same
// way and compares keys: a team's id or name, in any case, narrows to its
// work. Exact values, not a folded comparison: `tracker_tasks_filed_unit_idx`
// and `tracker_projects_unit_idx` index the stored text and `COLLATE NOCASE`
// stops the planner seeking them, so the CHART absorbs the case.
//
// An unresolvable reference keeps its literal, not an error or a widening: a
// stored unit may name a team the chart no longer has, and those rows are
// still its work. A blank one contributes nothing; callers check for an empty
// set before building a clause.
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
