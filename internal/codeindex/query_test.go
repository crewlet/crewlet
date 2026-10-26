package codeindex_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/knowledge"
)

// A QUERY IS PARSED INTO ITS FILTERS, and its text is kept as written.
//
// The filters lead and the text is everything after them, inner spaces and
// all; a filter word with nothing after its colon, or one after the text has
// begun, is text. Each filter is held to its own grammar — a repository
// name, a path pattern, one ref — and the whole to knowledge search's bound,
// which is this package's by a test rather than by an import.
func TestAQueryIsParsedIntoItsFilters(t *testing.T) {
	t.Parallel()
	literal := func(q codeindex.Query) codeindex.Query { q.Mode = codeindex.ModeLiteral; return q }
	for _, tc := range []struct {
		in   string
		want codeindex.Query
		err  error
	}{
		{in: "Println", want: literal(codeindex.Query{Text: "Println"})},
		{in: "  repo:api   path:internal/ ref:main  func Foo(  ", want: literal(codeindex.Query{
			Text: "func Foo(", Repos: []string{"api"}, Paths: []string{"internal/"}, Ref: "main"})},
		{in: "repo:api repo:web repo:api x := 1", want: literal(codeindex.Query{
			Text: "x := 1", Repos: []string{"api", "web"}})},
		{in: "path:*.go path:docs/ a  b", want: literal(codeindex.Query{
			Text: "a  b", Paths: []string{"*.go", "docs/"}})},
		{in: "ref:refs/tags/v1.2 tag", want: literal(codeindex.Query{Text: "tag", Ref: "refs/tags/v1.2"})},
		{in: "ref:" + strings.Repeat("ab", 20) + " oid", want: literal(codeindex.Query{
			Text: "oid", Ref: strings.Repeat("ab", 20)})},
		{in: "ref:main ref:main twice", want: literal(codeindex.Query{Text: "twice", Ref: "main"})},
		// The filters lead: after the text begins, a filter word is text.
		{in: "Println repo:api", want: literal(codeindex.Query{Text: "Println repo:api"})},
		// A filter word with nothing after its colon is text.
		{in: "repo: api", want: literal(codeindex.Query{Text: "repo: api"})},
		{in: "path:\tx", want: literal(codeindex.Query{Text: "path:\tx"})},
		{in: "ref:main ref:dev x", err: codeindex.ErrBadQuery},
		{in: "repo:Web x", err: codeindex.ErrBadQuery},
		{in: "repo:web- x", err: codeindex.ErrBadQuery},
		{in: "ref:..main x", err: codeindex.ErrBadQuery},
		{in: "ref:HEAD x", err: codeindex.ErrBadQuery},
		{in: "path:a//b x", err: codeindex.ErrBadQuery},
		{in: "path:a/../b x", err: codeindex.ErrBadQuery},
		{in: "path:**x x", err: codeindex.ErrBadQuery},
		{in: "path:/ x", err: codeindex.ErrBadQuery},
		{in: "repo:api", err: codeindex.ErrBadQuery},
		{in: "  ", err: codeindex.ErrBadQuery},
		{in: "caf\xe9", err: codeindex.ErrBadQuery},
		{in: strings.Repeat("q", knowledge.MaxQueryBytes+1), err: codeindex.ErrQueryTooLong},
		{in: " " + strings.Repeat("q", knowledge.MaxQueryBytes) + "\n",
			want: literal(codeindex.Query{Text: strings.Repeat("q", knowledge.MaxQueryBytes)})},
	} {
		got, err := codeindex.ParseQuery(tc.in)
		switch {
		case tc.err != nil:
			var qe *codeindex.QueryError
			if !errors.Is(err, tc.err) || !errors.Is(err, codeindex.ErrBadQuery) || !errors.As(err, &qe) {
				t.Errorf("ParseQuery(%q) = %+v, %v; want a refusal that is %v", tc.in, got, err, tc.err)
			}
		case err != nil:
			t.Errorf("ParseQuery(%q) refuses it: %v", tc.in, err)
		case !reflect.DeepEqual(got, tc.want):
			t.Errorf("ParseQuery(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
	if codeindex.MaxQueryBytes != knowledge.MaxQueryBytes {
		t.Errorf("a code search takes %d bytes and a knowledge search %d: the bound is one rule",
			codeindex.MaxQueryBytes, knowledge.MaxQueryBytes)
	}

	// WHAT A SURFACE SETS BESIDE THE TEXT is judged by the same grammar.
	q, err := codeindex.ParseQuery("x := 1")
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*codeindex.Query){
		"a mode no search has":      func(q *codeindex.Query) { q.Mode = "fuzzy" },
		"no mode":                   func(q *codeindex.Query) { q.Mode = "" },
		"a repository no name fits": func(q *codeindex.Query) { q.Repos = []string{"API"} },
		"a path pattern refused":    func(q *codeindex.Query) { q.Paths = []string{"a//b"} },
		"a ref that is none":        func(q *codeindex.Query) { q.Ref = "a b" },
		"a regex that does not compile": func(q *codeindex.Query) {
			q.Mode, q.Text = codeindex.ModeRegex, "a(b"
		},
		"a text no index narrows":  func(q *codeindex.Query) { q.Text = "ab" },
		"a regex across lines":     func(q *codeindex.Query) { q.Mode, q.Text = codeindex.ModeRegex, `foo\s+bar` },
		"a text past the bound":    func(q *codeindex.Query) { q.Text = strings.Repeat("q", 401) },
		"a text that is not UTF-8": func(q *codeindex.Query) { q.Text = "caf\xe9" },
	} {
		changed := q
		change(&changed)
		if err := changed.Validate(); !errors.Is(err, codeindex.ErrBadQuery) {
			t.Errorf("a query with %s validates: %v", name, err)
		}
	}
	if err := q.Validate(); err != nil {
		t.Errorf("the parsed query does not validate: %v", err)
	}
}

// EACH MODE IS THE EXPRESSION ITS DOC SAYS, line by line.
func TestAModeIsTheExpressionItsDocSays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode       codeindex.Mode
		text       string
		ignoreCase bool
		match, not []string
	}{
		{mode: codeindex.ModeLiteral, text: "a.b(c)", match: []string{"x a.b(c) y"}, not: []string{"axb(c)", "A.B(C)"}},
		{mode: codeindex.ModeLiteral, text: "été", ignoreCase: true, match: []string{"un ÉTÉ"}, not: []string{"ete"}},
		{mode: codeindex.ModeWord, text: "func", match: []string{"func main()", "a func", "(func)"},
			not: []string{"funcs", "myfunc", "func_x"}},
		// A boundary only where the text has a word character at its edge.
		{mode: codeindex.ModeWord, text: "--force", match: []string{"push --force", "a--force"},
			not: []string{"--forced"}},
		{mode: codeindex.ModeWord, text: "foo(", match: []string{"foo(1)"}, not: []string{"afoo(1)"}},
		// ^ and $ are a line's edges.
		{mode: codeindex.ModeRegex, text: `^func \w+$`, match: []string{"func main"}, not: []string{" func main",
			"func main()"}},
		{mode: codeindex.ModeRegex, text: `Print(ln|f)`, ignoreCase: true, match: []string{"fmt.PRINTF("},
			not: []string{"fmt.Print("}},
	} {
		q := codeindex.Query{Text: tc.text, Mode: tc.mode, IgnoreCase: tc.ignoreCase}
		plan, err := q.Plan()
		if err != nil {
			t.Fatalf("%s %q: %v", tc.mode, tc.text, err)
		}
		for _, line := range tc.match {
			if !plan.Regexp().MatchString(line) {
				t.Errorf("%s %q (ignore case %v) does not match %q", tc.mode, tc.text, tc.ignoreCase, line)
			}
		}
		for _, line := range tc.not {
			if plan.Regexp().MatchString(line) {
				t.Errorf("%s %q (ignore case %v) matches %q", tc.mode, tc.text, tc.ignoreCase, line)
			}
		}
	}
}

