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

// THE INDEX HOLDS EVERY CHANGE'S WORDS, whatever moved it: after a seeded run of opens, edits, moves
// and a purge, swept at random, each live document holds its scope and exactly its terms.
// Mutation: drop the state update on an equal hash, drop the purge follower, clear only the slots the
// new terms overwrite, or leave last_seq unmoved.
func TestTheChangeIndexHoldsEveryChangesWords(t *testing.T) {
	t.Parallel()
	for seed := range uint64(4) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			f := newChangeFixture(t)
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
			opened := 0
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
				if rng.IntN(4) == 0 {
					f.sweepOnce()
				}
				if position == 200 {
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
			f.assertHolds(rows)
		})
	}
}

// A MOVE THAT LEAVES THE TEXT WRITES NO POSTING: a head move and a state move after the first index
// leave every posting's slot and every document's number as they were and move the watermark.
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
	before, numbered := f.postingRows(), f.docNumbers()
	row.state = "draft"
	f.write(row, 3)
	f.write(row, 4)
	f.settle(4)
	f.sweepAll()
	if after := f.postingRows(); !maps.Equal(before, after) {
		t.Fatalf("a head move and a state move rewrote the postings: %v, then %v", before, after)
	}
	if after := f.docNumbers(); !maps.Equal(numbered, after) {
		t.Fatalf("a head move and a state move renumbered the documents: %v, then %v", numbered, after)
	}
	if held := f.holding(search.ChangeQuery{Terms: []string{"login"}, State: "draft", Limit: 10}); held.Through != 4 ||
		len(held.IDs) != 1 {
		t.Fatalf("after the moves the index answers %+v, want the change in its new state through 4", held)
	}
}

