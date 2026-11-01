package engine_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/sourcetree/decltype"
)

// EVERY FIELD OF THE ALARM READING HAS A PRODUCER: something the reading's
// assembly reaches writes it.
//
// # Why a gate
//
// An alarm reads one field of statelog.Reading, and a field nothing fills
// reads as zero, which every condition in the table reads as "nothing to
// report". So an alarm whose input has no producer is permanently silent — and
// silent is exactly what a healthy node looks like, so no test of the table,
// no screen and no operator can tell. It happened twice: the git cache's
// AlarmReading filled the code half and nothing called it, so
// `code_maintenance_refused` never fired; and PrefetchP95 was written by no
// code at all, so `prefetch_slow` never fired, while the metrics reference
// described the series that should have fed it. Each was found by reading; a
// third would be found the same way, or not at all.
//
// # What it checks
//
// The walk starts where a reading is assembled — retention.reading, the node's
// whole reading, and statelog's ReportInputs.alarms, which builds each
// domain's half — and follows every call that hands the reading on: a call
// with an argument that is a Reading or points at one, and a call whose
// result is one. A call is followed to the function a package names, the
// method a value's type declares, and, for a call through a FUNCTION-VALUED
// FIELD (retention.code, retention.index, retention.objects), to every value
// its package assigns that field — a method value, a function, a literal. A
// field is produced where a reached body assigns it, increments it, takes its
// address or names it in a composite literal. Every field of Reading must be.
// Reachability is the point: AlarmReading wrote its three fields all along,
// in a function nothing on this path called.
//
// # Coverage boundary, stated rather than assumed
//
// A call through an interface, a function value held in a variable or a
// parameter, or a function-valued field assigned outside its own package is
// not followed; nor is a write through a pointer to one field handed to a
// helper (`fill(&out.X)` counts as the write, the helper is not read). Each of
// those makes the gate report a field as unproduced that is produced — the
// loud direction: never a field passed that nothing fills. The failure names
// the calls it could not follow, so the next reader knows where to look.
//
// Mutation: drop r.code(&out) from the reading's assembly, or the code field
// from startRetention's retention literal, and the code half has no producer;
// drop PrefetchP95 from retention.observed, and it has none either.
func TestEveryAlarmReadingFieldHasAProducer(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	walk := readingProducers(t, root, productionRoots)
	if len(walk.reached) == 0 || len(walk.written) == 0 {
		t.Fatalf("the walk reached %d function(s) and found %d field(s) written — it is certifying "+
			"nothing. Check the roots %v and the module root %s", len(walk.reached), len(walk.written),
			productionRoots, root)
	}
	// A GATE ASSERTING PRESENCE PASSES IDENTICALLY WHEN A FIELD IS FILLED AND
	// WHEN THE WALK GOES WIDER THAN THE PATH: the git cache's half, and the
	// compactor's and the mirrors' beside it, are reached only through a
	// function-valued field and a method on another package's type, so a walk
	// that stopped following either would pass this by following something
	// else.
	for _, must := range []string{"code/cache.Cache.AlarmReading", "code/compact.Compactor.AlarmReading",
		"code/mirror.Mirrors.AlarmReading"} {
		if !slices.Contains(walk.reached, must) {
			t.Errorf("the walk did not reach %s through retention.code — the field-valued call or the "+
				"cross-package method is no longer followed", must)
		}
	}
	for _, field := range walk.unproduced() {
		t.Errorf("statelog.Reading.%s has no producer: nothing the alarm reading's assembly reaches "+
			"writes it, so every alarm reading it is permanently silent on a running node. Fill it "+
			"where its input is measured, on the path from retention.reading or "+
			"statelog.ReportInputs.alarms — or remove it with its alarm.%s", field, walk.unfollowedNote())
	}
	t.Logf("reached %d function(s) from %d root(s); %d of %d field(s) written; read %d package(s)",
		len(walk.reached), len(productionRoots), len(walk.written), len(walk.fields), walk.packages)
}

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN: a field written in the root, in a
// method the root calls, in a function another package declares, through a
// function-valued field assigned a method value and through one assigned a
// literal is produced; a field written only in a function nothing calls, one
// written behind a function-valued field nothing assigns, and one written
// nowhere are not.
func TestTheReadingWalkJudgesAKnownTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, body := range readingFixture {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	walk := readingProducers(t, root, []producerRoot{{Package: "internal/engine", Recv: "retention",
		Name: "reading"}})
	want := []string{"Detached", "Nowhere", "Uncalled"}
	if got := walk.unproduced(); !slices.Equal(got, want) {
		t.Errorf("the walk found %v unproduced, want %v", got, want)
	}
	for _, field := range []string{"Direct", "Composite", "Method", "OtherPackage", "Literal", "Incremented",
		"Addressed"} {
		if _, ok := walk.written[field]; !ok {
			t.Errorf("the walk missed the write of %s", field)
		}
	}
	if !slices.Contains(walk.reached, "code/cache.Cache.Fill") {
		t.Errorf("the walk reached %v, want the other package's method behind the field", walk.reached)
	}
}

