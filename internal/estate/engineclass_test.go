package estate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE ENGINE CLASS: the executor log's writes are the engine's record of what
// its own runs did, which no tool, no person and no operator surface makes
// ([originEngine]). Two gates hold it, one on each side: every write on the log
// is declared the engine's and nothing else is (the operation table), and no
// package that serves a person or a model reaches the log's write side (the
// source tree).

// EVERY WRITE ON THE EXECUTOR LOG IS THE ENGINE'S, AND NOTHING ELSE IS.
//
// A write on the log declared a surface's would be one SurfaceWriteDomains
// lists, so the person's write surfaces would have to class and name a write
// nobody can make of them. An engine write on any other domain would be
// carried by a facade the other side of the gate does not read, so the class
// would hold it to nothing. And a read is never the engine's: the engine's
// reads are ordinary ones, which a surface may hold.
//
// Mutation: drop byEngine() from executor.capture, or put it on a tracker
// write, and the operation is named; the planted table makes each of those
// moves and a read declared the engine's beside them.
func TestEveryExecutorWriteIsTheEngines(t *testing.T) {
	t.Parallel()
	for _, violation := range engineClassViolations(registry) {
		t.Error(violation)
	}
	var engine, executorWrites int
	for _, spec := range registry {
		if spec.origin == originEngine {
			engine++
		}
		if spec.domain == executorDomain && spec.class != opRead {
			executorWrites++
		}
	}
	if engine == 0 || executorWrites == 0 {
		t.Fatalf("%d operations are the engine's and %d write the executor log: the class holds "+
			"nothing, so this gate certifies nothing", engine, executorWrites)
	}

	planted := map[string]*opSpec{
		"executor.capture":    {class: opIdempotentWrite, domain: executorDomain},
		"executor.peek":       {class: opRead, domain: executorDomain, origin: originEngine},
		"tracker.repair":      {class: opIdempotentWrite, domain: trackerDomain, origin: originEngine},
		"executor.end":        {class: opIdempotentWrite, domain: executorDomain, origin: originEngine},
		"executor.head":       {class: opRead, domain: executorDomain},
		"tracker.create_task": {class: opIdempotentWrite, domain: trackerDomain},
	}
	violations := engineClassViolations(planted)
	var named []string
	for _, v := range violations {
		named = append(named, strings.Fields(v)[0])
	}
	if want := []string{"executor.capture", "executor.peek", "tracker.repair"}; !slices.Equal(named, want) {
		t.Errorf("the planted table's violations name %v, want %v:\n%s", named, want,
			strings.Join(violations, "\n"))
	}
}

// engineClassViolations is every operation in table that breaks the engine
// class, in the order of their names, each opening with the name.
func engineClassViolations(table map[string]*opSpec) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(table)) {
		spec := table[name]
		engine := spec.origin == originEngine
		switch {
		case spec.class == opRead && engine:
			out = append(out, name+" is a read declared the engine's: the engine's reads are "+
				"ordinary ones, which a surface may hold")
		case spec.class == opRead:
		case spec.domain == executorDomain && !engine:
			out = append(out, name+" writes the executor log and is not declared the engine's: "+
				"nothing but the engine records what its runs did, so declare it byEngine()")
		case spec.domain != executorDomain && engine:
			out = append(out, fmt.Sprintf("%s is declared the engine's and writes the %q domain: "+
				"the class is the executor log's writes, the only ones the surface guard "+
				"(TestNoSurfaceHoldsTheEnginesExecutorWrites) holds", name, spec.domain))
		}
	}
	return out
}

