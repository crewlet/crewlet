package codeindex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/regexlit"
)

// ErrBadQuery is every code search this package refuses — a query its
// grammar refuses, or a pattern no plan can be made of — so a surface maps
// them all to one answer; each is a *QueryError, whose text says what to
// change.
var ErrBadQuery = errors.New("codeindex: a code search refused")

// The refusals a caller may tell apart; each is also ErrBadQuery.
var (
	// ErrQueryTooLong is a query past MaxQueryBytes.
	ErrQueryTooLong = errors.New("codeindex: a code search past the bound")
	// ErrNotLineLocal is a pattern that can match across a line, or at a
	// whole file's start or end (regexlit.LineLocal), where a code search
	// answers lines.
	ErrNotLineLocal = errors.New("codeindex: a pattern that can match across lines")
	// ErrNoLiteral is a pattern whose matches need not hold any literal of
	// three bytes or more, so no index can narrow it.
	ErrNoLiteral = errors.New("codeindex: a pattern no index can narrow")
	// ErrTooComplex is a pattern whose program, as Go's regexp compiles it,
	// is past MaxProgram instructions.
	ErrTooComplex = errors.New("codeindex: a pattern past the program a search runs")
)

// MaxProgram is the most instructions a pattern's program may hold — the
// program Go's regexp runs it as, syntax.Compile of its simplified parse —
// for a search to plan it; a larger one is REFUSED (ErrTooComplex).
//
// What confirming a line costs is its bytes times the threads the regexp
// steps at each of them, at most its program's instructions (MaxLineWork),
// and a counted repetition compiles to as many copies of what it repeats:
// .{0,999} alone is two thousand instructions, and the 398-byte "the" +
// 49 × .{0,999} + "zzz" is 97 910, which took 1.5–2 ms a byte of a line
// holding no match. What a person or a seat searches for compiles to a few
// dozen — `func \w+\(` to ten, a hex object id's [0-9a-f]{40} to 42, a
// literal or a word of MaxQueryBytes, folded, to about four hundred and
// foo.{0,500}bar to 1 008 — so the bound refuses only a pattern a counted
// repetition multiplied, and every pattern it plans runs on any line of
// MaxLineWork/MaxProgram bytes (32 KiB) or fewer.
const MaxProgram = 1 << 10

// MaxLineWork bounds what confirming one line may cost, in the regexp's own
// unit: the line's bytes times its program's instructions. Go's regexp steps
// each instruction at most once a byte — its NFA queues each once a step, its
// backtracker visits each pair of instruction and position once — so the
// product bounds its work whatever the pattern and the line; and a run of it
// cannot be stopped part way, so a line past the bound is NOT RUN: Confirm
// counts it, for a caller to report among what the search did not read,
// never among what holds no match.
//
// 2^25: a program of 32 instructions or fewer — a literal of about thirty
// bytes, and most of the expressions a search is written with — runs on a
// whole MaxFileBytes line, the longest a blob the index holds can have (a
// minified file is one line), and one at MaxProgram on 32 KiB. Measured in
// one bounded run (BenchmarkALineAtTheWorkBound, -benchtime 3x, one count,
// no race detector, on two E-cores of a Core Ultra 7 265U), a regexp holding
// every instruction live costs 6.3–6.7 ns an instruction-byte: 0.22 s for
// MaxProgram's program over its 32 KiB, the bound exactly, and 0.16 s for 24
// instructions over a whole mebibyte line — so a search that checks its
// budget between lines (Confirm's context) overruns it by a quarter of a
// second at most there. One exploratory run on the same cores found the same
// 6.4–7.0 ns from 24 instructions to 2 006, and 15 ns at the 97 910 of the
// pattern MaxProgram's doc names, whose program no cache holds.
const MaxLineWork = MaxFileBytes << 5

// QueryError is a refused code search: which refusal, and the sentence a
// person or a seat reads — what was refused, and what to change.
type QueryError struct {
	// Err is ErrQueryTooLong, ErrNotLineLocal, ErrNoLiteral, or ErrBadQuery
	// for every other refusal.
	Err error
	// Reason is the sentence.
	Reason string
}

