package statelog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// FileCursor is one stream's committed checkpoint AS A COPY KEEPS IT: where
// the applier had got to, and which stream it was applying. The identity
// travels with the position for the reason [CursorFor] gives: a position out
// of the file paired with an identity out of the live broker handle names two
// streams once one is recreated, and still passes every check a recipient
// makes.
type FileCursor struct {
	// Position is the checkpoint the copy committed.
	Position Position

	// StreamCreatedAt is the creation instant ([StreamCreatedAt]) of the
	// stream the applier was reading at that position. Zero only where the
	// applier had none ([RunnerDeps].StreamCreatedAt's zero, which the engine
	// never passes): a claim about nothing, not a stream created at the epoch.
	StreamCreatedAt time.Time

	// AppliedVersion is the highest record version the copy's rows on the
	// stream were applied from — zero where they were applied from none.
	// Every writer of the row sets it, so a row holding none is refused
	// rather than read as some version.
	AppliedVersion int
}

// CursorsInFile reads every domain's committed checkpoint out of a COPY of the
// replicated estate.
//
// The position comes from the file, never the live database: the checkpoint
// commits with the rows it describes, so only the file's own position
// describes the file, while the live cursor moves on during the copy.
// Snapshots (so a recipient can refuse a stale donor) and backups (so a
// restore knows what to replay) share this one read, so neither reads a column
// the other dropped. The file is opened as ONE ESTATE: [store.OpenNode] would
// apply the node estate's migrations into this replicated copy.
func CursorsInFile(ctx context.Context, path string) (map[string]FileCursor, error) {
	db, err := store.OpenEstate(ctx, store.EstateReplicated, path, store.Options{})
	if err != nil {
		return nil, fmt.Errorf("statelog: open %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()
	out, err := Cursors(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("statelog: read the checkpoints in %s: %w", path, err)
	}
	return out, nil
}

// Cursors reads every domain's committed checkpoint out of one replicated
// estate, keyed by stream: [CursorsInFile]'s read over a handle the caller
// already holds (a backup pins the live estate's positions before copying it).
// Only logs with a row appear: a log's first apply writes it, so a caller
// naming every log places an absent one at the zero position itself.
func Cursors(ctx context.Context, db ReadEstate) (map[string]FileCursor, error) {
	out := map[string]FileCursor{}
	if err := db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT stream, generation, seq, stream_created_at, applied_version
			FROM statelog_cursor`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var stream string
			var generation, seq, created, applied int64
			if err := rows.Scan(&stream, &generation, &seq, &created, &applied); err != nil {
				return err
			}
			out[stream] = FileCursor{
				Position: Position{
					Stream: stream, Generation: uint32(generation), Seq: uint64(seq),
				},
				StreamCreatedAt: store.DecodeTime(created),
				AppliedVersion:  int(applied),
			}
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// CursorFor reads ONE stream's committed checkpoint out of the LIVE replicated
// estate, reporting false when this node has never committed on it.
//
// The three values come from one read: the loop resumes its consumer from the
// sequence, stamps records with the generation and detects a recreated stream
// from the instant, and reading them separately lets one describe a different
// checkpoint from the other two. [CheckpointOf] is the same read with the
// record the checkpoint names beside it.
func CursorFor(ctx context.Context, db ReadEstate, stream string) (Position, time.Time, bool, error) {
	cp, found, err := CheckpointOf(ctx, db, stream)
	return cp.At, cp.KeyedTo, found, err
}

// ReadEstate is the replicated estate as a caller that only reads it holds it:
// the runtime's [store.ReplicatedReader], resolved on every call, or a copy's
// [store.DB] a caller opened for the length of one read. A caller holding the
// write handle hands its Reader, so that what can write the estate stays the
// short list internal/store's applier gate reads.
type ReadEstate interface {
	Read(ctx context.Context, fn func(*sql.Tx) error) error
}

// Checkpoint is one stream's committed checkpoint row as a live estate holds
// it: the position, the creation instant of the stream its rows are keyed to,
// and the broker's instant for the record at the position — the one this node
// consumed there, zero where that is unknown.
type Checkpoint struct {
	At       Position
	KeyedTo  time.Time
	StoredAt time.Time
}

// CheckpointOf reads ONE stream's committed checkpoint row, whole, out of the
// LIVE replicated estate, reporting false when this node has never committed
// on it.
//
// ONE READ for what a verification of the checkpoint's record needs
// ([CheckpointDiverged]): a sequence from one read paired with the instant of
// another's names no record this node ever consumed.
func CheckpointOf(ctx context.Context, db ReadEstate, stream string) (Checkpoint, bool, error) {
	var generation, seq, created, storedAt int64
	err := db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT generation, seq, stream_created_at, stored_at
			FROM statelog_cursor WHERE stream = ?`,
			stream).Scan(&generation, &seq, &created, &storedAt)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Checkpoint{At: Position{Stream: stream}}, false, nil
	case err != nil:
		return Checkpoint{}, false, fmt.Errorf("statelog: read %s's checkpoint: %w", stream, err)
	}
	return Checkpoint{
		At:       Position{Stream: stream, Generation: uint32(generation), Seq: uint64(seq)},
		KeyedTo:  store.DecodeTime(created),
		StoredAt: decodeInstant(storedAt),
	}, true, nil
}
