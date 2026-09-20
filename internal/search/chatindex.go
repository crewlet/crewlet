package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/textindex"
)

// The chat corpus's own keyword index, and why it is not the knowledge base's.
//
// # A different corpus, deliberately
//
// Chat is in NEITHER half of the knowledge corpus. The argument is already in
// this package: [TaskSource] does not index a work item's comment thread,
// because a thread is a conversation ABOUT the item rather than a statement of
// it and indexing it would make one busy item outrank every concise one on any
// word said in passing. A chat message is that shape at ten times the volume.
//
// It has no semantic half either, and that is arithmetic rather than taste: a
// year of chat at the declared census needs several times the entire supported
// vector corpus, against an embedding duty whose whole budget is shared across
// every corpus a company has. Admitting chat to one half and not the other is
// the failure this tree has already recorded once, so it is admitted to
// neither and is its own thing instead.
//
// # A different walk, necessarily
//
// [Indexer] LAPS its sources by id, a batch at a time, with the cursor in
// memory and readiness meaning the first lap wrapped. That is right for a few
// thousand mutable documents and wrong for several million mostly-immutable
// ones: a lap over a year of chat is thousands of index-only scans, and a
// restart re-walks from the beginning.
//
// This one walks FORWARD ONLY, over `chat_messages.version` — the packed log
// position of the last record that changed a row. An edit and a tombstone both
// move it, so the forward walk re-visits exactly what changed and re-reads
// nothing else, and the watermark is durable so a restart resumes.
//
// # What a forward walk cannot see, and how each case is followed
//
// A row that VANISHED. Two things remove one and neither is silent:
//
//   - The retention prune, a committed record whose apply range-deletes below
//     a cutoff. The index follows it with the SAME predicate, per channel,
//     recording how far it has followed — so the work is bounded by what was
//     pruned rather than by the corpus.
//   - The compliance erase, which deletes one row and leaves a marker. The
//     index follows the markers, which is a handful of rows ever.
//
// A corpus-wide orphan pass would cover both and is exactly the lap this walk
// exists to avoid.

// ChatIndexBatch is how many messages one index transaction carries.
//
// TWO HUNDRED and FIFTY, against the knowledge indexer's twenty, and the
// difference is the document: a page is kilobytes and a message is a sentence,
// so the per-row cost here is dominated by the transaction rather than by
// tokenising. Two hundred and fifty messages is roughly a busy channel's
// half-hour, which keeps a batch inside the node's own writer for a short
// enough time that an applier is never visibly behind one.
const ChatIndexBatch = 250

// ChatPruneFollowChannels bounds how many channels one pass follows a prune
// into.
//
// SIXTY-FOUR, the same number the prune duty itself publishes per tick, so the
// follower cannot fall behind the thing it follows while both run on the same
// cadence. A company at the channel cap is therefore caught up within about a
// quarter of an hour of a sweep.
const ChatPruneFollowChannels = 64

// ChatIndexer maintains the inverted list over a company's chat.
type ChatIndexer struct {
	db *store.DB
}

// NewChatIndexer builds one over this node's store.
func NewChatIndexer(db *store.DB) *ChatIndexer { return &ChatIndexer{db: db} }

// chatDoc is one message on its way into the index.
type chatDoc struct {
	id        string
	channelID string
	body      string
	createdAt int64
	version   int64
	deleted   bool
}

// Sweep advances the index by at most one batch and reports how many documents
// it wrote.
//
// ONE BATCH PER CALL rather than a loop to exhaustion, so the caller owns the
// cadence and a cold start cannot hold this node's only writer for the length
// of a backfill. A zero with no error means the index is caught up.
func (x *ChatIndexer) Sweep(ctx context.Context, now time.Time) (int, error) {
	if err := x.followPrunes(ctx, now); err != nil {
		return 0, err
	}
	if err := x.followErasures(ctx); err != nil {
		return 0, err
	}
	return x.advance(ctx, now)
}

// advance indexes the next batch of changed messages.
func (x *ChatIndexer) advance(ctx context.Context, now time.Time) (int, error) {
	through, err := x.watermark(ctx)
	if err != nil {
		return 0, err
	}
	docs, err := x.changedSince(ctx, through)
	if err != nil {
		return 0, err
	}
	if len(docs) == 0 {
		return 0, nil
	}

	written := 0
	err = x.db.Tx(ctx, func(tx *sql.Tx) error {
		highest := through
		for _, doc := range docs {
			if doc.version > highest {
				highest = doc.version
			}
			// A TOMBSTONE IS A DELETE HERE. The row survives in the
			// transcript so replies still resolve, and the body is
			// blanked — so leaving it indexed would answer a search
			// with a message whose text the company removed.
			if doc.deleted {
				//nolint:govet // shadow: scoped to this block; see .golangci.yml
				if err := x.removeDoc(ctx, tx, doc.id); err != nil {
					return err
				}
				continue
			}
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := x.writeDoc(ctx, tx, doc, now); err != nil {
				return err
			}
			written++
		}
		return x.setWatermark(ctx, tx, highest, now)
	})
	if err != nil {
		return 0, fmt.Errorf("search: index a batch of chat: %w", err)
	}
	return written, nil
}