func (e *QueryError) Error() string { return e.Reason }

// Is makes a QueryError its own refusal, and ErrBadQuery.
func (e *QueryError) Is(target error) bool { return target == e.Err || target == ErrBadQuery }

// refuse is a QueryError of kind, its reason formatted.
func refuse(kind error, format string, args ...any) error {
	return &QueryError{Err: kind, Reason: fmt.Sprintf(format, args...)}
}

// Plan is what a search runs over a repository's index: the expression, what
// its matches cannot do without (regexlit.Required) — the clauses, each a set
// of literals every match holds one of — and the clauses the index can look
// up, none another implies. Made by NewPlan, or by a Query's Plan.
type Plan struct {
	re    *regexp.Regexp
	need  regexlit.Prefilter
	index []clause
	// held is the trigrams the plan's lookup reads in more than one place,
	// ascending: each decoded once a lookup, for every place that reads it
	// (ref).
	held []Trigram
	// insts is the instructions of re's program, at most MaxProgram.
	insts int
}

// ref is a trigram as a plan's lookup reads it (lookup.go). A lookup reads a
// trigram in a PLACE: a clause reads the trigrams every literal of it holds
// in one, as the blobs holding them all, and each literal its others in its
// own. Where held is a place in Plan.held, more than one place reads the
// trigram, and its list is decoded once a lookup for all of them; where it is
// none, the one place that reads it reads it alone, as it narrows what it
// admits.
type ref struct {
	t    Trigram
	held int
}

// clause is a clause of the prefilter the index can look up: every literal
// of it three bytes or more in the form the index holds it.
//
// ITS TRIGRAMS ARE SORTED BY WHERE A LOOKUP READS THEM, once a plan rather
// than once a segment searched, so that it reads each list the plan names
// once (lookup.go): those every literal holds (shared) once for them all,
// each literal's others (alternative.rest) by it, and a trigram more than
// one place reads, of this clause or another, once for every one (ref).
type clause struct {
	alts []alternative
	// shared is the trigrams every literal holds, ascending; none where no
	// trigram is in them all.
	shared []ref
	// whole is whether some literal holds nothing beyond shared: it admits
	// every blob holding shared, among which is every blob another literal
	// admits, so the clause's blobs are those.
	whole bool
	// folded is whether any literal of it is compared folded, so that a
	// blob holding an escape satisfies it whatever its trigrams say.
	folded bool
}

// alternative is one literal of a clause, lowered as the index is, with its
// trigrams beyond those every literal of the clause holds (clause.shared).
type alternative struct {
	text string
	// grep is every spelling of it the expression requires, as a line holds
	// it — a plain literal as written, a folded one in its lowering — and
	// folded whether any is compared folded: what git grep looks for
	// (Plan.Grep), which reads no lowering.
	grep   []string
	folded bool
	// rest is its trigrams beyond the clause's shared ones, ascending.
	rest []ref
}

// NewPlan plans re over the index. It REFUSES a pattern whose program is past
// MaxProgram instructions, because what confirming a line with it costs grows
// with them; one that can match across a line or at a file's very start or
// end (regexlit.LineLocal), because a search answers lines and confirms them
// one at a time; and one whose every match can do without a literal of three
// bytes or more, because no trigram narrows it and the search would read
// every file — ErrTooComplex, ErrNotLineLocal and ErrNoLiteral, each a
// *QueryError.
//
// re must come from regexp.Compile or regexp.MustCompile, as
// regexlit.Required's must.
func NewPlan(re *regexp.Regexp) (*Plan, error) {
	return newPlan(re, fmt.Sprintf("the pattern %q", re.String()))
}

