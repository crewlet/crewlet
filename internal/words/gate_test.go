package words_test

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// module is the module this walk judges; its dependencies are read only for
// the types they export.
const module = "github.com/crewlet/crewlet"

// listed is one package of the module as go list answers it: where its files
// are and the export data its importers are type-checked against.
type listed struct {
	importPath, dir, export string
	files                   []string
}

// NO FORMAT PRINTS A SET. Every call whose callee takes a format and its
// operands (`format string, args ...any`: fmt.Errorf and fmt.Sprintf, a
// package's own invalid() and fail(), a problem list's add) is read with its
// types, and an operand a verb prints is refused when it is a slice, an array
// or a map, because fmt prints those as Go spells them, `[a b c]`, whichever
// verb it is handed. The fix is [words.Or] or [words.And].
//
// AND NEITHER DOES A PRINT: fmt's Sprint, Print, Fprint and Append families
// and the log package's Print, Fatal and Panic families take no format and
// print every operand as %v does, so an operand of theirs is judged as one
// under %v ([printers]). By the callee itself rather than by shape, because a
// function taking only `...any` is as often a logger's key-value pairs as a
// print. Mutation: put `return fmt.Sprint(names)` back in
// internal/search/lexicalsources.go's sourceNames and this fails naming that
// line.
//
// WITH TYPES, because the shape is the operand's type and never its
// spelling: `artifacts.Kinds` and `AuthorKinds()` read like any identifier and
// any call. The export data comes from go list, run by the toolchain this
// test was built with and with its -race and -tags, so it reads the build
// cache the test run already filled.
//
// WHAT IS NOT JUDGED, each for a reason: a test file and a test-support
// package (a name ending in "test", the certification suites), whose text is a
// failing test's and read by whoever runs it; a type with its own String,
// Error or Format method, which says how it prints; a byte slice or array
// under %s, %q, %x or %X, which is text or a digest; %T and %p, which print no
// member; an interface operand, whose shape is not known until it runs; and a
// format that is not a constant, or operands forwarded whole (`args...`),
// which is the wrapper's own call site's to answer.
//
// Mutation: put `%v` back in front of artifacts.Kinds in
// internal/artifactsdomain/write_artifact.go's kind refusal and this fails
// naming that line.
func TestNoFormatPrintsASet(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	packages, exports := list(t, root)

	fset := token.NewFileSet()
	imports := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, fmt.Errorf("go list gave no export data for %s", path)
		}
		return os.Open(file) //nolint:gosec // a path go list answered for the build cache
	})
	calls, judged := 0, 0
	for _, pkg := range packages {
		if strings.HasSuffix(path.Base(pkg.importPath), "test") {
			continue
		}
		var files []*ast.File
		for _, name := range pkg.files {
			file, err := parser.ParseFile(fset, filepath.Join(pkg.dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parsing %s: %v", name, err)
			}
			files = append(files, file)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{},
			Uses: map[*ast.Ident]types.Object{}}
		conf := types.Config{Importer: imports}
		if _, err := conf.Check(pkg.importPath, fset, files, info); err != nil {
			t.Fatalf("type-checking %s: %v", pkg.importPath, err)
		}
		judged++
		for _, file := range files {
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				var found []printed
				if format, operands, ok := formatCall(info, call); ok {
					found = sets(info, format, operands)
				} else if operands, ok := printCall(info, call); ok {
					found = printedSets(info, operands)
				} else {
					return true
				}
				calls++
				for _, at := range found {
					rel, _ := filepath.Rel(root, fset.Position(at.operand.Pos()).Filename)
					t.Errorf("%s:%d prints %s with %%%c, which fmt writes as Go's [a b c]: "+
						"say it with words.Or or words.And", rel, fset.Position(at.operand.Pos()).Line,
						types.ExprString(at.operand), at.verb)
				}
				return true
			})
		}
	}
	// A WALK THAT READ NOTHING PASSES. The module has a few hundred
	// packages and several thousand format calls; a root resolved to the
	// wrong place, or a signature test that stopped matching fmt.Errorf,
	// finds no offence because it judged nothing.
	t.Logf("judged %d packages and %d format calls", judged, calls)
	if judged < 150 || calls < 8000 {
		t.Fatalf("judged %d packages and %d format calls: the walk is not reading the module", judged, calls)
	}
}