// changedSince reads the next batch of messages past the watermark.
//
// STRICTLY GREATER THAN, and ordered by the same column, which is what makes
// the walk forward: a position is assigned by the log and is unique per
// record, so there is no row the walk can straddle and none it re-reads.
func (x *ChatIndexer) changedSince(ctx context.Context, through int64) ([]chatDoc, error) {
	var out []chatDoc
	err := x.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, channel_id, body, created_at, version, deleted_at IS NOT NULL
			  FROM chat_messages
			 WHERE version > ?
			 ORDER BY version
			 LIMIT ?`, through, ChatIndexBatch)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var doc chatDoc
			if err := rows.Scan(&doc.id, &doc.channelID, &doc.body,
				&doc.createdAt, &doc.version, &doc.deleted); err != nil {
				return err
			}
			out = append(out, doc)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("search: read the chat messages past %d: %w", through, err)
	}
	return out, nil
}

// writeDoc replaces one message's document and its postings.
//
// DELETE THEN INSERT rather than a merge, for the reason the knowledge indexer
// gives: a term the edit REMOVED has to go, and a merge would leave it
// matching for ever. The document row's own upsert carries the statistics
// adjustment, because a maintained count that is only correct on inserts is a
// count that drifts on the first edit.
func (x *ChatIndexer) writeDoc(ctx context.Context, tx *sql.Tx, doc chatDoc, now time.Time) error {
	counts := textindex.Analyze(doc.body)
	length := 0
	for _, n := range counts {
		length += n
	}

	previous, had, err := x.docLength(ctx, tx, doc.id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chat_postings WHERE doc_id = ?`, doc.id); err != nil {
		return fmt.Errorf("search: clear the postings for chat message %s: %w", doc.id, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chat_docs (id, channel_id, search_shard, excerpt, length,
		                       created_at, indexed_rev, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		    channel_id = excluded.channel_id,
		    excerpt = excluded.excerpt,
		    length = excluded.length,
		    created_at = excluded.created_at,
		    indexed_rev = excluded.indexed_rev,
		    indexed_at = excluded.indexed_at`,
		doc.id, doc.channelID, ShardOf(chatShardSource, doc.id),
		textcut.Within(doc.body, chatExcerptBytes), length,
		doc.createdAt, doc.version, now.UTC().UnixMilli()); err != nil {
		return fmt.Errorf("search: index chat message %s: %w", doc.id, err)
	}
	for term, freq := range counts {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chat_postings (term, doc_id, freq) VALUES (?, ?, ?)`,
			term, doc.id, freq); err != nil {
			return fmt.Errorf("search: write the posting for %q on chat message %s: %w",
				term, doc.id, err)
		}
	}

	delta := length
	docs := 1
	if had {
		delta -= previous
		docs = 0
	}
	return x.adjustCorpus(ctx, tx, docs, delta, now)
}

// removeDoc drops one message's document, its postings and its contribution to
// the corpus statistics.
func (x *ChatIndexer) removeDoc(ctx context.Context, tx *sql.Tx, id string) error {
	length, had, err := x.docLength(ctx, tx, id)
	if err != nil {
		return err
	}
	if !had {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chat_postings WHERE doc_id = ?`, id); err != nil {
		return fmt.Errorf("search: clear the postings for chat message %s: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chat_docs WHERE id = ?`, id); err != nil {
		return fmt.Errorf("search: remove chat message %s from the index: %w", id, err)
	}
	return x.adjustCorpus(ctx, tx, -1, -length, time.Time{})
}

// docLength reads a document's indexed token count, reporting whether it was
// indexed at all — which is what tells an edit from a first index.
func (x *ChatIndexer) docLength(ctx context.Context, tx *sql.Tx, id string) (int, bool, error) {
	var length int
	err := tx.QueryRowContext(ctx,
		`SELECT length FROM chat_docs WHERE id = ?`, id).Scan(&length)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("search: read the indexed length of chat message %s: %w", id, err)
	}
	return length, true, nil
}

