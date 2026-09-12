package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
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

	// The SET operators, which are what a multi-valued field is actually
	// asked about. A `labels` field holds several options at once, so
	// `eq` against it is a question with no useful answer: it matches a
	// task whose field holds that option AND, because the value is one
	// row per option, says nothing about the others.
	FieldOpAny    = "any"
	FieldOpAll    = "all"
	FieldOpNotAny = "not_any"
	FieldOpNotAll = "not_all"

	// FieldOpIn is `any` over a SINGLE-valued text column. Spelled
	// separately because the two read differently on the surface they
	// apply to: "the status text is one of these" and "this label set
	// contains one of these" are not the same question, and one grammar
	// word for both would make a caller guess which they were asking.
	FieldOpIn = "in"

	// FieldOpStartsWith is the prefix comparison, which `contains` cannot
	// express and an index could serve where `contains` never can.
	FieldOpStartsWith = "startswith"

	// FieldOpRange is the closed interval, written `from..to`. Two bounds
	// in one filter rather than a `gte` and an `lte` a caller has to pair
	// up — and the pairing is what they get wrong.
	FieldOpRange = "range"

	// FieldOpMe is the viewer, on a `people` field. It takes no value at
	// all: a query that named a handle would be a query somebody saved
	// and everybody else read as that person's.
	FieldOpMe = "me"
)

// FieldOps are the seventeen.
var FieldOps = []string{
	FieldOpEq, FieldOpNe, FieldOpLt, FieldOpLte, FieldOpGt, FieldOpGte,
	FieldOpContains, FieldOpStartsWith, FieldOpIn, FieldOpRange,
	FieldOpAny, FieldOpAll, FieldOpNotAny, FieldOpNotAll,
	FieldOpMe, FieldOpNull, FieldOpNotNull,
}

// fieldOpsFor is which operators a field TYPE admits.
//
// PER TYPE, because an operator that does not apply is not a narrower answer
// — it is a clause that matches nothing and reads as "no task has this". A
// caller comparing a `labels` field with `eq` gets a board that looks empty
// rather than a refusal naming `any`.
//
// `null` and `not_null` are on every type and are not listed: "is this set"
// is a question about the ROW rather than about the value, so it means the
// same thing whatever the column holds.
func fieldOpsFor(t FieldType) []string {
	switch t {
	case FieldText, FieldURL, FieldEmail:
		return []string{FieldOpEq, FieldOpNe, FieldOpContains,
			FieldOpStartsWith, FieldOpIn}
	case FieldTextarea:
		// NO EQUALITY ON A 16 KiB BODY. Matching one exactly means
		// pasting it into the query, which nobody does — and the
		// comparison would be over a column the value table truncates.
		return []string{FieldOpContains, FieldOpStartsWith}
	case FieldNumber, FieldProgress:
		return []string{FieldOpEq, FieldOpNe, FieldOpLt, FieldOpLte,
			FieldOpGt, FieldOpGte, FieldOpRange}
	case FieldDate:
		// NO `ne` ON A DATE. An instant is stored to the microsecond,
		// so "not this instant" is true of everything and reads as a
		// filter that did nothing.
		return []string{FieldOpEq, FieldOpLt, FieldOpLte, FieldOpGt,
			FieldOpGte, FieldOpRange}
	case FieldDropdown:
		return []string{FieldOpEq, FieldOpNe, FieldOpAny, FieldOpNotAny}
	case FieldLabels:
		// NO `eq` ON A SET. The value is one row per option, so an
		// equality would ask whether the set contains one option while
		// looking like it asked whether the set IS that option.
		return []string{FieldOpAny, FieldOpAll, FieldOpNotAny, FieldOpNotAll}
	case FieldCheckbox:
		return []string{FieldOpEq}
	case FieldRelationship:
		return []string{FieldOpAny, FieldOpAll, FieldOpNotAny}
	case FieldPeople:
		return []string{FieldOpAny, FieldOpAll, FieldOpNotAny, FieldOpMe}
	case FieldRollup:
		// A ROLLUP IS A POST-FILTER over a correlated aggregate — it
		// has no row in the value table at all — so the comparison is
		// applied after the rows are read and only the ordered ones
		// mean anything.
		return []string{FieldOpLt, FieldOpGt, FieldOpRange}
	}
	return FieldOps
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
			field.Options = OptionIDs(candidate.Config.Options)
			field.Labels = make(map[string]string, len(candidate.Config.Options))
			for _, option := range candidate.Config.Options {
				// THE NAME, ALWAYS: a heading is what a person reads,
				// and the slug is what they type. An ARCHIVED option is
				// labelled too, for the reason [OptionIDs] resolves one:
				// its values are still on their tasks, so a board can
				// still draw a column for it — and one headed by a raw
				// id is a column nobody can read.
				field.Labels[option.ID] = option.Name
			}
		}
		return field, true
	}
	return resolvedField{}, false
}

