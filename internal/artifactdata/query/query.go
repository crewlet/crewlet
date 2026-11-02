package query

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifactdata/canon"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/words"
)

// Op is one filter's comparison.
type Op string

// The comparisons a filter can make.
const (
	OpEq       Op = "eq"       // the scalar field equals the value
	OpNe       Op = "ne"       // the scalar field is absent or differs
	OpLt       Op = "lt"       // number or string, ordered
	OpLe       Op = "le"       //
	OpGt       Op = "gt"       //
	OpGe       Op = "ge"       //
	OpIn       Op = "in"       // the scalar field equals one of the values
	OpContains Op = "contains" // an array field holds the value as an element
	OpPrefix   Op = "prefix"   // the string field starts with the value
)

// Ops are every comparison this build compiles.
var Ops = []Op{OpEq, OpNe, OpLt, OpLe, OpGt, OpGe, OpIn, OpContains, OpPrefix}

// Valid reports whether o is a comparison this build compiles.
func (o Op) Valid() bool { return slices.Contains(Ops, o) }

// Filter is one condition on one indexed path.
type Filter struct {
	Path   string            `json:"path"`
	Op     Op                `json:"op"`
	Value  json.RawMessage   `json:"value,omitempty"`
	Values []json.RawMessage `json:"values,omitempty"`
}

// Order is the one path a query is ordered by. The document id is always the
// tie-break, so an order is total and a cursor is exact.
type Order struct {
	Path string `json:"path"`
	Desc bool   `json:"desc,omitempty"`
}

// Query is one question over one collection: filters ANDed together, an order,
// a page size and the cursor of the page before.
type Query struct {
	Where   []Filter `json:"where,omitempty"`
	OrderBy *Order   `json:"order_by,omitempty"`
	Limit   int      `json:"limit,omitempty"`
	After   string   `json:"after,omitempty"`
}

// The query bounds this compiler holds a question to.
const (
	// MaxFilters is how many filters one query ANDs: each is a subquery
	// over the field index, and sixteen is more than any sheet view a
	// person builds while keeping one query's plan small.
	MaxFilters = 16

	// DefaultLimit is a page when the query names none.
	DefaultLimit = 100
)

// Errors a caller tells apart.
var (
	// ErrInvalid is a query this grammar does not express.
	ErrInvalid = errors.New("query: invalid")

	// ErrNotIndexed is a filter or an order on a path the field index does
	// not hold: declare it in the collection's indexed paths.
	ErrNotIndexed = errors.New("query: the path is not indexed")
)

// Index is what the compiler needs to know about one collection's field index.
type Index interface {
	// Indexed reports whether path is held by the index: every top-level
	// field, and the dotted paths the collection declares.
	Indexed(path string) bool

	// PathNo is the number the store interned path under, and false where no
	// document has ever held it (so nothing can match it).
	PathNo(path string) (int64, bool)
}

// Scope is which rows of the index a query may see.
type Scope struct {
	StoreNo      int64
	CollectionNo int64

	// CreatedBy restricts the answer to the documents one principal
	// created: a reader below editor on an `own_rows` collection, and
	// everybody on a `private` one. Empty restricts nothing.
	CreatedBy string
}

// Compiled is a query ready to run.
//
// THE SELECT'S COLUMNS ARE FIXED, in this order: doc_id, body, created_by,
// created_by_kind, created_at, updated_by, updated_by_kind, updated_at,
// version, size, and the three sort-key columns a cursor is built from
// ([Compiled.Cursor]).
type Compiled struct {
	SQL  string
	Args []any

	// Limit is the page size the SQL asks for, one more than the page so a
	// reader knows whether another follows.
	Limit int

	// Recheck are the filters a body must be checked against after the
	// SQL: string comparisons the truncated index answers only as a
	// superset ([Compiled.Keep]).
	Recheck []Filter

	desc bool
}

// Columns is how many columns [Compiled.SQL] selects.
const Columns = 13

