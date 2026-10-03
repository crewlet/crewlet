package knowledge

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// # Three modes, one vocabulary
//
// A search ranks by the WORDS a query used (keyword), by what it MEANS
// (semantic), or by both fused (hybrid). The same three values name the
// knowledge search and the tracker's ranked item search, on every surface —
// the query kinds, the tool layer and the dashboard, whose label for
// `semantic` is "Meaning" — so a mode is never spelled two ways for one
// ranker.
//
// # A mode asked for is not always a mode served, and the answer says which
//
// A company with no embeddings provider has nothing to rank by meaning with,
// and Confluence ranks by its own CQL search and nothing else. Neither is an
// error: a search is best effort by contract. What is NOT allowed is serving
// a different ranking than was asked for and saying nothing — a keyword
// answer presented as a hybrid one is a reader concluding that no page means
// what they typed. So every answer carries an [Outcome]: the mode it actually
// SERVED, the modes this backend could serve right now, and — when those
// differ from what was asked — a [Degradation] naming why.
//
// The two degradations differ in what they serve. HYBRID FALLS BACK to its
// keyword half, because the words are half of what was asked for and an
// answer from them is still an answer. SEMANTIC DOES NOT: somebody who asked
// for meaning asked precisely for the pages that share no word with the
// query, and a keyword answer is the one ranking guaranteed not to find them.
// It answers nothing, and says why.

// Mode is how a search ranks.
type Mode string

// The three modes.
const (
	// ModeHybrid fuses the keyword and semantic rankings by reciprocal
	// rank fusion. The default, and the zero value's meaning.
	ModeHybrid Mode = "hybrid"

	// ModeKeyword ranks by the words the query used and nothing else.
	ModeKeyword Mode = "keyword"

	// ModeSemantic ranks by meaning and nothing else.
	ModeSemantic Mode = "semantic"
)

// Modes is every mode, in the order a surface offers them.
var Modes = []Mode{ModeHybrid, ModeKeyword, ModeSemantic}

// Valid reports whether m is a mode this build knows. The zero value is
// valid: it is [ModeHybrid], the default.
func (m Mode) Valid() bool {
	switch m {
	case "", ModeHybrid, ModeKeyword, ModeSemantic:
		return true
	}
	return false
}

// Resolved is the mode a search runs in: the zero value is hybrid.
//
// A METHOD rather than a constructor default, so every search that names no
// mode — the prefetch, a seat's tools, a caller written before modes existed
// — gets the same answer from the one place that decides it.
func (m Mode) Resolved() Mode {
	if m == "" {
		return ModeHybrid
	}
	return m
}

// Lexical reports whether this mode ranks by the words.
func (m Mode) Lexical() bool { return m.Resolved() != ModeSemantic }

// Semantic reports whether this mode ranks by meaning.
func (m Mode) Semantic() bool { return m.Resolved() != ModeKeyword }

// ParseMode reads a mode off the wire. Empty is hybrid; anything else this
// build does not know is refused, naming the values it does — a mode that
// silently fell back to the default would answer a different question than
// the one the caller asked.
func ParseMode(s string) (Mode, error) {
	m := Mode(strings.TrimSpace(s))
	if !m.Valid() {
		return "", fmt.Errorf("knowledge: unknown search mode %q — the modes "+
			"are hybrid, keyword and semantic", s)
	}
	return m.Resolved(), nil
}

// Degradation says why a search served less than it was asked for. Empty is
// "it served what was asked".
type Degradation string