// productionRoots are where a reading is assembled for the alarm table: the
// node's whole reading, and each domain's half.
var productionRoots = []producerRoot{
	{Package: "internal/engine", Recv: "retention", Name: "reading"},
	{Package: "internal/statelog", Recv: "ReportInputs", Name: "alarms"},
}

// producerRoot names a function the walk starts from: a package relative to
// the module, a receiver type ("" for a function) and a name.
type producerRoot struct{ Package, Recv, Name string }

const producerModule = "github.com/crewlet/crewlet"

// readingType is the type whose fields must each have a producer.
var readingType = decltype.TypeName{Package: producerModule + "/internal/statelog", Name: "Reading"}

// producerWalk is what one walk found.
type producerWalk struct {
	// fields is every field statelog.Reading declares, sorted.
	fields []string
	// written is each field written somewhere the walk reached, with where.
	written map[string]string
	// reached is every function the walk read, as <package>.<receiver>.<name>
	// relative to the module.
	reached []string
	// unfollowed is every call handing the reading on that the walk could
	// not follow, as file:line.
	unfollowed []string
	// packages is how many packages the walk read.
	packages int
}

// unproduced is every field nothing the walk reached writes, sorted.
func (p producerWalk) unproduced() []string {
	var out []string
	for _, f := range p.fields {
		if _, ok := p.written[f]; !ok {
			out = append(out, f)
		}
	}
	return out
}

// unfollowedNote names the calls the walk could not follow, for a failure.
func (p producerWalk) unfollowedNote() string {
	if len(p.unfollowed) == 0 {
		return ""
	}
	return "\n\tThe walk could not follow these calls, which hand the reading on: " +
		strings.Join(p.unfollowed, ", ")
}

// funcKey is a function's declaration: its package's import path, its
// receiver's type name ("" for a function) and its name.
type funcKey struct{ pkg, recv, name string }

// funcBody is a body the walk reads, in the file that spells it.
type funcBody struct {
	label string
	body  *ast.BlockStmt
	file  *decltype.File
}

// producerWalker is one walk's state.
type producerWalker struct {
	t     *testing.T
	w     *decltype.World
	funcs map[funcKey]funcBody
	// indexed is the packages whose declarations are in funcs.
	indexed map[string]bool
	// assigned caches each function-valued field's assigned values.
	assigned map[string][]funcBody
	seen     map[*ast.BlockStmt]bool
	out      producerWalk
}

