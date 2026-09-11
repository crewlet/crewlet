package tracker

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// ONE GRAMMAR, parsed once.
//
// The board, a seat's `list_tasks`, a saved view and the dashboard's URL hash
// are the same question asked four ways, and they are parsed by this one
// function into one struct. That is not tidiness: a filter honoured on one
// path and ignored on another is how a saved view answers a different question
// from the screen that saved it, and nobody notices until somebody compares
// two numbers that should have matched.
//
// THERE IS NO QUERY LANGUAGE. Every key is a flat parameter, so a model can
// write one without a parser in its head and a URL can carry one without
// escaping. What that costs is expressiveness, and the `any` key is where the
// cost is paid: one level of disjunction, capped, and never nested.

// The limits a query is clamped or refused by.
const (
	// MaxAnyBranches caps the disjunction.
	//
	// An N-way OR across indexes is the one shape the planner handles
	// worst — it cannot drive from one index and has to union or scan —
	// and eight is the widest any view in this model needs.
	MaxAnyBranches = 8

	// MaxQueryText is the text bound. Four hundred bytes is a sentence, and
	// the ranker's own posting-scan cap makes a longer one buy nothing.
	MaxQueryText = 400

	// MaxTotals caps the aggregate list, because every entry is another
	// pass over the same rows.
	MaxTotals = 8

	// ActivityQuerySpanDays bounds a text search over the activity feed.
	//
	// The gate it enforces is on what the query would SCAN rather than on
	// which key is present — the shape it replaces asked for "a task, a
	// container or a since: bound", and `container=workspace` and a
	// five-year `since:` both satisfied that while narrowing nothing.
	ActivityQuerySpanDays = 90

	// GroupByRowCeiling is the row count at which a grouped answer's sort
	// working set crosses the page cache and spills.
	//
	// The gate runs a BOUNDED COUNT first and refuses past this, because
	// the materialisation a window function needs cannot be pushed down:
	// the whole partitioned, sorted input exists before the per-group limit
	// applies.
	GroupByRowCeiling = 20_000
)

// Container is what a query is scoped to.
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
// because what is legal depends on the field's declared type — which the
// parser does not have and the compiler does.
type FieldFilter struct {
	// Ref is the slug or the id the caller typed, unresolved: resolution
	// needs the catalogues, and a parser that read them would be a parser
	// that could fail on a store.
	Ref   string
	Op    string
	Value string
}

// SubtaskMode is how a subtree is filtered.
type SubtaskMode string

const (
	// SubtasksCollapsed and SubtasksExpanded filter ROOT tasks and let
	// their subtrees ride along unfiltered; SubtasksSeparate filters every
	// task on its own.
	SubtasksCollapsed SubtaskMode = "collapsed"
	SubtasksExpanded  SubtaskMode = "expanded"
	SubtasksSeparate  SubtaskMode = "separate"
)

// ShowClosed is how finished work is included.
type ShowClosed struct {
	All bool

	// Recent bounds the finished set by age. It applies to the WHOLE
	// finished set, so a task cancelled inside the window is in the answer
	// exactly as one done inside it is — which is what the group stamp
	// buys.
	Recent time.Duration
}

// ArchivedMode is how archived work is included.
type ArchivedMode string

const (
	ArchivedExclude ArchivedMode = "false"
	ArchivedInclude ArchivedMode = "true"
	ArchivedOnly    ArchivedMode = "only"
)

// Sort is one ordering term.
type Sort struct {
	Key        string
	Descending bool
}

