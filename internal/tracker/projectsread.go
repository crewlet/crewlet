package tracker

import (
	"context"
	"database/sql"
	"errors"
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
// # Both of these are SET reads
//
// A project's listing and its description are dominated by aggregates over
// TASK rows — the counts here, and `active_sprint {committed, done, …}`,
// `velocity_avg` and `by_assignee` in the description. A set read cannot
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

// SprintSummary is the one-line sprint state a listing row carries.
type SprintSummary struct {
	Active            *ActiveSprint `json:"active,omitempty"`
	Next              *int          `json:"next,omitempty"`
	PendingSpillovers []int         `json:"pending_spillovers,omitempty"`
}

// ActiveSprint is the running sprint, with what it is at.
type ActiveSprint struct {
	Number  int           `json:"number"`
	Name    string        `json:"name"`
	State   SprintState   `json:"state"`
	EndAt   time.Time     `json:"end_at"`
	Figures SprintFigures `json:"figures"`

	// DaysRemaining is whole days to the end, floored at zero — a sprint
	// past its end but not yet closed is late, not negative.
	DaysRemaining int `json:"days_remaining"`
}

// TaskCounts is a project's maintained task census.
type TaskCounts struct {
	Open   int `json:"open"`
	Done   int `json:"done"`
	Closed int `json:"closed"`
}

// ProjectRow is one project as a listing renders it.
type ProjectRow struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Purpose string `json:"purpose,omitempty"`

	Unit UnitRef `json:"unit"`
	Lead LeadRef `json:"lead"`

	DefaultAssignee string         `json:"default_assignee,omitempty"`
	Sprints         *SprintSummary `json:"sprints,omitempty"`
	Counts          TaskCounts     `json:"task_counts"`

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
// TWO HUNDRED, which is ≈ 30 KB of rows and is the seat tool's own ceiling. A
// company with more projects than that has a chart problem rather than a
// paging problem, and the answer says `truncated` and `total` rather than
// offering a cursor nobody would page: every screen that draws projects draws
// all of them.
const MaxProjectsPerAnswer = 200

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
}

// Projects answers the company's projects with their maintained counts.
func (r *Reader) Projects(ctx context.Context, q ProjectQuery, now time.Time) (
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
		Set:         true,
	}, func(tx *sql.Tx) error {
		rows, total, err := readProjectRows(ctx, tx, q, limit, now)
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
	limit int, now time.Time) ([]ProjectRow, int, error) {

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
		       p.active_sprint, p.sprint_next, p.open_count, p.done_count,
		       p.closed_count, p.archived, p.version
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
		var active sql.NullInt64
		var next int
		var archived int
		if err := rows.Scan(&row.Key, &row.Name, &row.Purpose, &unit,
			&row.DefaultAssignee, &active, &next, &row.Counts.Open,
			&row.Counts.Done, &row.Counts.Closed, &archived,
			&row.Version); err != nil {
			return nil, 0, fmt.Errorf("tracker: scan a project: %w", err)
		}
		row.Archived = archived != 0
		row.Unit, row.Lead = resolveUnit(q.Units, unit)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("tracker: read the projects: %w", err)
	}
	for i := range out {
		summary, err := readSprintSummary(ctx, tx, out[i].Key, now)
		if err != nil {
			return nil, 0, err
		}
		out[i].Sprints = summary
	}
	return out, total, nil
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

