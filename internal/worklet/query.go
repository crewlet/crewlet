package worklet

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ONE GRAMMAR, parsed once: the board, a seat's `list_tasks`, a saved view and
// the dashboard's URL hash are parsed by this one function into one struct, so
// no filter is honoured on one path and ignored on another (a saved view
// silently answering a different question from the screen that saved it).
//
// THERE IS NO QUERY LANGUAGE. Every key is a flat parameter, so a model writes
// one without a parser in its head and a URL carries one without escaping. The
// cost is expressiveness, paid at the `any` key: one level of disjunction,
// capped, never nested.

// The limits a query is clamped or refused by.
const (
	// MaxAnyBranches caps the disjunction: an N-way OR across indexes is the
	// shape the planner handles worst (it cannot drive from one index and has
	// to union or scan), and eight is the widest any view in this model needs.
	MaxAnyBranches = 8

	// MaxQueryText bounds the FIND ([Query.Text]) at the longest title: a find
	// is a substring of a key or a title, so nothing longer can match. Refused
	// past it rather than run as a LIKE certain to match nothing, because an
	// empty board reads as "there is no such item" where a refusal names the
	// bound.
	MaxQueryText = MaxTitle

	// MaxTotals caps the aggregate list, because every entry is another
	// pass over the same rows.
	MaxTotals = 8

	// ActivityQuerySpanDays bounds a text search over the activity feed.
	//
	// The gate is on what the query would SCAN, not on which key is present:
	// `container=workspace` or a five-year `since:` narrow nothing. `q=` is an
	// escaped LIKE over `excerpt`, which no index on `tracker_history` covers,
	// so unscoped it reads every commit the company ever made and busts a
	// five-second read budget inside the first year. A project and a ninety-day
	// window (a quarter, the span such a question is asked about) make it a
	// range over `(project_key, log_seq DESC)`. Routing `q=` through the lexical
	// index would be a second search surface for a facet nobody ranks, and
	// DEFAULTING a window would silently answer a different question. See
	// [Reader.Activity].
	ActivityQuerySpanDays = 90

	// GroupByRowCeiling is the row count at which a grouped answer's sort
	// working set crosses the page cache and spills. The gate runs a BOUNDED
	// COUNT first and refuses past it, because a window function's
	// materialisation cannot be pushed down: the whole partitioned, sorted
	// input exists before the per-group limit applies.
	GroupByRowCeiling = 20_000
)

// Scope is what a query is scoped to — the container a board is drawn over.
type Scope struct {
	// Workspace is the Everything level, and it is EXPLICIT — never
	// implied by an absent container, because "everything" and "you did not
	// say" are answers a caller acts on differently.
	Workspace bool

	// Project is the project key when the scope is one project.
	Project string
}

// TagMode is how a tag list combines.
type TagMode string

// The three tag modes: `any` (at least one of the tags), `all` (every one)
// and `none` (no member of the set) — a filter over the ABSENT rather than a
// negation of `any`, and how "untagged work in this project" is asked.
const (
	TagAny  TagMode = "any"
	TagAll  TagMode = "all"
	TagNone TagMode = "none"
)

// TagFilter is a tag list and how it combines.
type TagFilter struct {
	Mode TagMode
	Tags []string
}

// NumOp is a comparison against a numeric column.
type NumOp string

// The numeric comparisons, spelled as words because `<` and `>` would need
// escaping in a query string by every caller that builds one.
const (
	NumLT      NumOp = "lt"
	NumLTE     NumOp = "lte"
	NumGT      NumOp = "gt"
	NumGTE     NumOp = "gte"
	NumRange   NumOp = "range"
	NumNull    NumOp = "null"
	NumNotNull NumOp = "not_null"
)

// NumFilter is one numeric predicate.
type NumFilter struct {
	Op       NumOp
	From, To float64
}

// FieldFilter is one custom-field predicate, left as the raw op and value
// because what is legal depends on the field's declared type, which the
// compiler has and the parser does not.
type FieldFilter struct {
	// Ref is the slug or the id the caller typed, unresolved: resolution
	// needs the catalogues, and a parser that read them could fail on a store.
	Ref   string
	Op    string
	Value string
}

// SubtaskMode is how a subtree is filtered.
type SubtaskMode string

const (
	// SubtasksCollapsed hides a subtree behind its root. It and
	// SubtasksExpanded filter each task's ANCHOR and let what hangs from it
	// ride along unfiltered — live, a tree's root; in the trash, the removal
	// that took the task ([anchorOf]), so a task removed alone is listed
	// alone. SubtasksSeparate filters every task on its own.
	SubtasksCollapsed SubtaskMode = "collapsed"

	// SubtasksExpanded shows a subtree under the root it rides along with,
	// unfiltered — see [SubtasksCollapsed].
	SubtasksExpanded SubtaskMode = "expanded"

	// SubtasksSeparate files every task on its own row, each filtered in
	// its own right.
	SubtasksSeparate SubtaskMode = "separate"
)

// ShowClosed is how finished work is included.
//
// THREE WAYS TO ASK, at most one per query: every finished task, those
// finished within a DURATION of now (`show_closed=recent:168h`), and those
// finished at or after a CALENDAR BOUNDARY (`closed_since=sow`). The last two
// differ: "the last seven days" slides with the clock, while "this week"
// starts at Monday's midnight in the company's zone — the Board's Recent
// scope, a Done lane that empties at once rather than one task an hour.
type ShowClosed struct {
	All bool

	// Recent bounds the finished set by age, over the WHOLE finished set (the
	// group stamp), so a task cancelled inside the window is in as one done is.
	Recent time.Duration

	// Since is the same bound as an INSTANT, resolved by [ParseQuery] from a
	// date token on the company's clock, never the reader's: `closed_since=sow`
	// is the first instant of this ISO week. THE ZERO INSTANT IS UNSET, and the
	// parse refuses a `closed_since` resolving to it (that is every finished
	// task, `show_closed=true`). Not a pointer whose zero instant means that:
	// encoding/json's omitzero asks time.Time's IsZero through the pointer, so
	// one would cross the estate's wire as no bound, leaving out every finished
	// task.
	Since time.Time
}

