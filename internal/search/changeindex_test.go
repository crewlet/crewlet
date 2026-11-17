package search_test

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textindex"
)

// THE INDEX HOLDS EVERY CHANGE'S WORDS, whatever moved it: through a seeded run of opens, edits, moves
// and a purge, swept at random from its 150th position while a first build in small batches walks back
// over those, every row a walk has reached holds its scope and exactly its terms after each sweep, and so
// does every live row at the end.
// Mutation: drop the state update on an equal hash, drop the purge follower, clear only the slots the
// new terms overwrite, cut the floor one row below the batch, or let the watermark pass rows it did not write.
func TestTheChangeIndexHoldsEveryChangesWords(t *testing.T) {
	t.Parallel()
	for seed := range uint64(4) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			f := newChangeFixture(t)
			search.SetChangeBatchCap(f.x, 96)
			rng := rand.New(rand.NewPCG(seed, 24))
			words := []string{"login", "deploy", "cache", "flaky", "retry", "schema", "bump", "fix", "docs",
				"merge", "queue", "token"}
			text := func(n int) string {
				var b strings.Builder
				for range n {
					b.WriteString(words[rng.IntN(len(words))] + " ")
				}
				return b.String()
			}
			repos := []string{"repo-a", "repo-b", "repo-c"}
			for _, r := range repos {
				f.addRepo(r)
			}
			rows := map[string]changeFixtureRow{}
			position := int64(0)
			opened, midBuild := 0, 0
			for range 300 {
				position++
				switch op := rng.IntN(10); {
				case op < 3 || len(rows) == 0:
					// Several changes at one position, as a push record opens them.
					for range 1 + rng.IntN(3) {
						opened++
						row := changeFixtureRow{id: changeUUID(opened), repo: repos[rng.IntN(len(repos))],
							owner: []string{"ada", "bo"}[rng.IntN(2)], state: "open", title: text(4),
							body: text(rng.IntN(12))}
						if _, gone := f.purged[row.repo]; gone {
							continue
						}
						rows[row.id] = row
						f.write(row, position)
					}
				default:
					ids := slices.Sorted(maps.Keys(rows))
					row := rows[ids[rng.IntN(len(ids))]]
					switch op {
					case 3:
						row.title = text(4)
					case 4:
						// The same length, other words.
						row.body = strings.Repeat("zz ", len(row.body)/3) + strings.Repeat("q", len(row.body)%3)
					case 5:
						row.body = ""
					case 6, 7:
						// A head move: nothing the index reads.
					case 8:
						row.state = []string{"draft", "open", "closed", "merged"}[rng.IntN(4)]
					case 9:
						row.body = text(8)
					}
					rows[row.id] = row
					f.write(row, position)
				}
				f.settle(position)
				if position > 150 && rng.IntN(4) == 0 {
					f.sweepOnce()
					f.assertCovered()
					if _, floor := f.backFloor(); floor != "" {
						midBuild++
					}
				}
				if position == 200 {
					// THE PREMISE: the edits since 150 and this purge meet the backward walk.
					if back, floor := f.backFloor(); back < 0 || floor == "" || midBuild == 0 {
						t.Fatalf("at the purge the first build is at %d back to %q after %d sweeps mid-build: "+
							"the run never spans it", back, floor, midBuild)
					}
					gone := repos[rng.IntN(len(repos))]
					f.purge(gone)
					for id, row := range rows {
						if row.repo == gone {
							delete(rows, id)
						}
					}
				}
			}
			f.sweepAll()
			if held := f.holding(search.ChangeQuery{Terms: []string{"login"}, Limit: 1}); held.BackFloor != "" {
				t.Fatalf("swept to the end, the first build still reaches back only to %s", held.BackFloor)
			}
			f.assertHolds(rows)
		})
	}
}

// A MOVE THAT LEAVES THE TEXT WRITES NO POSTING: a head move and a state move after the first index
// leave every posting's slot and every document's key as they were and move the watermark.
// Mutation: re-index on every visit.
func TestAMoveThatLeavesTheTextWritesNoPosting(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	row := changeFixtureRow{id: changeUUID(1), repo: "repo-a", owner: "ada", state: "open",
		title: "Retry the flaky login", body: "the cache drops a token under load"}
	f.write(row, 1)
	// A later change's postings, so a re-index of the first would write rows past them.
	f.write(changeFixtureRow{id: changeUUID(2), repo: "repo-a", owner: "bo", state: "open", title: "later"}, 2)
	f.settle(2)
	f.sweepAll()
	before, keyed := f.postingRows(), f.docKeys()
	row.state = "draft"
	f.write(row, 3)
	f.write(row, 4)
	f.settle(4)
	f.sweepAll()
	if after := f.postingRows(); !maps.Equal(before, after) {
		t.Fatalf("a head move and a state move rewrote the postings: %v, then %v", before, after)
	}
	if after := f.docKeys(); !maps.Equal(keyed, after) {
		t.Fatalf("a head move and a state move moved the documents' keys: %v, then %v", keyed, after)
	}
	if held := f.holding(search.ChangeQuery{Terms: []string{"login"}, State: "draft", Limit: 10}); held.Through != 4 ||
		len(held.IDs) != 1 {
		t.Fatalf("after the moves the index answers %+v, want the change in its new state through 4", held)
	}
}

// A BATCH IS READ WHOLE AT ITS LAST POSITION: changes at one position past the row bound are indexed
// by the forward walk in one batch, and descriptions past the byte budget are cut into forward batches
// each ending on a position, and into backward batches each ending on a row whose id is the floor.
// Mutation: cut a forward batch at a row count, or cut a backward batch one row past the budget.
func TestAChangeBatchIsReadWholeAtItsLastPosition(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	f.write(changeFixtureRow{id: changeUUID(1), repo: "repo-a", owner: "ada", state: "open", title: "first"}, 1)
	f.settle(1)
	f.sweepAll()
	many := search.ChangeIndexBatchRows + 88
	for i := range many {
		f.write(changeFixtureRow{id: changeUUID(i + 2), repo: "repo-a", owner: "ada", state: "open",
			title: "bump the schema"}, 7)
	}
	f.settle(7)
	if n := f.sweepOnce(); n != many {
		t.Fatalf("one sweep walked %d of the %d changes one record moved at one position", n, many)
	}
	if held := f.holding(search.ChangeQuery{Terms: []string{"schema"}, Limit: 1_000}); len(held.IDs) != many ||
		held.Through != 7 {
		t.Fatalf("the index holds %d of %d changes through %d, want all through 7", len(held.IDs), many,
			held.Through)
	}

	// DESCRIPTIONS PAST THE BYTE BUDGET: two positions of three long changes each, three to a budget.
	long := strings.Repeat("word ", 800)
	search.SetChangeBatchCap(f.x, 3*len(long))
	for i := range 6 {
		f.write(changeFixtureRow{id: changeUUID(many + i + 2), repo: "repo-a", owner: "ada", state: "open",
			title: "a long one", body: long}, int64(8+i/3))
	}
	f.settle(9)
	if n := f.sweepOnce(); n != 3 {
		t.Fatalf("a sweep past the byte budget walked %d changes, want the three at its position", n)
	}
	if held := f.holding(search.ChangeQuery{Terms: []string{"long"}, Limit: 10}); held.Through != 8 {
		t.Fatalf("a batch cut by its bytes ended at %d, want the whole position 8", held.Through)
	}

	// AND BACKWARD, the first build's batches each end on the row whose bytes pass the budget.
	g := newChangeFixture(t)
	g.addRepo("repo-a")
	search.SetChangeBatchCap(g.x, 3*len(long))
	for i := range 7 {
		g.write(changeFixtureRow{id: changeUUID(i + 1), repo: "repo-a", owner: "ada", state: "open",
			title: "a long one", body: long}, int64(i+1))
	}
	g.settle(7)
	for _, want := range []struct {
		floor string
		held  int
	}{{changeUUID(5), 3}, {changeUUID(2), 6}, {"", 7}} {
		g.sweepOnce()
		if held := g.holding(search.ChangeQuery{Terms: []string{"long"}, Limit: 10}); held.BackFloor != want.floor ||
			len(held.IDs) != want.held {
			t.Fatalf("a backward batch reached back to %q holding %d, want %q holding %d", held.BackFloor,
				len(held.IDs), want.floor, want.held)
		}
	}
}

