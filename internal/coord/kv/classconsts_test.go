package kv

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// EVERY KEY CLASS THE PACKAGE WRITES WITH IS DECLARED FOR ITS BUCKET, read off
// the source: every coord.*Class constant this package's code names, every
// coord key or filter function it calls that builds on one, and every
// entryClass's filter, is a key class the holding of a bucket the same
// declaration touches declares (an entryClass's, the bucket its verbs write).
// The recorder ([keyWrites]) holds what the suites write; this holds what no
// case reaches.
//
// Mutation: drop the integration status's entry from integrationsHolding, put
// a session claim's key into the atlas sources, or give the SSH tests'
// entryClass the session claims' filter, and this is red.
func TestEveryKeyClassThePackageWritesWithIsDeclaredForItsBucket(t *testing.T) {
	t.Parallel()
	classes := coordClassValues(t)
	holdings := fleetHoldingsByField(t)
	entryBuckets := map[string]bool{}
	var refs []classRef
	fset := token.NewFileSet()
	for _, file := range parsePackage(t, fset, ".") {
		for _, decl := range file.Decls {
			if filepath.Base(fset.Position(decl.Pos()).Filename) == "settle.go" {
				continue
			}
			buckets := bucketsTouched(decl, holdings)
			if fn, ok := decl.(*ast.FuncDecl); ok && receiverIs(fn, "entryClass") {
				for b := range buckets {
					entryBuckets[b] = true
				}
			}
			refs = append(refs, classRefsIn(t, fset, decl, classes, buckets)...)
		}
	}
	if len(entryBuckets) != 1 {
		t.Fatalf("premise: entryClass's verbs touch %v, want the one bucket every class of it writes", entryBuckets)
	}
	entries, named := 0, 0
	for _, r := range refs {
		if r.entry {
			entries++
			r.buckets = entryBuckets
		} else {
			named++
		}
		if !declaredFor(r.value, r.buckets, holdings) {
			t.Errorf("%s writes with %s (%q), which no key class of %v declares", r.at, r.what, r.value,
				slices.Sorted(func(yield func(string) bool) {
					for b := range r.buckets {
						if !yield(b) {
							return
						}
					}
				}))
		}
	}
	if entries < 10 || named < 20 {
		t.Fatalf("premise: %d entryClass filters and %d named classes read, want every one in the package",
			entries, named)
	}
}

// classRef is one key class a declaration of this package writes with.
type classRef struct {
	at, what, value string
	buckets         map[string]bool
	entry           bool
}

// declaredFor reports whether a key of class value is declared a key class of
// one of buckets' holdings (any bucket's where none is named). A single-class
// holding declares every key its one class.
func declaredFor(value string, buckets map[string]bool, holdings map[string]holding) bool {
	for field, h := range holdings {
		if len(buckets) > 0 && !buckets[field] {
			continue
		}
		if len(h.classes) == 0 || slices.ContainsFunc(h.classes, func(k keyClass) bool {
			return k.keys == coord.DocumentFilter(value)
		}) {
			return true
		}
	}
	return false
}

// classRefsIn is every key class decl writes with: a coord.*Class constant, a
// coord function building a key or a filter on one, and an entryClass
// literal's filter.
func classRefsIn(t *testing.T, fset *token.FileSet, decl ast.Decl, classes coordClasses,
	buckets map[string]bool) []classRef {

	t.Helper()
	var out []classRef
	ast.Inspect(decl, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			if !isEntryClass(n.Type) {
				return true
			}
			for _, el := range n.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok || !isIdent(kv.Key, "filter") {
					continue
				}
				for _, v := range classes.filterOf(t, kv.Value) {
					out = append(out, classRef{at: fset.Position(kv.Pos()).String(), what: "an entryClass filter",
						value: v, entry: true})
				}
			}
			return false
		case *ast.SelectorExpr:
			if !isIdent(n.X, "coord") {
				return true
			}
			if v, ok := classes.consts[n.Sel.Name]; ok && strings.HasSuffix(n.Sel.Name, "Class") {
				out = append(out, classRef{at: fset.Position(n.Pos()).String(), what: "coord." + n.Sel.Name,
					value: v, buckets: buckets})
			}
			if v, ok := classes.funcs[n.Sel.Name]; ok {
				out = append(out, classRef{at: fset.Position(n.Pos()).String(), what: "coord." + n.Sel.Name,
					value: v, buckets: buckets})
			}
		}
		return true
	})
	return out
}

// coordClasses is internal/coord's key classes as its source spells them: each
// string constant's value, and each function that builds a key or a filter
// whose first segment is one, by name.
type coordClasses struct {
	consts, funcs map[string]string
}

