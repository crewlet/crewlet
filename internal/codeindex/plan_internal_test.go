package codeindex

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"regexp/syntax"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/regexlit"
	"github.com/crewlet/crewlet/internal/regexlit/regexlittest"
)

// vocabulary is what the property's patterns are written from: words whose
// letters have partners in other cases — non-ASCII pairs and orbits (É and
// é, ẞ and ß, Ω, ω and the Ohm sign, Å, å and the Ångström sign, ǅ's three,
// Cherokee, Georgian, Deseret), the long s and the Kelvin sign, Greek's
// sigmas, whose orbit lowers two ways, and İ, which lowers to an ASCII i it
// does not fold with — and the classes and flags a code search is written
// with, (?s), \s and a negated class among them, so the refusals are met too.
var vocabulary = regexlittest.Vocabulary{
	Flags: []string{"(?i)", "(?i)", "(?i)", "(?m)", "", "(?s)"},
	Words: []string{
		"status", "Session", "kind", "class", "École", "été", "ÉTÉ", "straße", "STRAẞE",
		"σοφία", "ΟΔΥΣΣΕΥΣ", "İstanbul", "ǅemal", "Ωmega", "Ωhm", "Ångström", "Ångst",
		"ᏣᎳᎩ", "ꮳꮃꭹ", "ᲰᲔᲚᲝ", "ჰელო", "𐐀𐐨𐐁", "ſtatus", "Kelvin", "func", "Println", "x := 1", "->",
	},
	Classes: []string{
		"[kK]", "[sſ]", "[éÉ]", "[Σσς]", "[a-c]", `\d`, `\w`, "[^x\n]", ".", "[ΩωΩ]", `\s`, "[^x]",
	},
}

// noise is context a match is planted in, on one line: letters of every case
// the vocabulary's words meet, the escape and the Kelvin sign, a byte that is
// not UTF-8, a carriage return.
func noise(rng *rand.Rand) string {
	pieces := []string{"a", "Z", " ", "\t", ".", "ſ", "K", "\xff", "É", "é", "Σ", "ς", "İ", "ı",
		"Ω", "Ꭰ", "ꭰ", "\r", "_", "STATUS", "class", "x"}
	var b strings.Builder
	for range rng.IntN(4) {
		b.WriteString(pieces[rng.IntN(len(pieces))])
	}
	return b.String()
}

