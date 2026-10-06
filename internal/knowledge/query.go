package knowledge

import (
	"errors"
	"fmt"
	"strings"
)

// MaxQueryBytes bounds the text of a search, in bytes, and a longer one is
// REFUSED naming the limit, never cut.
//
// ONE BOUND FOR BOTH OF THE ENGINE'S RANKED SEARCHES — the knowledge search
// and the tracker's item search — and for every surface that takes one: a
// seat's `search_knowledge`, `search_work_items` and `answer_knowledge`, the
// operator's catalogue, the API's `knowledge` and `work_search` and the
// dashboard that asks them (`SEARCH_QUERY_MAX`), and the fan-out every native
// search of either kind passes through. Declared here because this package is
// already the two searches' shared vocabulary ([Mode], [Outcome]), and a bound
// spelled once per surface is a bound that drifts until one of them cuts.
//
// FOUR HUNDRED BYTES is a long sentence and several keywords. A query is what
// somebody is looking for, and past that it is a pasted thread or a document:
// the keyword half would rank on dozens of incidental terms, the semantic half
// would place one vector at the mean of everything pasted, and either way the
// answer is to a question nobody asked.
//
// REFUSED, NOT CUT, which is the opposite of what the corpus does with a long
// page. A page is embedded as its OPENING because one vector stands for one
// source and an opening already says what a source is about; a query is a
// question, and a search on its first part is a different search whose answer
// the asker would read as the answer to theirs. So a query never goes through
// the corpus's cut. It does not need to: every model this build knows takes at
// least 2 032 bytes an input, so a query inside this bound is never refused by
// a provider for its length either.
const MaxQueryBytes = 400

// ErrQueryTooLong is a search whose text is past [MaxQueryBytes].
var ErrQueryTooLong = errors.New("knowledge: search text past the bound")

// QueryTooLongError is [ErrQueryTooLong] with the size that was refused.
type QueryTooLongError struct {
	// Bytes is the length of the refused text, trimmed, as it was measured.
	Bytes int
}

func (e *QueryTooLongError) Error() string {
	return fmt.Sprintf("a search takes at most %d bytes and this one is %d — "+
		"search on a few keywords or a phrase, not a pasted passage",
		MaxQueryBytes, e.Bytes)
}

// Is makes a QueryTooLongError [ErrQueryTooLong].
func (e *QueryTooLongError) Is(target error) bool { return target == ErrQueryTooLong }

// CheckQuery refuses search text past [MaxQueryBytes], measured with the
// surrounding whitespace trimmed — the text every searcher actually reads.
//
// THE ONE RULE every surface applies, so "how long may a query be" has one
// answer: a surface that writes its own refusal for its own reader still asks
// this whether to refuse.
func CheckQuery(text string) error {
	if n := len(strings.TrimSpace(text)); n > MaxQueryBytes {
		return &QueryTooLongError{Bytes: n}
	}
	return nil
}
