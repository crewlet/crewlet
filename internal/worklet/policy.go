package worklet

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/words"
)

// The company's ONE status set is a Go enum, so a status column means the same
// thing on every node for the life of the deployment. Nothing declares,
// overrides or remaps a set or retires a slug, which spares the remap table,
// lazy rewrite and per-container set a declarable one needs; what custom
// statuses would give, a dropdown field gives.

// Status is one of the six.
type Status string

const (
	// StatusTodo is where a new task lands unless it names another.
	StatusTodo Status = "todo"
	// StatusInProgress is somebody doing the work now.
	StatusInProgress Status = "in_progress"
	// StatusInReview is work awaiting a colleague's check.
	StatusInReview Status = "in_review"
	// StatusDone is delivered.
	StatusDone Status = "done"
	// StatusCancelled is finished WITHOUT being delivered: abandoned, a
	// duplicate, or closed undelivered.
	StatusCancelled Status = "cancelled"
	// StatusClosed is totally completed and out of the way.
	StatusClosed Status = "closed"
)

// DefaultType is the type a task takes when nothing names one. A plain string,
// not an enum: the type catalogue is the WORKSPACE's, and a closed set would
// refuse a company's "incident" or "spike".
const DefaultType = "task"

// Statuses are the six, in the order a board renders them.
var Statuses = []Status{
	StatusTodo, StatusInProgress, StatusInReview,
	StatusDone, StatusCancelled, StatusClosed,
}

// Valid reports whether a status off the wire is one of the six.
func (s Status) Valid() bool { return slices.Contains(Statuses, s) }

// StatusGroup is ClickUp's four, and the level every rule is written at.
type StatusGroup string

const (
	// GroupNotStarted — nobody is working on it yet.
	GroupNotStarted StatusGroup = "not_started"
	// GroupActive — somebody is.
	GroupActive StatusGroup = "active"
	// GroupDone — the work stopped, delivered or not.
	GroupDone StatusGroup = "done"
	// GroupClosed — and it is out of the way.
	GroupClosed StatusGroup = "closed"
)

// StatusGroups are the four.
var StatusGroups = []StatusGroup{
	GroupNotStarted, GroupActive, GroupDone, GroupClosed,
}

// Valid reports whether a group off the wire is one this build knows.
func (g StatusGroup) Valid() bool { return slices.Contains(StatusGroups, g) }

// Open reports a group where work has not finished.
func (g StatusGroup) Open() bool {
	return g == GroupNotStarted || g == GroupActive
}

// Finished reports a group where it has.
func (g StatusGroup) Finished() bool {
	return g == GroupDone || g == GroupClosed
}

// openGroupsSQL is [StatusGroup.Open] as the `IN (…)` list an SQL predicate
// takes, RENDERED FROM the closed set rather than typed again: the `due=overdue`
// alias, the two `show_closed` arms, the workload's overdue count and the
// `due:bucket` grouping all ask it, beside [TaskRow.Overdue] in Go, and a group
// added to [StatusGroups] must reach every copy. It holds only this package's
// constants, so it is safe to splice into a statement as a literal.
var openGroupsSQL = openGroupList()

// openGroupList renders the open half of [StatusGroups] as quoted values.
func openGroupList() string {
	out := make([]string, 0, len(StatusGroups))
	for _, group := range StatusGroups {
		if group.Open() {
			out = append(out, "'"+string(group)+"'")
		}
	}
	return strings.Join(out, ",")
}

// statusRow is one status's fixed properties.
type statusRow struct {
	label       string
	group       StatusGroup
	description string
}

// statusTable is THE table. Its descriptions are rendered to agents, because a
// model choosing between "in_review" and "done" from the slugs alone picks by
// spelling.
var statusTable = map[Status]statusRow{
	StatusTodo: {"To do", GroupNotStarted,
		"not started; nobody is working on it yet"},
	StatusInProgress: {"In progress", GroupActive,
		"somebody is doing the work now"},
	StatusInReview: {"In review", GroupActive,
		"the work is done and awaits a colleague's check"},
	StatusDone: {"Done", GroupDone,
		"delivered; the work is finished and accepted"},
	StatusCancelled: {"Cancelled", GroupDone,
		"finished WITHOUT being delivered — abandoned, a duplicate, or cancelled"},
	StatusClosed: {"Closed", GroupClosed,
		"totally completed and out of the way"},
}

// Label is the status as a person reads it.
func (s Status) Label() string { return statusTable[s].label }

// Group is the status's group, COPIED onto every record that names a status so
// the record is self-describing without a catalogue read.
func (s Status) Group() StatusGroup { return statusTable[s].group }