// THE WALK NEVER PASSES A RECORD THIS NODE RETAINS: a record retained below rows the checkpoint
// settled past holds the walk below it, and once the next build applies it at its own position, a
// change it opened and one it retitled are found by their words.
// Mutation: bound the walk by the settled checkpoint.
func TestAChangeIndexNeverWalksPastARetainedRecord(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	f.addRepo("repo-b")
	retitled := changeFixtureRow{id: changeUUID(1), repo: "repo-a", owner: "ada", state: "open",
		title: "the first words"}
	f.write(retitled, 1)
	f.settle(1)
	f.sweepAll()

	// Two records of repo-a retained at 5 and 6, and repo-b's changes applied above them.
	f.retain(5, "repo-a")
	f.retain(6, "repo-a")
	f.write(changeFixtureRow{id: changeUUID(2), repo: "repo-b", owner: "bo", state: "open",
		title: "applied above"}, 7)
	f.settle(7)
	f.sweepAll()
	if held := f.holding(search.ChangeQuery{Terms: []string{"applied"}, Limit: 10}); held.Through >= 5 ||
		len(held.IDs) != 0 {
		t.Fatalf("the walk reached %d past a record retained at 5 (%d ids)", held.Through, len(held.IDs))
	}

	// THE NEXT BUILD applies them at their own positions.
	opened := changeFixtureRow{id: changeUUID(3), repo: "repo-a", owner: "ada", state: "open",
		title: "opened by a newer build"}
	f.write(opened, 5)
	retitled.title = "the later words"
	f.write(retitled, 6)
	f.release()
	f.sweepAll()
	for term, want := range map[string]string{"newer": opened.id, "later": retitled.id,
		"applied": changeUUID(2)} {
		if held := f.holding(search.ChangeQuery{Terms: []string{term}, Limit: 10}); !slices.Equal(held.IDs,
			[]string{want}) {
			t.Errorf("%q finds %v, want %s: a change a retained record wrote is never indexed", term,
				held.IDs, want)
		}
	}
	if held := f.holding(search.ChangeQuery{Terms: []string{"first"}, Limit: 10}); len(held.IDs) != 0 {
		t.Errorf("the retitled change is still found by its old words: %v", held.IDs)
	}
}

// A PURGED REPOSITORY'S WORDS LEAVE THE INDEX, a batch a step, and a live repository's stay.
// Mutation: test the purging status instead of a missing row, or drop a document without its slots.
func TestAPurgedRepositorysWordsLeaveTheIndex(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	f.addRepo("repo-b")
	n := search.ChangeDropBatch + 5
	for i := range n {
		f.write(changeFixtureRow{id: changeUUID(i + 1), repo: "repo-a", owner: "ada", state: "open",
			title: "doomed words"}, int64(i+1))
	}
	f.write(changeFixtureRow{id: changeUUID(n + 1), repo: "repo-b", owner: "ada", state: "open",
		title: "doomed but kept"}, int64(n+1))
	f.settle(int64(n + 1))
	f.sweepAll()
	f.purge("repo-a")
	if first := f.sweepOnce(); first != search.ChangeDropBatch {
		t.Fatalf("the first sweep after the purge dropped %d documents, want one batch of %d", first,
			search.ChangeDropBatch)
	}
	f.sweepAll()
	if held := f.holding(search.ChangeQuery{Terms: []string{"doomed"}, Limit: 1_000}); !slices.Equal(held.IDs,
		[]string{changeUUID(n + 1)}) {
		t.Fatalf("after the purge the index holds %v, want only repo-b's change", held.IDs)
	}
	if orphans := f.count(orphanPostings); orphans != 0 {
		t.Fatalf("the purge left %d postings of no document", orphans)
	}
}

// THE PURGE FOLLOWER REACHES EVERY REPOSITORY THE INDEX HOLDS, past one step of its rotation.
// Mutation: restart the rotation at the first repository every sweep.
func TestThePurgeFollowerReachesEveryRepositoryTheIndexHolds(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	repos := search.ChangePurgeFollowRepos*2 + 3
	for i := range repos {
		repo := fmt.Sprintf("repo-%03d", i)
		f.addRepo(repo)
		f.write(changeFixtureRow{id: changeUUID(i + 1), repo: repo, owner: "ada", state: "open",
			title: "everywhere"}, int64(i+1))
	}
	f.settle(int64(repos))
	f.sweepAll()
	last := fmt.Sprintf("repo-%03d", repos-1)
	f.purge(last)
	for range 4 {
		f.sweepOnce()
	}
	if held := f.holding(search.ChangeQuery{Terms: []string{"everywhere"}, Limit: 1_000}); len(held.IDs) != repos-1 {
		t.Fatalf("after one rotation the index holds %d documents, want %d: the follower never reached %s",
			len(held.IDs), repos-1, last)
	}
}

// A REBUILD KEEPS THE INDEX AND WRITES IT AGAIN NEWEST FIRST: asked to rebuild over a copy that
// diverged (a change gone, one retitled, one moved to another owner alone, one new), the index
// answers Rebuilding and keeps every document until its first build begins again at the applied
// prefix, which a second indexer over the same store does; each stretch the build reaches holds exactly
// the copy's changes, the gone one's document leaves once the floor passes it, and so the end holds the
// copy alone.
// Mutation: drop pruneChangeDocs, or compare only the state on an equal hash.
func TestARebuildKeepsTheChangeIndexAndWritesItAgainNewestFirst(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	search.SetChangeBatchCap(f.x, 64)
	const total = 40
	rows := map[string]changeFixtureRow{}
	for i := range total {
		row := changeFixtureRow{id: changeUUID(i + 1), repo: "repo-a", owner: "ada", state: "open",
			title: "the original copy", body: "twenty bytes of text"}
		rows[row.id] = row
		f.write(row, int64(i+1))
	}
	f.settle(total)
	f.sweepAll()

	gone, retitled, moved := changeUUID(total/2), changeUUID(5), changeUUID(30)
	f.exec(`DELETE FROM code_change_bodies WHERE change_id = ?`, gone)
	f.exec(`DELETE FROM code_changes WHERE id = ?`, gone)
	delete(rows, gone)
	row := rows[retitled]
	row.title = "the donors words"
	rows[retitled] = row
	f.exec(`UPDATE code_changes SET title = ? WHERE id = ?`, row.title, retitled)
	row = rows[moved]
	row.owner = "bo"
	rows[moved] = row
	f.exec(`UPDATE code_changes SET owner = ? WHERE id = ?`, row.owner, moved)
	fresh := changeFixtureRow{id: changeUUID(total + 1), repo: "repo-a", owner: "ada", state: "open",
		title: "only the donor has it"}
	rows[fresh.id] = fresh
	f.write(fresh, total+1)
	f.settle(total + 1)

	f.x.RequestRebuild()
	if held := f.holding(search.ChangeQuery{Terms: []string{"original"}, Limit: 10}); !held.Rebuilding {
		t.Fatal("an index asked to rebuild answers as built")
	}
	f.sweepOnce()
	if held := f.holding(search.ChangeQuery{Terms: []string{"original"}, Limit: 10}); !held.Rebuilding {
		t.Fatalf("an index whose first build has not begun again answers %+v, want Rebuilding", held)
	}
	if docs := f.count(`SELECT COUNT(*) FROM change_docs`); docs != total {
		t.Fatalf("the rebuild's mark left %d of %d documents: it drops what the build would rewrite", docs, total)
	}
	f.x = search.NewChangeIndexer(f.db, f.db.Replicated().Reader(), gitlet.Domain{})
	search.SetChangeBatchCap(f.x, 64)
	f.sweepOnce()
	if held := f.holding(search.ChangeQuery{Terms: []string{"donor"}, Limit: 1}); held.Rebuilding ||
		held.BackPos != total+1 || held.BackFloor == "" || !slices.Equal(held.IDs, []string{fresh.id}) {
		t.Fatalf("the first build again answers %+v, want it begun at %d with the newest change and "+
			"older ones still to reach", held, total+1)
	}
	for range 100 {
		f.assertCovered()
		if _, floor := f.backFloor(); floor == "" || floor <= gone {
			if _, held := f.indexedDocs()[gone]; held {
				t.Fatalf("the build reached back to %q past the gone change, whose document is still held", floor)
			}
		}
		if f.sweepOnce() == 0 {
			break
		}
	}
	f.assertHolds(rows)
}