// Query is one parsed question.
//
// EVERY FIELD IS RESOLVED OR REFUSED — nothing here is a string somebody
// downstream has to re-parse — with two deliberate exceptions, both marked at
// the field: a custom field's op needs the catalogue, and the text query is the
// searcher's own input.
type Query struct {
	Scope Scope

	// View and Preset are loaded FIRST and explicit keys override them, so
	// a saved view is a set of defaults rather than a lock.
	View   string
	Preset string

	Status       []Status
	StatusNot    []Status
	StatusGroups []StatusGroup

	Assignee          []string
	Collaborator      []string
	Reporter          []string
	Watcher           []string
	ChecklistAssignee []string

	Unit        []string
	RoutingUnit []string

	Tags TagFilter

	Types      []string
	Priorities []Priority

	Parent   string
	Root     string
	Subtasks SubtaskMode

	// Keys narrows to the task keys a caller already holds — what
	// somebody pasted into chat, what a report linked. A LIST rather than
	// one key, because resolving eight pasted keys as eight reads is
	// eight round trips for one question.
	Keys []string

	// Removed is the TRASH, and it is three-valued for the reason every
	// other tombstone filter here is: absent excludes removed work (what
	// every board means), `true` asks for ONLY it (the restore surface),
	// and `false` is the same exclusion said out loud.
	//
	// Without it there is no way to list what a removal hid, so nothing
	// could reach [OpRestore] — `tracker_tasks_removed_idx` names
	// `work_trash` as its reader and the reader did not exist.
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
	AskedBy         string

	LinkedPage string
	References string
	Goal       string
	Batch      string

	Sprint []string

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
	Group      string
	Subgroup   string

	Sort []Sort

	Totals []string

	Level statelog.ReadLevel

	// MaxLag is the bound a `stale` read declares it will accept, and zero
	// accepts anything.
	//
	// CARRIED RATHER THAN MERELY VALIDATED. `max_lag_seconds` was checked
	// against the level and then dropped on the floor, so a dashboard tile
	// polling every twenty seconds and declaring a twenty-second bound was
	// served an answer of any age at all — and rendered it as a live tile,
	// because the answer came back at the level it asked for.
	MaxLag time.Duration

	// Session is the caller's own high-water mark on this domain's log,
	// which is what a `session` read waits through. Zero is a caller that
	// has written nothing, for whom every level below linearizable is the
	// same answer.
	Session statelog.Position

	// MinPosition is an explicit floor a caller may name — the position a
	// wake carried, or one its own previous write returned. The documented
	// override rather than the mechanism.
	MinPosition statelog.Position

	Limit  int
	Cursor string
}

// ParseQuery reads one flat parameter map into one query.
//
// `now` and `loc` are ARGUMENTS rather than reads, because every relative date
// resolves against the company's own clock and a parser that read a package
// clock could not be tested at a boundary — and half the tokens in this grammar
// are boundaries.
// Params is the request's own parameters, as this parser reads them.
//
// DECLARED HERE, BY THE CONSUMER, and kept to the five methods the grammar
// actually calls. The concrete bag lives with the API surface that fills it
// from a query string or a socket frame — and a parser that imported that
// surface would be the tracker depending on the transport it is read through,
// which is also an import cycle the moment the surface reads a tracker type.
type Params interface {
	// String is one value, empty when absent.
	String(key string) string

	// Int and Bool take a default, because "absent" and "zero" are
	// different answers for both: `limit=0` is a caller asking for
	// nothing, and an absent limit is a caller asking for the default.
	Int(key string, def int) int
	Bool(key string, def bool) bool

	// Has distinguishes them, which is what makes `open=false` and no
	// `open` at all two different questions.
	Has(key string) bool

	// Keys is every parameter named, so an unknown one is REFUSED rather
	// than ignored: a filter nobody parsed is a board showing more than
	// the person asked for, silently.
	Keys() []string
}

// QueryKeys is every parameter this grammar reads, sorted.
//
// IT EXISTS SO AN UNKNOWN KEY CAN BE REFUSED, which is what [Params.Keys] is
// for and what a caller is entitled to: a filter nobody parsed is a board
// showing more than the person asked for, silently, and the person has no way
// to tell. Ignoring `open=true` renders every closed task beside the open
// ones and looks exactly like a project where nothing is finished.
//
// It is a LIST rather than a check per parser because the refusal has to run
// before any of them: a key misspelled into a neighbour's territory must be
// named as unknown rather than swallowed by whichever helper reached it first.
// The one shape not in it is a custom field, `f.<ref>`, whose refs are a
// company's own and cannot be enumerated here — see [Query.parseFields].
var QueryKeys = []string{
	"any", "archived", "asked_by", "asked_of", "assignee", "batch",
	"blocked", "blocking", "checklist_assignee", "closed", "collaborator",
	"container", "created", "cursor", "done", "due", "estimate",
	"finished", "flag", "goal", "group", "group_by", "group_by2",
	"group_limit", "has_children", "has_dependencies", "has_open_asks",
	"has_parent", "key", "limit", "linked_page",
	"max_lag_seconds", "max_lag_seq", "parent", "points", "preset",
	"priority", "q", "read_level", "references", "removed", "reporter",
	"root", "routing_unit", "show_closed", "sort", "spend", "sprint",
	"start", "status", "status_entered", "status_group", "subgroup",
	"subtasks", "tag", "totals", "type", "unit", "updated", "view",
	"watcher",
}

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
		// THE REFUSAL NAMES THE KEY AND NOTHING ELSE. Listing sixty-odd
		// parameters at a caller who typed one wrong is a wall nobody
		// reads; the schema and the docs are where the set is written
		// down, and `f.` is named because it is the one shape a reader
		// cannot find in either.
		return fmt.Errorf("tracker: %q is not a query parameter — a filter "+
			"nothing parses would silently widen this answer, so it is "+
			"refused rather than ignored; a custom field is %s<ref>",
			key, FieldKeyPrefix)
	}
	return nil
}

