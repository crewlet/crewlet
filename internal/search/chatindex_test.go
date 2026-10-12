package search_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
)

// The chat index's own suite. What it defends is the WALK: a forward-only
// index is cheap precisely because it never re-reads, and every case here is
// about something a forward walk could miss.

// THE WALK IS FORWARD AND NEVER RE-READS HISTORY.
//
// The whole reason this index exists beside the knowledge base's rather than
// inside it. A lap over a year of chat is thousands of scans; a forward walk
// over the position of the last record that changed a row reads each message
// once and then only what changed.
func TestTheChatIndexNeverReWalksHistory(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())

	writeMessages(t, db, 0, 40, "c1", "deploy pipeline notes")
	first := sweepAll(ctx, t, indexer)
	if first != 40 {
		t.Fatalf("a first sweep indexed %d of 40 messages", first)
	}
	// NOTHING CHANGED, so a forward walk has nothing to do. An id lap
	// would re-read all forty here, which is the cost this shape exists to
	// remove.
	again, err := indexer.Sweep(ctx, time.Now())
	if err != nil {
		t.Fatalf("Sweep on a caught-up index: %v", err)
	}
	if again != 0 {
		t.Fatalf("a caught-up index re-indexed %d documents; the walk is not "+
			"forward-only and the cost of this corpus is a lap rather than a "+
			"tail", again)
	}

	writeMessages(t, db, 40, 3, "c1", "one more thing")
	third := sweepAll(ctx, t, indexer)
	if third != 3 {
		t.Fatalf("three new messages indexed %d documents", third)
	}
}

// AN EDIT RE-INDEXES AND A TOMBSTONE DROPS THE DOCUMENT.
//
// Both move the row's version, which is the watermark — so the forward walk
// picks them up with no second mechanism. The tombstone case matters: the row
// survives in the transcript so replies still resolve, and its body is
// blanked, so a document left in the index would answer a search with text the
// company removed.
func TestAnEditReIndexesAndATombstoneDropsTheDocument(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())

	writeMessages(t, db, 0, 1, "c1", "the original wording")
	sweepAll(ctx, t, indexer)
	if hits := searchChat(ctx, t, indexer, "original", "c1"); len(hits) != 1 {
		t.Fatalf("the original wording matched %d messages", len(hits))
	}

	execReplicated(t, db, `UPDATE chat_messages SET body = ?, version = ? WHERE id = ?`,
		"a completely different sentence", 5000, "m0")
	sweepAll(ctx, t, indexer)
	if hits := searchChat(ctx, t, indexer, "original", "c1"); len(hits) != 0 {
		t.Fatalf("a term the edit removed still matches %d messages — a merge "+
			"that never deletes leaves every old word matching for ever", len(hits))
	}
	if hits := searchChat(ctx, t, indexer, "different", "c1"); len(hits) != 1 {
		t.Fatalf("the edited wording matched %d messages", len(hits))
	}

	execReplicated(t, db,
		`UPDATE chat_messages SET body = '', deleted_at = 1, version = ? WHERE id = ?`,
		6000, "m0")
	sweepAll(ctx, t, indexer)
	if hits := searchChat(ctx, t, indexer, "different", "c1"); len(hits) != 0 {
		t.Fatalf("a tombstoned message still answers a search with %d hits — "+
			"the body is blank in the transcript and the index is the only "+
			"place its text survives", len(hits))
	}
}

