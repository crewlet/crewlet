package textindex

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// finderTexts are the texts a Finder is held to FindIn over: the package's find cases, the JSON texts its
// stretch test reads, and the edges writes add: a word whose term is cut inside a rune, at MaxTermLength and
// a byte past it; letters whose lower case is shorter or longer than they are; invalid UTF-8 and a rune the
// text ends inside; a long word written in escapes; and an escape the text ends inside, at every byte.
func finderTexts() []string {
	u := escapedUnit
	long := strings.Repeat("x", MaxTermLength+6)
	kelvin, dotted, stroked := string(rune(0x212A)), string(rune(0x130)), string(rune(0x23A))
	texts := []string{
		"deploy the pipeline", "redeployment done", "We DEPLOY now", "work_items-deploy", "deploy only",
		"über deploy", "alpha beta gamma", "see " + long + " here", "a b deploy", "ship it, then ship more",
		"alpha [cut] beta gamma",
		`{"command":"cd repo\ngit push origin main"}`, `{"content":"func main() {\n\tfmt.Println(1)\n}"}`,
		`{"say":"\"quoted\" words"}`, `{"path":"C:\\new\\test"}`, `{"name":"caf` + u('é') + ` au lait"}`,
		`{"x":"go` + u(0xd83d) + u(0xde00) + `lang"}`, `{"x":"ab` + u(0xd83d) + `cd"}`, `ab\qcd`,
		strings.Repeat("a", MaxTermLength-1) + "éé tail " + strings.Repeat("b", MaxTermLength) + " " +
			strings.Repeat("c", MaxTermLength+1) + " d",
		kelvin + "elvin " + strings.Repeat(dotted, 40) + " " + strings.Repeat(stroked, 30) + " ok",
		"ab\xffcd \xc3 ef\xe2\x82", strings.Repeat(u('A'), MaxTermLength+2) + " x" + u(0xd83d),
	}
	escape := `tail ` + u(0xd83d) + u(0xde00)
	for cut := len("tail ") + 1; cut < len(escape); cut++ {
		texts = append(texts, escape[:cut])
	}
	return append(texts, jsonStretchTexts...)
}

// readTerms is every term r reads in each of texts, once, in the order first read.
func readTerms(r Reading, texts ...string) []string {
	var out []string
	for _, text := range texts {
		r.tokenize(text, func(term []byte, _ int) bool {
			if !slices.Contains(out, string(term)) {
				out = append(out, string(term))
			}
			return true
		})
	}
	return out
}

// finderQuery is a query a text is searched for, and whether every run of it is marked and rewound.
type finderQuery struct {
	terms   []string
	rewinds bool
}

// finderQueries are the queries text is searched for under r: every term it reads, whole or cut in two
// stretches, together; those and one it never reads; and its first terms, its last and a term only a cut
// reads, each alone.
func finderQueries(r Reading, text string) []finderQuery {
	whole := readTerms(r, text)
	cut := slices.Clone(whole)
	for k := range len(text) {
		for _, term := range readTerms(r, text[:k], text[k:]) {
			if !slices.Contains(cut, term) {
				cut = append(cut, term)
			}
		}
	}
	out := []finderQuery{{terms: append(slices.Clone(cut), "zzabsentzz"), rewinds: true}}
	if len(cut) == 0 {
		return out
	}
	out = append(out, finderQuery{terms: cut})
	singles := whole[:min(len(whole), 3)]
	if len(whole) > 3 {
		singles = append(singles, whole[len(whole)-1])
	}
	if len(cut) > len(whole) {
		singles = append(singles, cut[len(whole)])
	}
	for i, term := range singles {
		out = append(out, finderQuery{terms: []string{term}, rewinds: i == 0})
	}
	return out
}

// piece is a stretch FindIn reads, at its offset in the text.
type piece struct {
	at   int
	text string
}

// findIn is FindIn of the pieces under r.
func findIn(r Reading, terms []string, pieces ...piece) (int, bool) {
	return r.FindIn(func(yield func(int, string) bool) {
		for _, p := range pieces {
			if !yield(p.at, p.text) {
				return
			}
		}
	}, terms)
}

