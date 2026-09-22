package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Reading a company's projects — the listing, and one project in full.
//
// # The counts are READ, never aggregated
//
// `task_counts` is `tracker_projects.open_count/done_count/closed_count`,
// maintained by the task apply whenever a task's status group changes or it
// enters, leaves or is removed from a project. The alternative — an aggregate
// over every task in every project — ran on every sixty-second dashboard poll
// and was O(all tasks) for a number that changes on a handful of commits an
// hour.
//
// `last_change` is the same bargain on the same row: the newest task commit's
// instant and actor, written by the apply that writes the project's history
// row ([Applier.stampProjectChange]) rather than sought per project per poll
// in a table nothing ever sweeps. It is ABSENT rather than zero for a project
// no work has been filed into — see [LastChange].
//
// # Both of these are SET reads
//
// A project's listing and its description are dominated by aggregates over
// TASK rows — the counts here, and `by_assignee` in the description. A set
// read cannot
// enumerate the subjects that would have ENTERED its answer, so neither can
// claim completeness while a deferred record's scope could intersect the
// project: both carry `complete` with its `incomplete` beside the read level,
// because coverage and freshness are two facts.
//
// Their closure is the project's CONTAINER (plus both catalogues for the
// description, which resolves field declarations), because enumerating a
// project's tasks is not tractable and a narrower term would certify an answer
// complete that a deferred task record was holding.

// UnitRef is a project's chart-owned unit as a reader renders it.
//
// RESOLVED IS A FIELD rather than an absence, because "this project names a
// unit the chart no longer has" is the finding `work_projects_report` exists to
// surface, and a nil unit would be indistinguishable from a project that names
// none.
type UnitRef struct {
	Key      string `json:"key,omitempty"`
	Name     string `json:"name,omitempty"`
	Resolved bool   `json:"resolved"`
}

// LeadRef is who leads the unit a project belongs to.
type LeadRef struct {
	Handle string     `json:"handle,omitempty"`
	Kind   AuthorKind `json:"kind,omitempty"`
}

// Units is what a CHART can answer about a project's owning unit.
//
// Defined here and satisfied by the caller, because the tracker holds no org:
// the chart-owned `Unit` is a string on the project record and only the epoch's
// organization can say whether it still names anything — which is why the
// applier may not read one and this resolution happens at read time.
//
// A NIL RESOLVER IS MEANINGFUL and is what a process with no loaded chart has:
// every row then renders its raw unit with `resolved: false`, which is honest,
// rather than an empty unit, which is a claim the project names none.
type Units interface {
	// ResolveUnit answers the unit's display name and whether the chart
	// still has it, plus its effective lead.
	ResolveUnit(name string) (display string, lead LeadRef, found bool)
}

// TaskCounts is a project's maintained task census.
type TaskCounts struct {
	Open   int `json:"open"`
	Done   int `json:"done"`
	Closed int `json:"closed"`
}

// LastChange is when a project's work last changed and who changed it.
//
// MAINTAINED beside the counts and on the same terms — see
// [Applier.stampProjectChange], which is the authority on which commits move
// it. The instant is the AUTHORED one, which is what every other "when did
// this last change" on these rows carries and what the activity feed displays
// for the same commit.
//
// A POINTER, because a project nobody has filed work into has no answer and
// the honest report of that is an ABSENCE. A zero instant here would render as
// the year 1 on every empty project, and an instant defaulted to the project's
// own creation would report a directory of untouched projects as freshly
// active — both are a made-up value where the truth is "nothing yet".
type LastChange struct {
	At time.Time `json:"at"`

	// Actor is the handle the commit was made under, and ActorKind which
	// of the four kinds that handle belongs to. Both are the commit's
	// own, so `ana` the person and `ana` the seat are distinguishable —
	// a directory draws them differently, and an `operator` write is a
	// token acting for the company rather than anybody's seat.
	//
	// Either may be empty: a record can carry no actor at all, and a
	// change that happened with nobody named is still a change. The
	// instant is what says the answer exists.
	Actor     string     `json:"actor,omitempty"`
	ActorKind AuthorKind `json:"actor_kind,omitempty"`
}