// The degradations.
const (
	// NotDegraded — the answer is the ranking that was asked for.
	NotDegraded Degradation = ""

	// DegradedNoEmbeddings — this company has no embeddings provider (or
	// `knowledge.vectors` is off), so there is nothing to rank by meaning
	// with. A hybrid search served its keyword half; a semantic one served
	// nothing. The remedy is configuration, and it is the operator's.
	DegradedNoEmbeddings Degradation = "no_embeddings"

	// DegradedEmbeddingFailed — a provider IS configured, and the query's
	// own vector could not be computed this time: the provider failed or
	// did not answer inside the budget. Transient, and nothing to
	// configure; the next search asks again.
	DegradedEmbeddingFailed Degradation = "embedding_failed"

	// DegradedSemanticPartial — the semantic ranking ran, and part of the
	// fleet answered without it: a participant whose vector scan failed.
	// The words were ranked over everything that was scanned; meaning was
	// ranked over less.
	DegradedSemanticPartial Degradation = "semantic_partial"

	// DegradedUnsupported — this BACKEND has no such ranker at all. It is
	// Confluence's answer to anything but keyword, and it is not
	// no_embeddings because the remedy differs: a provider would not add
	// meaning to a vendor's own search.
	DegradedUnsupported Degradation = "unsupported"
)

// Valid reports whether d is a degradation this build emits.
func (d Degradation) Valid() bool {
	switch d {
	case NotDegraded, DegradedNoEmbeddings, DegradedEmbeddingFailed,
		DegradedSemanticPartial, DegradedUnsupported:
		return true
	}
	return false
}

// Coverage is which part of the fleet an answer was assembled from.
//
// THE SHAPE EVERY FLEET ANSWER CARRIES — `nodes`, `complete` — plus the one
// figure a bucket-divided search has and a history read does not: how many of
// the corpus's buckets went unscanned. A search divides CPU rather than data
// (every node holds the whole corpus), so a node that did not answer costs
// the answer a slice of RELEVANCE, and that is only visible if it is said. A
// short result set is otherwise indistinguishable from a short corpus.
//
// A backend that is one live service (Confluence) has no division to report:
// no nodes, and complete when the call succeeded.
type Coverage struct {
	// Nodes is every participant the search was divided across, sorted by
	// id. Never nil on the wire.
	Nodes []NodeCoverage `json:"nodes"`

	// Complete is true only when every bucket was scanned.
	Complete bool `json:"complete"`

	// BucketsMissing is how many of the corpus's buckets no answer
	// covered. Zero when complete.
	BucketsMissing int `json:"buckets_missing"`
}

// NodeCoverage is one participant's part in an answer.
type NodeCoverage struct {
	ID       string `json:"id"`
	Answered bool   `json:"answered"`

	// Error says why a participant did not cover its range, in words an
	// operator can act on. Empty when it answered.
	Error string `json:"error"`
}

// Outcome is what a ranked search actually did: the mode it served, the modes
// this backend can serve, what it covered, and why it served less than was
// asked for, if it did.
//
// ONE TYPE for the knowledge search and the tracker's item search, because
// both are the same fan-out underneath and a screen renders both answers with
// one mode control.
type Outcome struct {
	// ServedMode is the ranking the hits came from. EMPTY when nothing
	// ran — a semantic search with nothing to rank by meaning with — and
	// never a mode that did not produce the hits.
	ServedMode Mode `json:"served_mode"`

	// Modes is what this backend can serve AS ASKED right now. A mode
	// missing from it would be answered degraded.
	Modes []Mode `json:"modes"`

	Coverage Coverage    `json:"coverage"`
	Degraded Degradation `json:"degraded"`

	// Partitions is what a search answered PARTITION BY PARTITION covered —
	// the engine's own corpus, which a partitioned estate divides between
	// the data nodes holding it. Coverage above is the CPU division of one
	// copy (a node that did not scan its buckets); this is the DATA
	// division (a partition no holder answered for), and it is the one a
	// short list cannot be told apart from: a partition that did not
	// answer is a part of the corpus this answer never searched. A caller
	// renders a non-empty [statelog.Coverage.Missing] as its
	// [statelog.Coverage.Notice], never as the shorter list alone —
	// "nothing matched" and "part of the knowledge base was not searched"
	// send a seat to different places.
	//
	// THE ZERO VALUE for a backend with no partitions — Confluence, whose
	// one corpus is somebody else's — which states nothing missing.
	Partitions statelog.Coverage `json:"partitions,omitzero"`
}

