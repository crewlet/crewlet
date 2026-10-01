package tracker

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
)

// Reading a company's projects — the listing, and one project in full.
//
// # The counts are READ, never aggregated
//
// `task_counts` is `tracker_projects.open_count/active_count/done_count/
// closed_count`, maintained by the task apply whenever a task's status group
// changes or it enters, leaves or is removed from a project — `todo` being the
// open work nobody has started, `open_count - active_count`. The alternative —
// an aggregate over every task in every project — ran on every sixty-second
// dashboard poll and was O(all tasks) for a number that changes on a handful of
// commits an hour.
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

// TaskCounts is a project's maintained task census, one number per status
// group.
//
// FOUR, NOT THREE. The census used to say `open`, which folded the work
// nobody has started into the work somebody is doing — and those are the two
// states a reader acts on oppositely: forty items waiting is a queue to
// triage, forty in progress is a team at full stretch. The status groups
// already draw the line, so the census follows them and there is no `open`:
// a surface wanting every unfinished item adds `todo` and `active`, which is
// one addition rather than a fifth number that could disagree with the two.
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

// ProjectCensus is how many projects each archival set holds.
//
// NARROWED BY `q` AND `unit`, AND BY NOTHING ELSE — it is the listing's own
// question minus its archival term. A directory narrowed to one unit that
// reported the whole company's archived count would offer a reader a segment
// that is empty under the filter they are looking through.
//
// IT EXISTS BECAUSE A SEGMENTED SCREEN CANNOT ASK TWICE. Selecting one set
// (which is what [ArchivedMode] is for) means an empty answer no longer says
// whether the company has no projects or has archived every one of them — two
// states one sentence cannot cover, and a reader acts on them differently. The
// alternative was a screen that either guessed, hedged in its own copy, or
// asked a second question per segment; the counts are one aggregate over rows
// the listing is already scanning.
type ProjectCensus struct {
	Active   int `json:"active"`
	Archived int `json:"archived"`
}

// Count is how many projects one archival mode selects out of this census.
//
// THE LISTING'S OWN `total` IS THIS, rather than a second `COUNT(*)` beside
// it: a total counted separately from the census is a number that can disagree
// with the one drawn beside it on the same screen.
func (c ProjectCensus) Count(mode ArchivedMode) int {
	switch mode {
	case ArchivedExclude:
		return c.Active
	case ArchivedOnly:
		return c.Archived
	case ArchivedInclude:
		return c.Active + c.Archived
	}
	// UNREACHABLE THROUGH [Reader.Projects], which refuses a mode that is
	// not one before it reads. Zero rather than a panic, because an
	// arithmetic helper is not where a bad enum should be discovered.
	return 0
}

// Total is every project the census counted, whichever set was asked for.
func (c ProjectCensus) Total() int { return c.Active + c.Archived }