// THE DOMAINS A SURFACE WRITES LEAVE THE ENGINE'S OUT, AND ONLY THE ENGINE'S.
//
// internal/agent/builtin holds its declaration of the domains a person's write
// surface classes refusals of and names objects of to exactly this list
// (TestEveryRoutedDomainIsDeclaredToTheSurfaces). A list that kept the
// executor log would demand a refusal class and an object argument for writes
// no surface makes; one that dropped a surface's domain beside it would let
// that domain's refusals reach a person as faults.
//
// Mutation: drop the origin from surfaceWrite and the executor log is listed;
// drop the class and a read is counted a surface's write.
func TestTheSurfacesWriteDomainsLeaveTheEnginesOut(t *testing.T) {
	t.Parallel()
	listed := SurfaceWriteDomains()
	if slices.Contains(listed, executorDomain) {
		t.Errorf("SurfaceWriteDomains lists %v, the executor log among them, whose writes are "+
			"the engine's alone", listed)
	}
	for _, domain := range []string{trackerDomain, pagesDomain, artifactsDomain, artifactDataDomain, chatDomain} {
		if !slices.Contains(listed, domain) {
			t.Errorf("SurfaceWriteDomains lists %v, without %s, which a surface writes", listed, domain)
		}
	}
	for name, tc := range map[string]struct {
		spec opSpec
		want bool
	}{
		"a surface's write":      {opSpec{class: opIdempotentWrite, domain: trackerDomain}, true},
		"a surface's once-write": {opSpec{class: opOnceWrite, domain: pagesDomain}, true},
		"the engine's write":     {opSpec{class: opIdempotentWrite, domain: executorDomain, origin: originEngine}, false},
		"a read":                 {opSpec{class: opRead, domain: pagesDomain}, false},
		"a read with no floor":   {opSpec{class: opRead}, false},
	} {
		if got := tc.spec.surfaceWrite(); got != tc.want {
			t.Errorf("%s: counted a surface's write = %v, want %v", name, got, tc.want)
		}
	}
}

// personSurfaces are the trees of the packages that serve a person or a model:
// a seat's tools, the API with its operator MCP, Ask Crewlet and the binary.
var personSurfaces = []string{"internal/agent/builtin", "internal/api", "internal/assistant", "cmd/crewlet"}

// engineDir is the engine package, whose exported hand-outs the guard derives.
const engineDir = "internal/engine"

// The import paths the guard reads names through, read off the types
// themselves so a moved package is followed.
var (
	estateImport   = reflect.TypeFor[Backend]().PkgPath()
	executorImport = reflect.TypeFor[executordomain.Writer]().PkgPath()
)

// engineWriteNames are the names the log's write side is reached by, matched
// on any operand: the router's writer facade and its accessor
// (ExecutorWrites), and the serving half's writer and the engine's own
// accessor of this node's writer (ExecutorWriter).
var engineWriteNames = []string{"ExecutorWrites", "ExecutorWriter"}

// writeSideTypes are this package's types that carry the log's write side, as
// a hand-out's results name them.
var writeSideTypes = []string{"ExecutorWrites", "ExecutorWriter", "ExecutorHalf"}

// domainWriteNames are the domain's own write side, matched through whatever
// name a file imports the domain under: its writer, how one is built, and the
// call every write takes and no read does.
var domainWriteNames = []string{"Writer", "NewWriter", "WriterDeps", "Call"}

// NO SURFACE HOLDS THE ENGINE'S EXECUTOR WRITES.
//
// The router declares the executor log's writes the engine's
// (TestEveryExecutorWriteIsTheEngines); what keeps a surface from making one
// anyway is that no package serving a person or a model reaches the write
// side ([personSurfaces]). Each tree is read whole, its non-test Go files less
// testdata, for a road to it:
//
//   - the router's writer facade and the serving half's writer, by name on
//     any operand ([engineWriteNames]): `estate.ExecutorWrites`,
//     `router.ExecutorWrites()`, `estate.ExecutorWriter` and the engine's own
//     `ExecutorWriter()`;
//   - the domain's own write side through whatever name a file imports the
//     domain under ([domainWriteNames]); a dot-import of the domain or of this
//     package is refused outright, since it reaches every name unqualified;
//   - every exported function and method of internal/engine whose results
//     name the write side, under whatever name it has: a hand-out is a road a
//     surface takes without naming the writer at all.
//
// It is SYNTACTIC, as every guard over the tree is: a writer reached through a
// value of another type that carries one, or through reflection, is no name it
// can match, and review holds those. A walk that read nothing would pass, so
// each surface must yield a Go file.
//
// Mutation: TestTheEngineWritesGuardFindsEveryRoad makes each move above.
func TestNoSurfaceHoldsTheEnginesExecutorWrites(t *testing.T) {
	t.Parallel()
	report, err := scanEngineWrites(sourcetree.Root(t), personSurfaces)
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range report.failures(personSurfaces) {
		t.Error(failure)
	}
	if report.engineFiles == 0 {
		t.Errorf("the guard read no source of %s, so it derived no hand-out a surface could take",
			engineDir)
	}
}

