package learning

import (
	"context"
	"errors"
	"fmt"
)

// An episode's vector, FILLED by the node holding its seat when the row has
// none of the current model — the diary's arrangement ([Diary.Unfilled]), and
// possible because the row stores the one part of its text nothing else on it
// holds, the ask.
//
// Without it a row whose vector was missing, or was of a model the company has
// since left, was out of similarity search for good: recall compares only rows
// of the query's model, so after a model change every turn a seat had taken
// was unreachable by meaning until enough new ones accumulated — and the seat
// asking `query_episodes` was told it had never done this work.

// Unfilled returns up to limit of a seat's RAW episodes that have no vector of
// model at this store's width, newest first, strictly after the cursor (see
// [FillCursor]) — each with exactly the text its vector is of ([episodeText]).
//
// RAW ONLY: a compacted row stands for a cluster of turns and is never
// searched by similarity ([Episodes.Recall]), so a vector on one would be paid
// for and never read. A row with no text at all is never returned: there is
// nothing to embed.
func (e *Episodes) Unfilled(ctx context.Context, handle, model string, after FillCursor,
	limit int,
) ([]Unfilled, error) {
	if handle == "" || model == "" {
		return nil, errors.New("learning: an unfilled-episodes read needs a seat and a model")
	}
	if limit <= 0 {
		limit = defaultEpisodeListing
	}
	args := e.unfilledArgs(handle, model)
	args = append(args, cursorArgs(after)...)
	args = append(args, limit)
	rows, err := e.db.SQL().QueryContext(ctx,
		`SELECT `+episodeColumns+` FROM episodes
		 WHERE `+unfilledEpisode+`
		   AND (? = 0 OR ended_at < ? OR (ended_at = ? AND id < ?))
		 ORDER BY ended_at DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("learning: unfilled episodes for %s: %w", handle, err)
	}
	episodes, err := collectEpisodes(rows)
	if err != nil {
		return nil, err
	}
	out := make([]Unfilled, len(episodes))
	for i, ep := range episodes {
		out[i] = Unfilled{ID: ep.ID, Text: episodeText(ep),
			Cursor: FillCursor{At: ep.EndedAt, ID: ep.ID}}
	}
	return out, nil
}

// Unsearchable is how many of a seat's raw episodes similarity recall under
// model cannot reach — no vector of it at this store's width — so a search
// that found nothing can say what it did not search rather than that the seat
// has never done the work.
//
// [Episodes.Unfilled]'s own predicate ([unfilledEpisode]), counted: a row it
// names is one the holder's fill will reach, which is what the answer tells the
// seat. A row with no text is not one — nothing could ever embed it, and a
// search by meaning has nothing in it to match — so it is not counted as a turn
// the search could not reach, where counting it told the seat for ever that the
// turn was being embedded again.
func (e *Episodes) Unsearchable(ctx context.Context, handle, model string) (int, error) {
	var n int
	err := e.db.SQL().QueryRowContext(ctx,
		`SELECT count(*) FROM episodes WHERE `+unfilledEpisode,
		e.unfilledArgs(handle, model)...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("learning: count unsearchable episodes for %s: %w", handle, err)
	}
	return n, nil
}

// FillEmbeddings stores vectors on raw episodes, in one transaction, and
// reports how many it stored — the rule every filled table follows
// ([fillVectors]). Each takes a fresh change sequence as its vector is set, so
// the memory changelog carries it to the seat's next holder.
func (e *Episodes) FillEmbeddings(ctx context.Context, fills []VectorFill) (int, error) {
	return fillVectors(ctx, e.db, episodeFillSQL, "episodes", "episode_fill_discarded", fills)
}

// unfilledEpisode is the predicate [Episodes.Unfilled] selects by and
// [Episodes.Unsearchable] counts by — ONE STATEMENT OF IT, so the count names
// exactly the rows the fill reaches: a seat's raw row with no vector of the
// model at this store's width, and with text to make one of ([hasTextSQL]).
// Its binds are [Episodes.unfilledArgs].
var unfilledEpisode = `agent_handle = ? AND kind = ?
	AND (embedding IS NULL OR embedding_model <> ?
	     OR (? > 0 AND length(embedding) <> ?))
	AND ` + hasTextSQL("task_summary || ask || plan_summary")

// unfilledArgs are [unfilledEpisode]'s binds, in order.
func (e *Episodes) unfilledArgs(handle, model string) []any {
	return []any{handle, string(KindRaw), model, e.vectorBytes(), e.vectorBytes()}
}

// vectorBytes is how many bytes a vector at this store's width packs to, or 0
// for a store opened with no width — which vetoes nothing.
func (e *Episodes) vectorBytes() int {
	return 4 * e.db.EmbeddingDim()
}