// list asks go list for every package of the module, its own export data and
// its dependencies'.
//
// THE TOOLCHAIN THAT BUILT THIS BINARY, because export data is read by the
// go/types this binary carries and another go writes a version it may not
// read: the Makefile's (CREWLET_GO, which internal/solo's roster guard lists
// packages with for the same reason), or the one go test names in GOROOT.
// Neither found is fatal rather than a guess at PATH's.
func list(t *testing.T, root string) ([]listed, map[string]string) {
	t.Helper()
	goCmd := os.Getenv("CREWLET_GO")
	if goCmd == "" {
		goroot := os.Getenv("GOROOT")
		if goroot == "" {
			t.Fatal("neither CREWLET_GO nor GOROOT names the toolchain that built this test: " +
				"run it through make or go test")
		}
		goCmd = filepath.Join(goroot, "bin", "go")
	}
	args := []string{"list", "-export", "-deps",
		"-f", "{{.ImportPath}}\t{{.Export}}\t{{.Dir}}\t{{with .Module}}{{.Path}}{{end}}\t{{join .GoFiles \" \"}}"}
	args = append(args, buildFlags()...)
	args = append(args, "./...")
	cmd := exec.CommandContext(t.Context(), goCmd, args...) //nolint:gosec // the toolchain that built this test
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	exports := map[string]string{}
	var packages []listed
	lines := bufio.NewScanner(bytes.NewReader(out))
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		field := strings.Split(lines.Text(), "\t")
		if len(field) != 5 {
			t.Fatalf("go list answered %q", lines.Text())
		}
		exports[field[0]] = field[1]
		if field[3] == module && field[4] != "" {
			packages = append(packages, listed{importPath: field[0], export: field[1], dir: field[2],
				files: strings.Fields(field[4])})
		}
	}
	return packages, exports
}

// buildFlags are the flags this test binary was built with that change what
// is compiled, so go list answers from the compiles the run already made
// rather than building the module again.
func buildFlags() []string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	var flags []string
	for _, s := range info.Settings {
		switch s.Key {
		case "-race":
			if s.Value == "true" {
				flags = append(flags, "-race")
			}
		case "-tags":
			flags = append(flags, "-tags="+s.Value)
		}
	}
	return flags
}

// formatCall reports whether call hands a constant format and its operands to
// a callee shaped like fmt.Sprintf: its last two parameters a string and
// ...any.
func formatCall(info *types.Info, call *ast.CallExpr) (string, []ast.Expr, bool) {
	if call.Ellipsis.IsValid() {
		return "", nil, false
	}
	callee, ok := info.Types[call.Fun]
	if !ok || callee.IsType() {
		return "", nil, false
	}
	sig, ok := callee.Type.Underlying().(*types.Signature)
	if !ok || !sig.Variadic() || sig.Params().Len() < 2 {
		return "", nil, false
	}
	params := sig.Params()
	rest, ok := params.At(params.Len() - 1).Type().(*types.Slice)
	if !ok {
		return "", nil, false
	}
	if any, ok := rest.Elem().Underlying().(*types.Interface); !ok || !any.Empty() {
		return "", nil, false
	}
	at := params.Len() - 2
	if text, ok := params.At(at).Type().Underlying().(*types.Basic); !ok || text.Kind() != types.String {
		return "", nil, false
	}
	if at >= len(call.Args) {
		return "", nil, false
	}
	value := info.Types[call.Args[at]].Value
	if value == nil || value.Kind() != constant.String {
		return "", nil, false
	}
	return constant.StringVal(value), call.Args[at+1:], true
}

// printers are the functions that take no format and print every operand as
// %v does, each with how many of its leading parameters are not operands (the
// writer an Fprint writes to, the slice an Append appends to). Keyed by
// [types.Func.FullName], so a *log.Logger's methods are the package's own
// functions' twins.
var printers = map[string]int{
	"fmt.Sprint": 0, "fmt.Sprintln": 0, "fmt.Print": 0, "fmt.Println": 0,
	"fmt.Fprint": 1, "fmt.Fprintln": 1, "fmt.Append": 1, "fmt.Appendln": 1,
	"log.Print": 0, "log.Println": 0, "log.Fatal": 0, "log.Fatalln": 0,
	"log.Panic": 0, "log.Panicln": 0,
	"(*log.Logger).Print": 0, "(*log.Logger).Println": 0, "(*log.Logger).Fatal": 0,
	"(*log.Logger).Fatalln": 0, "(*log.Logger).Panic": 0, "(*log.Logger).Panicln": 0,
}

// printCall reports whether call is one of [printers]' and, if so, the
// operands it prints.
func printCall(info *types.Info, call *ast.CallExpr) ([]ast.Expr, bool) {
	if call.Ellipsis.IsValid() {
		return nil, false
	}
	var name *ast.Ident
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		name = fun.Sel
	case *ast.Ident:
		name = fun
	default:
		return nil, false
	}
	fn, ok := info.Uses[name].(*types.Func)
	if !ok {
		return nil, false
	}
	skip, ok := printers[fn.FullName()]
	if !ok || skip > len(call.Args) {
		return nil, false
	}
	return call.Args[skip:], true
}

