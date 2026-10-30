package dashspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/words"
)

// ---- calculated datasets -------------------------------------------------- //
//
// A CALCULATION IS A FEW DECLARED STEPS OVER ROWS, NEVER A PROGRAM. Each step
// is one of a closed set ([Ops]), reads columns its input declares and
// declares the columns it answers, so a widget over a calculated dataset is
// validated at publish against columns the steps promise, and a viewer reads
// the steps in the Sources rail as the formula they are.
//
// DETERMINISTIC BY CONSTRUCTION: no step reads a clock, a locale or a random
// source; a group's rows come out ordered by their keys under [compare], a
// sort is stable, a sum adds in row order, and a join takes the first match
// in the right side's order. One input and one spec answer one table on every
// node and in every run.

// Calc is a calculated dataset: the dataset it starts from and its steps.
type Calc struct {
	From  string `json:"from"`
	Steps []Step `json:"steps"`
}

// Op is what one step does.
type Op string

// The steps a calculation may take.
const (
	// OpFilter keeps the rows every condition holds for.
	OpFilter Op = "filter"

	// OpGroup answers one row per distinct value of the `by` columns, each
	// with its aggregates.
	OpGroup Op = "group"

	// OpCompute adds (or replaces) one column from an arithmetic of two
	// operands, or a date cut to its day, week or month.
	OpCompute Op = "compute"

	// OpJoin adds columns of another dataset to each row whose key matches
	// (a left join: a row with no match keeps nil in them).
	OpJoin Op = "join"

	// OpSort orders the rows by columns, stably.
	OpSort Op = "sort"

	// OpLimit keeps the first rows.
	OpLimit Op = "limit"

	// OpSelect keeps the named columns, in the order named.
	OpSelect Op = "select"
)

// Ops are every step a calculation may take.
var Ops = []Op{OpFilter, OpGroup, OpCompute, OpJoin, OpSort, OpLimit, OpSelect}

// Valid reports whether o is a step this build runs.
func (o Op) Valid() bool { return slices.Contains(Ops, o) }

// Step is one step of a calculation. A FLAT shape, as [Widget] is: each op
// reads the fields it names and refuses the rest.
type Step struct {
	Op Op `json:"op"`

	// filter
	Where []Condition `json:"where,omitempty"`

	// group
	By         []string    `json:"by,omitempty"`
	Aggregates []Aggregate `json:"aggregates,omitempty"`

	// compute
	Column string    `json:"column,omitempty"`
	Fn     ComputeFn `json:"fn,omitempty"`
	Left   *Operand  `json:"left,omitempty"`
	Right  *Operand  `json:"right,omitempty"`

	// join
	With    string   `json:"with,omitempty"`
	On      string   `json:"on,omitempty"`
	Columns []string `json:"columns,omitempty"`

	// sort
	Order []SortKey `json:"order,omitempty"`

	// limit
	Count int `json:"count,omitempty"`
}

// Comparison is how a condition compares a column to its value.
type Comparison string

// The comparisons a filter may make.
const (
	CmpEq      Comparison = "eq"
	CmpNe      Comparison = "ne"
	CmpLt      Comparison = "lt"
	CmpLe      Comparison = "le"
	CmpGt      Comparison = "gt"
	CmpGe      Comparison = "ge"
	CmpPresent Comparison = "present" // the value is not nil
	CmpAbsent  Comparison = "absent"  // the value is nil
)

// Comparisons are every comparison a filter makes.
var Comparisons = []Comparison{CmpEq, CmpNe, CmpLt, CmpLe, CmpGt, CmpGe, CmpPresent, CmpAbsent}

// Valid reports whether c is a comparison this build makes.
func (c Comparison) Valid() bool { return slices.Contains(Comparisons, c) }

// unary reports a comparison that takes no value.
func (c Comparison) unary() bool { return c == CmpPresent || c == CmpAbsent }

// Condition is one test a filter keeps a row by. Value is a literal, or a
// filter of the board ([Arg]); a condition bound to a filter whose value is
// empty (an "All" choice) holds for every row.
type Condition struct {
	Column string     `json:"column"`
	Cmp    Comparison `json:"cmp"`
	Value  *Arg       `json:"value,omitempty"`
}

