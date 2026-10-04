package search

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
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
		err := storetest.EstateOf(db).Read(ctx, func(tx *sql.Tx) error {
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
	if err := storetest.EstateOf(a).Tx(ctx, func(tx *sql.Tx) error {
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
	if err := storetest.EstateOf(db).Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO kb_ivf_centroids (id, digest, centroids)
			VALUES (1, ?, ?)`, h.Digest, centroids)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return db, h
}
