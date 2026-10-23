package words_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
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
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"

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
	w := walkModule(t)
	for _, finding := range w.sets {
		t.Error(finding)
	}
	// A WALK THAT READ NOTHING PASSES. The module has a few hundred
	// packages and several thousand format calls; a root resolved to the
	// wrong place, or a signature test that stopped matching fmt.Errorf,
	// finds no offence because it judged nothing.
	t.Logf("judged %d packages and %d format calls", w.judged, w.calls)
	if w.judged < 150 || w.calls < 8000 {
		t.Fatalf("judged %d packages and %d format calls: the walk is not reading the module",
			w.judged, w.calls)
	}
}

// NO SENTENCE JOINS A SET BY HAND. A list a reader reads as a sentence, "a, b
// or c", is [words.Or] or [words.And]: the tree carried a copy of that join in
// a dozen packages (a CLI's field list, a credential's variable references,
// the alarm page's kinds, a grant's app names), each with its own last comma
// and its own idea of quoting, so a member the reader types read unquoted
// beside the value they typed and one copy wrote an Oxford comma the rest did
// not. Every copy had one of four shapes, and each is refused:
//
//   - a strings.Join whose separator is a conjunction,
//     `strings.Join(set, " or ")`;
//   - one written beside a conjunction, `strings.Join(set[:n-1], ", ") +
//     " and " + set[n-1]`;
//   - one that joins all but the last member by commas, whatever joins the
//     last (a conjunction held in a variable is still a conjunction);
//   - a closed set joined by commas right after the words that offer one,
//     `"is one of %s"` or `"(want %s)"` beside `strings.Join(set, ", ")`,
//     which said the set unquoted beside a quoted value and with no "or".
//
// A member that is not a value, a command or a phrase, is [words.Said] and
// joined the same way.
//
// THE CONJUNCTION IS A LOWER-CASE WORD, "or" or "and", which is what tells a
// sentence from the other things a separator joins: a path, a line, a SQL
// condition (`" AND "`), a placeholder list (`", "`). A list joined with
// commas alone and not offered as a choice is not judged, because nothing in
// its shape says a reader reads it as a sentence rather than as a column of
// names (a log field, a header row, a key list); that residue is stated
// rather than guessed at. The words package's own join is the one copy and is
// not judged.
//
// Mutation: put `strings.Join(fields[:len(fields)-1], ", ") + " and " +
// fields[len(fields)-1]` back in cmd/crewlet/artifacts.go's fieldList,
// `strings.Join(grants, " or ")` in cmd/crewlet/refusal.go, or
// `strings.Join(Modes(), ", ")` in internal/gitlab/reconcile.go's unknown
// mode, and this fails naming that line.
func TestNoSentenceJoinsASetByHand(t *testing.T) {
	t.Parallel()
	w := walkModule(t)
	for _, finding := range w.joins {
		t.Error(finding)
	}
	// A WALK THAT SAW NO JOIN PASSES: the module has several hundred, and
	// a callee test that stopped resolving strings.Join would judge none.
	t.Logf("read %d strings.Join calls", w.joinsRead)
	if w.joinsRead < 300 {
		t.Fatalf("read %d strings.Join calls: the walk is not finding them", w.joinsRead)
	}
}

// walked is what one type-checked walk of the module found: every format or
// print that prints a set, every set joined into a sentence by hand, and how
// much it read, which the two tests above judge apart.
type walked struct {
	judged, calls, joinsRead int
	sets, joins              []string
	err                      error
}

// moduleWalk is the walk both gates read, taken once: type-checking the module
// is most of either test's time, and the two questions are asked of the same
// calls.
var moduleWalk = sync.OnceValue(func() walked {
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		return walked{err: err}
	}
	return walkFrom(root)
})

// walkModule is [moduleWalk]'s answer, or t's failure when the walk could not
// be taken.
func walkModule(t *testing.T) walked {
	t.Helper()
	w := moduleWalk()
	if w.err != nil {
		t.Fatal(w.err)
	}
	return w
}