// Compile turns q into SQL over the field index for scope, refusing anything
// the grammar does not express and any path idx does not hold.
func Compile(q Query, scope Scope, idx Index) (Compiled, error) {
	if len(q.Where) > MaxFilters {
		return Compiled{}, fmt.Errorf("%w: a query names %d filters and the maximum is %d",
			ErrInvalid, len(q.Where), MaxFilters)
	}
	limit := q.Limit
	switch {
	case limit == 0:
		limit = DefaultLimit
	case limit < 0 || limit > artifacts.MaxQueryLimit:
		return Compiled{}, fmt.Errorf("%w: limit %d is outside 1..%d", ErrInvalid,
			q.Limit, artifacts.MaxQueryLimit)
	}

	var (
		where   = []string{"d.store_no = ?", "d.collection_no = ?", "d.body IS NOT NULL"}
		args    []any
		recheck []Filter
	)
	order := q.OrderBy
	orderNo := int64(-1)
	if order != nil {
		if err := indexedPath(order.Path, idx); err != nil {
			return Compiled{}, err
		}
		if n, known := idx.PathNo(order.Path); known {
			orderNo = n
		}
	}
	// THE JOIN'S ARGUMENT COMES FIRST, because it sits before WHERE.
	if order != nil {
		args = append(args, orderNo)
	}
	args = append(args, scope.StoreNo, scope.CollectionNo)
	if scope.CreatedBy != "" {
		where = append(where, "d.created_by = ?")
		args = append(args, scope.CreatedBy)
	}
	for i, f := range q.Where {
		clause, clauseArgs, again, err := compileFilter(f, scope, idx)
		if err != nil {
			return Compiled{}, fmt.Errorf("filter %d: %w", i, err)
		}
		where = append(where, clause)
		args = append(args, clauseArgs...)
		if again {
			recheck = append(recheck, f)
		}
	}
	desc := order != nil && order.Desc
	if q.After != "" {
		c, err := decodeCursor(q.After)
		if err != nil {
			return Compiled{}, err
		}
		clause, clauseArgs := keyset(c, order != nil, desc)
		where = append(where, clause)
		args = append(args, clauseArgs...)
	}

	var b strings.Builder
	b.WriteString("SELECT d.doc_id, d.body, d.created_by, d.created_by_kind, d.created_at, " +
		"d.updated_by, d.updated_by_kind, d.updated_at, d.version, d.size, ")
	if order != nil {
		b.WriteString("COALESCE(o.kind, ''), COALESCE(o.num, 0), COALESCE(o.txt, '') " +
			"FROM artifact_data_docs d LEFT JOIN artifact_data_fields o " +
			"ON o.store_no = d.store_no AND o.collection_no = d.collection_no " +
			"AND o.doc_id = d.doc_id AND o.path_no = ? AND o.ord = 0")
	} else {
		b.WriteString("'', 0, '' FROM artifact_data_docs d")
	}
	b.WriteString(" WHERE ")
	b.WriteString(strings.Join(where, " AND "))
	dir := ""
	if desc {
		dir = " DESC"
	}
	if order != nil {
		fmt.Fprintf(&b, " ORDER BY COALESCE(o.kind, '')%s, COALESCE(o.num, 0)%s, "+
			"COALESCE(o.txt, '')%s, d.doc_id%s", dir, dir, dir, dir)
	} else {
		b.WriteString(" ORDER BY d.doc_id")
	}
	b.WriteString(" LIMIT ?")
	args = append(args, limit+1)
	return Compiled{SQL: b.String(), Args: args, Limit: limit + 1, Recheck: recheck, desc: desc}, nil
}

// indexedPath refuses a path the grammar or the index does not hold.
func indexedPath(path string, idx Index) error {
	if !ValidPath(path) {
		return fmt.Errorf("%w: %q is not a field name or a dotted path one level deep",
			ErrInvalid, path)
	}
	if !idx.Indexed(path) {
		return fmt.Errorf("%w: %q is not in the field index; add it to the collection's "+
			"indexed paths with a schema change, then query it", ErrNotIndexed, path)
	}
	return nil
}