// newPlan is NewPlan, its refusals naming the pattern as what.
func newPlan(re *regexp.Regexp, what string) (*Plan, error) {
	// The program first: past the bound, the walks below would only cross
	// the hundred thousand nodes its counted repetitions expand to.
	insts, err := program(re)
	if err != nil {
		// re compiled, so its source compiles again; were it not to,
		// refusing is the one answer that runs nothing unbounded.
		return nil, refuse(ErrBadQuery, "%s does not compile: %v", what, err)
	}
	if insts > MaxProgram {
		return nil, refuse(ErrTooComplex, "%s compiles to %d instructions, past the %d a code search runs a "+
			"line with: a counted repetition compiles to as many copies of what it repeats, so write .* "+
			"where it has .{0,999}, or a smaller count, or search for a literal part of it alone", what, insts,
			MaxProgram)
	}
	if !regexlit.LineLocal(re) {
		return nil, refuse(ErrNotLineLocal, "%s can match a line break, or the very start or end of a "+
			"file (\\n, \\s, a class holding a line break, . under (?s), \\A, \\z), and a code search "+
			"answers lines: keep every part of it within one line", what)
	}
	need := regexlit.Required(re)
	p := &Plan{re: re, need: need, insts: insts}
	for _, terms := range need.Terms() {
		if c, ok := indexable(terms); ok {
			p.index = append(p.index, c)
		}
	}
	if len(p.index) == 0 {
		return nil, refuse(ErrNoLiteral, "%s can match without a run of three bytes or more that every "+
			"match holds, so no index narrows it and the search would read every file: search for a longer "+
			"word, or add one every match contains", what)
	}
	p.index = unimplied(p.index)
	p.share()
	return p, nil
}

// program is how many instructions Go's regexp runs re as: its source parsed
// as regexp.Compile parses it, simplified and compiled, which is what
// regexp.Compile does — the program itself is not exported.
func program(re *regexp.Regexp) (int, error) {
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return 0, err
	}
	prog, err := syntax.Compile(parsed.Simplify())
	if err != nil {
		return 0, err
	}
	return len(prog.Inst), nil
}

// indexable is terms as a clause the index can look up, or false where one of
// them is shorter than a trigram in the form the index holds it — a clause is
// satisfied by any one of its literals, so a literal no trigram can find
// leaves the whole clause unable to rule a blob out.
//
// A folded literal is already lowered (regexlit.Term). A plain one is lowered
// here, as the index lowered the blob: a blob holding it holds its lowering
// in its own, since lowering goes rune by rune and the literal is whole runes
// — so the lookup admits a blob that holds it in another case too, and never
// misses one that holds it as it is.
//
// Its literals' trigrams are sorted into those every literal holds and each
// one's rest (clause) here, once a plan, rather than once a segment
// searched; which of them more than one place reads is the plan's to say
// (Plan.share).
func indexable(terms []regexlit.Term) (clause, bool) {
	var c clause
	var trigrams [][]Trigram
	seen := map[string]int{}
	for _, term := range terms {
		text := term.Text
		if term.Folded {
			c.folded = true
		} else {
			text = string(lower([]byte(text)))
		}
		if len(text) < trigramBytes {
			return clause{}, false
		}
		if i, ok := seen[text]; ok {
			// One literal to the index, and another spelling to git grep,
			// which compares the bytes a line holds (Plan.Grep).
			a := &c.alts[i]
			if !slices.Contains(a.grep, term.Text) {
				a.grep = append(a.grep, term.Text)
			}
			a.folded = a.folded || term.Folded
			continue
		}
		seen[text] = len(c.alts)
		c.alts = append(c.alts, alternative{text: text, grep: []string{term.Text}, folded: term.Folded})
		trigrams = append(trigrams, appendTrigrams(nil, []byte(text)))
	}
	if len(c.alts) == 0 {
		return clause{}, false
	}
	// A literal's trigrams are distinct and ascending, so a trigram every
	// literal holds is one counted once for each, and each part keeps their
	// order.
	holders := map[Trigram]int{}
	for _, ts := range trigrams {
		for _, t := range ts {
			holders[t]++
		}
	}
	for i, ts := range trigrams {
		a := &c.alts[i]
		for _, t := range ts {
			switch {
			case holders[t] < len(c.alts):
				a.rest = append(a.rest, ref{t: t, held: -1})
			case i == 0:
				c.shared = append(c.shared, ref{t: t, held: -1})
			}
		}
		c.whole = c.whole || len(a.rest) == 0
	}
	return c, true
}