// THE GUARD FINDS EVERY ROAD IT CLAIMS TO CLOSE.
//
// Each case plants files in a tree where every surface otherwise holds a file
// that takes no road, and scans it: a road is one failure naming its file and
// line, and a file that only looks like one is none. The last case is a
// surface whose tree holds no Go file at all: a walk that read nothing.
//
// Mutation: match the facade only under the import name `estate`, match the
// domain's names on any operand, read the test files, or leave the engine's
// hand-outs underived, and the case for that road changes its answer.
func TestTheEngineWritesGuardFindsEveryRoad(t *testing.T) {
	t.Parallel()
	estate, executor := strconv.Quote(estateImport), strconv.Quote(executorImport)
	engine := strconv.Quote(path.Join(path.Dir(estateImport), "engine"))
	for name, tc := range map[string]struct {
		files map[string]string
		// want is a part of the one failure; empty, the files take no road.
		want string
	}{
		"the writer facade under an aliased import": {
			files: map[string]string{"internal/api/road.go": "package api\nimport est " + estate +
				"\nvar w est.ExecutorWrites\n"},
			want: "internal/api/road.go:3: ExecutorWrites",
		},
		"the router's accessor on any operand": {
			files: map[string]string{"internal/agent/builtin/road.go": "package builtin\n" +
				"func writes(r interface{ ExecutorWrites() any }) any {\n\treturn r.ExecutorWrites()\n}\n"},
			want: "internal/agent/builtin/road.go:3: ExecutorWrites",
		},
		"the serving half's writer in Ask Crewlet": {
			files: map[string]string{"internal/assistant/road.go": "package assistant\nimport " + estate +
				"\nvar w estate.ExecutorWriter\n"},
			want: "internal/assistant/road.go:3: ExecutorWriter",
		},
		"the engine's own writer, from the binary": {
			files: map[string]string{"cmd/crewlet/road.go": "package main\n" +
				"func writer(e interface{ ExecutorWriter() any }) any { return e.ExecutorWriter() }\n"},
			want: "cmd/crewlet/road.go:2: ExecutorWriter",
		},
		"the domain's writer under an aliased import": {
			files: map[string]string{"internal/api/road.go": "package api\nimport ed " + executor +
				"\nvar w *ed.Writer\n"},
			want: "internal/api/road.go:3: ed.Writer",
		},
		"a write's call built in the binary": {
			files: map[string]string{"cmd/crewlet/road.go": "package main\nimport " + executor +
				"\nvar c = executordomain.Call{OpID: \"op-1\"}\n"},
			want: "cmd/crewlet/road.go:3: executordomain.Call",
		},
		"the domain dot-imported": {
			files: map[string]string{"internal/api/road.go": "package api\nimport . " + executor +
				"\nvar w *Writer\n"},
			want: "internal/api/road.go:2: dot-imports",
		},
		"this package dot-imported": {
			files: map[string]string{"internal/assistant/road.go": "package assistant\nimport . " + estate +
				"\nvar r ExecutorReads\n"},
			want: "internal/assistant/road.go:2: dot-imports",
		},
		"a hand-out the engine exports under a name of its own": {
			files: map[string]string{
				"internal/engine/handout.go": "package engine\nimport " + estate + "\ntype Engine struct{}\n" +
					"func Recorder(e *Engine) estate.ExecutorWrites { return estate.ExecutorWrites{} }\n",
				"internal/api/road.go": "package api\nimport " + engine +
					"\nfunc record(e *engine.Engine) { _ = engine.Recorder(e) }\n",
			},
			want: "internal/api/road.go:3: Recorder",
		},
		"a hand-out method of the domain's writer": {
			files: map[string]string{
				"internal/engine/handout.go": "package engine\nimport ed " + executor + "\ntype Engine struct{}\n" +
					"func (e *Engine) Recorder() *ed.Writer { return nil }\n",
				"cmd/crewlet/road.go": "package main\n" +
					"func record(e interface{ Recorder() any }) any { return e.Recorder() }\n",
			},
			want: "cmd/crewlet/road.go:2: Recorder",
		},
		"the reads, which a surface may hold": {
			files: map[string]string{"internal/api/road.go": "package api\nimport " + estate + "\nimport " +
				executor + "\nvar r estate.ExecutorReads\nvar s executordomain.Session\n" +
				"var id = executordomain.EndOpID\n" +
				"func reads(x interface{ ExecutorReads() estate.ExecutorReads }) estate.ExecutorReads " +
				"{ return x.ExecutorReads() }\n"},
		},
		"another domain's call": {
			files: map[string]string{"internal/api/road.go": "package api\n" +
				"import \"github.com/crewlet/crewlet/internal/artifactsdomain\"\nvar c artifactsdomain.Call\n"},
		},
		"a surface's test": {
			files: map[string]string{"internal/api/road_test.go": "package api\nimport " + estate +
				"\nvar w estate.ExecutorWrites\n"},
		},
		"a surface's testdata": {
			files: map[string]string{"internal/api/testdata/road.go": "package api\nimport " + estate +
				"\nvar w estate.ExecutorWrites\n"},
		},
		"the engine holding its own writes": {
			files: map[string]string{"internal/engine/coordinator.go": "package engine\nimport " + estate +
				"\nvar writes estate.ExecutorWrites\nfunc unexported() estate.ExecutorWrites { return writes }\n"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report, err := scanEngineWrites(plantTree(t, tc.files), personSurfaces)
			if err != nil {
				t.Fatal(err)
			}
			failures := report.failures(personSurfaces)
			switch {
			case tc.want == "" && len(failures) > 0:
				t.Errorf("files that take no road failed:\n%s", strings.Join(failures, "\n"))
			case tc.want != "" && (len(failures) != 1 || !strings.Contains(failures[0], tc.want)):
				t.Errorf("failures %q, want exactly one naming %q", failures, tc.want)
			}
		})
	}

	t.Run("a surface whose tree holds no Go file", func(t *testing.T) {
		t.Parallel()
		root := plantTree(t, map[string]string{"cmd/crewlet/README.md": "no source here\n"})
		if err := os.Remove(filepath.Join(root, "cmd", "crewlet", "quiet.go")); err != nil {
			t.Fatal(err)
		}
		report, err := scanEngineWrites(root, personSurfaces)
		if err != nil {
			t.Fatal(err)
		}
		failures := report.failures(personSurfaces)
		if len(failures) != 1 || !strings.HasPrefix(failures[0], "cmd/crewlet: ") {
			t.Errorf("failures %q, want exactly one saying cmd/crewlet was read empty", failures)
		}
	})
}

