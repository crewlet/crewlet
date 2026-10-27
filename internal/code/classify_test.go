package code_test

import (
	"bufio"
	"bytes"
	"errors"
	"go/parser"
	"go/scanner"
	"go/token"
	"math"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// noClass is the want of a change no class holds for.
const noClass code.ChangeClass = ""

// wantClass holds ClassifyGo's answer for the change from before to after, and
// for the change back — a class is the change's, whichever way it runs — to
// want, noClass for none.
func wantClass(t *testing.T, name, before, after string, want code.ChangeClass) {
	t.Helper()
	for _, change := range [][2]string{{before, after}, {after, before}} {
		got, ok := code.ClassifyGo([]byte(change[0]), []byte(change[1]))
		if !ok {
			got = noClass
		}
		if got != want {
			t.Errorf("%s: classed %q, want %q\nfrom:\n%s\nto:\n%s", name, got, want, change[0], change[1])
		}
	}
}

// THE CHANGE CLASSES ARE A CLOSED SET, in the order a validator and a
// schema read them: the revision's class first, then the paths' narrowest
// first — and no_code_change is never a path's.
func TestTheChangeClassesAreAClosedSet(t *testing.T) {
	t.Parallel()
	if want := []code.ChangeClass{"no_code_change", "whitespace_only", "comment_only"}; !slices.Equal(code.ChangeClasses(), want) {
		t.Errorf("ChangeClasses = %v, want %v", code.ChangeClasses(), want)
	}
	if want := []code.ChangeClass{"whitespace_only", "comment_only"}; !slices.Equal(code.PathClasses(), want) {
		t.Errorf("PathClasses = %v, want %v", code.PathClasses(), want)
	}
	for _, c := range code.ChangeClasses() {
		if !c.Valid() {
			t.Errorf("%s is in the set and Valid says it is not", c)
		}
	}
	for _, c := range []code.ChangeClass{"", "whitespace", "Comment_only"} {
		if c.Valid() {
			t.Errorf("%q is no class this build knows and Valid says it is", c)
		}
	}
}

// A CHANGE THAT MOVES ONLY THE SPACE BETWEEN TOKENS IS whitespace_only — a
// reindent, blank lines, spaces around operators, a line broken where no
// statement ends, Windows line endings, the final newline — and one that
// moves a NEWLINE ENDING A STATEMENT is no change of space at all: the lexer
// inserts a semicolon there, so two statements joined on a line, a return's
// value moved onto its line, or a `;` swapped for a newline get no class.
func TestAWhitespaceOnlyGoChangeIsClassed(t *testing.T) {
	t.Parallel()
	const base = `package sum

// Sum adds what it is given.
func Sum(xs ...int) int {
	total := 0
	for _, x := range xs {
		total += x // one at a time
	}
	return total
}
`
	for _, tc := range []struct{ name, after string }{
		{"reindented with spaces", strings.ReplaceAll(base, "\t", "    ")},
		{"blank lines", `package sum


// Sum adds what it is given.
func Sum(xs ...int) int {

	total := 0

	for _, x := range xs {
		total += x // one at a time
	}

	return total
}
`},
		{"spaces around the tokens", `package sum

// Sum adds what it is given.
func Sum( xs ...int ) int{
	total:=0
	for _,x:=range xs{
		total+=x // one at a time
	}
	return total
}
`},
		{"lines broken where no statement ends", `package sum

// Sum adds what it is given.
func Sum(
	xs ...int) int {
	total := 0
	for _, x := range xs {
		total +=
			x // one at a time
	}
	return total
}
`},
		{"windows line endings", strings.ReplaceAll(base, "\n", "\r\n")},
		{"no final newline", strings.TrimSuffix(base, "\n")},
		{"trailing blanks", strings.ReplaceAll(base, "0\n", "0 \t\n")},
	} {
		wantClass(t, tc.name, base, tc.after, code.ChangeClassWhitespaceOnly)
	}
	for _, tc := range []struct{ name, before, after string }{
		{"two statements joined on one line",
			"package p\n\nfunc f() {\n\ta()\n\tb()\n}\n",
			"package p\n\nfunc f() {\n\ta() b()\n}\n"},
		{"a return's value moved onto its line",
			"package p\n\nfunc f() int {\n\treturn\n\t1\n}\n",
			"package p\n\nfunc f() int {\n\treturn 1\n}\n"},
		{"a line broken after an operand",
			"package p\n\nvar x = f(a,\n\tb)\n",
			"package p\n\nvar x = f(a\n\t, b)\n"},
		{"a semicolon swapped for a newline",
			"package p\n\nfunc f() { a(); b() }\n",
			"package p\n\nfunc f() {\n\ta()\n\tb()\n}\n"},
	} {
		wantClass(t, tc.name, tc.before, tc.after, noClass)
	}
}

// A DIRECTIVE IS CODE: a comment the compiler, cgo, go generate, go test, the
// build or a linter reads gets no class when it is edited, added, removed or
// moved — each of Go's own, a generator's marker, and every linter's
// suppression, a header's generated-code marker among them — and nor does a
// comment slipped between a directive and what it governs. The same edits of
// documentation in their place are comment_only: a class is refused for what
// a comment instructs, never for its being a comment.
func TestADirectiveCommentIsNeverCommentOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, before, after string }{
		{"a compiler pragma",
			"package p\n\n//go:noinline\nfunc f() {}\n",
			"package p\n\n//go:nosplit\nfunc f() {}\n"},
		{"a //go: line in any spelling",
			"package p\n\n//go:Noinline\nfunc f() {}\n",
			"package p\n\n//go:Nosplit\nfunc f() {}\n"},
		{"go generate's command",
			"package p\n\n//go:generate stringer -type=Kind\ntype Kind int\n",
			"package p\n\n//go:generate stringer -type=Mode\ntype Kind int\n"},
		{"an embed's pattern",
			"package p\n\nimport _ \"embed\"\n\n//go:embed hello.txt\nvar s string\n",
			"package p\n\nimport _ \"embed\"\n\n//go:embed other.txt\nvar s string\n"},
		{"a linkname",
			"package p\n\n//go:linkname now time.now\nfunc now() int64\n",
			"package p\n\n//go:linkname now time.Now\nfunc now() int64\n"},
		{"a build constraint",
			"//go:build linux\n\npackage p\n",
			"//go:build darwin\n\npackage p\n"},
		{"a +build line",
			"// +build linux\n\npackage p\n",
			"// +build darwin\n\npackage p\n"},
		{"a line directive",
			"package p\n\nfunc f() {\n//line gen.y:10\n\tg()\n}\n",
			"package p\n\nfunc f() {\n//line gen.y:20\n\tg()\n}\n"},
		{"a line directive in a block comment",
			"package p\n\nfunc f() {\n\t/*line gen.y:10:1*/ g()\n}\n",
			"package p\n\nfunc f() {\n\t/*line gen.y:20:1*/ g()\n}\n"},
		{"a cgo export",
			"package p\n\nimport \"C\"\n\n//export Twice\nfunc Twice() {}\n",
			"package p\n\nimport \"C\"\n\n//export Thrice\nfunc Twice() {}\n"},
		{"a gccgo extern",
			"package p\n\n//extern puts\nfunc puts()\n",
			"package p\n\n//extern printf\nfunc puts()\n"},
		{"a tool's name:value directive",
			"package p\n\n//revive:disable\nfunc f() {}\n",
			"package p\n\n//revive:enable\nfunc f() {}\n"},
		{"a nolint naming its linter",
			"package p\n\nfunc f() {\n\tg() //nolint:errcheck\n}\n",
			"package p\n\nfunc f() {\n\tg() //nolint:gosec\n}\n"},
		{"a nolint with a space",
			"package p\n\nfunc f() {\n\tg() // nolint\n}\n",
			"package p\n\nfunc f() {\n\tg() // nolint:errcheck\n}\n"},
		{"a bare nolint added",
			"package p\n\nfunc f() {\n\tg()\n}\n",
			"package p\n\nfunc f() {\n\tg() //nolint\n}\n"},
		{"a staticcheck ignore",
			"package p\n\nfunc f() {\n\t//lint:ignore SA1019 the old API is what this tests\n\tg()\n}\n",
			"package p\n\nfunc f() {\n\t//lint:ignore SA1019 the new API is what this tests\n\tg()\n}\n"},
		{"a gosec suppression",
			"package p\n\nfunc f() {\n\tg() // #nosec G104 -- checked above\n}\n",
			"package p\n\nfunc f() {\n\tg() // #nosec G104 G307 -- checked above\n}\n"},
		{"a generator's marker",
			"package p\n\ntype T struct {\n\t// +kubebuilder:validation:Maximum=10\n\tN int\n}\n",
			"package p\n\ntype T struct {\n\t// +kubebuilder:validation:Maximum=100\n\tN int\n}\n"},
		{"an example's output",
			"package p\n\nfunc ExampleF() {\n\tF()\n\t// Output: 3\n}\n",
			"package p\n\nfunc ExampleF() {\n\tF()\n\t// Output: 4\n}\n"},
		{"a line of an unordered output",
			"package p\n\nfunc ExampleF() {\n\tF()\n\t// Unordered output:\n\t// a\n\t// b\n}\n",
			"package p\n\nfunc ExampleF() {\n\tF()\n\t// Unordered output:\n\t// a\n\t// c\n}\n"},
		{"a directive added",
			"package p\n\nfunc f() {}\n",
			"package p\n\n//go:noinline\nfunc f() {}\n"},
		{"a directive removed below its doc",
			"package p\n\n// f does x.\n//go:noinline\nfunc f() {}\n",
			"package p\n\n// f does x.\nfunc f() {}\n"},
		{"a directive moved to another declaration",
			"package p\n\n//go:noinline\nfunc f() {}\n\nfunc g() {}\n",
			"package p\n\nfunc f() {}\n\n//go:noinline\nfunc g() {}\n"},
		{"a comment slipped between an embed and its variable",
			"package p\n\nimport _ \"embed\"\n\n//go:embed hello.txt\nvar s string\n",
			"package p\n\nimport _ \"embed\"\n\n//go:embed hello.txt\n/* the greeting */\nvar s string\n"},
		// Past a blank line, in a group of its own, the comment is still
		// between the directive and what it governs: a block comment there
		// fails the embed.
		{"a comment slipped below a blank line under an embed",
			"package p\n\nimport _ \"embed\"\n\n//go:embed hello.txt\n\nvar s string\n",
			"package p\n\nimport _ \"embed\"\n\n//go:embed hello.txt\n\n/* the greeting */\nvar s string\n"},
		{"a generated-code marker added to the header",
			"// Package p does x.\npackage p\n",
			"// Code generated by hand. DO NOT EDIT.\n\n// Package p does x.\npackage p\n"},
		{"a looser generated-code marker beside the package clause",
			"package p // the core\n\nfunc f() {}\n",
			"package p // the core; do not edit\n\nfunc f() {}\n"},
	} {
		wantClass(t, tc.name, tc.before, tc.after, noClass)
	}
	for _, tc := range []struct{ name, before, after string }{
		{"a function's doc",
			"package p\n\n// f does x.\nfunc f() {}\n",
			"package p\n\n// f does y.\nfunc f() {}\n"},
		{"the doc above a pragma",
			"package p\n\n// f does x.\n//go:noinline\nfunc f() {}\n",
			"package p\n\n// f does y.\n//go:noinline\nfunc f() {}\n"},
		{"a license above a build constraint",
			"// Copyright 2025.\n\n//go:build linux\n\npackage p\n",
			"// Copyright 2026.\n\n//go:build linux\n\npackage p\n"},
		{"a comment beside a call",
			"package p\n\nfunc f() {\n\tg() // why\n}\n",
			"package p\n\nfunc f() {\n\tg() // because\n}\n"},
		{"a comment that only mentions a directive",
			"package p\n\n// f is not //go:noinline.\nfunc f() {}\n",
			"package p\n\n// f is never //go:noinline.\nfunc f() {}\n"},
		{"a generator's words below the header",
			"package p\n\nfunc f() {}\n\n// g's key is generated by the store.\nfunc g() {}\n",
			"package p\n\nfunc f() {}\n\n// g's key is generated by the database.\nfunc g() {}\n"},
	} {
		wantClass(t, tc.name, tc.before, tc.after, code.ChangeClassCommentOnly)
	}
}

