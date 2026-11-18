package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// Reading a company's projects: the listing, and one project in full.
//
// The counts and `last_change` are READ off `tracker_projects`, never
// aggregated: the task apply maintains them ([Applier.stampProjectChange]),
// because an aggregate over every task was O(all tasks) on every dashboard
// poll for numbers that change on a handful of commits an hour.
//
// Both reads aggregate task rows, so are SET reads: neither can enumerate the
// subjects that would have entered it, so each carries `complete`/`incomplete`
// beside the read level. Their closure is the project's container (plus the
// catalogues for the detail): its tasks cannot be enumerated, and a narrower
// term would certify complete an answer a deferred task record was holding.

// TaskCounts is a project's maintained task census, one number per status
// group. There is no `open`: waiting work and work in progress are acted on
// oppositely, so a surface wanting every unfinished item adds Todo and Active
// rather than reading a fifth number that could disagree with them.
type TaskCounts struct {
	// Todo is the work in the `not_started` group.
	Todo int `json:"todo"`
	// Active is the work in the `active` group — somebody has started it.
	Active int `json:"active"`
	Done   int `json:"done"`
	Closed int `json:"closed"`
}

// censusColumns is how the four are read off a project row: the maintained
// open bucket less its active part, then the three columns themselves.
const censusColumns = `open_count - active_count, active_count, done_count, closed_count`

// LastChange is when a project's work last changed and who changed it,
// maintained beside the counts ([Applier.stampProjectChange] decides which
// commits move it). The instant is the AUTHORED one, as the activity feed shows.
//
// A pointer, because a project with no work filed has no answer: a zero instant
// would render as the year 1, and the project's creation would report untouched
// projects as freshly active.
type LastChange struct {
	At time.Time `json:"at"`

	// Actor is the commit's handle and ActorKind which of the four kinds it
	// is, so `ana` the person and `ana` the seat stay distinct. Either may be
	// empty (a record can name no actor); At alone says the answer exists.
	Actor     string     `json:"actor,omitempty"`
	ActorKind AuthorKind `json:"actor_kind,omitempty"`
}

// ProjectRow is one project as a listing renders it.
type ProjectRow struct {
	Key     string `json:"key"`
	Name    string `json:"name" untrusted:"name"`
	Purpose string `json:"purpose,omitempty" untrusted:"name"`

	Unit UnitRef `json:"unit"`
	Lead LeadRef `json:"lead"`

	DefaultAssignee string `json:"default_assignee,omitempty"`

	// TargetDate is when the lead means the project to be finished, a day
	// on the company's clock (`YYYY-MM-DD`), absent for no target — see
	// [Project.TargetDate].
	TargetDate string `json:"target_date,omitempty"`

	Counts TaskCounts `json:"task_counts"`

	// LastChange is nil for a project no work has ever been filed into —
	// see [LastChange].
	LastChange *LastChange `json:"last_change,omitempty"`

	Archived bool   `json:"archived,omitempty"`
	Version  uint64 `json:"version"`
}

// ProjectCensus is how many projects each archival set holds, narrowed by `q`
// and `unit` and nothing else: the listing's question minus its archival term,
// so no segment is offered that is empty under the reader's own filter.
//
// It exists because a screen showing one set ([ArchivedMode]) cannot otherwise
// tell an empty company from one that archived every project without asking
// once per segment.
type ProjectCensus struct {
	Active   int `json:"active"`
	Archived int `json:"archived"`
}

// Count is how many projects one archival mode selects out of this census.
// The listing's `total` is this rather than a second COUNT(*), so the two
// cannot disagree.
func (c ProjectCensus) Count(mode ArchivedMode) int {
	switch mode {
	case ArchivedExclude:
		return c.Active
	case ArchivedOnly:
		return c.Archived
	case ArchivedInclude:
		return c.Active + c.Archived
	}
	// Unreachable: [Reader.Projects] refuses an invalid mode first. Zero, not
	// a panic: an arithmetic helper is not where a bad enum should be found.
	return 0
}

