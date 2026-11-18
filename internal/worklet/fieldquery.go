package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// Resolving and compiling an `f.<ref>` filter.
//
// What is legal depends on the field's declared type, which neither the parser
// nor [compile] has: [compile] stays a pure function so a plan can be explained
// without a store. So [Reader.Tasks] resolves inside its read transaction and
// hands [compile] a map, one read of the declarations per query.
//
// A ref resolves by slug, then id, then case-folded name, as [agent/colleague]
// resolves a handle: a query that is exactly a slug is never fuzzy-matched.
// An unresolved ref is a refusal naming it, never an ignored clause, which
// would silently show more than was asked for.

// resolvedField is one declared field as a filter needs it.
type resolvedField struct {
	ID   string
	Slug string
	Type FieldType

	// Options maps an option's slug and case-folded name to its ID: a stored
	// value holds the ID and a caller types the word.
	Options map[string]string

	// Labels maps the ID to the option's name. Not an inversion of Options,
	// which holds several keys per option: inverting it would pick one by
	// map iteration order, and a column heading would vary between requests.
	Labels map[string]string

	// Multi marks a field a task can hold several values of, so a board
	// grouped on one puts the task on every column it chose.
	Multi bool
}

// The field-filter operators.
//
// A closed set: an operator is the one thing [compile] composes into SQL
// rather than binding, so it has to be enumerated.
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

	// The set operators, for a multi-valued field: its value is one row per
	// option, so `eq` would say nothing about the other options it holds.
	FieldOpAny    = "any"
	FieldOpAll    = "all"
	FieldOpNotAny = "not_any"
	FieldOpNotAll = "not_all"

	// FieldOpIn is `any` over a single-valued text column, spelled apart
	// because "the text is one of these" and "the set contains one of
	// these" are different questions.
	FieldOpIn = "in"

	// FieldOpStartsWith is the prefix comparison, which an index can serve
	// where `contains` never can.
	FieldOpStartsWith = "startswith"

	// FieldOpRange is the closed interval `from..to`, so a caller need not
	// pair a `gte` and an `lte` (see [fieldValueRange]).
	FieldOpRange = "range"

	// FieldOpMe is the viewer, on a `people` field. It takes no value: a
	// saved query naming a handle would read as that person's for everyone.
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
// An operator that does not apply would match nothing and read as "no task
// has this", so it is refused per type ([checkFieldOp]). `null` and
// `not_null` apply to every type and are not listed.
func fieldOpsFor(t FieldType) []string {
	switch t {
	case FieldText, FieldURL, FieldEmail:
		return []string{FieldOpEq, FieldOpNe, FieldOpContains,
			FieldOpStartsWith, FieldOpIn}
	case FieldTextarea:
		// No equality on a 16 KiB body: nobody pastes one into a query,
		// and the value table truncates the column it would compare.
		return []string{FieldOpContains, FieldOpStartsWith}
	case FieldNumber, FieldProgress:
		return []string{FieldOpEq, FieldOpNe, FieldOpLt, FieldOpLte,
			FieldOpGt, FieldOpGte, FieldOpRange}
	case FieldDate:
		// No `ne` on a date: stored to the microsecond, "not this
		// instant" is true of everything.
		return []string{FieldOpEq, FieldOpLt, FieldOpLte, FieldOpGt,
			FieldOpGte, FieldOpRange}
	case FieldDropdown:
		return []string{FieldOpEq, FieldOpNe, FieldOpAny, FieldOpNotAny}
	case FieldLabels:
		// No `eq` on a set: one row per option, it would test membership
		// while reading as "the set is that option".
		return []string{FieldOpAny, FieldOpAll, FieldOpNotAny, FieldOpNotAll}
	case FieldCheckbox:
		return []string{FieldOpEq}
	case FieldRelationship:
		return []string{FieldOpAny, FieldOpAll, FieldOpNotAny}
	case FieldPeople:
		return []string{FieldOpAny, FieldOpAll, FieldOpNotAny, FieldOpMe}
	case FieldRollup:
		// A rollup has no row in the value table: it is a post-filter
		// over a correlated aggregate, where only ordering means anything.
		return []string{FieldOpLt, FieldOpGt, FieldOpRange}
	}
	return FieldOps
}

// errUnresolvedField marks an `f.<ref>` nothing declares, apart from an
// unreadable store: a pinned view's count names the first on the view's row
// and fails the read on the second.
var errUnresolvedField = errors.New("no such field")

