package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// Resolving and compiling an `f.<ref>` filter.
//
// # Why the resolution is a separate step from the compilation
//
// [FieldFilter] leaves the ref and the op unresolved, and its own doc says why:
// what is legal depends on the field's DECLARED TYPE, which the parser does
// not have. But [compile] does not have it either — it is a pure function over
// a query, deliberately, so a plan can be explained without a store. So the
// resolution happens where the catalogue is reachable and the answer travels
// in: [Reader.Tasks] resolves inside its own read transaction and hands
// [compile] a map. One read of the declarations per query, and a compiler that
// is still a pure function of its inputs.
//
// # THREE TIERS, EARLIER ONES SHORT-CIRCUITING
//
// A caller types what it remembers: `f.impact`, `f.Impact`, or the field's
// uuid. The order is slug, then id, then case-folded name — the same shape
// [agent/colleague] resolves a handle with, and for the same reason: a query
// that is exactly somebody's slug must never be fuzzy-matched against
// everything else.
//
// An UNRESOLVED ref is a REFUSAL naming it, never an ignored clause. A filter
// nobody resolved is a board showing more than the person asked for, silently
// — which is the whole finding the unknown-key refusal came from.

// resolvedField is one declared field as a filter needs it.
type resolvedField struct {
	ID   string
	Slug string
	Type FieldType

	// Options maps an option's slug and its case-folded name to its ID,
	// because a stored value holds the ID and a caller types the word.
	Options map[string]string

	// Labels is the other direction — the ID to the option's own NAME —
	// and it is a second map rather than an inversion of the first for a
	// reason that is not tidiness: [resolvedField.Options] holds two keys
	// per option, so inverting it picks the slug or the name by Go's
	// randomised map iteration, and a board's column heading would differ
	// between two requests to one node.
	Labels map[string]string

	// Multi marks a field a task can hold several values of, so a board
	// grouped on one puts that task on every column it chose — as a tag
	// board does — rather than on the first value alone.
	Multi bool
}

// The field-filter operators.
//
// A CLOSED SET, because an operator reaches the statement as a NAME rather
// than a bound value — the one thing [compile] composes into SQL — so it has
// to be enumerated where a value never does.
const (
	FieldOpEq       = "eq"
	FieldOpNe       = "ne"
	FieldOpLt       = "lt"
	FieldOpLte      = "lte"
	FieldOpGt       = "gt"
	FieldOpGte      = "gte"
	FieldOpContains = "contains"
	FieldOpNull     = "null"
	FieldOpNotNull  = "not_null"
)

// FieldOps are the nine.
var FieldOps = []string{
	FieldOpEq, FieldOpNe, FieldOpLt, FieldOpLte, FieldOpGt, FieldOpGte,
	FieldOpContains, FieldOpNull, FieldOpNotNull,
}

// resolveFields reads the declarations every `f.<ref>` in a query names.
//
// THE WHOLE QUERY, disjunction branches included, because a branch is a
// predicate in the same statement and its refs are resolved against the same
// catalogue. Resolving per branch would read the declarations once per branch
// for an answer that cannot differ.
func resolveFields(ctx context.Context, tx *sql.Tx, q Query) (
	map[string]resolvedField, error) {

	refs := map[string]bool{}
	collectFieldRefs(q, refs)
	if len(refs) == 0 {
		return nil, nil
	}
	declared, err := declaredFields(ctx, tx, q.Scope.Project)
	if err != nil {
		return nil, err
	}
	bySlug := make(map[string]FieldDef, len(declared))
	byName := make(map[string]FieldDef, len(declared))
	for _, field := range declared {
		if field.Archived {
			// AN ARCHIVED FIELD IS NOT RESOLVABLE. Its values are
			// hidden from every index, so a filter on it would be a
			// clause that matches nothing and reads as "no task has
			// this" rather than "this field is gone".
			continue
		}
		bySlug[field.Slug] = field
		byName[strings.ToLower(field.Name)] = field
	}
	out := make(map[string]resolvedField, len(refs))
	for ref := range refs {
		field, held := resolveOneField(ref, declared, bySlug, byName)
		if !held {
			return nil, fmt.Errorf("tracker: f.%s names no field this company "+
				"declares — a filter nothing resolved would widen this answer "+
				"silently, so it is refused; read the catalogue for the slugs "+
				"that exist", ref)
		}
		out[ref] = field
	}
	return out, nil
}