// plantTree is a fresh tree holding files (a slash path below the root to its
// source), and a quiet Go file in every surface.
func plantTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, src string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, surface := range personSurfaces {
		write(surface+"/quiet.go", "package "+path.Base(surface)+"\n")
	}
	for rel, src := range files {
		write(rel, src)
	}
	return root
}

// engineWritesReport is one reading of the tree: how many Go files each
// surface's tree yielded, how many of the engine's the hand-outs were derived
// from, and every road a surface takes to the log's write side.
type engineWritesReport struct {
	read        map[string]int
	engineFiles int
	roads       []string
}

// failures is what the guard reports: every road, and every surface whose tree
// yielded no Go file.
func (r engineWritesReport) failures(surfaces []string) []string {
	out := slices.Clone(r.roads)
	for _, surface := range surfaces {
		if r.read[surface] == 0 {
			out = append(out, surface+": the guard read no Go file in this tree, so it "+
				"certifies nothing about it")
		}
	}
	return out
}

// scanEngineWrites reads the non-test Go files of each surface's tree under
// root, less testdata and hidden directories, for a road to the executor log's
// write side, after deriving the engine's hand-outs of it.
func scanEngineWrites(root string, surfaces []string) (engineWritesReport, error) {
	report := engineWritesReport{read: map[string]int{}}
	handOuts, files, err := engineHandOuts(filepath.Join(root, filepath.FromSlash(engineDir)))
	if err != nil {
		return report, err
	}
	report.engineFiles = files
	names := map[string]bool{}
	for _, name := range append(slices.Clone(engineWriteNames), handOuts...) {
		names[name] = true
	}
	fset := token.NewFileSet()
	for _, surface := range surfaces {
		err := sourcetree.Walk(filepath.Join(root, filepath.FromSlash(surface)),
			func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") {
						return filepath.SkipDir
					}
					return nil
				}
				if filepath.Ext(p) != ".go" || strings.HasSuffix(p, "_test.go") {
					return nil
				}
				parsed, err := parser.ParseFile(fset, p, nil, 0)
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				report.read[surface]++
				report.roads = append(report.roads, roadsIn(fset, filepath.ToSlash(rel), parsed, names)...)
				return nil
			})
		if err != nil {
			return report, fmt.Errorf("read %s: %w", surface, err)
		}
	}
	return report, nil
}