// Aggregate is one column a group answers: Agg folded over Of, written as
// Column. A count names no Of and counts the group's rows.
type Aggregate struct {
	Column string `json:"column"`
	Agg    Agg    `json:"agg"`
	Of     string `json:"of,omitempty"`
}

// ComputeFn is what a compute step derives.
type ComputeFn string

// The functions a compute step may apply.
const (
	FnAdd   ComputeFn = "add"
	FnSub   ComputeFn = "sub"
	FnMul   ComputeFn = "mul"
	FnDiv   ComputeFn = "div"   // nil where the right side is zero or absent
	FnDay   ComputeFn = "day"   // a date cut to its UTC day
	FnWeek  ComputeFn = "week"  // a date cut to the Monday of its ISO week, UTC
	FnMonth ComputeFn = "month" // a date cut to the first of its month, UTC
)

// ComputeFns are every function a compute step applies.
var ComputeFns = []ComputeFn{FnAdd, FnSub, FnMul, FnDiv, FnDay, FnWeek, FnMonth}

// Valid reports whether f is a function this build computes.
func (f ComputeFn) Valid() bool { return slices.Contains(ComputeFns, f) }

// dateFn reports a function over one date.
func (f ComputeFn) dateFn() bool { return f == FnDay || f == FnWeek || f == FnMonth }

// Operand is one side of an arithmetic: a column of the row, or a number.
type Operand struct {
	Column string
	Number *float64
}

// UnmarshalJSON reads a string as a column and a number as a literal.
func (o *Operand) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var col string
		if err := json.Unmarshal(raw, &col); err != nil {
			return err
		}
		*o = Operand{Column: col}
		return nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return errors.New("an operand is a column (a string) or a number")
	}
	*o = Operand{Number: &f}
	return nil
}

// MarshalJSON writes the form [Operand.UnmarshalJSON] reads.
func (o Operand) MarshalJSON() ([]byte, error) {
	if o.Number != nil {
		return json.Marshal(*o.Number)
	}
	return json.Marshal(o.Column)
}

// SortKey is one column a sort orders by.
type SortKey struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc,omitempty"`
}

// ---- columns a step answers ----------------------------------------------- //

