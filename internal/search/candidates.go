package search

// WHAT A CORPUS ANSWERS BEFORE ITS HITS ARE READ BACK.
//
// A caller gets the candidates, not a cut list: the index is behind the rows,
// so a key may name a document removed since, and a knowledge search never
// shows the tool-skills container. A list cut to the limit FIRST comes up short
// whenever the leaders held such a key. So a search answers its CANDIDATES
// (each method's top-[FuseN], WITH SCORES) and the caller walks
// [Candidates.Fused] past what it cannot show: a hidden document costs that
// document, never a place. The order is the fan-out's ([fuseSlices]): methods
// merged by score across slices, then reciprocal rank fusion once.

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
