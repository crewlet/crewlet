package learning

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A diary note's vector is computed as it is written ([Diary.Write]) and
// FILLED later by the seat's node ([Diary.Unfilled], [Diary.FillEmbeddings]):
// the similarity half of `## Personal memory` reads only rows with a vector,
// so without one a durable fact older than the newest fifty is never recalled
// and is first evicted by the 500-entry trim. A filled note travels because
// setting its vector stamps it: the memory changelog carries a row once its
// CHANGE SEQUENCE passes the last carry's watermark, so an update with no new
// number would leave the vector on this node. A trigger (node migration 0041)
// gives the row a fresh sequence when its vector is set; a peer holding the
// vectorless row takes the carried vector (internal/learning/memsync). Not
// `rowid = max(rowid) + 1`: rowids are reused, so a row is never carried.

// DiaryEmbedBudget bounds embedding one note as it is written.
//
// TWO SECONDS, search.QueryEmbedBudget's figure and ONLY its figure: that
// budget is also held at twice a semantic scan's, which says nothing about a
// note, so tuning one leaves the other. Here a note is one short input (at
// most [MaxContentChars] bytes) which a hosted endpoint answers in a few
// hundred milliseconds at the p99; the reflect_and_persist call that writes it
// has a model waiting mid-turn; and a miss costs only the vector, which the
// node holding the seat fills later.
const DiaryEmbedBudget = 2 * time.Second

// embedNote is the note's vector, or none.
//
// NEVER an error, for the episodist's reason: the note is what was asked to be
// kept, and its vector only decides whether recall can reach it by meaning.
func (d *Diary) embedNote(ctx context.Context, e DiaryEntry) Vector {
	if d.embed == nil || strings.TrimSpace(e.Content) == "" {
		return Vector{}
	}
	ctx, cancel := context.WithTimeout(ctx, DiaryEmbedBudget)
	defer cancel()
	vector, err := d.embed(ctx, e.Content)
	if errors.Is(err, ErrNoEmbeddings) {
		return Vector{}
	}
	if err != nil {
		log.WarnContext(ctx, "diary_embedding_failed", "entry", e.ID, "error", err.Error(),
			"detail", "the note is written without a vector; the node holding the "+
				"seat fills it later")
		return Vector{}
	}
	return vector
}

// Unfilled returns up to limit of a seat's live notes that have no vector of
// model at this store's width (written with no embedding provider answering,
// embedded under a model since moved off, or at a width a restart left
// behind), newest first, strictly after the cursor (see [FillCursor]).
//
// THE WIDTH AS WELL AS THE MODEL, because one model answers at whatever width
// is asked: a store reopened at a new width holds same-model notes that
// recall's width filter no longer admits, and only a fill brings them back.
//
// A note with no text is never returned: a row that could never be filled
// would be read on every pass for ever.
func (d *Diary) Unfilled(ctx context.Context, agentID, model string, now time.Time,
	after FillCursor, limit int,
) ([]Unfilled, error) {
	if agentID == "" || model == "" {
		return nil, errors.New("learning: an unfilled-notes read needs an agent and a model")
	}
	if limit <= 0 {
		limit = defaultDiaryListing
	}
	args := []any{agentID, store.EncodeTime(now), model, d.vectorBytes(), d.vectorBytes()}
	args = append(args, cursorArgs(after)...)
	args = append(args, limit)
	rows, err := d.db.SQL().QueryContext(ctx,
		`SELECT `+diaryColumns+` FROM agent_diary
		 WHERE agent_id = ?
		   AND (ttl_until IS NULL OR ttl_until > ?)
		   AND (embedding IS NULL OR embedding_model <> ?
		        OR (? > 0 AND length(embedding) <> ?))
		   AND `+hasTextSQL("content")+`
		   AND (? = 0 OR created_at < ? OR (created_at = ? AND id < ?))
		 ORDER BY created_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("learning: unfilled diary for %s: %w", agentID, err)
	}
	notes, err := collectDiary(rows)
	if err != nil {
		return nil, err
	}
	out := make([]Unfilled, len(notes))
	for i, note := range notes {
		out[i] = Unfilled{ID: note.ID, Text: note.Content,
			Cursor: FillCursor{At: note.CreatedAt, ID: note.ID}}
	}
	return out, nil
}

// vectorBytes is how many bytes a vector at this store's width packs to, or 0
// for a store opened with no width — which vetoes nothing, as it vetoes no
// write ([store.DB.EncodeVector]).
func (d *Diary) vectorBytes() int {
	return 4 * d.db.EmbeddingDim()
}

// FillEmbeddings stores vectors on notes, in one transaction, and reports how
// many it stored.
//
// EACH NOTE TAKES A FRESH CHANGE SEQUENCE in the statement that sets its
// vector — the table's own trigger stamps it — so the memory changelog's
// watermark carries it to the seat's next holder; see the file comment. The
// rest of the rule is every filled table's ([fillVectors]): a note already
// holding a vector of the same model at this width is left alone and not
// counted, a vector of the wrong width fails the whole fill, and a non-finite
// one, or one naming no model, is skipped and the note stays unfilled.
func (d *Diary) FillEmbeddings(ctx context.Context, fills []VectorFill) (int, error) {
	return fillVectors(ctx, d.db, diaryFillSQL, "agent_diary", "diary_fill_discarded", fills)
}