// stepColumns are the columns step answers over input columns in, and the key
// it keeps, or a finding message naming what is wrong. with is the joined
// dataset's columns, for a join. It is the one place a step's shape is
// decided: validation asks it at publish, and [Run] asks it again for every
// table it answers.
func stepColumns(st Step, in []Column, key string, with *Table) ([]Column, string, string) {
	col := func(k string) (Column, bool) {
		i := slices.IndexFunc(in, func(c Column) bool { return c.Key == k })
		if i < 0 {
			return Column{}, false
		}
		return in[i], true
	}
	switch st.Op {
	case OpFilter:
		if len(st.Where) == 0 {
			return nil, "", "a filter names at least one condition in `where`"
		}
		for i, w := range st.Where {
			c, ok := col(w.Column)
			switch {
			case !ok:
				return nil, "", fmt.Sprintf("where[%d] names the column %q, which the rows do not have",
					i, w.Column)
			case !w.Cmp.Valid():
				return nil, "", fmt.Sprintf("where[%d].cmp is one of %s", i, words.Or(Comparisons))
			case w.Cmp.unary() && w.Value != nil:
				return nil, "", fmt.Sprintf("where[%d] is %s, which takes no value", i, w.Cmp)
			case !w.Cmp.unary() && w.Value == nil:
				return nil, "", fmt.Sprintf("where[%d] is %s, which takes a value", i, w.Cmp)
			case w.Value != nil && w.Value.Param == "":
				if _, err := Coerce(c.Type, w.Value.Literal); err != nil {
					return nil, "", fmt.Sprintf("where[%d].value is not a %s: %v", i, c.Type, err)
				}
			}
		}
		return in, key, ""
	case OpGroup:
		if len(st.By) == 0 && len(st.Aggregates) == 0 {
			return nil, "", "a group names its `by` columns, its `aggregates`, or both"
		}
		var out []Column
		for _, b := range st.By {
			c, ok := col(b)
			if !ok {
				return nil, "", fmt.Sprintf("by names the column %q, which the rows do not have", b)
			}
			if slices.ContainsFunc(out, func(o Column) bool { return o.Key == b }) {
				return nil, "", fmt.Sprintf("by names %q twice", b)
			}
			out = append(out, c)
		}
		for i, a := range st.Aggregates {
			if !ValidColumnKey(a.Column) {
				return nil, "", fmt.Sprintf("aggregates[%d].column %q is not a column key", i, a.Column)
			}
			if slices.ContainsFunc(out, func(o Column) bool { return o.Key == a.Column }) {
				return nil, "", fmt.Sprintf("aggregates[%d] writes %q, which the group already answers",
					i, a.Column)
			}
			t, why := aggType(a.Agg, a.Of, col)
			if why != "" {
				return nil, "", fmt.Sprintf("aggregates[%d] %s", i, why)
			}
			out = append(out, Column{Key: a.Column, Type: t})
		}
		// A GROUP'S KEY is its one `by` column: two would name a row by a
		// pair no anchor addresses.
		if len(st.By) == 1 {
			return out, st.By[0], ""
		}
		return out, "", ""
	case OpCompute:
		if !ValidColumnKey(st.Column) {
			return nil, "", fmt.Sprintf("column %q is not a column key", st.Column)
		}
		if !st.Fn.Valid() {
			return nil, "", "fn is one of " + words.Or(ComputeFns)
		}
		var t ColumnType
		if st.Fn.dateFn() {
			if st.Left == nil || st.Left.Column == "" || st.Right != nil {
				return nil, "", fmt.Sprintf("%s takes one date column as `left` and no `right`", st.Fn)
			}
			c, ok := col(st.Left.Column)
			if !ok || c.Type != ColumnDate {
				return nil, "", fmt.Sprintf("%s reads a date column, and %q is not one", st.Fn, st.Left.Column)
			}
			t = ColumnDate
		} else {
			for _, operand := range []struct {
				side string
				o    *Operand
			}{{"left", st.Left}, {"right", st.Right}} {
				side, o := operand.side, operand.o
				if o == nil {
					return nil, "", fmt.Sprintf("%s takes a `left` and a `right`", st.Fn)
				}
				if o.Number != nil {
					if math.IsNaN(*o.Number) || math.IsInf(*o.Number, 0) {
						return nil, "", side + " is not a finite number"
					}
					continue
				}
				if c, ok := col(o.Column); !ok || c.Type != ColumnNumber {
					return nil, "", fmt.Sprintf("%s reads number columns, and %s %q is not one", st.Fn,
						side, o.Column)
				}
			}
			t = ColumnNumber
		}
		out := slices.DeleteFunc(slices.Clone(in), func(c Column) bool { return c.Key == st.Column })
		return append(out, Column{Key: st.Column, Type: t}), keyAfter(key, st.Column), ""
	case OpJoin:
		if with == nil {
			return nil, "", "a join names the dataset it joins in `with`"
		}
		c, ok := col(st.On)
		if !ok {
			return nil, "", fmt.Sprintf("on names the column %q, which the rows do not have", st.On)
		}
		wc, ok := with.column(st.On)
		if !ok || wc.Type != c.Type {
			return nil, "", fmt.Sprintf("on names %q, which %q does not hold as a %s", st.On, st.With, c.Type)
		}
		if len(st.Columns) == 0 {
			return nil, "", "a join names the columns it adds in `columns`"
		}
		out := slices.Clone(in)
		for _, k := range st.Columns {
			jc, ok := with.column(k)
			switch {
			case !ok:
				return nil, "", fmt.Sprintf("columns names %q, which %q does not have", k, st.With)
			case k == st.On:
				return nil, "", fmt.Sprintf("columns names the join's own key %q", k)
			case slices.ContainsFunc(out, func(o Column) bool { return o.Key == k }):
				return nil, "", fmt.Sprintf("columns adds %q, which the rows already have", k)
			}
			out = append(out, jc)
		}
		return out, key, ""
	case OpSort:
		if len(st.Order) == 0 {
			return nil, "", "a sort names at least one column in `order`"
		}
		for i, o := range st.Order {
			if _, ok := col(o.Column); !ok {
				return nil, "", fmt.Sprintf("order[%d] names the column %q, which the rows do not have",
					i, o.Column)
			}
		}
		return in, key, ""
	case OpLimit:
		if st.Count < 1 || st.Count > artifacts.MaxDatasetRows {
			return nil, "", fmt.Sprintf("count is 1 to %d", artifacts.MaxDatasetRows)
		}
		return in, key, ""
	case OpSelect:
		if len(st.Columns) == 0 {
			return nil, "", "a select names the columns it keeps in `columns`"
		}
		var out []Column
		for _, k := range st.Columns {
			c, ok := col(k)
			if !ok {
				return nil, "", fmt.Sprintf("columns names %q, which the rows do not have", k)
			}
			if slices.ContainsFunc(out, func(o Column) bool { return o.Key == k }) {
				return nil, "", fmt.Sprintf("columns names %q twice", k)
			}
			out = append(out, c)
		}
		if !slices.Contains(st.Columns, key) {
			key = ""
		}
		return out, key, ""
	}
	return nil, "", "op is one of " + words.Or(Ops)
}

