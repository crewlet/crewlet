package learning

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/textcut"
)

// A seat's episodes LISTED, and one of them READ WHOLE.
//
// Two of an episode's texts are bounded by nothing a row holds to: what the
// turn was ASKED, stored whole and bounded only by the event that delivered it,
// and what it DID, a review's account or the turn's whole final answer. A
// listing shows each as one line, and a listing that read them whole read
// megabytes from the store, and sent them across the fleet, to draw one — and
// fifty of them, one page, could be more than the transport carries, so the
// page itself was refused. So a listing reads each as its OPENING with the
// whole text's size beside it ([Episodes.Listing]), and the whole row is one
// read of its own ([Episodes.Get]): the record keeps every byte, and only the
// render is bounded. The opening is a PREVIEW of what the reader can open,
// which is textcut's one case for such a cut; it carries no marker in its text
// because the size beside it is the marker, and a screen draws it from that
// rather than from a character that would read as the turn's own.

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
// opening of each unbounded text — substr counts CHARACTERS, each at least one
// byte, so the opening's characters cover its bytes and the cut to bytes is
// made in Go, on a rune boundary — and no vector, which no listing draws and
// which is six kilobytes a row at 1 536 dimensions.
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
func (e *Episodes) Listing(ctx context.Context, handle string, limit, opening int) ([]ListedEpisode, error) {
	if limit <= 0 {
		limit = defaultEpisodeListing
	}
	if opening <= 0 {
		return nil, errors.New("learning: an episode listing needs an opening to read each text to")
	}
	// THE BINDS IN COLUMN ORDER: the account comes before the ask in
	// episodeColumns, and both take the same opening.
	rows, err := e.db.SQL().QueryContext(ctx,
		`SELECT `+episodeListingColumns+` FROM episodes
		 WHERE agent_handle = ? ORDER BY ended_at DESC, id DESC LIMIT ?`,
		opening, opening, handle, limit)
	if err != nil {
		return nil, fmt.Errorf("learning: list episodes for %s: %w", handle, err)
	}
	defer func() { _ = rows.Close() }()
	var out []ListedEpisode
	for rows.Next() {
		var listed ListedEpisode
		ep, err := scanEpisode(withSizes{rows: rows, sizes: []any{
			&listed.PlanSummaryBytes, &listed.AskBytes,
		}})
		if err != nil {
			return nil, fmt.Errorf("learning: scan a listed episode: %w", err)
		}
		ep.PlanSummary = textcut.Bytes(ep.PlanSummary, opening)
		ep.Ask = textcut.Bytes(ep.Ask, opening)
		listed.Episode = ep
		out = append(out, listed)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: list episodes for %s: %w", handle, err)
	}
	return out, nil
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
