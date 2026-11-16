package artifactsdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// The records on a comment thread's subject (ADR-0153). A refusal writes no
// row, and the writer reads that absence back as its verdict; every row
// carries the record's position, so a redelivery moves nothing.

// applyComment writes one record on a thread's subject.
func (a *Applier) applyComment(ctx context.Context, tx *sql.Tx, c applyContext) (int, error) {
	payload, err := DecodeMutation(c.record)
	if err != nil {
		return 0, err
	}
	artifact, thread, ok := commentSubjectIDs(c.record.Subject.ID)
	if !ok {
		return 0, nil
	}
	head, found, err := readHead(ctx, tx, artifact)
	if err != nil || !found || head.status == string(StatusTrashed) {
		return 0, err
	}
	var rows int
	switch p := payload.(type) {
	case CommentOpenPayload:
		rows, err = openThread(ctx, tx, c, artifact, thread, p)
	case CommentReplyPayload:
		rows, err = addRemark(ctx, tx, c, artifact, thread, p)
	case CommentEditPayload:
		rows, err = editRemark(ctx, tx, c, artifact, thread, p)
	case CommentRemovePayload:
		rows, err = removeRemark(ctx, tx, c, artifact, thread, p)
	case CommentStatusPayload:
		rows, err = setThreadStatus(ctx, tx, c, artifact, thread, head.head, p)
	default:
		return 0, fmt.Errorf("artifactsdomain: the comment record at %s carries a %T",
			c.position, payload)
	}
	if err != nil || rows == 0 {
		return rows, err
	}
	a.move(c, artifact, MoveComment, head.epoch, head.head)
	return rows, nil
}

// openThread writes a thread and its first remark, refused past the thread
// bound the record was stamped with.
func openThread(ctx context.Context, tx *sql.Tx, c applyContext, artifact, thread string,
	p CommentOpenPayload) (int, error) {

	if !isUUID(p.Remark) || p.MaxThreads <= 0 {
		return 0, nil
	}
	if _, found, err := readThreadTx(ctx, tx, artifact, thread); err != nil || found {
		return 0, err
	}
	threads, err := countThreads(ctx, tx, artifact)
	if err != nil || threads >= p.MaxThreads {
		return 0, err
	}
	anchor, err := json.Marshal(p.Anchor)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: encode the anchor at %s: %w", c.position, err)
	}
	var crop []byte
	if len(p.Crop) > 0 {
		crop = p.Crop
	}
	actor := c.record.actor()
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO artifact_comment_threads
			(artifact_id, id, version, anchor, quote, crop, crop_type, status,
			 created_by, created_by_kind, created_at, updated_at, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		artifact, thread, p.Version, string(anchor), p.Quote, crop, p.CropType,
		string(ThreadOpen), actor.Name, string(actor.Kind), c.at(), c.at(), c.packed); err != nil {
		return 0, fmt.Errorf("artifactsdomain: open comment thread %s on %s: %w", thread, artifact, err)
	}
	if err = insertRemark(ctx, tx, c, artifact, thread, p.Remark, p.Body, "", p.Mentions); err != nil {
		return 0, err
	}
	return 2, nil
}

// addRemark writes one reply, refused past the remark bound the record was
// stamped with, and moves the thread's last activity.
func addRemark(ctx context.Context, tx *sql.Tx, c applyContext, artifact, thread string,
	p CommentReplyPayload) (int, error) {

	if !isUUID(p.Remark) || p.MaxRemarks <= 0 {
		return 0, nil
	}
	if _, found, err := readThreadTx(ctx, tx, artifact, thread); err != nil || !found {
		return 0, err
	}
	if _, found, err := readRemarkTx(ctx, tx, artifact, thread, p.Remark); err != nil || found {
		return 0, err
	}
	remarks, err := countRemarks(ctx, tx, artifact, thread)
	if err != nil || remarks >= p.MaxRemarks {
		return 0, err
	}
	if err = insertRemark(ctx, tx, c, artifact, thread, p.Remark, p.Body, p.ReplyTo, p.Mentions); err != nil {
		return 0, err
	}
	n, err := touchThread(ctx, tx, c, artifact, thread)
	return 1 + n, err
}