// WHITESPACE ALONE CHANGES WHAT A DIRECTIVE DOES, so whitespace around one is
// code: a //line or //go:generate indented off its line's start is ignored, a
// pragma after code on its line is an error, a blank line parts an //export
// or a nolint from what it governs — as a column the two no longer share
// does — one inside an example's output splits off what go test compares, and
// a +build line with no blank line under it, or under a block comment, is no
// constraint. Whitespace that moves no directive — beside plain comments, a
// body below a pragma, or a blank line beside a //go:build line, which
// go/build reads anywhere in a header — is whitespace.
func TestAWhitespaceChangeAroundADirectiveIsCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, before, after string }{
		{"a line directive indented off its line's start",
			"package p\n\nfunc f() {\n//line gen.y:10\n\tg()\n}\n",
			"package p\n\nfunc f() {\n\t//line gen.y:10\n\tg()\n}\n"},
		{"go generate's line indented",
			"package p\n\n//go:generate stringer -type=Kind\ntype Kind int\n",
			"package p\n\n  //go:generate stringer -type=Kind\ntype Kind int\n"},
		{"a pragma moved onto a line of code",
			"package p\n\nvar x = 1\n\n//go:noinline\nfunc f() {}\n",
			"package p\n\nvar x = 1 //go:noinline\nfunc f() {}\n"},
		{"an export parted from its function",
			"package p\n\n//export F\nfunc F() {}\n",
			"package p\n\n//export F\n\nfunc F() {}\n"},
		{"a nolint parted from its declaration below its doc",
			"package p\n\n// F is long.\n//nolint:gocyclo\nfunc F() {}\n",
			"package p\n\n// F is long.\n//nolint:gocyclo\n\nfunc F() {}\n"},
		{"documentation joined to a directive's group",
			"package p\n\n// F is long.\n\n//nolint:gocyclo\nfunc F() {}\n",
			"package p\n\n// F is long.\n//nolint:gocyclo\nfunc F() {}\n"},
		// golangci-lint holds a nolint group to the statement starting on the
		// line below it AT THE GROUP'S COLUMN, so the column its first comment
		// starts at, and the one the statement does, are the directive's.
		{"the doc opening a nolint's group moved off its column",
			"package p\n\nfunc f() {\n\t// g cannot fail here.\n\t//nolint:errcheck\n\tg()\n}\n",
			"package p\n\nfunc f() {\n// g cannot fail here.\n\t//nolint:errcheck\n\tg()\n}\n"},
		{"a statement moved off its nolint's column",
			"package p\n\nfunc f() {\n\t//nolint:errcheck\n\tg()\n}\n",
			"package p\n\nfunc f() {\n\t//nolint:errcheck\n\t\tg()\n}\n"},
		{"a blank line inside an example's output",
			"package p\n\nfunc ExampleF() {\n\tF()\n\t// Output:\n\t// a\n\t// b\n}\n",
			"package p\n\nfunc ExampleF() {\n\tF()\n\t// Output:\n\n\t// a\n\t// b\n}\n"},
		{"a +build line no longer parted from the package clause",
			"// +build linux\n\npackage p\n",
			"// +build linux\npackage p\n"},
		{"a block comment above a +build line",
			"// +build linux\n\npackage p\n",
			"/* Package p. */\n\n// +build linux\n\npackage p\n"},
	} {
		wantClass(t, tc.name, tc.before, tc.after, noClass)
	}
	for _, tc := range []struct{ name, before, after string }{
		{"a doc comment parted from its function",
			"package p\n\n// F does x.\nfunc F() {}\n",
			"package p\n\n// F does x.\n\nfunc F() {}\n"},
		{"a body below a pragma reindented",
			"package p\n\n//go:noinline\nfunc f() {\n\tg()\n}\n",
			"package p\n\n//go:noinline\nfunc f() {\n        g()\n}\n"},
		{"a blank line between a license and a build constraint",
			"// Copyright 2026.\n//go:build linux\n\npackage p\n",
			"// Copyright 2026.\n\n//go:build linux\n\npackage p\n"},
	} {
		wantClass(t, tc.name, tc.before, tc.after, code.ChangeClassWhitespaceOnly)
	}
}