// runLengths are the runs marked at a byte, up to left: none, past a byte, a rune, an escape and a pair,
// and to the text's end.
func runLengths(left int) []int {
	var out []int
	for _, l := range []int{0, 1, 2, 3, 5, 6, 7, 11, 12, 13, left} {
		if l <= left && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// A FINDER FINDS WHAT FINDIN FINDS, HOWEVER THE TEXT IS HANDED OVER. Over the package's corpus and its
// edges, under both readings, for each query (finderQueries): Found is FindIn of what was handed over, at
// every byte; the whole is FindIn's whether written a byte at a time or in two writes cut anywhere; two
// stretches cut anywhere are FindIn's two pieces; a run marked anywhere and rewound, a stretch begun after
// it, is FindIn without the run, whether the run went on a stretch or began one, and committed, of the
// text; and a hit before Settled is the text's.
//
// Mutation: drop a held escape across writes, decide a long word's term only at its end and report it
// settled, or leave a term seen inside a rewound run, and a case goes red.
func TestAFinderFindsWhatFindInFinds(t *testing.T) {
	t.Parallel()
	for _, r := range Readings {
		for _, text := range finderTexts() {
			for _, q := range finderQueries(r, text) {
				holdFinderToFindIn(t, r, text, q)
			}
		}
	}
	for r, terms := range map[Reading][]string{Plain: nil, "": {"deploy"}} {
		f := r.Finder(terms)
		_, _ = f.Write([]byte("deploy"))
		if first, all := f.Found(); first != -1 || all {
			t.Errorf("a finder of %q under the reading %q finds (%d, %v), want nothing", terms, r, first, all)
		}
	}
}

// holdFinderToFindIn holds a Finder of q under r to FindIn over text, handed over every way the test names.
func holdFinderToFindIn(t *testing.T, r Reading, text string, q finderQuery) {
	t.Helper()
	terms, n := q.terms, len(text)
	wantFirst, wantAll := findIn(r, terms, piece{0, text})
	check := func(how string, f *Finder, first int, all bool) {
		t.Helper()
		if gf, ga := f.Found(); gf != first || ga != all {
			t.Fatalf("%s reading, %q in %q %s: the finder finds (%d, %v), FindIn (%d, %v)", r, terms, text, how,
				gf, ga, first, all)
		}
	}
	f := r.Finder(terms)
	for k := 0; k <= n; k++ {
		if k > 0 {
			_, _ = f.Write([]byte{text[k-1]})
		}
		first, all := findIn(r, terms, piece{0, text[:k]})
		check("a byte at a time, at byte "+strconv.Itoa(k), f, first, all)
		settled := f.Settled()
		if (wantFirst >= 0 && wantFirst < settled || first >= 0 && first < settled) && first != wantFirst {
			t.Fatalf("%s reading, %q in %q at byte %d: settled before %d, it finds %d, and the text first at %d",
				r, terms, text, k, settled, first, wantFirst)
		}
	}
	for k := 0; k <= n; k++ {
		f := r.Finder(terms)
		_, _ = f.Write([]byte(text[:k]))
		_, _ = f.Write([]byte(text[k:]))
		check("in two writes cut at "+strconv.Itoa(k), f, wantFirst, wantAll)
		f = r.Finder(terms)
		f.Stretch(0)
		_, _ = f.Write([]byte(text[:k]))
		f.Stretch(k)
		_, _ = f.Write([]byte(text[k:]))
		first, all := findIn(r, terms, piece{0, text[:k]}, piece{k, text[k:]})
		check("in two stretches cut at "+strconv.Itoa(k), f, first, all)
	}
	if !q.rewinds {
		return
	}
	for i := 0; i <= n; i++ {
		for _, l := range runLengths(n - i) {
			j := i + l
			for _, begins := range []bool{false, true} {
				for _, marker := range []bool{false, true} {
					how := "with " + text[i:j] + " marked at " + strconv.Itoa(i)
					f := r.Finder(terms)
					_, _ = f.Write([]byte(text[:i]))
					f.Mark()
					if begins {
						f.Stretch(i)
					}
					_, _ = f.Write([]byte(text[i:j]))
					before, words := []piece{{0, text[:i]}}, piece{0, text[i:j]}
					if begins {
						words.at = i
					} else {
						before, words = nil, piece{0, text[:j]}
					}
					first, all := findIn(r, terms, append(before, words)...)
					check(how+", inside the run", f, first, all)
					rest := []piece{{0, text[:i]}, {j, text[j:]}}
					if marker {
						f.Rewind()
						f.Stretch(j)
					} else {
						f.Commit()
						rest = append(before, piece{words.at, text[words.at:]})
					}
					_, _ = f.Write([]byte(text[j:]))
					first, all = findIn(r, terms, rest...)
					check(how+", then "+map[bool]string{true: "rewound", false: "committed"}[marker], f, first, all)
				}
			}
		}
	}
}

// FuzzAFinderFindsWhatFindInFinds holds a Finder to FindIn as TestAFinderFindsWhatFindInFinds does, over
// any text up to 128 bytes, the corpus its seeds: past that, a case costs the fuzzer more than it finds.
func FuzzAFinderFindsWhatFindInFinds(f *testing.F) {
	for _, text := range finderTexts() {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 128 {
			return
		}
		for _, r := range Readings {
			for _, q := range finderQueries(r, text) {
				holdFinderToFindIn(t, r, text, q)
			}
		}
	})
}