// Finished reports whether any finished work is in the answer — the one
// question [compilePredicate] and [admittedStatuses] both ask of this, stated
// once so a fourth way to ask cannot reach one of them and not the other.
func (s ShowClosed) Finished() bool {
	return s.All || s.Recent > 0 || !s.Since.IsZero()
}

// ArchivedMode is how archived work is included.
type ArchivedMode string

// The three archived modes, spelled `false` / `true` / `only` on the wire
// because the parameter reads as a boolean; `only` makes "what did we retire"
// a query rather than a scan a caller filters afterwards.
const (
	ArchivedExclude ArchivedMode = "false"
	ArchivedInclude ArchivedMode = "true"
	ArchivedOnly    ArchivedMode = "only"
)

// ArchivedModes is every mode, in the order a refusal names them.
var ArchivedModes = []ArchivedMode{ArchivedExclude, ArchivedInclude, ArchivedOnly}

// ArchivedModeNames is the same list as wire strings, for the surfaces that
// have to quote it back at a caller who spelled one wrong.
func ArchivedModeNames() []string {
	out := make([]string, len(ArchivedModes))
	for i, m := range ArchivedModes {
		out[i] = string(m)
	}
	return out
}

// Valid reports whether this is one of the three modes.
//
// THE ZERO VALUE IS NOT ONE, deliberately: "" is a caller that has not
// answered a three-way question, so every surface either resolves the absence
// to its own default ([Query.parseArchived], over the one [ParseQuery] sets)
// or refuses it by name ([Reader.Projects], which builds its query from a
// struct literal and has no constructor to carry one).
func (m ArchivedMode) Valid() bool { return slices.Contains(ArchivedModes, m) }

// Sort is one ordering term.
type Sort struct {
	Key        string
	Descending bool
}

// Query is one parsed question.
//
// EVERY FIELD IS RESOLVED OR REFUSED, so nothing downstream re-parses a
// string — except two, marked at the field: a custom field's op needs the
// catalogue, and the text query is the searcher's own input.
type Query struct {
	Scope Scope

	// View and Preset are loaded FIRST and explicit keys override them, so
	// a saved view is a set of defaults rather than a lock.
	View   string
	Preset string

	Status       []Status
	StatusNot    []Status
	StatusGroups []StatusGroup

	Assignee     []string
	Collaborator []string
	// AssigneeNot is `assignee=!<handle>`: the work that handle does NOT
	// hold, `!none` the work somebody does (negated as `status=` is), so a
	// person's followed work can leave out what their own queue already holds.
	AssigneeNot       []string
	Reporter          []string
	Watcher           []string
	ChecklistAssignee []string

	Unit        []string
	RoutingUnit []string

	// Units is the chart the two keys above resolve through, so a unit named
	// by id or name finds the work filed under its key ([Units], [unitKeys]).
	// Nil, for a surface holding no chart, matches what the caller typed.
	//
	// NOT PART OF THE GRAMMAR: it is the surface's own chart, set beside the
	// read level it resolves, so [ParseQuery] neither reads nor refuses a key
	// for it. NOT SERIALISED: a JSON encoder writes the interface as `{}` and
	// cannot decode it, so a wire's far side attaches ITS OWN chart (see
	// internal/estate).
	Units Units `json:"-"`

	Tags TagFilter

	Types      []string
	Priorities []Priority

	// PriorityListOf is whose stored priority list to narrow to — the
	// ordered ids on a Person, not the priority ENUM above. Named for the
	// API's own word (`Person.Priorities`, `update_priorities`), so one word
	// does not mean two things across it.
	PriorityListOf string

	// PriorityList is that list, RESOLVED inside the read's own transaction:
	// it lives on another object, and a parser that read it could fail on a
	// store.
	PriorityList []string

	Parent   string
	Root     string
	Subtasks SubtaskMode

	// Keys narrows to the task keys a caller already holds (pasted, linked):
	// a LIST, because eight pasted keys as eight reads is eight round trips.
	Keys []string

	// Removed is the TRASH, three-valued like every tombstone filter here:
	// absent excludes removed work (every board), `true` asks for ONLY it
	// (the restore surface, how [OpRestore] is reached), and `false` is the
	// exclusion said out loud. The trash is read through its own indexes
	// ([taskSource]); in the default subtask mode it lists each REMOVAL with
	// what it took riding along ([anchorOf]).
	Removed *bool

	// Dates are keyed by the column they filter, so a compiler walks them
	// rather than switching on eight named fields.
	Dates map[string]DateFilter

	Estimate *NumFilter
	Points   *NumFilter
	Spend    *NumFilter

	Fields []FieldFilter

	Blocked         *bool
	Blocking        *bool
	HasDependencies *bool
	HasChildren     *bool
	HasParent       *bool
	HasOpenAsks     *bool
	AskedOf         string

	// AskedBy is whose open questions to narrow to — the work a person is
	// WAITING on — by the asker's record name alone: a person bound to a seat
	// writes AS that seat whatever credential they hold (iam.ActorFor), so
	// their token's asks and their dashboard's carry the same author.
	AskedBy string

	LinkedPage string
	References string

	// Any is one level of disjunction, ANDed with the top-level keys.
	Any []Query

	// Text is a substring of a key or a title — the FIND, not a search.
	// See [Query.parseText] for why there is no mode beside it.
	Text string

	ShowClosed ShowClosed
	Archived   ArchivedMode
	Flags      []string

	GroupBy    string
	GroupBy2   string
	GroupLimit int

	// Group narrows a board to ONE column and Subgroup to one lane of it,
	// which is how a board loads a column further: a grouped answer mints no
	// cursor.
	//
	// POINTERS, because the empty string is a VALUE on every axis: the
	// absent-value column ("nobody is assigned", "untagged", "no due date")
	// is one a board draws, and `group=` loads it. As plain strings, that
	// request would answer the WHOLE board instead. [Params.Has] tells "named
	// and empty" from "absent", the rule `blocked=` is read by.
	Group    *string
	Subgroup *string

	Sort []Sort

	Totals []string

	// RowFields are the OPT-IN row facts asked for with `fields=`
	// ([RowField]); empty when none were named, which keeps [TaskRow] the
	// shape a seat's `list_work_items` reads.
	RowFields []RowField

	// Around is one task (id, key or former key, resolved as a task page
	// resolves its address) whose place in THIS answer's drawing order the
	// caller wants: `around=ENG-7` lets a task page opened from a board say
	// "3 of 18" and step to its neighbours without holding the board. Empty
	// is not asked. See [Around].
	Around string

	// DayStart is midnight today in the COMPANY's zone, resolved once by
	// [ParseQuery] so every answer about "today" agrees: the row's `overdue`
	// flag and the `due=overdue` filter both derive from it, never from the
	// reader's own `time.Now()`, which would put them on two clocks.
	DayStart time.Time

	// DayEnd and WeekEnd are when today and its Monday-anchored week end: the
	// `due:bucket` grouping's cuts between Today, This week and Later.
	//
	// RESOLVED HERE by [period.Resolve] (its `tomorrow` and `eow`), not
	// derived from DayStart in SQL: across a daylight-saving transition a day
	// is not 24 hours, so `DayStart + 86 400 s` would cut a band disagreeing
	// with the `due=range:today..tomorrow` filter that means the same thing.
	DayEnd  time.Time
	WeekEnd time.Time

	Level statelog.ReadLevel

	// MaxLag is the bound a `stale` read declares it will accept; zero
	// accepts anything. CARRIED, not merely validated: a bound checked and
	// then dropped serves a tile declaring twenty seconds an answer of any
	// age, rendered as live.
	MaxLag time.Duration

	// MaxLagSeq is the same bound counted in RECORDS, carried for the same
	// reason. A record count is what the broker measures (a duration is
	// derived from it through this node's drain rate), so it names the
	// measured quantity rather than an estimate. Both may be set; the read
	// refuses past whichever is reached first.
	MaxLagSeq uint64

	// Session is the caller's own high-water mark on this domain's log,
	// which is what a `session` read waits through. Zero is a caller that
	// has written nothing, for whom every level below linearizable is the
	// same answer.
	Session statelog.Position

	// Floors are the floors a caller named with `min_position` — the
	// position a wake carried, or one its own previous write returned —
	// and the answer is served from no earlier than the one on this log,
	// at every level. A floor on another log is refused by the read
	// ([statelog.Query.Floors]), never dropped.
	Floors []statelog.Position

	Limit  int
	Cursor string
}