// A REPLACEMENT A RESTART HID FROM THE INDEX STILL REBUILDS IT: the estate's own name for its copy,
// compared on every sweep, starts it over where no request survived.
// Mutation: drop the instance comparison from Sweep.
func TestAReplacementARestartHidFromTheChangeIndexStillRebuildsIt(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	row := changeFixtureRow{id: changeUUID(1), repo: "repo-a", owner: "ada", state: "open", title: "original"}
	f.write(row, 1)
	f.settle(1)
	f.sweepAll()
	if held := f.holding(search.ChangeQuery{Terms: []string{"original"}, Limit: 10}); held.BuiltFrom !=
		f.instance() {
		t.Fatalf("a fresh index records it was built from %q, want %q", held.BuiltFrom, f.instance())
	}
	f.exec(`UPDATE code_changes SET title = 'the donors words' WHERE id = ?`, row.id)
	f.exec(`UPDATE statelog_estate SET instance = ?`, "the-donors-copy")
	f.x = search.NewChangeIndexer(f.db, f.db.Replicated().Reader(), gitlet.Domain{})
	f.sweepAll()
	held := f.holding(search.ChangeQuery{Terms: []string{"donors"}, Limit: 10})
	if !slices.Equal(held.IDs, []string{row.id}) || held.BuiltFrom != "the-donors-copy" {
		t.Fatalf("the restarted index answers %+v: it never saw the estate was replaced", held)
	}
}

// A FRESH INDEX ON AN ADOPTED ESTATE WALKS IT AND REBUILDS NOTHING: an index holding nothing records
// the copy and walks it.
// Mutation: rebuild an index that holds nothing.
func TestAFreshChangeIndexOnAnAdoptedEstateWalksItAndRebuildsNothing(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.exec(`UPDATE statelog_estate SET instance = ?`, "an-adopted-copy")
	f.addRepo("repo-a")
	for i := range 7 {
		f.write(changeFixtureRow{id: changeUUID(i + 1), repo: "repo-a", owner: "ada", state: "open",
			title: "already here"}, int64(i+1))
	}
	f.settle(7)
	if worked := f.sweepAll(); worked != 7 {
		t.Fatalf("a fresh index walked %d units over 7 changes: the extra is a rebuild of nothing", worked)
	}
	if held := f.holding(search.ChangeQuery{Terms: []string{"here"}, Limit: 10}); held.BuiltFrom !=
		"an-adopted-copy" || len(held.IDs) != 7 {
		t.Fatalf("the index answers %+v, want the 7 changes of the copy it walked", held)
	}
}

