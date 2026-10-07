package learning

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// What the node holding a seat FILLS: the rows of the seat's memory that have
// no vector of the current model at this store's width, and the vectors it
// stores on them.
//
// ONE SHAPE for every table filled, so the holder's loop (internal/engine)
// walks a seat's diary and its episodes alike and holds both to one request
// discipline (embeddings.Pass): each table answers what its rows' vectors are
// OF ([Unfilled.Text]) and stores what came back ([VectorFill]), and nothing
// else about it is the loop's business.

// FillCursor is where a fill's walk over one seat's rows stopped.
//
// A table is walked NEWEST FIRST, a page at a time, each page strictly after
// the last row of the one before. A row the provider refused is not changed by
// the pass that refused it, so read from the top every time it would sit in the
// first page of every read and take a slot of every page's limit; walked past,
// it is read once a pass, and a page always holds rows the pass has not seen.
type FillCursor struct {
	// At is the row's place in the order — a note's created_at, an episode's
	// ended_at — and ID breaks a tie at one instant. The zero cursor is the
	// top of the table.
	At time.Time
	ID string
}

// Unfilled is one row with no vector of the current model at this store's
// width, as the holder's fill sends it.
type Unfilled struct {
	ID string

	// Text is exactly what the row's vector is of — the same function of the
	// stored row its writer embedded — so a filled vector is the vector the
	// row would have had.
	Text string

	// Cursor is this row's place in the walk: the cursor to pass for the
	// page after it.
	Cursor FillCursor
}

// VectorFill is one row's vector, to store with a table's FillEmbeddings.
type VectorFill struct {
	ID     string
	Vector Vector
}

// cursorArgs are the bind values for a page's "strictly after this cursor"
// predicate: `(? = 0 OR col < ? OR (col = ? AND id < ?))`.
func cursorArgs(after FillCursor) []any {
	if after.At.IsZero() {
		return []any{0, 0, 0, ""}
	}
	at := store.EncodeTime(after.At)
	return []any{1, at, at, after.ID}
}

// The fill statements, one per filled table and each naming its table, so no
// reader of the source — and no gate reading it for writes to the replicated
// estate — has to work out which table a fill writes.
//
// A ROW ALREADY HOLDING A VECTOR OF THE SAME MODEL AT THIS WIDTH IS LEFT ALONE
// ([staleVector]): two passes, or a pass racing the row's own write, fill it
// once. An episode's fill touches raw rows only, the rows recall searches.
const (
	staleVector = ` AND (embedding IS NULL OR embedding_model IS NULL
		OR embedding_model <> ? OR length(embedding) <> ?)`

	diaryFillSQL = `UPDATE agent_diary SET embedding = ?, embedding_model = ?
		WHERE id = ?` + staleVector

	episodeFillSQL = `UPDATE episodes SET embedding = ?, embedding_model = ?
		WHERE id = ? AND kind = 'raw'` + staleVector
)

// fillVectors stores vectors on rows of table through statement — one of the
// fill statements above — in one transaction, and reports how many it stored.
//
// A row already holding a vector of the same model at this width is not
// counted. Setting the vector stamps the row with a fresh change sequence —
// the table's own trigger (node migration 0041) — which is what carries it to
// the seat's next holder.
//
// The vector rule is the writers' own ([encodeVectorColumns]): a vector of the
// wrong width fails the whole fill — a provider answering another width is a
// configuration fault, and every vector beside it is suspect — while a
// non-finite one, or one naming no model, is skipped and the row stays
// unfilled.
func fillVectors(ctx context.Context, db *store.DB, statement, table, discarded string,
	fills []VectorFill,
) (int, error) {
	if len(fills) == 0 {
		return 0, nil
	}
	filled := 0
	err := db.Tx(ctx, func(tx *sql.Tx) error {
		filled = 0
		for _, fill := range fills {
			blob, model, err := encodeVectorColumns(db, fill.Vector.Values,
				fill.Vector.Model, discarded)
			if err != nil {
				return fmt.Errorf("learning: fill %s row %s: %w", table, fill.ID, err)
			}
			if blob == nil {
				continue
			}
			res, err := tx.ExecContext(ctx, statement,
				blob, model, fill.ID, model, 4*len(fill.Vector.Values))
			if err != nil {
				return fmt.Errorf("learning: fill %s row %s: %w", table, fill.ID, err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("learning: fill %s row %s: %w", table, fill.ID, err)
			}
			filled += int(n)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return filled, nil
}