// walkFrom type-checks every package of the module under root and judges every
// call in it.
func walkFrom(root string) walked {
	var w walked
	packages, exports, err := list(root)
	if err != nil {
		return walked{err: err}
	}
	fset := token.NewFileSet()
	imports := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file := exports[path]
		if file == "" {
			return nil, fmt.Errorf("go list gave no export data for %s", path)
		}
		return os.Open(file) //nolint:gosec // a path go list answered for the build cache
	})
	at := func(e ast.Expr) string {
		pos := fset.Position(e.Pos())
		rel, _ := filepath.Rel(root, pos.Filename)
		return fmt.Sprintf("%s:%d", rel, pos.Line)
	}
	for _, pkg := range packages {
		if strings.HasSuffix(path.Base(pkg.importPath), "test") {
			continue
		}
		var files []*ast.File
		for _, name := range pkg.files {
			file, err := parser.ParseFile(fset, filepath.Join(pkg.dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				return walked{err: fmt.Errorf("parsing %s: %w", name, err)}
			}
			files = append(files, file)
		}
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{},
			Uses: map[*ast.Ident]types.Object{}}
		conf := types.Config{Importer: imports}
		if _, err := conf.Check(pkg.importPath, fset, files, info); err != nil {
			return walked{err: fmt.Errorf("type-checking %s: %w", pkg.importPath, err)}
		}
		w.judged++
		// THE ONE COPY OF THE JOIN is the words package's own, which
		// is every other site's fix rather than an offence.
		own := pkg.importPath == module+"/internal/words"
		for _, file := range files {
			// A CHAIN IS JUDGED ONCE, from its outermost +: every
			// inner + of it is the same chain read again.
			inner := map[ast.Expr]bool{}
			ast.Inspect(file, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.BinaryExpr:
					if node.Op != token.ADD || inner[node] {
						return true
					}
					operands := flatten(node, inner)
					for _, joined := range handJoins(info, operands) {
						w.joins = append(w.joins, fmt.Sprintf("%s joins %s into a sentence beside %q "+
							"by hand: say it with words.Or or words.And (words.Said for a member "+
							"that is not a value)", at(joined.join), types.ExprString(joined.join.Args[0]),
							joined.beside))
					}
					return true
				case *ast.CallExpr:
					if isJoin(info, node) {
						w.joinsRead++
						sep, ok := constantText(info, node.Args[1])
						switch {
						case ok && conjunction(fields(sep)):
							w.joins = append(w.joins, fmt.Sprintf("%s joins %s with %q, a sentence's "+
								"conjunction: say it with words.Or or words.And (words.Said for a "+
								"member that is not a value)", at(node), types.ExprString(node.Args[0]), sep))
						case ok && strings.Contains(sep, ",") && allButLast(node.Args[0]) && !own:
							w.joins = append(w.joins, fmt.Sprintf("%s joins %s with %q, which is "+
								"a sentence's list written by hand, its last member joined apart: "+
								"say it with words.Or or words.And (words.Said for a member that is "+
								"not a value)", at(node), types.ExprString(node.Args[0]), sep))
						}
					}
					var found []printed
					if format, operands, ok := formatCall(info, node); ok {
						found = sets(info, format, operands)
						for _, joined := range closedSets(info, format, operands) {
							w.joins = append(w.joins, fmt.Sprintf("%s says %s as %q and a list "+
								"joined by hand: a closed set is words.Or, its members quoted as the "+
								"value beside them is", at(joined.join), types.ExprString(joined.join.Args[0]),
								strings.TrimSpace(joined.beside)))
						}
					} else if operands, ok := printCall(info, node); ok {
						found = printedSets(info, operands)
					} else {
						return true
					}
					w.calls++
					for _, p := range found {
						w.sets = append(w.sets, fmt.Sprintf("%s prints %s with %%%c, which fmt writes "+
							"as Go's [a b c]: say it with words.Or or words.And", at(p.operand),
							types.ExprString(p.operand), p.verb))
					}
				}
				return true
			})
		}
	}
	return w
}

// flatten is the operands of a chain of +, left to right, with every inner +
// of it marked so the walk does not read the chain again from inside.
func flatten(e ast.Expr, inner map[ast.Expr]bool) []ast.Expr {
	b, ok := ast.Unparen(e).(*ast.BinaryExpr)
	if !ok || b.Op != token.ADD {
		return []ast.Expr{e}
	}
	inner[b] = true
	return append(flatten(b.X, inner), flatten(b.Y, inner)...)
}

// handJoin is a strings.Join written beside a conjunction: the join and the
// text beside it.
type handJoin struct {
	join   *ast.CallExpr
	beside string
}

// handJoins is every strings.Join among a chain's operands whose neighbour is
// a constant opening with a conjunction after it (`+ " or " + last`) or
// closing with one before it (`first + ", and " +`).
func handJoins(info *types.Info, operands []ast.Expr) []handJoin {
	var out []handJoin
	for i, operand := range operands {
		call, ok := ast.Unparen(operand).(*ast.CallExpr)
		if !ok || !isJoin(info, call) {
			continue
		}
		if i+1 < len(operands) {
			if text, ok := constantText(info, operands[i+1]); ok {
				if words := fields(text); len(words) > 0 && conjunction(words[:1]) {
					out = append(out, handJoin{join: call, beside: text})
					continue
				}
			}
		}
		if i > 0 {
			if text, ok := constantText(info, operands[i-1]); ok {
				if words := fields(text); len(words) > 0 && conjunction(words[len(words)-1:]) {
					out = append(out, handJoin{join: call, beside: text})
				} else if commaJoin(info, call) && offersASet(text) {
					out = append(out, handJoin{join: call, beside: text})
				}
			}
		}
	}
	return out
}

