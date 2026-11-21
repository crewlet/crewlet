package knowledge

import (
	"errors"
	"fmt"
	"strings"
)

// MaxQueryBytes bounds the text of a search, in bytes, and a longer one is
// REFUSED naming the limit, never cut.
//
// ONE BOUND FOR EVERY RANKED SEARCH (knowledge, tracker items, chat messages)
// and every surface taking one, the dashboard's `SEARCH_QUERY_MAX` included:
// spelled per surface it drifts until one cuts. Chat shares only the bound
// (its corpus is its own, ADR-0071). FOUR HUNDRED BYTES is a long sentence and
// several keywords; past that it is a pasted thread. REFUSED, NOT CUT,
// unlike a long page: a search on a query's first part is a different search
// the asker would read as theirs. So the vector is the whole query's: one
// input on every model this build knows (each takes at least 2 032 bytes),
// pooled pieces on a narrower stated one (internal/search's queryEmbedding).
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
