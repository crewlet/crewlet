package search

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A PROCESS HOLDS ONE DECODED INDEX PER LOG, and a partition's retrain
// replaces its own entry.
//
// A partitioned layout's search asks every partition a node holds, one after
// another, and each has an index of its own. A memo with one slot evicted
// itself on every partition of every search — a read of the centroids, a hash
// and a decode per partition per query. Held per log, the second partition's
// load leaves the first one's in place: here the first store's centroids row is
// deleted after it was decoded, so a second load of it can only succeed from
// the memo. And a retrain of a log replaces that log's entry rather than
// lingering beside it, so the memo holds as many indexes as partitions.
func TestAProcessHoldsOneDecodedIndexPerLog(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	memo := &ivfMemo{}
	a, headA := memoStore(t, "log-a", 1)
	b, headB := memoStore(t, "log-b", 2)
	load := func(db *store.DB, h IndexHead) error {
		return storetest.EstateOf(db).Read(ctx, func(tx *sql.Tx) error {
			_, err := memo.load(ctx, tx, h)
			return err
		})
	}
	if err := load(a, headA); err != nil {
		t.Fatal(err)
	}
	if err := load(b, headB); err != nil {
		t.Fatal(err)
	}
	if err := storetest.EstateOf(a).Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM kb_ivf_centroids`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := load(a, headA); err != nil {
		t.Fatalf("the first log's index was evicted by the second's: %v", err)
	}

	c, retrained := memoStore(t, "log-a", 3)
	if err := load(c, retrained); err != nil {
		t.Fatal(err)
	}
	if len(memo.entries) != 2 {
		t.Fatalf("the memo holds %d indexes for two logs after one retrained",
			len(memo.entries))
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