// resolveOneField is the three-tier lookup.
func resolveOneField(ref string, byID map[string]FieldDef,
	bySlug, byName map[string]FieldDef) (resolvedField, bool) {

	trimmed := strings.TrimSpace(ref)
	for _, candidate := range []FieldDef{
		bySlug[strings.ToLower(trimmed)],
		byID[trimmed],
		byName[strings.ToLower(trimmed)],
	} {
		if candidate.ID == "" || candidate.Archived {
			continue
		}
		field := resolvedField{
			ID: candidate.ID, Slug: candidate.Slug, Type: candidate.Type,
		}
		field.Multi = MultiValued(candidate.Type)
		if FieldValueColumn(candidate.Type) == "ref" {
			field.Options = make(map[string]string, len(candidate.Config.Options)*2)
			field.Labels = make(map[string]string, len(candidate.Config.Options))
			for _, option := range candidate.Config.Options {
				if option.Archived {
					continue
				}
				field.Options[strings.ToLower(option.Slug)] = option.ID
				field.Options[strings.ToLower(option.Name)] = option.ID
				// THE NAME, ALWAYS: a heading is what a person reads,
				// and the slug is what they type.
				field.Labels[option.ID] = option.Name
			}
		}
		return field, true
	}
	return resolvedField{}, false
}

// collectFieldRefs gathers every ref a query and its branches name.
func collectFieldRefs(q Query, into map[string]bool) {
	for _, filter := range q.Fields {
		into[filter.Ref] = true
	}
	if ref, ok := strings.CutPrefix(q.GroupBy, FieldKeyPrefix); ok && ref != "" {
		into[ref] = true
	}
	if ref, ok := strings.CutPrefix(q.GroupBy2, FieldKeyPrefix); ok && ref != "" {
		into[ref] = true
	}
	// AND THE SORT, because `sort=f.<slug>` names one too — and a sort
	// term nothing resolved is silently dropped by [sortTerms], which
	// answers in the DEFAULT order with nothing anywhere saying the
	// caller's own ordering was ignored.
	for _, sort := range q.Sort {
		if ref, ok := strings.CutPrefix(sort.Key, FieldKeyPrefix); ok && ref != "" {
			into[ref] = true
		}
	}
	// AND THE TOTALS, because `f.<slug>:sum` names a field exactly as a
	// filter does — and a total whose field never resolved is a header
	// that reads as zero.
	for _, entry := range q.Totals {
		column, _, _ := strings.Cut(entry, ":")
		if ref, ok := strings.CutPrefix(strings.TrimSpace(column),
			FieldKeyPrefix); ok && ref != "" {
			into[ref] = true
		}
	}
	for _, branch := range q.Any {
		collectFieldRefs(branch, into)
	}
}