// Params is the request's own parameters, as this parser reads them.
//
// DECLARED BY THE CONSUMER, kept to the methods the grammar calls. The
// concrete bag lives with the API surface that fills it; importing that here
// would make the tracker depend on its transport, and cycle once the surface
// reads a tracker type.
type Params interface {
	// String is one value, empty when absent.
	String(key string) string

	// List is every value a key that may REPEAT was given ([RepeatedKeys]),
	// in order: each of a query string's values, each string of a JSON
	// array, and one value given plainly as a list of one. False is a value
	// that is neither a string nor a list of strings, which the grammar
	// refuses by the key's name rather than reading as absent.
	List(key string) ([]string, bool)

	// Int and Bool take a default, because "absent" and "zero" are
	// different answers for both: `limit=0` is a caller asking for
	// nothing, and an absent limit is a caller asking for the default.
	Int(key string, def int) int
	Bool(key string, def bool) bool

	// Has distinguishes them, which is what makes `open=false` and no
	// `open` at all two different questions.
	Has(key string) bool

	// Keys is every parameter named, so an unknown one is REFUSED rather
	// than ignored ([QueryKeys]).
	Keys() []string
}

// QueryKeys is every parameter this grammar reads, sorted.
//
// IT EXISTS SO AN UNKNOWN KEY IS REFUSED: a filter nobody parsed silently
// widens the answer, and an ignored `open=true` looks exactly like a project
// where nothing is finished. One LIST rather than a check per parser, because
// the refusal runs before any of them, so a key misspelled into a neighbour's
// territory is named unknown rather than swallowed. The one shape not in it is
// a custom field, `f.<ref>`, whose refs are a company's own
// ([Query.parseFields]).
var QueryKeys = []string{
	"any", "archived", "archived_at", "around", "asked_by", "asked_of", "assignee",
	"blocked", "blocking", "checklist_assignee", "closed",
	"closed_since", "collaborator",
	"container", "created", "cursor", "done", "due", "estimate",
	"fields", "finished", "flag", "group", "group_by", "group_by2",
	"group_limit", "has_children", "has_dependencies", "has_open_asks",
	"has_parent", "key", "limit", "linked_page",
	"max_lag_seconds", "max_lag_seq", "min_position", "parent", "points", "preset",
	"priorities", "priority", "q", "read_level", "references", "removed",
	"reporter",
	"root", "routing_unit", "show_closed", "sort", "spend_tokens",
	"start", "status", "status_entered", "status_group", "subgroup",
	"subtasks", "tag", "totals", "type", "unit", "updated", "view",
	"watcher",
}

// RepeatedKeys are the parameters a request may carry MORE THAN ONCE, read as
// every value given ([Params.List]): `min_position`, one floor per log an
// answer is read from ([statelog.ParseFreshness]).
//
// Only these: a transport hands a scalar reader a repeated key's first value,
// so it keeps every value of exactly these.
var RepeatedKeys = []string{"min_position"}

// FieldKeyPrefix is what makes a parameter a custom-field filter.
const FieldKeyPrefix = "f."

// checkKeys refuses a parameter nothing in this grammar reads.
func checkKeys(p Params) error {
	for _, key := range p.Keys() {
		if ref, ok := strings.CutPrefix(key, FieldKeyPrefix); ok && ref != "" {
			continue
		}
		if slices.Contains(QueryKeys, key) {
			continue
		}
		// THE REFUSAL NAMES THE KEY AND NOTHING ELSE: sixty-odd
		// parameters is a wall nobody reads, and the schema and docs hold
		// the set; `f.` is named as the one shape neither shows.
		return fmt.Errorf("worklet: %q is not a query parameter; a filter "+
			"nothing parses would silently widen this answer, so it is "+
			"refused rather than ignored; a custom field is %s<ref>",
			key, FieldKeyPrefix)
	}
	return nil
}