// EVERY INDEX WRITE RECORDS WHAT IT WAS DECIDED FROM: a forward batch, a backward batch and a purge
// drop each raise Seen to the settled checkpoint of the snapshot they read and never lower it; the
// first backward batch begins the build at the applied prefix of its snapshot, with the watermark at
// least there; a rebuild's mark sets Seen to 0 and the build to not begun; and an idle sweep or a walk
// held at a retained record writes nothing.
// Mutation: drop the update from the purge drop or the backward batch, or write it without MAX.
func TestEveryChangeIndexWriteRecordsWhatItWasDecidedFrom(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	f.addRepo("repo-b")
	f.write(changeFixtureRow{id: changeUUID(1), repo: "repo-a", owner: "ada", state: "open", title: "one"}, 1)
	f.write(changeFixtureRow{id: changeUUID(2), repo: "repo-b", owner: "ada", state: "open", title: "two"}, 2)
	f.settle(4)
	f.sweepAll()
	seen := func() int { return f.count(`SELECT seen_pos FROM change_index_state`) }
	if got := seen(); got != 4 {
		t.Fatalf("a walk batch over a snapshot settled at 4 recorded Seen %d", got)
	}
	if held := f.holding(search.ChangeQuery{Terms: []string{"one"}, Limit: 1}); held.BackPos != 4 ||
		held.Through != 4 {
		t.Fatalf("the first build over a prefix applied to 4 began at %d with the watermark at %d",
			held.BackPos, held.Through)
	}
	f.settle(9)
	if n := f.sweepOnce(); n != 0 || seen() != 4 {
		t.Fatalf("an idle sweep reported %d units and moved Seen to %d", n, seen())
	}
	// A WALK HELD AT A RETAINED RECORD writes nothing, and a purge drop then records its own snapshot.
	f.retain(10, "repo-a")
	f.write(changeFixtureRow{id: changeUUID(3), repo: "repo-b", owner: "ada", state: "open", title: "held"}, 11)
	f.settle(12)
	f.sweepAll()
	if got := seen(); got != 4 {
		t.Fatalf("a walk held at a retained record moved Seen to %d", got)
	}
	f.purge("repo-a")
	f.settle(14)
	if n := f.sweepOnce(); n != 1 {
		t.Fatalf("the purge follower dropped %d documents of the purged repository, want 1", n)
	}
	if got := seen(); got != 14 {
		t.Fatalf("a purge drop over a snapshot settled at 14 recorded Seen %d", got)
	}
	f.release()
	f.sweepAll()
	// NEVER LOWERED: a batch over an older settled position than one recorded leaves it.
	if err := f.db.Tx(f.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, `UPDATE change_index_state SET seen_pos = 100`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.write(changeFixtureRow{id: changeUUID(4), repo: "repo-b", owner: "ada", state: "open", title: "late"}, 15)
	f.settle(15)
	if n := f.sweepAll(); n != 1 || seen() != 100 {
		t.Fatalf("a batch of %d over a snapshot settled at 15 moved Seen from 100 to %d", n, seen())
	}
	f.x.RequestRebuild()
	f.sweepOnce()
	if got, back, floor := seen(), f.count(`SELECT back_pos FROM change_index_state`),
		f.count(`SELECT length(back_floor) FROM change_index_state`); got != 0 || back != -1 || floor != 0 {
		t.Fatalf("a rebuild's mark left Seen at %d, back_pos at %d and a floor of %d bytes", got, back, floor)
	}

	// A BACKWARD BATCH records its own snapshot's settled position, and never lowers one recorded.
	g := newChangeFixture(t)
	g.addRepo("repo-a")
	search.SetChangeBatchCap(g.x, 1)
	for i := range 4 {
		g.write(changeFixtureRow{id: changeUUID(i + 1), repo: "repo-a", owner: "ada", state: "open",
			title: "back", body: "one row a batch"}, int64(i+1))
	}
	g.settle(4)
	g.sweepOnce()
	g.settle(8)
	g.sweepOnce()
	if got := g.count(`SELECT seen_pos FROM change_index_state`); got != 8 {
		t.Fatalf("a backward batch over a snapshot settled at 8 recorded Seen %d", got)
	}
	if err := g.db.Tx(g.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(g.ctx, `UPDATE change_index_state SET seen_pos = 100`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	g.sweepOnce()
	if got := g.count(`SELECT seen_pos FROM change_index_state`); got != 100 {
		t.Fatalf("a backward batch over a snapshot settled at 8 moved Seen from 100 to %d", got)
	}
}

// HOLDING ANSWERS THE NEWEST CHANGES HOLDING EVERY WORD in its scope, driven by the narrowest of the
// scope and the words, newest minted first: the same corpus built by the first build's backward walk
// and by the forward walk as each change opens answers the same ids in the same order.
// Mutation: key documents by the order the walks take them, drive every search by its rarest word,
// drop an EXISTS, test a word over a neighbour's slot range, or order ascending.
func TestHoldingAnswersTheNewestChangesHoldingEveryWord(t *testing.T) {
	t.Parallel()
	// f takes the corpus in one first build; g as each change opens, a sweep after each.
	f, g := newChangeFixture(t), newChangeFixture(t)
	for _, r := range []string{"busy", "quiet"} {
		f.addRepo(r)
		g.addRepo(r)
	}
	g.settle(0)
	g.sweepAll()
	id := 0
	write := func(row changeFixtureRow, position int64) {
		f.write(row, position)
		g.write(row, position)
		g.settle(position)
		g.sweepAll()
	}
	add := func(repo, owner, state, title, body string) string {
		id++
		write(changeFixtureRow{id: changeUUID(id), repo: repo, owner: owner, state: state, title: title,
			body: body}, int64(id))
		return changeUUID(id)
	}
	// The quiet repository's changes first, then a word common across the company in the busy one's,
	// so a search driven on that word's newest postings never reaches them.
	q1 := add("quiet", "bo", "open", "common login", "")
	q2 := add("quiet", "bo", "merged", "common", "and a login in the description")
	q3 := add("quiet", "ada", "open", "login alone", "")
	for range 60 {
		add("busy", "ada", "merged", "common fix", "")
	}
	newest := add("busy", "ada", "merged", "common fix", "")
	// AN OLDER CHANGE TAKEN IN AFTER A NEWER ONE is answered after it: when it was minted, not taken in.
	newer := add("quiet", "cy", "draft", "reordered", "")
	id++
	older := changeUUID(0)
	write(changeFixtureRow{id: older, repo: "quiet", owner: "cy", state: "draft", title: "reordered"}, int64(id))
	add("quiet", "bo", "draft", "neighbour", "")
	f.settle(int64(id))
	f.sweepAll()
	search.SetChangeTextScan(f.x, 20)
	search.SetChangeTextScan(g.x, 20)

	for name, tc := range map[string]struct {
		q         search.ChangeQuery
		want      []string
		truncated bool
	}{
		"two words, split between the title and the description": {
			q: search.ChangeQuery{Terms: []string{"common", "login"}, Limit: 10}, want: []string{q2, q1}},
		"a repository and a common word": {
			q: search.ChangeQuery{Terms: []string{"common"}, RepoID: "quiet", Limit: 10}, want: []string{q2, q1}},
		"an owner and a common word": {
			q: search.ChangeQuery{Terms: []string{"common"}, Owner: "bo", Limit: 10}, want: []string{q2, q1}},
		"a state": {
			q: search.ChangeQuery{Terms: []string{"login"}, State: "open", Limit: 10}, want: []string{q3, q1}},
		"every scope together": {
			q:    search.ChangeQuery{Terms: []string{"login"}, RepoID: "quiet", State: "open", Owner: "bo", Limit: 10},
			want: []string{q1}},
		"a word in no change": {q: search.ChangeQuery{Terms: []string{"absent"}, Limit: 10}},
		"newest minted first": {q: search.ChangeQuery{Terms: []string{"reordered"}, Limit: 10},
			want: []string{newer, older}},
		"newest minted first, read by owner": {q: search.ChangeQuery{Terms: []string{"reordered"},
			Owner: "cy", Limit: 10}, want: []string{newer, older}},
		"a word and the word of the document before": {q: search.ChangeQuery{Terms: []string{"reordered",
			"common"}, Limit: 10}, want: []string{}},
		"a word and the word of the document after": {q: search.ChangeQuery{Terms: []string{"reordered",
			"neighbour"}, Limit: 10}, want: []string{}},
		"a word handed twice": {q: search.ChangeQuery{Terms: []string{"login", "login"}, Limit: 10},
			want: []string{q3, q2, q1}},
		"the limit": {q: search.ChangeQuery{Terms: []string{"login"}, Limit: 2}, want: []string{q3, q2},
			truncated: true},
		"the scan of a common word": {q: search.ChangeQuery{Terms: []string{"common"}, Limit: 100},
			truncated: true},
	} {
		t.Run(name, func(t *testing.T) {
			held := f.holding(tc.q)
			if tc.want != nil && !slices.Equal(held.IDs, tc.want) {
				t.Errorf("Holding(%+v) = %v, want %v", tc.q, held.IDs, tc.want)
			}
			if held.Truncated != tc.truncated {
				t.Errorf("Holding(%+v) truncated %v, want %v", tc.q, held.Truncated, tc.truncated)
			}
			if other := g.holding(tc.q); !slices.Equal(other.IDs, held.IDs) || other.Truncated != held.Truncated {
				t.Errorf("Holding(%+v) built as changes opened answers %v (truncated %v), built at once %v "+
					"(truncated %v)", tc.q, other.IDs, other.Truncated, held.IDs, held.Truncated)
			}
		})
	}
	scanned := f.holding(search.ChangeQuery{Terms: []string{"common"}, Limit: 100})
	if len(scanned.IDs) != 20 || scanned.IDs[0] != newest {
		t.Errorf("a search past the scan answers %d ids starting %s, want the newest 20 from %s",
			len(scanned.IDs), scanned.IDs[0], newest)
	}
	f.x.RequestRebuild()
	if held := f.holding(search.ChangeQuery{Terms: []string{"login"}, Limit: 10}); !held.Rebuilding ||
		held.IDs != nil {
		t.Errorf("a rebuilding index answers %+v, want its state alone", held)
	}
}

// A SCOPE DRIVES A WORD IT TIES PAST THE SCAN: a repository and a word both past it count alike, and
// the repository's newest matches are answered though the word's newest postings are all elsewhere.
// Mutation: let the word win the tie.
func TestAScopeDrivesAWordItTiesPastTheScan(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("mine")
	f.addRepo("theirs")
	var mine []string
	for i := range 60 {
		repo := "mine"
		if i >= 30 {
			repo = "theirs"
		}
		f.write(changeFixtureRow{id: changeUUID(i + 1), repo: repo, owner: "ada", state: "open",
			title: "flaky fix"}, int64(i+1))
		if repo == "mine" {
			mine = append(mine, changeUUID(i+1))
		}
	}
	f.settle(60)
	f.sweepAll()
	search.SetChangeTextScan(f.x, 20)
	held := f.holding(search.ChangeQuery{Terms: []string{"flaky"}, RepoID: "mine", Limit: 5})
	slices.Reverse(mine)
	if !slices.Equal(held.IDs, mine[:5]) || !held.Truncated {
		t.Fatalf("a repository and a word tied past the scan answered %v, truncated %v; want %v, truncated",
			held.IDs, held.Truncated, mine[:5])
	}
}

// EVERY TERM OF THE LARGEST CHANGE IS KEPT: a document's slots outnumber the runs a title and a
// description at Gitlet's bounds can hold, a change holding the most distinct terms they allow is
// indexed whole, and a document past the slots fails its batch, naming the change, and writes nothing.
// Mutation: shrink the slots to 1 << 13, or drop the terms past the slots instead of failing.
func TestEveryTermOfTheLargestChangeIsKept(t *testing.T) {
	t.Parallel()
	if runs := (gitlet.MaxChangeTitleBytes+1)/2 + (gitlet.MaxChangeBodyBytes+1)/2; runs > search.ChangeDocTermSlots {
		t.Fatalf("a title and a description at Gitlet's bounds hold up to %d runs, past the %d slots of a document",
			runs, search.ChangeDocTermSlots)
	}
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	title := distinctWords(0, gitlet.MaxChangeTitleBytes/4, 3)
	body := distinctWords(gitlet.MaxChangeTitleBytes/4, gitlet.MaxChangeBodyBytes/4, 3)
	if len(title) > gitlet.MaxChangeTitleBytes || len(body) > gitlet.MaxChangeBodyBytes {
		t.Fatalf("the largest change's text is %d and %d bytes, past Gitlet's bounds", len(title), len(body))
	}
	row := changeFixtureRow{id: changeUUID(1), repo: "repo-a", owner: "ada", state: "open", title: title, body: body}
	f.write(row, 1)
	f.settle(1)
	f.sweepAll()
	all := slices.Concat(strings.Fields(title), strings.Fields(body))
	for _, term := range []string{all[0], all[len(all)/2], all[len(all)-1]} {
		if held := f.holding(search.ChangeQuery{Terms: []string{term}, Limit: 10}); !slices.Equal(held.IDs,
			[]string{row.id}) {
			t.Errorf("%q of the largest change finds %v", term, held.IDs)
		}
	}
	f.assertHolds(map[string]changeFixtureRow{row.id: row})

	// PAST THE SLOTS, by raw rows no Gitlet write makes.
	g := newChangeFixture(t)
	g.addRepo("repo-a")
	past := changeFixtureRow{id: changeUUID(2), repo: "repo-a", owner: "ada", state: "open", title: "past",
		body: distinctWords(0, search.ChangeDocTermSlots, 4)}
	g.write(past, 1)
	g.settle(1)
	_, err := g.x.Sweep(g.ctx, time.Now())
	if err == nil || !strings.Contains(err.Error(), past.id) {
		t.Fatalf("a change of %d terms swept with %v, want its batch failed naming it",
			search.ChangeDocTermSlots+1, err)
	}
	if docs, postings := g.count(`SELECT COUNT(*) FROM change_docs`), g.count(`SELECT COUNT(*) FROM change_postings`); docs != 0 ||
		postings != 0 || g.count(`SELECT through_pos FROM change_index_state`) != 0 {
		t.Fatalf("the failed batch wrote %d documents and %d postings, or moved the watermark", docs, postings)
	}
}

// A DOCUMENT IS KEYED BY WHEN ITS CHANGE WAS MINTED, whichever walk took it and in whichever order; two
// ids sharing a natural key both get one, the second the next free key; ids past a natural key's window,
// with no instant, or at the last millisecond take the arena's keys; every one is found by its words.
// Mutation: take the natural key without reading the window, drop the arena, or let the last
// millisecond be natural (its slot range's end wraps negative).
func TestADocumentIsKeyedByWhenItsChangeWasMinted(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	at := changeEpoch.Add(time.Hour)
	// The newest is taken first by the first build; the two older ones by the forward walk, oldest last.
	minted := []string{mintedChangeID(at.Add(time.Second), 0, 1), mintedChangeID(at.Add(time.Millisecond), 0, 2),
		mintedChangeID(at, 0, 3)}
	for i, id := range minted {
		f.write(changeFixtureRow{id: id, repo: "repo-a", owner: "ada", state: "open", title: "minted"}, int64(i+1))
		f.settle(int64(i + 1))
		f.sweepAll()
	}
	keys := f.docKeys()
	if keys[minted[2]] >= keys[minted[1]] || keys[minted[1]] >= keys[minted[0]] {
		t.Fatalf("ids minted at t, t+1ms and t+1s took keys %d, %d and %d: not in that order", keys[minted[2]],
			keys[minted[1]], keys[minted[0]])
	}
	if held := f.holding(search.ChangeQuery{Terms: []string{"minted"}, Limit: 10}); !slices.Equal(held.IDs,
		minted) {
		t.Fatalf("the minted changes answer %v, want newest minted first %v", held.IDs, minted)
	}

	// ONE NATURAL KEY, ChangeDocProbe + 1 ids: the window's keys go first, then the arena's.
	shared := at.Add(time.Minute)
	var crowd []string
	for i := range search.ChangeDocProbe + 1 {
		crowd = append(crowd, mintedChangeID(shared, 7, uint64(i+1)))
	}
	odd := []string{mintedChangeID(time.UnixMilli(0), 0, 1), mintedChangeID(time.UnixMilli(1<<42-1), 31, 1)}
	position := int64(3)
	for i, id := range crowd {
		position++
		f.write(changeFixtureRow{id: id, repo: "repo-a", owner: "ada", state: "open",
			title: fmt.Sprintf("crowded c%04d", i)}, position)
	}
	for i, id := range odd {
		position++
		f.write(changeFixtureRow{id: id, repo: "repo-a", owner: "ada", state: "open",
			title: fmt.Sprintf("odd o%d", i)}, position)
	}
	f.settle(position)
	f.sweepAll()
	keys = f.docKeys()
	natural := shared.UnixMilli()<<5 | 7
	inWindow, inArena := 0, 0
	for _, id := range append(slices.Clone(crowd), odd...) {
		switch k := keys[id]; {
		case k >= natural && k < natural+search.ChangeDocProbe:
			inWindow++
		case k < 1<<45:
			inArena++
		default:
			t.Errorf("change %s took key %d, in neither its window nor the arena", id, k)
		}
	}
	if inWindow != search.ChangeDocProbe || inArena != 3 {
		t.Errorf("%d ids took window keys and %d arena keys, want %d and 3", inWindow, inArena,
			search.ChangeDocProbe)
	}
	// NEIGHBOURING KEYS keep their words apart: the second of one natural key is the first's next key.
	if keys[crowd[1]] != keys[crowd[0]]+1 {
		t.Fatalf("the second id of one natural key took %d, want %d", keys[crowd[1]], keys[crowd[0]]+1)
	}
	for _, terms := range [][]string{{"c0000", "c0001"}, {"c0001", "c0000"}} {
		if held := f.holding(search.ChangeQuery{Terms: terms, Limit: 10}); len(held.IDs) != 0 {
			t.Errorf("%v, each a word of one of two neighbouring documents, finds %v", terms, held.IDs)
		}
	}
	for i, id := range crowd {
		if held := f.holding(search.ChangeQuery{Terms: []string{"crowded", fmt.Sprintf("c%04d", i)},
			Limit: 10}); !slices.Equal(held.IDs, []string{id}) {
			t.Errorf("crowded change %d finds %v, want %s", i, held.IDs, id)
		}
	}
	for i, id := range odd {
		if held := f.holding(search.ChangeQuery{Terms: []string{"odd", fmt.Sprintf("o%d", i)},
			Limit: 10}); !slices.Equal(held.IDs, []string{id}) {
			t.Errorf("the change minted at an implausible instant %d finds %v, want %s", i, held.IDs, id)
		}
	}
}

// THE FIRST BUILD INDEXES THE NEWEST CHANGES FIRST: its floor only moves down, every change minted at
// or after it is held and none before, and a search for a word every change holds answers the newest
// from the top; a change opened or edited meanwhile is held by its words after the next sweep, and the
// walk back keeps the edit; once whole every change is held.
// Mutation: walk ids ascending, skip the forward walk while the build runs, or begin it at 0.
func TestTheFirstBuildIndexesTheNewestChangesFirst(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	const total = 2_000
	rows := make([]changeFixtureRow, total)
	for i := range rows {
		rows[i] = changeFixtureRow{id: changeUUID(i + 1), repo: "repo-a", owner: "ada", state: "open",
			title: fmt.Sprintf("every change w%04d", i+1), body: strings.Repeat("filler ", 10)}
	}
	f.writeAll(rows)
	search.SetChangeBatchCap(f.x, 7_000)
	floor := changeIDsTop
	check := func(when string) {
		t.Helper()
		held := f.holding(search.ChangeQuery{Terms: []string{"every"}, Limit: 5})
		if held.BackFloor > floor || held.BackFloor == floor && floor != "" {
			t.Fatalf("%s: the floor moved from %q to %q, not down", when, floor, held.BackFloor)
		}
		floor = held.BackFloor
		docs := f.docKeys()
		for _, row := range rows {
			if _, ok := docs[row.id]; ok != (row.id >= floor) {
				t.Fatalf("%s: change %s held %v below or above the floor %s", when, row.id, ok, floor)
			}
		}
		if want := []string{changeUUID(total), changeUUID(total - 1), changeUUID(total - 2), changeUUID(total - 3),
			changeUUID(total - 4)}; !slices.Equal(held.IDs, want) || !held.Truncated {
			t.Fatalf("%s: a word every change holds answers %v (truncated %v), want %v", when, held.IDs,
				held.Truncated, want)
		}
	}
	for i := range 5 {
		f.sweepOnce()
		check(fmt.Sprintf("sweep %d", i+1))
	}
	if floor == "" {
		t.Fatal("the premise: five sweeps of a small budget reached the oldest change")
	}

	// MID-BUILD: a change opened after the build began, and one below the floor edited.
	opened := changeFixtureRow{id: changeUUID(total + 1), repo: "repo-a", owner: "ada", state: "open",
		title: "opened meanwhile"}
	f.write(opened, total+1)
	edited := rows[4]
	edited.title = "edited meanwhile"
	f.write(edited, total+2)
	f.settle(total + 2)
	f.sweepOnce()
	if held := f.holding(search.ChangeQuery{Terms: []string{"meanwhile"}, Limit: 10}); held.BackFloor == "" ||
		!slices.Equal(held.IDs, []string{opened.id, edited.id}) {
		t.Fatalf("a sweep after an open and an edit mid-build answers %v (floor %q), want both", held.IDs,
			held.BackFloor)
	}
	f.sweepAll()
	rows[4] = edited
	want := map[string]changeFixtureRow{opened.id: opened}
	for _, row := range rows {
		want[row.id] = row
	}
	if held := f.holding(search.ChangeQuery{Terms: []string{"every"}, Limit: 1}); held.BackFloor != "" {
		t.Fatalf("swept to the end, the build reaches back only to %q", held.BackFloor)
	}
	f.assertHolds(want)
}

// changeIDsTop is above every change id, as a floor no build has moved yet.
const changeIDsTop = "~"

// A BATCH'S WRITE IS HELD TO ABOUT TWO SECONDS: its budget halves after a write past
// ChangeIndexBatchWrite, not below its least, doubles after one under a quarter of it, not past its
// cap, and stays between.
// Mutation: never halve, or halve below the least.
func TestABatchWriteIsHeldToAboutTwoSeconds(t *testing.T) {
	t.Parallel()
	limit, least := search.ChangeIndexBatchBytes, search.ChangeIndexBatchMinBytes
	for _, tc := range []struct {
		budget int
		took   time.Duration
		want   int
	}{
		{limit, search.ChangeIndexBatchWrite + time.Millisecond, limit / 2},
		{least + 1, 3 * search.ChangeIndexBatchWrite, least},
		{least, 3 * search.ChangeIndexBatchWrite, least},
		{limit / 4, search.ChangeIndexBatchWrite/4 - time.Millisecond, limit / 2},
		{limit / 2, time.Millisecond, limit},
		{limit, time.Millisecond, limit},
		{limit / 2, search.ChangeIndexBatchWrite, limit / 2},
		{limit / 2, search.ChangeIndexBatchWrite / 4, limit / 2},
	} {
		if got := search.NextChangeBatch(tc.budget, tc.took, least, limit); got != tc.want {
			t.Errorf("a budget of %d after a write of %v becomes %d, want %d", tc.budget, tc.took, got, tc.want)
		}
	}
}

// EACH KIND OF WRITE IS HELD BY ITS OWN TIMES: a slow backward write halves the next backward batch
// though a fast forward write came between, fast ones grow it back, the forward budget stays at its cap,
// and a slow purge drop's next step removes half the documents.
// Mutation: share one budget between the walks, time no write, or never move the drop step.
func TestEachKindOfWriteIsHeldByItsOwnTimes(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	f.addRepo("repo-b")
	const kib, total = 1 << 10, 300
	search.SetChangeBatchCap(f.x, 64*kib)
	slow, fast := 3*search.ChangeIndexBatchWrite, time.Millisecond
	var script []time.Duration
	search.SetChangeWriteTimer(f.x, func(time.Time) time.Duration {
		if len(script) == 0 {
			return fast
		}
		took := script[0]
		script = script[1:]
		return took
	})
	place := map[string]int{changeIDsTop: total + 1, "": 0}
	for i := range total {
		f.write(changeFixtureRow{id: changeUUID(i + 1), repo: "repo-a", owner: "ada", state: "open",
			title: "back", body: strings.Repeat("x", kib)}, int64(i+1))
		place[changeUUID(i+1)] = i + 1
	}
	f.settle(total)
	// The first sweep writes its backward batch alone; each later one a forward batch, then a backward.
	script = []time.Duration{slow, fast, slow, fast, slow, fast, fast, fast, fast}
	floor, position := changeIDsTop, int64(total)
	var took []int
	for k := range 5 {
		if k > 0 {
			position++
			f.write(changeFixtureRow{id: changeUUID(1_000 + k), repo: "repo-a", owner: "ada", state: "open",
				title: "forward"}, position)
			f.settle(position)
		}
		f.sweepOnce()
		_, next := f.backFloor()
		took = append(took, place[floor]-place[next])
		floor = next
	}
	if want := []int{64, 32, 16, 16, 32}; !slices.Equal(took, want) {
		t.Fatalf("the backward batches took %v rows of 1 KiB, want %v", took, want)
	}
	if forward, back, _ := search.ChangeBatchBudgets(f.x); forward != 64*kib || back != 64*kib {
		t.Fatalf("after fast writes the forward budget is %d and the backward %d, want both at %d",
			forward, back, 64*kib)
	}
	f.sweepAll()

	const doomed = 120
	for i := range doomed {
		position++
		f.write(changeFixtureRow{id: changeUUID(2_000 + i), repo: "repo-b", owner: "ada", state: "open",
			title: "doomed"}, position)
	}
	f.settle(position)
	f.sweepAll()
	f.purge("repo-b")
	script = []time.Duration{slow, slow, slow, slow}
	var dropped []int
	for range 4 {
		dropped = append(dropped, f.sweepOnce())
	}
	if want := []int{search.ChangeDropBatch, search.ChangeDropBatch / 2, search.ChangeDropBatch / 4,
		search.ChangeDropBatch / 8}; !slices.Equal(dropped, want) {
		t.Fatalf("slow drop steps removed %v documents, want %v", dropped, want)
	}
}

// THE FIRST BUILD BEGINS BELOW A RETAINED RECORD: begun while a record is retained, it begins at the
// applied prefix below it, leaves the rows above it to the forward walk, which waits, and once the record
// applies at its own position, the change it opened, the newest of all, is found.
// Mutation: begin the build at the settled checkpoint.
func TestTheFirstBuildBeginsBelowARetainedRecord(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	f.addRepo("repo-b")
	for i := range 3 {
		f.write(changeFixtureRow{id: changeUUID(i + 1), repo: "repo-b", owner: "bo", state: "open",
			title: "below"}, int64(i+1))
	}
	f.retain(5, "repo-a")
	for i := range 2 {
		f.write(changeFixtureRow{id: changeUUID(i + 4), repo: "repo-b", owner: "bo", state: "open",
			title: "above"}, int64(7+i))
	}
	f.settle(8)
	f.sweepAll()
	held := f.holding(search.ChangeQuery{Terms: []string{"above"}, Limit: 10})
	if held.BackPos >= 5 || len(held.IDs) != 0 || f.applied() >= held.Seen {
		t.Fatalf("begun while a record at 5 is retained, the build answers %+v (applied %d): want it begun "+
			"below 5, nothing above it indexed and the read not covered", held, f.applied())
	}
	opened := changeFixtureRow{id: changeUUID(100), repo: "repo-a", owner: "ada", state: "open",
		title: "opened by a newer build"}
	f.write(opened, 5)
	f.release()
	f.sweepAll()
	for term, want := range map[string][]string{"newer": {opened.id}, "above": {changeUUID(5), changeUUID(4)},
		"below": {changeUUID(3), changeUUID(2), changeUUID(1)}} {
		if held := f.holding(search.ChangeQuery{Terms: []string{term}, Limit: 10}); !slices.Equal(held.IDs, want) {
			t.Errorf("%q finds %v, want %v", term, held.IDs, want)
		}
	}
}

// distinctWords is n words of width letters and digits, the from-th onwards, each its own term.
func distinctWords(from, n, width int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]string, n)
	for k := range out {
		word := make([]byte, width)
		for i, v := width-1, from+k; i >= 0; i, v = i-1, v/len(alphabet) {
			word[i] = alphabet[v%len(alphabet)]
		}
		out[k] = string(word)
	}
	return strings.Join(out, " ")
}

// ---- fixtures ---------------------------------------------------------- //

// changeFixture is a node's own file and estate, with the change index over them.
type changeFixture struct {
	t      testing.TB
	ctx    context.Context
	db     *store.DB
	x      *search.ChangeIndexer
	purged map[string]bool
}

// changeFixtureRow is one change's row as the applier writes it, for the columns the index reads.
type changeFixtureRow struct {
	id, repo, owner, state, title, body string
}

func newChangeFixture(t testing.TB) *changeFixture {
	t.Helper()
	db := openStore(t)
	return &changeFixture{t: t, ctx: t.Context(), db: db,
		x: search.NewChangeIndexer(db, db.Replicated().Reader(), gitlet.Domain{}), purged: map[string]bool{}}
}

// changeEpoch is when the fixture's changes begin to be minted.
var changeEpoch = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// changeUUID is the i-th change's id: a UUIDv7 minted i seconds after changeEpoch with no random bits,
// so its natural key's order is i's.
func changeUUID(i int) string {
	return mintedChangeID(changeEpoch.Add(time.Duration(i)*time.Second), 0, 0)
}

// mintedChangeID is a UUIDv7 minted at at, its five bits after the version nibble tie and its last seven
// bytes rest.
func mintedChangeID(at time.Time, tie byte, rest uint64) string {
	var u uuid.UUID
	binary.BigEndian.PutUint64(u[0:8], uint64(at.UnixMilli())<<16)
	u[6] = 0x70 | tie>>1&0x0f
	u[7] = tie & 1 << 7
	binary.BigEndian.PutUint64(u[8:16], rest)
	u[8] = 0x80
	return u.String()
}

func (f *changeFixture) exec(query string, args ...any) {
	f.t.Helper()
	if err := f.db.Replicated().Tx(f.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, query, args...)
		return err
	}); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
}