// closedSets is every operand of format that is a list joined by commas
// right after the words that offer a closed set ("is one of %s",
// "(want %s)"): the set a reader picks from, said unquoted and without
// its last conjunction.
func closedSets(info *types.Info, format string, operands []ast.Expr) []handJoin {
	var out []handJoin
	for _, use := range verbs(format) {
		if use.arg < 0 || use.arg >= len(operands) || !offersASet(use.before) {
			continue
		}
		call, ok := ast.Unparen(operands[use.arg]).(*ast.CallExpr)
		if ok && commaJoin(info, call) {
			out = append(out, handJoin{join: call, beside: use.before})
		}
	}
	return out
}

// offersASet reports whether text ends with the words a sentence offers a
// closed set with: "(want", or "one of" followed by nothing, by a colon, or by
// a short noun phrase naming what the members are ("one of the tokens", "one
// of these spaces:"). Ending at "one of" alone, it passed "write in one of: "
// and "one of the tokens ", each a set said as a column of unquoted names.
// "none of" is not "one of": that is a list read whole.
func offersASet(text string) bool {
	text = strings.ToLower(strings.TrimRight(text, " "))
	if strings.HasSuffix(text, "(want") {
		return true
	}
	at := strings.LastIndex(text, "one of")
	if at < 0 || (at > 0 && unicode.IsLetter(rune(text[at-1]))) {
		return false
	}
	rest := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text[at+len("one of"):]), ":"))
	return rest == "" || (!strings.ContainsAny(rest, ".,;:!?()") && len(strings.Fields(rest)) <= 3)
}

// commaJoin reports whether call is a strings.Join by a constant separator
// holding a comma: a list's members, joined as a sentence joins all but its
// last two.
func commaJoin(info *types.Info, call *ast.CallExpr) bool {
	if !isJoin(info, call) {
		return false
	}
	sep, ok := constantText(info, call.Args[1])
	return ok && strings.Contains(sep, ",")
}

// allButLast reports whether e slices off its operand's last member,
// `set[:len(set)-1]` or `set[:n-1]`: the first half of a list joined by hand,
// whose last member is about to be joined by a conjunction.
func allButLast(e ast.Expr) bool {
	slice, ok := ast.Unparen(e).(*ast.SliceExpr)
	if !ok || slice.Low != nil || slice.High == nil || slice.Slice3 {
		return false
	}
	minus, ok := ast.Unparen(slice.High).(*ast.BinaryExpr)
	if !ok || minus.Op != token.SUB {
		return false
	}
	one, ok := ast.Unparen(minus.Y).(*ast.BasicLit)
	return ok && one.Kind == token.INT && one.Value == "1"
}

// isJoin reports whether call is strings.Join's.
func isJoin(info *types.Info, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || len(call.Args) != 2 {
		return false
	}
	fn, ok := info.Uses[sel.Sel].(*types.Func)
	return ok && fn.FullName() == "strings.Join"
}

// constantText is e's value when it is a constant string.
func constantText(info *types.Info, e ast.Expr) (string, bool) {
	value := info.Types[e].Value
	if value == nil || value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(value), true
}

// fields is text's words, split at the spaces and the punctuation a list puts
// between its members.
func fields(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == ',' || r == ';' || r == '\t' || r == '\n'
	})
}

// conjunction reports whether any of words is the lower-case "or" or "and" a
// sentence's list ends on. Lower-case, so a SQL condition's " AND " and " OR "
// are not one.
func conjunction(words []string) bool {
	return slices.ContainsFunc(words, func(w string) bool { return w == "or" || w == "and" })
}

