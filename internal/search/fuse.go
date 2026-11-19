package search

import "sort"

// Fuse joins two ranked lists by reciprocal rank fusion.
//
// It consumes RANKS, never a distance: the halves score in incomparable units
// (a BM25 weight, a cosine distance), and one scale is a weighting nobody can
// defend. A list contributes 1/(k+rank), and a document absent from it NOTHING,
// so a stage-one drop is the LARGEST perturbation to its score. Fusion absorbs
// the drop of a document the keyword half also found, which slides down; not
// that of a SEMANTIC-ONLY document, which leaves the fused list: exactly the
// class the semantic half exists for. So semantic recall is NOT a lower bound
// on delivered quality: at k = 60 a document in BOTH lists at any rank beats a
// semantic-only one at rank one (2/(60+r) > 1/61 for every r below 62).
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