// THE PRUNE IS FOLLOWED BY THE SAME RANGE, which is what a forward walk cannot
// see for itself: a row that vanished never moves past the watermark.
func TestThePruneIsFollowedByTheSameRange(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())

	writeMessages(t, db, 0, 10, "c1", "quarterly planning")
	sweepAll(ctx, t, indexer)
	if hits := searchChat(ctx, t, indexer, "planning", "c1"); len(hits) == 0 {
		t.Fatal("nothing was indexed to prune")
	}

	// The retention prune's own effect: a range delete below a cutoff.
	execReplicated(t, db, `DELETE FROM chat_messages WHERE channel_id = ? AND created_at <= ?`,
		"c1", 1005)
	sweepAll(ctx, t, indexer)

	corpus, err := indexer.Corpus(ctx)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	if corpus.Docs != 4 {
		t.Fatalf("the index holds %d documents after six of ten were pruned — "+
			"a forward walk cannot see a row that vanished, so the prune has "+
			"to be followed explicitly", corpus.Docs)
	}
	for _, hit := range searchChat(ctx, t, indexer, "planning", "c1") {
		if hit.CreatedAt <= 1005 {
			t.Fatalf("a pruned message at %d still answers searches", hit.CreatedAt)
		}
	}
}

// AN ERASED MESSAGE LEAVES THE INDEX, and it is the one deletion a range
// cannot reach: an erase destroys ONE row wherever it sits, so the index
// follows its markers instead.
func TestAnErasedMessageLeavesTheIndex(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())

	writeMessages(t, db, 0, 3, "c1", "sensitive credential material")
	sweepAll(ctx, t, indexer)

	eraseMessages(t, db, 9000, "m1")
	sweepAll(ctx, t, indexer)

	// THE INDEX'S OWN HOLDINGS, not only its answers: a search re-reads
	// every hit's row and skips one that is gone, so an erased message the
	// index kept would never show in a result — while its text stayed on
	// this node's disk and in every backup of it, and its terms took slots
	// and weight in every ranking.
	assertIndexed(ctx, t, indexer, 2)
	for _, hit := range searchChat(ctx, t, indexer, "credential", "c1") {
		if hit.MessageID == "m1" {
			t.Fatal("an erased message still answers a search; the compliance " +
				"gesture destroyed the row and the index kept its text")
		}
	}
}

// THE ERASE WALK IS FORWARD TOO, and follows each marker once.
//
// The markers are never pruned — a marker is what makes a destruction
// permanent — so a follower that read all of them on every sweep did work that
// grew with the deployment's age, and took this node's one writer every two
// seconds to do it once anything had ever been erased. Followed from a
// watermark, a caught-up index reads one empty range and reports no work.
func TestTheEraseWalkFollowsEachMarkerOnce(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())

	writeMessages(t, db, 0, 3, "c1", "sensitive credential material")
	sweepAll(ctx, t, indexer)
	eraseMessages(t, db, 9000, "m1")
	if followed := sweepAll(ctx, t, indexer); followed != 1 {
		t.Fatalf("following one erase of one message did %d units of work, want 1", followed)
	}
	again, err := indexer.Sweep(ctx, time.Now())
	if err != nil {
		t.Fatalf("Sweep on a caught-up index: %v", err)
	}
	if again != 0 {
		t.Fatalf("a caught-up index followed %d erase markers again — the walk "+
			"re-reads markers it has already followed, which is a cost that "+
			"grows for as long as the company keeps its history", again)
	}
}

// AN ERASE IS FOLLOWED WHOLE ACROSS A BATCH BOUNDARY.
//
// Every marker one erase writes carries that record's position, and the
// largest erase a gesture may make fills a batch on its own. Here a small erase
// comes first, so the first ChatIndexBatch markers reach INTO the large one and
// stop inside it: a walk that advanced its watermark to the last position it
// read would step past the rest of that record, and those messages would stay
// searchable for good.
func TestAnEraseIsFollowedWholeAcrossTheBatchBoundary(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())

	const small = 10
	total := small + chat.MaxEraseMessages
	if total <= search.ChatIndexBatch {
		t.Fatalf("%d markers fit one batch of %d, so this case would not cross "+
			"the boundary it exists for", total, search.ChatIndexBatch)
	}
	// ONE MORE THAN IS ERASED, and the one kept is the OLDEST: a channel
	// whose oldest message vanished reads to the prune follower as a prune,
	// and it would drop the erased documents below it whatever this walk did.
	writeMessages(t, db, 0, total+1, "c1", "the leaked spreadsheet")
	sweepAll(ctx, t, indexer)

	ids := make([]string, total)
	for i := range ids {
		ids[i] = "m" + itoa(i+1)
	}
	eraseMessages(t, db, 9000, ids[:small]...)
	eraseMessages(t, db, 9001, ids[small:]...)
	sweepAll(ctx, t, indexer)

	// A batch cut inside one erase advances past the rest of it, and the
	// documents left behind are exactly what this counts.
	assertIndexed(ctx, t, indexer, 1)
}