func (f *changeFixture) addRepo(id string) {
	f.t.Helper()
	f.exec(`INSERT INTO code_repos (id, name, status, managed_by, owner_kind, owner, default_branch, created_at)
		VALUES (?, ?, 'active', 'config', 'unit', 'ENG', 'main', 0)`, id, id)
}

// write is a record's write of row at position, its description beside it where it has one.
func (f *changeFixture) write(row changeFixtureRow, position int64) {
	f.t.Helper()
	f.exec(`INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref, target_ref,
		head_oid, revision, owner, owner_kind, opened_at, opened_position, version, position)
		VALUES (?, ?, 1, ?, ?, ?, 'refs/heads/topic', 'refs/heads/main', '', 1, ?, 'agent', 0, ?, 1, ?)
		ON CONFLICT (id) DO UPDATE SET title = excluded.title, body_bytes = excluded.body_bytes,
			state = excluded.state, position = excluded.position`,
		row.id, row.repo, row.title, len(row.body), row.state, row.owner, position, position)
	if row.body == "" {
		f.exec(`DELETE FROM code_change_bodies WHERE change_id = ?`, row.id)
		return
	}
	f.exec(`INSERT INTO code_change_bodies (change_id, body) VALUES (?, ?)
		ON CONFLICT (change_id) DO UPDATE SET body = excluded.body`, row.id, row.body)
}