// editRemark replaces a live remark's body and mentions, for its author alone.
func editRemark(ctx context.Context, tx *sql.Tx, c applyContext, artifact, thread string,
	p CommentEditPayload) (int, error) {

	r, found, err := readRemarkTx(ctx, tx, artifact, thread, p.Remark)
	actor := c.record.actor()
	if err != nil || !found || r.removed || r.position >= c.packed ||
		r.author != actor.Name || r.authorKind != string(actor.Kind) {
		return 0, err
	}
	mentions, err := encodeMentions(p.Mentions)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE artifact_comments SET body = ?, mentions = ?, edited_at = ?, position = ?
		WHERE artifact_id = ? AND thread_id = ? AND id = ?`,
		p.Body, mentions, c.at(), c.packed, artifact, thread, p.Remark); err != nil {
		return 0, fmt.Errorf("artifactsdomain: edit remark %s of %s: %w", p.Remark, thread, err)
	}
	n, err := touchThread(ctx, tx, c, artifact, thread)
	return 1 + n, err
}

// removeRemark marks a remark removed and clears its words; its row keeps its
// place and its author.
func removeRemark(ctx context.Context, tx *sql.Tx, c applyContext, artifact, thread string,
	p CommentRemovePayload) (int, error) {

	r, found, err := readRemarkTx(ctx, tx, artifact, thread, p.Remark)
	if err != nil || !found || r.removed || r.position >= c.packed {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE artifact_comments SET body = '', mentions = '[]', removed = 1, position = ?
		WHERE artifact_id = ? AND thread_id = ? AND id = ?`,
		c.packed, artifact, thread, p.Remark); err != nil {
		return 0, fmt.Errorf("artifactsdomain: remove remark %s of %s: %w", p.Remark, thread, err)
	}
	n, err := touchThread(ctx, tx, c, artifact, thread)
	return 1 + n, err
}

// setThreadStatus resolves an open thread or reopens a resolved one. A resolve
// names a version from the one the thread was made on to the head.
func setThreadStatus(ctx context.Context, tx *sql.Tx, c applyContext, artifact, thread string,
	headNumber int, p CommentStatusPayload) (int, error) {

	t, found, err := readThreadTx(ctx, tx, artifact, thread)
	if err != nil || !found || t.position >= c.packed {
		return 0, err
	}
	actor := c.record.actor()
	var res sql.Result
	switch c.record.Op {
	case OpCommentResolve:
		if t.status != ThreadOpen || p.Version < t.version || p.Version > headNumber {
			return 0, nil
		}
		res, err = tx.ExecContext(ctx, `
			UPDATE artifact_comment_threads SET status = ?, resolved_by = ?,
				resolved_by_kind = ?, resolved_at = ?, resolved_version = ?, updated_at = ?,
				position = ?
			WHERE artifact_id = ? AND id = ?`,
			string(ThreadResolved), actor.Name, string(actor.Kind), c.at(), p.Version, c.at(),
			c.packed, artifact, thread)
	case OpCommentReopen:
		if t.status != ThreadResolved {
			return 0, nil
		}
		res, err = tx.ExecContext(ctx, `
			UPDATE artifact_comment_threads SET status = ?, resolved_by = '',
				resolved_by_kind = '', resolved_at = 0, resolved_version = 0, updated_at = ?,
				position = ?
			WHERE artifact_id = ? AND id = ?`,
			string(ThreadOpen), c.at(), c.packed, artifact, thread)
	default:
		return 0, fmt.Errorf("artifactsdomain: %s is not an op a thread's status is set by",
			c.record.Op)
	}
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: %s comment thread %s of %s: %w", c.record.Op,
			thread, artifact, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// insertRemark writes one remark by the record's actor.
func insertRemark(ctx context.Context, tx *sql.Tx, c applyContext, artifact, thread, remark,
	body, replyTo string, mentions []string) error {

	encoded, err := encodeMentions(mentions)
	if err != nil {
		return err
	}
	actor := c.record.actor()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_comments
			(artifact_id, thread_id, id, author, author_kind, body, mentions, reply_to,
			 created_at, edited_at, removed, position)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?)`,
		artifact, thread, remark, actor.Name, string(actor.Kind), body, encoded, replyTo,
		c.at(), c.packed); err != nil {
		return fmt.Errorf("artifactsdomain: add remark %s to %s: %w", remark, thread, err)
	}
	return nil
}

// touchThread moves a thread's last activity to the record applying.
func touchThread(ctx context.Context, tx *sql.Tx, c applyContext, artifact, thread string) (int, error) {
	res, err := tx.ExecContext(ctx, `
		UPDATE artifact_comment_threads SET updated_at = ?, position = ?
		WHERE artifact_id = ? AND id = ? AND position < ?`,
		c.at(), c.packed, artifact, thread, c.packed)
	if err != nil {
		return 0, fmt.Errorf("artifactsdomain: touch comment thread %s of %s: %w", thread, artifact, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// encodeMentions is a mentions column: a JSON array, `[]` for none.
func encodeMentions(mentions []string) (string, error) {
	if len(mentions) == 0 {
		return "[]", nil
	}
	out, err := json.Marshal(mentions)
	if err != nil {
		return "", fmt.Errorf("artifactsdomain: encode a remark's mentions: %w", err)
	}
	return string(out), nil
}

// commentSubjectIDs splits a thread's subject id; false for one the writer never makes.
func commentSubjectIDs(id string) (artifact, thread string, ok bool) {
	artifact, thread, err := SplitCommentID(id)
	return artifact, thread, err == nil && isUUID(thread)
}

// isUUID reports whether s is a well-formed id.
func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}
