package words_test

import (
	"go/build"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/words"
)

type kind string

type version int

// A SET READS AS A SENTENCE SAYS ONE: every member quoted as %q quotes the
// value beside it, the last two joined by the conjunction, the rest by
// commas, and a named type spelt as its value rather than as Go prints the
// slice. A member its caller already said (a command, a phrase) is joined the
// same way and written as it is. Mutation: return fmt.Sprint(set) from either
// and every case fails; drop the Said arm of word and the three said cases do.
func TestASetReadsAsASentenceSaysOne(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"none", words.Or([]string{}), "none"},
		{"one", words.Or([]string{"html"}), `"html"`},
		{"two offered", words.Or([]string{"html", "slides"}), `"html" or "slides"`},
		{"three offered", words.Or([]kind{"html", "slides", "doc"}), `"html", "slides" or "doc"`},
		{"two listed", words.And([]string{"k1", "k2"}), `"k1" and "k2"`},
		{"four listed", words.And([]string{"a", "b", "c", "d"}), `"a", "b", "c" and "d"`},
		{"a quote in a member", words.Or([]string{`say "hi"`, "x"}), `"say \"hi\"" or "x"`},
		{"numbers", words.And([]version{3, 4, 7}), "3, 4 and 7"},
		{"empty list", words.And([]kind(nil)), "none"},
		{"within the cap", words.AndMore([]string{"a", "b"}, 5), `"a" and "b"`},
		{"past the cap", words.AndMore([]string{"a", "b", "c", "d"}, 2), `"a", "b" and 2 more`},
		{"a cap below one", words.AndMore([]string{"a", "b"}, 0), `"a" and 1 more`},
		{"said members", words.Or([]words.Said{"`crewlet llm login a`", "`crewlet llm import a`"}),
			"`crewlet llm login a` or `crewlet llm import a`"},
		{"said phrases", words.And([]words.Said{"${A}", "${B}", "the files under ~/.x"}),
			"${A}, ${B} and the files under ~/.x"},
		{"said past the cap", words.AndMore([]words.Said{"a", "b", "c"}, 1), "a and 2 more"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, c.got, c.want)
		}
		if strings.ContainsAny(c.got, "[]") {
			t.Errorf("%s: %s carries Go's brackets", c.name, c.got)
		}
	}
}

// THE PACKAGE IMPORTS ONLY THE STANDARD LIBRARY, because its readers include
// the leaves internal/iam and internal/artifacts, whose own suites hold them
// to what they may import, and internal/config, which imports nothing but
// the small grammars. Its gate (gate_test.go) reads the tree through
// internal/sourcetree, which is a test's business and not the package's.
//
// Mutation: add a blank import of github.com/google/uuid to words.go and this
// fails naming it.
func TestThePackageImportsOnlyTheStandardLibrary(t *testing.T) {
	t.Parallel()
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("listing the package's files: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range pkg.GoFiles {
		parsed, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: import %s: %v", name, spec.Path.Value, err)
			}
			checked++
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %q, which is not the standard library", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("read no imports: the walk is not reading the package")
	}
}
