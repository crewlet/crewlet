package artifactsdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Threads answers every comment thread of an artifact with its remarks, oldest
// first, exactly as made: where each thread is on a version is found by the
// caller through internal/artifacts/commentanchor (ADR-0153).
func (r *Reader) Threads(ctx context.Context, id artifacts.ID) ([]CommentThread, error) {
	var out []CommentThread
	_, err := r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(
		ArtifactScope(id.String()), true), func(tx *sql.Tx) error {
		if _, err := mustHead(ctx, tx, id.String()); err != nil {
			return err
		}
		var err error
		out, err = readThreads(ctx, tx, id, "")
		return err
	})
	return out, err
}

// Thread answers one thread with its remarks, or [ErrCommentNotFound].
func (r *Reader) Thread(ctx context.Context, id artifacts.ID, thread uuid.UUID) (CommentThread, error) {
	var out CommentThread
	_, err := r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(
		ArtifactScope(id.String()), false), func(tx *sql.Tx) error {
		if _, err := mustHead(ctx, tx, id.String()); err != nil {
			return err
		}
		threads, err := readThreads(ctx, tx, id, thread.String())
		if err != nil {
			return err
		}
		if len(threads) == 0 {
			return fmt.Errorf("%w: this artifact holds no comment thread %s", ErrCommentNotFound, thread)
		}
		out = threads[0]
		return nil
	})
	return out, err
}

// threadColumns are what a thread read selects, in [scanThread]'s order.
const threadColumns = `id, version, anchor, quote, crop, crop_type, status, resolved_by,
	resolved_by_kind, resolved_at, resolved_version, created_by, created_by_kind, created_at,
	updated_at, position`

// readThreads reads an artifact's threads, or the one named, with their
// remarks.
func readThreads(ctx context.Context, tx *sql.Tx, id artifacts.ID, only string) ([]CommentThread, error) {
	where, remarkWhere, args := `artifact_id = ?`, `artifact_id = ?`, []any{id.String()}
	if only != "" {
		where, remarkWhere = where+` AND id = ?`, remarkWhere+` AND thread_id = ?`
		args = append(args, only)
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+threadColumns+`
		FROM artifact_comment_threads WHERE `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read %s's comment threads: %w", id, err)
	}
	out := []CommentThread{}
	at := map[string]int{}
	for rows.Next() {
		t, err := scanThread(rows, id)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		at[t.ID.String()] = len(out)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if len(out) == 0 {
		return out, nil
	}
	remarks, err := tx.QueryContext(ctx, `
		SELECT thread_id, id, author, author_kind, body, mentions, reply_to, created_at,
		       edited_at, removed, position
		FROM artifact_comments WHERE `+remarkWhere+` ORDER BY thread_id, created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read %s's remarks: %w", id, err)
	}
	defer func() { _ = remarks.Close() }()
	for remarks.Next() {
		var (
			thread, remark, kind, mentions, replyTo string
			created, edited                         int64
			removed                                 int
			m                                       Remark
		)
		if err := remarks.Scan(&thread, &remark, &m.Author.Name, &kind, &m.Body, &mentions,
			&replyTo, &created, &edited, &removed, &m.Position); err != nil {
			return nil, fmt.Errorf("artifactsdomain: read %s's remarks: %w", id, err)
		}
		i, ok := at[thread]
		if !ok {
			continue
		}
		if m.ID, err = uuid.Parse(remark); err != nil {
			return nil, fmt.Errorf("artifactsdomain: remark id %q: %w", remark, err)
		}
		if replyTo != "" {
			if m.ReplyTo, err = uuid.Parse(replyTo); err != nil {
				return nil, fmt.Errorf("artifactsdomain: remark %s answers %q: %w", remark, replyTo, err)
			}
		}
		if m.Mentions, err = decodeMentions(mentions); err != nil {
			return nil, err
		}
		m.Author.Kind = iam.ActorKind(kind)
		m.CreatedAt = store.DecodeTime(created)
		if edited != 0 {
			m.EditedAt = store.DecodeTime(edited)
		}
		m.Removed = removed != 0
		out[i].Remarks = append(out[i].Remarks, m)
	}
	return out, remarks.Err()
}

// scanThread reads one thread row selected by [threadColumns].
func scanThread(row rowScanner, artifact artifacts.ID) (CommentThread, error) {
	var (
		t                                CommentThread
		id, anchor, status, resolvedKind string
		createdKind                      string
		resolvedAt, createdAt, updatedAt int64
		crop                             []byte
	)
	if err := row.Scan(&id, &t.Version, &anchor, &t.Quote, &crop, &t.CropType, &status,
		&t.ResolvedBy.Name, &resolvedKind, &resolvedAt, &t.ResolvedVersion, &t.CreatedBy.Name,
		&createdKind, &createdAt, &updatedAt, &t.Position); err != nil {
		return CommentThread{}, fmt.Errorf("artifactsdomain: read %s's comment threads: %w", artifact, err)
	}
	var err error
	if t.ID, err = uuid.Parse(id); err != nil {
		return CommentThread{}, fmt.Errorf("artifactsdomain: comment thread id %q: %w", id, err)
	}
	if err = json.Unmarshal([]byte(anchor), &t.Anchor); err != nil {
		return CommentThread{}, fmt.Errorf("artifactsdomain: comment thread %s's anchor: %w", id, err)
	}
	if len(crop) > 0 {
		t.Crop = crop
	}
	t.Artifact, t.Status = artifact, ThreadStatus(status)
	t.ResolvedBy.Kind, t.CreatedBy.Kind = iam.ActorKind(resolvedKind), iam.ActorKind(createdKind)
	if resolvedAt != 0 {
		t.ResolvedAt = store.DecodeTime(resolvedAt)
	}
	t.CreatedAt, t.UpdatedAt = store.DecodeTime(createdAt), store.DecodeTime(updatedAt)
	t.Remarks = []Remark{}
	return t, nil
}
