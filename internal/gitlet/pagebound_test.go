package gitlet_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// EVERY KIND'S PageMax IS THE BOUND ITS READ REFUSES PAST: a read asking
// gitlet.ReadQuery.PageMax rows is refused no field, and the same read asking
// one row more is refused invalid, so the limit alone refused it. Every kind
// in gitlet.ReadKinds is asked, a duty listing judging its own fields among
// them, and each variant a kind's bound turns on: a change read's parts, its
// findings by path, its viewed files, its detail alone and the findings it
// names, one push of a history, a listing counting pushes by day.
//
// Mutations: answer ReadChecks' PageMax as MaxSnapshotItems, and its read at
// the bound is refused, red; answer ReadIntroduced's as 0, and its read of one
// row is not refused, red.
func TestEveryKindsPageMaxIsTheBoundItsReadRefusesPast(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := n.ensure("web", suiteSettings(), t1).RepoID
	asks := everyReadAsked(id)
	var cases []gitlet.ReadQuery
	for _, kind := range gitlet.ReadKinds() {
		q, named := asks[kind]
		if !named {
			t.Errorf("a %s read is never asked here: name it in everyReadAsked, so its page bound is held", kind)
			continue
		}
		q.Kind = kind
		cases = append(cases, q)
	}
	variant := func(kind gitlet.ReadKind, with func(*gitlet.ReadQuery)) {
		q := asks[kind]
		q.Kind = kind
		with(&q)
		cases = append(cases, q)
	}
	for _, part := range gitlet.ChangeParts() {
		variant(gitlet.ReadChange, func(q *gitlet.ReadQuery) { q.Part = part })
	}
	variant(gitlet.ReadChange, func(q *gitlet.ReadQuery) { q.FindingPaths = true })
	variant(gitlet.ReadChange, func(q *gitlet.ReadQuery) { q.Viewed = true })
	variant(gitlet.ReadChange, func(q *gitlet.ReadQuery) { q.DetailOnly = true })
	variant(gitlet.ReadChange, func(q *gitlet.ReadQuery) {
		q.Part, q.FindingIDs = gitlet.PartFindings, []string{"0192f4c8-0000-7000-8000-000000000001"}
	})
	variant(gitlet.ReadPushes, func(q *gitlet.ReadQuery) { q.Push = "0192f4c8-0000-7000-8000-000000000002" })
	variant(gitlet.ReadRepos, func(q *gitlet.ReadQuery) {
		q.RepoFacts, q.DayBounds = true, midnights(time.UTC, 2031, 11, 1, 3)
	})

	for _, q := range cases {
		bound := q.PageMax()
		at := q
		at.Limit = bound
		if _, err := n.reader.Read(t.Context(), at); errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("%s asking PageMax's %d rows = %v, want no field refused", describeRead(q), bound, err)
			continue
		}
		at.Limit = bound + 1
		if _, err := n.reader.Read(t.Context(), at); !errors.Is(err, gitlet.ErrInvalid) {
			t.Errorf("%s asking %d rows, one past PageMax = %v, want it refused invalid", describeRead(q),
				bound+1, err)
		}
	}
}

// describeRead names a read by its kind and what its page bound turns on.
func describeRead(q gitlet.ReadQuery) string {
	return fmt.Sprintf("a %s read (part %q, finding_paths %t, viewed %t, detail_only %t, %d finding_ids, "+
		"push %q, %d day_bounds)", q.Kind, q.Part, q.FindingPaths, q.Viewed, q.DetailOnly, len(q.FindingIDs),
		q.Push, len(q.DayBounds))
}

// gitletPath is this package's import path, which every read the module
// makes names its query's type through.
const gitletPath = "github.com/crewlet/crewlet/internal/gitlet"