// A BATCH IS READ WHOLE AT ITS LAST POSITION: changes at one position past the row bound are indexed
// in one batch, and descriptions past the byte bound are cut into batches each ending on a position.
// Mutation: cut a batch at a row count.
func TestAChangeBatchIsReadWholeAtItsLastPosition(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	many := search.ChangeIndexBatchRows + 88
	for i := range many {
		f.write(changeFixtureRow{id: changeUUID(i + 1), repo: "repo-a", owner: "ada", state: "open",
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

	// DESCRIPTIONS PAST THE BYTE BOUND: two positions of three long changes each.
	long := strings.Repeat("word ", search.ChangeIndexBatchBytes/5/3+1)
	for i := range 6 {
		f.write(changeFixtureRow{id: changeUUID(many + i + 1), repo: "repo-a", owner: "ada", state: "open",
			title: "a long one", body: long}, int64(8+i/3))
	}
	f.settle(9)
	if n := f.sweepOnce(); n != 3 {
		t.Fatalf("a sweep past the byte bound walked %d changes, want the three at its position", n)
	}
	if held := f.holding(search.ChangeQuery{Terms: []string{"long"}, Limit: 10}); held.Through != 8 {
		t.Fatalf("a batch cut by its bytes ended at %d, want the whole position 8", held.Through)
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

// A REBUILD DROPS THE INDEX IN STEPS, AND A RESTART FINISHES IT: a sweep drops one batch, and a second
// indexer over the same store finishes the drop before it walks, so a change the replaced copy held and
// the donor's does not is found by nobody, and the walk after it numbers documents from 1 again.
// Mutation: resume the walk over a half-dropped index, or leave last_seq at a rebuild's mark.
func TestARebuildDropsTheChangeIndexInStepsAndARestartFinishesIt(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	f.addRepo("repo-a")
	total := 2*search.ChangeDropBatch + 10
	for i := range total {
		f.write(changeFixtureRow{id: changeUUID(i + 1), repo: "repo-a", owner: "ada", state: "open",
			title: "the original copy"}, int64(i+1))
	}
	f.settle(int64(total))
	f.sweepAll()
	gone := changeUUID(total / 2)
	f.exec(`DELETE FROM code_changes WHERE id = ?`, gone)
	f.x.RequestRebuild()
	if held := f.holding(search.ChangeQuery{Terms: []string{"original"}, Limit: 10}); !held.Rebuilding {
		t.Fatal("an index asked to rebuild answers as built")
	}
	f.sweepOnce()
	if docs := f.count(`SELECT COUNT(*) FROM change_docs`); docs < total-search.ChangeDropBatch {
		t.Fatalf("one sweep dropped %d of %d documents, past one batch of %d", total-docs, total,
			search.ChangeDropBatch)
	}
	f.x = search.NewChangeIndexer(f.db, f.db.Replicated().Reader(), gitlet.Domain{})
	f.sweepAll()
	held := f.holding(search.ChangeQuery{Terms: []string{"original"}, Limit: 1_000})
	if held.Rebuilding || len(held.IDs) != total-1 || slices.Contains(held.IDs, gone) {
		t.Fatalf("the restarted index answers %d ids (rebuilding %v), the gone change among them %v",
			len(held.IDs), held.Rebuilding, slices.Contains(held.IDs, gone))
	}
	if orphans := f.count(orphanPostings); orphans != 0 {
		t.Fatalf("the rebuild left %d postings of no document", orphans)
	}
	if first, last := f.count(`SELECT MIN(seq) FROM change_docs`), f.count(`SELECT last_seq FROM change_index_state`); first != 1 ||
		last != total-1 {
		t.Fatalf("the walk after the rebuild numbered documents %d to %d, want 1 to %d", first, last, total-1)
	}
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

// EVERY INDEX WRITE RECORDS WHAT IT WAS DECIDED FROM: a walk batch and a purge drop each raise Seen to
// the settled checkpoint of the snapshot they read and never lower it, a rebuild's mark sets it to 0,
// and an idle sweep or a walk held at a retained record writes nothing.
// Mutation: drop the update from the purge drop, or write it without MAX.
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
	if got := seen(); got != 0 {
		t.Fatalf("a rebuild's mark left Seen at %d", got)
	}
}

// HOLDING ANSWERS THE NEWEST CHANGES HOLDING EVERY WORD in its scope, driven by the narrowest of the
// scope and the words, in the order the index took them in.
// Mutation: drive every search by its rarest word, drop an EXISTS, test a word over a neighbour's
// slot range, order ascending, or order by the change's id.
func TestHoldingAnswersTheNewestChangesHoldingEveryWord(t *testing.T) {
	t.Parallel()
	f := newChangeFixture(t)
	for _, r := range []string{"busy", "quiet"} {
		f.addRepo(r)
	}
	id := 0
	add := func(repo, owner, state, title, body string) string {
		id++
		f.write(changeFixtureRow{id: changeUUID(id), repo: repo, owner: owner, state: state, title: title,
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
	// AN OLDER CHANGE TAKEN IN AFTER A NEWER ONE is answered before it: the index's order, not the ids'.
	newer := add("quiet", "cy", "draft", "reordered", "")
	id++
	older := changeUUID(0)
	f.write(changeFixtureRow{id: older, repo: "quiet", owner: "cy", state: "draft", title: "reordered"}, int64(id))
	add("quiet", "bo", "draft", "neighbour", "")
	f.settle(int64(id))
	f.sweepAll()
	search.SetChangeTextScan(f.x, 20)

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
		"the order the index took them in": {q: search.ChangeQuery{Terms: []string{"reordered"}, Limit: 10},
			want: []string{older, newer}},
		"the order the index took them in, read by owner": {q: search.ChangeQuery{Terms: []string{"reordered"},
			Owner: "cy", Limit: 10}, want: []string{older, newer}},
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

// changeUUID is the i-th change's id, a UUIDv7 whose order is i's.
func changeUUID(i int) string {
	var u uuid.UUID
	binary.BigEndian.PutUint64(u[0:8], uint64(i)<<16)
	u[6] = 0x70 | u[6]&0x0f
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

// orphanPostings counts the postings whose slot names no document's number.
const orphanPostings = `SELECT COUNT(*) FROM change_postings WHERE (slot >> 16) NOT IN (SELECT seq FROM change_docs)`

// docNumbers is every document's number by its change's id.
func (f *changeFixture) docNumbers() map[string]int64 {
	f.t.Helper()
	rows, err := f.db.SQL().QueryContext(f.ctx, `SELECT id, seq FROM change_docs`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var key []byte
		var seq int64
		if scanErr := rows.Scan(&key, &seq); scanErr != nil {
			f.t.Fatal(scanErr)
		}
		out[uuid.Must(uuid.FromBytes(key)).String()] = seq
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// slotTerms is the terms in document seq's slot range, in slot order.
func (f *changeFixture) slotTerms(seq int64) []string {
	f.t.Helper()
	rows, err := f.db.SQL().QueryContext(f.ctx, `SELECT term FROM change_postings WHERE slot >= ? AND slot < ?
		ORDER BY slot`, seq<<16, (seq+1)<<16)
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

// assertHolds fails the case unless the index holds exactly want's documents, each slot range exactly
// its terms, no posting of no document, and last_seq at or past every number.
func (f *changeFixture) assertHolds(want map[string]changeFixtureRow) {
	f.t.Helper()
	type doc struct {
		seq                int64
		repo, owner, state string
	}
	got := map[string]doc{}
	rows, err := f.db.SQL().QueryContext(f.ctx, `SELECT seq, id, repo_id, owner, state FROM change_docs`)
	if err != nil {
		f.t.Fatal(err)
	}
	for rows.Next() {
		var key []byte
		var d doc
		if scanErr := rows.Scan(&d.seq, &key, &d.repo, &d.owner, &d.state); scanErr != nil {
			f.t.Fatal(scanErr)
		}
		got[uuid.Must(uuid.FromBytes(key)).String()] = d
	}
	_ = rows.Close()
	if orphans := f.count(orphanPostings); orphans != 0 {
		f.t.Errorf("%d postings name no document's number", orphans)
	}
	if last, top := f.count(`SELECT last_seq FROM change_index_state`), f.count(`SELECT COALESCE(MAX(seq), 0)
		FROM change_docs`); last < top {
		f.t.Errorf("last_seq is %d below the document numbered %d", last, top)
	}
	if len(got) != len(want) {
		f.t.Errorf("the index holds %d documents, want %d", len(got), len(want))
	}
	for id, row := range want {
		d, ok := got[id]
		if !ok {
			f.t.Errorf("change %s has no document", id)
			continue
		}
		terms := map[string]int{}
		maps.Copy(terms, textindex.Analyze(row.title))
		maps.Copy(terms, textindex.Analyze(row.body))
		if held := f.slotTerms(d.seq); d.repo != row.repo || d.owner != row.owner || d.state != row.state ||
			!slices.Equal(held, slices.Sorted(maps.Keys(terms))) {
			f.t.Errorf("change %s's document is %+v holding %v, want %s/%s/%s %v", id, d, held, row.repo,
				row.owner, row.state, slices.Sorted(maps.Keys(terms)))
		}
	}
}
