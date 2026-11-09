package kept_test

import (
	"encoding/json"
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/plainjson"
)

const run kept.Record = "the run's record"

// LEN IS THE ENCODER'S ESCAPE, BYTE FOR BYTE, AND NEVER SHORT OF IT. Every
// bound this package keeps a payload within is in what json.Marshal writes, so
// a byte class it counts short is a record that grows past its ceiling while
// every check here says it fits: every ASCII byte, the two separators
// JavaScript cannot hold, multi-byte runes and random strings of all of them
// are counted exactly, and a string holding invalid bytes (which the encoder
// writes in three bytes or in six, by context) is counted at least what the
// encoder writes.
//
// Mutation: count `<` as one byte, `\b` as six, or an invalid byte as one,
// and this fails naming the string.
func TestLenIsTheEncodersEscapeByteForByte(t *testing.T) {
	t.Parallel()
	var corpus []string
	for b := range 0x80 {
		corpus = append(corpus, string(rune(b)), "a"+string(rune(b))+"z")
	}
	corpus = append(corpus, "\xff", "a\xc3", "\xe2\x80", "\u2028", "\u2029", "é", "日本", "😀",
		"\u007f", strings.Repeat("<&>", 100))
	rng := rand.New(rand.NewPCG(1, 2))
	pool := []string{"a", "\"", "\\", "\n", "\x01", "\b", "\f", "<", "&", "\u2028", "é", "😀", "\xff", "\x7f"}
	for range 500 {
		var b strings.Builder
		for range rng.IntN(40) {
			b.WriteString(pool[rng.IntN(len(pool))])
		}
		corpus = append(corpus, b.String())
	}
	for _, s := range corpus {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		got, want := kept.Len(s), len(raw)-2
		switch {
		case utf8.ValidString(s) && got != want:
			t.Errorf("Len(%q) = %d, json.Marshal writes %d", s, got, want)
		case got < want:
			t.Errorf("Len(%q) = %d, short of the %d json.Marshal writes", s, got, want)
		}
		if quoted := kept.Quoted(s); quoted != got+2 {
			t.Errorf("Quoted(%q) = %d, want Len and its quotes, %d", s, quoted, got+2)
		}
	}
}

// A TEXT PAST ITS BUDGET KEEPS AN OPENING THAT SAYS IT IS ONE. What a record
// keeps of a long output fits its budget as the record holds it — however the
// text escapes — opens with the text's own opening, ends with how much more of
// how much was not kept, and states the whole's size for a reader that decides
// by it; a text that fits is kept as it is and states none.
//
// Mutation: measure the opening by its raw length rather than escaped, and the
// markup-heavy case overruns its budget; drop the marker, and the cut text
// reads as whole.
func TestATextPastItsBudgetKeepsAnOpeningThatSaysSo(t *testing.T) {
	t.Parallel()
	const budget = 4 << 10
	for name, text := range map[string]string{
		"prose":  strings.Repeat("the build passed. ", 64<<10/18),
		"markup": strings.Repeat("<li>&amp;</li>\n", 64<<10/15),
		"bytes":  strings.Repeat("\x01\xff", 32<<10),
		"runes":  strings.Repeat("日本語😀", 64<<10/13),
	} {
		got, whole := run.Text(text, budget)
		if kept.Quoted(got) > budget {
			t.Errorf("%s: kept %d bytes as the record holds them, over its %d", name, kept.Quoted(got), budget)
		}
		if whole != len(text) {
			t.Errorf("%s: states a whole of %d, want %d", name, whole, len(text))
		}
		marker := strings.LastIndex(got, "…\n(")
		if marker < 0 || !strings.HasSuffix(got, " not kept on the run's record)") ||
			!strings.Contains(got[marker:], " of "+kept.Size(len(text))) {
			t.Errorf("%s: the cut does not say what it left out: %q", name, got[max(0, len(got)-120):])
			continue
		}
		opening := got[:marker]
		if !strings.HasPrefix(text, opening) || len(opening) == 0 {
			t.Errorf("%s: the kept opening is not the text's own: %q", name, opening[:min(40, len(opening))])
		}
		// CUT ON A RUNE BOUNDARY: a valid text keeps a valid opening.
		if utf8.ValidString(text) && !utf8.ValidString(opening) {
			t.Errorf("%s: the opening was cut inside a rune: %q", name, opening[max(0, len(opening)-8):])
		}
	}
	short := "ok"
	if got, whole := run.Text(short, budget); got != short || whole != 0 {
		t.Errorf("a text that fits came back %q stating %d", got, whole)
	}
}

