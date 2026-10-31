package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textindex"
	"github.com/crewlet/crewlet/internal/toolname"
)

// The chat corpus's own keyword index, and why it is not the knowledge base's.
//
// # A different corpus, deliberately
//
// Chat is in NEITHER half of the knowledge corpus (ADR-0071). The argument is
// already in this package: [TaskSource] does not index a work item's comment
// thread, because a thread is a conversation ABOUT the item rather than a
// statement of it and indexing it would make one busy item outrank every
// concise one on any word said in passing. A chat message is that shape at ten
// times the volume.
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
//   - The retention prune, a committed record whose apply range-deletes a
//     room's messages below a cutoff. The index follows it ROOM BY ROOM, in a
//     rotation over the rooms it holds documents for, comparing the oldest
//     document it holds with the oldest message the room's transcript still
//     has: what lies below the second has no row any more, so the work is
//     bounded by what was pruned rather than by the corpus.
//   - The compliance erase, which deletes one row and leaves a marker stamped
//     with the erase's own position. The index walks the markers FORWARD from
//     a watermark of its own, exactly as it walks the messages, because they
//     are never pruned and a pass over all of them would grow with the
//     deployment's age.
//
// A corpus-wide orphan pass would cover both and is exactly the lap this walk
// exists to avoid.
//
// And a whole estate that was REPLACED: an adoption puts a donor's file in
// place, whose rows below the watermark this walk will never read again. The
// rows cannot say so, but the FILE can: a join that installs a copy names it
// with a freshly minted instance ([statelog.EstateInstance]) before it is in
// place, the index records the instance it was built from, and every sweep
// compares the two — so a replacement is seen whenever it happened, a restart
// between the install and the next sweep included. The engine also says so the
// moment it replaces the estate under a running node
// ([ChatIndexer.RequestRebuild]), which only makes a search stop trusting the
// index sooner. Either way the index starts over — dropped a batch at a time
// from a durable mark, never in one transaction, for the reason
// [ChatIndexer.Sweep] takes one batch per call.
//
// # Statistics per room, maintained
//
// A search is ranked by the statistics of the rooms it SEARCHES, never the
// company's — [ChatIndexer.SearchMessages] says why — so the index keeps each
// room's document and token counts (`chat_index_rooms`) and how many of its
// documents carry each term (`chat_index_terms`), and moves them by the
// DIFFERENCE every write makes. A count that is right on an insert and wrong on
// an edit drifts on the first edit with nothing to say so, which is what
// TestTheMaintainedStatisticsAreTheCountedOnes holds against a recount.

// ChatIndexBatch is how many messages one index transaction carries.
//
// TWO HUNDRED and FIFTY, against the knowledge indexer's twenty, and the
// difference is the document: a page is kilobytes and a message is a sentence,
// so the per-row cost here is dominated by the transaction rather than by
// tokenising. Two hundred and fifty messages is roughly a busy channel's
// half-hour, which keeps a batch inside the node's own writer for a short
// enough time that an applier is never visibly behind one.
const ChatIndexBatch = 250

// ChatPruneFollowChannels is how many rooms one sweep's prune follower visits.
//
// SIXTY-FOUR, and the rotation is what bounds it: a visit is two index seeks —
// the oldest document this index holds for the room and the oldest message the
// room still has — so a sweep reads at most 128 rows to decide, and a company
// with a thousand rooms holding messages comes round in sixteen sweeps, about
// half a minute of idle ticks. The prune duty it follows publishes once an
// hour, so a room is caught up long before the next prune lands in it, and in
// between a search skips every pruned hit at hydration anyway.
const ChatPruneFollowChannels = 64

// ChatIndexer maintains the inverted list over a company's chat.
type ChatIndexer struct {
	// db is this node's own estate, where the index is kept, and estate the
	// replicated estate whose messages it indexes — read only, because the
	// messages are an applier's rows and the index is not. The same split
	// [Indexer] keeps, for the same reason: this is a reader that CROSSES THE
	// ESTATE BOUNDARY, so every comparison is a batch from each side rather
	// than a JOIN.
	db     *store.DB
	estate store.ReplicatedReader

	// pruneCursor is the last room the prune follower's rotation visited.
	//
	// IN MEMORY, for [Indexer]'s reason about its own cursors: losing it
	// costs one extra rotation over rooms that are already caught up, and a
	// durable cursor would be a second piece of state to keep in step with
	// an index that is itself rebuildable. UNGUARDED because
	// [ChatIndexer.Run] is ONE loop and every sweep runs inside it.
	pruneCursor string

	// rebuild is a request, from whichever goroutine noticed the estate was
	// replaced, that the next sweep start over. A FLAG THE LOOP HONOURS
	// rather than a reset done where it is noticed: a reset racing a sweep
	// that had read the old watermark would have that sweep's write raise it
	// back past every row the reset meant to re-read. The loop turns it into
	// the DURABLE mark the drop works from (`chat_index_state.rebuilding`),
	// so a process that stops halfway through a drop finishes it on its next
	// start rather than resuming a walk over an index it half emptied.
	rebuild atomic.Bool
}

// NewChatIndexer builds one over this node's own store and the replicated
// estate beside it — handed in by the runtime, like [NewIndexer]'s, because
// nothing outside it reaches the estate through the node's own handle.
func NewChatIndexer(db *store.DB, estate store.ReplicatedReader) *ChatIndexer {
	return &ChatIndexer{db: db, estate: estate}
}

// RequestRebuild asks the index to drop everything it holds and walk the
// transcript again from its start.
//
// FOR AN ESTATE THAT WAS REPLACED, the one change a forward walk cannot see: an
// adoption puts a donor's rows in place, and every row below the watermark is
// one this walk will never read again — so a document indexed from a copy that
// was WRONG would answer searches for as long as the deployment lived. Search
// is partial from this call until the walk catches up again, and
// [ChatIndexer.Ready] says so meanwhile.
//
// THE NUDGE, NOT THE GUARANTEE. The request is held in memory, and a process
// that stops before its next sweep loses it; what survives that is the estate's
// own instance, which every sweep compares with the one the index was built
// from ([ChatIndexer.Sweep]). This call is what makes the index stop answering
// as caught up from the instant the engine knows, rather than from the next
// sweep.
func (x *ChatIndexer) RequestRebuild() { x.rebuild.Store(true) }

