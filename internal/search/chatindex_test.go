package search_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
)

// The chat index's own suite. What it defends is the WALK — a forward-only
// index is cheap precisely because it never re-reads, and most cases here are
// about something a forward walk could miss — and the STATISTICS, which are
// maintained rather than counted and are the searched rooms' rather than the
// company's.

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
	assertIndexed(ctx, t, indexer, 0)
}

// THE PRUNE IS FOLLOWED BY THE TRANSCRIPT'S OWN FLOOR, which is what a forward
// walk cannot see for itself: a row that vanished never moves past the
// watermark.
func TestThePruneIsFollowedByTheTranscriptsFloor(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())

	writeMessages(t, db, 0, 10, "c1", "quarterly planning")
	sweepAll(ctx, t, indexer)
	if hits := searchChat(ctx, t, indexer, "planning", "c1"); len(hits) == 0 {
		t.Fatal("nothing was indexed to prune")
	}

	// The retention prune's own effect: a range delete before a cutoff.
	execReplicated(t, db, `DELETE FROM chat_messages WHERE channel_id = ? AND created_at < ?`,
		"c1", 1006)
	sweepAll(ctx, t, indexer)

	// THE INDEX'S OWN HOLDINGS, not only its answers: a search re-reads
	// every hit's row and skips one that is gone, so a pruned message the
	// index kept would never show in a result while its terms took slots
	// and weight in every ranking.
	assertIndexed(ctx, t, indexer, 4)
	for _, hit := range searchChat(ctx, t, indexer, "planning", "c1") {
		if hit.CreatedAt.Before(store.DecodeTime(1006)) {
			t.Fatalf("a pruned message posted at %v still answers searches", hit.CreatedAt)
		}
	}

	// AND A ROOM THE PRUNE EMPTIED is emptied in the index too: there is
	// no oldest message left to compare against, which is the case a
	// strictly-before delete at that oldest would miss.
	execReplicated(t, db, `DELETE FROM chat_messages WHERE channel_id = ?`, "c1")
	sweepAll(ctx, t, indexer)
	assertIndexed(ctx, t, indexer, 0)
	assertStatisticsCounted(ctx, t, db)
}