// scalar is one literal a filter compares with.
type scalar struct {
	kind string // s, n, b, z
	num  float64
	txt  string
}

// literal reads one filter value as a scalar.
func literal(raw json.RawMessage) (scalar, error) {
	v, err := canon.Parse(raw, 1)
	if err != nil {
		return scalar{}, fmt.Errorf("%w: a filter value: %w", ErrInvalid, err)
	}
	switch v.Kind {
	case canon.KindString:
		return scalar{kind: "s", txt: v.String}, nil
	case canon.KindNumber:
		f, err := strconv.ParseFloat(v.Number, 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return scalar{}, fmt.Errorf("%w: %s is not a finite number", ErrInvalid, v.Number)
		}
		return scalar{kind: "n", num: f}, nil
	case canon.KindBool:
		if v.Bool {
			return scalar{kind: "b", num: 1}, nil
		}
		return scalar{kind: "b", num: 0}, nil
	case canon.KindNull:
		return scalar{kind: "z"}, nil
	}
	return scalar{}, fmt.Errorf("%w: a filter compares a scalar, never an array or an object",
		ErrInvalid)
}

// recheckBound is the literal length past which a string match on the
// truncated index may be a false positive, so the body is checked again.
const recheckBound = TextIndexBytes - utf8.UTFMax

// compileFilter is one filter's clause, its arguments, and whether the body
// must be checked again.
func compileFilter(f Filter, scope Scope, idx Index) (string, []any, bool, error) {
	if !f.Op.Valid() {
		return "", nil, false, fmt.Errorf("%w: op %q is not one of %s", ErrInvalid, f.Op, words.Or(Ops))
	}
	if err := indexedPath(f.Path, idx); err != nil {
		return "", nil, false, err
	}
	pathNo, known := idx.PathNo(f.Path)
	if f.Op == OpIn {
		if len(f.Values) == 0 || len(f.Values) > artifacts.MaxQueryInValues || len(f.Value) > 0 {
			return "", nil, false, fmt.Errorf("%w: `in` names 1..%d values in `values`",
				ErrInvalid, artifacts.MaxQueryInValues)
		}
	} else if len(f.Values) > 0 || len(f.Value) == 0 {
		return "", nil, false, fmt.Errorf("%w: %s compares one `value`", ErrInvalid, f.Op)
	}
	// A PATH NO DOCUMENT HAS EVER HELD matches nothing, and its absence is
	// everything a `ne` asks.
	if !known {
		for _, raw := range append(slices.Clone(f.Values), f.Value) {
			if len(raw) == 0 {
				continue
			}
			if _, err := literal(raw); err != nil {
				return "", nil, false, err
			}
		}
		if f.Op == OpNe {
			return "1 = 1", nil, false, nil
		}
		return "1 = 0", nil, false, nil
	}

	var cond string
	var condArgs []any
	again := false
	switch f.Op {
	case OpEq, OpNe, OpContains:
		lit, err := literal(f.Value)
		if err != nil {
			return "", nil, false, err
		}
		ord := "f.ord = 0"
		if f.Op == OpContains {
			ord = "f.ord > 0"
		}
		match, matchArgs := equals(lit)
		cond, condArgs = ord+" AND "+match, matchArgs
		again = lit.kind == "s" && len(lit.txt) >= recheckBound
	case OpIn:
		var parts []string
		for _, raw := range f.Values {
			lit, err := literal(raw)
			if err != nil {
				return "", nil, false, err
			}
			match, matchArgs := equals(lit)
			parts = append(parts, "("+match+")")
			condArgs = append(condArgs, matchArgs...)
			if lit.kind == "s" && len(lit.txt) >= recheckBound {
				again = true
			}
		}
		cond = "f.ord = 0 AND (" + strings.Join(parts, " OR ") + ")"
	case OpLt, OpLe, OpGt, OpGe:
		lit, err := literal(f.Value)
		if err != nil {
			return "", nil, false, err
		}
		sym := map[Op]string{OpLt: "<", OpLe: "<=", OpGt: ">", OpGe: ">="}[f.Op]
		switch lit.kind {
		case "n":
			cond = "f.ord = 0 AND f.kind = 'n' AND f.num " + sym + " ?"
			condArgs = []any{lit.num}
		case "s":
			// THE TRUNCATED INDEX ORDERS STRINGS ONLY AS A SUPERSET:
			// truncation is monotone, so a strict comparison on the full
			// strings is at most a non-strict one on their prefixes. The
			// SQL asks the superset and the body decides.
			loose := map[Op]string{OpLt: "<=", OpLe: "<=", OpGt: ">=", OpGe: ">="}[f.Op]
			cond = "f.ord = 0 AND f.kind = 's' AND f.txt " + loose + " ?"
			condArgs = []any{TruncateText(lit.txt)}
			again = true
		default:
			return "", nil, false, fmt.Errorf("%w: %s orders a number or a string", ErrInvalid, f.Op)
		}
	case OpPrefix:
		lit, err := literal(f.Value)
		if err != nil {
			return "", nil, false, err
		}
		if lit.kind != "s" || lit.txt == "" {
			return "", nil, false, fmt.Errorf("%w: prefix matches a non-empty string", ErrInvalid)
		}
		p := TruncateText(lit.txt)
		// THE LOWER BOUND IS THE INDEX'S RANGE START, and the character
		// count of the prefix is what SQL's substr counts in.
		cond = "f.ord = 0 AND f.kind = 's' AND f.txt >= ? AND substr(f.txt, 1, ?) = ?"
		condArgs = []any{p, utf8.RuneCountInString(p), p}
		again = len(lit.txt) >= recheckBound
	}
	sub := "SELECT f.doc_id FROM artifact_data_fields f WHERE f.store_no = ? AND " +
		"f.collection_no = ? AND f.path_no = ? AND " + cond
	subArgs := append([]any{scope.StoreNo, scope.CollectionNo, pathNo}, condArgs...)
	if f.Op == OpNe {
		return "d.doc_id NOT IN (" + sub + ")", subArgs, again, nil
	}
	return "d.doc_id IN (" + sub + ")", subArgs, again, nil
}