// roadsIn is every road one surface file takes to the log's write side.
func roadsIn(fset *token.FileSet, rel string, file *ast.File, names map[string]bool) []string {
	var out []string
	at := func(n ast.Node, what string) {
		out = append(out, fmt.Sprintf("%s:%d: %s", rel, fset.Position(n.Pos()).Line, what))
	}
	for _, spec := range file.Imports {
		if imported, err := strconv.Unquote(spec.Path.Value); err == nil &&
			(imported == estateImport || imported == executorImport) &&
			spec.Name != nil && spec.Name.Name == "." {
			at(spec, "dot-imports "+imported+", which reaches every name of it unqualified: "+
				"import it by name")
		}
	}
	domain := importNamesOf(file, executorImport)
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if names[sel.Sel.Name] {
			at(sel, sel.Sel.Name+" reaches the executor log's writes, which are the engine's "+
				"alone (internal/estate's engine-originated class); a surface holds its "+
				"reads, ExecutorReads")
		}
		if x, isIdent := sel.X.(*ast.Ident); isIdent && domain[x.Name] &&
			slices.Contains(domainWriteNames, sel.Sel.Name) {
			at(sel, x.Name+"."+sel.Sel.Name+" is the executor domain's write side, which "+
				"only the engine holds")
		}
		return true
	})
	return out
}

// engineHandOuts is every exported function and method in dir — the engine
// package — whose results name the log's write side, and how many files it
// read: a door a surface could take the writes through without naming them.
func engineHandOuts(dir string) ([]string, int, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, 0, err
	}
	fset := token.NewFileSet()
	var out []string
	files := 0
	for _, p := range matches {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return nil, 0, err
		}
		files++
		estateNames, domainNames := importNamesOf(parsed, estateImport), importNamesOf(parsed, executorImport)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() || fn.Type.Results == nil {
				continue
			}
			if namesWriteSide(fn.Type.Results, estateNames, domainNames) {
				out = append(out, fn.Name.Name)
			}
		}
	}
	return out, files, nil
}

// importNamesOf is every name file imports want under, a dot-import and a
// blank one aside.
func importNamesOf(file *ast.File, want string) map[string]bool {
	out := map[string]bool{}
	for _, spec := range file.Imports {
		imported, err := strconv.Unquote(spec.Path.Value)
		if err != nil || imported != want {
			continue
		}
		name := path.Base(imported)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name != "." && name != "_" {
			out[name] = true
		}
	}
	return out
}

// namesWriteSide reports whether a declaration's results name the log's write
// side: one of this package's types that carry it, or the domain's writer.
func namesWriteSide(results *ast.FieldList, estateNames, domainNames map[string]bool) bool {
	found := false
	ast.Inspect(results, func(n ast.Node) bool {
		if found {
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, isIdent := sel.X.(*ast.Ident); isIdent {
			found = (estateNames[x.Name] && slices.Contains(writeSideTypes, sel.Sel.Name)) ||
				(domainNames[x.Name] && sel.Sel.Name == "Writer")
		}
		return !found
	})
	return found
}
