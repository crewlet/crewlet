package codeindex

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"iter"
	"regexp"
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
)

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
}

// clause is a clause of the prefilter the index can look up: every literal
// of it three bytes or more in the form the index holds it.
type clause struct {
	alts []alternative
	// folded is whether any literal of it is compared folded, so that a
	// blob holding an escape satisfies it whatever its trigrams say.
	folded bool
}

// alternative is one literal of a clause, lowered as the index is, with its
// trigrams.
type alternative struct {
	text     string
	trigrams []Trigram
}

// NewPlan plans re over the index. It REFUSES a pattern that can match across
// a line or at a file's very start or end (regexlit.LineLocal), because a
// search answers lines and confirms them one at a time; and one whose every
// match can do without a literal of three bytes or more, because no trigram
// narrows it and the search would read every file — ErrNotLineLocal and
// ErrNoLiteral, each a *QueryError.
//
// re must come from regexp.Compile or regexp.MustCompile, as
// regexlit.Required's must.
func NewPlan(re *regexp.Regexp) (*Plan, error) {
	return newPlan(re, fmt.Sprintf("the pattern %q", re.String()))
}

// newPlan is NewPlan, its refusals naming the pattern as what.
func newPlan(re *regexp.Regexp, what string) (*Plan, error) {
	if !regexlit.LineLocal(re) {
		return nil, refuse(ErrNotLineLocal, "%s can match a line break, or the very start or end of a "+
			"file (\\n, \\s, a class holding a line break, . under (?s), \\A, \\z), and a code search "+
			"answers lines: keep every part of it within one line", what)
	}
	need := regexlit.Required(re)
	p := &Plan{re: re, need: need}
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
	return p, nil
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
func indexable(terms []regexlit.Term) (clause, bool) {
	var c clause
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
		c.alts = append(c.alts, alternative{text: text, trigrams: appendTrigrams(nil, []byte(text))})
	}
	return c, len(c.alts) > 0
}

// Regexp is the expression p plans.
func (p *Plan) Regexp() *regexp.Regexp { return p.re }

// Candidates is every blob of s the plan cannot rule out, ascending by id —
// and so every blob of s that holds a match, which is the plan's whole
// promise; each still has to be confirmed (Confirm). A blob is ruled out by
// a clause none of whose literals it can hold — some trigram of each missing
// from its postings — unless the clause holds a folded literal and the blob
// an escape (regexlit.HasEscape), which no lowering sees through. A skipped
// blob is never a candidate.
func (p *Plan) Candidates(s *Segment) ([]OID, error) {
	var set []uint32
	for i, c := range p.index {
		got, err := c.blobs(s)
		if err != nil {
			return nil, err
		}
		if c.folded {
			got = unionInto(got, s.escapes)
		}
		if i == 0 {
			set = got
		} else {
			set = intersect(set, got)
		}
		if len(set) == 0 {
			return nil, nil
		}
	}
	out := make([]OID, 0, len(set))
	for _, o := range set {
		if !s.Skipped(int(o)) {
			out = append(out, s.OID(int(o)))
		}
	}
	return out, nil
}

// blobs is the ordinals of the blobs of s that can hold one of c's literals.
func (c clause) blobs(s *Segment) ([]uint32, error) {
	var union []uint32
	for _, a := range c.alts {
		got, err := a.blobs(s)
		if err != nil {
			return nil, err
		}
		union = unionInto(union, got)
	}
	return union, nil
}

// blobs is the ordinals of the blobs of s that hold every trigram of a: the
// shortest list read first, each other one only to narrow it.
func (a alternative) blobs(s *Segment) ([]uint32, error) {
	lists := make([][]byte, 0, len(a.trigrams))
	for _, t := range a.trigrams {
		list, ok := s.lookup(t)
		if !ok {
			return nil, nil
		}
		lists = append(lists, list)
	}
	slices.SortFunc(lists, func(x, y []byte) int { return cmp.Compare(len(x), len(y)) })
	set, err := s.decode(lists[0], nil)
	if err != nil {
		return nil, err
	}
	var next []uint32
	for _, list := range lists[1:] {
		if len(set) == 0 {
			break
		}
		if next, err = s.decode(list, next[:0]); err != nil {
			return nil, err
		}
		set = intersect(set, next)
	}
	return set, nil
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

// Confirm is every line of blob the plan's expression matches, in order:
// RE2's answer, whichever path offered the blob. The expression runs on each
// line alone — exact, because the plan admits only a pattern whose matches lie
// within a line (regexlit.LineLocal) — and only on a line that holds what
// every match needs (regexlit.Prefilter.Admits), so a blob of thousands of
// lines runs it on the few that can match.
func (p *Plan) Confirm(blob []byte) iter.Seq[Hit] {
	return func(yield func(Hit) bool) {
		line := 0
		for rest := blob; len(rest) > 0; {
			line++
			end, next := bytes.IndexByte(rest, '\n'), []byte(nil)
			if end < 0 {
				end = len(rest)
			} else {
				next = rest[end+1:]
			}
			text := rest[:end:end]
			rest = next
			if !p.need.Admits(text) {
				continue
			}
			loc := p.re.FindIndex(text)
			if loc == nil {
				continue
			}
			if !yield(Hit{Line: line, Text: text, Start: loc[0], End: loc[1]}) {
				return
			}
		}
	}
}
