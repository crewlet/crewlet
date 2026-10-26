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
// bucket's variables or is what a name is built from: it CONTAINS the
// variables' stem, CREWLET_BENCH_S3, and holds no whitespace. So it is a whole
// name (what os.Getenv or t.Setenv takes, or `NAME=` in a child's
// environment), the stem or the `_`-ended prefix a name is joined to, a
// Sprintf format, a reference expanded from the environment (`${NAME}` or
// `$NAME`, for os.ExpandEnv — or for envref.Resolve, which is how this tree
// reads a config value that points at the environment, so a benchmark
// building an s3obj.Config of references is the likeliest second parser of
// all), or a struct tag a decoder reads the environment by (`env:"NAME"`) —
// each a way a second parser spells one. A sentence that mentions a variable
// (a skip's reason, a refusal) holds a space and does not match.
//
// Matching the stem ANYWHERE in the value rather than whole names is what the
// shape of a copy demands: a matcher of whole names,
// `^CREWLET_BENCH_S3_[A-Z0-9_]*$`, passed `os.Getenv(stem + "_BUCKET")` and
// `fmt.Sprintf("CREWLET_BENCH_S3_%s", k)`, and one anchored at the stem,
// `^CREWLET_BENCH_S3(_\S*)?$`, passed every reference and every tag, whose
// value has the name after a character of its own.
//
// What it still cannot see is two things. The stem SPLIT across literals,
// which no copy of a parser does by accident and nothing short of evaluating
// the program's constants could see. And a value that HOLDS WHITESPACE — not
// only a sentence, which is the point, but a struct tag of several keys or a
// multi-line YAML or shell literal naming a variable, which the scanner cannot
// tell from a sentence without knowing what reads the text.
var benchVariable = regexp.MustCompile(`^\S*CREWLET_BENCH_S3\S*$`)

// benchVariableNeeds is what a value benchVariable matches cannot be spelled
// without — the stem — DERIVED from the pattern (regexlit.Required) rather
// than written beside it, where it would drift from the pattern it stands for.
var benchVariableNeeds = regexlit.Required(benchVariable)

// THE REAL BUCKET IS NAMED IN ONE PLACE: no Go source outside this package
// spells one of the CREWLET_BENCH_S3_* variables' names, or the stem a name
// is built from.
//
// Every benchmark that measures a real bucket — the object store's own, and
// each that measures a path over one — reads the variables through
// s3fake.RealBucket, because a second parser is the copy that quietly reads
// them differently: a region it does not require, a prefix it does not
// default, a path style it reads as false where the first refuses it. Each
// of those passes its own package's benchmarks and points a measurement at a
// bucket nobody meant. A copy is always a few lines that look reasonable, so
// only a scan of the tree finds one.
//
// The parser's own reads are the scan's CONTROL: RealBucket spells the
// bucket's variable, and a walk or a matcher that stopped finding it there
// would find nothing anywhere else either, and pass.
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
// detector, and scanning what this admits — the files that hold the stem and
// the few dozen that hold a numeric escape — under one.
//
// The check reads the file's BYTES and the matcher a literal's unquoted
// value, and the two differ in exactly two ways. An interpreted string's
// escape spells a character its bytes never show: a NUMERIC escape (`\x`,
// `\u`, `\U`, or a backslash before an octal digit) any character at all, a
// simple one only a quote, a backslash or a control character, none of which
// the stem holds (TestTheScanFindsTheStemInEveryUnspacedLiteral holds that).
// And a raw string's value drops every carriage return in it. So a file
// holding the stem nowhere, no numeric escape and no carriage return holds no
// literal whose value contains the stem. An escaped backslash before one of
// those bytes is read as starting an escape too, which admits more than it
// must and never less.
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
// A second parser need not spell a whole name: it can build one from the stem
// (`"CREWLET_BENCH_S3" + "_BUCKET"`, a Sprintf format), set one for a child
// process (`"CREWLET_BENCH_S3_BUCKET=" + …`), point at one with a reference
// it expands (`"${CREWLET_BENCH_S3_BUCKET}"`, through os.ExpandEnv or
// envref.Resolve) or a tag a decoder reads (`env:"CREWLET_BENCH_S3_BUCKET"`),
// or spell a character of one with an escape the file's bytes never show.
// Each case is a file the walk could meet, judged by exactly what the walk
// runs on it — the skip check AND the scanner — so a skip check that stopped
// admitting an escape, or a matcher that stopped at whole names or at a value
// that starts with the stem, turns its case red here, where the walk over
// this tree, which holds none of them, would stay green. What it cannot see
// is benchVariable's to say.
//
// It also holds the two premises mayHoldAName rests on that no case can
// exercise. The pattern still requires a stem: one requiring nothing makes
// the check admit every file, which is correct and gives the walk its six
// seconds back. And no character the stem holds is one a SIMPLE escape
// spells — a quote, a backslash or one of the seven control characters —
// since the check admits a file on a numeric escape only.
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