// GO GENERATE READS LINES, NOT TOKENS: every line of a file that starts
// //go:generate and a blank is a command it runs — inside a block comment or
// a raw string as much as in a comment of its own — and a carriage return the
// lexer drops from a comment or a raw string is a byte it reads. So a change
// to such a line, wherever it sits, is code, as is the newline that ends one
// (go generate fails on a last line with none) and a carriage return that
// takes one past the reader go generate reads it through (it fails on a line
// too long), while a change elsewhere in a file holding one keeps its class.
func TestAGoGenerateLineIsCodeWhereverItSits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, before, after string }{
		{"a command added in a block comment",
			"package p\n\nfunc f() {}\n",
			"package p\n\n/*\n//go:generate stringer -type=Kind\n*/\nfunc f() {}\n"},
		{"a command edited in a block comment",
			"package p\n\n/*\n//go:generate stringer -type=Kind\n*/\nfunc f() {}\n",
			"package p\n\n/*\n//go:generate stringer -type=Mode\n*/\nfunc f() {}\n"},
		{"a carriage return dropped before a command in a block comment",
			"package p\n\n/*\n\r//go:generate stringer -type=Kind\n*/\nfunc f() {}\n",
			"package p\n\n/*\n//go:generate stringer -type=Kind\n*/\nfunc f() {}\n"},
		{"a carriage return dropped before a command in a raw string",
			"package p\n\nconst s = `\n\r//go:generate stringer -type=Kind\n`\n",
			"package p\n\nconst s = `\n//go:generate stringer -type=Kind\n`\n"},
		{"a carriage return dropped inside a command's directive",
			"package p\n\n//go:gen\rerate stringer -type=Kind\ntype Kind int\n",
			"package p\n\n//go:generate stringer -type=Kind\ntype Kind int\n"},
		{"a carriage return dropped inside a command",
			"package p\n\n//go:generate stringer -type=Ki\rnd\ntype Kind int\n",
			"package p\n\n//go:generate stringer -type=Kind\ntype Kind int\n"},
		{"the newline after a command in a block comment dropped",
			"package p\n\n/*\n//go:generate stringer -type=Kind */\n",
			"package p\n\n/*\n//go:generate stringer -type=Kind */"},
	} {
		wantClass(t, tc.name, tc.before, tc.after, noClass)
	}
	// go generate reads a line through a bufio.Reader of the default size, its
	// newline included, and fails on a command the reader cannot hold, running
	// nothing after it: a carriage return at the end of a command that fills
	// the reader to the byte stops go generate there, and dropping it runs the
	// command. One byte shorter, the command fits either way.
	fits := "//go:generate echo " + strings.Repeat("x", bufio.NewReader(nil).Size()-len("//go:generate echo \n"))
	wantClass(t, "a carriage return taking a command past go generate's reader",
		"package p\n\n"+fits+"\ntype Kind int\n", "package p\n\n"+fits+"\r\ntype Kind int\n", noClass)
	short := fits[:len(fits)-1]
	wantClass(t, "a carriage return ending a command within go generate's reader",
		"package p\n\n"+short+"\ntype Kind int\n", "package p\n\n"+short+"\r\ntype Kind int\n", code.ChangeClassWhitespaceOnly)
	const generating = "package p\n\n/*\n//go:generate stringer -type=Kind\n*/\nvar _ int\n\n// Kind is a kind.\ntype Kind int\n"
	wantClass(t, "documentation elsewhere in a file with a command",
		generating, strings.Replace(generating, "a kind.", "a sort.", 1), code.ChangeClassCommentOnly)
	wantClass(t, "the words around a command in its block comment",
		generating, strings.Replace(generating, "/*\n", "/* Run before a release.\n", 1), code.ChangeClassCommentOnly)
	wantClass(t, "windows line endings across a file with commands",
		"package p\n\n//go:generate stringer -type=Kind\ntype Kind int\n\n/*\n//go:generate stringer -type=Mode\n*/\ntype Mode int\n",
		"package p\r\n\r\n//go:generate stringer -type=Kind\r\ntype Kind int\r\n\r\n/*\r\n//go:generate stringer -type=Mode\r\n*/\r\ntype Mode int\r\n",
		code.ChangeClassWhitespaceOnly)
}