// ParseQuery reads one flat parameter map into one query.
//
// `now` and `loc` are ARGUMENTS rather than reads: every relative date
// resolves on the company's own clock, and a parser reading a package clock
// could not be tested at a boundary, which half this grammar's tokens are.
func ParseQuery(p Params, now time.Time, loc *time.Location) (Query, error) {
	// FIRST, so a misspelling is named as unknown rather than reported by
	// whichever parser its neighbour belongs to.
	if err := checkKeys(p); err != nil {
		return Query{}, err
	}
	// THE DAY BOUNDARY THIS QUERY IS ABOUT, resolved once and carried
	// ([Query.DayStart]): `due=overdue`, `preset=overdue` and the row's own
	// `overdue` flag all derive from this one value.
	dayStart, err := period.Resolve("today", now, loc)
	if err != nil {
		return Query{}, err
	}
	// AND THE TWO BOUNDARIES THAT DAY AND ITS WEEK END ON, from the same
	// calendar in the same call — see [Query.DayEnd].
	dayEnd, err := period.Resolve("tomorrow", now, loc)
	if err != nil {
		return Query{}, err
	}
	weekEnd, err := period.Resolve("eow", now, loc)
	if err != nil {
		return Query{}, err
	}
	q := Query{
		Subtasks:   SubtasksCollapsed,
		Archived:   ArchivedExclude,
		DayStart:   dayStart.At,
		DayEnd:     dayEnd.At,
		WeekEnd:    weekEnd.At,
		Dates:      map[string]DateFilter{},
		View:       p.String("view"),
		Preset:     p.String("preset"),
		Cursor:     p.String("cursor"),
		LinkedPage: p.String("linked_page"),
		// UPPERCASED AS `key` IS: `references=` names a task KEY,
		// compared exactly against the upper-case
		// `tracker_task_keys.key`, so `references=eng-7` would answer
		// empty where `key=eng-7` resolves.
		References: ProjectKey(p.String("references")),
		AskedOf:    p.String("asked_of"),
		// THE LIST, not the enum. See [Query.PriorityListOf].
		PriorityListOf: strings.TrimSpace(p.String("priorities")),
		AskedBy:        strings.TrimSpace(p.String("asked_by")),
		Parent:         p.String("parent"),
		Root:           p.String("root"),
	}

	if err := q.parseScope(p); err != nil {
		return Query{}, err
	}
	if err := q.parseStatus(p); err != nil {
		return Query{}, err
	}
	for _, handle := range csv(p.String("assignee")) {
		if rest, negated := strings.CutPrefix(handle, "!"); negated {
			if rest == "" {
				return Query{}, fmt.Errorf("worklet: assignee=! names nobody to " +
					"leave out; it is !<handle>, or !none for the work somebody holds")
			}
			q.AssigneeNot = append(q.AssigneeNot, rest)
			continue
		}
		q.Assignee = append(q.Assignee, handle)
	}
	q.Collaborator = csv(p.String("collaborator"))
	q.Reporter = csv(p.String("reporter"))
	q.Watcher = csv(p.String("watcher"))
	q.ChecklistAssignee = csv(p.String("checklist_assignee"))
	q.Unit = csv(p.String("unit"))
	q.RoutingUnit = csv(p.String("routing_unit"))
	q.Types = csv(p.String("type"))
	q.Flags = csv(p.String("flag"))
	// KEYS ARE UPPERCASED, because a key is what somebody pasted and
	// `eng-7` is the same task as `ENG-7`. The project half is minted
	// upper-case, so lowering the column instead would defeat
	// `tracker_tasks_key_idx`.
	for _, key := range csv(p.String("key")) {
		q.Keys = append(q.Keys, ProjectKey(key))
	}
	if p.Has("removed") {
		removed := p.Bool("removed", true)
		q.Removed = &removed
	}

	if err := q.parseTags(p); err != nil {
		return Query{}, err
	}
	if err := q.parsePriorities(p); err != nil {
		return Query{}, err
	}
	if err := q.parseSubtasks(p); err != nil {
		return Query{}, err
	}
	if err := q.parseDates(p, now, loc); err != nil {
		return Query{}, err
	}
	if err := q.parseNumbers(p); err != nil {
		return Query{}, err
	}
	q.parseFields(p)
	q.parseBools(p)
	if err := q.parseText(p); err != nil {
		return Query{}, err
	}
	if err := q.parseShowClosed(p, now, loc); err != nil {
		return Query{}, err
	}
	if err := q.parseArchived(p); err != nil {
		return Query{}, err
	}
	if err := q.parseGrouping(p); err != nil {
		return Query{}, err
	}
	if err := q.parseSort(p); err != nil {
		return Query{}, err
	}
	if err := q.parseTotals(p); err != nil {
		return Query{}, err
	}
	if err := q.parseRowFields(p); err != nil {
		return Query{}, err
	}
	q.Around = strings.TrimSpace(p.String("around"))
	if err := q.parseLevel(p); err != nil {
		return Query{}, err
	}
	if err := q.parseAny(p, now, loc); err != nil {
		return Query{}, err
	}
	q.Limit = p.Int("limit", 0)
	return q, nil
}

// parseScope reads the container.
//
// AN ABSENT CONTAINER IS NEITHER the workspace nor a project: the caller
// resolves it from its surface (a seat's own projects, a human's everything).
// Defaulting to the workspace would make an omitted key the most expensive
// query in the system.
func (q *Query) parseScope(p Params) error {
	value := strings.TrimSpace(p.String("container"))
	switch {
	case value == "":
		return nil
	case value == "workspace":
		q.Scope.Workspace = true
		return nil
	}
	key := value
	if rest, ok := strings.CutPrefix(value, "project:"); ok {
		key = rest
	} else if kind, _, named := strings.Cut(value, ":"); named {
		// ANOTHER CONTAINER KIND IS REFUSED BY NAME, not read as a project
		// key (`container=unit:eng` would be the impossible project
		// `UNIT:ENG` and answer an empty list). A task belongs to no unit or
		// person; the grammar's own keys select their work.
		return fmt.Errorf("worklet: container %q is not a task container; "+
			"a task lives in the workspace or a project (container=workspace, "+
			"container=project:<KEY>); a team's work is unit=<name> and a "+
			"person's is assignee=<handle>, not container=%s:…", value, kind)
	}
	if key == "" {
		return fmt.Errorf("worklet: container %q names no project", value)
	}
	// UPPER-CASED: a project key is stored upper and the scope is an EXACT
	// compare against `project_key`, so `project:eng` would answer an empty
	// list, which a person acts on as missing work.
	q.Scope.Project = ProjectKey(key)
	return nil
}