// coordClassValues reads internal/coord's non-test source.
func coordClassValues(t *testing.T) coordClasses {
	t.Helper()
	c := coordClasses{consts: map[string]string{}, funcs: map[string]string{}}
	files := parsePackage(t, token.NewFileSet(), "..")
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if v, ok := stringLit(vs.Values[i]); ok {
							c.consts[name.Name] = v
						}
					}
				}
			}
		}
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || len(fn.Body.List) != 1 {
				continue
			}
			ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(ret.Results) != 1 {
				continue
			}
			call, ok := ret.Results[0].(*ast.CallExpr)
			if !ok || !isIdent(call.Fun, "DocumentKey") && !isIdent(call.Fun, "DocumentFilter") ||
				len(call.Args) == 0 {
				continue
			}
			if v, ok := c.value(call.Args[0], ""); ok {
				c.funcs[fn.Name.Name] = v
			}
		}
	}
	if len(c.funcs) < 20 || c.consts["SessionClaimClass"] != coord.SessionClaimClass {
		t.Fatalf("premise: read %d key functions of internal/coord and its session claim class as %q",
			len(c.funcs), c.consts["SessionClaimClass"])
	}
	return c
}

// value is e as a constant string: a literal, or a constant named bare in
// internal/coord (qualifier "") or as coord.Name here (qualifier "coord").
func (c coordClasses) value(e ast.Expr, qualifier string) (string, bool) {
	if v, ok := stringLit(e); ok {
		return v, true
	}
	switch e := e.(type) {
	case *ast.Ident:
		if qualifier == "" {
			v, ok := c.consts[e.Name]
			return v, ok
		}
	case *ast.SelectorExpr:
		if isIdent(e.X, qualifier) {
			v, ok := c.consts[e.Sel.Name]
			return v, ok
		}
	case *ast.CallExpr:
		// string(coord.X), a class of a named string type.
		if len(e.Args) == 1 && isIdent(e.Fun, "string") {
			return c.value(e.Args[0], qualifier)
		}
	}
	return "", false
}

// filterOf is the classes an entryClass's filter selects: coord.DocumentFilter
// of a coord constant, or of a field of a coord.GrantFamily, one class per
// family. It fails t on any other spelling, which it could not read.
func (c coordClasses) filterOf(t *testing.T, e ast.Expr) []string {
	t.Helper()
	call, ok := e.(*ast.CallExpr)
	if ok && len(call.Args) == 1 {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isIdent(sel.X, "coord") && sel.Sel.Name == "DocumentFilter" {
			if v, ok := c.value(call.Args[0], "coord"); ok {
				return []string{v}
			}
			if field, ok := call.Args[0].(*ast.SelectorExpr); ok {
				var out []string
				for _, family := range coord.GrantFamilies {
					if f := reflect.ValueOf(family).FieldByName(field.Sel.Name); f.Kind() == reflect.String {
						out = append(out, f.String())
					}
				}
				if len(out) == len(coord.GrantFamilies) {
					return out
				}
			}
		}
	}
	t.Fatalf("an entryClass filter spelled %s, which this gate cannot read: spell it coord.DocumentFilter "+
		"of a coord constant", exprString(e))
	return nil
}

// fleetHoldingsByField is the holding of every bucket a fleet store settles,
// by its field's name, and the lease store's three by theirs.
func fleetHoldingsByField(t *testing.T) map[string]holding {
	t.Helper()
	nc := embeddedNATS(t)
	store := openFleetForTest(t, nc, fmt.Sprintf("g%d", bucketSeq.Add(1)))
	out := map[string]holding{}
	fields := reflect.ValueOf(store).Elem()
	for i := range fields.NumField() {
		if f := fields.Field(i); f.Type() == reflect.TypeFor[jetstream.KeyValue]() {
			// The field is the package's own, so it is read as itself.
			handle := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface()
			out[fields.Type().Field(i).Name] = handle.(settledBucket).holding
		}
	}
	leases := openStore(t, nc, time.Minute)
	out["leases"], out["duties"] = leases.leases.kv.(settledBucket).holding, leases.duties.kv.(settledBucket).holding
	out["counter"] = leases.counter.kv.(settledBucket).holding
	return out
}

// bucketsTouched is every bucket decl names by a field of a store: f.runs,
// s.leases and the like, a name holdings knows.
func bucketsTouched(decl ast.Decl, holdings map[string]holding) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(decl, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && !isIdent(sel.X, "coord") {
			if _, ok := holdings[sel.Sel.Name]; ok {
				out[sel.Sel.Name] = true
			}
		}
		return true
	})
	return out
}

// receiverIs reports whether fn is a method of the generic type named.
func receiverIs(fn *ast.FuncDecl, name string) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	switch r := fn.Recv.List[0].Type.(type) {
	case *ast.IndexExpr:
		return isIdent(r.X, name)
	case *ast.IndexListExpr:
		return isIdent(r.X, name)
	}
	return false
}

// isEntryClass reports whether e names an entryClass instantiation.
func isEntryClass(e ast.Expr) bool {
	ix, ok := e.(*ast.IndexExpr)
	return ok && isIdent(ix.X, "entryClass")
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

func exprString(e ast.Expr) string {
	var b strings.Builder
	ast.Fprint(&b, token.NewFileSet(), e, nil)
	return b.String()
}

// parsePackage parses dir's non-test source, failing t on too few files to be
// a package of this module: a gate that parsed nothing certifies nothing.
func parsePackage(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out = append(out, file)
	}
	if len(out) < 5 {
		t.Fatalf("read %d source files in %s, which is not a package of this module", len(out), dir)
	}
	return out
}
