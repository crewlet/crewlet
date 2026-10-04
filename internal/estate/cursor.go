package estate

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A PAGED GATHER'S CURSOR: each partition's own, carried together.
//
// # Why one cursor per partition rather than one for the list
//
// A partition's cursor is a position in THAT partition's order — a keyset over
// its own rows, or a position on its own log — and it means nothing to any
// other partition. A merged page takes some rows from each partition, so where
// each one resumes is a different place in each, and the only cursor that can
// say so is all of them: the partitions still holding rows, each with the
// cursor that resumes it just after the last of its rows the page took.
//
// A partition the cursor does NOT name took its last row on an earlier page and
// is not asked again; one it names with the EMPTY cursor has not been read from
// yet, because none of its rows made a page. The two are kept apart by a map's
// presence, never by a sentinel string.
//
// The token is opaque to a caller — versioned and base64url, so it survives a
// URL and a later change of shape is a new version rather than a misread — and
// it NAMES THE LIST that minted it: a cursor another list minted, or one that
// names a partition this list does not address (minted under another layout),
// is refused rather than read. Pruned by this list's partitions instead, it
// would come back as an empty, complete page — the short list that reads like
// a company with less in it.

// gatherCursorVersion prefixes every gathered cursor this build mints.
const gatherCursorVersion = "g1."

// ErrBadCursor reports a gathered cursor this build cannot read: not one it
// minted, or one a different list, or a different shape of this one, minted.
var ErrBadCursor = errors.New("estate: the cursor is not one this list minted — " +
	"page again from the start")

// gatherCursor is a gathered cursor's body: the list that minted it, and where
// each partition still holding rows resumes, by partition id.
type gatherCursor struct {
	Op    string            `json:"op"`
	Parts map[string]string `json:"parts"`
}

// encodeGatherCursor is the cursor that resumes every partition in next of the
// list op, each from its own cursor — empty when no partition has more rows.
func encodeGatherCursor(op string, next map[statelog.PartitionID]string) string {
	if len(next) == 0 {
		return ""
	}
	body := gatherCursor{Op: op, Parts: make(map[string]string, len(next))}
	for p, own := range next {
		body.Parts[p.String()] = own
	}
	raw, err := json.Marshal(body) // strings and a map of them always encode, keys sorted
	if err != nil {
		return ""
	}
	return gatherCursorVersion + base64.RawURLEncoding.EncodeToString(raw)
}

// decodeGatherCursor reads back what [encodeGatherCursor] wrote for the list
// op: where each partition it names resumes. A cursor op did not mint, or one
// that names no partition — which [encodeGatherCursor] never writes — is
// [ErrBadCursor].
func decodeGatherCursor(op, token string) (map[statelog.PartitionID]string, error) {
	body, ok := strings.CutPrefix(token, gatherCursorVersion)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrBadCursor, token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadCursor, err)
	}
	var cursor gatherCursor
	if err := json.Unmarshal(raw, &cursor); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadCursor, err)
	}
	switch {
	case cursor.Op != op:
		return nil, fmt.Errorf("%w: it pages %q, not %s", ErrBadCursor, cursor.Op, op)
	case len(cursor.Parts) == 0:
		return nil, fmt.Errorf("%w: it names no partition", ErrBadCursor)
	}
	out := make(map[statelog.PartitionID]string, len(cursor.Parts))
	for name, own := range cursor.Parts {
		p, err := statelog.ParsePartitionID(name)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBadCursor, err)
		}
		out[p] = own
	}
	return out, nil
}

// pagedPart is one partition's page of a paged list, as a merge reads it.
type pagedPart[T any] struct {
	Partition statelog.PartitionID

	// From is the partition's own cursor the page was read from.
	From string

	// Rows are the page's rows in the list's own order, and Next the
	// partition's own cursor past the last of them — empty when the
	// partition has no rows beyond them.
	Rows []T
	Next string

	// Missing is a partition that did not answer this page: it has rows
	// nobody has seen, so it resumes where it was asked from.
	Missing bool
}

// pagedParts is the pages a paged gather's partitions answered, in the shape
// [mergePaged] reads — every partition, the ones that did not answer included,
// so none of them is read as having run out of rows.
func pagedParts[P, T any](parts []PartResult[P], rows func(P) ([]T, string)) []pagedPart[T] {
	out := make([]pagedPart[T], 0, len(parts))
	for _, part := range parts {
		page := pagedPart[T]{Partition: part.Partition, From: part.Cursor, Missing: part.Missing != nil}
		if !page.Missing {
			page.Rows, page.Next = rows(part.Value)
		}
		out = append(out, page)
	}
	return out
}

// mergePaged merges each partition's page into one page of up to limit rows,
// in the list's own order (cmp), and answers where each partition resumes next.
//
// EXACT across partitions for the reason a single partition's keyset page is:
// every partition was asked for a whole page from where it left off, so the
// first limit rows of the union in the list's order are the first limit rows
// of the whole list from those points. after is the partition's own cursor
// that resumes it just past one of its rows.
//
// Where each partition resumes:
//   - it did not answer — where it was asked from, unchanged: a partition
//     missing from one page is not a partition with no rows, and reading it
//     as one would lose every row it holds from every later page;
//   - every one of its rows taken — its own next cursor, and no entry at all
//     when it has none, since it has no rows left;
//   - some taken — the cursor past the last one taken;
//   - none taken — where it was read from, unchanged.
func mergePaged[T any](pages []pagedPart[T], cmp func(a, b T) int, after func(T) string,
	limit int) ([]T, map[statelog.PartitionID]string) {

	at := make([]int, len(pages))
	var out []T
	for limit <= 0 || len(out) < limit {
		best := -1
		for i, page := range pages {
			if at[i] >= len(page.Rows) {
				continue
			}
			if best < 0 || cmp(page.Rows[at[i]], pages[best].Rows[at[best]]) < 0 {
				best = i
			}
		}
		if best < 0 {
			break
		}
		out = append(out, pages[best].Rows[at[best]])
		at[best]++
	}
	next := map[statelog.PartitionID]string{}
	for i, page := range pages {
		switch taken := at[i]; {
		case page.Missing:
			next[page.Partition] = page.From
		case taken == len(page.Rows):
			if page.Next != "" {
				next[page.Partition] = page.Next
			}
		case taken > 0:
			next[page.Partition] = after(page.Rows[taken-1])
		default:
			next[page.Partition] = page.From
		}
	}
	return out, next
}
