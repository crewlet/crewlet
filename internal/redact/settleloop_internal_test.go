package redact

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

// SettledTwoWays is [Settled] of text and what a loop resolving one hold a
// pass answers, the oracle [heldLines.settled]'s sweep is held to, the text's
// holds read once for both.
func SettledTwoWays(text string) (swept, looped int) {
	h, _ := holding(text)
	return h.settled(), h.settledByLoop()
}

// HeldTexts are the package's internal texts [Settled] is held to, for its
// external tests: every shape of [gutterShapes] and [seamShapes], and lines of
// each of [passwordKeyGroups].
func HeldTexts() map[string]string {
	texts := map[string]string{}
	for _, c := range append(gutterShapes(), seamShapes()...) {
		texts["gutter shape "+c.name] = c.text
	}
	for name, group := range passwordKeyGroups {
		texts[name] = linesOf(group, 32<<10, 256)
	}
	return texts
}

// settledByLoop is the loop [heldLines.settled] replaced: the keys' holds to
// their fixed point, which is the rewritable point, then every hold, a claim
// beside a block lowering the rewritable point, until no hold moves the
// settled point.
func (h heldLines) settledByLoop() int {
	whole, end := h.whole, len(h.whole)
	keys := func(settled int) (int, bool) {
		moved := false
		if h.open >= 0 && settled > lineStart(whole, h.open) {
			settled, moved = lineStart(whole, h.open), true
		}
		if h.back >= 0 && settled > lineStart(whole, h.back) {
			settled, moved = lineStart(whole, h.back), true
		}
		for _, s := range h.spans {
			if s[0] < settled && settled < s[1] {
				settled, moved = lineStart(whole, s[0]), true
			}
		}
		return settled, moved
	}
	settled := end
	for moved := true; moved; {
		settled, moved = keys(settled)
	}
	rewritable := settled
	for {
		var moved bool
		settled, moved = keys(settled)
		for _, v := range h.claims {
			if v.decided < settled {
				continue
			}
			ls := lineStart(whole, v.start)
			for _, s := range h.spans {
				if s[1] > ls && s[1] <= v.start {
					rewritable = min(rewritable, lineStart(whole, s[0]))
					if settled > lineStart(whole, s[0]) {
						settled, moved = lineStart(whole, s[0]), true
					}
				}
			}
		}
		for _, v := range h.holds {
			if v.start < settled && v.decided >= settled && (!v.unread || v.decided >= rewritable) {
				settled, moved = lineStart(whole, v.start), true
			}
		}
		if !moved {
			return settled
		}
	}
}

// THE SWEEP SETTLES AT THE LARGEST LINE START NO BAR STANDS OVER, an unread
// hold's bar counted at the rewritable point of that line start (the package
// doc's "Settled in one pass"), and the claims' pairs are exactly the blocks
// ending on a claim's key's line before the key: over random lines, bars,
// pairs, spans and claims, each against a reading of every line start.
//
// Mutation: wake no unread bar passed before a pair lowered the rewritable
// point, or take a block ending at the key's line start as ending on it, and
// the sweep or the pairs come out otherwise.
func TestTheSweepSettlesWhereNoBarStandsAtItsRewritablePoint(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1666, 5))
	for trial := range 20000 {
		var b strings.Builder
		for range 1 + r.IntN(12) {
			b.WriteString(strings.Repeat("x", r.IntN(4)) + "\n")
		}
		h := heldLines{whole: b.String()}
		end := len(h.whole)
		at := func() int { return r.IntN(end + 1) }
		var bars, pairs []bar
		for range r.IntN(6) {
			lo := at()
			bars = append(bars, bar{lo: lo, hi: lo + r.IntN(end-lo+1), unread: r.IntN(2) == 0})
		}
		for range r.IntN(3) {
			pairs = append(pairs, bar{lo: lineStart(h.whole, at()), hi: at()})
		}
		r0 := lineStart(h.whole, at())
		if r.IntN(3) == 0 {
			r0 = end
		}
		slices.SortFunc(bars, byHi)
		slices.SortFunc(pairs, byHi)
		want := end
		for {
			rw := r0
			for _, p := range pairs {
				if p.hi >= want {
					rw = min(rw, p.lo)
				}
			}
			if want == 0 || !slices.ContainsFunc(bars, func(b bar) bool { return b.lo < want && want <= b.hi && (!b.unread || b.hi >= rw) }) {
				break
			}
			want = lineStart(h.whole, want-1)
		}
		if got := h.sweep(bars, end, r0, pairs); got != want {
			t.Fatalf("trial %d: swept to %d, where the largest line start no bar stands over is %d: lines %q, bars %v, pairs %v, rewritable %d",
				trial, got, want, h.whole, bars, pairs, r0)
		}

		h.spans = nil
		for s := 0; s < end; {
			s0 := s + r.IntN(end-s)
			s1 := s0 + 1 + r.IntN(end-s0)
			h.spans = append(h.spans, [2]int{s0, s1})
			s = s1 + r.IntN(3)
		}
		h.claims = nil
		for range r.IntN(4) {
			start := at()
			h.claims = append(h.claims, valueHold{start: start, decided: start + r.IntN(end-start+1)})
		}
		var wantPairs []bar
		for _, v := range h.claims {
			ls := lineStart(h.whole, v.start)
			for _, s := range h.spans {
				if s[1] > ls && s[1] <= v.start {
					wantPairs = append(wantPairs, bar{lo: lineStart(h.whole, s[0]), hi: v.decided})
				}
			}
		}
		// Pairs of one hi may come in any order.
		byHiThenLo := func(a, b bar) int { return cmp.Or(byHi(a, b), cmp.Compare(a.lo, b.lo)) }
		got := h.claimPairs()
		if !slices.IsSortedFunc(got, byHi) {
			t.Fatalf("trial %d: pairs %v, not in descending hi", trial, got)
		}
		slices.SortFunc(got, byHiThenLo)
		slices.SortFunc(wantPairs, byHiThenLo)
		if !slices.Equal(got, wantPairs) {
			t.Fatalf("trial %d: pairs %v, where the blocks ending on a claim's key's line before the key make %v: lines %q, spans %v, claims %v",
				trial, got, wantPairs, h.whole, h.spans, h.claims)
		}
	}
}