// A DIRECTIVE IS READ AS ITS TOOL READS IT: the compiler reads a pragma, an
// embed's pattern and a //line directive with every carriage return the
// lexer drops from a comment's text, but for the one a Windows line ending
// leaves at its end. So a carriage return inside a directive is code — a
// pragma it splits, a pattern or a file name it changes — while the Windows
// line endings of a file of directives, and one inside documentation, are
// whitespace.
func TestADirectivesCarriageReturnsAreCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, before, after string }{
		{"a carriage return inside a pragma",
			"package p\n\n//go:noin\rline\nfunc f() {}\n",
			"package p\n\n//go:noinline\nfunc f() {}\n"},
		{"a carriage return inside an embed's pattern",
			"package p\n\nimport _ \"embed\"\n\n//go:embed hel\rlo.txt\nvar s string\n",
			"package p\n\nimport _ \"embed\"\n\n//go:embed hello.txt\nvar s string\n"},
		{"a carriage return inside a line directive's file",
			"package p\n\nfunc f() {\n//line ge\rn.y:10\n\tg()\n}\n",
			"package p\n\nfunc f() {\n//line gen.y:10\n\tg()\n}\n"},
	} {
		wantClass(t, tc.name, tc.before, tc.after, noClass)
	}
	const directives = "package p\n\n//go:noinline\nfunc f() {\n//line gen.y:10\n\tg() //nolint:errcheck\n}\n"
	wantClass(t, "windows line endings across directives",
		directives, strings.ReplaceAll(directives, "\n", "\r\n"), code.ChangeClassWhitespaceOnly)
	wantClass(t, "a carriage return inside documentation",
		"package p\n\n// f does\r x.\nfunc f() {}\n", "package p\n\n// f does x.\nfunc f() {}\n", code.ChangeClassWhitespaceOnly)
}