// readingProducers walks the module under root from roots.
//
// IT READS sync/atomic beside the module, located by `go list` (which builds
// nothing): the published runtime every half reads its source from is an
// atomic.Pointer, and a value only an unread package can type is one the walk
// cannot follow a method of.
func readingProducers(t *testing.T, root string, roots []producerRoot) producerWalk {
	t.Helper()
	dirs, err := decltype.Locate(t.Context(), sourcetree.Root(t), "sync/atomic")
	if err != nil {
		t.Fatal(err)
	}
	pw := &producerWalker{
		t: t, w: decltype.New(root, producerModule, dirs),
		funcs: map[funcKey]funcBody{}, indexed: map[string]bool{}, assigned: map[string][]funcBody{},
		seen: map[*ast.BlockStmt]bool{}, out: producerWalk{written: map[string]string{}},
	}
	pw.out.fields = pw.readingFields()
	for _, r := range roots {
		key := funcKey{pkg: producerModule + "/" + r.Package, recv: r.Recv, name: r.Name}
		body, ok := pw.lookup(key)
		if !ok {
			t.Fatalf("the root %s.%s.%s is declared nowhere: the gate's subject moved", r.Package, r.Recv,
				r.Name)
		}
		pw.walk(body)
	}
	for _, err := range pw.w.Errors() {
		t.Errorf("the walk could not parse a file, so it judged nothing in it: %v", err)
	}
	slices.Sort(pw.out.reached)
	pw.out.packages = pw.w.Read()
	return pw.out
}

// readingFields is every field statelog.Reading declares, from its declaration.
func (pw *producerWalker) readingFields() []string {
	pkg := pw.w.Load(readingType.Package)
	if pkg == nil {
		pw.t.Fatalf("could not read %s", readingType.Package)
	}
	spec := pkg.TypeSpec(readingType.Name)
	if spec == nil {
		pw.t.Fatalf("%s declares no %s", readingType.Package, readingType.Name)
	}
	st, ok := spec.Type.(*ast.StructType)
	if !ok {
		pw.t.Fatalf("%s is not a struct", readingType)
	}
	var out []string
	for _, f := range st.Fields.List {
		for _, n := range f.Names {
			out = append(out, n.Name)
		}
	}
	slices.Sort(out)
	return out
}

// lookup is the declaration of key, its package read and indexed on first use.
func (pw *producerWalker) lookup(key funcKey) (funcBody, bool) {
	if !pw.indexed[key.pkg] {
		pw.indexed[key.pkg] = true
		if pkg := pw.w.Load(key.pkg); pkg != nil {
			for _, f := range pkg.Files() {
				for _, d := range f.AST.Decls {
					fd, ok := d.(*ast.FuncDecl)
					if !ok || fd.Body == nil {
						continue
					}
					k := funcKey{pkg: key.pkg, name: fd.Name.Name}
					if fd.Recv != nil && len(fd.Recv.List) == 1 {
						k.recv = receiverName(fd.Recv.List[0].Type)
					}
					rel := strings.TrimPrefix(strings.TrimPrefix(key.pkg, producerModule), "/internal/")
					label := rel + "." + fd.Name.Name
					if k.recv != "" {
						label = rel + "." + k.recv + "." + fd.Name.Name
					}
					pw.funcs[k] = funcBody{label: label, body: fd.Body, file: f}
				}
			}
		}
	}
	body, ok := pw.funcs[key]
	return body, ok
}

// receiverName is a method receiver's type name: through a pointer and any
// type parameters.
func receiverName(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

// walk reads one body: the fields it writes, and every call that hands the
// reading on, followed. A function literal inside it is read with it.
func (pw *producerWalker) walk(b funcBody) {
	if pw.seen[b.body] {
		return
	}
	pw.seen[b.body] = true
	if b.label != "" {
		pw.out.reached = append(pw.out.reached, b.label)
	}
	ast.Inspect(b.body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				pw.wrote(lhs, b.file)
			}
		case *ast.IncDecStmt:
			pw.wrote(x.X, b.file)
		case *ast.UnaryExpr:
			if x.Op == token.AND {
				pw.wrote(x.X, b.file)
			}
		case *ast.CompositeLit:
			if pw.isReading(pw.w.TypeOf(x, b.file)) {
				for _, elt := range x.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if id, ok := kv.Key.(*ast.Ident); ok {
							pw.record(id.Name, kv.Pos(), b.file)
						}
					}
				}
			}
		case *ast.CallExpr:
			if pw.handsOn(x, b.file) {
				targets := pw.targets(x, b.file)
				if len(targets) == 0 {
					p := pw.w.FileSet().Position(x.Pos())
					pw.out.unfollowed = append(pw.out.unfollowed,
						fmt.Sprintf("%s:%d", filepath.Base(p.Filename), p.Line))
				}
				for _, target := range targets {
					pw.walk(target)
				}
			}
		}
		return true
	})
}