// fieldClause compiles one custom-field filter.
//
// # Why this is an IN and not a correlated EXISTS
//
// The two are the same answer and a different PLAN. A correlated
// `EXISTS (… WHERE v.task_id = t.id AND v.field_id = ?)` leads with `task_id`,
// which is the value table's own primary key — so the planner takes the key,
// probes once per task, and the four partial indexes the DDL declares for this
// filter are read by nothing. Measured on the plan fixture it picked
// `sqlite_autoindex_tracker_field_values_1` every time.
//
// Written as `t.id IN (SELECT task_id … WHERE v.field_id = ? AND <column> …)`
// the subquery stands alone, so `(field_id, <column>) WHERE hidden = 0` is a
// seek the planner can drive on — and it is still free to drive on the tasks
// instead when the container is the more selective side. The IN gives it both;
// the EXISTS gave it one.
//
// A clause that compared the WRONG column would be an index the planner cannot
// use AND a predicate that matches nothing — see [FieldValueColumn].
func fieldClause(filter FieldFilter, field resolvedField) (string, []any, error) {
	column := "v." + FieldValueColumn(field.Type)
	inner := func(predicate string, args ...any) (string, []any, error) {
		return "t.id IN (SELECT v.task_id FROM tracker_field_values v " +
			"WHERE v.field_id = ? AND " + liveFieldValue + " AND " +
			predicate + ")", append([]any{field.ID}, args...), nil
	}
	switch filter.Op {
	case FieldOpNull:
		// UNSET IS THE ABSENCE OF A ROW, not a row holding nothing: the
		// apply writes no row for a value that decoded to nothing, so
		// "this field is not set" is exactly "no row".
		clause, args, err := inner(column + " IS NOT NULL")
		// NOT IN IS SAFE HERE because `task_id` is NOT NULL: one NULL
		// in the subquery would make the whole comparison NULL and the
		// predicate would match nothing at all.
		return negated(clause), args, err
	case FieldOpNotNull:
		return inner(column + " IS NOT NULL")
	}

	value, err := fieldValueArg(filter, field)
	if err != nil {
		return "", nil, err
	}
	switch filter.Op {
	case "", FieldOpEq:
		return inner(column+" = ?", value)
	case FieldOpNe:
		// NOT EQUAL IS "NO ROW EQUALS IT", never "some row differs":
		// a labels field with two members would satisfy the second for
		// every value it holds beside the one excluded.
		clause, args, err := inner(column+" = ?", value)
		return negated(clause), args, err
	case FieldOpLt:
		return inner(column+" < ?", value)
	case FieldOpLte:
		return inner(column+" <= ?", value)
	case FieldOpGt:
		return inner(column+" > ?", value)
	case FieldOpGte:
		return inner(column+" >= ?", value)
	case FieldOpContains:
		if FieldValueColumn(field.Type) != "text" {
			return "", nil, fmt.Errorf("tracker: f.%s is a %s and `contains` "+
				"is a text comparison — use eq, or one of lt, lte, gt and gte",
				field.Slug, field.Type)
		}
		// LIKE WITH THE VALUE BOUND and the wildcards composed here, so
		// a caller's own `%` is escaped rather than becoming a pattern.
		return inner(column+` LIKE ? ESCAPE '\'`,
			"%"+likeEscape(fmt.Sprint(value))+"%")
	}
	return "", nil, fmt.Errorf("tracker: %q is not a field comparison — the "+
		"nine are %s", filter.Op, strings.Join(FieldOps, ", "))
}

// fieldValueArg turns the caller's text into the column's own type.
//
// THE DECLARATION DECIDES, not the text: `f.impact=3` on a number field binds
// 3 and on a text field binds "3", and a comparison against the wrong one
// silently matches nothing.
func fieldValueArg(filter FieldFilter, field resolvedField) (any, error) {
	value := strings.TrimSpace(filter.Value)
	switch FieldValueColumn(field.Type) {
	case "num":
		if field.Type == FieldCheckbox {
			// A CHECKBOX TAKES A WORD, because that is what a person
			// types and what the apply stored it as.
			switch strings.ToLower(value) {
			case "true", "yes", "1", "checked":
				return float64(1), nil
			case "false", "no", "0", "unchecked":
				return float64(0), nil
			}
		}
		number, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("tracker: f.%s is a %s and %q is not a "+
				"number", field.Slug, field.Type, clipValue(value))
		}
		return number, nil
	case "at":
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if at, err := time.Parse(layout, value); err == nil {
				return store.EncodeTime(at.UTC()), nil
			}
		}
		return nil, fmt.Errorf("tracker: f.%s is a date and %q is not one — "+
			"write 2026-06-30 or a full RFC3339 instant",
			field.Slug, clipValue(value))
	case "ref":
		// THE SLUG OR THE LABEL RESOLVES TO THE OPTION'S ID, because
		// that is what the row holds — so renaming an option keeps every
		// task that chose it and every filter that names it.
		if id, held := field.Options[strings.ToLower(value)]; held {
			return id, nil
		}
		// AN UNKNOWN WORD IS PASSED THROUGH rather than refused, because
		// a relationship or a people field has no option list at all:
		// its ref is a task id or a handle the caller already holds.
		return value, nil
	}
	return value, nil
}

// likeEscape makes a caller's own wildcards literal.
func likeEscape(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(value)
}

// clipValue is a caller's own value, echoed back flattened.
func clipValue(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// negated turns an `IN` membership into its complement.
//
// TEXTUAL, on the one form [fieldClause] emits, rather than a wrapping `NOT
// (…)` — the two are the same to SQLite and only this one reads as what it is
// where it is called.
func negated(clause string) string {
	return strings.Replace(clause, "t.id IN (", "t.id NOT IN (", 1)
}