// Result is one knowledge search's answer.
type Result struct {
	Hits []Hit
	Outcome
}

// MergeOutcomes is what one search answered by several DISJOINT corpora did,
// from each corpus's own outcome — the partitions of a divided estate, each
// ranked by its own holders and fused where they are all held.
//
// THE MODE SERVED IS THE FULLEST ANY CORPUS SERVED, and the difference is
// said rather than averaged away: a corpus that ranked without meaning (its
// query vector failed there) beside one that ranked by it is a semantic
// ranking over part of the corpus, which is [DegradedSemanticPartial] — the
// same words main's bucket fan-out uses for one participant whose vector scan
// failed, one level up. Where no corpus ranked by meaning the answer carries
// the first reason any of them gave, which is the configuration's (every
// holder runs the same company) or one provider failure.
//
// MODES ARE THE ONES EVERY CORPUS OFFERS, because a mode one holder cannot
// serve as asked is a mode the fused answer cannot either; COVERAGE IS SUMMED
// — a participant that covered its range everywhere it was asked is answered,
// one that failed anywhere is named with the first reason it gave, and the
// buckets no answer covered are counted across the corpora. [Outcome.Partitions]
// is left for whoever gathered the corpora to state, since only it knows which
// did not answer at all.
//
// One outcome is returned unchanged, so a search over a single corpus — every
// search under the single-partition layout — answers exactly what that
// corpus's ranking did.
func MergeOutcomes(parts []Outcome) Outcome {
	switch len(parts) {
	case 0:
		return Outcome{Coverage: Coverage{Nodes: []NodeCoverage{}}}
	case 1:
		return parts[0]
	}
	out := Outcome{Coverage: Coverage{Complete: true}}
	semantic, partial := false, false
	for i, p := range parts {
		if i == 0 {
			out.Modes = slices.Clone(p.Modes)
		} else {
			out.Modes = slices.DeleteFunc(out.Modes, func(m Mode) bool {
				return !slices.Contains(p.Modes, m)
			})
		}
		if p.ServedMode != "" && p.ServedMode.Semantic() {
			semantic = true
		}
		if p.Degraded == DegradedSemanticPartial {
			partial = true
		}
		out.Coverage.Complete = out.Coverage.Complete && p.Coverage.Complete
		out.Coverage.BucketsMissing += p.Coverage.BucketsMissing
	}
	for _, p := range parts {
		switch {
		case semantic && p.ServedMode != "" && p.ServedMode.Semantic():
			out.ServedMode = p.ServedMode
		case semantic:
			// A corpus that ranked without meaning beside one that
			// ranked by it.
			partial = true
		case out.ServedMode == "":
			out.ServedMode = p.ServedMode
		}
		if out.Degraded == NotDegraded && !semantic {
			out.Degraded = p.Degraded
		}
	}
	if semantic && partial {
		out.Degraded = DegradedSemanticPartial
	}
	out.Coverage.Nodes = mergeNodes(parts)
	return out
}

// mergeNodes is every participant across the corpora, once, sorted by id:
// answered where it answered every range it was asked for, otherwise named
// with the first reason it gave.
func mergeNodes(parts []Outcome) []NodeCoverage {
	byID := map[string]NodeCoverage{}
	for _, p := range parts {
		for _, n := range p.Coverage.Nodes {
			prior, seen := byID[n.ID]
			switch {
			case !seen:
				byID[n.ID] = n
			case prior.Answered && !n.Answered:
				byID[n.ID] = n
			}
		}
	}
	out := make([]NodeCoverage, 0, len(byID))
	for _, n := range byID {
		out = append(out, n)
	}
	slices.SortFunc(out, func(a, b NodeCoverage) int { return strings.Compare(a.ID, b.ID) })
	return out
}
