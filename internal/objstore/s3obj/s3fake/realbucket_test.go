package s3fake_test

import (
	"bytes"
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/regexlit"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// benchVariable is the VALUE of a string literal that names one of the real
// bucket's variables or what a name is built from: it CONTAINS the stem,
// CREWLET_BENCH_S3, and holds no whitespace. So it matches a whole name
// (os.Getenv, t.Setenv, `NAME=`), the stem or `_`-ended prefix, a Sprintf
// format, a reference (`${NAME}`, `$NAME`, envref.Resolve) or a struct tag
// (`env:"NAME"`): each a way a second parser spells one. A sentence mentioning
// a variable holds a space and does not match. The stem is matched ANYWHERE
// because whole-name matching
// (`^CREWLET_BENCH_S3_[A-Z0-9_]*$`) passes `os.Getenv(stem + "_BUCKET")` and a
// Sprintf format, and anchoring at the stem passes every reference and tag.
// Blind to: the stem SPLIT across literals, and a value HOLDING WHITESPACE (a
// multi-key struct tag, a multi-line literal), which looks like a sentence.
var benchVariable = regexp.MustCompile(`^\S*CREWLET_BENCH_S3\S*$`)

// benchVariableNeeds is what a value benchVariable matches cannot be spelled
// without — the stem — DERIVED from the pattern (regexlit.Required) rather
// than written beside it, where it would drift from the pattern it stands for.
var benchVariableNeeds = regexlit.Required(benchVariable)

// THE REAL BUCKET IS NAMED IN ONE PLACE: no Go source outside this package
// spells one of the CREWLET_BENCH_S3_* variables' names, or the stem a name
// is built from.
//
// Every benchmark of a real bucket reads the variables through
// s3fake.RealBucket, because a second parser quietly reads them differently (a
// region it does not require, a prefix it does not default, a path style it
// reads as false), passes its own package's benchmarks and measures a bucket
// nobody meant. Only a scan of the tree finds such a copy.
//
// The parser's own reads are the scan's CONTROL: a walk or matcher that
// stopped finding the variable there would find nothing elsewhere and pass.
func TestNoOtherPackageNamesTheRealBucketsVariables(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	own := filepath.Join(root, "internal", "objstore", "s3obj", "s3fake")
	parser := filepath.Join(own, "realbucket.go")
	var (
		files int
		// parserNames is what the scan found in RealBucket's own file.
		parserNames = map[string]bool{}
		offenders   []string
	)
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, n := range namesIn(path, body) {
			if filepath.Dir(path) == own {
				if path == parser {
					parserNames[n.name] = true
				}
				continue
			}
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, fmt.Sprintf("%s:%d: %q", filepath.ToSlash(rel), n.line, n.name))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if files == 0 {
		t.Fatal("read no Go files — this gate was certifying nothing")
	}
	if !parserNames["CREWLET_BENCH_S3_BUCKET"] {
		t.Fatalf("found no CREWLET_BENCH_S3_BUCKET in internal/objstore/s3obj/s3fake/realbucket.go, "+
			"where s3fake.RealBucket reads it: the scan has stopped recognising a variable's "+
			"name, so it would find no copy anywhere (names it did find there: %v)", parserNames)
	}
	if len(offenders) > 0 {
		t.Fatalf("these name a real bucket's variable outside internal/objstore/s3obj/s3fake; "+
			"read the bucket through s3fake.RealBucket, the one parser of them:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// namesIn is the gate's whole judgement of one Go source: every literal in it
// whose value benchVariable matches, and none in a file mayHoldAName skips.
func namesIn(path string, body []byte) []variableName {
	if !mayHoldAName(body) {
		return nil
	}
	return variableNames(path, body)
}

// mayHoldAName is false only for a Go source in which no string literal's
// VALUE can hold the stem, so the scanner need not run on it: scanning every
// file costs the walk about six seconds over this tree under the race
// detector, scanning only what this admits under one. The check reads BYTES
// and the matcher a literal's unquoted value; they differ in two ways. A
// NUMERIC escape (`\x`, `\u`, `\U`, a backslash before an octal digit) spells
// any character its bytes never show, a simple one only a quote, backslash or
// control character, none in the stem
// (TestTheScanFindsTheStemInEveryUnspacedLiteral holds that). And a raw string
// drops every carriage return. So a file with no stem, numeric escape or
// carriage return holds no matching literal; an escaped backslash is read as
// an escape too, admitting more than needed, never less.
func mayHoldAName(body []byte) bool {
	return benchVariableNeeds.Admits(body) ||
		bytes.IndexByte(body, '\r') >= 0 ||
		holdsNumericEscape(body)
}

// holdsNumericEscape is whether a backslash in body precedes a byte that would
// make it a numeric escape inside an interpreted string.
func holdsNumericEscape(body []byte) bool {
	for {
		i := bytes.IndexByte(body, '\\')
		if i < 0 || i+1 == len(body) {
			return false
		}
		switch c := body[i+1]; {
		case c == 'x', c == 'u', c == 'U', '0' <= c && c <= '7':
			return true
		}
		body = body[i+1:]
	}
}

// variableName is one literal naming a bench variable, and its line.
type variableName struct {
	name string
	line int
}

// variableNames is every string literal in a Go source whose value
// benchVariable matches. The scanner's own tokens rather than a regular
// expression over the text, so a name in a COMMENT — every doc that tells a
// person which variable to set — is never mistaken for a read, and a literal
// is judged by the value it spells rather than by how its bytes spell it.
func variableNames(path string, body []byte) []variableName {
	fset := token.NewFileSet()
	file := fset.AddFile(path, fset.Base(), len(body))
	var s scanner.Scanner
	s.Init(file, body, nil, 0)
	var found []variableName
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return found
		}
		if tok != token.STRING {
			continue
		}
		value, err := strconv.Unquote(lit)
		if err != nil || !benchVariable.MatchString(value) {
			continue
		}
		found = append(found, variableName{name: value, line: fset.Position(pos).Line})
	}
}

// THE SCAN SEES THE STEM IN EVERY LITERAL THAT SPELLS IT WHOLE AND HOLDS NO
// WHITESPACE, and no sentence about a variable.
//
// A second parser can build a name from the stem (`"CREWLET_BENCH_S3" +
// "_BUCKET"`, a Sprintf format), set one for a child, point at one by reference
// (`"${CREWLET_BENCH_S3_BUCKET}"`) or tag, or spell a character with an escape.
// Each case is judged by what the walk runs, the skip check AND the scanner, so
// a check that stopped admitting an escape, or a matcher that stopped at whole
// names or at a value starting with the stem, goes red here though the walk
// would stay green. It also holds mayHoldAName's two premises no case can
// exercise: the pattern still requires a stem, and no stem character is one a
// SIMPLE escape spells.
func TestTheScanFindsTheStemInEveryUnspacedLiteral(t *testing.T) {
	t.Parallel()
	// simpleEscaped is what Go's simple escapes spell inside an interpreted
	// string: \a \b \f \n \r \t \v \\ and \".
	const simpleEscaped = "\a\b\f\n\r\t\v\\\""
	clauses := benchVariableNeeds.Clauses()
	if len(clauses) == 0 {
		t.Fatalf("%v requires no substring, so mayHoldAName skips no file", benchVariable)
	}
	for _, clause := range clauses {
		for _, needed := range clause {
			if strings.ContainsAny(needed, simpleEscaped) {
				t.Fatalf("%v requires %q, which holds a character a simple escape spells; "+
					"mayHoldAName admits a file on a numeric escape only", benchVariable, needed)
			}
		}
	}
	for _, tc := range []struct {
		name string
		// stmt is a statement of a function body.
		stmt string
		want []string
	}{
		{
			name: "a whole name",
			stmt: `_ = os.Getenv("CREWLET_BENCH_S3_BUCKET")`,
			want: []string{"CREWLET_BENCH_S3_BUCKET"},
		},
		{
			name: "a whole name, raw",
			stmt: "_ = os.Getenv(`CREWLET_BENCH_S3_REGION`)",
			want: []string{"CREWLET_BENCH_S3_REGION"},
		},
		{
			name: "the stem a name is joined to",
			stmt: `const stem = "CREWLET_BENCH_S3"; _ = os.Getenv(stem + "_BUCKET")`,
			want: []string{"CREWLET_BENCH_S3"},
		},
		{
			name: "the prefix a key is joined to",
			stmt: `_ = os.Getenv("CREWLET_BENCH_S3_" + key)`,
			want: []string{"CREWLET_BENCH_S3_"},
		},
		{
			name: "a format",
			stmt: `_ = os.Getenv(fmt.Sprintf("CREWLET_BENCH_S3_%s", key))`,
			want: []string{"CREWLET_BENCH_S3_%s"},
		},
		{
			name: "an entry of a child's environment",
			stmt: `cmd.Env = append(cmd.Env, "CREWLET_BENCH_S3_BUCKET="+bucket)`,
			want: []string{"CREWLET_BENCH_S3_BUCKET="},
		},
		{
			name: "a braced reference expanded from the environment",
			stmt: `_ = os.ExpandEnv("${CREWLET_BENCH_S3_BUCKET}")`,
			want: []string{"${CREWLET_BENCH_S3_BUCKET}"},
		},
		{
			name: "a bare reference expanded from the environment",
			stmt: `_ = os.ExpandEnv("$CREWLET_BENCH_S3_BUCKET")`,
			want: []string{"$CREWLET_BENCH_S3_BUCKET"},
		},
		{
			// The tree's own idiom for reading the environment: a config
			// value holding a reference, resolved where it is used.
			name: "a reference resolved the way a config value is",
			stmt: `_ = envref.Resolve("${CREWLET_BENCH_S3_BUCKET}", os.LookupEnv)`,
			want: []string{"${CREWLET_BENCH_S3_BUCKET}"},
		},
		{
			name: "a struct tag a decoder reads the environment by",
			stmt: "var cfg struct {\n\t\tBucket string `env:\"CREWLET_BENCH_S3_BUCKET\"`\n\t}\n\t_ = cfg",
			want: []string{`env:"CREWLET_BENCH_S3_BUCKET"`},
		},
		{
			name: "a hex escape after the stem",
			stmt: `_ = os.Getenv("CREWLET_BENCH_S3\x5fBUCKET")`,
			want: []string{"CREWLET_BENCH_S3_BUCKET"},
		},
		{
			name: "a hex escape inside the stem",
			stmt: `_ = os.Getenv("CREWLET\x5fBENCH_S3_BUCKET")`,
			want: []string{"CREWLET_BENCH_S3_BUCKET"},
		},
		{
			name: "a short Unicode escape inside the stem",
			stmt: `_ = os.Getenv("CREWLET\u005fBENCH_S3_BUCKET")`,
			want: []string{"CREWLET_BENCH_S3_BUCKET"},
		},
		{
			name: "a long Unicode escape inside the stem",
			stmt: `_ = os.Getenv("CREWLET\U0000005fBENCH_S3_BUCKET")`,
			want: []string{"CREWLET_BENCH_S3_BUCKET"},
		},
		{
			name: "an octal escape inside the stem",
			stmt: `_ = os.Getenv("CREWLET\137BENCH_S3_BUCKET")`,
			want: []string{"CREWLET_BENCH_S3_BUCKET"},
		},
		{
			// A raw string's value drops every carriage return in it.
			name: "a carriage return inside a raw stem",
			stmt: "_ = os.Getenv(`CREWLET_\rBENCH_S3_BUCKET`)",
			want: []string{"CREWLET_BENCH_S3_BUCKET"},
		},
		{
			name: "a sentence naming one",
			stmt: `b.Skip("CREWLET_BENCH_S3_BUCKET names no bucket, so none is measured")`,
		},
		{
			name: "a comment naming one",
			stmt: "// set CREWLET_BENCH_S3_BUCKET to measure a real bucket",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := "package probe\n\nfunc probe() {\n\t" + tc.stmt + "\n}\n"
			var got []string
			for _, n := range namesIn("probe.go", []byte(src)) {
				got = append(got, n.name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("the scan found %q in\n%s\nwant %q", got, src, tc.want)
			}
		})
	}
}
