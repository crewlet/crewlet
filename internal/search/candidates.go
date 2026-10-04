package search

// WHAT A CORPUS ANSWERS BEFORE IT IS FUSED.
//
// # Why a gather needs the candidates rather than the answer
//
// A fused answer is an ORDER: reciprocal rank fusion has turned two rankers'
// scores into placements, and a placement means nothing beside a placement
// from another corpus. Interleaving two partitions' fused answers would rank
// by placement — a rank-1 on a weak partition above a rank-2 on a strong one —
// which is the failure [fuseSlices] is arranged against one level down. So a
// corpus that is one of several answers its CANDIDATES — each method's
// top-[FuseN], WITH SCORES — and whoever holds every corpus's candidates fuses
// them by the one rule a single corpus is fused by: each method merged by
// score, RRF once.
//
// Taking [FuseN] per corpus is exact rather than sampled for [MergeByScore]'s
// reason: the corpora are disjoint, so the global top-N per method is inside
// the union of the per-corpus top-N.
//
// # And what that does NOT yet make exact
//
// Semantic scores are comparable across corpora by construction: one model,
// one metric, one space. A BM25 score is comparable only if every corpus
// scored it against the SAME statistics — the document count, the average
// length and each term's document frequency. Inside one partition the bucket
// fan-out guarantees that, because every bucket is scanned against the whole
// partition's tables. Across partitions it holds when the statistics are
// summed over all of them first, which is the statistics round of the
// partitioned estate's search plan; until that round runs, a partition's
// lexical scores are measured against that partition's own statistics. Under
// layout 0 there is one partition and its statistics are the company's, so
// the fused order is exactly the order a single scan produces.

// Candidates is one corpus's answer to a query before fusion: each method's
// top-[FuseN], best first, with scores.
type Candidates struct {
	Lexical  []Scored `json:"lexical,omitempty"`
	Semantic []Scored `json:"semantic,omitempty"`

	// SemanticSkipped says the corpus answered without its semantic half
	// — see [Slice.SemanticSkipped].
	SemanticSkipped bool `json:"semantic_skipped,omitempty"`
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

// FuseCandidates fuses DISJOINT corpora's candidates into one ranking, best
// first: each method merged by score across them, then reciprocal rank fusion
// once over the two global lists — the rule [fuseSlices] fuses one corpus's
// bucket slices by, so one corpus and many are ranked by the same arithmetic.
func FuseCandidates(parts []Candidates) []string {
	lexical := make([][]Scored, 0, len(parts))
	semantic := make([][]Scored, 0, len(parts))
	for _, p := range parts {
		lexical = append(lexical, p.Lexical)
		semantic = append(semantic, p.Semantic)
	}
	return Fuse(scoredKeys(MergeByScore(lexical, FuseN)), scoredKeys(MergeByScore(semantic, FuseN)))
}

// scoredKeys is a scored list's keys, in its order.
func scoredKeys(s []Scored) []string {
	ids := make([]string, 0, len(s))
	for _, one := range s {
		ids = append(ids, one.Key)
	}
	return ids
}