// OptionIDs maps every spelling of an option onto its ID — its own id, its
// slug and its name, case-folded.
//
// ONE SPELLING OF THE RULE, because BOTH SIDES need it and they disagreed. The
// read resolved a caller's word to the option's id before comparing; the write
// stored whatever text it was handed. So a task written as `{region: "eu"}` —
// the option SLUG, which is what a person types and what the read accepts —
// stored "eu" where the filter looked for "o-eu", and that task was invisible
// to every filter, grouping and total on the field it had set.
//
// An ARCHIVED option still resolves. Its values stay on their tasks and a
// filter naming it must still find them; what archiving stops is CHOOSING it,
// which is a rule about a write's validity rather than about lookup.
func OptionIDs(options []Option) map[string]string {
	out := make(map[string]string, len(options)*3)
	for _, option := range options {
		if option.ID == "" {
			continue
		}
		out[strings.ToLower(option.ID)] = option.ID
		if option.Slug != "" {
			out[strings.ToLower(option.Slug)] = option.ID
		}
		if option.Name != "" {
			out[strings.ToLower(option.Name)] = option.ID
		}
	}
	return out
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
	if err := checkFieldOp(filter.Op, field); err != nil {
		return "", nil, err
	}
	if filter.Op == "" {
		// RESOLVED ONCE, HERE, so every arm below reads one operator
		// rather than each remembering that an empty one is a value the
		// type decides — which is how the set arms would have been
		// skipped for the bare form and answered as an equality.
		filter.Op = defaultFieldOp(field.Type)
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

	// THE SET OPERATORS TAKE A LIST and are compiled before the single
	// value is parsed, because there is no single value to parse: their
	// argument is a comma-separated set and each member resolves on its
	// own.
	switch filter.Op {
	case FieldOpAny, FieldOpNotAny, FieldOpIn:
		values, err := fieldValueSet(filter, field)
		if err != nil {
			return "", nil, err
		}
		clause, args, err := inner(column+" IN ("+placeholders(len(values))+")",
			values...)
		if err != nil || filter.Op != FieldOpNotAny {
			return clause, args, err
		}
		// NOT_ANY IS "NO ROW IS ONE OF THESE", never "some row is not
		// one of these": a labels field holding two options satisfies
		// the second for every option it holds beside the excluded one,
		// so the negation has to be of the whole membership.
		return negated(clause), args, nil
	case FieldOpAll, FieldOpNotAll:
		values, err := fieldValueSet(filter, field)
		if err != nil {
			return "", nil, err
		}
		// ALL IS A COUNT, not an intersection: one subquery counting the
		// DISTINCT members a task holds from the set, compared against
		// the size of the set. Written as N chained IN clauses it would
		// be N subqueries over one index for a question one answers.
		clause, args, err := inner(column+" IN ("+placeholders(len(values))+")",
			values...)
		if err != nil {
			return "", nil, err
		}
		all := strings.Replace(clause, "SELECT v.task_id", "SELECT v.task_id", 1)
		all = strings.TrimSuffix(all, ")") +
			" GROUP BY v.task_id HAVING COUNT(DISTINCT " + column + ") = ?)"
		args = append(args, len(values))
		if filter.Op == FieldOpNotAll {
			return negated(all), args, nil
		}
		return all, args, nil
	case FieldOpRange:
		from, to, err := fieldValueRange(filter, field)
		if err != nil {
			return "", nil, err
		}
		return inner(column+" >= ? AND "+column+" <= ?", from, to)
	case FieldOpMe:
		// A VIEWER IS RESOLVED BY THE SURFACE from its own credential,
		// before the query is parsed — see [Reader.Expand], which is
		// also where `preset=my_queue` resolves the same word. One
		// reaching here is a surface that read without expanding, and
		// answering it as a literal would match the tasks whose people
		// field holds a person called "me".
		return "", nil, fmt.Errorf("tracker: `f.%s=me` reached the reader "+
			"unresolved — a surface resolves the viewer from its own "+
			"credential before it reads, which is what makes a saved view "+
			"mean the reader rather than whoever saved it", field.Slug)
	}

	value, err := fieldValueArg(filter, field)
	if err != nil {
		return "", nil, err
	}
	switch filter.Op {
	case FieldOpEq:
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
		// LIKE WITH THE VALUE BOUND and the wildcards composed here, so
		// a caller's own `%` is escaped rather than becoming a pattern.
		return inner(column+` LIKE ? ESCAPE '\'`,
			"%"+likeEscape(fmt.Sprint(value))+"%")
	case FieldOpStartsWith:
		// ONE TRAILING WILDCARD, which is the whole difference from
		// `contains`: a prefix is a RANGE over the column and an index
		// can serve it, where a leading `%` can only be scanned.
		return inner(column+` LIKE ? ESCAPE '\'`,
			likeEscape(fmt.Sprint(value))+"%")
	}
	return "", nil, fmt.Errorf("tracker: %q is not a field comparison — the "+
		"seventeen are %s", filter.Op, strings.Join(FieldOps, ", "))
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

// checkFieldOp refuses an operator the field's TYPE does not admit.
//
// NAMING WHAT IT DOES ADMIT, because the failure this replaces is silent: a
// `labels` field compared with `eq` produced a clause that matched nothing,
// and a board that came back empty reads as "no task has this label" rather
// than as "that is not a question you can ask of a set".
func checkFieldOp(op string, field resolvedField) error {
	// "IS IT SET" IS A QUESTION ABOUT THE ROW rather than about the value,
	// so it means the same thing on every type.
	if op == FieldOpNull || op == FieldOpNotNull {
		return nil
	}
	if op == "" {
		op = defaultFieldOp(field.Type)
	}
	allowed := fieldOpsFor(field.Type)
	if slices.Contains(allowed, op) {
		return nil
	}
	if !slices.Contains(FieldOps, op) {
		return fmt.Errorf("tracker: %q is not a field comparison — the "+
			"seventeen are %s", op, strings.Join(FieldOps, ", "))
	}
	return fmt.Errorf("tracker: f.%s is a %s, and %q is not a comparison it "+
		"admits — a %s takes %s, plus null and not_null",
		field.Slug, field.Type, op, field.Type, strings.Join(allowed, ", "))
}

// defaultFieldOp is what a bare `f.<slug>=<value>` means.
//
// THE TYPE'S NATURAL COMPARISON, not equality everywhere: a caller writing
// `f.areas=api` is naming a value, not claiming the set IS that value — so on
// a multi-valued field the bare form is `any`, which is what they meant. An
// EXPLICIT `eq:` on one is still refused, because that one is a claim.
func defaultFieldOp(t FieldType) string {
	switch t {
	case FieldLabels, FieldRelationship, FieldPeople:
		return FieldOpAny
	}
	return FieldOpEq
}

// fieldValueSet parses a set operator's comma-separated argument.
//
// EACH MEMBER RESOLVES ON ITS OWN, through the same three-tier rule one value
// takes: a caller writing `f.areas=any:api,UI` means two options, and one of
// them is spelled by its name.
func fieldValueSet(filter FieldFilter, field resolvedField) ([]any, error) {
	members := csv(filter.Value)
	if len(members) == 0 {
		return nil, fmt.Errorf("tracker: `f.%s=%s:` names no values — a set "+
			"comparison with nothing in it matches nothing and reads as a "+
			"field nobody has set", field.Slug, filter.Op)
	}
	if len(members) > MaxFieldSetMembers {
		return nil, fmt.Errorf("tracker: `f.%s=%s:` names %d values and at "+
			"most %d are compared at once — an N-way OR across one index is "+
			"the shape the planner handles worst",
			field.Slug, filter.Op, len(members), MaxFieldSetMembers)
	}
	out := make([]any, 0, len(members))
	for _, member := range members {
		value, err := fieldValueArg(FieldFilter{Value: member}, field)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

// fieldValueRange parses `from..to`.
//
// TWO BOUNDS IN ONE FILTER rather than a `gte` and an `lte` a caller pairs up
// themselves — and the pairing is what they get wrong: `gte` alone on a date
// field is the single most common way to ask for "this quarter" and get
// everything since it.
func fieldValueRange(filter FieldFilter, field resolvedField) (any, any, error) {
	from, to, found := strings.Cut(filter.Value, "..")
	if !found {
		return nil, nil, fmt.Errorf("tracker: `f.%s=range:` is written "+
			"`<from>..<to>` — both ends, because a range with one is `gte` "+
			"or `lte`", field.Slug)
	}
	low, err := fieldValueArg(FieldFilter{Value: strings.TrimSpace(from)}, field)
	if err != nil {
		return nil, nil, err
	}
	high, err := fieldValueArg(FieldFilter{Value: strings.TrimSpace(to)}, field)
	if err != nil {
		return nil, nil, err
	}
	return low, high, nil
}

// MaxFieldSetMembers bounds a set comparison.
//
// SIXTEEN, which is [MaxAnyBranches] doubled: a set operator compiles to ONE
// `IN` over one index rather than to an N-way OR across several, so it is
// cheaper per member than a disjunction — but it is still a list the planner
// expands, and a label filter naming more than sixteen options is a filter
// nobody typed.
const MaxFieldSetMembers = 16
