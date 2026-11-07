package search

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE DECODED INDEX IS SERVED FROM THE MEMO WHILE ITS DIGEST HOLDS, AND A
// RETRAIN REPLACES IT.
//
// A node holds one index, so the memo has one slot: a load of the centroids it
// decoded last needs no read at all — here the store's centroids row is deleted
// after the first load, so a second load can only succeed from the memo. And a
// hit is the DIGEST's, never the generation's: another store's index at the
// same generation is read from that store, and once it has been, the first
// index is no longer held — a retrain leaves nothing behind.
func TestTheDecodedIndexIsHeldUntilItsDigestMoves(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	memo := &ivfMemo{}
	a, headA := memoStore(t, "log", 1)
	b, headB := memoStore(t, "log", 2)
	load := func(db *store.DB, h IndexHead) (IVF, error) {
		var index IVF
		err := db.Replicated().Read(ctx, func(tx *sql.Tx) error {
			var err error
			index, err = memo.load(ctx, tx, h)
			return err
		})
		return index, err
	}
	first, err := load(a, headA)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Replicated().Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM kb_ivf_centroids`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if again, err := load(a, headA); err != nil || !reflect.DeepEqual(again, first) {
		t.Fatalf("the index decoded last was read again (%v), want it from the memo", err)
	}

	// THE SAME GENERATION, OTHER CENTROIDS: read from their own store.
	other, err := load(b, headB)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(other, first) {
		t.Fatal("another store's index at the same generation was answered from the memo")
	}
	// AND THE FIRST IS NO LONGER HELD: its row is gone, so a load must fail.
	if _, err := load(a, headA); err == nil {
		t.Fatal("the index the memo held before was still answered after another replaced it")
	}
}

// memoStore is a store with one installed index of the given seed's
// centroids, trained for log.
func memoStore(t *testing.T, log string, seed byte) (*store.DB, IndexHead) {
	t.Helper()
	ctx := context.Background()
	db, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })
	const dim, lists = 64, 4
	centroids := make([]byte, 8*CodeWords(dim)*lists)
	for i := range centroids {
		centroids[i] = seed + byte(i)
	}
	h := IndexHead{Generation: 9, Log: log, Model: "m", Dim: dim, Lists: lists,
		Probes: 1, Digest: digestOf(centroids)}
	if err := db.Replicated().Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO kb_ivf_centroids (id, digest, centroids)
			VALUES (1, ?, ?)`, h.Digest, centroids)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return db, h
}

// A STORED EXCERPT SAYS WHERE IT WAS CUT, so a snippet whose window reaches
// its end does not end as though the document did.
func TestAStoredExcerptIsMarkedWhereItWasCut(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("word ", 400)
	got := excerptOf("", long)
	if len(got) > excerptLimit || !strings.HasSuffix(got, "…") {
		t.Fatalf("a cut excerpt is %d bytes ending %q", len(got), got[max(0, len(got)-6):])
	}
	if short := excerptOf("", "a short page"); short != "a short page" {
		t.Errorf("a short body was altered: %q", short)
	}
}

// AN EXCERPT READS AS TEXT. A mark only means what it means at the start of a
// line, so one that survives the flattening is drawn as words by every reader.
func TestAnExcerptReadsAsTextNotAsMarkup(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, title, body, want string }{
		{
			name:  "headings, lists and quotes lose their marks",
			title: "Page",
			body:  "## Why this matters\n\n- Learners trust us\n1. Review it\n> Said once\n",
			want:  "Why this matters Learners trust us Review it Said once",
		},
		{
			name:  "a leading repeat of the title goes, a later one stays",
			title: "Release notes",
			body:  "# Release notes\n\nOpening words about Release notes\nRelease notes",
			want:  "Opening words about Release notes Release notes",
		},
		{
			name:  "rules and fences are no words at all, and a table keeps its cells",
			title: "Page",
			body:  "---\n| Name | Size |\n|---|---:|\n| a | 1 |\n```go\nx := 1\n```\n",
			want:  "Name Size a 1 x := 1",
		},
		{
			name:  "marks inside a line are left to the reader that renders them",
			title: "Page",
			body:  "a #hash and 1. not a list, *still* marked",
			want:  "a #hash and 1. not a list, *still* marked",
		},
	} {
		if got := excerptOf(c.title, c.body); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