// AN EXAMPLE'S BODY IS ITS TEST: go test compares an example's output with
// the last comment group of its body, so a comment added below an Output:
// leaves a test that compares nothing, and every comment of a top-level body
// holding an output, in either version, is code. Another function's comments,
// and an Output: outside any body, are documentation still.
func TestAnExamplesBodyCommentsAreCode(t *testing.T) {
	t.Parallel()
	const base = "package p\n\nfunc ExampleF() {\n\t// call it once\n\tF()\n\t// Output: 3\n}\n\nfunc g() {\n\t// a note\n\tF()\n}\n"
	for _, tc := range []struct{ name, after string }{
		{"a comment added below the output", strings.Replace(base, "// Output: 3\n", "// Output: 3\n\n\t// and done\n", 1)},
		{"a comment above the output edited", strings.Replace(base, "call it once", "call it", 1)},
		{"the output removed", strings.Replace(base, "\t// Output: 3\n", "", 1)},
	} {
		wantClass(t, tc.name, base, tc.after, noClass)
	}
	wantClass(t, "another function's comment", base, strings.Replace(base, "a note", "a remark", 1), code.ChangeClassCommentOnly)
	wantClass(t, "an output marker outside any body",
		"package p\n\n// Output: is what F prints.\nfunc F() {}\n",
		"package p\n\n// Output: is what F writes.\nfunc F() {}\n", code.ChangeClassCommentOnly)
}

// A RAW STRING'S SPACES ARE ITS VALUE'S, so a change to them is code — as a
// string's and a rune's are — while the space around one is the file's, and a
// carriage return inside one is no character of its value at all: the
// language discards it, so a file's Windows line endings, raw strings
// included, are whitespace.
func TestARawStringsWhitespaceIsCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, before, after string }{
		{"a space in a raw string", "package p\n\nconst s = `a b`\n", "package p\n\nconst s = `a  b`\n"},
		{"a raw string's lines reindented", "package p\n\nconst s = `\n\tline\n`\n", "package p\n\nconst s = `\n\t\tline\n`\n"},
		{"a raw string's line joined", "package p\n\nconst s = `a\nb`\n", "package p\n\nconst s = `a b`\n"},
		{"a space in a string", "package p\n\nconst s = \"a b\"\n", "package p\n\nconst s = \"a  b\"\n"},
		{"a rune's space for a tab", "package p\n\nconst r = ' '\n", "package p\n\nconst r = '\t'\n"},
	} {
		wantClass(t, tc.name, tc.before, tc.after, noClass)
	}
	wantClass(t, "the space around a raw string",
		"package p\n\nconst s = `a b`\n", "package p\n\nconst s =\t`a b`\n", code.ChangeClassWhitespaceOnly)
	wantClass(t, "windows line endings across a raw string",
		"package p\n\nconst s = `a\nb`\n", "package p\r\n\r\nconst s = `a\r\nb`\r\n", code.ChangeClassWhitespaceOnly)
}

