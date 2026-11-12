package gitlet

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A PAGE'S QUEUED CHANGES ARE PLACED BY ONE WALK A TARGET, each at its rank
// among its target's live entries: main's live entries at seqs 10, 30 and 50,
// an entry at 20 that ended and one at 40 on another target between them,
// answer places 1, 2 and 3 however the page lists them, and release's one
// entry place 1 — an ended entry and another target's taking no place.
//
// Mutations: number a place from 0 (places[e.EntryID] = at) — every place one
// short, red; walk a target's live entries up to the LAST of the page's
// entries there rather than the latest (latest[key] = e.Seq) — main's last
// listed is at 30, so the walk misses 50, red; place an entry by every walk,
// whatever its target — release's entry is sought among main's, red.
func TestAPageOfQueuedChangesIsPlacedOneWalkATarget(t *testing.T) {
	t.Parallel()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = node.Close() })
	const main, release = "refs/heads/main", "refs/heads/release"
	rows := []struct {
		entry, target, state string
		seq                  int64
	}{
		{"e10", main, "waiting", 10}, {"e20", main, "landed", 20}, {"e30", main, "testing", 30},
		{"e40", release, "waiting", 40}, {"e50", main, "waiting", 50},
	}
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO code_queue (repo_id, entry_id, target_ref,
					change_id, seq, head_oid, method, delete_source, close_task, enqueued_by, enqueued_kind,
					enqueued_operator, state, enqueued_at)
				VALUES ('repo-1', ?, ?, ?, ?, '', 'merge', 0, 0, 'dev', 'agent', '', ?, 1)`,
				r.entry, r.target, "change-"+r.entry, r.seq, r.state); err != nil {
				return fmt.Errorf("write entry %s: %w", r.entry, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	entry := func(id, target string, seq int64) queueEntry {
		return queueEntry{RepoID: "repo-1", EntryID: id, TargetRef: target, Seq: seq}
	}
	var places map[string]int
	if err := db.Read(t.Context(), func(tx *sql.Tx) (err error) {
		places, err = placesIn(t.Context(), tx, []queueEntry{entry("e50", main, 50), entry("e40", release, 40),
			entry("e10", main, 10), entry("e30", main, 30)})
		return err
	}); err != nil {
		t.Fatalf("placesIn: %v", err)
	}
	for id, want := range map[string]int{"e10": 1, "e30": 2, "e50": 3, "e40": 1} {
		if places[id] != want {
			t.Errorf("entry %s is placed %d, want %d (places %v)", id, places[id], want, places)
		}
	}
	if len(places) != 4 {
		t.Errorf("placesIn answered %v, want the four entries asked and no other", places)
	}
}
