package search

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/textindex"
)

// A ROOM LIST SPLIT ACROSS STATEMENTS ANSWERS WHAT ONE STATEMENT DOES, under
// ONE scan budget.
//
// The driver this build ships binds tens of thousands of parameters, so no
// visible set a test can build reaches the chunking through a search: here the
// three reads a ranking makes are run at a chunk of one room against a chunk
// that holds every room. The statistics and the postings must come out the
// same — the chunks are disjoint because a document is in one room — and a
// term in more postings than the budget must come out at the budget, not at a
// budget per chunk, which would make the scan grow with the room count it was
// split to stop depending on.
func TestAChunkedRoomListAnswersWhatOneStatementDoes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"),
		store.Options{}, 1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close the store: %v", err)
		}
	})
	const rooms, perRoom = 3, maxPostingScan / 2
	if err := estate.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, estate.Caps().MaxVariables, `
			INSERT INTO chat_messages (id, channel_id, thread_root, author_handle,
			                           author_kind, body, links, channel_seq,
			                           created_at, version, shard, document)
			VALUES`, `(?, ?, '', 'ada', 'human', ?, '', ?, ?, ?, 0, X'')`, "",
			rooms*perRoom, func(i int) []any {
				body := "deploy the build"
				if i%7 == 0 {
					body = "deploy the rare thing"
				}
				return []any{fmt.Sprintf("m%05d", i), fmt.Sprintf("r%d", i%rooms),
					body, i + 1, 1000 + i, 100 + i}
			})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	x := NewChatIndexer(db, estate.Reader())
	for range 100 {
		n, err := x.Sweep(ctx, time.Now())
		if err != nil {
			t.Fatalf("Sweep: %v", err)
		}
		if n == 0 {
			break
		}
	}

	all := []string{"r0", "r1", "r2"}
	type ranking struct {
		corpus   textindex.Corpus
		docs     map[string]int
		postings map[string][]textindex.Posting
	}
	rank := func(chunk int) ranking {
		out := ranking{docs: map[string]int{}, postings: map[string][]textindex.Posting{}}
		if err := db.Read(ctx, func(tx *sql.Tx) error {
			var err error
			if out.corpus, err = x.roomsCorpus(ctx, tx, all, chunk); err != nil {
				return err
			}
			for _, term := range []string{"deploy", "rare"} {
				if out.docs[term], err = x.termDocs(ctx, tx, term, all, chunk); err != nil {
					return err
				}
				if out.postings[term], err = x.chatPostings(ctx, tx, term, all, "", chunk); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	whole, split := rank(len(all)), rank(1)

	if whole.corpus != split.corpus || !reflect.DeepEqual(whole.docs, split.docs) {
		t.Fatalf("the statistics over one statement were %+v %v and over a "+
			"statement per room %+v %v", whole.corpus, whole.docs, split.corpus, split.docs)
	}
	if whole.corpus.Docs != rooms*perRoom {
		t.Fatalf("the rooms count %d documents, want %d", whole.corpus.Docs, rooms*perRoom)
	}
	ids := func(ps []textindex.Posting) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.DocID)
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(ids(whole.postings["rare"]), ids(split.postings["rare"])) {
		t.Fatalf("a term under the budget read %d postings in one statement and "+
			"%d split by room", len(whole.postings["rare"]), len(split.postings["rare"]))
	}
	for name, got := range map[string]int{
		"one statement":      len(whole.postings["deploy"]),
		"a statement a room": len(split.postings["deploy"]),
	} {
		if got != maxPostingScan {
			t.Errorf("a term in %d postings read %d of them in %s, want the "+
				"budget of %d — a budget per statement grows the scan with the "+
				"room count", rooms*perRoom, got, name, maxPostingScan)
		}
	}
}