// Description is what an agent chooses by.
func (s Status) Description() string { return statusTable[s].description }

// Delivered is THE one measurement predicate, read by children_done, cycle and
// lead time and created-vs-resolved. No bit is stamped: the status IS the
// verdict, so nodes cannot disagree and no policy edit re-decides finished
// work. Every abandoning path writes cancelled, which keeps it out of a
// delivery count without a second field.
func Delivered(s Status) bool {
	return s.Group().Finished() && s != StatusCancelled
}

// Priority is ClickUp's fixed scale, always on. A company that wants P0..P4
// declares a dropdown field: a declarable scale would be a second ordering
// with nothing to reconcile it against the board's.
type Priority string

// The five, lowest first. THE DECLARATION ORDER IS THE SCALE: [Priority.Rank]
// is an index into [Priorities], so no second table can drift from it.
const (
	// PriorityNone is the default, a real value rather than an absent
	// one: "nobody has said" and "not urgent" are the same fact here, and
	// a nullable priority would give every filter a null case.
	PriorityNone   Priority = "none"
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
	PriorityUrgent Priority = "urgent"
)

// Priorities are the five, lowest first.
var Priorities = []Priority{
	PriorityNone, PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent,
}

// Valid reports whether a priority off the wire is one of the five.
func (p Priority) Valid() bool { return slices.Contains(Priorities, p) }

// Rank orders the scale for sorting, none lowest.
func (p Priority) Rank() int { return slices.Index(Priorities, p) }

// The two slug grammars: a status, type or field is an IDENTIFIER a tool
// argument names, so it takes underscores; a tag is a LABEL people write in
// prose, so it takes hyphens and a leading digit. One grammar would either
// accept "in-progress" as a status or refuse "v2-api" as a tag.
var (
	// Compiled at first use, not at init: Gitlet's push hook is this
	// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
	slugPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	})
	// Compiled at first use, not at init: Gitlet's push hook is this
	// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
	tagPattern = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	})
)

// ValidSlug reports a well-formed status, type or field slug.
func ValidSlug(s string) bool { return slugPattern().MatchString(s) }

// ValidTagSlug reports a well-formed tag slug.
func ValidTagSlug(s string) bool { return tagPattern().MatchString(s) }

// A SLUG IS IMMUTABLE: a rename changes the label only, because stored values,
// saved views and history point at the slug, and moving it would re-point them
// all at nothing.

// Named is anything a caller may address by slug or by label. One interface
// for tags, types, fields and options, rather than four resolvers, because the
// resolution ORDER must be identical, and each copy is a chance to put
// case-insensitivity before an exact match.
type Named interface {
	// Ident is the stable slug or id, and Label is what a person reads.
	Ident() string
	Label() string
}

// ErrAmbiguous reports a query that matched two candidates at one tier. It
// names both, because the caller is usually a model: "ambiguous" sends it
// guessing, and "api or apis" is something it can act on.
type ErrAmbiguous struct {
	Query   string
	Matches []string
}

func (e *ErrAmbiguous) Error() string {
	return fmt.Sprintf("worklet: %q matches %s; name one of them exactly",
		e.Query, words.And(e.Matches))
}

// Resolve finds the one candidate a query names, in three tiers, an earlier
// one settling it whatever later ones hold: an exact slug, an exact label, a
// case-insensitive label. A query that IS somebody's slug must never be
// fuzzy-matched against another's label. Two matches at one tier are refused
// as [ErrAmbiguous].
func Resolve[T Named](query string, candidates []T) (T, error) {
	var zero T
	q := strings.TrimSpace(query)
	if q == "" {
		return zero, fmt.Errorf("worklet: nothing to resolve; an empty name " +
			"matches every candidate and therefore names none")
	}
	tiers := []func(T) bool{
		func(c T) bool { return c.Ident() == q },
		func(c T) bool { return c.Label() == q },
		func(c T) bool { return strings.EqualFold(c.Label(), q) },
	}
	for _, matches := range tiers {
		var hits []T
		for _, c := range candidates {
			if matches(c) {
				hits = append(hits, c)
			}
		}
		switch len(hits) {
		case 0:
			continue
		case 1:
			return hits[0], nil
		default:
			names := make([]string, 0, len(hits))
			for _, h := range hits {
				names = append(names, h.Ident())
			}
			slices.Sort(names)
			return zero, &ErrAmbiguous{Query: q, Matches: names}
		}
	}
	return zero, fmt.Errorf("worklet: nothing here is called %q", q)
}