// adjustCorpus moves the maintained statistics.
//
// MAINTAINED RATHER THAN COUNTED, which is the one thing this index does that
// the knowledge one does not: BM25 needs the corpus size and the average
// document length on EVERY query, and a `COUNT(*), AVG(length)` over several
// million rows is a full scan on the path of every question. The indexer
// already touches every row it changes, so it carries the two numbers along
// and a query reads one row.
//
// CLAMPED AT ZERO on the way down. A negative corpus is arithmetic nobody can
// act on, and the honest repair for a drifted count is a rebuild rather than a
// number that goes below empty.
func (x *ChatIndexer) adjustCorpus(ctx context.Context, tx *sql.Tx, docs, tokens int, now time.Time) error {
	stamp := now.UTC().UnixMilli()
	if now.IsZero() {
		stamp = 0
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE chat_index_state
		   SET doc_count   = MAX(0, doc_count + ?),
		       token_count = MAX(0, token_count + ?),
		       updated_at  = MAX(updated_at, ?)
		 WHERE id = 1`, docs, tokens, stamp)
	if err != nil {
		return fmt.Errorf("search: adjust the chat corpus statistics: %w", err)
	}
	return nil
}

// watermark reads how far this node has indexed.
func (x *ChatIndexer) watermark(ctx context.Context) (int64, error) {
	var through int64
	err := x.db.SQL().QueryRowContext(ctx,
		`SELECT through_rev FROM chat_index_state WHERE id = 1`).Scan(&through)
	if errors.Is(err, sql.ErrNoRows) {
		// The migration seeds the row, so its absence means a store this
		// build has not migrated — which is a caller's problem to see
		// rather than a zero to index from.
		return 0, errors.New("search: the chat index has no state row; the " +
			"node estate is behind its migrations")
	}
	if err != nil {
		return 0, fmt.Errorf("search: read the chat index watermark: %w", err)
	}
	return through, nil
}

// setWatermark advances it, never backwards.
func (x *ChatIndexer) setWatermark(ctx context.Context, tx *sql.Tx, through int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE chat_index_state
		   SET through_rev = MAX(through_rev, ?),
		       updated_at  = ?
		 WHERE id = 1`, through, now.UTC().UnixMilli())
	if err != nil {
		return fmt.Errorf("search: advance the chat index watermark: %w", err)
	}
	return nil
}

// Corpus reports the statistics a score needs.
func (x *ChatIndexer) Corpus(ctx context.Context) (textindex.Corpus, error) {
	var docs, tokens int64
	err := x.db.SQL().QueryRowContext(ctx,
		`SELECT doc_count, token_count FROM chat_index_state WHERE id = 1`).Scan(&docs, &tokens)
	if err != nil {
		return textindex.Corpus{}, fmt.Errorf("search: read the chat corpus statistics: %w", err)
	}
	corpus := textindex.Corpus{Docs: int(docs)}
	if docs > 0 {
		corpus.AvgLength = float64(tokens) / float64(docs)
	}
	return corpus, nil
}

