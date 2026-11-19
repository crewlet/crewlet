package codeindex

import (
	"encoding/binary"
	"fmt"
	"maps"
	"math/bits"
	"math/rand/v2"
	"regexp"
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

// named is the bytes of the posting lists the literals of clauses name in s,
// each list once however many literals and clauses hold its trigram: the
// most a lookup reading no list twice can read. Taken from the literals'
// texts, as perLiteral takes them.
func named(s *Segment, clauses ...clause) int {
	seen := map[Trigram]bool{}
	total := 0
	for _, c := range clauses {
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
	}
	return total
}

// trigramsOf is the trigrams of refs, in order.
func trigramsOf(refs []ref) []Trigram {
	out := make([]Trigram, len(refs))
	for i, f := range refs {
		out[i] = f.t
	}
	return out
}

// alone reports whether a lookup reads f in one place alone, rather than
// decoding it once for the several that read it (Plan.held).
func alone(f ref) bool { return f.held < 0 }

// A CLAUSE READS EACH LIST IT NAMES ONCE. An alternation's literals share much
// of what they hold ((?i)(create|delete|update)User: "use" in all three), and
// the commonest trigrams have the longest lists, so looking each literal up
// whole cost k·L where L would do.
//
// Segment of 2 048 blobs, each holding `x := ` and one word (p000word, ...):
// every word holds "wor" and "ord" (lists naming every blob); words of one
// decade and parity share a trigram ("p00"), those ending in one digit another
// ("0wo"). A clause of 64 words reads no more postings bytes than its lists
// hold, each once; allocates within three decodes of each list every word holds
// and one of each list some share; and costs within half again of a clause of 2
// words. Under race both read 12 288 bytes, against 289 280 looked up per literal.
func TestAClauseReadsEachListItNamesOnce(t *testing.T) {
	// Not parallel: a parallel case's measured call would be counted as this one's.
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
		// The case is the one it claims, read off the words themselves:
		// "wor" and "ord" in every word; past two words, trigrams some words
		// hold and others do not; and trigrams of each word no other holds.
		holders := map[Trigram]int{}
		for _, a := range c.alts {
			for _, tg := range appendTrigrams(nil, []byte(a.text)) {
				holders[tg]++
			}
		}
		var some []Trigram
		owners := map[string]bool{}
		for _, a := range c.alts {
			for _, tg := range appendTrigrams(nil, []byte(a.text)) {
				switch n := holders[tg]; {
				case n == 1:
					owners[a.text] = true
				case n < words && !slices.Contains(some, tg):
					some = append(some, tg)
				}
			}
		}
		if !slices.Equal(trigramsOf(c.shared), Trigrams([]byte("word"))) || c.whole ||
			(len(some) > 0) != (words > 2) || len(owners) != words {
			t.Fatalf("the clause of %d words holds %v in every word, %d trigrams some hold and others do not, and "+
				"trigrams of its own in %d words: the case is not the one it claims", words, trigramsOf(c.shared),
				len(some), len(owners))
		}
		var shared, common uint64
		for _, tg := range trigramsOf(c.shared) {
			list, ok := s.lookup(tg)
			ordinals, decodeErr := s.decode(list, nil)
			if !ok || decodeErr != nil || len(ordinals) != blobs {
				t.Fatalf("the list of %v names %d of the %d blobs (%v, %v): the case is not the one it claims", tg,
					len(ordinals), blobs, ok, decodeErr)
			}
			// One decode: four bytes an ordinal, a uint32.
			shared += 4 * uint64(len(ordinals))
		}
		for _, tg := range some {
			// Decoded into a buffer its bytes bound.
			list, _ := s.lookup(tg)
			common += 4 * uint64(len(list))
		}
		r := reading{s: s}
		set, err := plan.blobs(&r)
		if err != nil || len(set) != blobs {
			t.Fatalf("a clause of %d words admits %d of the %d blobs holding one (%v)", words, len(set), blobs, err)
		}
		var got []OID
		spent, _ := allocations(t, func() { got, err = plan.Candidates(s) })
		if err != nil || len(got) != blobs {
			t.Fatalf("a clause of %d words admits %d of the %d blobs holding one (%v)", words, len(got), blobs, err)
		}
		answer := uint64(len(got)) * oidBytes
		return cost{read: r.read, named: named(s, c), allocated: spent, budget: answer + 3*shared + common}
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

// A PLAN READS EACH LIST ITS CLAUSES NAME ONCE. A pattern's clauses share
// trigrams as an alternation's literals do (foo_a.*foo_b: both hold foo's), and
// two clauses differing only in case are two to regexlit.Required and one to an
// index that lowers both: (foo.*(?i:foo).*){16} is the clause foo and (?i)foo.
// So a list more than one clause reads is decoded once, and no implied clause
// is looked up.
//
// Segment of 2 048 blobs each holding all of 32 words (w000word, ...): the plan
// of all 32 reads no more postings bytes than its lists hold, each once; and
// one word repeated 16 times plain and 16 folded looks up one clause, reading
// and allocating what the word alone does. Looked up per clause, the 32 read
// 393 216 bytes of the 163 840 their lists hold.
func TestAPlanReadsEachListItsClausesNameOnce(t *testing.T) {
	// Not parallel: a parallel case's measured call would be counted as this one's.
	const blobs, words = 2048, 32
	alts := make([]string, words)
	for i := range alts {
		alts[i] = fmt.Sprintf("w%03dword", i)
	}
	texts := make([][]byte, blobs)
	for i := range texts {
		texts[i] = []byte("x := " + strings.Join(alts, " ") + "\n")
	}
	s := segmentOf(t, texts, 1, 1)
	type cost struct {
		clauses, read, named int
		allocated            uint64
	}
	lookup := func(pattern string) cost {
		plan, err := NewPlan(regexp.MustCompile(pattern))
		if err != nil {
			t.Fatal(err)
		}
		r := reading{s: s}
		if set, lookupErr := plan.blobs(&r); lookupErr != nil || len(set) != blobs {
			t.Fatalf("the plan of %s admits %d of the %d blobs, each holding every word (%v)", pattern, len(set),
				blobs, lookupErr)
		}
		var got []OID
		spent, _ := allocations(t, func() { got, err = plan.Candidates(s) })
		if err != nil || len(got) != blobs {
			t.Fatalf("the plan of %s admits %d of the %d blobs, each holding every word (%v)", pattern, len(got),
				blobs, err)
		}
		return cost{clauses: len(plan.index), read: r.read, named: named(s, plan.index...), allocated: spent}
	}
	every := lookup(strings.Join(alts, ".*"))
	alone := lookup(alts[0])
	repeated := lookup(fmt.Sprintf("(%s.*(?i:%s).*){%d}", alts[0], alts[0], words/2))
	t.Logf("over %d blobs, the plan of %d words, a clause each, read %d bytes of postings (its lists hold %d); the "+
		"plan of one word read %d and allocated %d, and of it repeated %d times, as it is and folded, %d "+
		"clauses, read %d and allocated %d", blobs, words, every.read, every.named, alone.read, alone.allocated,
		words, repeated.clauses, repeated.read, repeated.allocated)
	if before := len(regexlit.Required(regexp.MustCompile(fmt.Sprintf("(%s.*(?i:%s).*){%d}", alts[0], alts[0],
		words/2))).Clauses()); before != 2 {
		t.Fatalf("regexlit.Required makes %d clauses of the word as it is and folded, want its 2: the case does not "+
			"reach the index's own dropping", before)
	}
	if every.clauses != words {
		t.Fatalf("the plan of %d words is %d clauses: the case is not the one it claims", words, every.clauses)
	}
	if every.read > every.named {
		t.Errorf("the plan of %d words read %d bytes of postings, past the %d the lists it names hold: a list two "+
			"clauses read is read once a clause", words, every.read, every.named)
	}
	if repeated.clauses != 1 || repeated.read != alone.read {
		t.Errorf("the plan of one word repeated %d times looks up %d clauses and reads %d bytes of postings, and "+
			"of the word alone %d: a clause another implies is looked up", words, repeated.clauses,
			repeated.read, alone.read)
	}
	if repeated.allocated > alone.allocated+alone.allocated/2 {
		t.Errorf("the plan of one word repeated %d times allocated %d bytes for its candidates and of the word "+
			"alone %d: a clause another implies holds its blobs again", words, repeated.allocated,
			alone.allocated)
	}
}

// A LOOKUP ADMITS WHAT ITS LITERALS LOOKED UP ALONE ADMIT: reading each list
// once and skipping implied clauses change how a lookup reads, never what it
// answers. Clauses are GENERATED to share trigrams every way (a core in every literal,
// in some, or alone), with implied clauses now and then (repeated, capitals,
// folded, one more literal) or a plain clause made from a folded one, which
// that one does not imply since it admits an escape; segments over the same
// letters, with escapes and binary blobs. Each clause's blobs equal looking
// every literal up whole; a plan's candidates intersect every clause generated,
// an implied one included; no lookup reads more bytes than its lists hold.
// A FIXED SEED reproduces a failure; the reported counts say every way a
// literal can be read and each edge case was met.
func TestALookupAdmitsWhatItsLiteralsLookedUpAloneAdmit(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(20261028, 7))
	var clauses, plans, wholes, fromShared, fromHeld, fromAlone, unheld, escaped, across, implied int
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

		var generated [][]regexlit.Term
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
			generated = append(generated, terms)
		}
		if rng.IntN(3) == 0 {
			// A clause one already generated implies — or, from a folded
			// one, a plain one it does not, since the folded one admits an
			// escape the plain one does not.
			terms := slices.Clone(generated[rng.IntN(len(generated))])
			switch rng.IntN(5) {
			case 0: // the same again
			case 1:
				terms = append(terms, regexlit.Term{Text: word(3, 6), Folded: terms[0].Folded})
			case 2: // in capitals, which an index lowers
				for i := range terms {
					if !terms[i].Folded {
						terms[i].Text = strings.ToUpper(terms[i].Text)
					}
				}
			case 3: // folded, so admitting an escape too
				for i := range terms {
					terms[i].Folded = true
				}
			case 4: // plain, so admitting no escape
				for i := range terms {
					terms[i].Folded = false
				}
			}
			generated = append(generated, terms)
		}
		all := make([]clause, len(generated))
		for i, terms := range generated {
			c, ok := indexable(terms)
			if !ok {
				t.Fatalf("the clause of %v cannot be looked up: every literal of it is three bytes or more", terms)
			}
			all[i] = c
		}
		plan := &Plan{index: unimplied(slices.Clone(all))}
		plan.share()
		implied += len(all) - len(plan.index)

		for _, c := range plan.index {
			clauses++
			r := reading{s: s}
			got, err := c.blobs(&r, r.held(plan.held))
			if err != nil {
				t.Fatal(err)
			}
			want := perLiteral(t, c, s)
			if !slices.Equal(got, want) {
				t.Fatalf("the clause of %v (%d shared) over %d blobs admits %v; its literals looked up alone admit %v",
					literals(c), len(c.shared), len(texts), got, want)
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
			if limit := named(s, c); r.read > limit {
				t.Fatalf("the lookup of the clause of %v read %d bytes of postings, past the %d the lists it names "+
					"hold: a list was read twice", literals(c), r.read, limit)
			}
			// How each literal was read: the least of what it holds.
			if c.whole {
				if len(holding(t, s, trigramsOf(c.shared))) > 0 {
					wholes++
				}
				continue
			}
			first, held := readFirst(t, c, s)
			fromShared += first["shared"]
			fromHeld += first["held"]
			fromAlone += first["alone"]
			unheld += held
		}
		if acrossClauses(plan) {
			across++
		}

		r := reading{s: s}
		if _, err := plan.blobs(&r); err != nil {
			t.Fatal(err)
		}
		if limit := named(s, plan.index...); r.read > limit {
			t.Fatalf("the lookup of a plan of %d clauses read %d bytes of postings, past the %d the lists it names "+
				"hold: a list was read twice", len(plan.index), r.read, limit)
		}
		got, err := plan.Candidates(s)
		if err != nil {
			t.Fatal(err)
		}
		var want []OID
		var set []uint32
		for i, c := range all {
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
			t.Fatalf("a plan of %d clauses, %d of them generated, answers %v; every clause generated, looked up a "+
				"literal at a time, intersects to %v", len(plan.index), len(all), got, want)
		}
		if len(plan.index) > 1 && len(want) > 0 {
			plans++
		}
	}
	// EACH WAY A LITERAL IS READ WAS MET, or a lookup whose one path is
	// wrong would pass on cases that never take it.
	t.Logf("%d clauses: %d their shared trigrams alone; literals read first from the blobs every literal of their "+
		"clause holds %d times, from a list more than one place reads %d, from a list they read alone %d; %d literals "+
		"no blob can hold; %d blobs only an escape admits; %d plans with a list two clauses read; %d clauses dropped "+
		"as implied; %d plans of several clauses answered", clauses, wholes, fromShared, fromHeld, fromAlone, unheld,
		escaped, across, implied, plans)
	if wholes < 20 || fromShared < 50 || fromHeld < 50 || fromAlone < 50 || unheld < 20 || escaped < 20 ||
		across < 50 || implied < 50 || plans < 40 {
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

// acrossClauses reports whether p's lookup decodes a list for places in two
// clauses: one of p.held that two clauses read.
func acrossClauses(p *Plan) bool {
	readers := map[int]map[int]bool{}
	for i, c := range p.index {
		refs := slices.Clone(c.shared)
		for _, a := range c.alts {
			refs = append(refs, a.rest...)
		}
		for _, f := range refs {
			if alone(f) {
				continue
			}
			if readers[f.held] == nil {
				readers[f.held] = map[int]bool{}
			}
			readers[f.held][i] = true
		}
	}
	for _, by := range readers {
		if len(by) > 1 {
			return true
		}
	}
	return false
}

// readFirst is how c's lookup in s reads each of its literals first: from the
// blobs every literal of c holds ("shared"), a list more than one place
// reads ("held") or one it reads alone ("alone"), whichever is strictly the
// least of what it holds — and how many of its literals hold a trigram no
// blob does, which a lookup does not read at all. Worked out from the lists'
// sizes as the lookup orders them, independently of it.
func readFirst(t *testing.T, c clause, s *Segment) (map[string]int, int) {
	t.Helper()
	first, held := map[string]int{}, 0
	shared := -1
	if len(c.shared) > 0 {
		shared = len(holding(t, s, trigramsOf(c.shared)))
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
		for _, f := range a.rest {
			list, ok := s.lookup(f.t)
			absent = absent || !ok
			if alone(f) {
				consider(len(list), "alone")
			} else {
				consider(len(list), "held")
			}
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
		if got, _ := gallop(nil, set, of); !slices.Equal(got, want) {
			t.Fatalf("gallop of %v within %v keeps %v; a walk keeps %v", set, of, got, want)
		}
		if got, _ := gallop(set[:0], set, of); !slices.Equal(got, want) {
			t.Fatalf("gallop narrowing in place within %v keeps %v; a walk keeps %v", of, got, want)
		}
	}
}

// GALLOP LOOKS AT A FEW ORDINALS FOR EACH IT LOOKS FOR. What narrowing by
// blobs several places share costs a literal is what it holds, never the
// length of what it shares (lookup.go's head): a literal of a clause of 64,
// narrowing its few blobs by the blobs every literal holds, must not walk
// them, or the clause walks them 64 times — the k·L that reading each list
// once saved in bytes, spent again in time. So over sets generated sparse
// against sorted ones of up to 4 096 ordinals, and dense, gallop looks at no
// more than 2(⌊log₂ n⌋+2) ordinals of sorted for each of set's, n sorted's
// length; and of 8 ordinals spread over 4 096, within the 224 that bound,
// where a walk looks at every ordinal up to the last of them.
func TestGallopLooksAtAFewOrdinalsForEachItLooksFor(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(20261102, 1))
	sorted := func(n, span int) []uint32 {
		seen := map[uint32]bool{}
		for range n {
			seen[uint32(rng.IntN(span))] = true
		}
		return slices.Sorted(maps.Keys(seen))
	}
	bound := func(set, of []uint32) int { return len(set) * 2 * (bits.Len(uint(len(of))) + 1) }
	for range 2000 {
		span := 1 + rng.IntN(4096)
		set, of := sorted(1+rng.IntN(16), span), sorted(rng.IntN(span+1), span)
		if rng.IntN(4) == 0 {
			set = sorted(rng.IntN(span+1), span)
		}
		if _, probes := gallop(nil, set, of); probes > bound(set, of) {
			t.Fatalf("gallop of %d ordinals within %d looked at %d of them, past the %d of 2(⌊log₂ %d⌋+2) for "+
				"each: it walks what it narrows by", len(set), len(of), probes, bound(set, of), len(of))
		}
	}
	set := make([]uint32, 8)
	for i := range set {
		set[i] = uint32(i*512 + 441)
	}
	of := make([]uint32, 4096)
	for i := range of {
		of[i] = uint32(i)
	}
	_, probes := gallop(nil, set, of)
	t.Logf("gallop of 8 ordinals spread over 4 096 looked at %d of them, against a bound of %d", probes,
		bound(set, of))
	if probes > bound(set, of) {
		t.Errorf("gallop of 8 ordinals spread over 4 096 looked at %d of them, past the %d of 2(⌊log₂ 4096⌋+2) "+
			"for each: it walks what it narrows by", probes, bound(set, of))
	}
}

// through is the bytes of list, a well-formed posting list, up to and
// including its first ordinal past last, or all of them where none is: the
// most a read that stops once it passes last can read. Counted from the
// bytes, apart from the reader a lookup reads them with.
func through(list []byte, last uint32) int {
	var at uint64
	for i := 0; i < len(list); {
		v, n := binary.Uvarint(list[i:])
		if n <= 0 {
			return len(list)
		}
		if i == 0 {
			at = v
		} else {
			at += v
		}
		i += n
		if at > uint64(last) {
			return i
		}
	}
	return len(list)
}

// A LOOKUP READS A LIST ONLY AS FAR AS THE BLOBS IT KEEPS. Blobs sit in id
// order, so a place narrowing by a list of its own reads it only until it
// passes the last blob it keeps (reading.listed), and a list decoded for
// several places only as far as the furthest blob one needs (decoded.upTo): a
// pattern held by a few early blobs, beside lists naming every blob, reads
// those lists a few bytes in, never to their ends.
//
// Segment of 2 048 blobs each holding abcdefmn and abcdefuv, the first 16
// zzabcdefmn and yyabcdefuv. The plan of zzabcdefmn.*yyabcdefuv is two clauses:
// abcdef's trigrams, in both, decoded once for the two (Plan.held); efm, fmn,
// efu and fuv each read alone, all naming every blob, zz's and yy's the first
// 16. Each list is read no further than its first ordinal past the 16th blob.
func TestALookupReadsAListOnlyAsFarAsTheBlobsItKeeps(t *testing.T) {
	t.Parallel()
	const blobs, early = 2048, 16
	texts := make([][]byte, blobs)
	for i := range texts {
		texts[i] = []byte("x := abcdefmn abcdefuv\n")
		if i < early {
			texts[i] = []byte("x := zzabcdefmn yyabcdefuv\n")
		}
	}
	s := segmentOf(t, texts, 1, 1)
	plan, err := NewPlan(regexp.MustCompile("zzabcdefmn.*yyabcdefuv"))
	if err != nil {
		t.Fatal(err)
	}
	// The case is the one it claims: two clauses, lists they both read and
	// lists one reads alone, each naming every blob.
	var heldEvery, aloneEvery bool
	for _, c := range plan.index {
		refs := slices.Clone(c.shared)
		for _, a := range c.alts {
			refs = append(refs, a.rest...)
		}
		for _, f := range refs {
			list, _ := s.lookup(f.t)
			if got, decodeErr := s.decode(list, nil); decodeErr != nil || len(got) != blobs {
				continue
			}
			if alone(f) {
				aloneEvery = true
			} else {
				heldEvery = true
			}
		}
	}
	if len(plan.index) != 2 || !heldEvery || !aloneEvery {
		t.Fatalf("the plan is %d clauses, a list naming every blob read by several %v, by one alone %v: the "+
			"case is not the one it claims", len(plan.index), heldEvery, aloneEvery)
	}

	r := reading{s: s}
	set, err := plan.blobs(&r)
	if err != nil || len(set) != early || set[early-1] != early-1 {
		t.Fatalf("the plan admits %v (%v); the first %d blobs hold its pattern", set, err, early)
	}
	bound, whole := 0, 0
	seen := map[Trigram]bool{}
	for _, c := range plan.index {
		for _, a := range c.alts {
			for _, tg := range appendTrigrams(nil, []byte(a.text)) {
				if seen[tg] {
					continue
				}
				seen[tg] = true
				list, _ := s.lookup(tg)
				bound += through(list, set[early-1])
				whole += len(list)
			}
		}
	}
	t.Logf("over %d blobs, a lookup admitting the first %d read %d bytes of postings; its lists through their "+
		"first ordinal past the last it keeps hold %d, whole %d", blobs, early, r.read, bound, whole)
	if r.read > bound {
		t.Errorf("a lookup admitting the first %d of %d blobs read %d bytes of postings, past the %d of its lists "+
			"through their first ordinal past the last it keeps: a list is read past the blobs a lookup keeps",
			early, blobs, r.read, bound)
	}
}

// A CLAUSE'S UNION SPARES WHAT IT MERGES, NOT WHAT ITS BUFFER CAN HOLD. A
// lookup decodes a literal's least list straight into its clause's union and
// narrows it there (reading.admitted), so a union can hold a few blobs in the
// capacity of the longest list decoded; the spare it merges into is made as
// long as what it merges, not the buffer, or it was that list's decode again
// for every clause of two or more narrowing literals. Segment of 16 384 blobs each holding the first halves of three words (abcde,
// ijklm, qrstu) or their second halves, one in 1 024 all three whole: each
// literal of abcdefgh|ijklmnop|qrstuvwx decodes half the blobs and keeps 16.
// Merging the 48 admitted ordinals allocates within twice them and their 3
// lists' ends. It measures the merge alone: the whole lookup's allocation
// turns on whether race doubles slices.Grow's growth.
func TestAClausesUnionSparesWhatItMergesNotWhatItsBufferHolds(t *testing.T) {
	// Not parallel: a parallel case's measured call would be counted as this one's.
	const blobs, every = 16384, 1024
	texts := make([][]byte, blobs)
	for i := range texts {
		switch {
		case i%every == 0:
			texts[i] = []byte("x := abcdefgh ijklmnop qrstuvwx\n")
		case i%2 == 0:
			texts[i] = []byte("x := abcde ijklm qrstu\n")
		default:
			texts[i] = []byte("x := defgh lmnop tuvwx\n")
		}
	}
	s := segmentOf(t, texts, 1, 1)
	plan, err := NewPlan(regexp.MustCompile("abcdefgh|ijklmnop|qrstuvwx"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.index) != 1 || len(plan.index[0].alts) != 3 || len(plan.index[0].shared) != 0 {
		t.Fatalf("the plan is %d clauses: the case is not the one it claims", len(plan.index))
	}
	c := plan.index[0]
	r := reading{s: s}
	var u union
	if err := c.literals(&r, &u, r.held(plan.held)); err != nil {
		t.Fatal(err)
	}
	admitted, lists := len(u.buf), len(u.ends)
	// The case is the one it claims: the union holds what three literals
	// admit in the capacity of a list one of them decoded.
	if lists != 3 || admitted != 3*blobs/every || cap(u.buf) < blobs/2 {
		t.Fatalf("the union holds %d lists, %d ordinals in a capacity of %d: the case is not the one it claims",
			lists, admitted, cap(u.buf))
	}
	capacity := cap(u.buf)
	var got []uint32
	allocated, _ := allocations(t, func() { got = u.merged() })
	if want := perLiteral(t, c, s); !slices.Equal(got, want) {
		t.Fatalf("the union of what the literals admit is %v; looked up a literal at a time, %v", got, want)
	}
	budget := uint64(2 * (4*admitted + 8*lists))
	t.Logf("merging %d ordinals of %d lists from a buffer of capacity %d allocated %d bytes, against a budget of "+
		"%d", admitted, lists, capacity, allocated, budget)
	if allocated > budget {
		t.Errorf("merging the %d ordinals %d literals admit allocated %d bytes, past the %d of twice them and "+
			"their ends: the spare is as long as the buffer can hold, not as what it merges", admitted, lists,
			allocated, budget)
	}
}
