package browser_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/browser/chromiumtest"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// NO GO FILE BUT THE RENDER'S CLIENT NAMES A DEVTOOLS METHOD: no comment,
// string or rune literal, or run of literals joined by + in the module's Go
// files, tests and generated files included, names a command or event either
// pinned Chromium's own protocol describes, as written or with JSON's \u
// escapes decoded, but in internal/artifactrender/internal/cdp itself; nor
// does a string literal of a package of another module the module links. On
// both pins: the floor describes names the newest dropped.
//
// Mutations, each red: in a test under internal/sandbox, a literal naming the
// page domain's navigate command, the same in a comment, split across a +
// after a call that takes a literal, its dot escaped for JSON in a raw
// literal, or an event's name; the same beneath the client; no exemption.
func TestNoGoFileButTheRendersClientNamesADevToolsMethod(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		methods := chromiumtest.Protocol(t, chrome).Methods()
		read, err := goText()
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case read.files < minGoFiles || read.tests < minTestFiles:
			t.Fatalf("the walk read %d Go files, %d of them tests: it read something else", read.files, read.tests)
		case read.exempt == 0:
			t.Fatalf("the walk read no file of %s, the one exemption: it read something else", clientDir)
		case read.packages < minDependencies:
			t.Fatalf("the walk read %d packages of other modules: it read the module's own alone", read.packages)
		}
		arm := t.Name()[strings.LastIndex(t.Name(), "/")+1:]
		for _, h := range named(slices.Concat(read.own, read.deps), methods) {
			t.Errorf("%s:%d names %s, which the %s pin's own protocol describes: no Go but the artifact "+
				"render's client (%s) names a DevTools method (internal/browser's doc, ADR-0165)",
				h.file, h.line, h.name, arm, clientDir)
		}
	})
}

// THE WALK FINDS A METHOD HOWEVER GO SPELLS IT, naming its file and line: in
// an interpreted or a raw literal, a raw one with its dot escaped for JSON, a
// line or a block comment, a run of literals joined by + across lines and
// parentheses, or one after a call that takes a literal, and in a package
// beneath the exempt one; never in a dotted word that is no method, a selector
// in code, a file that is not Go or one in the exempt directory, nor in a
// dependency's comment. The name is built at run time, so this file names none.
//
// Mutations, each red: match the value only as written; break a run at a
// parenthesis; let a call's literal join the run after it; read a dependency's
// comments; read the exempt directory; exempt the packages beneath it.
func TestTheWalkFindsAMethodHoweverGoSpellsIt(t *testing.T) {
	t.Parallel()
	domain, member := "Probe", "touchDown"
	name := strings.Join([]string{domain, member}, ".")
	methods := map[string]bool{name: true}
	for _, tc := range []struct {
		spelling, src string
		found         bool
	}{
		{"an interpreted literal", "package p\n\nvar v = " + strconv.Quote(name) + "\n", true},
		{"a raw literal", "package p\n\nvar v = `" + name + "`\n", true},
		{"a raw literal, its dot escaped for JSON", "package p\n\nvar v = `" + domain + `\u002e` + member + "`\n", true},
		{"a line comment", "package p\n\n// " + name + " is sent first\n", true},
		{"a block comment", "package p\n\n/*\n" + name + "\n*/\n", true},
		{"a run across lines and parentheses", "package p\n\nvar v = " + strconv.Quote(domain+".") + " +\n\t(" +
			strconv.Quote(member) + ")\n", true},
		{"a run after a call that takes a literal", "package p\n\nvar v = f(\"x\") + " + strconv.Quote(domain+".") +
			" + " + strconv.Quote(member) + "\n", true},
		{"a dotted word that is no method", "package p\n\nvar v = \"A " + domain + ".md#\"\n", false},
		{"a selector in code", "package p\n\nvar v = " + strings.ToLower(domain) + "." + domain +
			strings.ToUpper(member[:1]) + member[1:] + "\n", false},
	} {
		read, err := texts("p.go", []byte(tc.src), true)
		if err != nil {
			t.Fatalf("%s: %v", tc.spelling, err)
		}
		hits := named(words(read), methods)
		switch {
		case tc.found && (len(hits) != 1 || hits[0].file != "p.go" || hits[0].line != 3 || hits[0].name != name):
			t.Errorf("%s: found %+v, want %s at p.go:3", tc.spelling, hits, name)
		case !tc.found && len(hits) != 0:
			t.Errorf("%s: found %+v, want nothing", tc.spelling, hits)
		}
	}

	root := t.TempDir()
	for file, body := range map[string]string{
		"notes.txt":         name + "\n",
		"p.go":              "package p\n",
		"exempt/client.go":  "package client\n\nvar v = " + strconv.Quote(name) + "\n",
		"exempt/sub/sub.go": "package sub\n\nvar v = " + strconv.Quote(name) + "\n",
		"dependency/dep.go": "package dep\n\n// " + name + "\nvar v = " + strconv.Quote(name) + "\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c, err := walkModule(root, "exempt")
	if err != nil {
		t.Fatal(err)
	}
	// dependency/dep.go is read as the module's here, comments and all, and a
	// package under the exempt one is no part of it.
	if hits := named(c.own, methods); c.files != 3 || c.exempt != 1 || len(hits) != 3 {
		t.Errorf("the walk read %d Go files and %d exempt ones, finding %+v; want 3, 1, and dep.go's two and "+
			"sub.go's one", c.files, c.exempt, hits)
	}
	dep := filepath.Join(root, "dependency")
	deps, err := readDependencies(map[string][]string{dep: {"dep.go"}})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dep, "dep.go")
	if hits := named(deps, methods); len(hits) != 1 || hits[0].file != want || hits[0].line != 4 {
		t.Errorf("a dependency's texts named %+v; want its literal alone, at %s:4", hits, want)
	}
}