// parseStatus reads the status and status_group keys, with `!` negation.
func (q *Query) parseStatus(p Params) error {
	for _, value := range csv(p.String("status")) {
		negated := strings.HasPrefix(value, "!")
		slug := Status(strings.TrimPrefix(value, "!"))
		if !slug.Valid() {
			return fmt.Errorf("worklet: %q is not one of this company's six "+
				"statuses — there is no per-container set and no cancelled= key, "+
				"because cancelled IS a status", value)
		}
		if negated {
			q.StatusNot = append(q.StatusNot, slug)
			continue
		}
		q.Status = append(q.Status, slug)
	}
	for _, value := range csv(p.String("status_group")) {
		group := StatusGroup(value)
		if !group.Valid() {
			return fmt.Errorf("worklet: %q is not one of the four status groups",
				value)
		}
		q.StatusGroups = append(q.StatusGroups, group)
	}
	return nil
}

// parseTags reads the tag filter and its mode.
func (q *Query) parseTags(p Params) error {
	value := strings.TrimSpace(p.String("tag"))
	if value == "" {
		return nil
	}
	mode := TagAny
	if head, rest, found := strings.Cut(value, ":"); found {
		switch TagMode(head) {
		case TagAny, TagAll, TagNone:
			mode, value = TagMode(head), rest
		default:
			return fmt.Errorf("worklet: %q is not a tag mode; the three "+
				"are `any:`, `all:` and `none:`", head)
		}
	}
	// EVERY TAG IS NORMALISED AS A WRITE NORMALISES IT ([tagSlugs]): the
	// rows hold slugs compared exactly, so `tag=Regression` as typed would
	// match nothing and read as "no work carries this tag". A value that
	// normalises to no slug gets the write's own refusal; a duplicate is
	// named once.
	tags, err := tagSlugs(csv(value))
	if err != nil {
		return err
	}
	q.Tags = TagFilter{Mode: mode, Tags: tags}
	return nil
}

func (q *Query) parsePriorities(p Params) error {
	for _, value := range csv(p.String("priority")) {
		priority := Priority(value)
		if !priority.Valid() {
			return fmt.Errorf("worklet: %q is not one of the five priorities", value)
		}
		q.Priorities = append(q.Priorities, priority)
	}
	return nil
}

func (q *Query) parseSubtasks(p Params) error {
	switch value := SubtaskMode(p.String("subtasks")); value {
	case "":
	case SubtasksCollapsed, SubtasksExpanded, SubtasksSeparate:
		q.Subtasks = value
	default:
		return fmt.Errorf("worklet: %q is not a subtask mode; the three are "+
			"collapsed, expanded and separate", value)
	}
	return nil
}

// dateKeys are the nine columns a date filter may name.
//
// `status_entered` reads the EFFECTIVE instant because it starts a duration;
// the other eight read the AUTHORED one, a wall-clock bound the caller typed
// that a clamp would change. `archived_at` is spelled in full because
// `archived` is already the three-valued MODE ([ArchivedMode]).
var dateKeys = []string{
	"due", "start", "created", "updated", "done", "closed", "finished",
	"archived_at", "status_entered",
}

func (q *Query) parseDates(p Params, now time.Time, loc *time.Location) error {
	for _, key := range dateKeys {
		value := strings.TrimSpace(p.String(key))
		if value == "" {
			continue
		}
		filter, err := ParseDateFilter(value, now, loc)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		q.Dates[key] = filter
	}
	return nil
}

func (q *Query) parseNumbers(p Params) error {
	for key, target := range map[string]**NumFilter{
		"estimate": &q.Estimate,
		"points":   &q.Points,
		// NAMED AFTER THE COLUMN, as the sort beside it is — see
		// [sortKeys] for why `spend` alone stopped naming one number.
		"spend_tokens": &q.Spend,
	} {
		value := strings.TrimSpace(p.String(key))
		if value == "" {
			continue
		}
		filter, err := parseNumFilter(value)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*target = &filter
	}
	return nil
}

func parseNumFilter(value string) (NumFilter, error) {
	switch NumOp(value) {
	case NumNull, NumNotNull:
		return NumFilter{Op: NumOp(value)}, nil
	}
	op, rest, found := strings.Cut(value, ":")
	if !found {
		return NumFilter{}, fmt.Errorf("worklet: %q names no comparison; a "+
			"numeric filter is lt:, lte:, gt:, gte:, range:a..b, null or "+
			"not_null", value)
	}
	switch NumOp(op) {
	case NumLT, NumLTE, NumGT, NumGTE:
		n, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return NumFilter{}, fmt.Errorf("worklet: %q is not a number", rest)
		}
		return NumFilter{Op: NumOp(op), From: n}, nil
	case NumRange:
		from, to, ok := strings.Cut(rest, "..")
		if !ok {
			return NumFilter{}, fmt.Errorf("worklet: a range is a..b and %q "+
				"carries no separator", rest)
		}
		lo, err := strconv.ParseFloat(from, 64)
		if err != nil {
			return NumFilter{}, fmt.Errorf("worklet: %q is not a number", from)
		}
		hi, err := strconv.ParseFloat(to, 64)
		if err != nil {
			return NumFilter{}, fmt.Errorf("worklet: %q is not a number", to)
		}
		if hi < lo {
			return NumFilter{}, fmt.Errorf("worklet: the range %q ends below "+
				"where it starts, so it matches nothing", rest)
		}
		return NumFilter{Op: NumRange, From: lo, To: hi}, nil
	}
	return NumFilter{}, fmt.Errorf("worklet: %q is not a numeric comparison", op)
}