// THE PRUNE FOLLOWER REACHES EVERY ROOM THE INDEX HOLDS, however many there are.
//
// It follows a ROTATION of [search.ChatPruneFollowChannels] rooms a sweep, so a
// sweep's cost is bounded by that rather than by the company. A follower that
// read the first sixty-four rooms on every sweep — which is what an unordered
// `GROUP BY … LIMIT` over the documents does — never reached the
// sixty-fifth, and that room's pruned messages stayed in the index for good.
func TestThePruneFollowerReachesEveryRoomTheIndexHolds(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())

	rooms := search.ChatPruneFollowChannels + 6
	for i := range rooms {
		writeMessages(t, db, 10*i, 2, fmt.Sprintf("r%03d", i), "the standup")
	}
	sweepAll(ctx, t, indexer)
	assertIndexed(ctx, t, indexer, 2*rooms)

	// THE ROOM THAT SORTS LAST loses its oldest message.
	last := fmt.Sprintf("r%03d", rooms-1)
	execReplicated(t, db, `DELETE FROM chat_messages WHERE id = ?`, "m"+itoa(10*(rooms-1)))
	for range rooms/search.ChatPruneFollowChannels + 2 {
		if _, err := indexer.Sweep(ctx, time.Now()); err != nil {
			t.Fatalf("Sweep: %v", err)
		}
	}
	assertIndexed(ctx, t, indexer, 2*rooms-1)
	for _, hit := range searchChat(ctx, t, indexer, "standup", last) {
		if hit.MessageID == "m"+itoa(10*(rooms-1)) {
			t.Fatalf("the pruned message in %s still answers a search", last)
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
	// index kept would never show in a result — while its terms stayed on
	// this node's disk and in every backup of it, and took slots and weight
	// in every ranking.
	assertIndexed(ctx, t, indexer, 2)
	for _, hit := range searchChat(ctx, t, indexer, "credential", "c1") {
		if hit.MessageID == "m1" {
			t.Fatal("an erased message still answers a search; the compliance " +
				"gesture destroyed the row and the index kept its text")
		}
	}
	assertStatisticsCounted(ctx, t, db)
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
// in the index for good.
func TestAnEraseIsFollowedWholeAcrossTheBatchBoundary(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())

	const small = 10
	total := small + chatlet.MaxEraseMessages
	if total <= search.ChatIndexBatch {
		t.Fatalf("%d markers fit one batch of %d, so this case would not cross "+
			"the boundary it exists for", total, search.ChatIndexBatch)
	}
	// ONE MORE THAN IS ERASED, and the one kept is the OLDEST: a room whose
	// oldest message vanished reads to the prune follower as a prune, and
	// it would drop the erased documents before the new oldest whatever
	// this walk did.
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

// A TRANSCRIPT BATCH IS READ WHOLE AT ITS LAST POSITION.
//
// Every message a record changes carries that record's position, so a batch
// cut at a row count inside one position would advance the watermark past the
// rest of it and those messages would never be indexed. No record changes two
// messages today, so this writes the shape the walk must survive directly: a
// whole batch and then some at ONE position.
func TestATranscriptBatchIsReadWholeAtItsLastPosition(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())

	rows := make([]chatRow, 0, search.ChatIndexBatch+20)
	for i := range search.ChatIndexBatch + 20 {
		rows = append(rows, chatRow{id: fmt.Sprintf("m%04d", i), room: "c1",
			author: "founder", body: "one record moved us all",
			createdAt: int64(1000 + i), version: 777})
	}
	writeChatRows(t, db, rows...)
	sweepAll(ctx, t, indexer)
	assertIndexed(ctx, t, indexer, len(rows))
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

// A SEARCH SEES ONLY THE ASKER'S OWN ROOMS — and does see them.
func TestAChatSearchIsScopedToTheViewersChannels(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	writeMessages(t, db, 0, 2, "open", "the shipping schedule")
	writeMessages(t, db, 10, 2, "closed", "the shipping schedule")
	sweepAll(ctx, t, indexer)

	hits := searchChat(ctx, t, indexer, "shipping", "open")
	if len(hits) != 2 {
		t.Fatalf("a search of one room found %d of its two matching messages", len(hits))
	}
	for _, hit := range hits {
		if hit.ChannelID != "open" {
			t.Fatalf("a search scoped to one room answered with a message in %s",
				hit.ChannelID)
		}
	}
}

// A SEARCH NARROWED TO ONE SPEAKER ANSWERS ONLY THEIR MESSAGES.
//
// The narrowing is carried by every surface that offers it — a seat's
// `search_messages` and the API's chat search both take an author — so an
// index that ignored it answered "what did Ada say about the release" with
// everybody's messages, ranked as if they were hers.
func TestAChatSearchNarrowedToASpeakerAnswersOnlyTheirs(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	writeChatRows(t, db,
		chatRow{id: "m-ada", room: "c1", author: "ada", body: "the release slipped",
			createdAt: 1000, version: 100},
		chatRow{id: "m-bo", room: "c1", author: "bo", body: "the release slipped again",
			createdAt: 1001, version: 101},
	)
	sweepAll(ctx, t, indexer)

	hits, err := indexer.SearchMessages(ctx, search.ChatQuery{Text: "release",
		Channels: []string{"c1"}, Author: "ada"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 1 || hits[0].MessageID != "m-ada" || hits[0].Author != "ada" {
		t.Fatalf("a search narrowed to ada answered %+v; want her one message", hits)
	}
	if all := searchChat(ctx, t, indexer, "release", "c1"); len(all) != 2 {
		t.Fatalf("the same search for anybody answered %d of two messages", len(all))
	}
}

// A SCORE DOES NOT MOVE WITH WHAT IS SAID IN ROOMS THE ASKER CANNOT READ.
//
// The statistics a search is ranked by are the SEARCHED rooms', never the
// company's. Taken from the company, a private room saying a word more often
// lowered that word's weight in every visible hit, so the scores — and the
// order — of a search over rooms somebody may read moved with the contents of
// rooms they are not in.
func TestAScoreDoesNotMoveWithRoomsTheAskerCannotRead(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	writeMessages(t, db, 0, 1, "open", "the acquisition closes friday")
	writeMessages(t, db, 1, 5, "open", "lunch plans for the offsite")
	writeMessages(t, db, 10, 3, "secret", "nothing to see")
	sweepAll(ctx, t, indexer)
	before := searchChat(ctx, t, indexer, "acquisition", "open")
	if len(before) != 1 {
		t.Fatalf("the visible room's one matching message matched %d times", len(before))
	}

	// THE ROOM NOBODY ASKING CAN READ starts saying the word constantly.
	writeMessages(t, db, 100, 40, "secret", "acquisition acquisition rumours")
	sweepAll(ctx, t, indexer)
	after := searchChat(ctx, t, indexer, "acquisition", "open")
	if len(after) != 1 || after[0].Score != before[0].Score {
		t.Fatalf("the visible hit scored %v before a private room said the word "+
			"forty times and %+v after — its score encodes how often a word is "+
			"said where the asker cannot read", before[0].Score, after)
	}
}

// THE MAINTAINED STATISTICS ARE THE COUNTED ONES, through every write the
// index makes: a post, an edit that changes words and length, a tombstone, an
// erase, a prune and a rebuild. Each is a place a count can be moved by the
// wrong amount, and a count right on inserts and wrong on edits drifts on the
// first edit with nothing to say so.
func TestTheMaintainedStatisticsAreTheCountedOnes(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())

	writeMessages(t, db, 0, 6, "c1", "one two three")
	writeMessages(t, db, 10, 6, "c2", "two three four four")
	sweepAll(ctx, t, indexer)
	assertStatisticsCounted(ctx, t, db)

	execReplicated(t, db, `UPDATE chat_messages SET body = ?, version = ? WHERE id = ?`,
		"five six seven eight nine", 7000, "m0")
	execReplicated(t, db, `UPDATE chat_messages SET body = ?, version = ? WHERE id = ?`,
		"two", 7001, "m11")
	sweepAll(ctx, t, indexer)
	assertStatisticsCounted(ctx, t, db)

	// m0's words are its own since the edit, so its tombstone takes each of
	// them to nothing — the counts that must be DELETED rather than kept at
	// zero.
	execReplicated(t, db,
		`UPDATE chat_messages SET body = '', deleted_at = 1, version = ? WHERE id = ?`,
		7002, "m0")
	eraseMessages(t, db, 9000, "m2")
	execReplicated(t, db, `DELETE FROM chat_messages WHERE channel_id = ? AND created_at < ?`,
		"c2", 1013)
	sweepAll(ctx, t, indexer)
	assertStatisticsCounted(ctx, t, db)

	indexer.RequestRebuild()
	sweepAll(ctx, t, indexer)
	assertStatisticsCounted(ctx, t, db)
	assertIndexed(ctx, t, indexer, 7)
}

// A REBUILD DROPS WHAT A REPLACED ESTATE NO LONGER HOLDS, and Ready says when
// the walk has caught up again.
//
// An adoption puts a donor's file in place, and a forward walk never reads a
// row below its watermark again: a message the replaced copy had and the
// donor's does not, or a body the two copies disagree about at one position,
// stays as the index last saw it — searchable, and wrong — for as long as the
// deployment lives. Here the "donor" lacks two messages in the middle of a room
// (no prune and no erase, so neither follower can see it) and holds other words
// for a third at the same position.
func TestARebuildDropsWhatAReplacedEstateNoLongerHolds(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	if indexer.Ready() {
		t.Fatal("an index that has never swept says it is caught up")
	}
	writeMessages(t, db, 0, 5, "c1", "the original copy")
	sweepAll(ctx, t, indexer)
	if !indexer.Ready() {
		t.Fatal("an index whose walk found nothing past its watermark says it " +
			"is still behind")
	}

	execReplicated(t, db, `DELETE FROM chat_messages WHERE id IN ('m1', 'm2')`)
	execReplicated(t, db, `UPDATE chat_messages SET body = ? WHERE id = ?`,
		"what the donor says", "m3")
	sweepAll(ctx, t, indexer)
	if hits := searchChat(ctx, t, indexer, "donor", "c1"); len(hits) != 0 {
		t.Fatalf("the premise: a forward walk read a row below its watermark "+
			"(%d hits), so this case cannot show what a rebuild is for", len(hits))
	}

	indexer.RequestRebuild()
	if indexer.Ready() {
		t.Fatal("an index asked to rebuild still says it is caught up — a search " +
			"answered before the next sweep takes the request is answered over " +
			"the index of the estate that was replaced")
	}
	if _, err := indexer.Sweep(ctx, time.Now()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if indexer.Ready() {
		t.Fatal("an index that has just dropped everything says it is caught up")
	}
	sweepAll(ctx, t, indexer)
	if !indexer.Ready() {
		t.Fatal("a rebuilt index that caught up says it is still behind")
	}
	assertIndexed(ctx, t, indexer, 3)
	if hits := searchChat(ctx, t, indexer, "donor", "c1"); len(hits) != 1 {
		t.Fatalf("the donor's words for m3 matched %d messages after the rebuild", len(hits))
	}
	for _, hit := range searchChat(ctx, t, indexer, "original", "c1") {
		if hit.MessageID == "m3" {
			t.Fatal("m3 still matches the words the replaced copy held")
		}
	}
	assertStatisticsCounted(ctx, t, db)
}

// A REBUILD IS DROPPED A BATCH AT A TIME, AND A RESTART FINISHES IT.
//
// The drop is the size of the index — tens of millions of postings at a year
// of chat — and emptied in one transaction it held this node's only writer for
// all of them. So a sweep drops one batch, and the rebuild is a DURABLE mark: a
// process that stops halfway through starts again with the drop, rather than
// resuming its walk from the old watermark over an index it half emptied —
// which is what leaves a message the replaced copy had and the donor's does not
// searchable for good. Here a restart is a second indexer over the same store,
// which is all a process keeps across one.
func TestARebuildIsDroppedInStepsAndARestartFinishesIt(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	total := 2*search.ChatIndexBatch + 10
	writeMessages(t, db, 0, total, "c1", "the original copy")
	sweepAll(ctx, t, indexer)
	assertIndexed(ctx, t, indexer, total)

	// THE DONOR lacks one message in the middle of the room — no prune and
	// no erase, so only a rebuild can find it.
	gone := "m" + itoa(total/2)
	execReplicated(t, db, `DELETE FROM chat_messages WHERE id = ?`, gone)
	indexer.RequestRebuild()
	if _, err := indexer.Sweep(ctx, time.Now()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	held, err := indexer.Corpus(ctx)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	if held.Docs < total-search.ChatIndexBatch {
		t.Fatalf("one sweep after the request dropped %d of %d documents; a "+
			"sweep drops at most %d, or a rebuild holds this node's writer for "+
			"the whole index", total-held.Docs, total, search.ChatIndexBatch)
	}
	assertStatisticsCounted(ctx, t, db)

	restarted := search.NewChatIndexer(db, db.Replicated().Reader())
	sweepAll(ctx, t, restarted)
	if !restarted.Ready() {
		t.Fatal("a restarted index that finished the drop and caught up says it is behind")
	}
	assertIndexed(ctx, t, restarted, total-1)
	for _, hit := range searchChat(ctx, t, restarted, "original", "c1") {
		if hit.MessageID == gone {
			t.Fatalf("%s, which the replaced estate held and the donor's does "+
				"not, still answers a search: the restart resumed the walk over "+
				"a half-dropped index rather than finishing the drop", gone)
		}
	}
	assertStatisticsCounted(ctx, t, db)
}

// A STRETCH OF TOMBSTONES IS WALKED WITHOUT A PAUSE.
//
// A batch of tombstones for messages the index never held — a room cleaned of
// spam before this node indexed it, or any of it after a rebuild — changes no
// document and still moves the watermark. A sweep that reported it as no work
// read to the loop as a caught-up walk, so the loop slept an idle tick between
// every such batch, and anything counting on a zero meaning "caught up" stopped
// short of the message after them.
func TestAStretchOfTombstonesIsWalkedWithoutAPause(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	stretch := search.ChatIndexBatch + 10
	writeMessages(t, db, 0, stretch, "c1", "")
	execReplicated(t, db, `UPDATE chat_messages SET deleted_at = 1`)
	writeMessages(t, db, stretch, 1, "c1", "the one that survived")
	sweepAll(ctx, t, indexer)
	assertIndexed(ctx, t, indexer, 1)
	if !indexer.Ready() {
		t.Fatal("an index whose walk reached the transcript's end says it is behind")
	}
}

// A FREQUENT TERM CONTRIBUTES ITS NEWEST POSTINGS.
//
// A term in more messages than the scan budget contributes only that many,
// and which ones is the order of the scan: walking the list forward by message
// id — a UUIDv7, so forward is oldest first — a word the company says every
// day was searched only in its oldest messages, and a person looking for last
// week's discussion of it found the first month's.
func TestAFrequentTermContributesItsNewestPostings(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	const extra = 100
	total := search.MaxPostingScan + extra
	rows := make([]chatRow, 0, total)
	for i := range total {
		rows = append(rows, chatRow{id: fmt.Sprintf("m%05d", i), room: "c1",
			author: "founder", body: "deploy now", createdAt: int64(1000 + i),
			version: int64(100 + i)})
	}
	writeChatRows(t, db, rows...)
	sweepAll(ctx, t, indexer)

	hits := searchChat(ctx, t, indexer, "deploy", "c1")
	if len(hits) == 0 {
		t.Fatal("a term in every message matched none")
	}
	for _, hit := range hits {
		if hit.MessageID < fmt.Sprintf("m%05d", extra) {
			t.Fatalf("%s is among the %d oldest messages carrying the term — the "+
				"scan kept its oldest %d postings rather than its newest",
				hit.MessageID, extra, search.MaxPostingScan)
		}
	}
}

// A HIT CARRIES A SNIPPET OF THE MESSAGE, never the whole of a pasted block.
//
// A message may be tens of kilobytes, and twenty of them whole is most of a
// megabyte handed to a seat as one tool result. A message that fits is the
// whole message; one that does not is the window around the word that made it
// a hit.
func TestAHitCarriesASnippetOfTheMessage(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	long := strings.Repeat("filler words here ", 120) + "the needle sits here " +
		strings.Repeat("trailing words too ", 60)
	writeChatRows(t, db,
		chatRow{id: "m-long", room: "c1", author: "founder", body: long,
			createdAt: 1000, version: 100},
		chatRow{id: "m-short", room: "c1", author: "founder", body: "a short needle",
			createdAt: 1001, version: 101},
	)
	sweepAll(ctx, t, indexer)

	snippets := map[string]string{}
	for _, hit := range searchChat(ctx, t, indexer, "needle", "c1") {
		snippets[hit.MessageID] = hit.Snippet
	}
	if got := snippets["m-short"]; got != "a short needle" {
		t.Errorf("a message that fits came back as %q, want the whole of it", got)
	}
	got := snippets["m-long"]
	if len(got) > search.ChatSnippetBytes+2*len("…") || !strings.Contains(got, "needle") {
		t.Errorf("a %d-byte message came back as a %d-byte snippet %q; want at "+
			"most %d bytes around the word that matched", len(long), len(got), got,
			search.ChatSnippetBytes)
	}
}

// A DOCUMENT IS FILED IN THE BUCKET ITS MESSAGE IS, which is
// [search.ChatShardOf] on both sides: the applier stamps the replicated row with
// it, and an index that stamped another would have a fan-out read the message
// in one bucket and look for it in another.
func TestADocumentIsFiledInItsMessagesBucket(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	writeMessages(t, db, 0, 8, "c1", "bucket me")
	sweepAll(ctx, t, indexer)

	for i := range 8 {
		id := "m" + itoa(i)
		var stored int
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT search_shard FROM chat_docs WHERE id = ?`, id).Scan(&stored); err != nil {
			t.Fatalf("read the bucket of %s: %v", id, err)
		}
		if want := search.ChatShardOf(id); stored != want {
			t.Errorf("%s was indexed in bucket %d and its message is in %d", id, stored, want)
		}
	}
}

// THE CORPUS STATISTICS ARE MAINTAINED AND STAY TRUE THROUGH AN EDIT.
//
// BM25 reads the corpus size and the average length on every query, and this
// index maintains both rather than counting them — which is only safe while an
// edit adjusts them by the DIFFERENCE.
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

// A SEARCH ASKED FOR MORE HITS THAN EXIST ANSWERS THE ONES THAT DO.
//
// The limit is the caller's number — a seat's `limit` argument reaches here as
// it was typed — and the ranking sized its working list by it, so a limit past
// what a slice can hold panicked the node's search on three matching messages.
func TestASearchAskedForMoreHitsThanExistAnswersThem(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ctx := t.Context()
	indexer := search.NewChatIndexer(db, db.Replicated().Reader())
	writeMessages(t, db, 0, 3, "c1", "the quarterly numbers")
	sweepAll(ctx, t, indexer)

	hits, err := indexer.SearchMessages(ctx, search.ChatQuery{Text: "quarterly",
		Channels: []string{"c1"}, Limit: math.MaxInt})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("a search for every hit answered %d of the three", len(hits))
	}
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

// ---- fixtures ---------------------------------------------------------- //

// chatRow is one transcript row as the applier writes it, for the fields the
// index reads.
type chatRow struct {
	id, room, author, body string
	createdAt, version     int64
}

// writeChatRows puts rows into the replicated transcript in one transaction.
func writeChatRows(t *testing.T, db *store.DB, rows ...chatRow) {
	t.Helper()
	err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		for i, r := range rows {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO chat_messages (id, channel_id, thread_root, author_handle,
				                           author_kind, body, links, channel_seq,
				                           created_at, version, shard, document)
				VALUES (?, ?, '', ?, 'human', ?, '', ?, ?, ?, ?, X'')`,
				r.id, r.room, r.author, r.body, i+1, r.createdAt, r.version,
				search.ChatShardOf(r.id)); err != nil {
				return fmt.Errorf("insert message %s: %w", r.id, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// writeMessages puts n messages into one room of the replicated transcript, as
// the applier would. The version carries the walk, and created_at carries the
// prune.
func writeMessages(t *testing.T, db *store.DB, from, n int, channel, body string) {
	t.Helper()
	rows := make([]chatRow, 0, n)
	for i := range n {
		rows = append(rows, chatRow{id: "m" + itoa(from+i), room: channel,
			author: "founder", body: body, createdAt: int64(1000 + from + i),
			version: int64(100 + from + i)})
	}
	writeChatRows(t, db, rows...)
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
		t.Fatalf("the index holds %d documents, want %d — a removed message's "+
			"document survived, and with it its words", corpus.Docs, want)
	}
}

// assertStatisticsCounted fails the case unless every room's maintained
// statistics and every term count equal what the documents and postings the
// index holds count to — and no count was kept at zero.
func assertStatisticsCounted(ctx context.Context, t *testing.T, db *store.DB) {
	t.Helper()
	counted := func(query string) map[string][2]int64 {
		rows, err := db.SQL().QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		defer func() { _ = rows.Close() }()
		out := map[string][2]int64{}
		for rows.Next() {
			var key string
			var a, b int64
			if err := rows.Scan(&key, &a, &b); err != nil {
				t.Fatalf("%s: %v", query, err)
			}
			out[key] = [2]int64{a, b}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return out
	}
	held := counted(`SELECT channel_id, docs, tokens FROM chat_index_rooms`)
	want := counted(`SELECT channel_id, COUNT(*), SUM(length) FROM chat_docs GROUP BY channel_id`)
	if !maps.Equal(held, want) {
		t.Errorf("the maintained room statistics are %v and the documents count "+
			"to %v", held, want)
	}
	held = counted(`SELECT term || '@' || channel_id, docs, 0 FROM chat_index_terms`)
	want = counted(`
		SELECT p.term || '@' || d.channel_id, COUNT(*), 0
		  FROM chat_postings p JOIN chat_docs d ON d.id = p.doc_id
		 GROUP BY p.term, d.channel_id`)
	if !maps.Equal(held, want) {
		t.Errorf("the maintained term counts are %v and the postings count to %v",
			held, want)
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

// sweepAll runs the indexer until a sweep finds nothing to do, and reports
// what it did.
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

func itoa(n int) string { return fmt.Sprint(n) }