// clientDir is the artifact render's DevTools client, the one directory whose
// files may name a DevTools method: none beneath it.
const clientDir = "internal/artifactrender/internal/cdp"

// The walk's floors. The module holds some 8,000 Go files, 4,500 of them
// tests, and links about 480 packages of other modules, so a walk below any of
// them read something other than the tree: the first two are storetest's plan
// gate's floors, and the third a quarter of what ./cmd/crewlet alone links.
const (
	minGoFiles      = 1000
	minTestFiles    = 500
	minDependencies = 100
)

// text is one piece of Go text the walk reads, where it sits: a comment, a
// literal's value, or a run of literals joined by +.
type text struct {
	file  string
	line  int
	value string
}

// corpus is what the walk read: the words of the module's own texts and of
// those of the packages of other modules it links, with what it counted.
type corpus struct {
	own, deps                      []word
	files, tests, exempt, packages int
}

// goText is the module's corpus, read once per test binary.
var goText = sync.OnceValues(func() (corpus, error) {
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		return corpus{}, err
	}
	c, err := walkModule(root, clientDir)
	if err != nil {
		return corpus{}, err
	}
	dirs, err := dependencies(root)
	if err != nil {
		return corpus{}, err
	}
	c.deps, err = readDependencies(dirs)
	c.packages = len(dirs)
	return c, err
})

// walkModule reads every Go file under root, testdata aside, but those in the
// directory exempt, which it counts; a directory beneath exempt is read.
func walkModule(root, exempt string) (corpus, error) {
	var c corpus
	err := sourcetree.Walk(root, func(file string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(file, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if path.Dir(rel) == exempt {
			c.exempt++
			return nil
		}
		src, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		c.files++
		if strings.HasSuffix(rel, "_test.go") {
			c.tests++
		}
		read, err := texts(rel, src, true)
		c.own = append(c.own, words(read)...)
		return err
	})
	return c, err
}

// listed is one package as go list names it.
type listed struct {
	Dir      string
	GoFiles  []string
	CgoFiles []string
	Standard bool
	Module   *struct{ Main bool }
}

// dependencies is every package of another module that the module's packages
// or their tests link, as the files each one builds.
func dependencies(root string) (map[string][]string, error) {
	cmd := exec.CommandContext(context.Background(), "go", "list", "-e", "-deps", "-test",
		"-json=Dir,GoFiles,CgoFiles,Standard,Module", "./...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, errors.New("go list -deps -test: " + err.Error() + ": " + stderr.String())
	}
	dirs := map[string][]string{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listed
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			return dirs, nil
		} else if err != nil {
			return nil, err
		}
		if p.Standard || p.Module == nil || p.Module.Main {
			continue
		}
		dirs[p.Dir] = append(p.GoFiles, p.CgoFiles...)
	}
}

// readDependencies is the words of the string literals of the files dirs
// name, never of their comments: what a package of another module says is its
// own, and what it sends is in its strings.
func readDependencies(dirs map[string][]string) ([]word, error) {
	var out []word
	for dir, files := range dirs {
		for _, name := range files {
			file := filepath.Join(dir, name)
			src, err := os.ReadFile(file)
			if err != nil {
				return nil, err
			}
			read, err := texts(file, src, false)
			if err != nil {
				return nil, err
			}
			out = append(out, words(read)...)
		}
	}
	return out, nil
}

