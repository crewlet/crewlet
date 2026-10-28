package codeindex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"slices"

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
// up. Made by NewPlan, or by a Query's Plan.
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
	seen := map[string]bool{}
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
		if seen[text] {
			continue
		}
		seen[text] = true
		c.alts = append(c.alts, alternative{text: text})
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