// purge is a repository's purge: its changes, their descriptions and its row gone.
func (f *changeFixture) purge(repo string) {
	f.t.Helper()
	f.exec(`DELETE FROM code_change_bodies WHERE change_id IN (SELECT id FROM code_changes WHERE repo_id = ?)`, repo)
	f.exec(`DELETE FROM code_changes WHERE repo_id = ?`, repo)
	f.exec(`DELETE FROM code_repos WHERE id = ?`, repo)
	f.purged[repo] = true
}

// settle moves the code log's checkpoint to position, as an applier's commit does.
func (f *changeFixture) settle(position int64) {
	f.t.Helper()
	f.exec(`INSERT INTO statelog_cursor (stream, generation, seq, stream_created_at, updated_at, applied_version)
		VALUES (?, 0, ?, 0, 0, 0) ON CONFLICT (stream) DO UPDATE SET seq = excluded.seq`,
		gitlet.Domain{}.Stream().Name, position)
}

// retain files a record of repo's this node could not decode, at position.
func (f *changeFixture) retain(position int64, repo string) {
	f.t.Helper()
	f.exec(`INSERT INTO code_log_deferred (position, subject, subject_kind, subject_id, version, payload, stored_at)
		VALUES (?, ?, 'repo', ?, ?, x'00', 0)`, position, "repo."+repo, repo, gitlet.RecordVersion+1)
	f.exec(`INSERT INTO code_log_deferred_scope (position, path) VALUES (?, ?)`, position, gitlet.RepoPath(repo))
}