// keyAfter is the key a step that writes column leaves: none, when it
// rewrote the key itself.
func keyAfter(key, written string) string {
	if key == written {
		return ""
	}
	return key
}

// aggType is the type agg answers over the column of, or why it cannot.
func aggType(agg Agg, of string, col func(string) (Column, bool)) (ColumnType, string) {
	if !agg.Valid() {
		return "", "agg is one of " + words.Or(Aggs)
	}
	if agg == AggCount {
		if of != "" {
			if _, ok := col(of); !ok {
				return "", fmt.Sprintf("of names the column %q, which the rows do not have", of)
			}
		}
		return ColumnNumber, ""
	}
	c, ok := col(of)
	if !ok {
		return "", fmt.Sprintf("%s names the column it folds in `of`, and %q is not one the rows have",
			agg, of)
	}
	switch agg {
	case AggSum, AggAvg:
		if c.Type != ColumnNumber {
			return "", fmt.Sprintf("%s folds a number column, and %q is a %s", agg, of, c.Type)
		}
		return ColumnNumber, ""
	case AggMin, AggMax:
		if c.Type == ColumnBoolean {
			return "", fmt.Sprintf("%s folds a number, date or text column, and %q is a boolean", agg, of)
		}
	}
	return c.Type, ""
}

// ---- running a calculation ------------------------------------------------ //

// Inputs answers a calculated dataset's inputs, each already read as the
// viewer: [Run] never reads a source itself.
type Inputs func(dataset string) (Table, error)

// Run is calculated dataset d's table over its inputs, with the board's
// resolved filters. An input that could not be read is the calculation's
// failure, wrapped, so a refusal of an input reaches the viewer as the reason
// the figure is not drawn.
func Run(d Dataset, params map[string]string, inputs Inputs) (Table, error) {
	if d.Source != SourceCalc || d.Calc == nil {
		return Table{}, fmt.Errorf("%w: %q is not a calculated dataset", ErrInvalid, d.ID)
	}
	t, err := inputs(d.Calc.From)
	if err != nil {
		return Table{}, err
	}
	t.Rows = slices.Clone(t.Rows)
	for i, st := range d.Calc.Steps {
		var with *Table
		if st.Op == OpJoin {
			w, err := inputs(st.With)
			if err != nil {
				return Table{}, err
			}
			with = &w
		}
		cols, key, why := stepColumns(st, t.Columns, t.Key, with)
		if why != "" {
			return Table{}, fmt.Errorf("%w: %s.calc.steps[%d]: %s", ErrInvalid, d.ID, i, why)
		}
		rows, err := runStep(st, t, cols, params, with)
		if err != nil {
			return Table{}, fmt.Errorf("%s.calc.steps[%d]: %w", d.ID, i, err)
		}
		t = Table{Columns: cols, Key: key, Rows: rows, Truncated: t.Truncated || (with != nil && with.Truncated)}
	}
	if len(t.Rows) > artifacts.MaxDatasetRows {
		t.Rows, t.Truncated = t.Rows[:artifacts.MaxDatasetRows], true
	}
	return t, nil
}