// texts is a file's comments, when comments is set, the value of each of its
// string and rune literals, and each run of string literals one expression
// joins by +, across lines: a parenthesised operand continues a run, and any
// other operand, a call among them, ends it. Runs are read from the syntax
// tree, since only it tells a call's parentheses from an operand's.
func texts(file string, src []byte, comments bool) ([]text, error) {
	fset := token.NewFileSet()
	mode := parser.SkipObjectResolution
	if comments {
		mode |= parser.ParseComments
	}
	f, err := parser.ParseFile(fset, file, src, mode)
	if err != nil {
		return nil, err
	}
	var out []text
	add := func(at token.Pos, value string) {
		out = append(out, text{file: file, line: fset.Position(at).Line, value: value})
	}
	for _, group := range f.Comments {
		for _, c := range group.List {
			add(c.Slash, c.Text)
		}
	}
	inner := map[*ast.BinaryExpr]bool{} // a + read already as part of an outer one
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BasicLit:
			if n.Kind == token.STRING || n.Kind == token.CHAR {
				add(n.ValuePos, unquoted(n))
			}
		case *ast.BinaryExpr:
			if n.Op != token.ADD || inner[n] {
				break
			}
			var run []string
			var at token.Pos
			flush := func() {
				if len(run) > 1 {
					add(at, strings.Join(run, ""))
				}
				run = nil
			}
			for _, operand := range operands(n, inner) {
				lit, ok := operand.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					flush()
					continue
				}
				if len(run) == 0 {
					at = lit.ValuePos
				}
				run = append(run, unquoted(lit))
			}
			flush()
		}
		return true
	})
	return out, nil
}

// operands is what e joins by +, in order, through parentheses and the + it
// holds, each of which it marks in inner.
func operands(e ast.Expr, inner map[*ast.BinaryExpr]bool) []ast.Expr {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return operands(x.X, inner)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			inner[x] = true
			return append(operands(x.X, inner), operands(x.Y, inner)...)
		}
	}
	return []ast.Expr{e}
}

// unquoted is a literal's value, Go's escapes decoded.
func unquoted(lit *ast.BasicLit) string {
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return lit.Value
	}
	return value
}

// word is a word of a text that could name a method, where the text sits.
type word struct {
	file string
	line int
	name string
}

// jsonEscape is one of JSON's \u escapes, which a value carries to a browser
// as the character it stands for.
var jsonEscape = regexp.MustCompile(`\\u([0-9a-fA-F]{4})`)

// words is every word of each text that could name a method, read as written
// and again with JSON's \u escapes decoded, each once a text.
func words(texts []text) []word {
	var out []word
	for _, x := range texts {
		seen := map[string]bool{}
		add := func(name string) {
			if !seen[name] {
				seen[name] = true
				out = append(out, word{file: x.file, line: x.line, name: name})
			}
		}
		dottedWords(x.value, add)
		if strings.Contains(x.value, `\u`) {
			dottedWords(jsonEscape.ReplaceAllStringFunc(x.value, func(e string) string {
				n, err := strconv.ParseUint(e[2:], 16, 32)
				if err != nil {
					return e
				}
				return string(rune(n))
			}), add)
		}
	}
	return out
}

// dottedWords calls each with every word of s that could name a method: a run
// of letters and digits opening on an upper-case letter, a dot, and a run
// opening on a lower-case one, standing alone as a regexp's \b reads it (an
// underscore joins a word). A byte scan, since the walk reads the module's
// every comment and literal.
func dottedWords(s string, each func(string)) {
	alnum := func(b byte) bool { return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' }
	for i := 0; i < len(s); i++ {
		if s[i] != '.' {
			continue
		}
		start, end := i, i+1
		for start > 0 && alnum(s[start-1]) {
			start--
		}
		for end < len(s) && alnum(s[end]) {
			end++
		}
		switch {
		case start == i || end == i+1:
		case s[start] < 'A' || s[start] > 'Z' || s[i+1] < 'a' || s[i+1] > 'z':
		case start > 0 && s[start-1] == '_', end < len(s) && s[end] == '_':
		default:
			each(s[start:end])
		}
	}
}

// named is every word of ws that methods holds.
func named(ws []word, methods map[string]bool) []word {
	var out []word
	for _, w := range ws {
		if methods[w.name] {
			out = append(out, w)
		}
	}
	return out
}