// A CHAT SEARCH WITH NO VIEWER IS REFUSED, NOT ANSWERED EMPTY.
//
// The empty slice is what a caller that lost its viewer produces, and the two
// readings differ by the whole transcript: read as "everything" it serves
// every private channel and every direct message to whoever asked.
func TestAChatSearchWithoutAViewerIsRefused(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	writeMessages(t, db, 0, 2, "private", "the acquisition terms")
	sweepAll(ctx, t, indexer)

	_, err := indexer.SearchMessages(ctx, search.ChatQuery{Text: "acquisition"})
	if !errors.Is(err, search.ErrNoViewer) {
		t.Fatalf("a search naming no visible channels answered %v; it must be "+
			"refused, because a caller that forgot its viewer is exactly the "+
			"one an empty answer would hide", err)
	}
}

// A SEARCH SEES ONLY THE ASKER'S OWN ROOMS.
func TestAChatSearchIsScopedToTheViewersChannels(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	writeMessages(t, db, 0, 2, "open", "the shipping schedule")
	writeMessages(t, db, 10, 2, "closed", "the shipping schedule")
	sweepAll(ctx, t, indexer)

	for _, hit := range searchChat(ctx, t, indexer, "shipping", "open") {
		if hit.ChannelID != "open" {
			t.Fatalf("a search scoped to one room answered with a message in %s",
				hit.ChannelID)
		}
	}
}

// THE CORPUS STATISTICS ARE MAINTAINED AND STAY TRUE THROUGH AN EDIT.
//
// BM25 reads the corpus size and the average length on every query, and this
// index maintains both rather than counting them — which is only safe while an
// edit adjusts them by the DIFFERENCE. A count that is correct on inserts and
// wrong on updates drifts on the first edit and nothing says so.
func TestTheCorpusStatisticsSurviveAnEdit(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())

	writeMessages(t, db, 0, 5, "c1", "one two three")
	sweepAll(ctx, t, indexer)
	before, err := indexer.Corpus(ctx)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	if before.Docs != 5 {
		t.Fatalf("the corpus counts %d of 5 documents", before.Docs)
	}

	execReplicated(t, db, `UPDATE chat_messages SET body = ?, version = ? WHERE id = ?`,
		"one two three four five six seven eight", 7000, "m0")
	sweepAll(ctx, t, indexer)
	after, err := indexer.Corpus(ctx)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	if after.Docs != 5 {
		t.Fatalf("an edit changed the document count to %d; it replaced a row "+
			"rather than adding one", after.Docs)
	}
	if !(after.AvgLength > before.AvgLength) {
		t.Fatalf("the average length did not move when a message grew from "+
			"three tokens to eight (%v then %v) — the statistics are counted "+
			"on insert and never adjusted", before.AvgLength, after.AvgLength)
	}
}

// ---- fixtures ---------------------------------------------------------- //

// writeMessages puts n messages into the replicated transcript, as the applier
// would. The version carries the walk, and created_at carries the prune.
func writeMessages(t *testing.T, db *store.DB, from, n int, channel, body string) {
	t.Helper()
	for i := range n {
		id := "m" + itoa(from+i)
		execReplicated(t, db, `
			INSERT INTO chat_messages (id, channel_id, thread_root, author_handle,
			                           author_kind, body, links, channel_seq,
			                           created_at, version, shard, document)
			VALUES (?, ?, '', 'founder', 'human', ?, '', ?, ?, ?, 0, X'')`,
			id, channel, body, from+i+1, 1000+from+i, 100+from+i)
	}
}