// runStep is one step's rows.
func runStep(st Step, t Table, cols []Column, params map[string]string, with *Table) ([]Row, error) {
	typeOf := func(k string) ColumnType {
		c, _ := t.column(k)
		return c.Type
	}
	switch st.Op {
	case OpFilter:
		conds := make([]resolvedCondition, 0, len(st.Where))
		for _, w := range st.Where {
			rc, keep, err := resolveCondition(w, typeOf(w.Column), params)
			if err != nil {
				return nil, err
			}
			if keep {
				conds = append(conds, rc)
			}
		}
		return slices.DeleteFunc(t.Rows, func(r Row) bool {
			for _, c := range conds {
				if !c.holds(r) {
					return true
				}
			}
			return false
		}), nil
	case OpGroup:
		return group(st, t), nil
	case OpCompute:
		out := make([]Row, len(t.Rows))
		for i, r := range t.Rows {
			n := cloneRow(r)
			n[st.Column] = compute(st, r)
			out[i] = n
		}
		return out, nil
	case OpJoin:
		index := map[string]Row{}
		for _, r := range with.Rows {
			k := joinKey(r[st.On])
			if _, seen := index[k]; !seen && r[st.On] != nil {
				index[k] = r
			}
		}
		out := make([]Row, len(t.Rows))
		for i, r := range t.Rows {
			n := cloneRow(r)
			match, ok := index[joinKey(r[st.On])]
			for _, k := range st.Columns {
				if ok && r[st.On] != nil {
					n[k] = match[k]
				} else {
					n[k] = nil
				}
			}
			out[i] = n
		}
		return out, nil
	case OpSort:
		out := slices.Clone(t.Rows)
		slices.SortStableFunc(out, func(a, b Row) int {
			for _, o := range st.Order {
				c := compare(typeOf(o.Column), a[o.Column], b[o.Column])
				if o.Desc {
					c = -c
				}
				if c != 0 {
					return c
				}
			}
			return 0
		})
		return out, nil
	case OpLimit:
		if len(t.Rows) > st.Count {
			return t.Rows[:st.Count], nil
		}
		return t.Rows, nil
	case OpSelect:
		out := make([]Row, len(t.Rows))
		for i, r := range t.Rows {
			n := make(Row, len(cols))
			for _, c := range cols {
				n[c.Key] = r[c.Key]
			}
			out[i] = n
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: op %q", ErrInvalid, st.Op)
}

// joinKey is a canonical value as a map key: type-tagged, so the number 1 and
// the text "1" never meet.
func joinKey(v any) string { return fmt.Sprintf("%T:%v", v, v) }

func cloneRow(r Row) Row {
	n := make(Row, len(r)+1)
	for k, v := range r {
		n[k] = v
	}
	return n
}

// resolvedCondition is a condition with its value coerced for one read.
type resolvedCondition struct {
	column string
	typ    ColumnType
	cmp    Comparison
	value  any
}

// resolveCondition coerces w's value for one read, and reports false for a
// condition bound to a filter whose value is empty, which holds for every row.
func resolveCondition(w Condition, t ColumnType, params map[string]string) (resolvedCondition, bool, error) {
	rc := resolvedCondition{column: w.Column, typ: t, cmp: w.Cmp}
	if w.Cmp.unary() {
		return rc, true, nil
	}
	raw := w.Value.Literal
	if w.Value.Param != "" {
		v, ok := params[w.Value.Param]
		if !ok {
			return rc, false, fmt.Errorf("%w: the condition on %q is bound to the filter %q, which this "+
				"read did not resolve", ErrInvalid, w.Column, w.Value.Param)
		}
		if v == "" {
			return rc, false, nil
		}
		raw = v
	}
	v, err := Coerce(t, raw)
	if err != nil {
		return rc, false, fmt.Errorf("%w: the condition on %q: %w", ErrInvalid, w.Column, err)
	}
	rc.value = v
	return rc, true, nil
}

// holds reports whether the condition holds for r. A nil value compares as
// absent: present, absent and ne hold for it, and no ordering does.
func (c resolvedCondition) holds(r Row) bool {
	v := r[c.column]
	switch c.cmp {
	case CmpPresent:
		return v != nil
	case CmpAbsent:
		return v == nil
	case CmpNe:
		return v == nil || compare(c.typ, v, c.value) != 0
	}
	if v == nil {
		return false
	}
	x := compare(c.typ, v, c.value)
	switch c.cmp {
	case CmpEq:
		return x == 0
	case CmpLt:
		return x < 0
	case CmpLe:
		return x <= 0
	case CmpGt:
		return x > 0
	case CmpGe:
		return x >= 0
	}
	return false
}

// group folds t's rows into one per distinct `by` tuple, ordered by the tuple.
func group(st Step, t Table) []Row {
	type bucket struct {
		keys []any
		rows []Row
	}
	var order []string
	buckets := map[string]*bucket{}
	for _, r := range t.Rows {
		keys := make([]any, len(st.By))
		var id bytes.Buffer
		for i, b := range st.By {
			keys[i] = r[b]
			id.WriteString(joinKey(r[b]))
			id.WriteByte(0)
		}
		b, ok := buckets[id.String()]
		if !ok {
			b = &bucket{keys: keys}
			buckets[id.String()] = b
			order = append(order, id.String())
		}
		b.rows = append(b.rows, r)
	}
	if len(st.By) == 0 && len(order) == 0 {
		// A WHOLE-SET FOLD over no rows is one row of empty folds (a count
		// of zero), not no row: a stat over an empty dataset says 0.
		order = append(order, "")
		buckets[""] = &bucket{}
	}
	slices.SortStableFunc(order, func(a, b string) int {
		ka, kb := buckets[a].keys, buckets[b].keys
		for i, col := range st.By {
			c, _ := t.column(col)
			if x := compare(c.Type, ka[i], kb[i]); x != 0 {
				return x
			}
		}
		return 0
	})
	out := make([]Row, 0, len(order))
	for _, id := range order {
		b := buckets[id]
		r := Row{}
		for i, col := range st.By {
			r[col] = b.keys[i]
		}
		for _, a := range st.Aggregates {
			c, _ := t.column(a.Of)
			r[a.Column] = Fold(a.Agg, c.Type, b.rows, a.Of)
		}
		out = append(out, r)
	}
	return out
}

// Fold is agg over column of rows, a column of type t. A count is a number of
// rows, and a sum over NO ROWS is zero: the source answered, and a total of
// nothing is a measured nought, not a gap. Every other fold over no rows, and
// any fold over rows none of which holds a value, is nil: there is nothing to
// pick or average, and rows whose value nobody recorded total nothing anybody
// measured. The one fold a group and a stat widget both compute, so a stat
// and the calculated dataset behind it cannot disagree.
func Fold(agg Agg, t ColumnType, rows []Row, column string) any {
	if agg == AggSum && len(rows) == 0 {
		return 0.0
	}
	if agg == AggCount {
		n := 0
		for _, r := range rows {
			if column == "" || r[column] != nil {
				n++
			}
		}
		return float64(n)
	}
	var present []any
	for _, r := range rows {
		if v := r[column]; v != nil {
			present = append(present, v)
		}
	}
	if len(present) == 0 {
		return nil
	}
	switch agg {
	case AggFirst:
		return present[0]
	case AggLast:
		return present[len(present)-1]
	case AggSum, AggAvg:
		sum := 0.0
		for _, v := range present {
			f, _ := v.(float64)
			sum += f
		}
		if agg == AggAvg {
			return sum / float64(len(present))
		}
		return sum
	case AggMin, AggMax:
		best := present[0]
		for _, v := range present[1:] {
			c := compare(t, v, best)
			if (agg == AggMin && c < 0) || (agg == AggMax && c > 0) {
				best = v
			}
		}
		return best
	}
	return nil
}

// compute is one compute step's value for r.
func compute(st Step, r Row) any {
	if st.Fn.dateFn() {
		at, ok := instant(r[st.Left.Column])
		if !ok {
			return nil
		}
		at = at.UTC()
		day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		switch st.Fn {
		case FnWeek:
			// ISO 8601: a week starts on Monday.
			back := (int(day.Weekday()) + 6) % 7
			day = day.AddDate(0, 0, -back)
		case FnMonth:
			day = time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
		return day.Format(dayLayout)
	}
	value := func(o *Operand) (float64, bool) {
		if o.Number != nil {
			return *o.Number, true
		}
		f, ok := r[o.Column].(float64)
		return f, ok
	}
	a, okA := value(st.Left)
	b, okB := value(st.Right)
	if !okA || !okB {
		return nil
	}
	var out float64
	switch st.Fn {
	case FnAdd:
		out = a + b
	case FnSub:
		out = a - b
	case FnMul:
		out = a * b
	case FnDiv:
		if b == 0 {
			return nil
		}
		out = a / b
	}
	if math.IsNaN(out) || math.IsInf(out, 0) {
		return nil
	}
	return out
}

// NumberText writes f as a calculation's text form, for a value a source
// parameter or a CSV export takes: the shortest exact decimal.
func NumberText(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
