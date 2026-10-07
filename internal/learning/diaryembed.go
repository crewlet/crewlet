package learning

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A diary note's vector: computed as it is written, and FILLED later for the
// notes that were written without one.
//
// # Why the similarity half had nothing to find
//
// `## Personal memory` is built from two halves, the notes most similar to the
// turn's ask and the most recent fifty, and the similarity half reads only rows
// with a vector. No writer ever set one: the column was declared, the recall
// was written against it, and every note landed NULL — so a durable fact older
// than the newest fifty could not reach the relevance filter by push or by
// refresh_memory, and, never selected, it was the first thing the 500-entry
// trim evicted. A note is now embedded as it is written ([Diary.Write]), and
// every note without a vector of the current model is filled by the node
// holding its seat ([Diary.Unfilled], [Diary.FillEmbeddings]).
//
// # A filled note travels because setting its vector stamps it
//
// The memory changelog carries a diary row once its CHANGE SEQUENCE — a number
// the row takes when it is inserted — passes the watermark the last carry
// left. An update in place that took no new number would leave the row behind
// that watermark, so a vector set on it would stay on this node, and the
// seat's next holder would recall from a copy that has none. So the table
// stamps a row with a fresh sequence whenever its vector is set (a trigger,
// node migration 0041), in the statement that sets it, and the watermark
// carries the filled note like a new one. The receiving end is the
// other half: a peer that already holds the vectorless row takes the carried
// vector over it rather than skipping a row it has (internal/learning/memsync).
//
// It used to MOVE the row instead — `rowid = max(rowid) + 1` — and the rowid
// is the one number here that is reused: with the newest note deleted by the
// trim or the expiry, the next insert or fill took a rowid at or below the
// watermark and was never carried at all.

// DiaryEmbedBudget bounds embedding one note as it is written.
//
// TWO SECONDS, search.QueryEmbedBudget's figure and for its reason: a note is
// one short input — at most [MaxContentChars] bytes — which a hosted endpoint
// answers in a few hundred milliseconds at the p99, and the reflect_and_persist
// call that writes one has a model waiting on it. A miss costs nothing but
// time: the note lands without a vector and the holder's fill gives it one.
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
// model at this store's width — written before notes were embedded, written
// while no provider answered, embedded under a model the company has since
// moved off, or at a width a restart left behind — newest first, strictly
// after the cursor (see [FillCursor]).
//
// THE WIDTH AS WELL AS THE MODEL, because one model answers at whatever width
// is asked: a store reopened at a new width holds notes of the same model that
// recall's width filter no longer admits, and only a fill brings them back.
//
// A note with no text is never returned: there is nothing to embed, and a row
// that could never be filled would be read on every pass for ever. A note's
// vector is of its content, as [Diary.Write] embeds it.
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
		   AND (embedding IS NULL OR embedding_model IS NULL OR embedding_model <> ?
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
