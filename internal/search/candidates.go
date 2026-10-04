package search

// WHAT A CORPUS ANSWERS BEFORE ITS HITS ARE READ BACK.
//
// # Why a caller is handed the candidates rather than a cut list
//
// A fused answer is an ORDER, and a caller does not show every key in it: the
// index is behind the rows by design, so a key may name a document removed
// since it was indexed, and a knowledge search never shows a page of the
// tool-skills container. A list cut to the caller's limit FIRST and filtered
// after is a short list whenever the leaders included such a key — a company
// with many skills is handed a fraction of what it asked for. So a search
// answers its CANDIDATES — each method's top-[FuseN], WITH SCORES — and the
// caller walks the fused order ([Candidates.Fused]) past what it cannot show
// until its limit is filled: a hidden or removed document costs the answer
// that document and never a place.
//
// The order is the one the fan-out fused the corpus by ([fuseSlices]): each
// method merged by score across the answering slices, then reciprocal rank
// fusion once over the two lists — so the candidates a caller walks and the
// hits the fan-out itself answers are one ranking.

// Candidates is one corpus's answer to a query before its hits are read back:
// each method's top-[FuseN], best first, with scores.
type Candidates struct {
	Lexical  []Scored
	Semantic []Scored

	// SemanticSkipped says the corpus answered without its semantic half
	// — see [Slice.SemanticSkipped].
	SemanticSkipped bool
}

// Keys is every document the candidates name, each once, lexical first.
func (c Candidates) Keys() []string {
	seen := make(map[string]bool, len(c.Lexical)+len(c.Semantic))
	out := make([]string, 0, len(c.Lexical)+len(c.Semantic))
	for _, list := range [][]Scored{c.Lexical, c.Semantic} {
		for _, s := range list {
			if !seen[s.Key] {
				seen[s.Key] = true
				out = append(out, s.Key)
			}
		}
	}
	return out
}

// Fused is the candidates' one ranking, best first: each method's list by
// score, cut at [FuseN], then reciprocal rank fusion once over the two — the
// rule [fuseSlices] fuses a corpus's bucket slices by, so the order a caller
// walks is the order the fan-out answered.
func (c Candidates) Fused() []string {
	return Fuse(scoredKeys(MergeByScore([][]Scored{c.Lexical}, FuseN)),
		scoredKeys(MergeByScore([][]Scored{c.Semantic}, FuseN)))
}

// scoredKeys is a scored list's keys, in its order.
func scoredKeys(s []Scored) []string {
	ids := make([]string, 0, len(s))
	for _, one := range s {
		ids = append(ids, one.Key)
	}
	return ids
}