// isReading reports whether t is statelog.Reading, through pointers.
func (pw *producerWalker) isReading(t decltype.Type) bool {
	name, ok := pw.w.Named(t)
	return ok && name == readingType
}

// wrote records e as a write when it selects a field of a Reading.
func (pw *producerWalker) wrote(e ast.Expr, f *decltype.File) {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok {
		return
	}
	s := pw.w.Select(pw.w.TypeOf(sel.X, f), sel.Sel.Name)
	if s.Kind == decltype.Field && s.Owner == readingType {
		pw.record(sel.Sel.Name, sel.Pos(), f)
	}
}

// record notes that field is written at pos.
func (pw *producerWalker) record(field string, pos token.Pos, f *decltype.File) {
	if _, ok := pw.out.written[field]; ok {
		return
	}
	p := pw.w.FileSet().Position(pos)
	pw.out.written[field] = fmt.Sprintf("%s:%d", f.Rel, p.Line)
}

// handsOn reports whether a call hands a reading on: an argument that is one
// or points at one, or a result that is one.
func (pw *producerWalker) handsOn(call *ast.CallExpr, f *decltype.File) bool {
	if pw.isReading(pw.w.TypeOf(call, f)) {
		return true
	}
	for _, a := range call.Args {
		if pw.isReading(pw.w.TypeOf(a, f)) {
			return true
		}
	}
	return false
}

// targets is every body a call may run: the function or method it names, or
// every value its package assigns the function-valued field it calls through.
func (pw *producerWalker) targets(call *ast.CallExpr, f *decltype.File) []funcBody {
	fun := ast.Unparen(call.Fun)
	switch x := fun.(type) {
	case *ast.IndexExpr:
		fun = x.X
	case *ast.IndexListExpr:
		fun = x.X
	}
	switch x := fun.(type) {
	case *ast.Ident:
		if body, ok := pw.lookup(funcKey{pkg: f.Package.Path, name: x.Name}); ok {
			return []funcBody{body}
		}
	case *ast.SelectorExpr:
		if path, ok := pw.w.PackageNamed(x.X, f); ok {
			if body, ok := pw.lookup(funcKey{pkg: path, name: x.Sel.Name}); ok {
				return []funcBody{body}
			}
			return nil
		}
		s := pw.w.Select(pw.w.TypeOf(x.X, f), x.Sel.Name)
		switch {
		case s.Kind == decltype.Method && !s.Interface:
			if body, ok := pw.lookup(funcKey{pkg: s.Owner.Package, recv: s.Owner.Name, name: x.Sel.Name}); ok {
				return []funcBody{body}
			}
		case s.Kind == decltype.Field && s.Owner.Package != "":
			return pw.fieldValues(s.Owner, x.Sel.Name)
		}
	case *ast.FuncLit:
		// Read with the body that spells it.
		return []funcBody{{body: x.Body, file: f}}
	}
	return nil
}

