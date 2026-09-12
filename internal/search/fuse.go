package search

import "sort"

// Fuse joins two ranked lists by reciprocal rank fusion.
//
// # It consumes RANKS and never a distance, which is a decision rather than a
// convenience
//
// The two halves score in incomparable units — a BM25 weight and a cosine
// distance — and any attempt to put them on one scale is a weighting nobody
// can defend and everybody tunes. Rank fusion needs neither: a document's
// contribution from a list is 1/(k+rank), so the only thing read from either
// half is the ORDER it put things in.
//
// # What fusion absorbs, and what it does not
//
// A document absent from a list contributes NOTHING from it, so a stage-one
// drop is the LARGEST possible perturbation to that document's score rather
// than an invisible one. What fusion absorbs is the drop of a document the
// keyword half also found: it keeps its lexical term and slides down. What it
// does not absorb is the drop of a SEMANTIC-ONLY document, which leaves the
// fused list entirely — and that is precisely the class the semantic half
// exists for, the question whose answer shares no word with it.
//
// So semantic recall is NOT a lower bound on delivered quality, and the
// arithmetic says something sharper instead: at k = 60, a document present in
// BOTH lists at any rank beats a semantic-only document at rank one, because
// 2/(60+r) > 1/61 for every r below 62. A semantic-only document's best
// possible fused position is therefore already one past the overlap, and
// losing it costs the answer its best distinct entry.
func Fuse(lists ...[]string) []string {
	scores := make(map[string]float64)
	first := make(map[string]int)
	for _, list := range lists {
		for rank, id := range list {
			if _, seen := scores[id]; !seen {
				first[id] = len(scores)
			}
			scores[id] += 1 / float64(FuseK+rank+1)
		}
	}
	fused := make([]string, 0, len(scores))
	for id := range scores {
		fused = append(fused, id)
	}
	sort.SliceStable(fused, func(a, b int) bool {
		if scores[fused[a]] != scores[fused[b]] {
			return scores[fused[a]] > scores[fused[b]]
		}
		// A STABLE TIE BREAK, and it is first-seen order rather than the
		// id's own: two documents at one fused score are two documents
		// the fusion cannot separate, and ordering them by name would
		// make the answer depend on what somebody called a task.
		return first[fused[a]] < first[fused[b]]
	})
	if len(fused) > FuseCandidateCeiling {
		fused = fused[:FuseCandidateCeiling]
	}
	return fused
}

// FusedScore is one document's fused score, for a caller that needs to reason
// about a single document rather than about the order.
func FusedScore(id string, lists ...[]string) float64 {
	total := 0.0
	for _, list := range lists {
		for rank, other := range list {
			if other == id {
				total += 1 / float64(FuseK+rank+1)
				break
			}
		}
	}
	return total
}