// list asks go list for every package of the module, its own export data and
// its dependencies'.
//
// THE TOOLCHAIN THAT BUILT THIS BINARY, because export data is read by the
// go/types this binary carries and another go writes a version it may not
// read: the Makefile's (CREWLET_GO, which internal/solo's roster guard lists
// packages with for the same reason), or the one go test names in GOROOT.
// Neither found is fatal rather than a guess at PATH's.
func list(root string) ([]listed, map[string]string, error) {
	goCmd := os.Getenv("CREWLET_GO")
	if goCmd == "" {
		goroot := os.Getenv("GOROOT")
		if goroot == "" {
			return nil, nil, errors.New("neither CREWLET_GO nor GOROOT names the toolchain " +
				"that built this test: run it through make or go test")
		}
		goCmd = filepath.Join(goroot, "bin", "go")
	}
	args := []string{"list", "-export", "-deps",
		"-f", "{{.ImportPath}}\t{{.Export}}\t{{.Dir}}\t{{with .Module}}{{.Path}}{{end}}\t{{join .GoFiles \" \"}}"}
	args = append(args, buildFlags()...)
	args = append(args, "./...")
	cmd := exec.CommandContext(context.Background(), goCmd, args...) //nolint:gosec // the toolchain that built this test
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("go list: %w\n%s", err, stderr.String())
	}
	exports := map[string]string{}
	var packages []listed
	lines := bufio.NewScanner(bytes.NewReader(out))
	lines.Buffer(nil, 1<<20)
	for lines.Scan() {
		field := strings.Split(lines.Text(), "\t")
		if len(field) != 5 {
			return nil, nil, fmt.Errorf("go list answered %q", lines.Text())
		}
		exports[field[0]] = field[1]
		if field[3] == module && field[4] != "" {
			packages = append(packages, listed{importPath: field[0], export: field[1], dir: field[2],
				files: strings.Fields(field[4])})
		}
	}
	return packages, exports, nil
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
	// before is the format's text between the previous verb, or its start,
	// and this one: what a sentence says just before the operand.
	before string
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
	from := 0
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			continue
		}
		before := format[from:i]
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
		out = append(out, use{verb: r, arg: arg, before: before})
		arg++
		from = i + utf8.RuneLen(r)
	}
	return out
}

// THE GATE HEARS EVERY WAY A SET IS OFFERED, and nothing that only resembles
// one. Each true row is a shape a closed set was offered in beside a
// strings.Join, said as a column of unquoted names; each false row is text that
// offers no set. Mutation: end the check at "one of" alone and the colon and
// noun-phrase rows fail; drop the word boundary and the "none of" row does.
func TestTheGateHearsEveryWayASetIsOffered(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]bool{
		"kind %q is not one of ":            true,
		"write in one of: ":                 true,
		"one of the tokens ":                true,
		"pass one of these spaces: ":        true,
		"(want ":                            true,
		"none of ":                          false,
		"one of them failed, and the rest ": false,
		"the nodes ":                        false,
	} {
		if got := offersASet(text); got != want {
			t.Errorf("offersASet(%q) = %v, want %v", text, got, want)
		}
	}
}

// THE GATE KNOWS WHAT A SENTENCE SAYS JUST BEFORE EACH OPERAND, which is how
// it tells a closed set joined by hand ("is one of %s") from any other list:
// the text from the previous verb, or the format's start, to this one.
// Mutation: leave `from` at zero in verbs and the second verb's text reads
// the whole format.
func TestTheGateReadsTheTextBeforeEachVerb(t *testing.T) {
	t.Parallel()
	got := verbs("%q isn't a mode. Use one of %s, or %d%% of none")
	want := []string{"", " isn't a mode. Use one of ", ", or "}
	if len(got) != len(want) {
		t.Fatalf("got %d verbs, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].before != want[i] {
			t.Errorf("verb %d: the text before it is %q, want %q", i, got[i].before, want[i])
		}
	}
}

// THE GATE READS A FORMAT AS FMT DOES, or it judges the wrong operand: an
// explicit index moves the next verb's operand, a * width or precision takes
// one of its own, and %% takes none. Mutation: drop the `arg = n - 1` in
// verbs and the indexed case fails.
func TestTheGateReadsAFormatAsFmtDoes(t *testing.T) {
	t.Parallel()
	type operand struct {
		verb rune
		arg  int
	}
	cases := []struct {
		format string
		want   []operand
	}{
		{"%w: kind %q is not one of %v", []operand{{'w', 0}, {'q', 1}, {'v', 2}}},
		{"100%% of %s", []operand{{'s', 0}}},
		{"%[2]v then %[1]q then %d", []operand{{'v', 1}, {'q', 0}, {'d', 1}}},
		{"%*d and %.*s and %-8v", []operand{{'d', 1}, {'s', 3}, {'v', 4}}},
		{"%+v %#v %x", []operand{{'v', 0}, {'v', 1}, {'x', 2}}},
	}
	for _, c := range cases {
		got := verbs(c.format)
		if len(got) != len(c.want) {
			t.Errorf("%q: got %v, want %v", c.format, got, c.want)
			continue
		}
		for i := range got {
			if got[i].verb != c.want[i].verb || got[i].arg != c.want[i].arg {
				t.Errorf("%q: verb %d is %c of operand %d, want %c of operand %d", c.format, i,
					got[i].verb, got[i].arg, c.want[i].verb, c.want[i].arg)
			}
		}
	}
}