// readSprintSummary is the sprint state a listing row carries.
//
// NIL when the project runs no sprints at all, which is what makes an absent
// `sprints` block "this team does not work in sprints" rather than "this team
// has none right now".
func readSprintSummary(ctx context.Context, tx *sql.Tx, project string,
	now time.Time) (*SprintSummary, error) {

	var any bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM tracker_sprints WHERE project_key = ?)`,
		project).Scan(&any); err != nil {
		return nil, fmt.Errorf("tracker: look for the sprints of %s: %w",
			project, err)
	}
	if !any {
		return nil, nil
	}
	summary := &SprintSummary{}

	// THE SPRINT ROW'S OWN STATE, not the project's pointer: the pointer
	// is the start guard and can briefly name a sprint the duty has just
	// closed, and a screen rendering a closed sprint as active is the one
	// thing this block must not do.
	var number int
	var name string
	var end int64
	switch err := tx.QueryRowContext(ctx, `
		SELECT number, name, end_at FROM tracker_sprints
		WHERE project_key = ? AND state = ? AND archived = 0
		ORDER BY number LIMIT 1`,
		project, string(SprintActive)).Scan(&number, &name, &end); {
	case err == nil:
		endAt := store.DecodeTime(end)
		summary.Active = &ActiveSprint{
			Number: number, Name: name, State: SprintActive,
			EndAt: endAt, DaysRemaining: daysRemaining(endAt, now),
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return nil, fmt.Errorf("tracker: read the active sprint of %s: %w",
			project, err)
	}

	var next sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MIN(number) FROM tracker_sprints
		WHERE project_key = ? AND state = ? AND archived = 0`,
		project, string(SprintFuture)).Scan(&next); err != nil {
		return nil, fmt.Errorf("tracker: read the next sprint of %s: %w",
			project, err)
	}
	if next.Valid {
		n := int(next.Int64)
		summary.Next = &n
	}

	// PENDING: closed, and nobody has decided where the unfinished work
	// goes. The shipped partial index `tracker_sprints_unsettled_idx` is
	// exactly this predicate.
	pending, err := tx.QueryContext(ctx, `
		SELECT number FROM tracker_sprints
		WHERE project_key = ? AND state = 'closed' AND rollover_to IS NULL
		ORDER BY number`, project)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the pending spillovers of %s: %w",
			project, err)
	}
	defer func() { _ = pending.Close() }()
	for pending.Next() {
		var n int
		if err := pending.Scan(&n); err != nil {
			return nil, fmt.Errorf("tracker: scan a pending spillover of %s: %w",
				project, err)
		}
		summary.PendingSpillovers = append(summary.PendingSpillovers, n)
	}
	if err := pending.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read the pending spillovers of %s: %w",
			project, err)
	}
	return summary, nil
}

// daysRemaining is whole days from now to an end, floored at zero.
func daysRemaining(end, now time.Time) int {
	days := int(end.Sub(now).Hours() / 24)
	if remainder := end.Sub(now); remainder > 0 && remainder.Hours() < 24 {
		// A SPRINT ENDING TODAY HAS A DAY LEFT, not none: rounding it to
		// zero tells a team the sprint is over while they are still in
		// it.
		return 1
	}
	if days < 0 {
		return 0
	}
	return days
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

	SprintPolicy *SprintPolicy `json:"sprint_policy,omitempty"`

	// Recent is the last [SprintWindowDefault] sprints with every figure,
	// and VelocityAvg is their mean delivery — the same arithmetic
	// `sprint_report` runs, from the same function.
	Recent      []SprintRow `json:"recent_sprints,omitempty"`
	VelocityAvg *float64    `json:"velocity_avg,omitempty"`

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
}

// Project answers one project in full.
//
// ONE READ TRANSACTION for the project, both catalogues, its tags and its
// sprints — so the fields a form draws and the policy stamp it validates
// against come from one apply.
func (r *Reader) Project(ctx context.Context, q ProjectDetailQuery,
	now time.Time) (ProjectDetail, error) {

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
		Set:         true,
	}, func(tx *sql.Tx) error {
		return readProjectDetail(ctx, tx, key, q, now, &out)
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
	q ProjectDetailQuery, now time.Time, out *ProjectDetail) error {

	project, found, err := readProject(ctx, tx, key)
	if err != nil {
		return err
	}
	if !found {
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
	out.SprintPolicy = project.Sprints

	if err := tx.QueryRowContext(ctx, `
		SELECT open_count, done_count, closed_count FROM tracker_projects
		WHERE key = ?`, key).Scan(&out.Counts.Open, &out.Counts.Done,
		&out.Counts.Closed); err != nil {
		return fmt.Errorf("tracker: read the task counts of %s: %w", key, err)
	}

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

	summary, err := readSprintSummary(ctx, tx, key, now)
	if err != nil {
		return err
	}
	out.Sprints = summary
	if summary != nil {
		recent, _, err := readSprintRows(ctx, tx, project,
			SprintQuery{Project: key}, SprintWindowDefault, now)
		if err != nil {
			return err
		}
		out.Recent = recent
		out.VelocityAvg = velocityOf(recent)
		if summary.Active != nil {
			for _, row := range recent {
				if row.Number == summary.Active.Number {
					summary.Active.Figures = row.Figures
					break
				}
			}
		}
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