// resolveFields reads the declarations every `f.<ref>` in a query names.
//
// The whole query, disjunction branches included, in one read: every branch
// resolves against the same catalogue.
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
			// An archived field's values are hidden from every index, so
			// a filter on it would match nothing rather than be refused.
			continue
		}
		bySlug[field.Slug] = field
		byName[strings.ToLower(field.Name)] = field
	}
	out := make(map[string]resolvedField, len(refs))
	for ref := range refs {
		field, held := resolveOneField(ref, declared, bySlug, byName)
		if !held {
			return nil, fmt.Errorf("worklet: f.%s names no field this company "+
				"declares — a filter nothing resolved would widen this answer "+
				"silently, so it is refused; read the catalogue for the slugs "+
				"that exist: %w", ref, errUnresolvedField)
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
				// The name, which a person reads. An archived option is
				// labelled too: its values remain on tasks ([OptionIDs]),
				// so a board can still draw its column.
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
// The read and the write both resolve through it: a write storing the slug
// where a filter looks for the id makes that task invisible to every filter,
// grouping and total on the field.
//
// An archived option still resolves: its values stay on their tasks. What
// archiving stops is choosing it, a rule about a write's validity.
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
	// The sort too: [sortTerms] silently drops an unresolved term and
	// answers in the default order.
	for _, sort := range q.Sort {
		if ref, ok := strings.CutPrefix(sort.Key, FieldKeyPrefix); ok && ref != "" {
			into[ref] = true
		}
	}
	// The totals too: an unresolved `f.<slug>:sum` would read as zero.
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

// fieldClause compiles one custom-field filter: an IN, not a correlated
// EXISTS, which leads with the primary key `task_id` and never reads the
// partial indexes. The standalone subquery seeks `(field_id, <column>) WHERE
// hidden = 0` on [FieldValueColumn]'s column, or lets the tasks drive.
//
// It names its index ([fieldValueIndex]): nothing runs ANALYZE, and without
// statistics the planner seeks `field_id` alone on whichever partial index it
// costs first, reading every value of the field. `INDEXED BY` is also a guard:
// a dropped index fails with `no such index`, and a read no longer implying
// its predicate ([liveFieldValue]) with `no query solution`.
func fieldClause(filter FieldFilter, field resolvedField, on alias) (string, []any, error) {
	column := "v." + FieldValueColumn(field.Type)
	inner := func(predicate string, args ...any) (string, []any, error) {
		return on.col("id") + " IN (SELECT v.task_id FROM tracker_field_values v " +
			"INDEXED BY " + fieldValueIndex(field.Type) + " " +
			"WHERE v.field_id = ? AND " + liveFieldValue + " AND " +
			predicate + ")", append([]any{field.ID}, args...), nil
	}
	if err := checkFieldOp(filter.Op, field); err != nil {
		return "", nil, err
	}
	if filter.Op == "" {
		// Resolved once here, or the set arms would miss the bare form
		// and answer it as an equality.
		filter.Op = defaultFieldOp(field.Type)
	}
	switch filter.Op {
	case FieldOpNull:
		// Unset is the absence of a row: the apply writes none for a
		// value that decoded to nothing.
		clause, args, err := inner(column + " IS NOT NULL")
		// NOT IN is safe only because `task_id` is NOT NULL: one NULL in
		// the subquery would make the predicate match nothing.
		return negated(clause, on), args, err
	case FieldOpNotNull:
		return inner(column + " IS NOT NULL")
	}

	// The set operators take a list, so they compile before a single
	// value is parsed.
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
		// not_any is "no row is one of these", never "some row is not
		// one of these", which a multi-valued field almost always meets.
		return negated(clause, on), args, nil
	case FieldOpAll, FieldOpNotAll:
		values, err := fieldValueSet(filter, field)
		if err != nil {
			return "", nil, err
		}
		// all is a count of the distinct members held, compared with the
		// set's size: one subquery rather than N chained IN clauses.
		clause, args, err := inner(column+" IN ("+placeholders(len(values))+")",
			values...)
		if err != nil {
			return "", nil, err
		}
		// The GROUP BY and HAVING go inside the subquery's parentheses,
		// so its closing paren comes off first. The select list stays
		// the task id: the HAVING reads the column directly.
		all := strings.TrimSuffix(clause, ")") +
			" GROUP BY v.task_id HAVING COUNT(DISTINCT " + column + ") = ?)"
		args = append(args, len(values))
		if filter.Op == FieldOpNotAll {
			return negated(all, on), args, nil
		}
		return all, args, nil
	case FieldOpRange:
		from, to, err := fieldValueRange(filter, field)
		if err != nil {
			return "", nil, err
		}
		return inner(column+" >= ? AND "+column+" <= ?", from, to)
	case FieldOpMe:
		// The surface resolves the viewer from its credential before
		// parsing ([Reader.Expand]). One reaching here was not expanded,
		// and as a literal it would match a person called "me".
		return "", nil, fmt.Errorf("worklet: `f.%s=me` reached the reader "+
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
		// ne is "no row equals it", never "some row differs", which a
		// multi-valued field almost always meets.
		clause, args, err := inner(column+" = ?", value)
		return negated(clause, on), args, err
	case FieldOpLt:
		return inner(column+" < ?", value)
	case FieldOpLte:
		return inner(column+" <= ?", value)
	case FieldOpGt:
		return inner(column+" > ?", value)
	case FieldOpGte:
		return inner(column+" >= ?", value)
	case FieldOpContains:
		// The value is escaped, so a caller's own `%` is not a pattern.
		return inner(column+` LIKE ? ESCAPE '\'`,
			"%"+likeEscape(fmt.Sprint(value))+"%")
	case FieldOpStartsWith:
		// Trailing wildcard only: a prefix is a range an index can serve.
		return inner(column+` LIKE ? ESCAPE '\'`,
			likeEscape(fmt.Sprint(value))+"%")
	}
	return "", nil, fmt.Errorf("worklet: %q is not a field comparison; the "+
		"seventeen are %s", filter.Op, strings.Join(FieldOps, ", "))
}

// fieldValueArg turns the caller's text into the column's own type.
//
// The declaration decides, not the text: `f.impact=3` binds 3 on a number
// field and "3" on a text one, and the wrong one silently matches nothing.
func fieldValueArg(filter FieldFilter, field resolvedField) (any, error) {
	value := strings.TrimSpace(filter.Value)
	switch FieldValueColumn(field.Type) {
	case "num":
		if field.Type == FieldCheckbox {
			// A checkbox takes a word, as the apply does.
			switch strings.ToLower(value) {
			case "true", "yes", "1", "checked":
				return float64(1), nil
			case "false", "no", "0", "unchecked":
				return float64(0), nil
			}
		}
		number, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("worklet: f.%s is a %s and %q is not a "+
				"number", field.Slug, field.Type, clipValue(value))
		}
		return number, nil
	case "at":
		for _, layout := range []string{time.RFC3339, "2006-01-02"} {
			if at, err := time.Parse(layout, value); err == nil {
				return store.EncodeTime(at.UTC()), nil
			}
		}
		return nil, fmt.Errorf("worklet: f.%s is a date and %q is not one; "+
			"write 2026-06-30 or a full RFC3339 instant",
			field.Slug, clipValue(value))
	case "ref":
		// The row holds the option's id, so renaming an option keeps
		// every task and filter that names it.
		if id, held := field.Options[strings.ToLower(value)]; held {
			return id, nil
		}
		// An unknown word passes through: a relationship or people field
		// has no options, its ref is a task id or a handle.
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
// Textual, on the one form [fieldClause] emits; the same to SQLite as a
// wrapping `NOT (…)`, and reads as what it is.
func negated(clause string, on alias) string {
	return strings.Replace(clause, on.col("id")+" IN (", on.col("id")+" NOT IN (", 1)
}

// checkFieldOp refuses an operator the field's TYPE does not admit.
//
// It names what the type does admit; an inapplicable operator would match
// nothing and read as "no task has this".
func checkFieldOp(op string, field resolvedField) error {
	// "Is it set" is about the row, not the value, so every type admits it.
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
		return fmt.Errorf("worklet: %q is not a field comparison; the "+
			"seventeen are %s", op, strings.Join(FieldOps, ", "))
	}
	return fmt.Errorf("worklet: f.%s is a %s, and %q is not a comparison it "+
		"admits — a %s takes %s, plus null and not_null",
		field.Slug, field.Type, op, field.Type, strings.Join(allowed, ", "))
}

// defaultFieldOp is what a bare `f.<slug>=<value>` means.
//
// The type's natural comparison: on a multi-valued field `f.areas=api` names
// a member, so it is `any`. An explicit `eq:` on one is still refused.
func defaultFieldOp(t FieldType) string {
	switch t {
	case FieldLabels, FieldRelationship, FieldPeople:
		return FieldOpAny
	}
	return FieldOpEq
}

// fieldValueSet parses a set operator's comma-separated argument.
//
// Each member resolves on its own, as one value does: `f.areas=any:api,UI`
// may spell one option by slug and another by name.
func fieldValueSet(filter FieldFilter, field resolvedField) ([]any, error) {
	members := csv(filter.Value)
	if len(members) == 0 {
		return nil, fmt.Errorf("worklet: `f.%s=%s:` names no values; a set "+
			"comparison with nothing in it matches nothing and reads as a "+
			"field nobody has set", field.Slug, filter.Op)
	}
	if len(members) > MaxFieldSetMembers {
		return nil, fmt.Errorf("worklet: `f.%s=%s:` names %d values and at "+
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
// Two bounds in one filter, because callers pairing `gte` and `lte` forget
// one: `gte` alone asks for "this quarter" and gets everything since.
func fieldValueRange(filter FieldFilter, field resolvedField) (any, any, error) {
	from, to, found := strings.Cut(filter.Value, "..")
	if !found {
		return nil, nil, fmt.Errorf("worklet: `f.%s=range:` is written "+
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
// Sixteen, [MaxAnyBranches] doubled: one `IN` over one index is cheaper per
// member than a disjunction, but still a list the planner expands, and more
// than sixteen options is a filter nobody typed.
const MaxFieldSetMembers = 16