// release is the next build applying every retained record.
func (f *changeFixture) release() {
	f.t.Helper()
	f.exec(`DELETE FROM code_log_deferred_scope`)
	f.exec(`DELETE FROM code_log_deferred`)
}

func (f *changeFixture) sweepOnce() int {
	f.t.Helper()
	n, err := f.x.Sweep(f.ctx, time.Now())
	if err != nil {
		f.t.Fatalf("Sweep: %v", err)
	}
	return n
}

func (f *changeFixture) sweepAll() int {
	f.t.Helper()
	total := 0
	for range 1_000 {
		n := f.sweepOnce()
		if n == 0 {
			return total
		}
		total += n
	}
	f.t.Fatal("the change index never caught up in a thousand sweeps")
	return total
}

func (f *changeFixture) holding(q search.ChangeQuery) search.ChangeHeld {
	f.t.Helper()
	held, err := f.x.Holding(f.ctx, q)
	if err != nil {
		f.t.Fatalf("Holding(%+v): %v", q, err)
	}
	return held
}

func (f *changeFixture) instance() string {
	f.t.Helper()
	var instance string
	if err := f.db.Replicated().Reader().Read(f.ctx, func(tx *sql.Tx) (err error) {
		instance, err = statelog.EstateInstance(f.ctx, tx)
		return err
	}); err != nil {
		f.t.Fatalf("read the estate's instance: %v", err)
	}
	return instance
}