// assertIndexed fails the case unless the index holds exactly want documents
// — its own holdings, which a search cannot show: every hit is re-read from
// the transcript and one whose row is gone is skipped.
func assertIndexed(ctx context.Context, t *testing.T, indexer *search.ChatIndexer, want int) {
	t.Helper()
	corpus, err := indexer.Corpus(ctx)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	if corpus.Docs != want {
		t.Fatalf("the index holds %d documents, want %d — an erased message's "+
			"document survived, and with it the text the erase destroyed",
			corpus.Docs, want)
	}
}

// eraseMessages is a compliance erase's effect on the transcript, as the
// applier writes it: the rows gone, and a marker for each carrying the erase's
// own position — one transaction, as one record's apply is.
func eraseMessages(t *testing.T, db *store.DB, position int64, ids ...string) {
	t.Helper()
	err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(t.Context(),
				`DELETE FROM chat_messages WHERE id = ?`, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO chat_deletions (message_id, channel_id, erased_at, op_id, by, version)
				VALUES (?, 'c1', ?, ?, 'founder', ?)`,
				id, position, "op-"+itoa(int(position)), position); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("erase %d messages: %v", len(ids), err)
	}
}

func execReplicated(t *testing.T, db *store.DB, query string, args ...any) {
	t.Helper()
	err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), query, args...)
		return err
	})
	if err != nil {
		t.Fatalf("write the transcript: %v", err)
	}
}

// sweepAll runs the indexer to exhaustion and reports what it wrote.
func sweepAll(ctx context.Context, t *testing.T, indexer *search.ChatIndexer) int {
	t.Helper()
	total := 0
	for range 100 {
		n, err := indexer.Sweep(ctx, time.Now())
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		if n == 0 {
			return total
		}
		total += n
	}
	t.Fatal("the index never caught up in a hundred sweeps")
	return total
}

func searchChat(ctx context.Context, t *testing.T, indexer *search.ChatIndexer,
	text string, channels ...string) []search.ChatHit {

	t.Helper()
	hits, err := indexer.SearchMessages(ctx, search.ChatQuery{Text: text, Channels: channels})
	if err != nil {
		t.Fatalf("SearchMessages(%q): %v", text, err)
	}
	return hits
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

// A CHAT QUERY PAST THE BOUND EVERY RANKED SEARCH SHARES IS REFUSED BEFORE IT
// RUNS, on the node that ranks it, whichever surface or peer asked — and before
// the viewer check, so a caller that forgot both learns the one it can fix by
// rewording. Each term is a postings read of its own here, so a pasted passage
// would otherwise be an unbounded number of them. At the bound it is a search.
func TestALongChatQueryIsRefusedBeforeItRuns(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	writeMessages(t, db, 0, 2, "c1", "the acquisition terms")
	sweepAll(ctx, t, indexer)

	long := strings.Repeat("acquisition ", knowledge.MaxQueryBytes/len("acquisition ")+1)
	for name, channels := range map[string][]string{
		"for a viewer": {"c1"}, "naming no viewer": nil,
	} {
		_, err := indexer.SearchMessages(ctx, search.ChatQuery{Text: long, Channels: channels})
		if !errors.Is(err, knowledge.ErrQueryTooLong) {
			t.Errorf("a %d-byte query %s answered %v, want the query-too-long class",
				len(long), name, err)
		}
	}
	at := strings.Repeat("a", knowledge.MaxQueryBytes-len(" acquisition")) + " acquisition"
	if hits := searchChat(ctx, t, indexer, at, "c1"); len(hits) == 0 {
		t.Error("a query at the bound found nothing it contains")
	}
}