// A CGO PREAMBLE IS C SOURCE, so any change to it is code — its text, a space
// in it, a line added, a blank line inside it or between it and the import
// (cgo reads the import's doc comment, which a blank line detaches) — in each
// place cgo reads one from: the declaration's doc, and a parenthesized
// declaration's spec's. Everything else in a cgo file is Go, classed as Go.
func TestACgoPreambleIsCode(t *testing.T) {
	t.Parallel()
	const base = "package p\n\n// #include <stdlib.h>\n// #define LIMIT 10\nimport \"C\"\n\n// f frees nothing.\nfunc f() {}\n"
	for _, tc := range []struct{ name, after string }{
		{"its C edited", strings.Replace(base, "LIMIT 10", "LIMIT 20", 1)},
		{"a space in its C", strings.Replace(base, "LIMIT 10", "LIMIT  10", 1)},
		{"a line added to it", strings.Replace(base, "10\nimport", "10\n// #define FLOOR 1\nimport", 1)},
		{"a blank line inside it", strings.Replace(base, "h>\n// #define", "h>\n\n// #define", 1)},
		{"a blank line between it and the import", strings.Replace(base, "10\nimport", "10\n\nimport", 1)},
		{"a line of it indented", strings.Replace(base, "// #define", "  // #define", 1)},
	} {
		wantClass(t, tc.name, base, tc.after, noClass)
	}
	for _, tc := range []struct{ name, before, after string }{
		{"a spec's doc in a parenthesized declaration",
			"package p\n\nimport (\n\t\"unsafe\"\n\n\t// #include <string.h>\n\t\"C\"\n)\n",
			"package p\n\nimport (\n\t\"unsafe\"\n\n\t// #include <strings.h>\n\t\"C\"\n)\n"},
		{"the declaration's doc where C is its only spec",
			"package p\n\n// #include <stdio.h>\nimport (\n\t\"C\"\n)\n",
			"package p\n\n// #include <stdlib.h>\nimport (\n\t\"C\"\n)\n"},
	} {
		wantClass(t, tc.name, tc.before, tc.after, noClass)
	}
	wantClass(t, "a function's doc in a cgo file", base, strings.Replace(base, "nothing", "everything", 1), code.ChangeClassCommentOnly)
	wantClass(t, "a function in a cgo file reindented", base, strings.Replace(base, "func f() {}", "func f() {\n}", 1), code.ChangeClassWhitespaceOnly)
	wantClass(t, "a comment above another import",
		"package p\n\n// fmt formats.\nimport \"fmt\"\n",
		"package p\n\n// fmt prints.\nimport \"fmt\"\n", code.ChangeClassCommentOnly)
}

// A VERSION THE LEXER CANNOT JUDGE GETS NO CLASS: an empty one — a file
// added or deleted — one with a lexical error, a byte-order mark one version
// has and the other lacks, and one past MaxClassedBlobBytes, while a version
// of exactly that many bytes is lexed.
func TestAnUnclassableVersionGetsNoClass(t *testing.T) {
	t.Parallel()
	const src = "package p\n\nfunc f() {}\n"
	for _, tc := range []struct{ name, before, after string }{
		{"an added file", "", src},
		{"two empty versions", "", ""},
		{"an unterminated raw string in both", "package p\n\nvar s = `open\n", "package p\n\nvar s =  `open\n"},
		{"an illegal character in both", "package p\n\n@\n", "package p\n\n @\n"},
		{"a byte-order mark added", src, "\uFEFF" + src},
	} {
		wantClass(t, tc.name, tc.before, tc.after, noClass)
	}
	wantClass(t, "a byte-order mark on both", "\uFEFF"+src, "\uFEFF"+strings.Replace(src, "f() {}", "f()  {}", 1), code.ChangeClassWhitespaceOnly)

	// A comment pads a file to exactly n bytes.
	sized := func(n int) string {
		const head, tail = "package p\n\n//", "\n"
		return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
	}
	atBound := sized(code.MaxClassedBlobBytes)
	wantClass(t, "a version at the bound",
		atBound, strings.Replace(atBound, "package p", "package\tp", 1), code.ChangeClassWhitespaceOnly)
	wantClass(t, "a version a byte past the bound",
		atBound, strings.Replace(atBound, "package p", "package  p", 1), noClass)
}