// commit is a code_commit_files call's arguments as the engine records them:
// identifiers beside a body far past any record's budget.
func commit(t *testing.T, content string) string {
	t.Helper()
	raw, err := plainjson.Marshal(map[string]any{
		"repo": "acme/api", "branch": "crewlet/eng/fix", "message": "fix the flake",
		"files": []any{map[string]any{"path": "internal/a.go", "content": content}},
		"issue": json.Number("12345678901234567890"),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A CALL'S ARGUMENTS LEAVE OUT THEIR BODIES AND KEEP WHAT NAMES THEM. A one-MiB
// file in a commit's arguments is replaced by a marker naming its size and its
// digest, and every identifier beside it — the repository, the branch, the
// file's path, a twenty-digit id as the digits it was written in — stays whole,
// in the same shape, within the budget as the record holds it.
//
// Mutation: leave out containers before leaves, and the file's path goes with
// its content; key the marker's digest on nothing, and two different bodies
// read as one.
func TestACallsArgumentsLeaveOutTheirBodiesAndKeepWhatNamesThem(t *testing.T) {
	t.Parallel()
	const budget = 8 << 10
	text := commit(t, strings.Repeat("<x>", 1<<20/3))
	got, whole := run.Args(text, budget)
	if whole != len(text) {
		t.Fatalf("states a whole of %d, want %d", whole, len(text))
	}
	if kept.Quoted(got) > budget {
		t.Fatalf("kept %d bytes as the record holds them, over its %d", kept.Quoted(got), budget)
	}
	var tree map[string]any
	dec := json.NewDecoder(strings.NewReader(got))
	dec.UseNumber()
	if err := dec.Decode(&tree); err != nil {
		t.Fatalf("what is kept is not the call's arguments: %v: %q", err, got)
	}
	files, _ := tree["files"].([]any)
	file, _ := firstOf(files).(map[string]any)
	if tree["repo"] != "acme/api" || tree["branch"] != "crewlet/eng/fix" ||
		tree["issue"] != json.Number("12345678901234567890") || file["path"] != "internal/a.go" {
		t.Errorf("an identifier was not kept whole: %q", got)
	}
	content, _ := file["content"].(string)
	if !strings.HasPrefix(content, "(1.0 MiB not kept on the run's record, digest ") {
		t.Errorf("the body is not said to be left out: %q", content)
	}

	other, _ := run.Args(commit(t, strings.Repeat("<y>", 1<<20/3)), budget)
	if other == got {
		t.Error("two different bodies were left out as the same marker")
	}
	again, _ := run.Args(text, budget)
	if again != got {
		t.Errorf("the same arguments kept two forms:\n%q\n%q", got, again)
	}
}

func firstOf(list []any) any {
	if len(list) == 0 {
		return nil
	}
	return list[0]
}

// ARGUMENTS THAT FIT ARE KEPT BYTE FOR BYTE, and state no whole: a reader takes
// a call with no size beside it for the call as it was made.
//
// Mutation: re-encode what fits, and a text the engine did not write in its own
// spelling changes.
func TestArgumentsThatFitAreKeptByteForByte(t *testing.T) {
	t.Parallel()
	text := `{"channel":"C1","text":"a < b && c > d"}`
	if got, whole := run.Args(text, 8<<10); got != text || whole != 0 {
		t.Errorf("Args = %q stating %d, want the text as it was", got, whole)
	}
}

// ARGUMENTS ARE MEASURED AS THE ENGINE SPELLS THEM, whoever spelt the text. A
// text written by json.Marshal spells every `<` as six bytes the engine's own
// encoder writes as one, so weighed as written, a body left out still left the
// text over its budget, and the container holding the file's path went too. And
// a text that fits only once spelt as the engine spells it is the same
// arguments, kept whole.
//
// Mutation: take the total off the text as written, and the file's path is
// left out with the container that held it.
func TestArgumentsAreMeasuredAsTheEngineSpellsThem(t *testing.T) {
	t.Parallel()
	escaped, err := json.Marshal(map[string]any{"repo": "acme/api",
		"files": []any{map[string]any{"path": "internal/a.go", "content": strings.Repeat("<", 1<<20)}}})
	if err != nil {
		t.Fatal(err)
	}
	got, whole := run.Args(string(escaped), 8<<10)
	if whole != len(escaped) || !strings.Contains(got, `"path":"internal/a.go"`) ||
		!strings.Contains(got, "1.0 MiB not kept on the run's record") {
		t.Errorf("Args kept %q stating %d; want the path beside the left-out content", got, whole)
	}
	small, err := json.Marshal(map[string]any{"text": strings.Repeat("<", 1200)})
	if err != nil {
		t.Fatal(err)
	}
	got, whole = run.Args(string(small), 8<<10)
	if whole != 0 || !strings.HasPrefix(got, `{"text":"<<<`) {
		t.Errorf("arguments that fit as the engine spells them kept %.40q stating %d, want them whole", got, whole)
	}
}

// MANY SMALL VALUES GIVE WAY AS A CONTAINER, AND A SHAPE THAT CANNOT FIT AS A
// WHOLE. Ten thousand ids are no body to leave out, so the list goes as one;
// ten thousand keys at the top hold no container to give up, and a text that is
// no JSON object has no values at all — both keep only the marker of the whole,
// in an object, within the budget.
//
// Mutation: stop after the leaves, and the long list stays over its budget.
func TestManySmallValuesGiveWayAsAContainerAndAShapeAsAWhole(t *testing.T) {
	t.Parallel()
	const budget = 8 << 10
	ids := make([]any, 10_000)
	for i := range ids {
		ids[i] = fmt.Sprintf("id-%05d", i)
	}
	listed, err := plainjson.Marshal(map[string]any{"ids": ids, "project": "acme"}, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := run.Args(string(listed), budget)
	var tree map[string]any
	if err := json.Unmarshal([]byte(got), &tree); err != nil || tree["project"] != "acme" {
		t.Fatalf("the list's neighbour was not kept: %v: %q", err, got)
	}
	if s, _ := tree["ids"].(string); !strings.Contains(s, "not kept on the run's record") {
		t.Errorf("the list is not said to be left out: %q", got)
	}

	keys := map[string]any{}
	for i := range 10_000 {
		keys[fmt.Sprintf("k%05d", i)] = i
	}
	wide, err := plainjson.Marshal(keys, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"ten thousand keys": string(wide), "not an object": "[1,2,3" +
		strings.Repeat(",1", 10_000) + "]"} {
		got, whole := run.Args(text, budget)
		if whole != len(text) || kept.Quoted(got) > budget {
			t.Errorf("%s: kept %d bytes stating %d", name, kept.Quoted(got), whole)
		}
		var only map[string]string
		if err := json.Unmarshal([]byte(got), &only); err != nil || len(only) != 1 ||
			!strings.Contains(only[kept.NotKeptKey], "not kept on the run's record") {
			t.Errorf("%s: kept %q, want the marker of the whole", name, got)
		}
	}
}

// A SIZE PAST A BOUND READS PAST IT, AND A SIZE WITHIN ONE NEVER DOES. A
// refusal states what was refused in SizeUp beside the bound in Size, and Size
// rounds whole KiB down: stated in Size, a reply one byte past an answer's
// 16 KiB read "16 KiB, and an answer may be at most 16 KiB". So every size
// from a byte short of each bound a refusal states to a few past it reads
// against that bound as it compares, a figure is never more than one unit
// past what it states, and one that rounds up to a whole MiB is said in MiB.
//
// Mutation: round SizeUp's KiB down, and the byte past a bound reads as the
// bound; drop its MiB clause's lower edge to 1 MiB, and a size a few bytes
// short of one reads "1024 KiB".
func TestASizePastItsBoundReadsPastIt(t *testing.T) {
	t.Parallel()
	value := func(s string) float64 {
		t.Helper()
		number, unit, ok := strings.Cut(s, " ")
		f, err := strconv.ParseFloat(number, 64)
		if !ok || err != nil {
			t.Fatalf("%q is not a number and a unit", s)
		}
		switch unit {
		case "MiB":
			return f * (1 << 20)
		case "KiB":
			return f * (1 << 10)
		case "bytes":
			return f
		}
		t.Fatalf("%q states no unit this package writes", s)
		return 0
	}
	for _, bound := range []int{1 << 10, 16 << 10, 512 << 10, 1 << 20, 8 << 20} {
		stated := value(kept.Size(bound))
		for n := bound - 2; n <= bound+2; n++ {
			got := value(kept.SizeUp(n))
			if past := n > bound; past != (got > stated) {
				t.Errorf("%d bytes against a bound of %s reads %s: past=%v, want %v",
					n, kept.Size(bound), kept.SizeUp(n), got > stated, past)
			}
			unit := float64(1 << 10)
			if n > 1<<20-1<<10 {
				unit = 0.1 * (1 << 20)
			}
			if got < float64(n) || got-float64(n) >= unit {
				t.Errorf("%d bytes reads %s, which is not that size rounded up to a unit",
					n, kept.SizeUp(n))
			}
		}
	}
	for n, want := range map[int]string{
		1000: "1000 bytes", 1 << 10: "1 KiB", 16<<10 + 1: "17 KiB",
		1<<20 - 1<<10: "1023 KiB", 1<<20 - 1: "1.0 MiB", 1<<20 + 1: "1.1 MiB",
	} {
		if got := kept.SizeUp(n); got != want {
			t.Errorf("SizeUp(%d) = %q, want %q", n, got, want)
		}
	}
}

// THIS PACKAGE IMPORTS THE STANDARD LIBRARY AND THE ENGINE'S TEXT ENCODER ONLY.
// It is a leaf because the writer of a run's row and the condensation of a
// parked turn both read its rule from heights neither can import the other
// from; the first engine package admitted here is the import that stops one of
// them using it.
//
// Mutation: import internal/textcut here, and this fails naming it.
func TestThePackageImportsOnlyTheStandardLibraryAndPlainjson(t *testing.T) {
	t.Parallel()
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("listing the package's files: %v", err)
	}
	fset := token.NewFileSet()
	files := 0
	for _, list := range [][]string{pkg.GoFiles, pkg.TestGoFiles, pkg.XTestGoFiles} {
		for _, name := range list {
			files++
			parsed, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parsing %s: %v", name, err)
			}
			for _, spec := range parsed.Imports {
				path, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				switch {
				case path == "github.com/crewlet/crewlet/internal/plainjson":
				case path == "github.com/crewlet/crewlet/internal/kept" && strings.HasSuffix(name, "_test.go"):
				case strings.Contains(strings.Split(path, "/")[0], "."):
					t.Errorf("%s imports %q, which is neither the standard library nor plainjson", name, path)
				}
			}
		}
	}
	if len(pkg.GoFiles) == 0 || files < 2 {
		t.Fatalf("walked %d files; the walk is not reading the package", files)
	}
}