// parseFields collects the custom-field predicates, LEFT UNRESOLVED: which
// ops a field admits depends on its declared type in a catalogue, and a parser
// that read one could fail on a store and could not run inside a transaction.
func (q *Query) parseFields(p Params) {
	for _, key := range p.Keys() {
		ref, ok := strings.CutPrefix(key, FieldKeyPrefix)
		if !ok || ref == "" {
			continue
		}
		value := strings.TrimSpace(p.String(key))
		// `null` AND `not_null` ARE THE WHOLE VALUE, as in a numeric
		// filter: they take no operand. Read as `eq` against that text they
		// would be refused by a number column and silently match the
		// literal text on a text one.
		switch value {
		case FieldOpNull, FieldOpNotNull, FieldOpMe:
			// `me` too: its operand is the VIEWER, which [Reader.Expand]
			// substitutes first (where `preset=my_queue` resolves it), so
			// one reaching here is a surface that did not expand, and
			// [checkFieldOp] says so.
			q.Fields = append(q.Fields, FieldFilter{Ref: ref, Op: value})
			continue
		}
		// A COLON SEPARATES THE OPERATOR, EXCEPT BEFORE `//`: a URL's
		// scheme is no operator (`f.homepage=https://x`). Any other unknown
		// prefix is still carried as an operator, so a TYPO is refused
		// naming the set rather than answered as a value nobody holds.
		//
		// A BARE VALUE GETS NO OPERATOR rather than `eq`: what no operator means
		// depends on the field's TYPE (on a set it is `any`), which this
		// parser cannot see.
		op, rest := "", value
		if head, tail, found := strings.Cut(value, ":"); found &&
			!strings.HasPrefix(tail, "//") {
			op, rest = head, tail
		}
		q.Fields = append(q.Fields, FieldFilter{Ref: ref, Op: op, Value: rest})
	}
	// THE ORDER IS STABLE BECAUSE Keys IS SORTED, there rather than here:
	// every reader that enumerates parameters needs the same guarantee.
}

func (q *Query) parseBools(p Params) {
	for key, target := range map[string]**bool{
		"blocked":          &q.Blocked,
		"blocking":         &q.Blocking,
		"has_dependencies": &q.HasDependencies,
		"has_children":     &q.HasChildren,
		"has_parent":       &q.HasParent,
		"has_open_asks":    &q.HasOpenAsks,
	} {
		if !p.Has(key) {
			continue
		}
		value := p.Bool(key, true)
		*target = &value
	}
}

// parseText reads the FIND, which is not a search.
//
// `q` is a substring of a key or a title, "the item I half remember", and
// nothing more. RANKED SEARCH over what the work says is [Searcher]'s
// (`search_work_items`, `work_search` on the API); this grammar is a filter
// composed into the board query, which a ranking cannot be (see search.go).
// So there is no `mode`: a knob whose values cannot differ would answer a
// caller asking for `semantic` with a substring match.
func (q *Query) parseText(p Params) error {
	q.Text = strings.TrimSpace(p.String("q"))
	if len(q.Text) > MaxQueryText {
		return fmt.Errorf("worklet: the find text is %d bytes and the bound is "+
			"%d — this is a substring of a key or a title, and no title is "+
			"longer, so a longer one matches nothing; to find work by what it "+
			"is about, search it ("+SearchWorkItemsTool+", or work_search)",
			len(q.Text), MaxQueryText)
	}
	return nil
}

func (q *Query) parseShowClosed(p Params, now time.Time, loc *time.Location) error {
	value := strings.TrimSpace(p.String("show_closed"))
	since := strings.TrimSpace(p.String("closed_since"))
	if since != "" {
		// ONE ANSWER TO ONE QUESTION: `closed_since` and `show_closed`
		// both decide which finished work is in the answer, so naming
		// both is refused rather than resolved by whichever was read last.
		if value != "" {
			return fmt.Errorf("worklet: closed_since and show_closed both say "+
				"which finished work is in the answer — closed_since=%s is "+
				"open work plus what finished since then, so drop "+
				"show_closed=%s", since, value)
		}
		// ON THE COMPANY'S CLOCK, through the calendar every `due=` bound
		// resolves on, so `closed_since=sow` begins the week the
		// `due:bucket` bands and `due=range:sow..eow` mean.
		at, err := period.Resolve(since, now, loc)
		if err != nil {
			return fmt.Errorf("closed_since: %w", err)
		}
		if at.At.IsZero() {
			// THE ZERO INSTANT IS NO BOUND ([ShowClosed.Since]): every
			// finished task has its own spelling.
			return fmt.Errorf("worklet: closed_since=%s is the zero instant, January of year one, "+
				"which bounds nothing — every finished task is show_closed=true", since)
		}
		q.ShowClosed.Since = at.At
		return nil
	}
	switch value {
	case "", "false":
		return nil
	case "true":
		q.ShowClosed.All = true
		return nil
	}
	rest, ok := strings.CutPrefix(value, "recent:")
	if !ok {
		return fmt.Errorf("worklet: show_closed is false, true or recent:<dur>, "+
			"and %q is none of them", value)
	}
	d, err := time.ParseDuration(rest)
	if err != nil || d <= 0 {
		return fmt.Errorf("worklet: %q is not a positive duration", rest)
	}
	q.ShowClosed.Recent = d
	return nil
}

func (q *Query) parseArchived(p Params) error {
	value := ArchivedMode(p.String("archived"))
	// ABSENT IS NOT A FOURTH STATE: it leaves the default [ParseQuery] set.
	if value == "" {
		return nil
	}
	if !value.Valid() {
		return fmt.Errorf("worklet: archived is %s, and %q is none of them",
			strings.Join(ArchivedModeNames(), ", "), value)
	}
	q.Archived = value
	return nil
}

// groupKeys are the groupings a caller may ask for.
var groupKeys = []string{
	"status", "status_group", "assignee", "priority", "tag", "type",
	"project", "unit", "routing_unit", "parent",
	"due:day", "due:week", groupByDueBucket, "start:week",
}

// GroupKeys is every grouping the grammar takes, for the one caller that
// carries its own copy of the list — the dashboard's Display menu — to be held
// against; see client_gate_test.go.
func GroupKeys() []string { return slices.Clone(groupKeys) }