// A CLASSIFICATION'S MEMORY FOLLOWS ITS BYTES, NOT ITS COMMENTS: the walk
// holds the comments of one gap at a time, as records of 24 bytes in a gap
// that doubles as it grows — under four records a comment allocated in all —
// so past what the lexer allocates reading the two versions it allocates
// under 32 bytes a byte of them, since a comment takes three at the least,
// its slashes and its newline. That holds where the file is one gap of
// comments, every one of which a directive governs, the costliest shape.
// Keeping every comment of both versions to the walk's end, and a
// directive's entries made of them, allocated 231 bytes a byte of it.
//
// NOT PARALLEL, because an allocation figure is the whole process's; each
// figure is the least of three runs, so another goroutine's allocation
// during one is not counted.
func TestAClassificationAllocatesByItsBytes(t *testing.T) {
	before := filled("package p\n\n//go:noinline\n", "//\n", "func f() {}\n", 64<<10)
	after := bytes.Clone(before)
	lexed := leastAllocated(func() { lexes(before); lexes(after) })
	var class code.ChangeClass
	var ok bool
	classed := leastAllocated(func() { class, ok = code.ClassifyGo(before, after) })
	if !ok || class != code.ChangeClassWhitespaceOnly {
		t.Fatalf("classed %q (%v), want whitespace_only: the case is not the one it measures", class, ok)
	}
	if limit := lexed + 32*uint64(len(before)+len(after)); classed > limit {
		t.Errorf("classing %d bytes of comments allocated %d bytes, past the lexer's %d and 32 a byte: %d",
			len(before)+len(after), classed, lexed, limit)
	}
}

// leastAllocated is the fewest bytes any of three calls of f allocated.
func leastAllocated(f func()) uint64 {
	least := uint64(math.MaxUint64)
	for range 3 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		f()
		runtime.ReadMemStats(&after)
		least = min(least, after.TotalAlloc-before.TotalAlloc)
	}
	return least
}

// FuzzClassifyGo: whatever two versions are, a class is the same both ways
// and a version against itself is whitespace_only wherever it lexes; and a
// class given is TRUE — read again by means ClassifyGo does not share: a lexer
// dropping the comments finds the same tokens, go/parser accepts both or
// neither, the plainest directives read alike in both, byte for byte as their
// tools read them, go generate's reading of the file a line at a time finds
// the same commands, and a whitespace_only change differs in nothing but
// blanks and line breaks.
func FuzzClassifyGo(f *testing.F) {
	for _, seed := range [][2]string{
		{"package p\n\nfunc f() {\n\ta()\n\tb()\n}\n", "package p\n\nfunc f() {\n    a()\n\n    b()\n}\n"},
		{"package p\n\nfunc f() {\n\ta()\n\tb()\n}\n", "package p\n\nfunc f() {\n\ta() b()\n}\n"},
		{"package p\n\n// f does x.\n//go:noinline\nfunc f() {}\n", "package p\n\n// f does y.\n//go:noinline\nfunc f() {}\n"},
		{"package p\n\nfunc f() {\n//line gen.y:10\n\tg()\n}\n", "package p\n\nfunc f() {\n\t//line gen.y:10\n\tg()\n}\n"},
		{"package p\n\n// #include <stdlib.h>\nimport \"C\"\n", "package p\n\n// #include <stdlib.h>\n\nimport \"C\"\n"},
		{"package p\n\nfunc ExampleF() {\n\tF()\n\t// Output: 3\n}\n", "package p\n\nfunc ExampleF() {\n\tF()\n\t// Output: 3\n\n\t// done\n}\n"},
		{"// +build linux\n\npackage p\n", "/* x */\n\n// +build linux\n\npackage p\n"},
		{"// Copyright.\n//go:build linux\n\npackage p\n", "// Copyright.\n\n//go:build linux\n\npackage p\n"},
		{"package p\n\nconst s = `a\nb`\n", "package p\r\n\r\nconst s = `a\r\nb`\r\n"},
		{"package p\n\nvar x = 1 /* a\nb */ + 2\n", "package p\n\nvar x = 1 /* a b */ + 2\n"},
		{"\uFEFFpackage p\n", "package p\n"},
		{"package p\n\ntype T struct {\n\t// +optional\n\tN int // nolint\n}\n", "package p\n\ntype T struct {\n\t// +optional\n\tN int //nolint\n}\n"},
		{"// Package p.\npackage p\n", "// Code generated by x. DO NOT EDIT.\n\n// Package p.\npackage p\n"},
		{"package p\n\n/*\n//go:generate x\n*/\n", "package p\n\n/*\n\r//go:generate x\n*/\n"},
		{"package p\n\nconst s = `\n//go:generate x\n`\n", "package p\r\n\r\nconst s = `\r\n//go:generate x\r\n`\r\n"},
		{"package p\n\n//go:noin\rline\nfunc f() {}\n", "package p\n\n//go:noinline\nfunc f() {}\n"},
	} {
		f.Add([]byte(seed[0]), []byte(seed[1]))
	}
	f.Fuzz(func(t *testing.T, before, after []byte) {
		class, ok := code.ClassifyGo(before, after)
		if back, backOK := code.ClassifyGo(after, before); back != class || backOK != ok {
			t.Fatalf("classed %q (%v) one way and %q (%v) the other", class, ok, back, backOK)
		}
		if len(before) > 0 && len(before) <= code.MaxClassedBlobBytes && lexes(before) {
			if same, sameOK := code.ClassifyGo(before, before); !sameOK || same != code.ChangeClassWhitespaceOnly {
				t.Fatalf("a version against itself classed %q (%v), want whitespace_only", same, sameOK)
			}
		}
		if !ok {
			return
		}
		if !slices.Contains(code.PathClasses(), class) {
			t.Fatalf("classed %q, which is no path's class", class)
		}
		if !slices.Equal(tokensOf(before), tokensOf(after)) {
			t.Fatalf("classed %q, and the versions' tokens differ:\n%q\n%q", class, tokensOf(before), tokensOf(after))
		}
		if parses(before) != parses(after) {
			t.Fatalf("classed %q, and go/parser accepts one version and not the other", class)
		}
		if !slices.Equal(directivesOf(before), directivesOf(after)) {
			t.Fatalf("classed %q, and the directives differ:\n%q\n%q", class, directivesOf(before), directivesOf(after))
		}
		if !slices.Equal(generateLinesOf(before), generateLinesOf(after)) {
			t.Fatalf("classed %q, and go generate's commands differ:\n%q\n%q", class, generateLinesOf(before), generateLinesOf(after))
		}
		if class == code.ChangeClassWhitespaceOnly && spaceless(before) != spaceless(after) {
			t.Fatalf("classed whitespace_only, and the versions differ in more than blanks and line breaks")
		}
	})
}

