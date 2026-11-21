package learning

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A seat's episodes LISTED, and one of them READ WHOLE.
//
// Two of an episode's texts have no bound a row holds to: what the turn was
// ASKED (bounded only by the delivering event) and what it DID (a review's
// account or the whole final answer). A listing shows each as one line, and
// reading them whole costs megabytes per row, enough to exceed the transport on
// one page. So a listing reads each as its OPENING with the whole text's size
// beside it ([Episodes.Listing]), and the whole row is one read of its own
// ([Episodes.Get]): the record keeps every byte, only the render is bounded.
// The opening is a PREVIEW, textcut's one case for such a cut; it carries no
// marker in its text because the size beside it is the marker.

// ListedEpisode is one episode as a listing reads it: the row, but its vector,
// with its ask and its account cut to an opening, and the size of each whole.
type ListedEpisode struct {
	Episode

	// AskBytes and PlanSummaryBytes are the whole texts' sizes in bytes;
	// Ask and PlanSummary hold at most the opening the listing asked for, so
	// a text is whole exactly when its size is its length.
	AskBytes, PlanSummaryBytes int
}

// openedColumns are what a listing reads in place of a column, by name: the
// opening of each unbounded text, one marker deeper than the cut so the marker
// a cut can land in is read whole ([openingOf]) — substr counts CHARACTERS,
// each at least one byte, so they cover the bytes and the cut is made in Go —
// and no vector, which no listing draws and which is six kilobytes a row at
// 1 536 dimensions.
var openedColumns = map[string]string{
	"plan_summary":    "substr(plan_summary, 1, ?)",
	"ask":             "substr(ask, 1, ?)",
	"embedding":       "NULL",
	"embedding_model": "NULL",
}

// episodeListingColumns is [episodeColumns], in its order — so [scanEpisode]
// reads it — with [openedColumns] in place of the columns they name, and the
// two texts' whole sizes after them. DERIVED rather than spelled again, so a
// column added to the row is read by the listing too.
var episodeListingColumns = func() string {
	columns := strings.Split(episodeColumns, ",")
	for i, column := range columns {
		name := strings.TrimSpace(column)
		if opened, ok := openedColumns[name]; ok {
			columns[i] = " " + opened
		}
	}
	return strings.Join(columns, ",") + ", octet_length(plan_summary), octet_length(ask)"
}()

// Listing returns a seat's most recent episodes, newest first — the rows
// [Episodes.Recent] returns — with each one's ask and account read only as far
// as an opening of at most opening bytes, and no vector.
//
// It ends at limit rows, or before the row that would take the text it holds
// ([textBytes]) past maxBytes, whichever comes first: the openings bound two
// texts, and the label, the outcome, the tool and skill lists and a compacted
// row's patterns are bounded by nothing a row holds to.
func (e *Episodes) Listing(ctx context.Context, handle string, limit, opening, maxBytes int) ([]ListedEpisode, error) {
	if limit <= 0 {
		limit = defaultEpisodeListing
	}
	if opening <= 0 {
		return nil, errors.New("learning: an episode listing needs an opening to read each text to")
	}
	if maxBytes <= 0 {
		return nil, errors.New("learning: an episode listing needs a byte bound to end at")
	}
	// THE BINDS IN COLUMN ORDER: the account comes before the ask in
	// episodeColumns, and both take the same read.
	read := opening + redact.LongestMarker()
	rows, err := e.db.SQL().QueryContext(ctx,
		`SELECT `+episodeListingColumns+` FROM episodes
		 WHERE agent_handle = ? ORDER BY ended_at DESC, id DESC LIMIT ?`,
		read, read, handle, limit)
	if err != nil {
		return nil, fmt.Errorf("learning: list episodes for %s: %w", handle, err)
	}
	defer func() { _ = rows.Close() }()
	var (
		out  []ListedEpisode
		held int
	)
	for rows.Next() {
		var listed ListedEpisode
		ep, err := scanEpisode(withSizes{rows: rows, sizes: []any{
			&listed.PlanSummaryBytes, &listed.AskBytes,
		}})
		if err != nil {
			return nil, fmt.Errorf("learning: scan a listed episode: %w", err)
		}
		ep.PlanSummary = openingOf(ep.PlanSummary, opening)
		ep.Ask = openingOf(ep.Ask, opening)
		listed.Episode = ep
		size := textBytes(ep)
		if !fitsBytes(len(out), held, size, maxBytes) {
			break
		}
		held += size
		out = append(out, listed)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: list episodes for %s: %w", handle, err)
	}
	return out, nil
}

// openingOf is at most n bytes of text, cut on a rune boundary and NEVER
// INSIDE A MARKER the redaction wrote: an episode's ask and account are
// redacted on the turn's completed record, and a marker cut in part reads as
// the turn's own words, so the cut moves back to where the
// markers begin ([redact.MarkersAround]). text is read n plus the longest
// marker deep, so the marker a cut at n falls in is whole in it.
func openingOf(text string, n int) string {
	kept := textcut.Bytes(text, n)
	if begin, _, inside := redact.MarkersAround(text, len(kept)); inside {
		kept = kept[:begin]
	}
	return kept
}

// withSizes is a row whose scan takes the sizes the listing reads after the
// episode's own columns.
type withSizes struct {
	rows  *sql.Rows
	sizes []any
}

func (w withSizes) Scan(dest ...any) error {
	return w.rows.Scan(append(dest, w.sizes...)...)
}

// Get returns one of a seat's episodes whole, and false when the seat has no
// episode of that id — the lifecycle dropped or folded it, or it is another
// seat's, which a read of this seat's memory must not answer with.
func (e *Episodes) Get(ctx context.Context, handle, id string) (Episode, bool, error) {
	ep, err := scanEpisode(e.db.SQL().QueryRowContext(ctx,
		`SELECT `+episodeColumns+` FROM episodes WHERE agent_handle = ? AND id = ?`,
		handle, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Episode{}, false, nil
	case err != nil:
		return Episode{}, false, fmt.Errorf("learning: read episode %s of %s: %w", id, handle, err)
	}
	return ep, true, nil
}