// EVERY READ THE MODULE MAKES ASKS A PAGE ITS KIND CARRIES: a limit the
// engine chooses (a constant, or min over one) is held, where it is written,
// to gitlet.ReadQuery.PageMax of the query it pages. Such a limit is the
// engine's own on every call, so one past the bound is refused invalid on
// every call, and no argument a caller changes clears it. A query naming no
// kind there (one a helper finishes) is held to the least page any paged kind
// carries. NOT JUDGED: a limit computed at run time, a person's or a seat's
// own, which the reader refuses naming the bound.
//
// Mutation: page code_rerun_checks' attempts at gitlet.MaxSnapshotItems
// again, and its read is reported.
func TestEveryEngineReadAsksAPageItsKindCarries(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	judged := map[string]int{}
	err := sourcetree.TypeCheck{
		Fset:   fset,
		Select: func(p *sourcetree.Listed) bool { return p.ImportPath == gitletPath || importsGitlet(p) },
	}.Run(sourcetree.Root(t), func(u *sourcetree.Unit) error {
		for _, f := range u.Files {
			for _, read := range pagedReads(u.Info, f) {
				judged[u.Package.ImportPath]++
				bound, kind := read.bound()
				if read.limit > int64(bound) {
					t.Errorf("%s: a %s read asks a page of %d rows, past the %d its kind carries "+
						"(gitlet.ReadQuery.PageMax): Gitlet refuses it invalid on every call",
						fset.Position(read.at), kind, read.limit, bound)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// THE WALK READ THE MODULE: the engine's own paged reads, in the
	// packages that make them.
	for _, want := range []string{"internal/agent/builtin", "internal/atlas/gather", "internal/codesearch",
		"internal/gitlet/checkrun", "internal/gitlet/home"} {
		if judged["github.com/crewlet/crewlet/"+want] == 0 {
			t.Errorf("the walk judged no limit in %s (judged %v); want every package paging a read read",
				want, judged)
		}
	}
}

// pagedRead is one limit the engine chooses for a read: where it is written,
// the most it can be, and the query it pages as far as the source says, known
// false where nothing there names its kind.
type pagedRead struct {
	at    token.Pos
	limit int64
	query gitlet.ReadQuery
	known bool
}

// bound is the page r's query carries, and its kind's name: for a query whose
// kind is unknown, the least page any paged kind carries.
func (r pagedRead) bound() (int, string) {
	if r.known {
		return r.query.PageMax(), string(r.query.Kind)
	}
	least := 0
	for _, kind := range gitlet.ReadKinds() {
		q := r.query
		q.Kind = kind
		if b := q.PageMax(); b > 0 && (least == 0 || b < least) {
			least = b
		}
	}
	return least, "any kind's"
}

// pagedReads is every limit f writes on a gitlet.ReadQuery that the engine
// chooses: a literal's `Limit:`, and an assignment to a query's Limit, the
// query read from the literal its variable was declared or assigned.
func pagedReads(info *types.Info, f *ast.File) []pagedRead {
	declared := map[types.Object]*ast.CompositeLit{}
	ast.Inspect(f, func(n ast.Node) bool {
		var names, values []ast.Expr
		switch s := n.(type) {
		case *ast.AssignStmt:
			names, values = s.Lhs, s.Rhs
		case *ast.ValueSpec:
			for _, id := range s.Names {
				names = append(names, id)
			}
			values = s.Values
		default:
			return true
		}
		for i := range min(len(names), len(values)) {
			id, ok := names[i].(*ast.Ident)
			lit := readLiteral(info, values[i])
			if !ok || lit == nil {
				continue
			}
			if obj := objectOf(info, id); obj != nil {
				declared[obj] = lit
			}
		}
		return true
	})
	var out []pagedRead
	ast.Inspect(f, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.CompositeLit:
			if !isReadQuery(info.TypeOf(s)) {
				return true
			}
			for _, el := range s.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok && keyName(kv) == "Limit" {
					if limit, chosen := chosenLimit(info, kv.Value); chosen {
						q, known := queryOf(info, s)
						out = append(out, pagedRead{at: kv.Pos(), limit: limit, query: q, known: known})
					}
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Limit" || !isReadQuery(info.TypeOf(sel.X)) || i >= len(s.Rhs) {
					continue
				}
				limit, chosen := chosenLimit(info, s.Rhs[i])
				if !chosen {
					continue
				}
				r := pagedRead{at: lhs.Pos(), limit: limit}
				if id, ok := ast.Unparen(sel.X).(*ast.Ident); ok {
					if lit := declared[objectOf(info, id)]; lit != nil {
						r.query, r.known = queryOf(info, lit)
					}
				}
				out = append(out, r)
			}
		}
		return true
	})
	return out
}

// chosenLimit is the most a limit can be where the engine chooses it: a
// constant's value, or the least constant a min call takes. false for a limit
// computed at run time, and for 0, a kind's default page.
func chosenLimit(info *types.Info, e ast.Expr) (int64, bool) {
	if v, ok := intConstant(info, e); ok {
		return v, v != 0
	}
	call, ok := ast.Unparen(e).(*ast.CallExpr)
	if !ok {
		return 0, false
	}
	fn, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if b, builtin := info.Uses[fn].(*types.Builtin); !ok || !builtin || b.Name() != "min" {
		return 0, false
	}
	least, found := int64(0), false
	for _, arg := range call.Args {
		if v, ok := intConstant(info, arg); ok && (!found || v < least) {
			least, found = v, true
		}
	}
	return least, found && least != 0
}

// intConstant is e's value where it is an integer constant.
func intConstant(info *types.Info, e ast.Expr) (int64, bool) {
	tv, ok := info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.Int {
		return 0, false
	}
	return constant.Int64Val(tv.Value)
}

// queryOf is the query lit spells, as far as its source says: each field a
// constant sets holds it, and each a value computed at run time sets holds
// one that is not its zero (so a bound that turns on it is the one asked);
// false where no constant names its kind.
func queryOf(info *types.Info, lit *ast.CompositeLit) (gitlet.ReadQuery, bool) {
	var q gitlet.ReadQuery
	known := false
	v := reflect.ValueOf(&q).Elem()
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		field := v.FieldByName(keyName(kv))
		if !field.IsValid() || !field.CanSet() {
			continue
		}
		tv := info.Types[kv.Value]
		if tv.Value == nil {
			if keyName(kv) != "Kind" {
				setPresent(field)
			}
			continue
		}
		known = known || keyName(kv) == "Kind"
		switch field.Kind() {
		case reflect.String:
			field.SetString(constant.StringVal(tv.Value))
		case reflect.Bool:
			field.SetBool(constant.BoolVal(tv.Value))
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if i, exact := constant.Int64Val(tv.Value); exact {
				field.SetInt(i)
			}
		default:
			setPresent(field)
		}
	}
	return q, known
}

// setPresent gives field a value that is not its type's zero.
func setPresent(field reflect.Value) {
	switch field.Kind() {
	case reflect.Pointer:
		field.Set(reflect.New(field.Type().Elem()))
	case reflect.Slice:
		field.Set(reflect.MakeSlice(field.Type(), 1, 1))
	case reflect.Map:
		field.Set(reflect.MakeMap(field.Type()))
	case reflect.String:
		field.SetString("set")
	case reflect.Bool:
		field.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		field.SetInt(1)
	}
}

// readLiteral is the gitlet.ReadQuery literal e spells, itself or its
// address; nil for anything else.
func readLiteral(info *types.Info, e ast.Expr) *ast.CompositeLit {
	e = ast.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = ast.Unparen(u.X)
	}
	if lit, ok := e.(*ast.CompositeLit); ok && isReadQuery(info.TypeOf(lit)) {
		return lit
	}
	return nil
}

// objectOf is what id declares or uses.
func objectOf(info *types.Info, id *ast.Ident) types.Object {
	if obj := info.Defs[id]; obj != nil {
		return obj
	}
	return info.Uses[id]
}

// keyName is a struct literal's field name.
func keyName(kv *ast.KeyValueExpr) string {
	if id, ok := kv.Key.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// isReadQuery reports whether t is gitlet.ReadQuery, or a pointer to one.
func isReadQuery(t types.Type) bool {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Name() == "ReadQuery" && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == gitletPath
}

// importsGitlet reports whether a non-test file of p imports this package.
func importsGitlet(p *sourcetree.Listed) bool {
	for _, name := range p.GoFiles {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(p.Dir, name), nil, parser.ImportsOnly)
		if err != nil {
			// THE CHECK REPORTS WHAT IT CANNOT PARSE: selected, its parse
			// fails the walk.
			return true
		}
		if slices.ContainsFunc(f.Imports, func(spec *ast.ImportSpec) bool {
			path, unquoteErr := strconv.Unquote(spec.Path.Value)
			return unquoteErr == nil && path == gitletPath
		}) {
			return true
		}
	}
	return false
}