// ProjectRow is one project as a listing renders it.
type ProjectRow struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Purpose string `json:"purpose,omitempty"`

	Unit UnitRef `json:"unit"`
	Lead LeadRef `json:"lead"`

	DefaultAssignee string     `json:"default_assignee,omitempty"`
	Counts          TaskCounts `json:"task_counts"`

	// LastChange is nil for a project no work has ever been filed into —
	// see [LastChange].
	LastChange *LastChange `json:"last_change,omitempty"`

	Archived bool   `json:"archived,omitempty"`
	Version  uint64 `json:"version"`
}

// ProjectListing is the answer.
type ProjectListing struct {
	Projects []ProjectRow `json:"projects"`

	// Total is how many projects match the filter, and Truncated says the
	// listing stopped short of it. A count rather than a cursor because a
	// company's projects are tens, not thousands — [MaxProjectsPerAnswer]
	// carries the rationale.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated,omitempty"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// MaxProjectsPerAnswer is how many projects one listing carries.
//
// TWO HUNDRED, which is what a SCREEN takes: a company with more projects than
// that has a chart problem rather than a paging problem, and the answer says
// `truncated` and `total` rather than offering a cursor nobody would page —
// every screen that draws projects draws all of them.
//
// IT IS NOT THE SEAT TOOL'S CEILING, although it said it was. Measured, an
// ordinary row — an eight-character key, a twenty-character name, a
// forty-character purpose, a resolved unit, a lead and a default assignee —
// encodes at ≈ 610 bytes as a tool answer renders it (indented), so two
// hundred of them is ≈ 93 KiB against a 64 KiB ceiling and the call is REFUSED
// for weight somewhere around a hundred and forty projects. That is what
// [MaxProjectsPerToolAnswer] is for.
const MaxProjectsPerAnswer = 200

// MaxProjectsPerToolAnswer is how many a SEAT's listing carries.
//
// FIFTY, and the figure is measured rather than round. A tool answer is read
// by a model out of the same context window as everything else in the turn,
// and `builtin.ToolAnswerBytes` REFUSES one past 64 KiB rather than cutting it
// — so a cap that does not fit is not a large answer, it is a call that
// returns nothing but advice to narrow. At ≈ 610 bytes a row these fifty
// encode at ≈ 30 KiB, which leaves room for rows twice as long as the ones
// measured (a company writes prose into a unit's purpose and nothing caps it)
// before the tripwire is anywhere near.
//
// It is a PAGE rather than a refusal, on [MaxCatalogueOptions]'s own
// reasoning: the answer carries `total` beside `truncated`, so a seat is told
// what it is not seeing and has `q` and `unit` to narrow with. Fifty projects
// is already more of a company than one answer can usefully teach a model.
//
// `TestEveryToolAnswerFitsToolAnswerBytes` is what re-measures this when the
// row grows a field.
const MaxProjectsPerToolAnswer = 50

// ProjectQuery asks for a company's projects.
type ProjectQuery struct {
	// Q narrows by a case-insensitive substring of the key, the name or
	// the purpose — what a person types into a filter box.
	Q string

	// Unit narrows to the projects one chart unit owns.
	Unit string

	// Archived includes the archived ones; absent excludes them.
	Archived bool

	// Limit bounds the rows, clamped to [MaxProjectsPerAnswer].
	Limit int

	// Units resolves the chart-owned unit. Nil renders every row
	// unresolved — see [Units].
	Units Units

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration

	// MaxLagSeq is the same bound counted in RECORDS, which is what
	// the broker actually answers — the duration above is derived
	// from it through this node's own drain rate. Both may be set
	// and the read refuses past whichever is reached first.
	MaxLagSeq uint64
}

// Projects answers the company's projects with their maintained counts.
func (r *Reader) Projects(ctx context.Context, q ProjectQuery) (
	ProjectListing, error) {

	if q.Level == "" {
		return ProjectListing{}, fmt.Errorf("tracker: this project read " +
			"names no level — a surface resolves an absent read_level to its " +
			"own default before it reads")
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxProjectsPerAnswer {
		limit = MaxProjectsPerAnswer
	}

	var listing ProjectListing
	served, err := r.log.Read(ctx, statelog.Query{
		Level:       q.Level,
		Scope:       projectListScope(),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		MaxLagSeq:   q.MaxLagSeq,
		Set:         true,
	}, func(tx *sql.Tx) error {
		rows, total, err := readProjectRows(ctx, tx, q, limit)
		if err != nil {
			return err
		}
		listing.Projects, listing.Total = rows, total
		listing.Truncated = total > len(rows)
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		listing.LogSeq, listing.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return ProjectListing{}, err
	}
	listing.Level = served.Level
	listing.Complete = served.Complete
	listing.LogLag = served.Lag
	if served.Incomplete != nil {
		listing.Incomplete = incompleteFrom(served.Incomplete)
	}
	return listing, nil
}

// projectListScope is the DOMAIN, and honestly so.
//
// A listing spans every container, so no container term covers it and the
// honest closure is the one every deferred record intersects. That is the
// right answer rather than a pessimistic one: a listing IS about the whole
// company, and a narrower term would certify it complete while a deferred
// record in some project held the row that moves a count.
func projectListScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised()
}

// readProjectRows reads the projects a query names.
func readProjectRows(ctx context.Context, tx *sql.Tx, q ProjectQuery,
	limit int) ([]ProjectRow, int, error) {

	where := []string{}
	var args []any
	if !q.Archived {
		where = append(where, "p.archived = 0")
	}
	if unit := strings.TrimSpace(q.Unit); unit != "" {
		where = append(where, "p.unit = ?")
		args = append(args, unit)
	}
	if term := strings.TrimSpace(q.Q); term != "" {
		// THE KEY, THE NAME AND THE PURPOSE, because a filter box is
		// typed into by somebody who remembers one of the three. LIKE
		// with the term escaped, which is what [likeEscape] is for.
		where = append(where, `(p.key LIKE ? ESCAPE '\' OR p.name LIKE ? `+
			`ESCAPE '\' OR p.purpose LIKE ? ESCAPE '\')`)
		pattern := "%" + likeEscape(term) + "%"
		args = append(args, pattern, pattern, pattern)
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + joinAnd(where)
	}

	var total int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tracker_projects p`+clause, args...).
		Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("tracker: count the projects: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT p.key, p.name, p.purpose, p.unit, p.default_assignee,
		       p.open_count, p.done_count,
		       p.closed_count, p.last_change_at, p.last_change_actor,
		       p.last_change_actor_kind, p.archived, p.version
		FROM tracker_projects p`+clause+`
		ORDER BY p.key
		LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, 0, fmt.Errorf("tracker: read the projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProjectRow
	for rows.Next() {
		var row ProjectRow
		var unit string
		var archived int
		var change lastChangeColumns
		if err := rows.Scan(&row.Key, &row.Name, &row.Purpose, &unit,
			&row.DefaultAssignee, &row.Counts.Open,
			&row.Counts.Done, &row.Counts.Closed, &change.At,
			&change.Actor, &change.ActorKind, &archived,
			&row.Version); err != nil {
			return nil, 0, fmt.Errorf("tracker: scan a project: %w", err)
		}
		row.Archived = archived != 0
		row.LastChange = change.value()
		row.Unit, row.Lead = resolveUnit(q.Units, unit)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("tracker: read the projects: %w", err)
	}
	return out, total, nil
}

// lastChangeColumns is the three columns as they come off the row.
//
// THE INSTANT IS THE NULLABLE ONE and the two strings are NOT NULL with an
// empty default, which is the same division the columns are declared with:
// "nothing has been filed here" is one fact, and "the commit named nobody" is
// a different one that must not be able to hide it.
type lastChangeColumns struct {
	At        sql.NullInt64
	Actor     string
	ActorKind string
}

// value is the answer, or nil for a project with no work.
func (c lastChangeColumns) value() *LastChange {
	if !c.At.Valid {
		return nil
	}
	return &LastChange{
		At:        store.DecodeTime(c.At.Int64),
		Actor:     c.Actor,
		ActorKind: AuthorKind(c.ActorKind),
	}
}

// resolveUnit renders a project's chart-owned unit against the chart.
//
// A PROJECT THAT NAMES NO UNIT IS RESOLVED, because naming none is a valid
// state — the finding is a project naming one the chart does not have, and
// conflating the two would report every unfiled project as orphaned.
func resolveUnit(units Units, name string) (UnitRef, LeadRef) {
	name = strings.TrimSpace(name)
	if name == "" {
		return UnitRef{Resolved: true}, LeadRef{}
	}
	if units == nil {
		return UnitRef{Key: name}, LeadRef{}
	}
	display, lead, found := units.ResolveUnit(name)
	return UnitRef{Key: name, Name: display, Resolved: found}, lead
}

// ---- one project, in full ---------------------------------------------- //

// FieldGroup is a project's fields as a form renders them: by what they apply
// to, required first.
type FieldGroup struct {
	// AppliesTo is the type slug, or empty for the fields that apply to
	// every type.
	AppliesTo string     `json:"applies_to,omitempty"`
	Fields    []FieldDef `json:"fields"`
}

// StatusDef is one of the fixed six with what a model chooses BY.
type StatusDef struct {
	Status      Status      `json:"status"`
	Label       string      `json:"label"`
	Group       StatusGroup `json:"group"`
	Description string      `json:"description"`
}

// ProjectDetail is one project in full — what a seat learns its vocabulary
// from, and what the Project screen's Overview tab draws.
type ProjectDetail struct {
	ProjectRow

	Statuses []StatusDef  `json:"statuses"`
	Types    []TaskType   `json:"types"`
	Fields   []FieldGroup `json:"fields"`

	// Shadowed names the workspace field ids this project redeclares, so a
	// reader can see which definition is in force — the state a field in
	// the middle of a move between scopes is in.
	Shadowed []string `json:"shadowed,omitempty"`

	Tags []Tag `json:"tags,omitempty"`

	// PolicyStamp is what a task's own stamp is compared against: a task
	// validated below it has not been checked against the current
	// declarations.
	PolicyStamp int `json:"policy_stamp"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// ProjectDetailQuery asks for one.
type ProjectDetailQuery struct {
	// Project is the key. Required — there is no "the" project.
	Project string

	// ForType narrows the fields to the ones that apply to one type, which
	// is what a model filing a bug wants rather than the whole catalogue.
	ForType string

	// Units resolves the chart-owned unit — see [Units].
	Units Units

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration

	// MaxLagSeq is the same bound counted in RECORDS, which is what
	// the broker actually answers — the duration above is derived
	// from it through this node's own drain rate. Both may be set
	// and the read refuses past whichever is reached first.
	MaxLagSeq uint64
}

// Project answers one project in full.
//
// ONE READ TRANSACTION for the project, both catalogues and its tags — so the
// fields a form draws and the policy stamp it validates against come from one
// apply.
func (r *Reader) Project(ctx context.Context, q ProjectDetailQuery) (
	ProjectDetail, error) {

	if q.Level == "" {
		return ProjectDetail{}, fmt.Errorf("tracker: this project read " +
			"names no level — a surface resolves an absent read_level to its " +
			"own default before it reads")
	}
	key := ProjectKey(q.Project)
	if key == "" {
		return ProjectDetail{}, fmt.Errorf("tracker: describing a project " +
			"names one — pass its key")
	}

	var out ProjectDetail
	served, err := r.log.Read(ctx, statelog.Query{
		Level:       q.Level,
		Scope:       projectDetailScope(key),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		MaxLagSeq:   q.MaxLagSeq,
		Set:         true,
	}, func(tx *sql.Tx) error {
		return readProjectDetail(ctx, tx, key, q, &out)
	})
	if err != nil {
		return ProjectDetail{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	out.LogLag = served.Lag
	if served.Incomplete != nil {
		out.Incomplete = incompleteFrom(served.Incomplete)
	}
	return out, nil
}

// projectDetailScope is the project's container PLUS the catalogue family.
//
// The container because every aggregate on this answer is over the project's
// task rows; the catalogue family because the fields and types are resolved
// from the workspace declarations, which no container's closure reaches — a
// node holding a catalogue record it cannot decode would draw a form from a
// stale declaration and report the answer complete.
func projectDetailScope(project string) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{
		ScopeTerm{Kind: TermContainer, ID: project}.Path(),
		ScopeTerm{Kind: TermFamily, ID: string(KindCatalogue)}.Path(),
	}}.Normalised()
}

func readProjectDetail(ctx context.Context, tx *sql.Tx, key string,
	q ProjectDetailQuery, out *ProjectDetail) error {

	project, found, err := readProject(ctx, tx, key)
	if err != nil {
		return err
	}
	if !found {
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		near, err := nearestProjects(ctx, tx, key)
		if err != nil {
			return err
		}
		if len(near) == 0 {
			return fmt.Errorf("tracker: no project %q, and this company has "+
				"none: %w", key, ErrNoProject)
		}
		return fmt.Errorf("tracker: no project %q — did you mean %s: %w",
			key, strings.Join(near, ", "), ErrNoProject)
	}

	out.Key = project.Key
	out.Name = project.Name
	out.Purpose = project.Purpose
	out.DefaultAssignee = project.DefaultAssignee
	out.Archived = project.Archived
	out.Version = project.Version
	out.Unit, out.Lead = resolveUnit(q.Units, project.Unit)

	// THE MAINTAINED COLUMNS ARE NOT ON THE DOCUMENT, so they are read
	// from the row rather than decoded with everything above: the census
	// and the last change are what the APPLIER derives from the work
	// filed into the project, and the document is what a writer stated
	// about the project itself.
	var change lastChangeColumns
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := tx.QueryRowContext(ctx, `
		SELECT open_count, done_count, closed_count, last_change_at,
		       last_change_actor, last_change_actor_kind
		FROM tracker_projects
		WHERE key = ?`, key).Scan(&out.Counts.Open, &out.Counts.Done,
		&out.Counts.Closed, &change.At, &change.Actor,
		&change.ActorKind); err != nil {
		return fmt.Errorf("tracker: read the task counts of %s: %w", key, err)
	}
	out.LastChange = change.value()

	out.Statuses = statusDefs()

	types, _, err := readTypeCatalogue(ctx, tx)
	if err != nil {
		return err
	}
	out.Types = liveTypes(EffectiveTypes(types.Types))
	forType := strings.TrimSpace(q.ForType)
	if forType != "" && !knownType(out.Types, forType) {
		return fmt.Errorf("tracker: no type %q — this company files %s: %w",
			forType, strings.Join(typeSlugs(out.Types), ", "), ErrNoProject)
	}

	fields, _, err := readFieldCatalogue(ctx, tx)
	if err != nil {
		return err
	}
	out.Fields, out.Shadowed = groupFields(fields.Fields, project.Fields, forType)
	out.PolicyStamp = fields.PolicyVersion
	if project.PolicyVersion > out.PolicyStamp {
		// THE HIGHER OF THE TWO, because a task is validated against
		// both declarations and its stamp records the pair: a project
		// that redeclared a field after the workspace last changed one
		// is the nearer authority.
		out.PolicyStamp = project.PolicyVersion
	}

	tags, held, err := readTagSet(ctx, tx, key)
	if err != nil {
		return err
	}
	if held {
		out.Tags = liveTags(tags.Tags)
	}

	position, applied, err := readCheckpoint(ctx, tx)
	if err != nil {
		return err
	}
	out.LogSeq, out.AppliedThrough = position, applied
	return nil
}

// statusDefs is the fixed six with their labels, groups and descriptions.
//
// DERIVED from [Statuses] rather than typed out, for the reason
// [deliveredClause] derives its groups: a status added to the set must reach
// every reader, and a literal list is the reader it would not reach.
func statusDefs() []StatusDef {
	out := make([]StatusDef, 0, len(Statuses))
	for _, status := range Statuses {
		out = append(out, StatusDef{
			Status: status, Label: status.Label(),
			Group: status.Group(), Description: status.Description(),
		})
	}
	return out
}

// groupFields groups the effective declarations by what they apply to.
//
// REQUIRED FIRST inside each group, because the caller reading this is either
// drawing a form or filing work, and both want the fields that will refuse
// them at the top. A project declaration of the same id SHADOWS the workspace
// one and the shadowed id is NAMED, which is what a field in the middle of a
// move between scopes looks like.
func groupFields(workspace, project []FieldDef, forType string) (
	[]FieldGroup, []string) {

	effective := map[string]FieldDef{}
	for _, field := range workspace {
		effective[field.ID] = field
	}
	var shadowed []string
	for _, field := range project {
		if _, both := effective[field.ID]; both {
			shadowed = append(shadowed, field.ID)
		}
		effective[field.ID] = field
	}
	sort.Strings(shadowed)

	groups := map[string][]FieldDef{}
	for _, field := range effective {
		if field.Archived {
			// AN ARCHIVED DEFINITION IS HIDDEN HERE. Its values stay
			// on their tasks and `get_task` returns them under
			// `archived_fields`; what this answer is for is what a
			// caller may FILE, and an archived field is not that.
			continue
		}
		if len(field.AppliesTo) == 0 {
			// A FIELD THAT NAMES NO TYPE APPLIES TO EVERY ONE, so it
			// survives a `for_type` narrowing rather than being
			// filtered out by it — filtering it would hide exactly
			// the fields a caller filing that type must carry.
			groups[""] = append(groups[""], field)
			continue
		}
		for _, slug := range field.AppliesTo {
			if forType != "" && slug != forType {
				continue
			}
			groups[slug] = append(groups[slug], field)
		}
	}

	keys := make([]string, 0, len(groups))
	for slug := range groups {
		keys = append(keys, slug)
	}
	sort.Strings(keys)

	out := make([]FieldGroup, 0, len(keys))
	for _, slug := range keys {
		fields := groups[slug]
		sort.SliceStable(fields, func(i, j int) bool {
			if fields[i].Required != fields[j].Required {
				return fields[i].Required
			}
			return fields[i].Slug < fields[j].Slug
		})
		out = append(out, FieldGroup{AppliesTo: slug, Fields: fields})
	}
	return out, shadowed
}

func liveTags(in []Tag) []Tag {
	out := make([]Tag, 0, len(in))
	for _, tag := range in {
		if !tag.Archived {
			out = append(out, tag)
		}
	}
	return out
}

func knownType(types []TaskType, slug string) bool {
	for _, t := range types {
		if t.Slug == slug {
			return true
		}
	}
	return false
}

func typeSlugs(types []TaskType) []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, t.Slug)
	}
	sort.Strings(out)
	return out
}

// nearestProjects is the three keys a refusal names.
//
// THE REFUSAL REPEATS THE VALID VALUES, which is this surface's own rule: a
// model that typed a key wrong learns the right one from the refusal rather
// than by calling the listing.
func nearestProjects(ctx context.Context, tx *sql.Tx, key string) ([]string, error) {
	// A SHARED FIRST LETTER FIRST, then length, then alphabetical — which
	// is enough to put `ENG` at the top for a typed `EGN` without carrying
	// an edit-distance implementation into SQL for a three-item list.
	prefix := ""
	if key != "" {
		prefix = key[:1] + "%"
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT key FROM tracker_projects WHERE archived = 0
		ORDER BY (CASE WHEN ? <> '' AND key LIKE ? THEN 0 ELSE 1 END),
		         ABS(LENGTH(key) - ?), key
		LIMIT 3`, prefix, prefix, len(key))
	if err != nil {
		return nil, fmt.Errorf("tracker: look for a project near %q: %w", key, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var near string
		if err := rows.Scan(&near); err != nil {
			return nil, fmt.Errorf("tracker: scan a nearby project: %w", err)
		}
		out = append(out, near)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: look for a project near %q: %w", key, err)
	}
	return out, nil
}