// ProjectListing is the answer.
type ProjectListing struct {
	// Projects is EMPTY RATHER THAN ABSENT when the asked set holds none,
	// and never nil: the field carries no `omitempty`, so a nil slice is
	// `"projects": null` on the wire — a third state beside "some" and
	// "none" that every reader has to have thought about, and the one this
	// listing's own client had not. See [readProjectRows].
	Projects []ProjectRow `json:"projects"`

	// Total is how many projects match the filter, and Truncated says the
	// listing stopped short of it. A count rather than a cursor because a
	// company's projects are tens, not thousands — [MaxProjectsPerAnswer]
	// carries the rationale.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated,omitempty"`

	// Census is the same question's answer for BOTH archival sets, so a
	// caller that selected one can still tell an empty set from an empty
	// company — see [ProjectCensus]. `Total` is [ProjectCensus.Count] of
	// the mode that was asked for.
	Census ProjectCensus `json:"census"`

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

// ProjectSort is one ordering a projects listing may be asked for.
//
// A CLOSED SET OVER `tracker_projects`' OWN COLUMNS, because the ordering has
// to be the ENGINE's. The listing is bounded at [MaxProjectsPerAnswer] and an
// ordering applied AFTER that bound orders the page rather than the company:
// `sort=-todo` over a key-ordered first two hundred answers "the most waiting
// work among the projects whose keys sort first", which is not a question
// anybody asked and reads exactly like the answer to the one they did.
//
// THE LEAD IS NOT IN THE SET AND CANNOT BE. A project's lead is resolved at
// READ time against the epoch's chart ([Units]) — the tracker holds no org, so
// there is no column to order by, and a key naming one would be this package
// claiming an ordering it cannot produce.
type ProjectSort string

// The nine orderings. Each reads a column the project row already carries, so
// every one of them orders the whole asked set rather than a page of it — and
// the four census keys are the four numbers `task_counts` carries, so a
// directory can order by exactly what it draws.
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

// ProjectSortNames is the same list as wire strings, for the surfaces that
// quote it back at a caller who spelled one wrong — and for the gate that
// holds the directory's own copy of it against this one.
func ProjectSortNames() []string {
	out := make([]string, len(ProjectSorts))
	for i, k := range ProjectSorts {
		out[i] = string(k)
	}
	return out
}

// Valid reports whether this is one of the nine orderings.
//
// THE ZERO VALUE IS NOT ONE, and unlike [ArchivedMode]'s it is still a
// meaningful field value — see [ProjectQuery.Sort]. The difference is what the
// two absences would hide: an ordering nobody asked for changes the ORDER of
// an answer, where an archival set nobody asked for decides which projects are
// in it at all.
func (k ProjectSort) Valid() bool { return slices.Contains(ProjectSorts, k) }

// projectSortColumns is the expression each ordering reads.
//
// A TABLE KEYED ON THE TYPED ENUM rather than a `switch` in the SQL builder,
// because these strings are interpolated into the statement: a map that only a
// [ProjectSort] can index is what says no caller's text ever reaches it.
var projectSortColumns = map[ProjectSort]string{
	ProjectSortKey:    "p.key",
	ProjectSortTodo:   "p.open_count - p.active_count",
	ProjectSortActive: "p.active_count",
	ProjectSortDone:   "p.done_count",
	ProjectSortClosed: "p.closed_count",

	// LOWERED, and not `COLLATE NOCASE`. These two are prose typed by
	// whoever wrote the org chart, and BINARY — this store's default,
	// which the tracker's schema keeps everywhere for the manual order's
	// sake (`TestNoCollateInTracker`) — sorts every lowercase name after
	// every uppercase one. `lower()` is a function on the READ rather than
	// a collation on the column, so the rank algebra's comparison is
	// untouched, and a directory of tens of rows pays a scan over a column
	// nothing indexes either way.
	ProjectSortName: "lower(p.name)",
	ProjectSortUnit: "lower(p.unit)",

	ProjectSortLastChange: "p.last_change_at",

	// THE TEXT OF THE DAY, which sorts in calendar order because it is
	// `YYYY-MM-DD` — the one spelling [coerceDay] ever stores.
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

	// Archived is WHICH SET, and [Reader.Projects] refuses a query that
	// does not say — on the same terms as the absent [ProjectQuery.Level]
	// beside it, because this struct is built as a literal at every call
	// site and so has no constructor to carry a default.
	//
	// IT SELECTS RATHER THAN WIDENS. This was a `bool` that INCLUDED the
	// archived ones, which is two answers to a three-answer question: a
	// screen wanting the retired ones alone had to ask for both sets and
	// narrow the page it got back — so past [MaxProjectsPerAnswer] active
	// projects it was narrowing a page holding no archived row at all, and
	// reported a company with dozens of them as having archived nothing.
	Archived ArchivedMode

	// Sort orders the whole asked set, and Descending reverses it.
	//
	// THE ZERO VALUE IS [ProjectSortKey] ASCENDING, which is the order this
	// listing has always had. [ProjectSortKey] is also the TIEBREAK under
	// every other ordering, so two projects level on the sorted column come
	// back in one stable order rather than whichever the planner walked.
	Sort       ProjectSort
	Descending bool

	// Limit bounds the rows, clamped to [MaxProjectsPerAnswer].
	Limit int

	// Units resolves the chart-owned unit — both for rendering each row
	// and for the filter above. Nil renders every row unresolved and
	// matches the filter literally — see [Units].
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

// ParseProjectQuery reads the five keys this listing's GRAMMAR owns — `q`,
// `unit`, `archived`, `sort` and `limit` — and refuses a value by name.
//
// ONE PARSE FOR BOTH SURFACES. A REST call and a seat's `list_projects` ask
// the same question through different bags, and a grammar written twice is how
// `archived=only` comes to mean one thing on a screen and another in a tool —
// which is precisely the failure [Query]'s own one-parse rule exists to
// prevent. [MapParams] is what lets a tool's argument map through it.
//
// WHAT IT DOES NOT FILL is what is not the grammar's: the freshness keys are
// [ParseFreshness]'s, [ProjectQuery.Units] is the surface's own chart seam,
// and the LIMIT IS PARSED BUT NOT CLAMPED, because the two surfaces clamp to
// different ceilings ([MaxProjectsPerAnswer] and [MaxProjectsPerToolAnswer])
// and each one's reason is its own.
//
// AN ABSENT `archived` RESOLVES HERE and not in the reader: this is the
// surface's own default, and [Reader.Projects] refuses a query that reaches it
// without one.
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
			return ProjectQuery{}, fmt.Errorf("tracker: archived is one of %s, "+
				"and %q is none of them", strings.Join(ArchivedModeNames(), ", "),
				raw)
		}
		q.Archived = mode
	}
	if raw := strings.TrimSpace(p.String("sort")); raw != "" {
		// THE SAME `-<key>` GRAMMAR THE TASK LISTING'S `sort=` TAKES,
		// because it is the same control writing it: every grid in the
		// dashboard puts `-<column>` in the URL. ONE TERM rather than
		// that grammar's comma-separated list — a directory of tens of
		// rows is ordered by one column and its tiebreak, and the
		// control that writes this key can express nothing else.
		descending := strings.HasPrefix(raw, "-")
		// NOT NAMED `sort`: the standard library's package of that name
		// is imported here, and a local shadowing it is a trap for the
		// next reader rather than a compile error.
		order := ProjectSort(strings.TrimPrefix(raw, "-"))
		if !order.Valid() {
			return ProjectQuery{}, fmt.Errorf("tracker: sort is one of %s, "+
				"each optionally with a leading `-` for descending, and %q is "+
				"none of them", strings.Join(ProjectSortNames(), ", "), raw)
		}
		q.Sort, q.Descending = order, descending
	}
	return q, nil
}

