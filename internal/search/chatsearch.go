package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/textindex"
)

// Searching the company's chat, and the one thing this ranking has that the
// knowledge base's does not: a VIEWER.
//
// A private channel's contents are not a fact about the company the way a page
// is — the room's own membership decides who may read it — so every query here
// carries the set of channels its asker may see, computed once by the caller
// that already knows the viewer. That set is REQUIRED and an empty one is
// REFUSED rather than read as "everything": the empty slice is what a caller
// that lost its viewer produces, and the two readings differ by the whole
// transcript.

// ChatQuery is one search over a company's chat.
type ChatQuery struct {
	// Text is what somebody typed.
	Text string

	// Channels is the viewer's visible set. REQUIRED — see the file doc.
	Channels []string

	// Author narrows to one speaker, empty for anybody.
	Author string

	// Limit bounds the answer; zero takes the default.
	Limit int
}

// ChatHit is one message a search matched.
type ChatHit struct {
	MessageID  string
	ChannelID  string
	ThreadRoot string
	Author     string
	Excerpt    string
	CreatedAt  int64
	Score      float64
}

// ErrNoViewer is a chat search that named no visible channels.
//
// ITS OWN ERROR rather than an empty result, because the two are opposite
// facts and only one of them is safe to render: a caller that genuinely has no
// visible channels is answered with no hits by the ordinary path, and a caller
// that FORGOT to resolve its viewer must not be handed one.
var ErrNoViewer = errors.New("search: a chat search needs the viewer's visible channels")

// chatSearchLimit is how many hits a chat query returns when it says nothing.
//
// Twenty, against the knowledge search's ten, because a chat hit is one
// sentence where a knowledge hit is a document: the same screen holds twice as
// many of them before a person has to page.
const chatSearchLimit = 20

// SearchMessages ranks a company's chat against a query.
//
// IT RAISES rather than answering empty, for the reason the knowledge search
// gives: this is the storage layer, where "the store would not answer" and
// "nothing matched" are different facts, and the seam above it is what turns a
// failure into the empty block a turn tolerates.
func (x *ChatIndexer) SearchMessages(ctx context.Context, q ChatQuery) ([]ChatHit, error) {
	if len(q.Channels) == 0 {
		return nil, ErrNoViewer
	}
	terms := textindex.Terms(q.Text)
	if len(terms) == 0 {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = chatSearchLimit
	}

	corpus, err := x.Corpus(ctx)
	if err != nil {
		return nil, err
	}
	if corpus.Docs == 0 {
		return nil, nil
	}

	scores := map[string]float64{}
	for _, term := range terms {
		postings, docs, err := x.chatPostings(ctx, term, q)
		if err != nil {
			return nil, err
		}
		idf := textindex.IDF(corpus.Docs, docs)
		for _, p := range postings {
			// THE CHAT PROFILE, not the prose one. A chat corpus is
			// bimodal — one-line acknowledgements beside pasted blocks —
			// and the prose tuning's length penalty hands every query to
			// whoever typed the shortest reply.
			scores[p.DocID] += textindex.ChatProfile.Score(idf, p, corpus)
		}
	}
	if len(scores) == 0 {
		return nil, nil
	}
	return x.hydrateChat(ctx, scores, limit)
}

// chatPostings streams one term's postings, narrowed to the viewer's channels,
// and reports how many documents in the whole corpus carry the term.
//
// THE DOCUMENT FREQUENCY IS THE CORPUS'S, not the viewer's. An IDF computed
// over one person's visible rooms would make a word rank differently for two
// people reading the same message — and would leak the shape of the rooms they
// cannot see, because the score itself would encode how common the word is
// outside them.
func (x *ChatIndexer) chatPostings(ctx context.Context, term string, q ChatQuery) (
	[]textindex.Posting, int, error) {

	var (
		postings []textindex.Posting
		docs     int
	)
	err := x.db.Read(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM chat_postings WHERE term = ?`, term).Scan(&docs); err != nil {
			return err
		}
		if docs == 0 {
			return nil
		}
		args := []any{term}
		filter := &strings.Builder{}
		filter.WriteString(` AND d.channel_id IN (`)
		for i, channel := range q.Channels {
			if i > 0 {
				filter.WriteString(",")
			}
			filter.WriteString("?")
			args = append(args, channel)
		}
		filter.WriteString(")")
		args = append(args, maxPostingScan)

		rows, err := tx.QueryContext(ctx, `
			SELECT p.doc_id, p.freq, d.length
			  FROM chat_postings p
			  JOIN chat_docs d ON d.id = p.doc_id
			 WHERE p.term = ?`+filter.String()+`
			 ORDER BY p.doc_id
			 LIMIT ?`, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p textindex.Posting
			if err := rows.Scan(&p.DocID, &p.Freq, &p.Length); err != nil {
				return err
			}
			postings = append(postings, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, fmt.Errorf("search: read the chat postings for %q: %w", term, err)
	}
	return postings, docs, nil
}

// hydrateChat turns scored ids into hits, reading the message rows themselves.
//
// FROM THE TRANSCRIPT rather than from the index, because the index keeps an
// excerpt and a hit renders the message: a second copy of every body in the
// node estate would double the largest thing on this disk to save one read of
// rows this node already holds.
//
// A MESSAGE THE INDEX HAS AND THE TRANSCRIPT NO LONGER DOES IS SKIPPED, not
// raised. That gap is the ordinary window between a prune landing and the
// index following it, and a search that failed during it would turn a few
// seconds of catch-up into an outage.
func (x *ChatIndexer) hydrateChat(ctx context.Context, scores map[string]float64, limit int) (
	[]ChatHit, error) {

	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int {
		if scores[a] != scores[b] {
			// Higher score first.
			if scores[a] > scores[b] {
				return -1
			}
			return 1
		}
		// The id breaks the tie, so two nodes answering one query with
		// the same rows order them identically.
		return strings.Compare(a, b)
	})
	if len(ids) > limit {
		ids = ids[:limit]
	}

	hits := make([]ChatHit, 0, len(ids))
	err := x.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			var hit ChatHit
			var deleted sql.NullInt64
			err := tx.QueryRowContext(ctx, `
				SELECT id, channel_id, thread_root, author_handle, body,
				       created_at, deleted_at
				  FROM chat_messages WHERE id = ?`, id).
				Scan(&hit.MessageID, &hit.ChannelID, &hit.ThreadRoot,
					&hit.Author, &hit.Excerpt, &hit.CreatedAt, &deleted)
			switch {
			case errors.Is(err, sql.ErrNoRows), deleted.Valid:
				// Pruned, erased, or tombstoned since it was indexed.
				continue
			case err != nil:
				return err
			}
			hit.Score = scores[id]
			hits = append(hits, hit)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("search: hydrate the chat hits: %w", err)
	}
	return hits, nil
}
