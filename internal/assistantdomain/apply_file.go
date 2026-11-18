package assistantdomain

import (
	"context"
	"database/sql"
	"fmt"
)

// applyFile writes one file's row and moves the conversation's activity (an
// upload or a delivery is use). It writes nothing for a conversation missing or
// not the owner's, an unknown origin, or one already holding
// [FilesPerConversation] of the origin: the bound is decided again here, in log
// order, because the record is additive and two uploads from one snapshot
// would both land. The writer then reports [ErrTooManyFiles], and the
// collector deletes the object no row names.
func applyFile(ctx context.Context, tx *sql.Tx, c applyContext, id string,
	p FileRecorded) (int, error) {

	if !present(p.File) || !present(p.Object) {
		return 0, nil
	}
	bound := FilesPerConversation(p.Origin)
	if bound == 0 {
		return 0, nil
	}
	row, found, err := readConversation(ctx, tx, id)
	if err != nil || !found || row.owner != p.Owner {
		return 0, err
	}
	held, err := filesHeld(ctx, tx, id, string(p.Origin))
	if err != nil {
		return 0, err
	}
	if held >= bound {
		return 0, nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO assistant_files
			(id, conversation_id, turn_id, owner_id, origin, name_sealed, media_type,
			 size, object, hash, object_size, content_key, created_at, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		p.File, id, p.Turn, p.Owner, string(p.Origin), p.NameSealed, p.MediaType,
		p.Size, p.Object, p.Hash, p.ObjectSize, p.ContentKey, c.at(), c.packed)
	if err != nil {
		return 0, fmt.Errorf("assistantdomain: record file %s of conversation %s at %s: %w",
			p.File, id, c.position, err)
	}
	written := affected(res)
	if written == 0 {
		return 0, nil
	}
	touched, err := touch(ctx, tx, c, id)
	return written + touched, err
}

// filesHeld is how many files of origin conversation keeps.
func filesHeld(ctx context.Context, tx *sql.Tx, conversation, origin string) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM assistant_files WHERE conversation_id = ? AND origin = ?`,
		conversation, origin).Scan(&n); err != nil {
		return 0, fmt.Errorf("assistantdomain: count conversation %s's %s files: %w",
			conversation, origin, err)
	}
	return n, nil
}
