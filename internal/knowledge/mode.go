package knowledge

import (
	"fmt"
	"strings"
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
	ServedMode Mode

	// Modes is what this backend can serve AS ASKED right now. A mode
	// missing from it would be answered degraded.
	Modes []Mode

	Coverage Coverage
	Degraded Degradation
}

// Result is one knowledge search's answer.
type Result struct {
	Hits []Hit
	Outcome
}