// blobOf is a blob of a few lines of noise, a match of parsed planted on one
// of them half the time and on any line now and then.
func blobOf(rng *rand.Rand, parsed *syntax.Regexp) []byte {
	var b strings.Builder
	lines, plant := 1+rng.IntN(5), -1
	if rng.IntN(2) == 0 {
		plant = rng.IntN(lines)
	}
	for i := range lines {
		b.WriteString(noise(rng))
		if i == plant || rng.IntN(8) == 0 {
			regexlittest.Generate(rng, parsed, &b)
		}
		b.WriteString(noise(rng))
		if i+1 < lines || rng.IntN(2) == 0 {
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

// testOID is an object id that orders as n does.
func testOID(n int) OID {
	var o OID
	binary.BigEndian.PutUint32(o[oidBytes-4:], uint32(n))
	return o
}

// segmentOf is the segment of texts, the i-th as testOID(first+i*step).
func segmentOf(t *testing.T, texts [][]byte, first, step int) *Segment {
	t.Helper()
	var b Builder
	for i, text := range texts {
		if err := b.Add(testOID(first+i*step), text); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := WriteSegment(&out, &b); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSegment(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// linesMatched is every line of blob re matches, each line alone, by regexp
// alone: what Confirm must answer.
func linesMatched(re *regexp.Regexp, blob []byte) []Hit {
	if len(blob) == 0 {
		return nil
	}
	lines := bytes.Split(blob, []byte("\n"))
	if blob[len(blob)-1] == '\n' {
		lines = lines[:len(lines)-1]
	}
	var out []Hit
	for i, line := range lines {
		if loc := re.FindIndex(line); loc != nil {
			out = append(out, Hit{Line: i + 1, Text: line, Start: loc[0], End: loc[1]})
		}
	}
	return out
}

func sameHits(a, b []Hit) bool {
	return slices.EqualFunc(a, b, func(x, y Hit) bool {
		return x.Line == y.Line && bytes.Equal(x.Text, y.Text) && x.Start == y.Start && x.End == y.End
	})
}

// confirmed is every hit Confirm finds in blob, read whole: no line of it past
// the plan's bound, and nothing to end it.
func confirmed(t *testing.T, p *Plan, blob []byte) []Hit {
	t.Helper()
	var hits []Hit
	long, err := p.Confirm(t.Context(), blob, func(h Hit) bool {
		hits = append(hits, h)
		return true
	})
	if err != nil || long != 0 {
		t.Fatalf("Confirm of %s over %q stopped (%v) or left %d lines unrun", p.re, blob, err, long)
	}
	return hits
}

// trigramSet is the trigrams of lowered, as a set.
func trigramSet(lowered []byte) map[Trigram]bool {
	set := map[Trigram]bool{}
	for _, t := range appendTrigrams(nil, lowered) {
		set[t] = true
	}
	return set
}

// holds reports whether a blob of the trigrams set holds some literal of c:
// every trigram of the literal's text, taken from the text again rather than
// from the parts a plan keeps of it, so a split that lost one is caught here.
func holds(c clause, set map[Trigram]bool) bool {
	return slices.ContainsFunc(c.alts, func(a alternative) bool {
		return !slices.ContainsFunc(appendTrigrams(nil, []byte(a.text)), func(t Trigram) bool { return !set[t] })
	})
}

// asciiLower is a lowering of ASCII letters alone: what a cheaper index
// would take its trigrams over.
func asciiLower(b []byte) []byte {
	out := bytes.Clone(b)
	for i, c := range out {
		if 'A' <= c && c <= 'Z' {
			out[i] = c + 'a' - 'A'
		}
	}
	return out
}

// A PLAN NEVER REJECTS A BLOB GO'S REGEXP FINDS A MATCH IN.
//
// The plan's whole promise: a blob holding a match is a candidate, so a
// search through the index answers what reading every file with the same
// expression would. Held first on the cases each half of the index's
// comparison exists for — a non-ASCII letter (?i) folds that only a Unicode
// lowering meets, and the long s no lowering sees through, which the escape
// flag admits — and then over patterns GENERATED from the vocabulary above
// and blobs holding matches GENERATED from each pattern's own syntax tree
// (regexlittest), each blob a segment's, every one Go's regexp matches
// required among the plan's candidates, and every blob's lines confirmed
// exactly as regexp finds them line by line. Each eighth pattern's blobs are
// also split across two segments and merged, and the merge must answer what
// the whole did.
//
// A FIXED SEED, so a failure reproduces exactly; its breadth is the count,
// and the counts it reports say the property was met where it can fail: a
// match only a Unicode lowering finds, and one only the escape flag admits —
// the two an ASCII-only lowering and a dropped flag would lose.
func TestAPlanNeverRejectsAMatchGoRegexpFinds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ pattern, blob string }{
		{`(?i)été`, "un ÉTÉ chaud\n"},
		{`(?i)straße`, "STRAẞE\n"},
		{`(?i)Ωmega`, "ΩMEGA"},
		{`(?i)ꮳꮃꭹ`, "ᏣᎳᎩ\n"},
		{`École`, "l'École\n"},
		{`(?i)status`, "ſtatus: ok\n"},
		{`(?i)sessions`, "the ſeſſionS\n"},
		{`(?i)kelvin`, "KELVIN\n"},
	} {
		re := regexp.MustCompile(tc.pattern)
		plan, err := NewPlan(re)
		if err != nil {
			t.Fatalf("NewPlan(%s): %v", tc.pattern, err)
		}
		s := segmentOf(t, [][]byte{[]byte(tc.blob), []byte("nothing to see here\n")}, 1, 1)
		got, err := plan.Candidates(s)
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(tc.blob) {
			t.Fatalf("the case is wrong: %s does not match %q", tc.pattern, tc.blob)
		}
		if !slices.Contains(got, testOID(1)) {
			t.Errorf("the plan of %s rules out %q, which the pattern matches", tc.pattern, tc.blob)
		}
		if slices.Contains(got, testOID(2)) {
			t.Errorf("the plan of %s admits a blob holding none of its literal, so the cases above prove "+
				"nothing", tc.pattern)
		}
	}

	rng := rand.New(rand.NewPCG(20261026, 4))
	var planned, unlocal, unnarrowed, blobs, skipped, matched, ruledOut, merged int
	var unicodeOnly, escapeOnly, hits int
	for n := range 1500 {
		pattern := vocabulary.Pattern(rng, 2)
		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Fatalf("the generator wrote %q, which does not compile: %v", pattern, err)
		}
		plan, err := NewPlan(re)
		switch {
		case errors.Is(err, ErrNotLineLocal):
			unlocal++
			continue
		case errors.Is(err, ErrNoLiteral):
			unnarrowed++
			continue
		case err != nil:
			t.Fatalf("NewPlan(%s): %v", pattern, err)
		}
		planned++
		parsed, err := syntax.Parse(pattern, syntax.Perl)
		if err != nil {
			t.Fatalf("parse %q: %v", pattern, err)
		}
		texts := make([][]byte, 12)
		for i := range texts {
			texts[i] = blobOf(rng, parsed)
		}
		s := segmentOf(t, texts, 1, 1)
		got, err := plan.Candidates(s)
		if err != nil {
			t.Fatalf("Candidates of %s: %v", pattern, err)
		}
		candidate := map[OID]bool{}
		for _, oid := range got {
			candidate[oid] = true
		}
		for i, text := range texts {
			blobs++
			want := linesMatched(re, text)
			if re.Match(text) != (len(want) > 0) {
				t.Fatalf("%s matches %q as a whole %v, and line by line %v: a plan admits only a pattern "+
					"whose matches lie within a line", pattern, text, re.Match(text), len(want) > 0)
			}
			if Skip(int64(len(text)), text) {
				// Never searched, by either path; never a candidate.
				skipped++
				if candidate[testOID(1+i)] {
					t.Fatalf("the plan of %s admits the skipped blob %q", pattern, text)
				}
				continue
			}
			if got := confirmed(t, plan, text); !sameHits(got, want) {
				t.Fatalf("Confirm of %s over %q answers %+v; regexp line by line finds %+v", pattern, text,
					got, want)
			}
			hits += len(want)
			if len(want) == 0 {
				if !candidate[testOID(1+i)] {
					ruledOut++
				}
				continue
			}
			matched++
			if !candidate[testOID(1+i)] {
				t.Fatalf("the plan of %s rules out %q, which the pattern matches (%v)", pattern, text,
					plan.need)
			}
			// What this blob's candidacy rested on: a folded clause only
			// the Unicode lowering holds — a folded literal is lowered by
			// regexlit whatever this package lowers a blob with — or one
			// only the escape admits.
			lowered, ascii := trigramSet(lower(text)), trigramSet(asciiLower(text))
			if !regexlit.HasEscape(text) && slices.ContainsFunc(plan.index, func(c clause) bool {
				return c.folded && holds(c, lowered) && !holds(c, ascii)
			}) {
				unicodeOnly++
			}
			if slices.ContainsFunc(plan.index, func(c clause) bool { return c.folded && !holds(c, lowered) }) {
				if !regexlit.HasEscape(text) {
					t.Fatalf("the plan of %s admits %q on a folded clause it does not hold, and the blob "+
						"holds no escape", pattern, text)
				}
				escapeOnly++
			}
		}
		if n%8 != 0 {
			continue
		}
		// THE SAME ANSWER FROM TWO SEGMENTS MERGED: the even blobs in
		// one, the odd in the other.
		var even, odd [][]byte
		for i, text := range texts {
			if i%2 == 0 {
				even = append(even, text)
			} else {
				odd = append(odd, text)
			}
		}
		var out bytes.Buffer
		if err := MergeSegments(&out, []*Segment{segmentOf(t, even, 1, 2), segmentOf(t, odd, 2, 2)}, nil); err != nil {
			t.Fatal(err)
		}
		m, err := OpenSegment(out.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		again, err := plan.Candidates(m)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(again, got) {
			t.Fatalf("the plan of %s over the merged segment answers %v, and over the whole %v", pattern,
				again, got)
		}
		merged++
	}
	// THE PROPERTY WAS MET WHERE IT CAN FAIL, or a plan that admitted every
	// blob, or a generator that stopped writing the hard letters, would pass.
	switch {
	case planned < 400 || matched < planned*2:
		t.Fatalf("%d of 1500 patterns planned and %d blobs matched: the property was hardly exercised",
			planned, matched)
	case ruledOut < planned:
		t.Fatalf("only %d blobs ruled out over %d plans: a plan that admits everything passes the property "+
			"without being one", ruledOut, planned)
	case unicodeOnly < 20 || escapeOnly < 20:
		t.Fatalf("%d matches only a Unicode lowering finds and %d only the escape admits: the property never "+
			"met what an ASCII-only lowering or a dropped flag would lose", unicodeOnly, escapeOnly)
	case unlocal == 0 || unnarrowed == 0:
		t.Fatalf("%d patterns refused as crossing lines and %d as unnarrowed: a refusal was never met",
			unlocal, unnarrowed)
	}
	t.Logf("%d patterns planned (%d refused crossing lines, %d with no literal to look up); %d blobs: %d "+
		"matched, all candidates, %d of the rest ruled out, %d skipped; %d lines confirmed; %d matches only a "+
		"Unicode lowering finds, %d only the escape admits; %d merges answered alike", planned, unlocal,
		unnarrowed, blobs, matched, ruledOut, skipped, hits, unicodeOnly, escapeOnly, merged)
}

// A PLAN REFUSES WHAT THE INDEX CANNOT NARROW, what a line cannot hold, and
// what costs too much to run on one.
//
// The last is a program past MaxProgram instructions: a counted repetition
// compiles to as many copies of what it repeats, and the 398 bytes of "the",
// forty-nine .{0,999} and "zzz" — within MaxQueryBytes, line-local, holding a
// literal — compile to nearly a hundred thousand, two milliseconds a byte of
// a line holding no match. foo.{0,500}bar, just within the bound, plans.
func TestAPlanRefusesWhatTheIndexCannotNarrow(t *testing.T) {
	t.Parallel()
	multiplied := "the" + strings.Repeat(".{0,999}", 49) + "zzz"
	for pattern, want := range map[string]error{
		`foo`:            nil,
		`(?m)^func`:      nil,
		`(?i)ſtatus`:     nil,
		`foo|barbaz`:     nil,
		`foo.{0,500}bar`: nil,
		`ab`:             ErrNoLiteral,
		`a.b`:            ErrNoLiteral,
		`\w+`:            ErrNoLiteral,
		`(?i)ab`:         ErrNoLiteral,
		`fo|barbaz`:      ErrNoLiteral, // its one clause holds a literal no trigram finds
		`\x{212A}x`:      ErrNoLiteral, // four bytes, two once lowered
		`foo\nbar`:       ErrNotLineLocal,
		`(?s)foo.bar`:    ErrNotLineLocal,
		`\Afoo`:          ErrNotLineLocal,
		`foo\s+bar`:      ErrNotLineLocal,
		`^foo`:           ErrNotLineLocal, // the text's start, without (?m)
		`foo.{0,600}bar`: ErrTooComplex,
		multiplied:       ErrTooComplex,
	} {
		_, err := NewPlan(regexp.MustCompile(pattern))
		switch {
		case want == nil && err != nil:
			t.Errorf("NewPlan(%s) refuses it: %v", pattern, err)
		case want != nil && !errors.Is(err, want):
			t.Errorf("NewPlan(%s) = %v, want %v", pattern, err, want)
		case want != nil && !errors.Is(err, ErrBadQuery):
			t.Errorf("NewPlan(%s)'s refusal %v is not ErrBadQuery", pattern, err)
		}
	}

	// The sentence says how large the program is and what to write instead.
	insts, err := program(regexp.MustCompile(multiplied))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Query{Text: multiplied, Mode: ModeRegex}.Plan()
	var qe *QueryError
	switch {
	case insts < 50*MaxProgram:
		t.Errorf("the forty-nine repetitions compile to %d instructions, so the case proves little", insts)
	case !errors.As(err, &qe) || !errors.Is(err, ErrTooComplex):
		t.Errorf("a query of the forty-nine repetitions plans: %v", err)
	case !strings.Contains(qe.Reason, fmt.Sprintf("%d instructions", insts)) || !strings.Contains(qe.Reason, ".*"):
		t.Errorf("the refusal %q names neither the program's %d instructions nor what to write instead",
			qe.Reason, insts)
	}
}

// A CLAUSE OF MANY LITERALS UNIONS THEIR BLOBS ONCE. An alternation of
// literals is one clause, which a blob holding any of them satisfies, and a
// query of MaxQueryBytes can name a hundred; their blobs are merged as one
// union, whose work is the blobs admitted times ⌈log₂ k⌉ and whose buffers
// are two, never folded in one literal at a time into a fresh slice each,
// which copied what was admitted so far once for each literal after it. A
// clause of 64 words over a segment whose 2 048 blobs each hold one of them
// allocates what a clause of 2 words does over a segment whose 2 048 blobs
// each hold one of those. The words share no trigram, so what each literal's
// own lookup costs is the blobs it admits, whichever clause it is in.
func TestAClauseOfManyLiteralsUnionsTheirBlobsOnce(t *testing.T) {
	// Not parallel: runtime.MemStats counts every goroutine's allocations,
	// and a parallel case's would be counted as the plan's.
	const blobs = 2048
	rng := rand.New(rand.NewPCG(20261027, 5))
	candidates := func(words int) uint64 {
		alts := make([]string, words)
		texts := make([][]byte, blobs)
		seen := map[Trigram]bool{}
		for i := range alts {
			// Neighbours differ in their first letter, or Go's parser
			// factors their common prefix out of the alternation, and the
			// plan is of the prefix and the rest.
			word := []byte{"pq"[i%2], 0, 0, 0, 0, 0, 0, 0}
			for again := true; again; {
				for j := 1; j < len(word); j++ {
					word[j] = byte('a' + rng.IntN(26))
				}
				again = slices.ContainsFunc(Trigrams(word), func(t Trigram) bool { return seen[t] })
			}
			for _, t := range Trigrams(word) {
				seen[t] = true
			}
			alts[i] = string(word)
		}
		for i := range texts {
			texts[i] = []byte("x := " + alts[i%words] + "\n")
		}
		plan, err := NewPlan(regexp.MustCompile(strings.Join(alts, "|")))
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.index) != 1 || len(plan.index[0].alts) != words {
			t.Fatalf("the plan of %d words is %d clauses, the first of %d literals; want one of %d", words,
				len(plan.index), len(plan.index[0].alts), words)
		}
		s := segmentOf(t, texts, 1, 1)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got, err := plan.Candidates(s)
		runtime.ReadMemStats(&after)
		if err != nil || len(got) != blobs {
			t.Fatalf("a clause of %d words admits %d of the %d blobs holding one (%v)", words, len(got), blobs, err)
		}
		return after.TotalAlloc - before.TotalAlloc
	}
	two, many := candidates(2), candidates(64)
	t.Logf("the candidates of %d blobs allocated %d bytes for a clause of 2 words and %d for one of 64", blobs, two,
		many)
	if many > two+two/2 {
		t.Errorf("a clause of 64 words allocated %d bytes for its candidates and one of 2 words %d, over as many "+
			"blobs: what a clause allocates grows with its literals", many, two)
	}
}

// CONFIRM STOPS BETWEEN LINES ONCE ITS CONTEXT ENDS, and says so. A run of
// Go's regexp over a line cannot be stopped part way, so a search holds its
// budget to within one line's run by checking between them: once the
// context ends, no further line is run, a matching one included, and the
// caller is told why the answer ends there. A hit refused stops it with
// nothing to tell.
func TestAConfirmStopsBetweenLinesOnceItsContextEnds(t *testing.T) {
	t.Parallel()
	plan, err := NewPlan(regexp.MustCompile(`needle`))
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte(strings.Repeat("a needle\n", 3) + strings.Repeat("hay\n", 1000) + "the last needle\n")
	if hits := confirmed(t, plan, blob); len(hits) != 4 || hits[3].Line != 1004 {
		t.Fatalf("the blob read whole answers %+v, want lines 1–3 and 1004", hits)
	}

	ctx, cancel := context.WithCancel(t.Context())
	var lines []int
	long, err := plan.Confirm(ctx, blob, func(h Hit) bool {
		lines = append(lines, h.Line)
		if len(lines) == 3 {
			cancel()
		}
		return true
	})
	if !errors.Is(err, context.Canceled) || !slices.Equal(lines, []int{1, 2, 3}) || long != 0 {
		t.Errorf("a context ended at the third hit: lines %v, %d long, %v; want lines 1–3 and the context's "+
			"error", lines, long, err)
	}
	lines = nil
	if _, err := plan.Confirm(ctx, blob, func(h Hit) bool { lines = append(lines, h.Line); return true }); !errors.Is(err, context.Canceled) || len(lines) != 0 {
		t.Errorf("a context already ended: lines %v, %v; want none and the context's error", lines, err)
	}

	lines = nil
	if _, err := plan.Confirm(t.Context(), blob, func(h Hit) bool {
		lines = append(lines, h.Line)
		return false
	}); err != nil || !slices.Equal(lines, []int{1}) {
		t.Errorf("a hit refused at the first: lines %v, %v; want line 1 alone and no error", lines, err)
	}
}

// A LINE LONGER THAN ITS PLAN RUNS ON IS COUNTED, NEVER RUN: what confirming
// a line costs is its bytes times its program's instructions (MaxLineWork),
// and a run cannot be stopped part way. Only a line that could match is
// counted — one that cannot holds no match whatever its length — so the
// count is of what the search did not read. The bound is the plan's own: a
// short program runs on a whole MaxFileBytes line, the longest a blob the
// index holds can have.
func TestALineLongerThanItsPlanRunsOnIsCountedNotRun(t *testing.T) {
	t.Parallel()
	plan, err := NewPlan(regexp.MustCompile(`foo.{0,500}bar`))
	if err != nil {
		t.Fatal(err)
	}
	limit := plan.LineBytes()
	if limit != MaxLineWork/plan.insts || plan.insts < MaxProgram/2 {
		t.Fatalf("a program of %d instructions runs on lines of %d bytes; the case needs one near MaxProgram",
			plan.insts, limit)
	}
	line := func(n int, holds string) string { return holds + strings.Repeat("y", n-len(holds)) }
	blob := line(limit, "foo bar") + "\n" + line(limit+1, "foo bar") + "\n" + line(limit+1, "foo") + "\nfoo bar\n"
	var lines []int
	long, err := plan.Confirm(t.Context(), []byte(blob), func(h Hit) bool {
		lines = append(lines, h.Line)
		return true
	})
	if err != nil || !slices.Equal(lines, []int{1, 4}) || long != 1 {
		t.Errorf("lines of %d, %d (could match), %d (cannot) and 7 bytes against a bound of %d: hits on %v, %d "+
			"counted, %v; want hits on 1 and 4 and line 2 counted", limit, limit+1, limit+1, limit, lines, long, err)
	}

	short, err := NewPlan(regexp.MustCompile(`needle`))
	if err != nil {
		t.Fatal(err)
	}
	whole := []byte(strings.Repeat("y", MaxFileBytes-len("needle")) + "needle")
	if hits := confirmed(t, short, whole); short.LineBytes() < MaxFileBytes || len(hits) != 1 {
		t.Errorf("a program of %d instructions runs on lines of %d bytes and finds %d hits on a whole "+
			"MaxFileBytes line; want the line run and one hit", short.insts, short.LineBytes(), len(hits))
	}
}

// BenchmarkALineAtTheWorkBound is MaxLineWork's measurement: one line's
// confirm at the bound, by a regexp holding every instruction of its program
// live at every byte — [a-z]{3} starts a thread at each letter and .{0,k}
// keeps one at each count — over a line holding the literal its plan needs
// and no match. program=1024 is MaxProgram's program over its LineBytes;
// program=24 a short one over a whole MaxFileBytes line, which its bound
// passes.
func BenchmarkALineAtTheWorkBound(b *testing.B) {
	for _, k := range []int{508, 8} {
		plan, err := NewPlan(regexp.MustCompile(fmt.Sprintf(`[a-z]{3}.{0,%d}zzz`, k)))
		if err != nil {
			b.Fatal(err)
		}
		n := min(plan.LineBytes(), MaxFileBytes)
		line := append([]byte("zzz"), bytes.Repeat([]byte("ab"), n/2)...)[:n]
		b.Run(fmt.Sprintf("program=%d", plan.insts), func(b *testing.B) {
			for b.Loop() {
				hits := 0
				long, err := plan.Confirm(b.Context(), line, func(Hit) bool { hits++; return true })
				if err != nil || long != 0 || hits != 0 {
					b.Fatalf("the line was not run to its end: %d hits, %d long, %v", hits, long, err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(plan.insts)/float64(n),
				"ns/inst-byte")
		})
	}
}
