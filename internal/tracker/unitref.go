package tracker

import "strings"

// ---- the unit seam ------------------------------------------------------ //
//
// A unit reference stored here is a STRING, and the same unit answers to
// several of them. Its KEY is an ADDRESS in the org chart: the chart mints one
// when the unit is created, and a rename moves it — while the key the unit was
// CREATED under and every key it has answered to since go on resolving to it
// (internal/chart, where a key is not an identity). And a unit keyed before
// the chart gave units keys — or built in Go with none — answers to its NAME.
// Which spelling a row holds depends on when it was written, because a task's
// `filed_unit` is a record of what was true and nothing rewrites it: work
// filed before a rename holds the key the unit had then. So every question
// about a unit — what to store, what to display, which rows to match, whose
// lead to wake — goes through one resolution, and the chart is the only thing
// that can perform it.

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
	// ResolveUnit answers the unit a reference names — any of its
	// spellings (its key, the key it was created under, a former key, its
	// name), however it is cased — and false where the chart has no such
	// unit.
	ResolveUnit(ref string) (ChartUnit, bool)

	// AllUnits is every unit the chart holds, for the one question
	// resolution cannot answer: the BOARD groups rows it has not read yet,
	// so it has to fold the spellings onto their keys inside the statement
	// — see [unitAxis] — and that needs the whole set up front.
	//
	// ON THE INTERFACE rather than a second one the grouping asserts for,
	// which is the choice that fails LOUDLY: a chart that could not
	// enumerate would silently group on the stored string, which is
	// exactly the split-column defect this removes and is invisible in the
	// answer. One method on one seam is a compile error instead.
	//
	// The order is the chart's own walk. Nothing here depends on it: the
	// arms of one CASE are mutually exclusive by construction, since a
	// unit key is unique across the company by admission rule.
	AllUnits() []ChartUnit
}

// ChartUnit is one unit as the chart answers it.
//
// FOUR ANSWERS FROM ONE RESOLUTION, because the callers ask in four
// directions and a seam that answered one of them would be resolved twice:
// a write stores the Key, a screen renders the Name, a wake reaches the Lead,
// and a filter or a board matches every spelling a row may hold.
type ChartUnit struct {
	// Key is the unit's CURRENT address — `org.Unit.Key`: its id where the
	// chart gave it one, its name where it did not. IT IS WHAT EVERY WRITE
	// HERE STORES, and what a board column is keyed on, so the rows a
	// rename leaves under a former key fold onto the one the unit answers
	// to now.
	Key string

	// Name is what a person reads: the unit's name as the chart spells it,
	// whatever spelling the reference used.
	Name string

	// OriginKey is the key the unit was CREATED under — its identity, which
	// no rename moves and the chart never issues twice — and FormerKeys the
	// keys it has answered to since, newest first (`org.Unit.OriginKey`,
	// `org.Unit.FormerKeys`). EMPTY FOR A UNIT NEVER RENAMED, whose origin
	// is its key.
	//
	// THEY ARE SPELLINGS A ROW MAY HOLD. A task filed before a rename keeps
	// the key the unit had then, and nothing rewrites a record — so a
	// `unit=` filter, a board column and a narrowing that knew only the key
	// and the name matched none of the work a renamed team did before its
	// rename, and drew it as a second column headed with the team's own
	// name. See [unitSpellings] and [unitAxis].
	OriginKey  string
	FormerKeys []string

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
// is the one with several spellings, so the same team addressed by its key, by
// a key it answered to before a rename and by its name was that many strips: a
// view saved from one was invisible from the others, and neither surface could
// tell that from a container nobody has saved a view in.
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
// THE READ HALF OF THE SPELLINGS. A write stores a unit's key as it is at that
// moment, and a record is never rewritten: rows written before a rename hold a
// key the unit answered to then, and rows written before the chart gave the
// unit a key at all hold its name — so a filter that compared against the one
// string somebody typed would answer with part of the team's work, silently,
// exactly when the team is renamed. Each reference is resolved through the
// chart and contributes the SET its unit answers to: its key, the key it was
// created under, every former key and its name.
//
// EXACT VALUES RATHER THAN A FOLDED COMPARISON. Both columns this filters
// have an index over their stored text (`tracker_tasks_filed_unit_idx`,
// `tracker_projects_unit_idx`), and a `COLLATE NOCASE` comparison stops a
// planner seeking one — so the CHART absorbs the case, resolving whatever was
// typed, and what comes back out is the unit's own spellings.
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
	out := make([]string, 0, len(refs)*3)
	seen := make(map[string]bool, len(refs)*3)
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
		for _, spelling := range unit.spellings() {
			add(spelling)
		}
	}
	return out
}

// spellings is every string a stored row may hold for this unit, its current
// key first: the key, the key it was created under, each former key newest
// first, and its name. Trimmed, since that is how every reference reaches the
// chart; blanks are left out, and duplicates for the caller to fold.
//
// ONE LIST FOR THE FILTER AND THE BOARD, so the two can never disagree about
// which rows are a team's: [unitSpellings] matches them and [unitAxis] folds
// them onto the key.
func (u ChartUnit) spellings() []string {
	out := make([]string, 0, 3+len(u.FormerKeys))
	for _, s := range append([]string{u.Key, u.OriginKey}, u.FormerKeys...) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if name := strings.TrimSpace(u.Name); name != "" {
		out = append(out, name)
	}
	return out
}