// count reads one integer from this node's own file.
func (f *changeFixture) count(query string) int {
	f.t.Helper()
	var n int
	if err := f.db.SQL().QueryRowContext(f.ctx, query).Scan(&n); err != nil {
		f.t.Fatalf("%s: %v", query, err)
	}
	return n
}

// postingRows is every posting by its rowid.
func (f *changeFixture) postingRows() map[int64]string {
	f.t.Helper()
	rows, err := f.db.SQL().QueryContext(f.ctx, `SELECT rowid, term FROM change_postings`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var term string
		if scanErr := rows.Scan(&id, &term); scanErr != nil {
			f.t.Fatal(scanErr)
		}
		out[id] = term
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// orphanPostings counts the postings whose slot names no document's key.
const orphanPostings = `SELECT COUNT(*) FROM change_postings WHERE (slot >> 16) NOT IN (SELECT doc FROM change_docs)`

// docKeys is every document's key by its change's id.
func (f *changeFixture) docKeys() map[string]int64 {
	f.t.Helper()
	rows, err := f.db.SQL().QueryContext(f.ctx, `SELECT id, doc FROM change_docs`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var key []byte
		var doc int64
		if scanErr := rows.Scan(&key, &doc); scanErr != nil {
			f.t.Fatal(scanErr)
		}
		out[uuid.Must(uuid.FromBytes(key)).String()] = doc
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// slotTerms is the terms in document doc's slot range, in slot order.
func (f *changeFixture) slotTerms(doc int64) []string {
	f.t.Helper()
	rows, err := f.db.SQL().QueryContext(f.ctx, `SELECT term FROM change_postings WHERE slot >= ? AND slot < ?
		ORDER BY slot`, doc<<16, (doc+1)<<16)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var term string
		if scanErr := rows.Scan(&term); scanErr != nil {
			f.t.Fatal(scanErr)
		}
		out = append(out, term)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// indexedDoc is one document as the index holds it.
type indexedDoc struct {
	doc                int64
	repo, owner, state string
}

// indexedDocs is every document the index holds, by its change's id.
func (f *changeFixture) indexedDocs() map[string]indexedDoc {
	f.t.Helper()
	got := map[string]indexedDoc{}
	rows, err := f.db.SQL().QueryContext(f.ctx, `SELECT doc, id, repo_id, owner, state FROM change_docs`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key []byte
		var d indexedDoc
		if scanErr := rows.Scan(&d.doc, &key, &d.repo, &d.owner, &d.state); scanErr != nil {
			f.t.Fatal(scanErr)
		}
		got[uuid.Must(uuid.FromBytes(key)).String()] = d
	}
	if err = rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return got
}

// assertDoc fails the case unless d holds row's scope and exactly its terms.
func (f *changeFixture) assertDoc(id string, d indexedDoc, ok bool, row changeFixtureRow) {
	f.t.Helper()
	if !ok {
		f.t.Errorf("change %s has no document", id)
		return
	}
	terms := map[string]int{}
	maps.Copy(terms, textindex.Analyze(row.title))
	maps.Copy(terms, textindex.Analyze(row.body))
	if held := f.slotTerms(d.doc); d.repo != row.repo || d.owner != row.owner || d.state != row.state ||
		!slices.Equal(held, slices.Sorted(maps.Keys(terms))) {
		f.t.Errorf("change %s's document is %+v holding %v, want %s/%s/%s %v", id, d, held, row.repo,
			row.owner, row.state, slices.Sorted(maps.Keys(terms)))
	}
}

// assertHolds fails the case unless the index holds exactly want's documents, each slot range exactly
// its terms, and no posting of no document.
func (f *changeFixture) assertHolds(want map[string]changeFixtureRow) {
	f.t.Helper()
	got := f.indexedDocs()
	if orphans := f.count(orphanPostings); orphans != 0 {
		f.t.Errorf("%d postings name no document's key", orphans)
	}
	if len(got) != len(want) {
		f.t.Errorf("the index holds %d documents, want %d", len(got), len(want))
	}
	for id, row := range want {
		d, ok := got[id]
		f.assertDoc(id, d, ok, row)
	}
}

// assertCovered fails the case unless every live row a walk has reached has its document decided from
// that row: above where the first build began and no further than the watermark, or at or below it
// with an id at or above the floor (ADR-0156). The fixture settles its log, so the index is covered.
func (f *changeFixture) assertCovered() {
	f.t.Helper()
	var through, back int64
	var floor string
	if err := f.db.SQL().QueryRowContext(f.ctx, `SELECT through_pos, back_pos, back_floor
		FROM change_index_state`).Scan(&through, &back, &floor); err != nil {
		f.t.Fatal(err)
	}
	if back < 0 {
		return
	}
	got := f.indexedDocs()
	rows := map[string]changeFixtureRow{}
	positions := map[string]int64{}
	if err := f.db.Replicated().Reader().Read(f.ctx, func(tx *sql.Tx) error {
		read, err := tx.QueryContext(f.ctx, `SELECT c.id, c.repo_id, c.owner, c.state, c.title,
			COALESCE(b.body, ''), c.position FROM code_changes AS c
			LEFT JOIN code_change_bodies AS b ON b.change_id = c.id`)
		if err != nil {
			return err
		}
		defer func() { _ = read.Close() }()
		for read.Next() {
			var row changeFixtureRow
			var position int64
			if err = read.Scan(&row.id, &row.repo, &row.owner, &row.state, &row.title, &row.body,
				&position); err != nil {
				return err
			}
			rows[row.id], positions[row.id] = row, position
		}
		return read.Err()
	}); err != nil {
		f.t.Fatal(err)
	}
	for id, row := range rows {
		p := positions[id]
		if p <= back && (floor == "" || id >= floor) || p > back && p <= through {
			d, ok := got[id]
			f.assertDoc(id, d, ok, row)
		}
	}
}

// backFloor is where the first build began and the least id it has reached back to.
func (f *changeFixture) backFloor() (int64, string) {
	f.t.Helper()
	var back int64
	var floor string
	if err := f.db.SQL().QueryRowContext(f.ctx, `SELECT back_pos, back_floor FROM change_index_state`).
		Scan(&back, &floor); err != nil {
		f.t.Fatal(err)
	}
	return back, floor
}

// applied is the code log's applied prefix in the estate now.
func (f *changeFixture) applied() int64 {
	f.t.Helper()
	var at int64
	if err := f.db.Replicated().Reader().Read(f.ctx, func(tx *sql.Tx) error {
		prefix, err := statelog.PrefixIn(f.ctx, tx, gitlet.Domain{})
		at = prefix.Applied().Packed()
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return at
}