// Ready reports whether this index holds everything the transcript beside it
// does: whether a search is answered over every message this node holds,
// rather than over the part of it the walk has reached.
//
// MEASURED, NEVER REMEMBERED. It is the index's own durable watermarks against
// the transcript, read now: no `chat_messages` row past the transcript walk's
// (a post, an edit and a tombstone each move a row's version) and no
// `chat_deletions` marker past the erase walk's, with no drop under way and the
// index built from the copy of the estate beside it.
// A flag set the first time the walk reached the end said nothing about the
// messages posted after it — a node whose walk had once caught up answered
// every later search as caught up, through any backlog, until a rebuild.
//
// A STANDING REQUEST IS NOT READY, from the instant it is made: the index it
// would drop was built from the estate that was replaced, so a search answered
// before the loop takes the request is answered over the wrong copy.
//
// WHAT IT DOES NOT MEASURE is the prune follower, which has no watermark: a
// document whose message the retention prune took stays in the index for up
// to one rotation, and costs only its weight in the statistics — a search
// re-reads every hit from the transcript and skips one that is gone.
//
// THREE-VALUED: an error is "this node could not tell", which is neither
// answer, and the caller decides what that makes a search.
func (x *ChatIndexer) Ready(ctx context.Context) (bool, error) {
	if x == nil || x.rebuild.Load() {
		return false, nil
	}
	state, err := x.indexState(ctx)
	if err != nil {
		return false, err
	}
	if state.rebuilding {
		return false, nil
	}
	var (
		instance string
		behind   bool
	)
	err = x.estate.Read(ctx, func(tx *sql.Tx) error {
		var readErr error
		if instance, readErr = statelog.EstateInstance(ctx, tx); readErr != nil {
			return readErr
		}
		behind = false
		for _, past := range []struct {
			statement string
			through   int64
		}{
			{chatPastWatermarkStatement, state.through},
			{chatPastErasedStatement, state.erased},
		} {
			var one int
			switch scanErr := tx.QueryRowContext(ctx, past.statement, past.through).Scan(&one); {
			case errors.Is(scanErr, sql.ErrNoRows):
			case scanErr != nil:
				return scanErr
			default:
				behind = true
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("search: read how far the chat transcript "+
			"reaches: %w", err)
	}
	return state.builtFrom == instance && !behind, nil
}

// chatDoc is one message on its way into the index.
type chatDoc struct {
	id        string
	channelID string
	author    string
	body      string
	createdAt int64
	version   int64
	deleted   bool
}

// Sweep advances the index by at most one batch of each walk and one step of
// the prune follower's rotation — or, while a rebuild is dropping the index,
// by one batch of that drop and nothing else — and reports how much it did:
// the transcript rows it walked, the erase markers it followed, the documents
// a prune took and the documents a rebuild dropped.
//
// ONE BATCH PER CALL rather than a loop to exhaustion, so the caller owns the
// cadence and a cold start cannot hold this node's only writer for the length
// of a backfill — nor a rebuild for the length of a drop, which at a year of
// chat is tens of millions of postings in one transaction. A zero with no
// error means both forward walks are caught up, no drop is under way and the
// rooms this step of the rotation visited had nothing pruned; the rotation
// comes round the rest within [ChatPruneFollowChannels]'s bound. A followed
// marker and a walked row count whatever they changed, because a backlog of
// erasures, or a stretch of tombstones for messages the index never held, is
// work the caller's loop must come straight back for, not an idle tick's.
func (x *ChatIndexer) Sweep(ctx context.Context, now time.Time) (int, error) {
	// WHICH COPY OF THE ESTATE THIS IS, read before anything is decided, so
	// a rebuild begun below records the copy it will walk.
	instance, err := x.estateInstance(ctx)
	if err != nil {
		return 0, err
	}
	if x.rebuild.Load() {
		if beginErr := x.beginRebuild(ctx, now, "requested"); beginErr != nil {
			// THE REQUEST STANDS until its mark commits: dropped with
			// the failure, it would leave the replaced estate's rows
			// unread for good.
			return 0, beginErr
		}
		// THE REQUEST IS TAKEN ONCE ITS MARK COMMITS, and not before, so
		// no instant between the two reads as ready ([ChatIndexer.Ready]):
		// the flag answers until the durable mark does. A request made
		// while the mark was committing is answered by the same rebuild,
		// whose drop has not begun.
		x.rebuild.Store(false)
	}
	state, err := x.indexState(ctx)
	if err != nil {
		return 0, err
	}
	// THE ESTATE WAS REPLACED SINCE THIS INDEX BEGAN, whether or not the
	// process that replaced it lived to say so. Not asked while a drop is
	// under way: whatever the estate is called by the time it ends, the walk
	// that follows starts from nothing and records the copy it starts on.
	if !state.rebuilding && state.builtFrom != instance {
		if state.rebuilding, err = x.followInstance(ctx, now, state, instance); err != nil {
			return 0, err
		}
	}
	if state.rebuilding {
		return x.dropStep(ctx, now)
	}
	pruned, err := x.followPrunes(ctx)
	if err != nil {
		return 0, err
	}
	followed, err := x.followErasures(ctx, now)
	if err != nil {
		return pruned, err
	}
	walked, err := x.advance(ctx, now)
	return pruned + followed + walked, err
}

// The statements the index's walks and writes issue, named so the plan gate
// explains these rather than copies of them
// (TestEveryChatIndexServesARegisteredQuery).
const (
	// chatChangedReachStatement is how far the next transcript batch
	// reaches: the position of the last of the next [ChatIndexBatch] rows
	// past the watermark, on the transcript's own watermark index.
	chatChangedReachStatement = `
		SELECT MAX(version) FROM (
		    SELECT version FROM chat_messages
		     WHERE version > ? ORDER BY version LIMIT ?)`

	// chatChangedRowsStatement reads that batch WHOLE: every row past the
	// watermark up to the position it reaches. A ROOM EVENT is read as a
	// row the index holds no document for, as a tombstone is: it is the
	// room's own change rather than anything said there, and an empty
	// document would count toward its room's statistics with no word to
	// match.
	chatChangedRowsStatement = `
		SELECT id, channel_id, author_handle, body, created_at, version,
		       deleted_at IS NOT NULL OR event <> ''
		  FROM chat_messages
		 WHERE version > ? AND version <= ?
		 ORDER BY version`

	// chatErasedReachStatement and chatErasedRowsStatement are the same
	// pair over the erase markers' positions.
	chatErasedReachStatement = `
		SELECT MAX(version) FROM (
		    SELECT version FROM chat_deletions
		     WHERE version > ? ORDER BY version LIMIT ?)`
	chatErasedRowsStatement = `
		SELECT message_id FROM chat_deletions
		 WHERE version > ? AND version <= ?
		 ORDER BY version`

	// chatLiveOldestStatement is the oldest message one room's transcript
	// still holds, on the retention prune's own index.
	chatLiveOldestStatement = `
		SELECT created_at FROM chat_messages
		 WHERE channel_id = ? ORDER BY created_at LIMIT 1`

	// chatHeldOldestStatement is the oldest document this index holds for
	// one room.
	chatHeldOldestStatement = `
		SELECT created_at FROM chat_docs
		 WHERE channel_id = ? ORDER BY created_at LIMIT 1`

	// chatPastWatermarkStatement and chatPastErasedStatement ask whether
	// the transcript, or its erase markers, hold a row past a watermark —
	// what [ChatIndexer.Ready] asks of the index's two. A SEEK to the
	// watermark on the index the walk drives on, answered by its first row:
	// `MAX(version)` is the same question and this planner answers it with a
	// walk of the whole index, on every search.
	chatPastWatermarkStatement = `SELECT 1 FROM chat_messages WHERE version > ? LIMIT 1`
	chatPastErasedStatement    = `SELECT 1 FROM chat_deletions WHERE version > ? LIMIT 1`

	// chatRoomRotationStatement is the next step of the prune follower's
	// rotation over the rooms this index holds documents for.
	chatRoomRotationStatement = `
		SELECT channel_id FROM chat_index_rooms
		 WHERE channel_id > ? ORDER BY channel_id LIMIT ?`

	// chatHeldDocStatement is what the index holds for one message: the
	// room and the length its statistics were counted under.
	chatHeldDocStatement = `SELECT channel_id, length FROM chat_docs WHERE id = ?`

	// chatDocTermsStatement is one document's terms, which a re-index or a
	// removal subtracts from its room's term counts.
	chatDocTermsStatement = `SELECT term FROM chat_postings WHERE doc_id = ?`

	// chatClearPostingsStatement drops one document's postings before its
	// replacement is written.
	chatClearPostingsStatement = `DELETE FROM chat_postings WHERE doc_id = ?`

	// chatRemoveDocStatement removes one document; its postings cascade.
	chatRemoveDocStatement = `DELETE FROM chat_docs WHERE id = ?`

	// chatDocUpsertStatement writes one document.
	chatDocUpsertStatement = `
		INSERT INTO chat_docs (id, channel_id, author, search_shard, length, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
		    channel_id = excluded.channel_id,
		    author     = excluded.author,
		    length     = excluded.length,
		    created_at = excluded.created_at`

	// chatRangeSizeStatement, chatRangeTermsStatement and
	// chatRangeDropStatement are a prune's: what a room holds before an
	// instant, the terms those documents carry, and their removal.
	//
	// THE TERMS ARE READ FROM THE DOCUMENTS, which CROSS JOIN fixes: joined
	// plainly, the planner is free to drive on the postings and walk every
	// term in the corpus to find the ones a room's oldest documents carry.
	chatRangeSizeStatement = `
		SELECT COUNT(*), COALESCE(SUM(length), 0) FROM chat_docs
		 WHERE channel_id = ? AND created_at < ?`
	chatRangeTermsStatement = `
		SELECT p.term, COUNT(*)
		  FROM chat_docs AS d CROSS JOIN chat_postings AS p
		 WHERE d.channel_id = ? AND d.created_at < ? AND p.doc_id = d.id
		 GROUP BY p.term`
	chatRangeDropStatement = `
		DELETE FROM chat_docs WHERE channel_id = ? AND created_at < ?`

	// chatWatermarkStatement and chatErasedThroughStatement read the two
	// walks' positions, and chatStateStatement the whole row: which copy of
	// the estate the index was built from, both positions and whether a
	// rebuild is dropping it.
	chatWatermarkStatement     = `SELECT through_rev FROM chat_index_state WHERE id = 1`
	chatErasedThroughStatement = `SELECT erased_through FROM chat_index_state WHERE id = 1`
	chatStateStatement         = `
		SELECT built_from, through_rev, erased_through, rebuilding
		  FROM chat_index_state WHERE id = 1`

	// chatAdvanceWatermarkStatement and chatAdvanceErasedStatement move the
	// two walks' positions, never backwards.
	chatAdvanceWatermarkStatement = `
		UPDATE chat_index_state
		   SET through_rev = MAX(through_rev, ?), updated_at = ?
		 WHERE id = 1`
	chatAdvanceErasedStatement = `
		UPDATE chat_index_state
		   SET erased_through = MAX(erased_through, ?), updated_at = ?
		 WHERE id = 1`

	// chatBeginRebuildStatement is a rebuild's durable mark: both walks back
	// at the transcript's start, and the drop under way. ONE STATEMENT, so a
	// process that stops after it finds both halves on its next start and
	// one that stops before it finds neither.
	chatBeginRebuildStatement = `
		UPDATE chat_index_state
		   SET rebuilding = 1, through_rev = 0, erased_through = 0, updated_at = ?
		 WHERE id = 1`

	// chatRecordInstanceStatement names the copy an index that has indexed
	// NOTHING is about to be built from — a fresh node's, or one whose
	// rebuild's drop has just ended — and it is the ONE writer of
	// `built_from`, so what the column names is always the copy the walk
	// began on. Conditional on the row being that index, so it can never
	// stand in for the rebuild an index that holds documents owes.
	chatRecordInstanceStatement = `
		UPDATE chat_index_state SET built_from = ?, updated_at = ?
		 WHERE id = 1 AND through_rev = 0 AND erased_through = 0 AND rebuilding = 0`

	// chatDropBatchStatement is the next batch of documents a rebuild drops.
	// A WALK by design, taken from the front of the table a batch at a time:
	// whatever order it reads in, every document it names is gone by the
	// next one.
	chatDropBatchStatement = `SELECT id FROM chat_docs LIMIT ?`

	// chatRebuiltStatement ends a rebuild's drop, so the walk starts over.
	chatRebuiltStatement = `
		UPDATE chat_index_state SET rebuilding = 0, updated_at = ? WHERE id = 1`

	// chatCorpusStatement totals every room's statistics. A WALK of the
	// rooms table, which holds one row per room with a document in it —
	// never the documents themselves.
	chatCorpusStatement = `
		SELECT COALESCE(SUM(docs), 0), COALESCE(SUM(tokens), 0) FROM chat_index_rooms`
)

// chatRebuiltDrops are what a rebuild's last step deletes once every document
// is gone: whatever statistics are left. WALKS by design, and of rows that
// should not be there — every document's drop took its own counts out — so
// what they remove is drift, which a rebuild is the one moment to discard
// rather than carry into the counts the walk starts again from.
var chatRebuiltDrops = []string{
	`DELETE FROM chat_index_terms`,
	`DELETE FROM chat_index_rooms`,
}

// advance indexes the next batch of changed messages, and reports how many
// transcript rows it walked.
func (x *ChatIndexer) advance(ctx context.Context, now time.Time) (int, error) {
	through, err := x.position(ctx, chatWatermarkStatement, "the chat index watermark")
	if err != nil {
		return 0, err
	}
	docs, reached, err := x.changedSince(ctx, through)
	if err != nil {
		return 0, err
	}
	if len(docs) == 0 {
		// NOTHING PAST THE WATERMARK: every message this node holds is
		// indexed — which [ChatIndexer.Ready] measures for itself.
		return 0, nil
	}

	maxVariables := x.db.Caps().MaxVariables
	err = x.db.Tx(ctx, func(tx *sql.Tx) error {
		// FRESH PER ATTEMPT: a transaction the store retries after a
		// snapshot race runs this function again, and a delta carried
		// over from the abandoned attempt would count the batch twice.
		delta := newChatDelta()
		for _, doc := range docs {
			// A TOMBSTONE IS A DELETE HERE. The row survives in the
			// transcript so replies still resolve, and the body is
			// blanked — so leaving it indexed would answer a search
			// with a message whose text the company removed.
			if doc.deleted {
				if removeErr := x.removeDoc(ctx, tx, doc.id, delta); removeErr != nil {
					return removeErr
				}
				continue
			}
			if writeErr := x.writeDoc(ctx, tx, doc, delta, maxVariables); writeErr != nil {
				return writeErr
			}
		}
		if applyErr := delta.apply(ctx, tx, maxVariables); applyErr != nil {
			return applyErr
		}
		if _, execErr := tx.ExecContext(ctx, chatAdvanceWatermarkStatement,
			reached, now.UTC().UnixMilli()); execErr != nil {
			return fmt.Errorf("search: advance the chat index watermark: %w", execErr)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("search: index a batch of chat: %w", err)
	}
	// THE ROWS WALKED, not the documents changed: a batch of tombstones for
	// messages the index never held changes nothing and still moved the
	// watermark, and reported as zero it would read to [ChatIndexer.Run] as
	// a caught-up walk and put an idle tick between every such batch.
	return len(docs), nil
}

// changedSince reads the next batch of messages past the watermark, and the
// position it reaches.
//
// A BATCH ENDS ON A WHOLE POSITION: the first [ChatIndexBatch] rows past the
// watermark, widened to every row at the last position they reach, read in one
// snapshot so the two halves agree. Every message a record changes carries that
// record's position, and a batch cut at a row count inside one position would
// advance the watermark past the rest of it — those rows would never be
// indexed. Today a record changes at most one message, so the widening adds
// nothing; it is here so that stays a property of the applier rather than an
// assumption this walk silently depends on, and it is the erase walk's rule.
func (x *ChatIndexer) changedSince(ctx context.Context, through int64) ([]chatDoc, int64, error) {
	var (
		out     []chatDoc
		reached int64
	)
	err := x.estate.Read(ctx, func(tx *sql.Tx) error {
		out, reached = nil, 0
		var reach sql.NullInt64
		if err := tx.QueryRowContext(ctx, chatChangedReachStatement,
			through, ChatIndexBatch).Scan(&reach); err != nil {
			return err
		}
		if !reach.Valid {
			return nil
		}
		reached = reach.Int64
		rows, err := tx.QueryContext(ctx, chatChangedRowsStatement, through, reached)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var doc chatDoc
			if err := rows.Scan(&doc.id, &doc.channelID, &doc.author, &doc.body,
				&doc.createdAt, &doc.version, &doc.deleted); err != nil {
				return err
			}
			out = append(out, doc)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, fmt.Errorf("search: read the chat messages past %d: %w", through, err)
	}
	return out, reached, nil
}

// writeDoc replaces one message's document and its postings, and counts what
// the replacement changed into its room's statistics.
//
// DELETE THEN INSERT rather than a merge, for the reason the knowledge indexer
// gives: a term the edit REMOVED has to go, and a merge would leave it
// matching for ever. The postings go in ONE STATEMENT PER CHUNK rather than one
// per term, the shape [store.InsertRows] exists for: a row per unique term per
// message, an ExecContext each, is the shape the drain benchmark measures as
// the slowest.
func (x *ChatIndexer) writeDoc(ctx context.Context, tx *sql.Tx, doc chatDoc,
	delta *chatDelta, maxVariables int) error {

	counts := textindex.Analyze(doc.body)
	postings := make([]posting, 0, len(counts))
	terms := make([]string, 0, len(counts))
	length := 0
	for term, n := range counts {
		postings = append(postings, posting{term: term, freq: n})
		terms = append(terms, term)
		length += n
	}

	held, had, err := x.heldDoc(ctx, tx, doc.id)
	if err != nil {
		return err
	}
	if had {
		gone, err := x.docTerms(ctx, tx, doc.id)
		if err != nil {
			return err
		}
		delta.drop(held.channelID, 1, held.length, gone)
		if _, err := tx.ExecContext(ctx, chatClearPostingsStatement, doc.id); err != nil {
			return fmt.Errorf("search: clear the postings for chat message %s: %w", doc.id, err)
		}
	}
	if _, err := tx.ExecContext(ctx, chatDocUpsertStatement,
		doc.id, doc.channelID, doc.author, ChatShardOf(doc.id), length,
		doc.createdAt); err != nil {
		return fmt.Errorf("search: index chat message %s: %w", doc.id, err)
	}
	// NO CONFLICT CLAUSE: the delete above cleared this document's list, and
	// (term, doc_id) is unique within it because `counts` is keyed on the
	// term — a duplicate would be the index contradicting itself.
	if _, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO chat_postings (term, doc_id, freq) VALUES`, `(?, ?, ?)`, "",
		len(postings), func(i int) []any {
			return []any{postings[i].term, doc.id, postings[i].freq}
		}); err != nil {
		return fmt.Errorf("search: write %d postings for chat message %s: %w",
			len(postings), doc.id, err)
	}
	delta.add(doc.channelID, length, terms)
	return nil
}

// removeDoc drops one message's document — its postings cascade — and counts
// what that takes out of its room's statistics. A message the index does not
// hold is nothing to drop.
func (x *ChatIndexer) removeDoc(ctx context.Context, tx *sql.Tx, id string,
	delta *chatDelta) error {

	held, had, err := x.heldDoc(ctx, tx, id)
	if err != nil || !had {
		return err
	}
	terms, err := x.docTerms(ctx, tx, id)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, chatRemoveDocStatement, id); err != nil {
		return fmt.Errorf("search: remove chat message %s from the index: %w", id, err)
	}
	delta.drop(held.channelID, 1, held.length, terms)
	return nil
}

// heldChatDoc is what the index holds for one message.
type heldChatDoc struct {
	channelID string
	length    int
}

// heldDoc reads what the index holds for one message, reporting whether it is
// indexed at all — which is what tells an edit from a first index.
func (x *ChatIndexer) heldDoc(ctx context.Context, tx *sql.Tx, id string) (heldChatDoc, bool, error) {
	var held heldChatDoc
	err := tx.QueryRowContext(ctx, chatHeldDocStatement, id).Scan(&held.channelID, &held.length)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return heldChatDoc{}, false, nil
	case err != nil:
		return heldChatDoc{}, false, fmt.Errorf("search: read the indexed chat message %s: %w", id, err)
	}
	return held, true, nil
}

// docTerms reads one document's terms.
func (x *ChatIndexer) docTerms(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, chatDocTermsStatement, id)
	if err != nil {
		return nil, fmt.Errorf("search: read the terms of chat message %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var terms []string
	for rows.Next() {
		var term string
		if err := rows.Scan(&term); err != nil {
			return nil, fmt.Errorf("search: scan a term of chat message %s: %w", id, err)
		}
		terms = append(terms, term)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: read the terms of chat message %s: %w", id, err)
	}
	return terms, nil
}

// position reads one of the two walks' watermarks.
func (x *ChatIndexer) position(ctx context.Context, statement, what string) (int64, error) {
	var through int64
	err := x.db.SQL().QueryRowContext(ctx, statement).Scan(&through)
	if errors.Is(err, sql.ErrNoRows) {
		// The migration seeds the row, so its absence means a store this
		// build has not migrated — which is a caller's problem to see
		// rather than a zero to index from.
		return 0, errors.New("search: the chat index has no state row; the " +
			"node estate is behind its migrations")
	}
	if err != nil {
		return 0, fmt.Errorf("search: read %s: %w", what, err)
	}
	return through, nil
}

// Corpus reports what the whole index holds: every room's documents and their
// average length. A search ranks by the rooms it searches instead; this is the
// index's own size, for whoever reports it.
func (x *ChatIndexer) Corpus(ctx context.Context) (textindex.Corpus, error) {
	var docs, tokens int64
	err := x.db.SQL().QueryRowContext(ctx, chatCorpusStatement).Scan(&docs, &tokens)
	if err != nil {
		return textindex.Corpus{}, fmt.Errorf("search: read the chat corpus statistics: %w", err)
	}
	return corpusOf(docs, tokens), nil
}

// corpusOf is BM25's two statistics from a document and a token count.
func corpusOf(docs, tokens int64) textindex.Corpus {
	corpus := textindex.Corpus{Docs: int(docs)}
	if docs > 0 {
		corpus.AvgLength = float64(tokens) / float64(docs)
	}
	return corpus
}

// followPrunes removes the documents of messages the retention prune took, in
// the next step of a rotation over the rooms this index holds documents for,
// and reports how many it removed.
//
// DERIVED RATHER THAN SUBSCRIBED, because the prune is a committed record this
// node has already applied by the time the index runs: the evidence is the gap
// between the oldest document the index holds for a room and the oldest message
// the room's transcript still has. Everything before the second has no row any
// more, whichever record removed it, so the follower deletes exactly that and
// needs no record of how far it has followed — which it would have to keep in
// step with what it derives from.
//
// A ROTATION rather than a pass over every room, so a sweep costs
// [ChatPruneFollowChannels] rooms rather than the company's. The cursor moves
// only once the step has committed, so a failed step is retried over the same
// rooms rather than skipping them until the rotation comes round.
func (x *ChatIndexer) followPrunes(ctx context.Context) (int, error) {
	rooms, err := x.nextRooms(ctx)
	if err != nil {
		return 0, err
	}
	if len(rooms) == 0 {
		x.pruneCursor = ""
		return 0, nil
	}
	held := make(map[string]int64, len(rooms))
	for _, room := range rooms {
		var oldest int64
		readErr := x.db.SQL().QueryRowContext(ctx, chatHeldOldestStatement, room).Scan(&oldest)
		switch {
		case errors.Is(readErr, sql.ErrNoRows):
			continue
		case readErr != nil:
			return 0, fmt.Errorf("search: read the oldest indexed message in "+
				"chat room %s: %w", room, readErr)
		}
		held[room] = oldest
	}
	live := make(map[string]int64, len(held))
	if readErr := x.estate.Read(ctx, func(tx *sql.Tx) error {
		clear(live)
		for room := range held {
			var oldest int64
			scanErr := tx.QueryRowContext(ctx, chatLiveOldestStatement, room).Scan(&oldest)
			switch {
			case errors.Is(scanErr, sql.ErrNoRows):
				// THE PRUNE EMPTIED THE ROOM, and every document
				// this index holds for it is gone from the
				// transcript.
			case scanErr != nil:
				return scanErr
			default:
				live[room] = oldest
			}
		}
		return nil
	}); readErr != nil {
		return 0, fmt.Errorf("search: read the oldest message each chat room "+
			"still holds: %w", readErr)
	}

	cutoffs := map[string]int64{}
	for room, indexedFrom := range held {
		liveFrom, kept := live[room]
		switch {
		case !kept:
			cutoffs[room] = math.MaxInt64
		case indexedFrom < liveFrom:
			cutoffs[room] = liveFrom
		}
	}
	removed := 0
	if len(cutoffs) > 0 {
		maxVariables := x.db.Caps().MaxVariables
		err = x.db.Tx(ctx, func(tx *sql.Tx) error {
			delta := newChatDelta()
			removed = 0
			for room, before := range cutoffs {
				n, dropErr := x.dropBefore(ctx, tx, room, before, delta)
				if dropErr != nil {
					return dropErr
				}
				removed += n
			}
			return delta.apply(ctx, tx, maxVariables)
		})
		if err != nil {
			return 0, fmt.Errorf("search: follow the chat retention prune: %w", err)
		}
	}
	x.pruneCursor = rooms[len(rooms)-1]
	if len(rooms) < ChatPruneFollowChannels {
		// THE ROTATION WRAPS: the next step starts again from the first
		// room.
		x.pruneCursor = ""
	}
	return removed, nil
}

// nextRooms reads the next step of the prune follower's rotation.
func (x *ChatIndexer) nextRooms(ctx context.Context) ([]string, error) {
	rows, err := x.db.SQL().QueryContext(ctx, chatRoomRotationStatement,
		x.pruneCursor, ChatPruneFollowChannels)
	if err != nil {
		return nil, fmt.Errorf("search: read the next chat rooms to follow: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var rooms []string
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			return nil, fmt.Errorf("search: scan a chat room to follow: %w", err)
		}
		rooms = append(rooms, room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: read the next chat rooms to follow: %w", err)
	}
	return rooms, nil
}

// dropBefore removes a room's documents stored strictly before an instant —
// their postings cascade — counts what they took out of its statistics, and
// reports how many it removed.
//
// STRICTLY BEFORE, which is the prune's own boundary: the oldest message the
// room still has is AT the instant, and it stays indexed.
func (x *ChatIndexer) dropBefore(ctx context.Context, tx *sql.Tx, room string,
	before int64, delta *chatDelta) (int, error) {

	var docs, tokens int64
	if err := tx.QueryRowContext(ctx, chatRangeSizeStatement, room, before).
		Scan(&docs, &tokens); err != nil {
		return 0, fmt.Errorf("search: measure the prune in chat room %s: %w", room, err)
	}
	if docs == 0 {
		return 0, nil
	}
	counts, err := x.rangeTerms(ctx, tx, room, before)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, chatRangeDropStatement, room, before); err != nil {
		return 0, fmt.Errorf("search: prune the documents in chat room %s: %w", room, err)
	}
	delta.dropCounted(room, int(docs), int(tokens), counts)
	return int(docs), nil
}

// rangeTerms counts, per term, the documents of one room before an instant
// that carry it.
func (x *ChatIndexer) rangeTerms(ctx context.Context, tx *sql.Tx, room string,
	before int64) (map[string]int, error) {

	rows, err := tx.QueryContext(ctx, chatRangeTermsStatement, room, before)
	if err != nil {
		return nil, fmt.Errorf("search: read the terms the prune takes from chat "+
			"room %s: %w", room, err)
	}
	defer func() { _ = rows.Close() }()
	counts := map[string]int{}
	for rows.Next() {
		var term string
		var n int
		if err := rows.Scan(&term, &n); err != nil {
			return nil, fmt.Errorf("search: scan a term the prune takes from chat "+
				"room %s: %w", room, err)
		}
		counts[term] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: read the terms the prune takes from chat "+
			"room %s: %w", room, err)
	}
	return counts, nil
}

// followErasures removes the documents of messages a compliance erase
// destroyed, and reports how many markers it followed.
//
// A FORWARD WALK over the markers' positions (replicated 0055) from a durable
// watermark of its own (node 0056) — the message walk's shape, for the message
// walk's reason. The markers are never pruned, because a marker is what makes
// a destruction permanent, so a join that read every one of them on every sweep
// would take this node's one writer for all of them every two seconds once
// anything had ever been erased: a cost that grew for the life of the
// deployment. Walked forward, an idle node reads one empty index range and
// writes nothing.
//
// A BATCH ENDS ON A WHOLE ERASE. Every marker one erase writes carries that
// record's position, so a batch cut at a row count inside one record would
// advance the watermark past the rest of it, and those messages would stay
// indexed for good. The batch is the first [ChatIndexBatch] markers widened to
// the end of the last record they reach — at most [ChatIndexBatch] plus
// chatlet.MaxEraseMessages less one — read in one snapshot so the two halves
// agree.
func (x *ChatIndexer) followErasures(ctx context.Context, now time.Time) (int, error) {
	through, err := x.position(ctx, chatErasedThroughStatement, "the chat erasure watermark")
	if err != nil {
		return 0, err
	}
	var erased []string
	var reached int64
	err = x.estate.Read(ctx, func(tx *sql.Tx) error {
		erased, reached = nil, 0
		var reach sql.NullInt64
		if scanErr := tx.QueryRowContext(ctx, chatErasedReachStatement,
			through, ChatIndexBatch).Scan(&reach); scanErr != nil {
			return scanErr
		}
		if !reach.Valid {
			return nil
		}
		reached = reach.Int64
		rows, queryErr := tx.QueryContext(ctx, chatErasedRowsStatement, through, reached)
		if queryErr != nil {
			return queryErr
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if scanErr := rows.Scan(&id); scanErr != nil {
				return scanErr
			}
			erased = append(erased, id)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, fmt.Errorf("search: read the chat erasures past %d: %w", through, err)
	}
	if len(erased) == 0 {
		return 0, nil
	}
	maxVariables := x.db.Caps().MaxVariables
	err = x.db.Tx(ctx, func(tx *sql.Tx) error {
		delta := newChatDelta()
		for _, id := range erased {
			if removeErr := x.removeDoc(ctx, tx, id, delta); removeErr != nil {
				return removeErr
			}
		}
		if applyErr := delta.apply(ctx, tx, maxVariables); applyErr != nil {
			return applyErr
		}
		if _, execErr := tx.ExecContext(ctx, chatAdvanceErasedStatement,
			reached, now.UTC().UnixMilli()); execErr != nil {
			return fmt.Errorf("search: advance the chat erasure watermark: %w", execErr)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("search: follow the chat erasures: %w", err)
	}
	return len(erased), nil
}

// beginRebuild writes a rebuild's durable mark: both walks back at the
// transcript's start, and the index's documents to be dropped a batch at a
// time by the sweeps that follow ([ChatIndexer.dropStep]).
//
// A MARK RATHER THAN THE DROP ITSELF, because the drop is the size of the
// index: measured on Turso at about five microseconds a posting, emptying the
// tables in one transaction holds this node's only writer for some 3.5 s at
// 80 000 messages, which at a year of chat at the declared census is minutes
// — every other write this node makes, the audit log's included, queued
// behind it. And DURABLE, because a drop that lived in memory and stopped
// halfway would leave the walk to resume from a watermark over an index it
// had half emptied.
func (x *ChatIndexer) beginRebuild(ctx context.Context, now time.Time, reason string) error {
	err := x.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, chatBeginRebuildStatement, now.UTC().UnixMilli())
		return err
	})
	if err != nil {
		return fmt.Errorf("search: mark the chat index for a rebuild: %w", err)
	}
	x.pruneCursor = ""
	log.InfoContext(ctx, "chat_index_rebuilding", "reason", reason,
		"detail", "the chat index is being dropped and walked again because "+
			"the replicated estate under it was replaced; "+toolname.SearchMessages+" "+
			"answers over what is indexed meanwhile, and Ready says false "+
			"until the walk catches up")
	return nil
}

// chatIndexState is the index's one state row, as a sweep decides from it.
type chatIndexState struct {
	// builtFrom is the estate instance the index was built from, empty
	// for one that has never recorded any.
	builtFrom string

	// through and erased are the two walks' positions.
	through, erased int64

	// rebuilding is whether a rebuild's drop is under way.
	rebuilding bool
}

// pristine reports whether the index has indexed nothing at all — which is
// what lets it adopt a copy's instance without dropping anything, since it
// holds nothing built from another one. Every document is written by the
// transcript walk in the transaction that moves its watermark, so a walk at
// zero with no drop under way holds none.
func (s chatIndexState) pristine() bool {
	return s.through == 0 && s.erased == 0 && !s.rebuilding
}

// indexState reads the index's one state row.
func (x *ChatIndexer) indexState(ctx context.Context) (chatIndexState, error) {
	var state chatIndexState
	var rebuilding int64
	err := x.db.SQL().QueryRowContext(ctx, chatStateStatement).
		Scan(&state.builtFrom, &state.through, &state.erased, &rebuilding)
	if errors.Is(err, sql.ErrNoRows) {
		return chatIndexState{}, errors.New("search: the chat index has no state " +
			"row; the node estate is behind its migrations")
	}
	if err != nil {
		return chatIndexState{}, fmt.Errorf("search: read the chat index's state: %w", err)
	}
	state.rebuilding = rebuilding != 0
	return state, nil
}

// estateInstance reads which copy of the replicated estate the index walks.
func (x *ChatIndexer) estateInstance(ctx context.Context) (string, error) {
	var instance string
	err := x.estate.Read(ctx, func(tx *sql.Tx) error {
		var readErr error
		instance, readErr = statelog.EstateInstance(ctx, tx)
		return readErr
	})
	if err != nil {
		return "", fmt.Errorf("search: read which copy of the replicated estate "+
			"the chat index walks: %w", err)
	}
	return instance, nil
}

// followInstance answers an index built from another copy of the estate than
// the one it now walks, and reports whether a rebuild's drop is now under way.
//
// AN INDEX THAT HOLDS NOTHING RECORDS THE COPY AND WALKS IT, since there is
// nothing to drop — the first sweep of a fresh node, or the first after a
// rebuild's drop ended. ANY OTHER IS REBUILT: the copy it was built from is
// gone, and every document below its watermark may describe a row the new copy
// holds differently or not at all. The rebuild records nothing itself; its drop
// ends with an index that holds nothing, and the sweep after it records the copy
// the walk then begins on.
func (x *ChatIndexer) followInstance(ctx context.Context, now time.Time,
	state chatIndexState, instance string) (bool, error) {

	if state.pristine() {
		recorded := false
		err := x.db.Tx(ctx, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, chatRecordInstanceStatement, instance,
				now.UTC().UnixMilli())
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			recorded = n == 1
			return err
		})
		if err != nil {
			return false, fmt.Errorf("search: record the copy of the estate the "+
				"chat index walks: %w", err)
		}
		if recorded {
			return false, nil
		}
	}
	log.InfoContext(ctx, "chat_index_estate_replaced",
		"built_from", state.builtFrom, "estate_instance", instance,
		"detail", "the chat index was built from another copy of the replicated "+
			"estate than the one under it now, so it is rebuilt from this one")
	if err := x.beginRebuild(ctx, now, "estate_instance_moved"); err != nil {
		return false, err
	}
	return true, nil
}

// dropStep removes the next batch of documents a rebuild drops, and reports
// how many — or, once none is left, discards whatever statistics remain and
// lifts the mark so the next sweep starts the walk over, reporting that as one
// unit of work so the caller's loop comes straight back for it.
//
// [ChatIndexBatch] DOCUMENTS A STEP, the walk's own unit, through the removal
// every other deletion here takes ([ChatIndexer.removeDoc]), so every count
// moves with its document: a search answered during the drop is ranked over
// exactly what is still held, and the statistics shrink with the documents
// rather than waiting at the end as one more delete the size of the index. A
// step holds the writer about as long as a batch of the walk does — measured
// on Turso at 80 000 messages, 160 ms on average and 250 ms at worst against
// 135 ms a walk batch — and a whole drop costs roughly what the walk that
// follows it does, which is the price of never holding the writer for more.
func (x *ChatIndexer) dropStep(ctx context.Context, now time.Time) (int, error) {
	maxVariables := x.db.Caps().MaxVariables
	dropped, done := 0, false
	err := x.db.Tx(ctx, func(tx *sql.Tx) error {
		dropped, done = 0, false
		ids, err := x.dropBatch(ctx, tx)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			for _, statement := range chatRebuiltDrops {
				if _, execErr := tx.ExecContext(ctx, statement); execErr != nil {
					return execErr
				}
			}
			if _, execErr := tx.ExecContext(ctx, chatRebuiltStatement,
				now.UTC().UnixMilli()); execErr != nil {
				return execErr
			}
			done = true
			return nil
		}
		delta := newChatDelta()
		for _, id := range ids {
			if removeErr := x.removeDoc(ctx, tx, id, delta); removeErr != nil {
				return removeErr
			}
		}
		dropped = len(ids)
		return delta.apply(ctx, tx, maxVariables)
	})
	if err != nil {
		return 0, fmt.Errorf("search: drop a batch of the chat index to rebuild it: %w", err)
	}
	if done {
		log.InfoContext(ctx, "chat_index_dropped",
			"detail", "the chat index is empty and its walk starts over from "+
				"the transcript's first position")
		return 1, nil
	}
	return dropped, nil
}

// dropBatch reads the next batch of documents a rebuild drops.
func (x *ChatIndexer) dropBatch(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, chatDropBatchStatement, ChatIndexBatch)
	if err != nil {
		return nil, fmt.Errorf("search: read the next chat documents to drop: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("search: scan a chat document to drop: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search: read the next chat documents to drop: %w", err)
	}
	return ids, nil
}

// chatDelta is what one transaction's writes change in the per-room
// statistics, applied once at its end.
//
// ACCUMULATED rather than written per document, because a batch touches the
// same few rooms and the same common terms over and over: a write per document
// would update one room's row two hundred and fifty times in one transaction.
type chatDelta struct {
	rooms map[string]*chatRoomDelta
	terms map[chatTermKey]int
}

// chatRoomDelta is one room's change in documents and tokens.
type chatRoomDelta struct{ docs, tokens int }

// chatTermKey is one term in one room.
type chatTermKey struct{ term, room string }

func newChatDelta() *chatDelta {
	return &chatDelta{rooms: map[string]*chatRoomDelta{}, terms: map[chatTermKey]int{}}
}

func (d *chatDelta) room(id string) *chatRoomDelta {
	r := d.rooms[id]
	if r == nil {
		r = &chatRoomDelta{}
		d.rooms[id] = r
	}
	return r
}

// add counts one document into a room, carrying each of terms once.
func (d *chatDelta) add(room string, tokens int, terms []string) {
	r := d.room(room)
	r.docs++
	r.tokens += tokens
	for _, term := range terms {
		d.terms[chatTermKey{term, room}]++
	}
}

// drop counts documents out of a room, carrying each of terms once.
func (d *chatDelta) drop(room string, docs, tokens int, terms []string) {
	r := d.room(room)
	r.docs -= docs
	r.tokens -= tokens
	for _, term := range terms {
		d.terms[chatTermKey{term, room}]--
	}
}

// dropCounted counts many documents out of a room, their terms already counted
// per term.
func (d *chatDelta) dropCounted(room string, docs, tokens int, terms map[string]int) {
	r := d.room(room)
	r.docs -= docs
	r.tokens -= tokens
	for term, n := range terms {
		d.terms[chatTermKey{term, room}] -= n
	}
}

// apply writes the accumulated change, and deletes every count it took to
// nothing.
//
// AN UPSERT THAT ADDS, one statement per chunk: a room or a term seen for the
// first time is inserted at the delta, and one already counted moves by it. A
// count that reaches zero is DELETED rather than kept, because a term nobody
// says any more is otherwise a row per room per token ever typed — an id, a
// hash, a pasted log line — for the life of the deployment, and a room with no
// documents is one the prune follower's rotation would visit for nothing.
func (d *chatDelta) apply(ctx context.Context, tx *sql.Tx, maxVariables int) error {
	type roomRow struct {
		id           string
		docs, tokens int
	}
	var rooms []roomRow
	var shrunkRooms []string
	for id, r := range d.rooms {
		if r.docs == 0 && r.tokens == 0 {
			continue
		}
		rooms = append(rooms, roomRow{id, r.docs, r.tokens})
		if r.docs < 0 {
			shrunkRooms = append(shrunkRooms, id)
		}
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO chat_index_rooms (channel_id, docs, tokens) VALUES`, `(?, ?, ?)`,
		`ON CONFLICT (channel_id) DO UPDATE SET
		     docs   = chat_index_rooms.docs + excluded.docs,
		     tokens = chat_index_rooms.tokens + excluded.tokens`,
		len(rooms), func(i int) []any {
			return []any{rooms[i].id, rooms[i].docs, rooms[i].tokens}
		}); err != nil {
		return fmt.Errorf("search: adjust the statistics of %d chat rooms: %w", len(rooms), err)
	}
	for chunk := range slices.Chunk(shrunkRooms, max(1, maxVariables)) {
		if _, err := tx.ExecContext(ctx, chatEmptiedRoomsStatement(len(chunk)),
			anys(chunk)...); err != nil {
			return fmt.Errorf("search: forget the chat rooms left with no documents: %w", err)
		}
	}

	type termRow struct {
		key  chatTermKey
		docs int
	}
	var terms []termRow
	shrunkTerms := map[string][]string{}
	for key, n := range d.terms {
		if n == 0 {
			continue
		}
		terms = append(terms, termRow{key, n})
		if n < 0 {
			shrunkTerms[key.room] = append(shrunkTerms[key.room], key.term)
		}
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables,
		`INSERT INTO chat_index_terms (term, channel_id, docs) VALUES`, `(?, ?, ?)`,
		`ON CONFLICT (term, channel_id) DO UPDATE SET
		     docs = chat_index_terms.docs + excluded.docs`,
		len(terms), func(i int) []any {
			return []any{terms[i].key.term, terms[i].key.room, terms[i].docs}
		}); err != nil {
		return fmt.Errorf("search: adjust %d chat term counts: %w", len(terms), err)
	}
	for room, gone := range shrunkTerms {
		for chunk := range slices.Chunk(gone, max(1, maxVariables-1)) {
			if _, err := tx.ExecContext(ctx, chatEmptiedTermsStatement(len(chunk)),
				append([]any{room}, anys(chunk)...)...); err != nil {
				return fmt.Errorf("search: forget the terms chat room %s no longer "+
					"carries: %w", room, err)
			}
		}
	}
	return nil
}

// chatEmptiedRoomsStatement deletes the rooms of a list whose documents are all
// gone.
func chatEmptiedRoomsStatement(n int) string {
	return `DELETE FROM chat_index_rooms WHERE channel_id IN (` + binds(n) + `) AND docs <= 0`
}

// chatEmptiedTermsStatement deletes the terms of a list one room's documents
// no longer carry.
func chatEmptiedTermsStatement(n int) string {
	return `DELETE FROM chat_index_terms WHERE channel_id = ? AND term IN (` +
		binds(n) + `) AND docs <= 0`
}

// anys is a list of strings as statement arguments.
func anys(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Run sweeps until the context ends.
//
// THE CADENCE IS THE SAME [indexIdle] THE KNOWLEDGE INDEXER USES, and a sweep
// that did nothing is what earns the sleep: a cold start and a rebuild's drop
// run flat out, and a steady state costs one read of the estate's instance,
// three of the one state row — the whole row and the two watermarks — and one
// step of the prune rotation per idle tick. Two loops rather than one over both
// corpora because the walks are not alike — this one is forward over
// `chat_messages.version` with a durable watermark, and the other is a lap by
// id that repairs a drifted row — so a shared loop would have to pick one of
// the two shapes for both.
//
// A FAILED SWEEP IS LOGGED AND RETRIED rather than ending the loop. The index
// is this node's own and is rebuilt from rows that are still there, so the
// honest consequence of a bad step is that `search_messages` answers over
// slightly older messages until the next one — never that a node stops
// indexing for the life of the process.
func (x *ChatIndexer) Run(ctx context.Context) {
	for {
		worked, err := x.Sweep(ctx, time.Now())
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			log.WarnContext(ctx, "chat_index_step_failed",
				"error", err.Error(),
				"detail", "the chat index is behind this node's own rows; "+
					toolname.SearchMessages+" answers over the messages it has "+
					"indexed so far and catches up on the next sweep")
		case worked > 0:
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(indexIdle):
		}
	}
}
