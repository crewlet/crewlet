package codeindex

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/regexlit"
)

// perLiteral is c's blobs in s found a literal at a time, as a lookup found
// them before it read each list a clause names once: every trigram of each
// literal's text — taken from the text again, not from the parts the plan
// keeps of it, so a part the split lost is caught — decoded whole and
// intersected, the literals' blobs unioned, and the escapes where c is
// folded.
func perLiteral(t *testing.T, c clause, s *Segment) []uint32 {
	t.Helper()
	admitted := map[uint32]bool{}
	for _, a := range c.alts {
		for _, o := range holding(t, s, appendTrigrams(nil, []byte(a.text))) {
			admitted[o] = true
		}
	}
	if c.folded {
		for _, o := range s.escapes {
			admitted[o] = true
		}
	}
	return slices.Sorted(maps.Keys(admitted))
}

// holding is the blobs of s holding every one of trigrams, each list decoded
// whole and intersected: a literal's blobs, as a lookup found them a literal
// at a time.
func holding(t *testing.T, s *Segment, trigrams []Trigram) []uint32 {
	t.Helper()
	var set []uint32
	for i, tg := range trigrams {
		list, ok := s.lookup(tg)
		if !ok {
			return nil
		}
		got, err := s.decode(list, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			set = got
		} else {
			set = intersect(set, got)
		}
	}
	return set
}

// named is the bytes of the posting lists c's literals name in s, each list
// once however many literals hold its trigram: the most a lookup reading no
// list twice can read. Taken from the literals' texts, as perLiteral takes
// them.
func named(c clause, s *Segment) int {
	seen := map[Trigram]bool{}
	total := 0
	for _, a := range c.alts {
		for _, tg := range appendTrigrams(nil, []byte(a.text)) {
			if seen[tg] {
				continue
			}
			seen[tg] = true
			if list, ok := s.lookup(tg); ok {
				total += len(list)
			}
		}
	}
	return total
}