func (q *Query) parseGrouping(p Params) error {
	q.GroupLimit = p.Int("group_limit", 0)
	// NAMED, NOT NON-EMPTY — see [Query.Group]. `group=` is a request for
	// the column holding the rows with no value, and it is the only way to
	// ask for one.
	for key, target := range map[string]**string{
		"group":    &q.Group,
		"subgroup": &q.Subgroup,
	} {
		if !p.Has(key) {
			continue
		}
		value := strings.TrimSpace(p.String(key))
		*target = &value
	}
	for key, target := range map[string]*string{
		"group_by":  &q.GroupBy,
		"group_by2": &q.GroupBy2,
	} {
		value := strings.TrimSpace(p.String(key))
		if value == "" {
			continue
		}
		if !slices.Contains(groupKeys, value) && !strings.HasPrefix(value, "f.") {
			return fmt.Errorf("worklet: %q is not a grouping", value)
		}
		*target = value
	}
	if q.GroupBy2 != "" && q.GroupBy2 == q.GroupBy {
		return fmt.Errorf("worklet: the secondary grouping is %q, the same as "+
			"the first — every row would be alone in its own subgroup", q.GroupBy)
	}
	if q.GroupBy2 != "" && q.GroupBy == "" {
		return fmt.Errorf("worklet: group_by2 was passed without group_by; the " +
			"secondary grouping is a split of the first")
	}
	// `project` groups only at the workspace level, because inside one
	// project every row is in the same group.
	if q.GroupBy == "project" && q.Scope.Project != "" {
		return fmt.Errorf("worklet: group_by=project inside project %s puts "+
			"every row in one group", q.Scope.Project)
	}
	// A COLUMN WITH NO AXIS IS REFUSED NAMING IT: `group=todo` means nothing
	// without the board, and ignoring it would answer the WHOLE set.
	if q.Group != nil && q.GroupBy == "" {
		return fmt.Errorf("worklet: group=%s was passed without group_by; a "+
			"column with no axis is a narrowing that would silently answer "+
			"everything", *q.Group)
	}
	if q.Subgroup != nil && q.GroupBy2 == "" {
		return fmt.Errorf("worklet: subgroup=%s was passed without group_by2, "+
			"so there is no second axis for it to name", *q.Subgroup)
	}
	if q.GroupLimit != 0 && q.GroupBy == "" {
		return fmt.Errorf("worklet: group_limit bounds the rows one COLUMN " +
			"carries and no group_by was passed — `limit` is what bounds a " +
			"flat answer")
	}
	if q.GroupLimit < 0 {
		return fmt.Errorf("worklet: group_limit is %d; a column carries a "+
			"number of rows, and a negative one is not a bound", q.GroupLimit)
	}
	return nil
}

// sortKeys are the orderings a caller may ask for.
//
// `spend_tokens` IS THE COLUMN'S OWN NAME, as a total's is: `spend` alone is
// ambiguous, since `fields=spend` is an object of five. The last four order by
// what a value means rather than how it is spelled, so each compiles to terms
// of its own ([derivedOrders]) rather than a column.
var sortKeys = []string{
	"rank", "updated", "due", "start", "priority", "created", "title",
	"estimate", "points", "spend_tokens", "reopens", "status_entered", "removed",
	"key", "status", "assignee", "type",
}

func (q *Query) parseSort(p Params) error {
	for _, value := range csv(p.String("sort")) {
		descending := strings.HasPrefix(value, "-")
		key := strings.TrimPrefix(value, "-")
		// A BARE `f.` IS NOT A FIELD: resolving to nothing, the term would
		// be dropped at compile and the answer silently come back in the
		// DEFAULT order.
		if ref, ok := strings.CutPrefix(key, FieldKeyPrefix); ok && ref == "" {
			return fmt.Errorf("worklet: %q names no field to sort on; a "+
				"custom-field sort is %s<slug>", value, FieldKeyPrefix)
		}
		if !slices.Contains(sortKeys, key) && !strings.HasPrefix(key, FieldKeyPrefix) {
			return fmt.Errorf("worklet: %q is not a sort key", value)
		}
		q.Sort = append(q.Sort, Sort{Key: key, Descending: descending})
	}
	return nil
}

func (q *Query) parseTotals(p Params) error {
	q.Totals = csv(p.String("totals"))
	if len(q.Totals) > MaxTotals {
		return fmt.Errorf("worklet: %d totals were asked for and the cap is %d; "+
			"each one is another pass over the same rows", len(q.Totals), MaxTotals)
	}
	for _, entry := range q.Totals {
		if _, _, found := strings.Cut(entry, ":"); !found {
			return fmt.Errorf("worklet: a total is <column>:<op> and %q carries "+
				"no operation", entry)
		}
	}
	// THE COLUMN AND THE OP ARE CHECKED AT COMPILE, as a custom-field filter
	// is: `f.<slug>:sum` needs the catalogue to know the field is summable.
	return nil
}

// RowField is one opt-in fact a row carries when a caller asks for it with
// `fields=`.
//
// OPT-IN because [TaskRow] is what a SEAT reads through `list_work_items`, so
// every byte lands in a model's context on every listing; a fact only a board
// card draws is asked for by that surface alone. Each is a batched read over
// the page's ids inside the answer's own transaction: a join per fact would
// multiply the row statement every listing shares, and a read outside it would
// decorate the rows from a different snapshot.
type RowField string

// The four row facts.
const (
	// RowFieldTags is the task's labels, sorted, and an empty list when it
	// has none — present because it was asked for.
	RowFieldTags RowField = "tags"

	// RowFieldDependentsCount is how many live tasks wait on this one: the
	// AUTHORED edges (`waiting_on` on the dependent, what `blocking=` filters
	// on), never the mirrored list, which lags while an edge is one-sided.
	RowFieldDependentsCount RowField = "dependents_count"

	// RowFieldOpenAsks is how many questions on the task are still waiting
	// for an answer — the predicate `has_open_asks=` filters on, so the
	// count and the filter cannot disagree about one task.
	RowFieldOpenAsks RowField = "open_asks"

	// RowFieldSpend is what the task has cost — see [RowSpend].
	RowFieldSpend RowField = "spend"
)

// RowFieldNames are the four, in the order a refusal lists them.
var RowFieldNames = []RowField{
	RowFieldTags, RowFieldDependentsCount, RowFieldOpenAsks, RowFieldSpend,
}