// Projects answers the company's projects with their maintained counts.
func (r *Reader) Projects(ctx context.Context, q ProjectQuery) (
	ProjectListing, error) {

	if q.Level == "" {
		return ProjectListing{}, fmt.Errorf("tracker: this project read " +
			"names no level — a surface resolves an absent read_level to its " +
			"own default before it reads")
	}
	// AND NO ARCHIVAL SET IS THE SAME KIND OF SILENCE, refused for the same
	// reason: the answer's MEMBERSHIP turns on it, and a default applied
	// here would be one no caller had chosen and none could see.
	if !q.Archived.Valid() {
		return ProjectListing{}, fmt.Errorf("tracker: this project read names "+
			"no archival set — `archived` is one of %s, and a surface resolves "+
			"an absent one to its own default before it reads",
			strings.Join(ArchivedModeNames(), ", "))
	}
	if q.Sort != "" && !q.Sort.Valid() {
		return ProjectListing{}, fmt.Errorf("tracker: %q is not a project sort "+
			"key — one of %s", q.Sort, strings.Join(ProjectSortNames(), ", "))
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
		rows, census, err := readProjectRows(ctx, tx, q, limit)
		if err != nil {
			return err
		}
		// THE TOTAL IS THE CENSUS'S OWN ARITHMETIC, never a second count:
		// one number cannot then disagree with the two drawn beside it.
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

// readProjectRows reads the projects a query names, and the census of both
// archival sets under the same narrowing.
//
// THE NARROWING AND THE ARCHIVAL TERM ARE BUILT APART, because the census is
// the listing's question MINUS the archival term and the rows are it WITH one.
// Folded into one clause they could not both be asked.
func readProjectRows(ctx context.Context, tx *sql.Tx, q ProjectQuery,
	limit int) ([]ProjectRow, ProjectCensus, error) {

	// narrowing is `q` and `unit`: what the caller asked ABOUT, as opposed
	// to which archival set they asked FOR.
	narrowing := []string{}
	var args []any
	// EITHER SPELLING OF THE UNIT, through the chart — see [unitSpellings].
	// A project row is chart-owned and rewritten by the next epoch apply,
	// so it holds the current key within a beat of one being added; the
	// set is what keeps the filter answering in the beat before that, and
	// what lets a caller filter by the name a screen showed them.
	//
	// IN THE NARROWING, so the CENSUS is cut by the same spellings as the
	// rows: the two numbers beside a directory are the same question asked
	// of both archival sets, and a census narrowed by one spelling beside
	// rows narrowed by two would count what it did not list.
	if spellings := unitSpellings(q.Units, []string{q.Unit}); len(spellings) > 0 {
		narrowing = append(narrowing, "p.unit IN ("+placeholders(len(spellings))+")")
		args = append(args, anyOf(spellings)...)
	}
	if term := strings.TrimSpace(q.Q); term != "" {
		// THE KEY, THE NAME AND THE PURPOSE, because a filter box is
		// typed into by somebody who remembers one of the three. LIKE
		// with the term escaped, which is what [likeEscape] is for.
		narrowing = append(narrowing, `(p.key LIKE ? ESCAPE '\' OR p.name LIKE ? `+
			`ESCAPE '\' OR p.purpose LIKE ? ESCAPE '\')`)
		pattern := "%" + likeEscape(term) + "%"
		args = append(args, pattern, pattern, pattern)
	}
	narrowed := ""
	if len(narrowing) > 0 {
		narrowed = " WHERE " + joinAnd(narrowing)
	}

	// ONE AGGREGATE FOR BOTH SETS, grouped on the column that divides them
	// — and it replaces the `COUNT(*)` that answered `total` alone, so the
	// listing is no more queries than it was.
	census, err := readProjectCensus(ctx, tx, narrowed, args)
	if err != nil {
		return nil, ProjectCensus{}, err
	}

	// EXACTLY THE ASKED SET. [ArchivedInclude] is the only mode with no
	// term, because it is the only one asking for both.
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
		return nil, ProjectCensus{}, fmt.Errorf("tracker: read the projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// AN ANSWER'S LIST IS NEVER NIL, and that is a wire property rather
	// than a taste: [ProjectListing.Projects] carries no `omitempty`, so a
	// nil slice here encodes as `"projects": null` and the only client of
	// this answer reads `projects?.length === 0` to decide whether the
	// company has any. On a company with none that test was `undefined ===
	// 0` — false — so the landing screen drew the ordinary list instead of
	// the panel that says where a project comes from, and did it on exactly
	// the day-one state the panel exists for. Every fixture said `[]`,
	// because a fixture is written by somebody who knows the answer.
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
			return nil, ProjectCensus{}, fmt.Errorf("tracker: scan a project: %w", err)
		}
		row.Archived = archived != 0
		row.TargetDate = target.String
		row.LastChange = change.value()
		row.Unit, row.Lead = resolveUnit(q.Units, unit)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, ProjectCensus{}, fmt.Errorf("tracker: read the projects: %w", err)
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

// readProjectCensus counts both archival sets under one narrowing.
//
// GROUPED RATHER THAN TWO COUNTS, so the two halves are read in one pass over
// one set of rows and cannot be taken from different snapshots — which,
// inside the read's own transaction, is a property this does not have to think
// about and would lose the moment somebody moved one of them.
func readProjectCensus(ctx context.Context, tx *sql.Tx, narrowed string,
	args []any) (ProjectCensus, error) {

	rows, err := tx.QueryContext(ctx,
		`SELECT p.archived, COUNT(*) FROM tracker_projects p`+narrowed+
			` GROUP BY p.archived`, args...)
	if err != nil {
		return ProjectCensus{}, fmt.Errorf("tracker: count the projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var census ProjectCensus
	for rows.Next() {
		var archived, count int
		if err := rows.Scan(&archived, &count); err != nil {
			return ProjectCensus{}, fmt.Errorf("tracker: scan the project census: %w", err)
		}
		// ANY NON-ZERO IS ARCHIVED, which is how every other reader of
		// this column reads it — the schema defaults it to 0 and the
		// applier writes `boolInt`, but a column is not a bool and a
		// reader that tested `== 1` would drop a row nobody can see.
		if archived != 0 {
			census.Archived += count
			continue
		}
		census.Active += count
	}
	if err := rows.Err(); err != nil {
		return ProjectCensus{}, fmt.Errorf("tracker: count the projects: %w", err)
	}
	return census, nil
}

// projectOrderBy is the ordering clause one query compiles to.
//
// IT IS BUILT FROM THE ENUM AND NEVER FROM A STRING, and the caller has
// already been refused if the key is not one — [Reader.Projects] is the one
// entry point and it checks before it reads.
func projectOrderBy(q ProjectQuery) string {
	column, ok := projectSortColumns[q.Sort]
	if !ok {
		// AN ABSENT ORDERING IS THE KEY, which is what the tiebreak
		// below is anyway — so this arm emits `p.key` once rather than
		// twice.
		return "p.key"
	}
	direction := ""
	if q.Descending {
		direction = " DESC"
	}
	clause := column + direction
	if q.Sort == ProjectSortLastChange || q.Sort == ProjectSortTarget {
		// AN ABSENT VALUE SORTS LAST IN BOTH DIRECTIONS. SQLite orders
		// NULL first ascending and last descending, so the default would
		// open a newest-first directory with every project nothing has
		// been filed into, and a soonest-first one with every project
		// nobody gave a target — and "nothing recorded" is not the
		// smallest value, it is not a value. It is the rule the grid
		// drawing these rows already states for the same columns.
		clause = column + " IS NULL, " + clause
	}
	if q.Sort == ProjectSortKey {
		return clause
	}
	// THE TIEBREAK, so two projects level on the sorted column come back
	// in one stable order. Without it the planner's walk decides, and a
	// directory reshuffles its equal rows between two identical polls.
	return clause + ", p.key"
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
	out.TargetDate = project.TargetDate
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
		SELECT `+censusColumns+`, last_change_at,
		       last_change_actor, last_change_actor_kind
		FROM tracker_projects
		WHERE key = ?`, key).Scan(&out.Counts.Todo, &out.Counts.Active,
		&out.Counts.Done, &out.Counts.Closed, &change.At, &change.Actor,
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
			forType, strings.Join(typeSlugs(out.Types), ", "), ErrNoType)
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
