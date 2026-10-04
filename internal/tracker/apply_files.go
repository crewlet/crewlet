package tracker

import (
	"context"
	"database/sql"
	"fmt"
)

// fileMatches refuses a file record whose payload is not the file its subject
// arbitrated, or whose chunks are not content addresses.
//
// THE SUBJECT IS THE ADDRESS, for the page title's reason: a writer that took
// one path at the broker and wrote another into every node's row would leave
// the arbitrated path held by nothing and the written one held by two.
//
// AND EVERY CHUNK IS A VALID ADDRESS: the collector and the backup read each
// name back through [objstore.ParseHash], so a row naming something that is
// not sixty-four lowercase hex digits would stop every collection pass and
// every backup for good, on every node.
func fileMatches(c applyContext, id string, file File) error {
	path, err := NormalizeFilePath(file.Path)
	switch {
	case err != nil:
		return fmt.Errorf("tracker: the file record at %s: %w", c.position, err)
	case path != file.Path || FileSubject(file.Project, file.Path).ID != id:
		return fmt.Errorf("tracker: the record at %s arbitrated file %s and its "+
			"payload claims %s/%q — the subject IS the address", c.position, id,
			file.Project, file.Path)
	}
	for i, chunk := range file.Chunks {
		if !chunk.Hash.Valid() {
			return fmt.Errorf("tracker: the file record at %s names chunk %d as "+
				"%q, which is not a content address", c.position, i, chunk.Hash)
		}
	}
	return nil
}

// explodeFileChunks rebuilds a file's chunk rows from its record — the rows
// the object store's collector reads its references from. A removed file has
// none, which is what lets its chunks go.
func (a *Applier) explodeFileChunks(ctx context.Context, tx *sql.Tx, id string,
	c applyContext) (int, error) {

	var file File
	if err := decodePayload(c.record.Mutation, &file); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tracker_file_chunks WHERE file_id = ?`, id); err != nil {
		return 0, fmt.Errorf("tracker: clear the chunks of file %s: %w", id, err)
	}
	type numbered struct {
		seq   int
		chunk FileChunk
	}
	rows := make([]numbered, 0, len(file.Chunks))
	for i, chunk := range file.Chunks {
		rows = append(rows, numbered{seq: i, chunk: chunk})
	}
	return insertMany(ctx, tx, c.maxVariables, `
		INSERT INTO tracker_file_chunks (file_id, seq, chunk, size)
		VALUES`,
		`(?,?,?,?)`,
		`ON CONFLICT (file_id, seq) DO UPDATE SET
			chunk = excluded.chunk, size = excluded.size`,
		rows, func(r numbered) []any {
			return []any{id, r.seq, string(r.chunk.Hash), r.chunk.Size}
		})
}