// scanAll calls see with every token of src the lexer returns in mode, and
// where its bytes start and end, and reports whether it found no lexical
// error.
func scanAll(src []byte, mode scanner.Mode, see func(off, end int, tok token.Token, lit string)) bool {
	var s scanner.Scanner
	file := token.NewFileSet().AddFile("", -1, len(src))
	s.Init(file, src, nil, mode)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return s.ErrorCount == 0
		}
		see(file.Offset(pos), file.Offset(s.End()), tok, lit)
	}
}

// lexes reports whether src lexes with no error.
func lexes(src []byte) bool {
	return scanAll(src, scanner.ScanComments, func(int, int, token.Token, string) {})
}

// tokensOf is src's tokens with its comments dropped and the semicolons the
// lexer inserts kept: what the two versions of a classed change share.
func tokensOf(src []byte) []string {
	var toks []string
	scanAll(src, 0, func(_, _ int, tok token.Token, lit string) { toks = append(toks, tok.String()+" "+lit) })
	return toks
}

// directivesOf is src's comments that read as the plainest directives, in
// order, each as its tool reads it: its bytes from its start to the newline
// or the */ that ends it, carriage returns and all, but the one a Windows
// line ending leaves at a line comment's end.
func directivesOf(src []byte) []string {
	var out []string
	scanAll(src, scanner.ScanComments, func(off, _ int, tok token.Token, lit string) {
		if tok != token.COMMENT {
			return
		}
		plain := lit == "//nolint" || strings.Contains(lit, "#nosec")
		for _, prefix := range []string{"//go:", "//line ", "/*line ", "//export ", "//extern ", "//nolint:", "//nolint ", "//lint:", "// +build"} {
			plain = plain || strings.HasPrefix(lit, prefix)
		}
		if !plain {
			return
		}
		raw := src[off:]
		if strings.HasPrefix(lit, "//") {
			if nl := bytes.IndexByte(raw, '\n'); nl >= 0 {
				raw = raw[:nl]
			}
			raw = bytes.TrimSuffix(raw, []byte("\r"))
		} else if end := bytes.Index(raw[2:], []byte("*/")); end >= 0 {
			raw = raw[:2+end+2]
		}
		out = append(out, string(raw))
	})
	return out
}

// generateLinesOf is the commands go generate finds in src, read as it reads
// them, its loop restated (cmd/go's generate.go): a line at a time through a
// bufio.Reader of the default size, whatever lexeme a line sits in. A line
// that starts //go:generate and a blank is a command, less the carriage
// return a Windows line ending leaves before its newline; one the reader
// cannot hold, or one no newline ends, is go generate's error, and nothing
// after it runs. Any other line too long for the reader is skipped whole.
func generateLinesOf(src []byte) []string {
	var commands []string
	input := bufio.NewReader(bytes.NewReader(src))
	for {
		line, err := input.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if isGoGenerate(line) {
				return append(commands, "fails on a line too long: "+string(line))
			}
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = input.ReadSlice('\n')
			}
			if err != nil {
				return commands
			}
			continue
		}
		if err != nil {
			if isGoGenerate(line) {
				commands = append(commands, "fails on "+string(line))
			}
			return commands
		}
		if isGoGenerate(line) {
			commands = append(commands, "runs "+string(bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))))
		}
	}
}

// isGoGenerate reports a line go generate reads as a command: cmd/go's own
// test.
func isGoGenerate(line []byte) bool {
	return bytes.HasPrefix(line, []byte("//go:generate ")) || bytes.HasPrefix(line, []byte("//go:generate\t"))
}

// parses reports whether go/parser accepts src.
func parses(src []byte) bool {
	_, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	return err == nil
}

// spaceless is src without its blanks and line breaks, byte for byte.
func spaceless(src []byte) string {
	var b strings.Builder
	for _, c := range src {
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			b.WriteByte(c)
		}
	}
	return b.String()
}