// A CLAUSE READS EACH LIST IT NAMES ONCE. An alternation's literals share
// much of what they hold — (?i)(create|delete|update)User crosses into three
// literals that each hold "use" and "ser" — and the lists of the commonest
// trigrams are a segment's longest, so a clause that looked each literal up
// whole read a list once for every literal holding it, decoded each time into
// a buffer of its own: k·L work and garbage where L would do.
//
// Over a segment of 2 048 blobs, each holding `x := ` and one of the clause's
// words — p000word, q001word, p002word, ... — every word holds "wor" and
// "ord", whose lists name every blob; the words of one decade and parity
// share a trigram ("p00", "q01", ...), and those ending in one digit another
// ("0wo", "1wo", ...); the rest of each word's are its own. A clause of 64
// such words reads no more bytes of postings than the lists it names hold,
// each once. It allocates, beside the object ids it answers, within three
// decodes of each list every word holds — the blobs holding them, read once,
// and the union of what its literals admit, every blob here and so as many
// as such a list names, in the two buffers the union merges between, each
// grown as it fills — and one decode of each list some words share. And it
// reads and allocates within half again of what a clause of 2 such words
// does over a segment whose 2 048 blobs each hold one of those.
//
// Measured under the race detector, as the suite runs: both clauses read
// 12 288 bytes, what their lists hold, and allocate 86 768 bytes (2 words)
// and 100 288 (64). Looked up a literal at a time, the 64 words read 289 280
// bytes to the 2 words' 16 384 and allocated 1 457 152 to their 142 160 —
// eighteen and ten times as much; with only the lists every word holds read
// once, they still read 30 936, two and a half times what the 2 did.
func TestAClauseReadsEachListItNamesOnce(t *testing.T) {
	// Not parallel: runtime.MemStats counts every goroutine's allocations,
	// and a parallel case's would be counted as the plan's.
	const blobs = 2048
	type cost struct {
		read, named       int
		allocated, budget uint64
	}
	lookup := func(words int) cost {
		alts := make([]string, words)
		for i := range alts {
			// Neighbours differ in their first letter, or Go's parser
			// factors their common prefix out of the alternation, and the
			// plan is of the prefix and the rest.
			alts[i] = fmt.Sprintf("%c%03dword", "pq"[i%2], i)
		}
		texts := make([][]byte, blobs)
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
		c, s := plan.index[0], segmentOf(t, texts, 1, 1)
		// The case is the one it claims: "wor" and "ord" in every word,
		// lists some words share once there are more than two, and some
		// trigrams of each word its own.
		if !slices.Equal(c.shared, Trigrams([]byte("word"))) || c.whole || (len(c.common) > 0) != (words > 2) ||
			slices.ContainsFunc(c.alts, func(a alternative) bool { return len(a.own) == 0 }) {
			t.Fatalf("the clause of %d words holds %v in every word and %d trigrams some share: the case is not "+
				"the one it claims", words, c.shared, len(c.common))
		}
		var shared, common uint64
		for _, tg := range c.shared {
			list, ok := s.lookup(tg)
			ordinals, decodeErr := s.decode(list, nil)
			if !ok || decodeErr != nil || len(ordinals) != blobs {
				t.Fatalf("the list of %v names %d of the %d blobs (%v, %v): the case is not the one it claims", tg,
					len(ordinals), blobs, ok, decodeErr)
			}
			// One decode: four bytes an ordinal, a uint32.
			shared += 4 * uint64(len(ordinals))
		}
		for _, tg := range c.common {
			// Decoded into a buffer its bytes bound.
			list, _ := s.lookup(tg)
			common += 4 * uint64(len(list))
		}
		r := reading{s: s}
		set, err := plan.blobs(&r)
		if err != nil || len(set) != blobs {
			t.Fatalf("a clause of %d words admits %d of the %d blobs holding one (%v)", words, len(set), blobs, err)
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got, err := plan.Candidates(s)
		runtime.ReadMemStats(&after)
		if err != nil || len(got) != blobs {
			t.Fatalf("a clause of %d words admits %d of the %d blobs holding one (%v)", words, len(got), blobs, err)
		}
		answer := uint64(len(got)) * oidBytes
		return cost{read: r.read, named: named(c, s), allocated: after.TotalAlloc - before.TotalAlloc,
			budget: answer + 3*shared + common}
	}
	two, many := lookup(2), lookup(64)
	t.Logf("over %d blobs, a clause of 2 words read %d bytes of postings (its lists hold %d) and allocated %d; one "+
		"of 64 read %d (its lists hold %d) and allocated %d, against a budget of %d", blobs, two.read, two.named,
		two.allocated, many.read, many.named, many.allocated, many.budget)
	for words, c := range map[int]cost{2: two, 64: many} {
		if c.read > c.named {
			t.Errorf("a clause of %d words read %d bytes of postings, past the %d the lists it names hold: a list "+
				"is read once for every literal holding its trigram", words, c.read, c.named)
		}
	}
	if many.read > two.read+two.read/2 {
		t.Errorf("a clause of 64 words read %d bytes of postings and one of 2 words %d, over as many blobs: what a "+
			"clause reads grows with its literals", many.read, two.read)
	}
	if many.allocated > many.budget {
		t.Errorf("a clause of 64 words allocated %d bytes for its candidates, past the %d of its answer, three "+
			"decodes of each list every word holds and one of each some share: a list is decoded once a literal",
			many.allocated, many.budget)
	}
	if many.allocated > two.allocated+two.allocated/2 {
		t.Errorf("a clause of 64 words allocated %d bytes for its candidates and one of 2 words %d, over as many "+
			"blobs: what a clause allocates grows with its literals", many.allocated, two.allocated)
	}
}

// A CLAUSE ADMITS WHAT ITS LITERALS LOOKED UP ALONE ADMIT. Reading each list
// a clause names once — the blobs every literal holds hoisted out of their
// union, a list some share decoded once for them, each literal starting from
// the least of what it holds — changes how a lookup reads, never what it
// answers.
//
// Over clauses GENERATED to share trigrams every way — a core in every
// literal, so some trigrams are in them all, and now and then a literal that
// is the core alone; a core in some; words over a few letters, so others are
// in some by chance — and segments of blobs over the same letters, so lists
// run from a blob or two to nearly every one, with a letter no blob holds,
// escapes and binary blobs among them: each clause's blobs are what looking
// every literal up whole finds — every list of it decoded and intersected,
// as a lookup did before — and what the blobs' own text says, a blob admitted
// holding some literal's every trigram, or an escape where the clause is
// folded. A plan of a few such clauses answers their intersection, skipped
// blobs dropped. And no clause's lookup reads more bytes of postings than
// the lists it names hold, each once.
//
// A FIXED SEED, so a failure reproduces exactly; the counts it reports say
// each way a literal can be read was met — from the blobs every literal
// holds, from a list some share, from a list of its own — and a clause that
// is its shared trigrams alone, with blobs holding them, a literal no blob
// can hold, a blob only its escape admits, and plans of several clauses that
// admit some.
func TestAClauseAdmitsWhatItsLiteralsLookedUpAloneAdmit(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(20261028, 7))
	var clauses, plans, wholes, fromShared, fromCommon, fromOwn, unheld, escaped int
	for range 400 {
		letters := "abcdefg"[:3+rng.IntN(5)]
		word := func(least, most int) string {
			b := make([]byte, least+rng.IntN(most-least+1))
			for i := range b {
				b[i] = letters[rng.IntN(len(letters))]
			}
			return string(b)
		}
		texts := make([][]byte, 1+rng.IntN(160))
		for i := range texts {
			var b strings.Builder
			for range rng.IntN(16) {
				b.WriteString(word(1, 6))
				b.WriteByte(" \n"[rng.IntN(2)])
			}
			switch rng.IntN(12) {
			case 0:
				b.WriteString("ſ")
			case 1:
				b.WriteByte(0) // binary, so skipped
			}
			texts[i] = []byte(b.String())
		}
		s := segmentOf(t, texts, 1, 1)

		plan := &Plan{}
		for range 1 + rng.IntN(3) {
			core := word(3, 5)
			if rng.IntN(8) == 0 {
				core += "z" // a letter no blob holds
			}
			every, folded := rng.IntN(2) == 0, rng.IntN(3) == 0
			var terms []regexlit.Term
			for range 1 + rng.IntN(12) {
				middle := word(3, 5)
				if every || rng.IntN(2) == 0 {
					middle = core
				}
				text := word(0, 3) + middle + word(0, 3)
				if rng.IntN(16) == 0 {
					text += "z"
				}
				terms = append(terms, regexlit.Term{Text: text, Folded: folded})
			}
			if every && rng.IntN(4) == 0 {
				terms = append(terms, regexlit.Term{Text: core, Folded: folded})
			}
			c, ok := indexable(terms)
			if !ok {
				t.Fatalf("the clause of %v cannot be looked up: every literal of it is three bytes or more", terms)
			}
			plan.index = append(plan.index, c)
		}

		for _, c := range plan.index {
			clauses++
			r := reading{s: s}
			got, err := c.blobs(&r)
			if err != nil {
				t.Fatal(err)
			}
			want := perLiteral(t, c, s)
			if !slices.Equal(got, want) {
				t.Fatalf("the clause of %v (%d shared, %d some share) over %d blobs admits %v; its literals looked "+
					"up alone admit %v", literals(c), len(c.shared), len(c.common), len(texts), got, want)
			}
			for i, text := range texts {
				if Skip(int64(len(text)), text) {
					if slices.Contains(want, uint32(i)) {
						t.Fatalf("the clause of %v admits the skipped blob %q", literals(c), text)
					}
					continue
				}
				set := trigramSet(lower(text))
				escape := c.folded && regexlit.HasEscape(text) && !holds(c, set)
				if slices.Contains(want, uint32(i)) != (holds(c, set) || escape) {
					t.Fatalf("the clause of %v admits %q %v; its text holds a literal %v", literals(c), text,
						slices.Contains(want, uint32(i)), holds(c, set))
				}
				if escape {
					escaped++
				}
			}
			if limit := named(c, s); r.read > limit {
				t.Fatalf("the lookup of the clause of %v read %d bytes of postings, past the %d the lists it names "+
					"hold: a list was read twice", literals(c), r.read, limit)
			}
			// How each literal was read: the least of what it holds.
			if c.whole {
				if len(holding(t, s, c.shared)) > 0 {
					wholes++
				}
				continue
			}
			first, held := readFirst(t, c, s)
			fromShared += first["shared"]
			fromCommon += first["common"]
			fromOwn += first["own"]
			unheld += held
		}

		got, err := plan.Candidates(s)
		if err != nil {
			t.Fatal(err)
		}
		var want []OID
		var set []uint32
		for i, c := range plan.index {
			if i == 0 {
				set = perLiteral(t, c, s)
			} else {
				set = intersect(set, perLiteral(t, c, s))
			}
		}
		for _, o := range set {
			if !s.Skipped(int(o)) {
				want = append(want, s.OID(int(o)))
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("a plan of %d clauses answers %v; its clauses looked up a literal at a time intersect to %v",
				len(plan.index), got, want)
		}
		if len(plan.index) > 1 && len(want) > 0 {
			plans++
		}
	}
	// EACH WAY A LITERAL IS READ WAS MET, or a lookup whose one path is
	// wrong would pass on cases that never take it.
	t.Logf("%d clauses: %d their shared trigrams alone; literals read first from the blobs every literal holds %d "+
		"times, from a list some share %d, from a list of their own %d; %d literals no blob can hold; %d blobs only "+
		"an escape admits; %d plans of several clauses answered", clauses, wholes, fromShared, fromCommon, fromOwn,
		unheld, escaped, plans)
	if wholes < 20 || fromShared < 50 || fromCommon < 50 || fromOwn < 50 || unheld < 20 || escaped < 20 ||
		plans < 50 {
		t.Fatalf("a way a literal is read was hardly met: the generator no longer writes the cases it claims")
	}
}

// literals is c's literals' texts, for a failure message.
func literals(c clause) []string {
	out := make([]string, len(c.alts))
	for i, a := range c.alts {
		out[i] = a.text
	}
	return out
}

// readFirst is how c's lookup in s reads each of its literals first: from the
// blobs every literal holds ("shared"), a list some literals share
// ("common") or one of its own ("own"), whichever is strictly the least of
// what it holds — and how many of its literals hold a trigram no blob does,
// which a lookup does not read at all. Worked out from the lists' sizes as
// the lookup orders them, independently of it.
func readFirst(t *testing.T, c clause, s *Segment) (map[string]int, int) {
	t.Helper()
	first, held := map[string]int{}, 0
	shared := -1
	if len(c.shared) > 0 {
		shared = len(holding(t, s, c.shared))
	}
	for _, a := range c.alts {
		least, kind, tie := -1, "", false
		consider := func(size int, k string) {
			switch {
			case least < 0 || size < least:
				least, kind, tie = size, k, false
			case size == least:
				tie = true
			}
		}
		if shared >= 0 {
			consider(shared, "shared")
		}
		absent := false
		for _, i := range a.common {
			list, ok := s.lookup(c.common[i])
			absent = absent || !ok
			consider(len(list), "common")
		}
		for _, tg := range a.own {
			list, ok := s.lookup(tg)
			absent = absent || !ok
			consider(len(list), "own")
		}
		switch {
		case shared == 0:
		case absent:
			held++
		case !tie:
			first[kind]++
		}
	}
	return first, held
}

// GALLOP KEEPS WHAT A WALK KEEPS. A literal narrows what it admits by blobs
// it shares with other literals by looking each of its own up from where the
// one before it was found, by steps that double and a binary search back,
// rather than walking the shared blobs — whose length is what reading a
// shared list once saves — so it must keep exactly what intersect keeps:
// over sets generated sparse and dense against each other, the edges a
// doubling step lands on among them (an ordinal found where a step stops,
// sorted's first and last, one past its last, nothing at all), and narrowed
// in its own storage.
func TestGallopKeepsWhatAWalkKeeps(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(20261028, 8))
	sorted := func(n, span int) []uint32 {
		seen := map[uint32]bool{}
		for range n {
			seen[uint32(rng.IntN(span))] = true
		}
		return slices.Sorted(maps.Keys(seen))
	}
	for range 3000 {
		set, of := sorted(rng.IntN(48), 1+rng.IntN(600)), sorted(rng.IntN(600), 1+rng.IntN(600))
		want := intersect(slices.Clone(set), of)
		if got := gallop(nil, set, of); !slices.Equal(got, want) {
			t.Fatalf("gallop of %v within %v keeps %v; a walk keeps %v", set, of, got, want)
		}
		if got := gallop(set[:0], set, of); !slices.Equal(got, want) {
			t.Fatalf("gallop narrowing in place within %v keeps %v; a walk keeps %v", of, got, want)
		}
	}
}