// printedSets is every operand of a [printers] call that fmt prints as a Go
// set, each as %v prints it.
func printedSets(info *types.Info, operands []ast.Expr) []printed {
	var out []printed
	for _, operand := range operands {
		if t := info.Types[operand].Type; t != nil && printsASet(t, 'v') {
			out = append(out, printed{operand: operand, verb: 'v'})
		}
	}
	return out
}

// printed is one operand a verb prints as a set.
type printed struct {
	operand ast.Expr
	verb    rune
}

// sets is every operand of format that a verb prints as a Go set.
func sets(info *types.Info, format string, operands []ast.Expr) []printed {
	var out []printed
	for _, use := range verbs(format) {
		if use.arg < 0 || use.arg >= len(operands) {
			continue
		}
		operand := operands[use.arg]
		if t := info.Types[operand].Type; t != nil && printsASet(t, use.verb) {
			out = append(out, printed{operand: operand, verb: use.verb})
		}
	}
	return out
}

// printsASet reports whether fmt prints a value of t under verb as Go's
// bracketed set.
func printsASet(t types.Type, verb rune) bool {
	if verb == 'T' || verb == 'p' {
		return false
	}
	methods := types.NewMethodSet(t)
	for _, name := range []string{"String", "Error", "Format"} {
		if methods.Lookup(nil, name) != nil {
			return false
		}
	}
	var elem types.Type
	switch shape := t.Underlying().(type) {
	case *types.Slice:
		elem = shape.Elem()
	case *types.Array:
		elem = shape.Elem()
	case *types.Map:
		return true
	default:
		return false
	}
	if b, ok := elem.Underlying().(*types.Basic); ok && b.Kind() == types.Byte {
		return !strings.ContainsRune("sqxX", verb)
	}
	return true
}

// use is one verb of a format and the operand it prints.
type use struct {
	verb rune
	arg  int
}

// verbs reads format as fmt does: flags, an explicit operand index, a width
// and a precision (either of which may take an operand of its own with *),
// then the verb, which takes the next operand unless %%.
func verbs(format string) []use {
	var out []use
	arg := 0
	index := func(i int) (int, bool) {
		if i >= len(format) || format[i] != '[' {
			return i, false
		}
		end := strings.IndexByte(format[i:], ']')
		if end < 0 {
			return i, false
		}
		n := 0
		for _, r := range format[i+1 : i+end] {
			if r < '0' || r > '9' {
				return i + end + 1, false
			}
			n = n*10 + int(r-'0')
		}
		arg = n - 1
		return i + end + 1, true
	}
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		i++
		for i < len(format) && strings.IndexByte("+-# 0", format[i]) >= 0 {
			i++
		}
		i, _ = index(i)
		if i < len(format) && format[i] == '*' {
			arg++
			i++
		}
		for i < len(format) && format[i] >= '0' && format[i] <= '9' {
			i++
		}
		if i < len(format) && format[i] == '.' {
			i++
			i, _ = index(i)
			if i < len(format) && format[i] == '*' {
				arg++
				i++
			}
			for i < len(format) && format[i] >= '0' && format[i] <= '9' {
				i++
			}
		}
		i, _ = index(i)
		if i >= len(format) {
			break
		}
		if format[i] == '%' {
			continue
		}
		r := rune(format[i])
		if r >= 0x80 {
			r = []rune(format[i:])[0]
		}
		out = append(out, use{verb: r, arg: arg})
		arg++
	}
	return out
}

// THE GATE READS A FORMAT AS FMT DOES, or it judges the wrong operand: an
// explicit index moves the next verb's operand, a * width or precision takes
// one of its own, and %% takes none. Mutation: drop the `arg = n - 1` in
// verbs and the indexed case fails.
func TestTheGateReadsAFormatAsFmtDoes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		format string
		want   []use
	}{
		{"%w: kind %q is not one of %v", []use{{'w', 0}, {'q', 1}, {'v', 2}}},
		{"100%% of %s", []use{{'s', 0}}},
		{"%[2]v then %[1]q then %d", []use{{'v', 1}, {'q', 0}, {'d', 1}}},
		{"%*d and %.*s and %-8v", []use{{'d', 1}, {'s', 3}, {'v', 4}}},
		{"%+v %#v %x", []use{{'v', 0}, {'v', 1}, {'x', 2}}},
	}
	for _, c := range cases {
		got := verbs(c.format)
		if len(got) != len(c.want) {
			t.Errorf("%q: got %v, want %v", c.format, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: verb %d is %c of operand %d, want %c of operand %d", c.format, i,
					got[i].verb, got[i].arg, c.want[i].verb, c.want[i].arg)
			}
		}
	}
}
