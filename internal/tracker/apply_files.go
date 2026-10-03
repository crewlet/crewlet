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
// AND EVERY CHUNK IS A VALID ADDRESS, because the row's slot is DERIVED from it
// ([explodeFileChunks]) and a string that is not sixty-four lowercase hex
// digits has none. Filed anyway, it would be a row every pass over its group
// refuses — the passes read each name back through [objstore.ParseHash] — so
// that group's repair and collection would stop for good, on every node.
// Lowercase matters for the rows written before slots too: replicated
// migration 0032 computed theirs from the chunk's text with an expression that
// reads lowercase hex only, which is sound because this refusal was already in
// force when every one of them was applied.
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
// the object store reads a placement group's references from. A removed file
// has none, which is what lets its chunks go.
//
// Each row carries its chunk's SLOT, computed here from the hash. Computing
// is safe on an applier, which must produce identical rows on every node,
// because the slot is a function of the content address alone — no map, no
// group count, no configuration another node could hold differently — and it
// is what lets a pass read one group's references as one range of
// `tracker_file_chunks_slot_idx` at whatever group count the map has.
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
		INSERT INTO tracker_file_chunks (file_id, seq, chunk, size, slot)
		VALUES`,
		`(?,?,?,?,?)`,
		`ON CONFLICT (file_id, seq) DO UPDATE SET
			chunk = excluded.chunk, size = excluded.size, slot = excluded.slot`,
		rows, func(r numbered) []any {
			return []any{id, r.seq, string(r.chunk.Hash), r.chunk.Size, r.chunk.Hash.Slot()}
		})
}
