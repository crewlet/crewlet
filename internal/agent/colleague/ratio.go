package colleague

import (
	"slices"
	"strings"
)

// ratio implements difflib's SequenceMatcher.ratio — the gestalt
// pattern-matching similarity measure.
//
// IMPLEMENTED RATHER THAN SUBSTITUTED, and the reason is the cutoff. FuzzyCutoff is
// 0.6 on THIS measure — a number that was tuned against real role names by
// watching which near-misses it let through. Swapping in Levenshtein or
// Jaro-Winkler, both of which Go has libraries for, would keep the constant
// and silently change what it means: 0.6 is a different set of pairs under
// every metric, and the failure would be a lookup that started guessing wrong
// colleagues with nothing in the diff to explain it.
//
// The measure: 2·M / T, where M is the total length of the matching blocks
// found by recursively taking the LONGEST contiguous common run and repeating
// on what lies either side of it, and T is the combined length. So it rewards
// contiguity — "engineering" against "engineer" scores far above the same
// characters scattered — which is what makes it right for names typed from
// memory rather than mistyped.
//
// difflib's "autojunk" heuristic (ignore elements appearing in more than 1% of
// a sequence of 200+) is deliberately not implemented: it engages only at 200
// elements, and a handle or role name that long is not a thing this corpus
// holds. Implementing a branch that can never be taken would be a claim the
// next reader has to verify.
func ratio(a, b string) float64 {
	ar, br := []rune(a), []rune(b)
	total := len(ar) + len(br)
	if total == 0 {
		// TWO EMPTY STRINGS ARE IDENTICAL, and difflib says 1.0 rather
		// than dividing by zero. Unreachable from Resolve — tier 4 needs
		// a query of at least MinFuzzyQuery runes — but a primitive that
		// disagrees with the measure it implements anywhere is a
		// disagreement waiting for a caller, and this one was found by
		// differential-testing 600 random pairs against a reference
		// implementation.
		return 1
	}
	return 2 * float64(matched(ar, br)) / float64(total)
}

// matched sums the matching blocks between two rune slices.
//
// Iterative rather than recursive, over an explicit stack of the ranges still
// to examine. Not for speed: the recursion depth is bounded by the number of
// blocks, which is bounded by the shorter string — but an unbounded-looking
// recursion over caller-supplied text is the shape that invites a reader to
// wonder, and a stack answers the question in the code.
func matched(a, b []rune) int {
	type span struct{ alo, ahi, blo, bhi int }
	stack := []span{{0, len(a), 0, len(b)}}
	total := 0

	// b's index, built once. difflib builds this per SequenceMatcher and
	// reuses it across every find_longest_match on the same b.
	index := map[rune][]int{}
	for j, r := range b {
		index[r] = append(index[r], j)
	}

	for len(stack) > 0 {
		s := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if s.alo >= s.ahi || s.blo >= s.bhi {
			continue
		}
		i, j, size := longestMatch(a, index, s.alo, s.ahi, s.blo, s.bhi)
		if size == 0 {
			continue
		}
		total += size
		// Either side of the block, exactly as difflib recurses.
		stack = append(stack, span{s.alo, i, s.blo, j})
		stack = append(stack, span{i + size, s.ahi, j + size, s.bhi})
	}
	return total
}

// longestMatch finds the longest contiguous common run inside a window,
// returning its start in each sequence and its length.
//
// EARLIEST wins a tie — the first maximal block by position in a, then in b.
// That is difflib's rule and it is what makes the whole ratio deterministic:
// with ties broken differently the recursion splits at a different place and
// the totals can differ for the same pair of strings.
func longestMatch(a []rune, index map[rune][]int, alo, ahi, blo, bhi int) (int, int, int) {
	besti, bestj, bestsize := alo, blo, 0

	// j2len[j] is the length of the run ending at b[j] for the PREVIOUS i.
	// Rebuilt each row, which is what keeps the runs contiguous in a as
	// well as in b.
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range index[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}
	return besti, bestj, bestsize
}

// sortByHandle orders candidates alphabetically, for a stable disambiguation
// list: the org's enumeration order is not something a reader should be able
// to notice, and a list that reshuffles between two identical queries makes an
// agent's "which of these did you mean" unanswerable.
func sortByHandle(c []Candidate) []Candidate {
	slices.SortFunc(c, func(x, y Candidate) int {
		return strings.Compare(x.Seat.Handle, y.Seat.Handle)
	})
	return c
}

func sortEntries(e []entry) {
	slices.SortFunc(e, func(x, y entry) int {
		return strings.Compare(x.seat.Handle, y.seat.Handle)
	})
}