// Valid reports whether this is one of the four.
func (f RowField) Valid() bool { return slices.Contains(RowFieldNames, f) }

// parseRowFields reads `fields=`, refusing a name it does not know rather
// than ignoring it: a card that asked for `spend` spelled wrong would draw
// every task as having cost nothing.
func (q *Query) parseRowFields(p Params) error {
	for _, name := range csv(p.String("fields")) {
		field := RowField(name)
		if !field.Valid() {
			names := make([]string, len(RowFieldNames))
			for i, f := range RowFieldNames {
				names[i] = string(f)
			}
			return fmt.Errorf("worklet: %q is not a row field; the four are %s",
				name, strings.Join(names, ", "))
		}
		if !slices.Contains(q.RowFields, field) {
			q.RowFields = append(q.RowFields, field)
		}
	}
	return nil
}

// Wants reports whether the caller asked for one row fact.
func (q Query) Wants(field RowField) bool { return slices.Contains(q.RowFields, field) }

// parseLevel reads the four freshness keys onto the query through the
// framework's one grammar for them ([statelog.ParseFreshness]), which every
// domain shares; only the tracker's prefix on a refusal is added here.
func (q *Query) parseLevel(p Params) error {
	got, err := statelog.ParseFreshness(p)
	if err != nil {
		return fmt.Errorf("worklet: %w", err)
	}
	q.Level, q.MaxLag, q.MaxLagSeq = got.Level, got.MaxLag, got.MaxLagSeq
	q.Floors = got.Floors
	return nil
}

// parseAny reads the one level of disjunction.
//
// ONE LEVEL, NEVER NESTED; a branch takes the grammar minus the keys about the
// ANSWER rather than the rows — a branch with its own limit, cursor, sort or
// grouping would be a second query pretending to be a predicate.
func (q *Query) parseAny(p Params, now time.Time, loc *time.Location) error {
	raw := strings.TrimSpace(p.String("any"))
	if raw == "" {
		return nil
	}
	var branches []map[string]any
	if err := json.Unmarshal([]byte(raw), &branches); err != nil {
		return fmt.Errorf("worklet: any is a JSON list of parameter objects: %w", err)
	}
	if len(branches) > MaxAnyBranches {
		return fmt.Errorf("worklet: any carries %d branches and the cap is %d; "+
			"an N-way OR across indexes is the one shape the planner handles "+
			"worst", len(branches), MaxAnyBranches)
	}
	for i, branch := range branches {
		// THE ANSWER-SHAPE KEYS TOO: `removed`, `archived`, `show_closed`
		// and `subtasks` decide which tasks the answer is about, one
		// decision for every branch; ANDed inside the OR, one would narrow
		// what the top level asked for. The freshness keys are the
		// caller's own ([expansionRefused]).
		for _, forbidden := range []string{
			"any", "limit", "cursor", "group_by", "group_by2", "group",
			"subgroup", "group_limit", "sort", "view", "preset", "totals",
			"removed", "archived", "show_closed", "closed_since", "subtasks",
			"fields", "around",
			"read_level", "max_lag_seconds", "max_lag_seq", "min_position",
		} {
			if _, present := branch[forbidden]; present {
				return fmt.Errorf("worklet: any branch %d carries %q, which is "+
					"about the ANSWER rather than about the rows — a branch is a "+
					"predicate, not a second query", i, forbidden)
			}
		}
		parsed, err := ParseQuery(MapParams(branch), now, loc)
		if err != nil {
			return fmt.Errorf("any branch %d: %w", i, err)
		}
		q.Any = append(q.Any, parsed)
	}
	return nil
}

// csv splits a comma-separated value, dropping empties.
func csv(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// MapParams reads a query written as a plain object: a disjunction branch
// ([Query.parseAny]) or a seat tool's arguments. Both parse through exactly the
// grammar a URL does, so no filter is honoured on one surface and ignored on
// another.
type MapParams map[string]any

// String is one value. It renders a number or a bool the way the transport's
// own bag does, because a branch written as `{"limit": 5}` in JSON and
// `limit=5` in a query string must parse identically.
func (m MapParams) String(key string) string {
	switch v := m[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	}
	return fmt.Sprint(m[key])
}

// List is every value given for a key that may repeat ([RepeatedKeys]) — see
// [ListOf].
func (m MapParams) List(key string) ([]string, bool) { return ListOf(m[key]) }

// ListOf is one parameter's value read as a list, the ONE reading every bag
// takes of a key that may repeat ([RepeatedKeys]): a string is a list of one; a
// list (a socket frame's JSON array, or a query string's repeats as they cross
// the estate) is each of its strings; absent is empty. Anything else is false,
// which the grammar refuses by the key's name rather than reading as nothing.
func ListOf(value any) ([]string, bool) {
	switch v := value.(type) {
	case nil:
		return nil, true
	case string:
		return []string{v}, true
	case []string:
		return slices.Clone(v), true
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// Int is one value as a number, or def when it is absent OR unparsable.
//
// A BAD VALUE READS AS ABSENT, deliberately: an unknown KEY is refused by
// name, but every numeric parameter here has a safe documented default, so
// falling back to it is a narrower answer rather than a wrong one.
func (m MapParams) Int(key string, def int) int {
	if raw := m.String(key); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			return n
		}
	}
	return def
}

// Bool is one value as a boolean, or def when it is absent or unparsable —
// see [MapParams.Int] for why a malformed value falls back rather than
// refusing.
func (m MapParams) Bool(key string, def bool) bool {
	if raw := m.String(key); raw != "" {
		if b, err := strconv.ParseBool(raw); err == nil {
			return b
		}
	}
	return def
}

// Has reports whether a key was NAMED, however it was spelled.
//
// "Set to empty" and "not set" are different requests for some keys
// (`group=` asks for the column of rows with no value, an absent `group` for
// the whole board) and not others (an empty `assignee` narrows nothing;
// unassigned is `assignee=none`), so they are told apart here rather than by
// whether [MapParams.String] came back empty.
func (m MapParams) Has(key string) bool {
	_, held := m[key]
	return held
}

// Keys is every parameter named, SORTED, so a refusal names the same unknown
// key on every run rather than whichever the map iterated to first.
func (m MapParams) Keys() []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
