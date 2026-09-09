package tracker

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The company's ONE status set, and why nothing declares another.
//
// A status is a Go enum, so a task's status column means the same thing on
// every node for the life of the deployment and two nodes at one checkpoint
// cannot disagree about it. Nothing declares a set, nothing overrides one,
// nothing remaps one and nothing retires a slug — which is what removes the
// remap table, the lazy rewrite and the per-container set that a declarable
// set drags behind it. What a company gains from custom statuses it can have
// from a dropdown field; what it would lose is that sentence.

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
	// StatusCancelled is finished WITHOUT being delivered — abandoned, a
	// duplicate, or swept away by a sprint close.
	StatusCancelled Status = "cancelled"
	// StatusClosed is totally completed and out of the way.
	StatusClosed Status = "closed"
)

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

// statusRow is one status's fixed properties.
type statusRow struct {
	label       string
	group       StatusGroup
	description string
}

// statusTable is THE table, and the description is rendered to agents.
//
// A DESCRIPTION PER STATUS, because a status is chosen by meaning: a model
// picking between "in_review" and "done" from the slugs alone picks by
// spelling, and the difference between the two is a whole colleague's work.
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
		"finished WITHOUT being delivered — abandoned, a duplicate, or swept away by a sprint close"},
	StatusClosed: {"Closed", GroupClosed,
		"totally completed and out of the way"},
}

// Label is the status as a person reads it.
func (s Status) Label() string { return statusTable[s].label }

// Group is the status's group, and it is COPIED onto every record that names
// a status so the record is self-describing without a catalogue read.
func (s Status) Group() StatusGroup { return statusTable[s].group }

// Description is what an agent chooses by.
func (s Status) Description() string { return statusTable[s].description }

// Delivered is THE measurement predicate, and there is exactly one.
//
// Read by children_done, velocity, the burndown, the burnup, cycle and lead
// time, a goal's task targets and created-vs-resolved. NO BIT IS STAMPED
// ANYWHERE: the status IS the verdict, so two nodes cannot disagree about it
// and no policy edit can re-decide it for work already finished. Every
// "abandoned" path in this design simply writes cancelled, which is what
// makes those tasks invisible to velocity without a second field.
func Delivered(s Status) bool {
	return s.Group().Finished() && s != StatusCancelled
}

// Priority is ClickUp's fixed scale, always on.
//
// A company that wants P0..P4 declares a dropdown field. Making the scale
// declarable would put a second ordering in the model with nothing to
// reconcile it against the board's own.
type Priority string

const (
	// PriorityNone is the default and is a real value rather than an
	// absent one: "nobody has said" and "explicitly not urgent" are the
	// same fact here, and a nullable priority would make every filter
	// carry a null case for no gain.
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

// The two slug grammars.
//
// TWO RATHER THAN ONE, and the difference is the separator: a status, a type
// and a field are IDENTIFIERS a tool argument names, so they take underscores
// and read as one word; a tag is a LABEL people write in prose and copy from
// each other, so it takes hyphens and digits at the front. Merging them would
// mean either accepting "in-progress" as a status or refusing "v2-api" as a
// tag, and both have been asked for.
var (
	slugPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	tagPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

// ValidSlug reports a well-formed status, type or field slug.
func ValidSlug(s string) bool { return slugPattern.MatchString(s) }

// ValidTagSlug reports a well-formed tag slug.
func ValidTagSlug(s string) bool { return tagPattern.MatchString(s) }

// A SLUG IS IMMUTABLE. A rename changes the label and never the slug, because
// a slug is what a stored value, a saved view's parameters and a year of
// history all point at — and a rename that moved it would silently re-point
// every one of them at nothing.

// Named is anything a caller may address by slug or by label.
//
// AN INTERFACE THE CALLER SATISFIES rather than four near-identical resolvers,
// because the resolution ORDER is the part that has to be identical: a tag, a
// type, a field and an option are all "the thing the model typed", and four
// copies of a three-tier fallback is four chances to put case-insensitivity
// before an exact match.
type Named interface {
	// Ident is the stable slug or id, and Label is what a person reads.
	Ident() string
	Label() string
}

// ErrAmbiguous reports a query that matched two candidates at one tier.
//
// IT NAMES BOTH, because the caller is usually a model with one round left:
// "ambiguous" sends it guessing, and "api or apis" is something it can act on.
type ErrAmbiguous struct {
	Query   string
	Matches []string
}

func (e *ErrAmbiguous) Error() string {
	return fmt.Sprintf("tracker: %q matches %s — name one of them exactly",
		e.Query, strings.Join(e.Matches, " and "))
}

// Resolve finds the one candidate a query names.
//
// THREE TIERS, EARLIER ONES SHORT-CIRCUITING LATER: an exact slug, then an
// exact label, then a case-insensitive label. The order is the whole content
// of the rule — a query that IS somebody's slug must never be fuzzy-matched
// against everybody else's label, which is how "api" resolves to "apis" on a
// company that has both.
//
// Two matches at ONE tier is ambiguous and is refused naming both; a match at
// an earlier tier settles it whatever the later tiers hold.
func Resolve[T Named](query string, candidates []T) (T, error) {
	var zero T
	q := strings.TrimSpace(query)
	if q == "" {
		return zero, fmt.Errorf("tracker: nothing to resolve — an empty name " +
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
	return zero, fmt.Errorf("tracker: nothing here is called %q", q)
}
