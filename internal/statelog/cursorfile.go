package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// CursorsInFile reads every domain's committed checkpoint out of a COPY of the
// replicated estate.
//
// # Why a position is read from the file and never from the live database
//
// The checkpoint commits in the SAME transaction as the rows it describes, so
// the position inside a file is the only position that describes that file. A
// caller that stamped its manifest from the live cursor would be writing down
// where the node was when the copy started, not where the copy actually
// finished — and a copy is taken while the applier is running, so those two
// differ by however long it took.
//
// It is the same read for both artefacts a node produces, and that is why it
// is here rather than beside either: a snapshot names its positions so a
// recipient can refuse a stale donor, and a backup names them so a restore
// knows which records the log still has to replay. Two implementations of one
// query is how one of them starts reading a column the other dropped.
//
// The file is opened as ONE ESTATE. [store.Open] would treat it as a node —
// applying the node estate's whole migration sequence into this copy of the
// replicated file and opening a second file beside it.
func CursorsInFile(ctx context.Context, path string) (map[string]Position, error) {
	db, err := store.OpenEstate(ctx, store.EstateReplicated, path, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("statelog: open %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()

	out := map[string]Position{}
	if err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT stream, generation, seq FROM statelog_cursor`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var stream string
			var generation, seq int64
			if err := rows.Scan(&stream, &generation, &seq); err != nil {
				return err
			}
			out[stream] = Position{
				Stream: stream, Generation: uint32(generation), Seq: uint64(seq),
			}
		}
		return rows.Err()
	}); err != nil {
		return nil, fmt.Errorf("statelog: read the checkpoints in %s: %w", path, err)
	}
	return out, nil
}

// CursorFor reads ONE stream's committed checkpoint out of a LIVE replicated
// estate, reporting false when this node has never committed on it.
//
// Two callers need it and both need the same three values: the loop resumes
// its broker consumer from the sequence, stamps its records with the
// generation, and detects a recreated stream from the instant. Reading them
// separately is how one of them ends up describing a different checkpoint from
// the other two.
func CursorFor(ctx context.Context, db *store.DB, stream string) (Position, time.Time, bool, error) {
	var generation, seq, created int64
	err := db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT generation, seq, stream_created_at FROM statelog_cursor WHERE stream = ?`,
			stream).Scan(&generation, &seq, &created)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Position{Stream: stream}, time.Time{}, false, nil
	case err != nil:
		return Position{}, time.Time{}, false,
			fmt.Errorf("statelog: read %s's checkpoint: %w", stream, err)
	}
	return Position{
			Stream: stream, Generation: uint32(generation), Seq: uint64(seq),
		},
		store.DecodeTime(created), true, nil
}