// unimplied is index without each clause another one implies — every blob
// the other admits, it admits too, so the intersection of them all is the
// same without it, and a lookup reads none of its lists: each literal of the
// other, as the index holds it, is one of its own, and it admits an escape
// where the other does. Of two alike, the first stays. regexlit.Required
// requires a part once for every time a pattern repeats it — (foo.*){100}
// is a hundred clauses of foo, each of which would read foo's lists and hold
// their every blob — and two clauses whose literals differ only in case are
// one to an index that lowers them both.
func unimplied(index []clause) []clause {
	texts := make([]map[string]bool, len(index))
	for i, c := range index {
		texts[i] = map[string]bool{}
		for _, a := range c.alts {
			texts[i][a.text] = true
		}
	}
	// implies is whether every blob index[i] admits, index[j] admits too.
	implies := func(i, j int) bool {
		if index[i].folded && !index[j].folded {
			return false
		}
		for text := range texts[i] {
			if !texts[j][text] {
				return false
			}
		}
		return true
	}
	var out []clause
	for i := range index {
		implied := false
		for j := range index {
			if j != i && implies(j, i) && (j < i || !implies(i, j)) {
				implied = true
				break
			}
		}
		if !implied {
			out = append(out, index[i])
		}
	}
	return out
}

// share makes each trigram p's lookup reads in more than one place (ref) a
// place of p.held, so that it is decoded once a lookup for all of them —
// whether two literals of one clause hold it, or two clauses do.
func (p *Plan) share() {
	each := func(f func(*ref)) {
		for i := range p.index {
			c := &p.index[i]
			for j := range c.shared {
				f(&c.shared[j])
			}
			for k := range c.alts {
				for j := range c.alts[k].rest {
					f(&c.alts[k].rest[j])
				}
			}
		}
	}
	reads := map[Trigram]int{}
	each(func(r *ref) { reads[r.t]++ })
	for t, n := range reads {
		if n > 1 {
			p.held = append(p.held, t)
		}
	}
	slices.Sort(p.held)
	each(func(r *ref) {
		if i, ok := slices.BinarySearch(p.held, r.t); ok {
			r.held = i
		}
	})
}

// Regexp is the expression p plans.
func (p *Plan) Regexp() *regexp.Regexp { return p.re }

