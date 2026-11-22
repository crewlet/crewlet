package artifactsdomain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Threads answers every comment thread of an artifact with its remarks, oldest
// first, exactly as made: where each thread is on a version is found by the
// caller through internal/artifacts/commentanchor (ADR-0154).
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
		t, err := scanThread(rows, id) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		at[t.ID.String()] = len(out)
		out = append(out, t)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if len(out) == 0 {
		return out, nil
	}
	remarks, err := tx.QueryContext(ctx, `SELECT `+remarkColumns+`
		FROM artifact_comments WHERE `+remarkWhere+` ORDER BY thread_id, created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read %s's remarks: %w", id, err)
	}
	defer func() { _ = remarks.Close() }()
	for remarks.Next() {
		thread, m, err := scanRemark(remarks, id) //nolint:govet // shadow: scoped to this loop
		if err != nil {
			return nil, err
		}
		if i, ok := at[thread]; ok {
			out[i].Remarks = append(out[i].Remarks, m)
		}
	}
	return out, remarks.Err()
}

// remarkColumns are what a remark read selects, in [scanRemark]'s order.
const remarkColumns = `thread_id, id, author, author_kind, body, mentions, reply_to, created_at,
	edited_at, removed, position`

// scanRemark reads one remark row selected by [remarkColumns], and its thread.
func scanRemark(row rowScanner, artifact artifacts.ID) (string, Remark, error) {
	var (
		thread, remark, kind, mentions, replyTo string
		created, edited                         int64
		removed                                 int
		m                                       Remark
	)
	if err := row.Scan(&thread, &remark, &m.Author.Name, &kind, &m.Body, &mentions,
		&replyTo, &created, &edited, &removed, &m.Position); err != nil {
		return "", Remark{}, fmt.Errorf("artifactsdomain: read %s's remarks: %w", artifact, err)
	}
	var err error
	if m.ID, err = uuid.Parse(remark); err != nil {
		return "", Remark{}, fmt.Errorf("artifactsdomain: remark id %q: %w", remark, err)
	}
	if replyTo != "" {
		if m.ReplyTo, err = uuid.Parse(replyTo); err != nil {
			return "", Remark{}, fmt.Errorf("artifactsdomain: remark %s answers %q: %w", remark, replyTo, err)
		}
	}
	if m.Mentions, err = decodeMentions(mentions); err != nil {
		return "", Remark{}, err
	}
	m.Author.Kind = iam.ActorKind(kind)
	m.CreatedAt = store.DecodeTime(created)
	if edited != 0 {
		m.EditedAt = store.DecodeTime(edited)
	}
	m.Removed = removed != 0
	return thread, m, nil
}

// threadCursor is where a page of threads ends: after a whole thread, or
// inside one, after one of its remarks ([Reader.ThreadsPage]).
type threadCursor struct {
	At       int64  `json:"a"`
	ID       string `json:"i"`
	Inside   bool   `json:"in,omitempty"`
	RemarkAt int64  `json:"ra,omitempty"`
	RemarkID string `json:"ri,omitempty"`
}

// ThreadsPage is [Reader.Threads] a page at a time, as a node holding no data
// reads it: oldest first after the cursor (empty for the first), each thread
// with its remarks, held to the bytes the read carries ([statelog.RowsOf]).
// A page that ends inside a thread's remarks marks it [CommentThread.RemarksCut]
// and the next one starts with that thread again, carrying the rest: one
// thread's remarks at their caps run past a reply.
func (r *Reader) ThreadsPage(ctx context.Context, id artifacts.ID, cursor string) ([]CommentThread, string, error) {
	var from threadCursor
	if cursor != "" {
		if err := json.Unmarshal([]byte(cursor), &from); err != nil || from.ID == "" {
			return nil, "", fmt.Errorf("%w: the cursor %q is not one a page answered", ErrInvalid, cursor)
		}
	}
	var out []CommentThread
	var next string
	_, err := r.log.Read(ctx, staleFresh(statelog.Freshness{}).Query(
		ArtifactScope(id.String()), true), func(tx *sql.Tx) error {
		if _, err := mustHead(ctx, tx, id.String()); err != nil {
			return err
		}
		var err error
		out, next, err = threadsPage(ctx, tx, id, from)
		return err
	})
	return out, next, err
}

// threadsPage reads one page of threads after from (every one from the zero):
// the threads' own rows first, a statement read to the answer's bytes, then
// each in order held with its remarks, a statement each, the page ending where
// a thread's row or one of its remarks is refused.
func threadsPage(ctx context.Context, tx *sql.Tx, id artifacts.ID,
	from threadCursor) ([]CommentThread, string, error) {

	after := ">"
	if from.Inside {
		after = ">="
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+threadColumns+`
		FROM artifact_comment_threads WHERE artifact_id = ?
		  AND (? = '' OR (created_at, id) `+after+` (?, ?)) ORDER BY created_at, id`,
		id.String(), from.ID, from.At, from.ID)
	if err != nil {
		return nil, "", fmt.Errorf("artifactsdomain: read %s's comment threads: %w", id, err)
	}
	scanned := statelog.RowsOf(ctx)
	out := []CommentThread{}
	for rows.Next() {
		t, err := scanThread(rows, id) //nolint:govet // shadow: scoped to this loop
		if err != nil {
			_ = rows.Close()
			return nil, "", err
		}
		if !scanned.TakeEncoded(t) {
			break
		}
		out = append(out, t)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, "", err
	}
	if len(out) > 0 && from.Inside {
		out[0].Continued = true
	}
	held := statelog.RowsOf(ctx)
	for i := range out {
		place := threadCursor{At: store.EncodeTime(out[i].CreatedAt), ID: out[i].ID.String()}
		if !held.TakeEncoded(out[i]) {
			prev := out[i-1]
			return out[:i], threadPlace(threadCursor{At: store.EncodeTime(prev.CreatedAt), ID: prev.ID.String()}), nil
		}
		remarkAt, remarkID := int64(0), ""
		if i == 0 && from.Inside {
			remarkAt, remarkID = from.RemarkAt, from.RemarkID
		}
		cut, err := threadRemarks(ctx, tx, id, &out[i], remarkAt, remarkID, held, i == 0 && from.Inside)
		if err != nil {
			return nil, "", err
		}
		if cut != nil {
			out[i].RemarksCut = true
			place.Inside, place.RemarkAt, place.RemarkID = true, cut.At, cut.ID
			return out[:i+1], threadPlace(place), nil
		}
	}
	if scanned.Cut() {
		last := out[len(out)-1]
		return out, threadPlace(threadCursor{At: store.EncodeTime(last.CreatedAt), ID: last.ID.String()}), nil
	}
	return out, "", nil
}

// threadRemarks fills t's remarks after (remarkAt, remarkID), up to what held
// keeps, and the place of the last one kept where held refused one. A page
// resuming inside t keeps at least one remark, so paging always moves on.
func threadRemarks(ctx context.Context, tx *sql.Tx, id artifacts.ID, t *CommentThread,
	remarkAt int64, remarkID string, held *statelog.RowBytes, resumed bool) (*threadCursor, error) {

	remarks, err := tx.QueryContext(ctx, `SELECT `+remarkColumns+`
		FROM artifact_comments WHERE artifact_id = ? AND thread_id = ?
		  AND (? = '' OR (created_at, id) > (?, ?)) ORDER BY created_at, id`,
		id.String(), t.ID.String(), remarkID, remarkAt, remarkID)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read %s's remarks: %w", id, err)
	}
	defer func() { _ = remarks.Close() }()
	last := threadCursor{At: remarkAt, ID: remarkID}
	for remarks.Next() {
		_, m, err := scanRemark(remarks, id)
		if err != nil {
			return nil, err
		}
		if !held.TakeEncoded(m) {
			if resumed && len(t.Remarks) == 0 {
				t.Remarks = append(t.Remarks, m)
				last = threadCursor{At: store.EncodeTime(m.CreatedAt), ID: m.ID.String()}
			}
			return &last, remarks.Err()
		}
		t.Remarks = append(t.Remarks, m)
		last = threadCursor{At: store.EncodeTime(m.CreatedAt), ID: m.ID.String()}
	}
	return nil, remarks.Err()
}

// JoinThreads is page appended to the threads read before it: a thread the
// last page cut inside its remarks takes the rest from the page's first.
func JoinThreads(read, page []CommentThread) []CommentThread {
	if n := len(read); n > 0 && len(page) > 0 && page[0].Continued && read[n-1].ID == page[0].ID {
		read[n-1].Remarks = append(read[n-1].Remarks, page[0].Remarks...)
		read[n-1].RemarksCut, page = page[0].RemarksCut, page[1:]
	}
	return append(read, page...)
}

// threadPlace is a cursor as a page answers it.
func threadPlace(c threadCursor) string {
	raw, _ := json.Marshal(c)
	return string(raw)
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
