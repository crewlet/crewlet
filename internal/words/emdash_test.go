package words_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// emDash is spelled by its code point, so this file holds none of its own.
const emDash = "—"

// personFacing is the source a person reads the sentences of: what the API
// answers as a `detail` or a `hint`, what the CLI prints, what configuration
// validation reports (on `crewlet validate` and as a `/config` refusal), the
// tracker's tag refusals, which reach the dashboard as an `invalid` detail,
// and the files the retention gestures are decided in (a capacity change, a
// reanchor, an eviction and a readmission), whose told refusals, and the state
// log's details a gate's per-log hint splices in, the API and the CLI answer
// word for word. A directory is walked whole, a file alone.
//
// GROWN AS THE SWEEP REACHES MORE OF THE ENGINE, never shrunk: the domain
// refusal sentences a person also reads as a `detail` (the rest of
// Worklet, Chatlet, Doclet, the identity directory, the tools' own invalid and
// forbidden answers), the `crewlet llm doctor` and integration reports are
// still written with em dashes, and each joins this list in the change that
// rewrites it.
var personFacing = []string{
	"internal/api",
	"cmd/crewlet",
	"internal/config",
	"internal/worklet/tags.go",
	"internal/engine/capacity.go",
	"internal/engine/gesturerefusal.go",
	"internal/engine/nodegate.go",
	"internal/engine/reanchor.go",
	"internal/statelog/eviction.go",
	"internal/statelog/publish.go",
	"internal/statelog/reanchor.go",
}

// NO SENTENCE A PERSON READS CARRIES AN EM DASH.
//
// The engine's sentences for a person are short, and use commas, colons,
// parentheses and full stops: a dash was how a sentence went on past where
// it should have stopped, and the dashboard's copy gate (`app/copy.test.ts`)
// already refused one in everything it ships while the API details it draws,
// the CLI's lines and the configuration problems carried dozens.
//
// Every string literal in [personFacing] is judged except where nobody reads
// it as a sentence: a log line's (the log is for whoever runs the node), a
// panic's (a wiring mistake), a struct tag (the schema's descriptions are
// documentation), and an em dash standing alone as a table cell, which is
// how the CLI's tables mark an empty value.
//
// Mutation: put an em dash back into any `detail`, CLI line or problem in
// those trees and this names its line; drop the log exemption and the walk
// names the log lines it should not judge.
func TestNoSentenceAPersonReadsCarriesAnEmDash(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	read := 0
	for _, entry := range personFacing {
		path := filepath.Join(root, filepath.FromSlash(entry))
		files, err := goFiles(path)
		if err != nil {
			t.Fatalf("%s: %v", entry, err)
		}
		if len(files) == 0 {
			t.Fatalf("%s holds no Go source: the walk reads nothing there", entry)
		}
		for _, file := range files {
			read += judgeEmDashes(t, root, file)
		}
	}
	// A WALK THAT READ NOTHING PASSES, and these trees hold thousands of
	// literals.
	if read < 5000 {
		t.Fatalf("judged %d string literals: the walk is not reading the trees", read)
	}
}

// goFiles is every non-test Go file at path: the file itself, or the
// directory's, walked as this repository's tree.
func goFiles(path string) ([]string, error) {
	if strings.HasSuffix(path, ".go") {
		return []string{path}, nil
	}
	var out []string
	err := sourcetree.Walk(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// judgeEmDashes reports every string literal in file that a person reads and
// that carries an em dash, and answers how many literals it judged.
func judgeEmDashes(t *testing.T, root, file string) int {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	judged := 0
	var stack []ast.Node
	ast.Inspect(parsed, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		if node, ok := n.(*ast.BasicLit); ok {
			// A struct tag is the schema's documentation, never a sentence
			// the engine answers with.
			if node.Kind != token.STRING || taggedField(stack) || unread(stack) {
				return true
			}
			text, err := strconv.Unquote(node.Value)
			if err != nil {
				return true
			}
			judged++
			if strings.Contains(text, emDash) && !emptyCells(text) {
				rel, _ := filepath.Rel(root, file)
				t.Errorf("%s:%d: a sentence a person reads carries an em dash: %q",
					filepath.ToSlash(rel), fset.Position(node.Pos()).Line, text)
			}
		}
		return true
	})
	return judged
}

// taggedField says whether the literal on top of stack is a struct field's
// tag.
func taggedField(stack []ast.Node) bool {
	if len(stack) < 2 {
		return false
	}
	field, ok := stack[len(stack)-2].(*ast.Field)
	return ok && field.Tag == stack[len(stack)-1]
}

// unread says whether the literal on top of stack is an argument of a call
// nobody reads as a sentence: a log line or a panic, at any depth.
func unread(stack []ast.Node) bool {
	for i := len(stack) - 2; i >= 0; i-- {
		call, ok := stack[i].(*ast.CallExpr)
		if !ok {
			continue
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name == "panic" {
				return true
			}
		case *ast.SelectorExpr:
			if logged(fun) {
				return true
			}
		}
	}
	return false
}

// logged says whether sel is a call into a logger: a method of `log`,
// `slog` or a `logger`, or one of the logger methods by name.
func logged(sel *ast.SelectorExpr) bool {
	if x, ok := sel.X.(*ast.Ident); ok {
		switch x.Name {
		case "log", "slog", "logger":
			return true
		}
	}
	switch sel.Sel.Name {
	case "Debug", "Info", "Warn", "Error", "DebugContext", "InfoContext",
		"WarnContext", "ErrorContext", "Log", "LogAttrs":
		return true
	}
	return false
}

// emptyCells says whether every em dash in text stands alone as a cell of a
// tab-separated table row: the mark the CLI's tables print for an empty
// value, which no person reads as punctuation.
func emptyCells(text string) bool {
	cells := strings.FieldsFunc(text, func(r rune) bool { return r == '\t' || r == '\n' })
	for _, cell := range cells {
		if strings.Contains(cell, emDash) && strings.TrimSpace(cell) != emDash {
			return false
		}
	}
	return true
}