func ParseQuery(p Params, now time.Time, loc *time.Location) (Query, error) {
	// FIRST, so a misspelling is named as unknown rather than reported by
	// whichever parser its neighbour belongs to.
	if err := checkKeys(p); err != nil {
		return Query{}, err
	}
	q := Query{
		Subtasks:   SubtasksCollapsed,
		Archived:   ArchivedExclude,
		Dates:      map[string]DateFilter{},
		View:       p.String("view"),
		Preset:     p.String("preset"),
		Group:      p.String("group"),
		Subgroup:   p.String("subgroup"),
		Cursor:     p.String("cursor"),
		LinkedPage: p.String("linked_page"),
		References: p.String("references"),
		Goal:       p.String("goal"),
		Batch:      p.String("batch"),
		AskedOf:    p.String("asked_of"),
		AskedBy:    p.String("asked_by"),
		Parent:     p.String("parent"),
		Root:       p.String("root"),
	}

	if err := q.parseScope(p); err != nil {
		return Query{}, err
	}
	if err := q.parseStatus(p); err != nil {
		return Query{}, err
	}
	q.Assignee = csv(p.String("assignee"))
	q.Collaborator = csv(p.String("collaborator"))
	q.Reporter = csv(p.String("reporter"))
	q.Watcher = csv(p.String("watcher"))
	q.ChecklistAssignee = csv(p.String("checklist_assignee"))
	q.Unit = csv(p.String("unit"))
	q.RoutingUnit = csv(p.String("routing_unit"))
	q.Types = csv(p.String("type"))
	q.Sprint = csv(p.String("sprint"))
	q.Flags = csv(p.String("flag"))
	// KEYS ARE UPPERCASED, because a key is what somebody pasted and
	// `eng-7` is the same task as `ENG-7`. The project half is minted
	// upper-case, so lowering the column instead would defeat
	// `tracker_tasks_key_idx`.
	for _, key := range csv(p.String("key")) {
		q.Keys = append(q.Keys, strings.ToUpper(key))
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
	if err := q.parseShowClosed(p); err != nil {
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
// AN ABSENT CONTAINER IS NEITHER — not the workspace and not a project — and
// the caller resolves it from its own surface: a seat's tools scope to the
// seat's own projects, and a human's to everything. Defaulting to the
// workspace here would make an omitted key the most expensive query in the
// system.
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
	}
	if key == "" {
		return fmt.Errorf("tracker: container %q names no project", value)
	}
	// UPPER-CASED, because a project key is stored upper and the scope is
	// an EXACT compare against `project_key`. A board asked for
	// `project:eng` would otherwise answer an empty list rather than a
	// refusal — the one failure shape a person acts on, by filing the
	// duplicate or concluding the migration lost their work.
	q.Scope.Project = strings.ToUpper(key)
	return nil
}

// parseStatus reads the status and status_group keys, with `!` negation.
func (q *Query) parseStatus(p Params) error {
	for _, value := range csv(p.String("status")) {
		negated := strings.HasPrefix(value, "!")
		slug := Status(strings.TrimPrefix(value, "!"))
		if !slug.Valid() {
			return fmt.Errorf("tracker: %q is not one of this company's six "+
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
			return fmt.Errorf("tracker: %q is not one of the four status groups",
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
			return fmt.Errorf("tracker: %q is not a tag mode — the three are "+
				"any:, all: and none:", head)
		}
	}
	q.Tags = TagFilter{Mode: mode, Tags: csv(value)}
	return nil
}

func (q *Query) parsePriorities(p Params) error {
	for _, value := range csv(p.String("priority")) {
		priority := Priority(value)
		if !priority.Valid() {
			return fmt.Errorf("tracker: %q is not one of the five priorities", value)
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
		return fmt.Errorf("tracker: %q is not a subtask mode — the three are "+
			"collapsed, expanded and separate", value)
	}
	return nil
}

// dateKeys are the eight columns a date filter may name.
//
// `status_entered` reads the EFFECTIVE instant because it is the start of a
// duration; the other seven read the AUTHORED one, because each is a
// wall-clock bound the caller typed and a clamp would answer a different
// question from the one on the screen.
var dateKeys = []string{
	"due", "start", "created", "updated", "done", "closed", "finished",
	"status_entered",
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
		"spend":    &q.Spend,
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
		return NumFilter{}, fmt.Errorf("tracker: %q names no comparison — a "+
			"numeric filter is lt:, lte:, gt:, gte:, range:a..b, null or "+
			"not_null", value)
	}
	switch NumOp(op) {
	case NumLT, NumLTE, NumGT, NumGTE:
		n, err := strconv.ParseFloat(rest, 64)
		if err != nil {
			return NumFilter{}, fmt.Errorf("tracker: %q is not a number", rest)
		}
		return NumFilter{Op: NumOp(op), From: n}, nil
	case NumRange:
		from, to, ok := strings.Cut(rest, "..")
		if !ok {
			return NumFilter{}, fmt.Errorf("tracker: a range is a..b and %q "+
				"carries no separator", rest)
		}
		lo, err := strconv.ParseFloat(from, 64)
		if err != nil {
			return NumFilter{}, fmt.Errorf("tracker: %q is not a number", from)
		}
		hi, err := strconv.ParseFloat(to, 64)
		if err != nil {
			return NumFilter{}, fmt.Errorf("tracker: %q is not a number", to)
		}
		if hi < lo {
			return NumFilter{}, fmt.Errorf("tracker: the range %q ends below "+
				"where it starts, so it matches nothing", rest)
		}
		return NumFilter{Op: NumRange, From: lo, To: hi}, nil
	}
	return NumFilter{}, fmt.Errorf("tracker: %q is not a numeric comparison", op)
}

// parseFields collects the custom-field predicates.
//
// LEFT UNRESOLVED, deliberately: which ops a field admits depends on its
// declared type, and the declaration lives in a catalogue this parser does not
// read. A parser that read one could fail on a store, and a query that cannot
// be parsed without I/O cannot be parsed inside a transaction.
func (q *Query) parseFields(p Params) {
	for _, key := range p.Keys() {
		ref, ok := strings.CutPrefix(key, FieldKeyPrefix)
		if !ok || ref == "" {
			continue
		}
		value := strings.TrimSpace(p.String(key))
		// `null` AND `not_null` ARE THE WHOLE VALUE, exactly as they are
		// in a numeric filter beside this one: they take no operand, so
		// a caller writes `f.effort=not_null` rather than a colon and
		// nothing. Read as an `eq` against the text "not_null" they
		// would be compared as a VALUE — against a number column, which
		// refuses it, and against a text one, which silently matches the
		// tasks whose field literally says "not_null".
		switch value {
		case FieldOpNull, FieldOpNotNull:
			q.Fields = append(q.Fields, FieldFilter{Ref: ref, Op: value})
			continue
		}
		op, rest, found := strings.Cut(value, ":")
		if !found {
			op, rest = FieldOpEq, value
		}
		q.Fields = append(q.Fields, FieldFilter{Ref: ref, Op: op, Value: rest})
	}
	// THE ORDER IS STABLE BECAUSE Keys IS SORTED, and it is sorted there
	// rather than here: every reader that enumerates parameters needs the
	// same guarantee, and a second sort in this function would be a second
	// place the property could be true.
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
// `q` here is a substring of a key or a title — "the item I half remember" —
// and that is the whole of it. RANKED SEARCH OVER THE COMPANY'S PROSE IS
// `search_knowledge`'s, behind the [knowledge] seam, which is where the
// analyzer, the inverted list and the vectors are. This grammar has none of
// them: `kb_docs` and `kb_postings` index PAGES, and nothing has ever put a
// task in them.
//
// So there is no `mode`. Three modes over one behaviour is a knob whose values
// cannot differ, which is worse than no knob: a caller that asked for
// `semantic` and got a substring match has been answered by a name rather than
// by a search.
func (q *Query) parseText(p Params) error {
	q.Text = strings.TrimSpace(p.String("q"))
	if len(q.Text) > MaxQueryText {
		return fmt.Errorf("tracker: the find text is %d bytes and the bound is "+
			"%d — this is a substring of a key or a title, and a longer one "+
			"matches nothing; ask search_knowledge a question this long",
			len(q.Text), MaxQueryText)
	}
	return nil
}

func (q *Query) parseShowClosed(p Params) error {
	value := strings.TrimSpace(p.String("show_closed"))
	switch value {
	case "", "false":
		return nil
	case "true":
		q.ShowClosed.All = true
		return nil
	}
	rest, ok := strings.CutPrefix(value, "recent:")
	if !ok {
		return fmt.Errorf("tracker: show_closed is false, true or recent:<dur>, "+
			"and %q is none of them", value)
	}
	d, err := time.ParseDuration(rest)
	if err != nil || d <= 0 {
		return fmt.Errorf("tracker: %q is not a positive duration", rest)
	}
	q.ShowClosed.Recent = d
	return nil
}

func (q *Query) parseArchived(p Params) error {
	switch value := ArchivedMode(p.String("archived")); value {
	case "":
	case ArchivedExclude, ArchivedInclude, ArchivedOnly:
		q.Archived = value
	default:
		return fmt.Errorf("tracker: archived is false, true or only, and %q is "+
			"none of them", value)
	}
	return nil
}

// groupKeys are the groupings a caller may ask for.
var groupKeys = []string{
	"status", "status_group", "assignee", "priority", "tag", "type",
	"project", "sprint", "unit", "routing_unit", "parent",
	"due:day", "due:week", "start:week",
}

func (q *Query) parseGrouping(p Params) error {
	q.GroupLimit = p.Int("group_limit", 0)
	for key, target := range map[string]*string{
		"group_by":  &q.GroupBy,
		"group_by2": &q.GroupBy2,
	} {
		value := strings.TrimSpace(p.String(key))
		if value == "" {
			continue
		}
		if !slices.Contains(groupKeys, value) && !strings.HasPrefix(value, "f.") {
			return fmt.Errorf("tracker: %q is not a grouping", value)
		}
		*target = value
	}
	if q.GroupBy2 != "" && q.GroupBy2 == q.GroupBy {
		return fmt.Errorf("tracker: the secondary grouping is %q, the same as "+
			"the first — every row would be alone in its own subgroup", q.GroupBy)
	}
	if q.GroupBy2 != "" && q.GroupBy == "" {
		return fmt.Errorf("tracker: group_by2 was passed without group_by; the " +
			"secondary grouping is a split of the first")
	}
	// `project` groups only at the workspace level, because inside one
	// project every row is in the same group.
	if q.GroupBy == "project" && q.Scope.Project != "" {
		return fmt.Errorf("tracker: group_by=project inside project %s puts "+
			"every row in one group", q.Scope.Project)
	}
	return nil
}

// sortKeys are the orderings a caller may ask for.
var sortKeys = []string{
	"rank", "updated", "due", "priority", "created", "title", "estimate",
	"points", "spend", "status_entered",
}

func (q *Query) parseSort(p Params) error {
	for _, value := range csv(p.String("sort")) {
		descending := strings.HasPrefix(value, "-")
		key := strings.TrimPrefix(value, "-")
		if !slices.Contains(sortKeys, key) && !strings.HasPrefix(key, "f.") {
			return fmt.Errorf("tracker: %q is not a sort key", value)
		}
		q.Sort = append(q.Sort, Sort{Key: key, Descending: descending})
	}
	return nil
}

func (q *Query) parseTotals(p Params) error {
	q.Totals = csv(p.String("totals"))
	if len(q.Totals) > MaxTotals {
		return fmt.Errorf("tracker: %d totals were asked for and the cap is %d — "+
			"each one is another pass over the same rows", len(q.Totals), MaxTotals)
	}
	for _, entry := range q.Totals {
		if _, _, found := strings.Cut(entry, ":"); !found {
			return fmt.Errorf("tracker: a total is <column>:<op> and %q carries "+
				"no operation", entry)
		}
	}
	// THE COLUMN AND THE OP ARE CHECKED AT COMPILE, not here, for the
	// reason a custom-field filter is: `f.<slug>:sum` needs the catalogue
	// to know whether the field is summable at all, and a parser that read
	// one could fail on a store.
	return nil
}

func (q *Query) parseLevel(p Params) error {
	value := strings.TrimSpace(p.String("read_level"))
	if value == "" {
		// ABSENT IS NOT A FOURTH STATE. It resolves to the SURFACE's own
		// default — a seat tool linearizable, a dashboard poll stale —
		// which is what makes the default a property of the surface
		// rather than of the model that happened to omit the key.
		return nil
	}
	level := statelog.ReadLevel(value)
	if !level.Valid() {
		return fmt.Errorf("tracker: %q is not a read level — the four are "+
			"linearizable, session, stale and consistent_prefix", value)
	}
	q.Level = level
	if level != statelog.ReadStale {
		for _, key := range []string{"max_lag_seq", "max_lag_seconds"} {
			if p.Has(key) {
				return fmt.Errorf("tracker: %s bounds how STALE an answer may "+
					"be and read_level is %s, which is not a staleness bound at "+
					"all", key, level)
			}
		}
		return nil
	}
	// AND THE BOUND IS CARRIED, not just checked. A bound refused when it
	// is inconsistent and dropped when it is not is a bound that never
	// bounded anything.
	if p.Has("max_lag_seconds") {
		secs := p.Int("max_lag_seconds", 0)
		if secs < 0 {
			return fmt.Errorf("tracker: max_lag_seconds is how many seconds "+
				"behind an answer may be, and %d is not a duration", secs)
		}
		q.MaxLag = time.Duration(secs) * time.Second
	}
	return nil
}

// parseAny reads the one level of disjunction.
//
// ONE LEVEL, NEVER NESTED, and the branches carry the same grammar minus the
// keys that are about the ANSWER rather than about the rows — a branch with its
// own limit, cursor, sort or grouping would be a second query pretending to be
// a predicate.
func (q *Query) parseAny(p Params, now time.Time, loc *time.Location) error {
	raw := strings.TrimSpace(p.String("any"))
	if raw == "" {
		return nil
	}
	var branches []map[string]any
	if err := json.Unmarshal([]byte(raw), &branches); err != nil {
		return fmt.Errorf("tracker: any is a JSON list of parameter objects: %w", err)
	}
	if len(branches) > MaxAnyBranches {
		return fmt.Errorf("tracker: any carries %d branches and the cap is %d — "+
			"an N-way OR across indexes is the one shape the planner handles "+
			"worst", len(branches), MaxAnyBranches)
	}
	for i, branch := range branches {
		for _, forbidden := range []string{
			"any", "limit", "cursor", "group_by", "group_by2", "group",
			"subgroup", "group_limit", "sort", "view", "preset", "totals",
		} {
			if _, present := branch[forbidden]; present {
				return fmt.Errorf("tracker: any branch %d carries %q, which is "+
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

// MapParams reads a query written as a plain object.
//
// TWO CALLERS AND ONE OF THEM IS INSIDE THIS FILE. A disjunction branch is a
// query written inside a query — the transport's own bag holds the outer one,
// and the inner one has never been through a query string — and a seat's tool
// builds one from the arguments a model passed. Both parse through exactly the
// same grammar as a query typed into a URL, which is the whole point: a filter
// honoured on one surface and ignored on another is the failure one grammar
// exists to prevent.
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

func (m MapParams) Int(key string, def int) int {
	if raw := m.String(key); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			return n
		}
	}
	return def
}

func (m MapParams) Bool(key string, def bool) bool {
	if raw := m.String(key); raw != "" {
		if b, err := strconv.ParseBool(raw); err == nil {
			return b
		}
	}
	return def
}

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