// Total is every project the census counted, whichever set was asked for.
func (c ProjectCensus) Total() int { return c.Active + c.Archived }

// ProjectListing is the answer.
type ProjectListing struct {
	// Projects is never nil: with no `omitempty` a nil slice is
	// `"projects": null`, a third state its client mishandles — see
	// [readProjectRows].
	Projects []ProjectRow `json:"projects"`

	// Total is how many projects match the filter, and Truncated says the
	// listing stopped short of it; no cursor — see [MaxProjectsPerAnswer].
	Total     int  `json:"total"`
	Truncated bool `json:"truncated,omitempty"`

	// Census answers the same question for BOTH archival sets, so a caller
	// that selected one can still tell an empty set from an empty company.
	// Total is its [ProjectCensus.Count] of the asked mode.
	Census ProjectCensus `json:"census"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// MaxProjectsPerAnswer is how many projects one listing carries: what a screen
// takes. A company with more has a chart problem rather than a paging problem,
// so the answer says `truncated` and `total` rather than offering a cursor.
//
// It is not the seat tool's ceiling: at a measured ≈ 610 bytes an indented row,
// two hundred rows overrun the 64 KiB `builtin.ToolAnswerBytes` — see
// [MaxProjectsPerToolAnswer].
const MaxProjectsPerAnswer = 200

// MaxProjectsPerToolAnswer is how many a SEAT's listing carries. Fifty rows at
// a measured ≈ 610 bytes encode at ≈ 30 KiB, leaving room for rows twice as
// long (nothing caps a unit's purpose) under `builtin.ToolAnswerBytes`, which
// REFUSES an answer past 64 KiB rather than cutting it.
//
// A page rather than a refusal, as [MaxCatalogueOptions]: `total` beside
// `truncated` tells a seat what it is not seeing, and `q` and `unit` narrow.
// `TestEveryToolAnswerFitsToolAnswerBytes` re-measures this when the row grows.
const MaxProjectsPerToolAnswer = 50

// ProjectSort is one ordering a projects listing may be asked for.
//
// A closed set over `tracker_projects`' own columns, because the engine must
// order: an ordering applied after the [MaxProjectsPerAnswer] bound orders a
// page, not the company, and reads exactly like the answer to the real question.
// The lead is not in it: it is resolved at read time against the chart
// ([Units]), so there is no column to order by.
type ProjectSort string

// The nine orderings, each over a column the project row carries, so each
// orders the whole asked set; the four census keys are `task_counts`' four.
const (
	ProjectSortKey        ProjectSort = "key"
	ProjectSortName       ProjectSort = "name"
	ProjectSortUnit       ProjectSort = "unit"
	ProjectSortTodo       ProjectSort = "todo"
	ProjectSortActive     ProjectSort = "active"
	ProjectSortDone       ProjectSort = "done"
	ProjectSortClosed     ProjectSort = "closed"
	ProjectSortLastChange ProjectSort = "last_change"
	ProjectSortTarget     ProjectSort = "target"
)

// ProjectSorts is every ordering, in the order a refusal names them.
var ProjectSorts = []ProjectSort{
	ProjectSortKey, ProjectSortName, ProjectSortUnit,
	ProjectSortTodo, ProjectSortActive, ProjectSortDone, ProjectSortClosed,
	ProjectSortLastChange, ProjectSortTarget,
}

// ProjectSortNames is the same list as wire strings, for refusals and for the
// gate that holds the directory's own copy of it against this one.
func ProjectSortNames() []string {
	out := make([]string, len(ProjectSorts))
	for i, k := range ProjectSorts {
		out[i] = string(k)
	}
	return out
}

// Valid reports whether this is one of the nine orderings. The zero value is
// not one yet is a meaningful field value ([ProjectQuery.Sort]), unlike
// [ArchivedMode]'s: a missing ordering changes only the order, a missing
// archival set changes which projects are in the answer.
func (k ProjectSort) Valid() bool { return slices.Contains(ProjectSorts, k) }

// projectSortColumns is the expression each ordering reads, keyed on the typed
// enum because these strings are interpolated into SQL: only a [ProjectSort]
// can index it, so no caller's text reaches the statement.
var projectSortColumns = map[ProjectSort]string{
	ProjectSortKey:    "p.key",
	ProjectSortTodo:   "p.open_count - p.active_count",
	ProjectSortActive: "p.active_count",
	ProjectSortDone:   "p.done_count",
	ProjectSortClosed: "p.closed_count",

	// lower() rather than `COLLATE NOCASE`: BINARY, the tracker's collation
	// everywhere for the manual order's sake (`TestNoCollateInTracker`), sorts
	// lowercase after uppercase, and a function on the read leaves the rank
	// algebra's comparison untouched. Nothing indexes these columns anyway.
	ProjectSortName: "lower(p.name)",
	ProjectSortUnit: "lower(p.unit)",

	ProjectSortLastChange: "p.last_change_at",

	// `YYYY-MM-DD` text sorts in calendar order; [coerceDay] stores no other.
	ProjectSortTarget: "p.target_date",
}

// ProjectQuery asks for a company's projects.
type ProjectQuery struct {
	// Q narrows by a case-insensitive substring of the key, the name or
	// the purpose — what a person types into a filter box.
	Q string

	// Unit narrows to the projects one chart unit owns, named by either of
	// its spellings — its id or its name, in any case.
	Unit string

	// Archived selects WHICH SET rather than widening: narrowing a page of
	// both sets would miss archived rows past [MaxProjectsPerAnswer] active
	// ones. [Reader.Projects] refuses a query that does not say, as it does an
	// absent Level: this struct is built as a literal at every call site, so
	// no constructor can carry a default.
	Archived ArchivedMode

	// Sort orders the whole asked set, and Descending reverses it. The zero
	// value is [ProjectSortKey] ascending, which is also the tiebreak under
	// every other ordering, so rows level on the sorted column keep one order.
	Sort       ProjectSort
	Descending bool

	// Limit bounds the rows, clamped to [MaxProjectsPerAnswer].
	Limit int

	// Units resolves the chart-owned unit for each row and for the filter;
	// nil renders rows unresolved and matches literally — see [Units]. Not
	// serialised (an interface encodes as `{}`): the far side of a wire
	// attaches its own chart — see internal/estate.
	Units Units `json:"-"`

	Level   statelog.ReadLevel
	Session statelog.Position
	Floors  []statelog.Position
	MaxLag  time.Duration

	// MaxLagSeq is the same bound counted in RECORDS, what the broker
	// answers; MaxLag is derived from it through this node's drain rate. The
	// read refuses past whichever is reached first.
	MaxLagSeq uint64
}

// ParseProjectQuery reads the five keys this listing's grammar owns — `q`,
// `unit`, `archived`, `sort` and `limit` — and refuses a value by name.
//
// One parse for the REST call and a seat's `list_projects` (via [MapParams]),
// so `archived=only` cannot mean two things — [Query]'s one-parse rule. The
// freshness keys are [statelog.ParseFreshness]'s and [ProjectQuery.Units] the
// surface's; the limit is parsed but not clamped, since the surfaces clamp to
// different ceilings ([MaxProjectsPerAnswer], [MaxProjectsPerToolAnswer]).
// An absent `archived` resolves here, to the surface's default:
// [Reader.Projects] refuses a query without one.
func ParseProjectQuery(p Params) (ProjectQuery, error) {
	q := ProjectQuery{
		Q:        strings.TrimSpace(p.String("q")),
		Unit:     strings.TrimSpace(p.String("unit")),
		Archived: ArchivedExclude,
		Limit:    p.Int("limit", 0),
	}
	if raw := strings.TrimSpace(p.String("archived")); raw != "" {
		mode := ArchivedMode(raw)
		if !mode.Valid() {
			return ProjectQuery{}, fmt.Errorf("worklet: archived is one of %s, "+
				"and %q is none of them", words.Or(ArchivedModeNames()),
				raw)
		}
		q.Archived = mode
	}
	if raw := strings.TrimSpace(p.String("sort")); raw != "" {
		// The task listing's `-<key>` grammar, as the same dashboard control
		// writes it, but ONE term: the control can express nothing else.
		descending := strings.HasPrefix(raw, "-")
		// Not named `sort`, which would shadow the imported package.
		order := ProjectSort(strings.TrimPrefix(raw, "-"))
		if !order.Valid() {
			return ProjectQuery{}, fmt.Errorf("worklet: sort is one of %s, "+
				"each optionally with a leading `-` for descending, and %q is "+
				"none of them", words.Or(ProjectSortNames()), raw)
		}
		q.Sort, q.Descending = order, descending
	}
	return q, nil
}

// Projects answers the company's projects with their maintained counts.
func (r *Reader) Projects(ctx context.Context, q ProjectQuery) (
	ProjectListing, error) {

	if q.Level == "" {
		return ProjectListing{}, fmt.Errorf("worklet: this project read " +
			"names no level — a surface resolves an absent read_level to its " +
			"own default before it reads")
	}
	// An absent archival set is refused like an absent level: membership
	// turns on it, and a default here would be one no caller chose or sees.
	if !q.Archived.Valid() {
		return ProjectListing{}, fmt.Errorf("worklet: this project read names "+
			"no archival set — `archived` is one of %s, and a surface resolves "+
			"an absent one to its own default before it reads",
			words.Or(ArchivedModeNames()))
	}
	if q.Sort != "" && !q.Sort.Valid() {
		return ProjectListing{}, fmt.Errorf("worklet: %q is not a project sort "+
			"key — one of %s", q.Sort, words.Or(ProjectSortNames()))
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxProjectsPerAnswer {
		limit = MaxProjectsPerAnswer
	}

	var listing ProjectListing
	served, err := r.log.Read(ctx, statelog.Query{
		Level:     q.Level,
		Scope:     projectListScope(),
		Session:   q.Session,
		Floors:    q.Floors,
		MaxLag:    q.MaxLag,
		MaxLagSeq: q.MaxLagSeq,
		Set:       true,
	}, func(tx *sql.Tx) error {
		rows, census, err := readProjectRows(ctx, tx, q, limit)
		if err != nil {
			return err
		}
		// Total is the census's arithmetic, never a second count.
		listing.Projects, listing.Census = rows, census
		listing.Total = census.Count(q.Archived)
		listing.Truncated = listing.Total > len(rows)
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

// projectListScope is the DOMAIN: a listing spans every container and is about
// the whole company, so a narrower term would certify it complete while a
// deferred record in some project held a row that moves a count.
func projectListScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised()
}

// readProjectRows reads the projects a query names, and the census of both
// archival sets under the same narrowing. The narrowing and the archival term
// are built apart: the census is the question without the archival term, the
// rows are it with one.
func readProjectRows(ctx context.Context, tx *sql.Tx, q ProjectQuery,
	limit int) ([]ProjectRow, ProjectCensus, error) {

	// narrowing is `q` and `unit`: what the caller asked ABOUT, not which
	// archival set FOR.
	narrowing := []string{}
	var args []any
	// The unit's key through the chart ([unitKeys]), so a caller filters by
	// the name a screen showed; in the narrowing, so the census is cut by it.
	if units := unitKeys(q.Units, []string{q.Unit}); len(units) > 0 {
		narrowing = append(narrowing, "p.unit IN ("+placeholders(len(units))+")")
		args = append(args, anyOf(units)...)
	}
	if term := strings.TrimSpace(q.Q); term != "" {
		// Key, name or purpose: a filter-box user remembers one of the
		// three. The term is escaped by [likeEscape].
		narrowing = append(narrowing, `(p.key LIKE ? ESCAPE '\' OR p.name LIKE ? `+
			`ESCAPE '\' OR p.purpose LIKE ? ESCAPE '\')`)
		pattern := "%" + likeEscape(term) + "%"
		args = append(args, pattern, pattern, pattern)
	}
	narrowed := ""
	if len(narrowing) > 0 {
		narrowed = " WHERE " + joinAnd(narrowing)
	}

	// One grouped aggregate for both sets, and the listing's only count.
	census, err := readProjectCensus(ctx, tx, narrowed, args)
	if err != nil {
		return nil, ProjectCensus{}, err
	}

	// Exactly the asked set; [ArchivedInclude], asking for both, adds none.
	clause := narrowed
	switch q.Archived {
	case ArchivedExclude:
		clause = appendTerm(narrowed, "p.archived = 0")
	case ArchivedOnly:
		clause = appendTerm(narrowed, "p.archived = 1")
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT p.key, p.name, p.purpose, p.unit, p.default_assignee,
		       p.target_date, p.open_count - p.active_count, p.active_count,
		       p.done_count, p.closed_count, p.last_change_at, p.last_change_actor,
		       p.last_change_actor_kind, p.archived, p.version
		FROM tracker_projects p`+clause+`
		ORDER BY `+projectOrderBy(q)+`
		LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, ProjectCensus{}, fmt.Errorf("worklet: read the projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// Never nil: [ProjectListing.Projects] has no `omitempty`, so nil encodes
	// as `"projects": null`, and the client's `projects?.length === 0` then
	// misreads a company with none, hiding its first-project panel.
	out := make([]ProjectRow, 0, limit)
	for rows.Next() {
		var row ProjectRow
		var unit string
		var archived int
		var change lastChangeColumns
		var target sql.NullString
		if err := rows.Scan(&row.Key, &row.Name, &row.Purpose, &unit,
			&row.DefaultAssignee, &target, &row.Counts.Todo, &row.Counts.Active,
			&row.Counts.Done, &row.Counts.Closed, &change.At,
			&change.Actor, &change.ActorKind, &archived,
			&row.Version); err != nil {
			return nil, ProjectCensus{}, fmt.Errorf("worklet: scan a project: %w", err)
		}
		row.Archived = archived != 0
		row.TargetDate = target.String
		row.LastChange = change.value()
		row.Unit, row.Lead = resolveUnit(q.Units, unit)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, ProjectCensus{}, fmt.Errorf("worklet: read the projects: %w", err)
	}
	return out, census, nil
}

// appendTerm ANDs one more term onto a clause that may be empty.
func appendTerm(clause, term string) string {
	if clause == "" {
		return " WHERE " + term
	}
	return clause + " AND " + term
}

// readProjectCensus counts both archival sets under one narrowing, grouped
// rather than two counts so both halves come from one pass over one snapshot.
func readProjectCensus(ctx context.Context, tx *sql.Tx, narrowed string,
	args []any) (ProjectCensus, error) {

	rows, err := tx.QueryContext(ctx,
		`SELECT p.archived, COUNT(*) FROM tracker_projects p`+narrowed+
			` GROUP BY p.archived`, args...)
	if err != nil {
		return ProjectCensus{}, fmt.Errorf("worklet: count the projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var census ProjectCensus
	for rows.Next() {
		var archived, count int
		if err := rows.Scan(&archived, &count); err != nil {
			return ProjectCensus{}, fmt.Errorf("worklet: scan the project census: %w", err)
		}
		// Any non-zero is archived, as every reader of this column reads it:
		// a reader testing `== 1` would silently drop a row.
		if archived != 0 {
			census.Archived += count
			continue
		}
		census.Active += count
	}
	if err := rows.Err(); err != nil {
		return ProjectCensus{}, fmt.Errorf("worklet: count the projects: %w", err)
	}
	return census, nil
}

// projectOrderBy is the ordering clause one query compiles to, built from the
// enum, never a string; [Reader.Projects] has already refused an invalid key.
func projectOrderBy(q ProjectQuery) string {
	column, ok := projectSortColumns[q.Sort]
	if !ok {
		// An absent ordering is the key, also the tiebreak: emit it once.
		return "p.key"
	}
	direction := ""
	if q.Descending {
		direction = " DESC"
	}
	clause := column + direction
	if q.Sort == ProjectSortLastChange || q.Sort == ProjectSortTarget {
		// Absent values sort LAST in both directions: SQLite puts NULL first
		// ascending, and "nothing recorded" is not the smallest value, it is
		// not a value. The grid drawing these rows states the same rule.
		clause = column + " IS NULL, " + clause
	}
	if q.Sort == ProjectSortKey {
		return clause
	}
	// The tiebreak, so equal rows don't reshuffle between identical polls.
	return clause + ", p.key"
}

// lastChangeColumns is the three columns as they come off the row. Only the
// instant is nullable (the strings default empty), as the columns are
// declared: "nothing filed here" must not hide behind "the commit named nobody".
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
	Label       string      `json:"label" untrusted:"name"`
	Group       StatusGroup `json:"group"`
	Description string      `json:"description" untrusted:"name"`
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

	// OwnFields are the project's own declarations as stored, archived
	// included: the whole post-state a `fields` write on write_project
	// replaces. Apart from [ProjectDetail.Fields], the effective set (scopes
	// merged, archived hidden): sent back, that one would copy every workspace
	// field in as a shadow and drop the archived declarations, and with them
	// the names their values are kept under.
	OwnFields []FieldDef `json:"own_fields"`

	Tags []Tag `json:"tags,omitempty"`

	// ArchivedTags are the labels that take no new work, which [Tags]
	// leaves out because a create refuses them. A lead reads them to know
	// what was retired and under which word, since the tasks filed under
	// one keep it.
	ArchivedTags []Tag `json:"archived_tags,omitempty"`

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

	// Units resolves the chart-owned unit — see [Units]. Not serialised (an
	// interface encodes as `{}`): the far side of a wire attaches its own
	// chart — see internal/estate.
	Units Units `json:"-"`

	Level   statelog.ReadLevel
	Session statelog.Position
	Floors  []statelog.Position
	MaxLag  time.Duration

	// MaxLagSeq is the same bound counted in RECORDS, what the broker
	// answers; MaxLag is derived from it through this node's drain rate. The
	// read refuses past whichever is reached first.
	MaxLagSeq uint64
}

// Project answers one project in full, in one read transaction over the
// project, both catalogues and its tags, so a form's fields and the policy
// stamp it validates against come from one apply.
func (r *Reader) Project(ctx context.Context, q ProjectDetailQuery) (
	ProjectDetail, error) {

	if q.Level == "" {
		return ProjectDetail{}, fmt.Errorf("worklet: this project read " +
			"names no level — a surface resolves an absent read_level to its " +
			"own default before it reads")
	}
	key := ProjectKey(q.Project)
	if key == "" {
		return ProjectDetail{}, fmt.Errorf("worklet: describing a project " +
			"names one — pass its key")
	}

	var out ProjectDetail
	served, err := r.log.Read(ctx, statelog.Query{
		Level:     q.Level,
		Scope:     projectDetailScope(key),
		Session:   q.Session,
		Floors:    q.Floors,
		MaxLag:    q.MaxLag,
		MaxLagSeq: q.MaxLagSeq,
		Set:       true,
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

// projectDetailScope is the project's container, for the aggregates over its
// tasks, plus the catalogue family: fields and types resolve from workspace
// declarations no container reaches, and without it a form drawn from a stale
// declaration would be reported complete.
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
			return fmt.Errorf("worklet: no project %q, and this company has "+
				"none: %w", key, ErrNoProject)
		}
		return fmt.Errorf("worklet: no project %q; did you mean %s: %w",
			key, strings.Join(near, ", "), ErrNoProject)
	}

	out.Key = project.Key
	out.Name = project.Name
	out.Purpose = project.Purpose
	out.DefaultAssignee = project.DefaultAssignee
	out.TargetDate = project.TargetDate
	out.Archived = project.Archived
	out.Version = project.Version
	out.Unit, out.Lead = resolveUnit(q.Units, project.Unit)

	// The census and last change are the applier's derived row columns, not
	// on the writer's document, so they are read from the row.
	var change lastChangeColumns
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := tx.QueryRowContext(ctx, `
		SELECT `+censusColumns+`, last_change_at,
		       last_change_actor, last_change_actor_kind
		FROM tracker_projects
		WHERE key = ?`, key).Scan(&out.Counts.Todo, &out.Counts.Active,
		&out.Counts.Done, &out.Counts.Closed, &change.At, &change.Actor,
		&change.ActorKind); err != nil {
		return fmt.Errorf("worklet: read the task counts of %s: %w", key, err)
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
		return fmt.Errorf("worklet: no type %q; this company files %s: %w",
			forType, strings.Join(typeSlugs(out.Types), ", "), ErrNoType)
	}

	fields, _, err := readFieldCatalogue(ctx, tx)
	if err != nil {
		return err
	}
	out.Fields, out.Shadowed = groupFields(fields.Fields, project.Fields, forType)
	out.OwnFields = ownFields(project.Fields)
	out.PolicyStamp = fields.PolicyVersion
	if project.PolicyVersion > out.PolicyStamp {
		// The higher of the two: a task's stamp records both declarations,
		// and a project redeclaring a field after the workspace changed one
		// is the nearer authority.
		out.PolicyStamp = project.PolicyVersion
	}

	tags, held, err := readTagSet(ctx, tx, key)
	if err != nil {
		return err
	}
	if held {
		out.Tags, out.ArchivedTags = liveTags(tags.Tags), archivedTags(tags.Tags)
	}

	position, applied, err := readCheckpoint(ctx, tx)
	if err != nil {
		return err
	}
	out.LogSeq, out.AppliedThrough = position, applied
	return nil
}

// statusDefs is the fixed six with their labels, groups and descriptions,
// derived from [Statuses] (as [deliveredClause] derives its groups) so a status
// added to the set reaches this reader too.
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

// groupFields groups the effective declarations by what they apply to,
// required first, since a form or a filer wants the fields that will refuse it
// at the top. A project declaration shadows the workspace one of the same id,
// and the shadowed id is named.
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
			// Hidden: this answer is what a caller may FILE. The values
			// stay on their tasks; `get_task` returns them under
			// `archived_fields`.
			continue
		}
		if len(field.AppliesTo) == 0 {
			// A field naming no type applies to every one, so it survives
			// `for_type`: filtering it would hide fields a filer of that
			// type must carry.
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

// ownFields is a project's own declarations as a write sends them back:
// archived included, never nil, so "none" is an empty list, not a missing key.
func ownFields(stored []FieldDef) []FieldDef {
	return append(make([]FieldDef, 0, len(stored)), stored...)
}

// archivedTags is the other half of [liveTags].
func archivedTags(in []Tag) []Tag {
	var out []Tag
	for _, tag := range in {
		if tag.Archived {
			out = append(out, tag)
		}
	}
	return out
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

// nearestProjects is the three keys a refusal names, so a model that typed a
// key wrong learns the right one without calling the listing.
func nearestProjects(ctx context.Context, tx *sql.Tx, key string) ([]string, error) {
	// A shared first letter, then length, then alphabetical: enough to put
	// `ENG` first for `EGN` without an edit distance in SQL for three items.
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
		return nil, fmt.Errorf("worklet: look for a project near %q: %w", key, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var near string
		if err := rows.Scan(&near); err != nil {
			return nil, fmt.Errorf("worklet: scan a nearby project: %w", err)
		}
		out = append(out, near)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worklet: look for a project near %q: %w", key, err)
	}
	return out, nil
}