// equals is the index condition a scalar equality reads.
func equals(lit scalar) (string, []any) {
	switch lit.kind {
	case "s":
		return "f.kind = 's' AND f.txt = ?", []any{TruncateText(lit.txt)}
	case "n":
		return "f.kind = 'n' AND f.num = ?", []any{lit.num}
	case "b":
		return "f.kind = 'b' AND f.num = ?", []any{lit.num}
	}
	return "f.kind = 'z'", nil
}

// cursor is the sort key of the last row of a page.
type cursor struct {
	Kind string  `json:"k"`
	Num  float64 `json:"n"`
	Txt  string  `json:"t"`
	Doc  string  `json:"i"`
}

// Cursor is the cursor of a page that ended at a row with these sort-key
// columns and this document id.
func (c Compiled) Cursor(kind string, num float64, txt, doc string) string {
	raw, err := json.Marshal(cursor{Kind: kind, Num: num, Txt: txt, Doc: doc})
	if err != nil {
		// UNREACHABLE: four plain fields always marshal.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeCursor reads a cursor a page minted.
func decodeCursor(s string) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: that cursor is not one a page minted", ErrInvalid)
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil || c.Doc == "" ||
		math.IsInf(c.Num, 0) || math.IsNaN(c.Num) {
		return cursor{}, fmt.Errorf("%w: that cursor is not one a page minted", ErrInvalid)
	}
	return c, nil
}

// keyset is the clause that starts a page after cursor c.
func keyset(c cursor, ordered, desc bool) (string, []any) {
	gt := ">"
	if desc {
		gt = "<"
	}
	if !ordered {
		return "d.doc_id " + gt + " ?", []any{c.Doc}
	}
	k, n, t := "COALESCE(o.kind, '')", "COALESCE(o.num, 0)", "COALESCE(o.txt, '')"
	return "(" + k + " " + gt + " ? OR (" + k + " = ? AND " + n + " " + gt + " ?) OR (" +
			k + " = ? AND " + n + " = ? AND " + t + " " + gt + " ?) OR (" +
			k + " = ? AND " + n + " = ? AND " + t + " = ? AND d.doc_id " + gt + " ?))",
		[]any{c.Kind, c.Kind, c.Num, c.Kind, c.Num, c.Txt, c.Kind, c.Num, c.Txt, c.Doc}
}

// Keep reports whether a document whose canonical body is body satisfies every
// filter [Compiled.Recheck] names: the exact answer for the string
// comparisons the truncated index answered as a superset.
func (c Compiled) Keep(body []byte) (bool, error) {
	if len(c.Recheck) == 0 {
		return true, nil
	}
	doc, err := canon.Parse(body, canon.MaxDepth)
	if err != nil {
		return false, fmt.Errorf("query: re-check a body: %w", err)
	}
	for _, f := range c.Recheck {
		ok, err := keep(f, doc)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// keep is one filter's exact answer on one document.
func keep(f Filter, doc canon.Value) (bool, error) {
	v, found := lookup(doc, f.Path)
	switch f.Op {
	case OpIn:
		for _, raw := range f.Values {
			lit, err := literal(raw)
			if err != nil {
				return false, err
			}
			if found && equalScalar(v, lit) {
				return true, nil
			}
		}
		return false, nil
	case OpContains:
		lit, err := literal(f.Value)
		if err != nil || !found || v.Kind != canon.KindArray {
			return false, err
		}
		for _, e := range v.Array {
			if equalScalar(e, lit) {
				return true, nil
			}
		}
		return false, nil
	}
	lit, err := literal(f.Value)
	if err != nil {
		return false, err
	}
	switch f.Op {
	case OpEq:
		return found && equalScalar(v, lit), nil
	case OpNe:
		return !found || !equalScalar(v, lit), nil
	case OpPrefix:
		return found && v.Kind == canon.KindString && strings.HasPrefix(v.String, lit.txt), nil
	}
	if !found || lit.kind != "s" || v.Kind != canon.KindString {
		return false, nil
	}
	cmp := strings.Compare(v.String, lit.txt)
	switch f.Op {
	case OpLt:
		return cmp < 0, nil
	case OpLe:
		return cmp <= 0, nil
	case OpGt:
		return cmp > 0, nil
	}
	return cmp >= 0, nil
}

// lookup is the value at path in doc: a top-level member, or a member of a
// top-level object one level down.
func lookup(doc canon.Value, path string) (canon.Value, bool) {
	head, tail, dotted := strings.Cut(path, ".")
	v, ok := member(doc, head)
	if !ok || !dotted {
		return v, ok
	}
	return member(v, tail)
}

// member is one key of an object.
func member(v canon.Value, key string) (canon.Value, bool) {
	if v.Kind != canon.KindObject {
		return canon.Value{}, false
	}
	i, found := slices.BinarySearchFunc(v.Members, key, func(m canon.Member, k string) int {
		return strings.Compare(m.Key, k)
	})
	if !found {
		return canon.Value{}, false
	}
	return v.Members[i].Value, true
}

// equalScalar reports whether a stored value equals a literal, by the rule the
// index answers: strings by their text, numbers by their float value, booleans
// and null by themselves.
func equalScalar(v canon.Value, lit scalar) bool {
	switch v.Kind {
	case canon.KindString:
		return lit.kind == "s" && v.String == lit.txt
	case canon.KindNumber:
		f, err := strconv.ParseFloat(v.Number, 64)
		return err == nil && lit.kind == "n" && f == lit.num
	case canon.KindBool:
		return lit.kind == "b" && (lit.num == 1) == v.Bool
	case canon.KindNull:
		return lit.kind == "z"
	}
	return false
}