// followPrunes deletes the documents of messages the retention prune removed.
//
// THE SAME RANGE PREDICATE the prune itself used, per channel, which is what
// makes this bounded: the prune deleted `created_at <= cutoff` in one channel,
// so the index deletes exactly that and records the cutoff it followed.
func (x *ChatIndexer) followPrunes(ctx context.Context, now time.Time) error {
	cutoffs, err := x.prunesToFollow(ctx)
	if err != nil || len(cutoffs) == 0 {
		return err
	}
	return x.db.Tx(ctx, func(tx *sql.Tx) error {
		for channel, cutoff := range cutoffs {
			if err := x.dropBelow(ctx, tx, channel, cutoff); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO chat_index_pruned (channel_id, followed_at, updated_at)
				VALUES (?, ?, ?)
				ON CONFLICT(channel_id) DO UPDATE SET
				    followed_at = MAX(chat_index_pruned.followed_at, excluded.followed_at),
				    updated_at = excluded.updated_at`,
				channel, cutoff, now.UTC().UnixMilli()); err != nil {
				return fmt.Errorf("search: record the prune followed in %s: %w", channel, err)
			}
		}
		return nil
	})
}

// prunesToFollow finds channels whose oldest INDEXED document predates the
// oldest message still in the transcript.
//
// DERIVED RATHER THAN SUBSCRIBED, because the prune is a committed record this
// node has already applied by the time the index runs: the evidence is the gap
// between what the index holds and what the transcript does, which needs no
// second delivery and cannot be missed by a node that was down for one.
func (x *ChatIndexer) prunesToFollow(ctx context.Context) (map[string]int64, error) {
	rows, err := x.db.SQL().QueryContext(ctx, `
		SELECT d.channel_id, MIN(d.created_at), COALESCE(p.followed_at, 0)
		  FROM chat_docs d
		  LEFT JOIN chat_index_pruned p ON p.channel_id = d.channel_id
		 GROUP BY d.channel_id
		 LIMIT ?`, ChatPruneFollowChannels)
	if err != nil {
		return nil, fmt.Errorf("search: read the chat index's own floors: %w", err)
	}
	defer func() { _ = rows.Close() }()

	oldest := map[string]int64{}
	for rows.Next() {
		var channel string
		var indexedFrom, followed int64
		if err := rows.Scan(&channel, &indexedFrom, &followed); err != nil {
			return nil, fmt.Errorf("search: scan a chat index floor: %w", err)
		}
		oldest[channel] = indexedFrom
		_ = followed
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: read the chat index's own floors: %w", err)
	}
	if len(oldest) == 0 {
		return nil, nil
	}

	cutoffs := map[string]int64{}
	for channel, indexedFrom := range oldest {
		var liveFrom int64
		err := x.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `
				SELECT COALESCE(MIN(created_at), 0) FROM chat_messages
				 WHERE channel_id = ?`, channel).Scan(&liveFrom)
		})
		if err != nil {
			return nil, fmt.Errorf("search: read the live floor of chat channel %s: %w",
				channel, err)
		}
		// A channel the prune emptied entirely reads as a live floor of
		// zero, and everything indexed for it is gone — which the
		// strictly-below delete would miss, so it takes the index's own
		// newest instead.
		if liveFrom == 0 {
			cutoffs[channel] = newestIndexed(ctx, x, channel)
			continue
		}
		if liveFrom > indexedFrom {
			cutoffs[channel] = liveFrom - 1
		}
	}
	return cutoffs, nil
}

// newestIndexed is the highest instant this index holds for a channel, which
// is the cutoff that empties it.
func newestIndexed(ctx context.Context, x *ChatIndexer, channel string) int64 {
	var newest int64
	_ = x.db.SQL().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(created_at), 0) FROM chat_docs WHERE channel_id = ?`,
		channel).Scan(&newest)
	return newest
}

// dropBelow removes a channel's documents at or below an instant, adjusting
// the corpus statistics by what it removed.
func (x *ChatIndexer) dropBelow(ctx context.Context, tx *sql.Tx, channel string, cutoff int64) error {
	var docs, tokens int64
	err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(length), 0) FROM chat_docs
		 WHERE channel_id = ? AND created_at <= ?`, channel, cutoff).Scan(&docs, &tokens)
	if err != nil {
		return fmt.Errorf("search: measure the prune in chat channel %s: %w", channel, err)
	}
	if docs == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM chat_postings WHERE doc_id IN (
		    SELECT id FROM chat_docs WHERE channel_id = ? AND created_at <= ?)`,
		channel, cutoff); err != nil {
		return fmt.Errorf("search: prune the postings in chat channel %s: %w", channel, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chat_docs WHERE channel_id = ? AND created_at <= ?`,
		channel, cutoff); err != nil {
		return fmt.Errorf("search: prune the documents in chat channel %s: %w", channel, err)
	}
	return x.adjustCorpus(ctx, tx, int(-docs), int(-tokens), time.Time{})
}

// followErasures removes the documents of messages a compliance erase
// destroyed.
//
// A HANDFUL OF ROWS EVER, so this is a join rather than a walk: an erase is an
// operator gesture on a named message, not a sweep.
func (x *ChatIndexer) followErasures(ctx context.Context) error {
	var erased []string
	err := x.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT message_id FROM chat_deletions`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			erased = append(erased, id)
		}
		return rows.Err()
	})
	if err != nil {
		return fmt.Errorf("search: read the chat erasures: %w", err)
	}
	if len(erased) == 0 {
		return nil
	}
	return x.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, id := range erased {
			if err := x.removeDoc(ctx, tx, id); err != nil {
				return err
			}
		}
		return nil
	})
}

// chatShardSource is the namespace a message's search bucket is hashed under.
//
// A LOCAL CONSTANT rather than a member of [Source], deliberately: that enum is
// what the embedding duty's own certification derives its corpora from, so a
// value added there would enrol chat in a duty this design refuses to give it.
// What the shard needs is a stable namespace string, and this is one.
const chatShardSource = "message"

// chatExcerptBytes is how much of a message the index keeps for rendering.
//
// Six hundred, the excerpt every other surface in this engine uses, so a hit
// in chat renders at the same size as a hit in the knowledge base and a
// screen showing both does not have two idea of how long a preview is.
const chatExcerptBytes = 600