// fieldValues is every function value owner's own package assigns its field
// name: in a composite literal of owner, or by an assignment to the field of a
// value of owner.
func (pw *producerWalker) fieldValues(owner decltype.TypeName, name string) []funcBody {
	key := owner.String() + "." + name
	if values, ok := pw.assigned[key]; ok {
		return values
	}
	var values []funcBody
	pkg := pw.w.Load(owner.Package)
	if pkg == nil {
		return nil
	}
	for _, f := range pkg.Files() {
		ast.Inspect(f.AST, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if t, ok := pw.w.Named(pw.w.TypeOf(x, f)); !ok || t != owner {
					return true
				}
				for _, elt := range x.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == name {
						values = append(values, pw.funcValue(kv.Value, f)...)
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != name || i >= len(x.Rhs) {
						continue
					}
					if t, ok := pw.w.Named(pw.w.TypeOf(sel.X, f)); ok && t == owner {
						values = append(values, pw.funcValue(x.Rhs[i], f)...)
					}
				}
			}
			return true
		})
	}
	pw.assigned[key] = values
	return values
}

// funcValue is the body a function value runs: a method value, a function a
// package or this one declares, or a literal.
func (pw *producerWalker) funcValue(e ast.Expr, f *decltype.File) []funcBody {
	switch x := ast.Unparen(e).(type) {
	case *ast.FuncLit:
		return []funcBody{{body: x.Body, file: f}}
	case *ast.Ident:
		if body, ok := pw.lookup(funcKey{pkg: f.Package.Path, name: x.Name}); ok {
			return []funcBody{body}
		}
	case *ast.SelectorExpr:
		if path, ok := pw.w.PackageNamed(x.X, f); ok {
			if body, ok := pw.lookup(funcKey{pkg: path, name: x.Sel.Name}); ok {
				return []funcBody{body}
			}
			return nil
		}
		s := pw.w.Select(pw.w.TypeOf(x.X, f), x.Sel.Name)
		if s.Kind == decltype.Method && !s.Interface {
			if body, ok := pw.lookup(funcKey{pkg: s.Owner.Package, recv: s.Owner.Name, name: x.Sel.Name}); ok {
				return []funcBody{body}
			}
		}
	}
	return nil
}

// readingFixture is a tree whose verdict is known, for
// TestTheReadingWalkJudgesAKnownTree: paths relative to the tree's root, read
// as this module's own packages.
var readingFixture = map[string]string{
	"internal/statelog/alarms.go": `package statelog

type Reading struct {
	Direct, Composite, Method, OtherPackage, Literal, Incremented, Addressed int
	Uncalled, Detached, Nowhere                                          int
}
`,
	"internal/code/cache/cache.go": `package cache

import "github.com/crewlet/crewlet/internal/statelog"

type Cache struct{}

func (c *Cache) Fill(out *statelog.Reading) { out.OtherPackage = 1 }
`,
	"internal/engine/retention.go": `package engine

import (
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/statelog"
)

type Engine struct{ cache *cache.Cache }

type retention struct {
	code     func(*statelog.Reading)
	literal  func(*statelog.Reading)
	detached func(*statelog.Reading)
}

func start(e *Engine) *retention {
	r := &retention{code: e.codeReading}
	r.literal = func(out *statelog.Reading) { out.Literal = 1 }
	return r
}

func (e *Engine) codeReading(out *statelog.Reading) { e.cache.Fill(out) }

func (r *retention) reading() statelog.Reading {
	out := statelog.Reading{Composite: 1}
	out.Direct = 1
	out.Incremented++
	set(&out.Addressed)
	r.method(&out)
	r.code(&out)
	r.literal(&out)
	r.detached(&out)
	return out
}

func (r *retention) method(out *statelog.Reading) { out.Method = 1 }

func set(n *int) { *n = 1 }

// uncalled writes a field from a function nothing on the path calls.
func uncalled(out *statelog.Reading) { out.Uncalled = 1 }

// detachedReading writes a field behind a function-valued field nothing assigns.
func detachedReading(out *statelog.Reading) { out.Detached = 1 }

var _ = uncalled
var _ = detachedReading
`,
}