// Grep is what a search that reads a commit with git grep in place of an
// index looks for (M6-D13): ONE extended regular expression (git grep -E)
// that is nothing but an alternation of fixed strings, any one of which a
// line must hold for p's expression to match it, and whether git compares
// them ignoring case (-i) — so the lines git answers are a superset of the
// lines the expression matches, which Confirm then holds to RE2 exactly as
// the index path's candidates are. ok is false where no clause of p can be
// looked for that way, and git must answer every line.
//
// THE MOST SELECTIVE CLAUSE THAT GIT READS EXACTLY: the one whose shortest
// string is longest, of the fewest strings. git's -i folds ASCII letters
// alone and lowers no line, so a clause is exact only where every folded
// literal of it is ASCII — a plain literal is looked for as written, its
// other bytes compared as they are — and, where -i is set, each rune
// outside ASCII that RE2 folds onto an ASCII letter, or that a folded
// literal's lowering turned into one (asciiFolds: ſ for s, the Kelvin sign
// for k, İ for i), is looked for too, admitting every line that holds one:
// (?i)status matches ſtatus, which no ASCII fold finds, and (?i)İstanbul,
// lowered to istanbul, matches İSTANBUL, which git's -i cannot see. A clause
// folding a literal outside ASCII — (?i)école, whose É git's -i cannot
// see — is not one git reads exactly, and is never chosen; nor is one with a
// string holding a NUL, which no argument of a process can carry, or a
// newline, at which git splits a pattern in two.
//
// ONE ALTERNATION, NEVER A STRING A PATTERN (-F -e s1 -e s2 …): git looks
// ahead for the next line any of its patterns matches, scanning the rest of
// the file for EACH pattern after every hit, so a second string a file lacks
// costs a scan of the rest of it at every line the first matches —
// quadratic in the file: a second for a 1 MiB file of matching lines; and
// -F -i compiles each string outside ASCII as a case-folding expression of
// its own: three and a half seconds for that file with the three
// asciiFolds. One alternation is one pattern, whose look-ahead is linear:
// 0.02 s for each (git 2.53 under LC_ALL=C, as gitproc runs it, one core,
// a file of 46 000 lines each matching, every figure measured once). It is
// no second dialect of the query: every byte of the expression syntax in a
// string is escaped (ereMeta), so it matches exactly the lines -F would,
// and the query is answered by RE2 alone (Confirm).
func (p *Plan) Grep() (pattern string, fold, ok bool) {
	pieces, fold, ok := p.grepStrings()
	if !ok {
		return "", false, false
	}
	var b strings.Builder
	for i, piece := range pieces {
		if i > 0 {
			b.WriteByte('|')
		}
		for j := range len(piece) {
			if strings.IndexByte(ereMeta, piece[j]) >= 0 {
				b.WriteByte('\\')
			}
			b.WriteByte(piece[j])
		}
	}
	return b.String(), fold, true
}

// ereMeta is every byte a POSIX extended regular expression reads as
// syntax: Grep escapes each, so a string is matched as its bytes.
const ereMeta = `\.[]()*+?{}|^$`

// grepStrings is the strings of the clause Grep looks for, and whether git
// compares them ignoring case.
func (p *Plan) grepStrings() (pieces []string, fold, ok bool) {
	best, bestShort := -1, 0
	for i, c := range p.index {
		short, exact := 0, true
		for j, a := range c.alts {
			if a.folded && !ascii(a.text) {
				exact = false
			}
			for k, g := range a.grep {
				if strings.ContainsAny(g, "\x00\n") {
					exact = false
				}
				if (j == 0 && k == 0) || len(g) < short {
					short = len(g)
				}
			}
			if !exact {
				break
			}
		}
		if !exact {
			continue
		}
		if best < 0 || short > bestShort || (short == bestShort && c.grepLen() < p.index[best].grepLen()) {
			best, bestShort = i, short
		}
	}
	if best < 0 {
		return nil, false, false
	}
	c := p.index[best]
	for _, a := range c.alts {
		pieces = append(pieces, a.grep...)
	}
	if c.folded {
		pieces = append(pieces, asciiFolds()...)
	}
	return pieces, c.folded, true
}

// grepLen is how many strings git grep looks for of c.
func (c clause) grepLen() int {
	n := 0
	for _, a := range c.alts {
		n += len(a.grep)
	}
	return n
}