// A PATH FILTER MATCHES AS ITS DOC SAYS: a .gitignore line's reading,
// without its negation, escapes and classes.
func TestAPathFilterMatchesAsItsDocSays(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		patterns   []string
		match, not []string
	}{
		{patterns: nil, match: []string{"a", "a/b/c.go"}},
		// A directory is its every file; a / inside anchors at the root.
		{patterns: []string{"internal/code"}, match: []string{"internal/code/doc.go", "internal/code",
			"internal/code/cache/sync.go"}, not: []string{"internal/codeindex/doc.go", "a/internal/code/x.go"}},
		// No / but at the end: any depth.
		{patterns: []string{"*.go"}, match: []string{"main.go", "a/b/c.go"}, not: []string{"main.go.txt", "go"}},
		{patterns: []string{"testdata"}, match: []string{"a/testdata/x.json", "testdata"},
			not: []string{"a/testdata2/x"}},
		// A leading / anchors at the root.
		{patterns: []string{"/Makefile"}, match: []string{"Makefile"}, not: []string{"a/Makefile"}},
		// A trailing / is directories only.
		{patterns: []string{"docs/"}, match: []string{"docs/x.md", "a/docs/y"}, not: []string{"docs", "a/docs"}},
		// ** is any number of components, none included.
		{patterns: []string{"internal/**/*_test.go"}, match: []string{"internal/x_test.go",
			"internal/a/b/c_test.go"}, not: []string{"cmd/x_test.go", "internal/x.go"}},
		{patterns: []string{"**/vendor/**"}, match: []string{"vendor/a.go", "a/vendor/b/c.go"},
			not: []string{"vendors/a.go"}},
		// ? is one character, a rune, never a /.
		{patterns: []string{"?.go"}, match: []string{"a.go", "é.go"}, not: []string{"ab.go", ".go"}},
		{patterns: []string{"a?b"}, match: []string{"axb"}, not: []string{"a/b"}},
		{patterns: []string{"*"}, match: []string{"a", "a/b"}},
		// Any one pattern of several.
		{patterns: []string{"*.md", "cmd/crewlet"}, match: []string{"README.md", "cmd/crewlet/main.go"},
			not: []string{"main.go"}},
	} {
		f, err := codeindex.NewPathFilter(tc.patterns)
		if err != nil {
			t.Fatalf("NewPathFilter(%q): %v", tc.patterns, err)
		}
		for _, path := range tc.match {
			if !f.Match(path) {
				t.Errorf("path:%q does not match %q", tc.patterns, path)
			}
		}
		for _, path := range tc.not {
			if f.Match(path) {
				t.Errorf("path:%q matches %q", tc.patterns, path)
			}
		}
	}
	for _, bad := range []string{"", "/", "a//b", "./a", "a/..", "a**", "**b/c", "a\x00b", "caf\xe9",
		strings.Repeat("a", codeindex.MaxQueryBytes+1)} {
		if _, err := codeindex.NewPathFilter([]string{bad}); !errors.Is(err, codeindex.ErrBadQuery) {
			t.Errorf("path:%q is read: %v", bad, err)
		}
	}
}