// ascii reports whether s holds no byte past ASCII.
func ascii(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// asciiFolds is every rune outside ASCII that a folded clause's ASCII
// strings can stand for in a line git's ASCII-only -i does not fold: those
// RE2's (?i) folds together with an ASCII letter, and those whose Unicode
// lowering is one — a folded literal is held as its lowering
// (regexlit.Term), so (?i)İstanbul's is istanbul, while RE2 matches the İ it
// was written with. Derived from the Unicode tables this build carries, as
// regexlit's escapes are, rather than listed: U+0130 (İ), U+017F (ſ) and
// U+212A (the Kelvin sign) today. Not regexlit.HasEscape's set: the Kelvin
// sign and İ lower to k and i, so the index sees through them, while git's
// -i does not.
var asciiFolds = sync.OnceValue(func() []string {
	found := map[rune]bool{}
	for c := rune(0); c < utf8.RuneSelf; c++ {
		for m := unicode.SimpleFold(c); m != c; m = unicode.SimpleFold(m) {
			if m >= utf8.RuneSelf {
				found[m] = true
			}
		}
	}
	// Every rune with a lowering lies in a case range, so the ranges are
	// the whole of the walk.
	for _, cr := range unicode.CaseRanges {
		for r := max(rune(cr.Lo), utf8.RuneSelf); r <= rune(cr.Hi); r++ {
			if l := unicode.ToLower(r); l < utf8.RuneSelf {
				found[r] = true
			}
		}
	}
	var out []string
	for _, m := range slices.Sorted(maps.Keys(found)) {
		out = append(out, string(m))
	}
	return out
})

// LineBytes is the longest line Confirm runs p's expression on: MaxLineWork
// over its program's instructions, so MaxLineWork/MaxProgram (32 KiB) at the
// least and, for a program of 32 instructions or fewer, past any line a blob
// the index holds can have.
func (p *Plan) LineBytes() int { return MaxLineWork / p.insts }

// Candidates is every blob of s the plan cannot rule out, ascending by id —
// and so every blob of s that holds a match, which is the plan's whole
// promise; each still has to be confirmed (Confirm). A blob is ruled out by
// a clause none of whose literals it can hold — some trigram of each missing
// from its postings — unless the clause holds a folded literal and the blob
// an escape (regexlit.HasEscape), which no lowering sees through. A skipped
// blob is never a candidate.
//
// The lookup reads each list the plan names once, whichever literals of
// whichever clauses hold its trigram (lookup.go).
func (p *Plan) Candidates(s *Segment) ([]OID, error) {
	set, err := p.blobs(&reading{s: s})
	if err != nil || len(set) == 0 {
		return nil, err
	}
	out := make([]OID, 0, len(set))
	for _, o := range set {
		if !s.Skipped(int(o)) {
			out = append(out, s.OID(int(o)))
		}
	}
	return out, nil
}

// Hit is a line of a blob the plan's expression matches.
type Hit struct {
	// Line is the line's number, the blob's first line being 1.
	Line int
	// Text is the line without its newline: the blob's own bytes, valid
	// while they are, and capped so an append to it copies.
	Text []byte
	// Start and End are the line's first match, as offsets into Text.
	Start, End int
}

// Confirm calls hit with every line of blob the plan's expression matches, in
// order, until hit returns false: RE2's answer, whichever path offered the
// blob. The expression runs on each line alone — exact, because the plan
// admits only a pattern whose matches lie within a line (regexlit.LineLocal)
// — and only on a line that holds what every match needs
// (regexlit.Prefilter.Admits), so a blob of thousands of lines runs it on the
// few that can match.
//
// What it spends is bounded where a run of the regexp cannot be stopped. A
// line that holds what every match needs but is longer than LineBytes is NOT
// RUN, and counted in long — what a search reports among what it did not
// read, never among what holds no match. And ctx is checked between lines:
// once it ends, Confirm runs no further line and returns its error, so a
// caller's budget holds to within one line's run, which MaxLineWork bounds.
// long counts the lines Confirm passed before it returned.
func (p *Plan) Confirm(ctx context.Context, blob []byte, hit func(Hit) bool) (long int, err error) {
	done, limit := ctx.Done(), p.LineBytes()
	line := 0
	for rest := blob; len(rest) > 0; {
		select {
		case <-done:
			return long, ctx.Err()
		default:
		}
		line++
		end, next := bytes.IndexByte(rest, '\n'), []byte(nil)
		if end < 0 {
			end = len(rest)
		} else {
			next = rest[end+1:]
		}
		text := rest[:end:end]
		rest = next
		switch {
		case !p.need.Admits(text):
			continue
		case len(text) > limit:
			long++
			continue
		}
		loc := p.re.FindIndex(text)
		if loc == nil {
			continue
		}
		if !hit(Hit{Line: line, Text: text, Start: loc[0], End: loc[1]}) {
			return long, nil
		}
	}
	return long, nil
}
