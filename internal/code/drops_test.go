package code_test

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/store"
)

// THE REF DROPS ON THE WHOLE PATH: a real broker, store and applier, and the
// purge's decide over them. The drops are SEEDED, at the position and broker
// instant each case is about, since what a purge decides of them is a count.

// seedDrops writes count ref drops of repoID straight into a node's estate, all
// left by one record — at the packed position on the code log and its broker
// instant at.
func (n *node) seedDrops(repoID string, position int64, at time.Time, count int) {
	n.t.Helper()
	ctx := n.t.Context()
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, n.db.Caps().MaxVariables,
			`INSERT INTO code_ref_drops (repo_id, position, ref, old, at) VALUES `, `(?, ?, ?, ?, ?)`, ``,
			count, func(i int) []any {
				return []any{repoID, position, fmt.Sprintf("refs/heads/dropped-%06d", i),
					oidOf(fmt.Sprint(repoID, i)), store.EncodeTime(at)}
			})
		return err
	}); err != nil {
		n.t.Fatalf("seed %s's ref drops: %v", repoID, err)
	}
}

// A PURGE REMOVES A REPOSITORY'S REF DROPS, whatever their grace — they keep
// objects of the repository's own packs reachable, and those go with it — and
// counts them toward its parts: a deleted repository holding nothing but ref
// drops, one past a part's rows, is purged in a part and then the purge, never
// by one unbounded statement, and no drop of it is left.
//
// Mutation: leave the drops out of the bounded count, and the purge is one
// record deleting all of them at once; drop the part's delete, and the parts
// remove nothing; drop the final purge's, and a drop outlives its repository.
func TestAPurgeRemovesARepositorysRefDrops(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("one"), t1)
	id := n.mustID("app")
	n.deleteAt(id, harnessAt.Add(-2*keep))
	n.seedDrops(id, 1, harnessAt.Add(-time.Hour), code.PurgePartRows+1)
	drops := func() int { return n.count(`SELECT COUNT(*) FROM code_ref_drops WHERE repo_id = ?`, id) }
	var steps []int
	for range 3 {
		left := drops()
		res, err := n.store.PurgeStep(t.Context(), id, keep)
		if err != nil {
			t.Fatalf("PurgeStep: %v", err)
		}
		steps = append(steps, left-drops())
		if res.Done {
			break
		}
	}
	if want := []int{code.PurgePartRows, 1}; !slices.Equal(steps, want) {
		t.Fatalf("the purge removed %v ref drops a record, want %v: a part of PurgePartRows (%d), "+
			"then the purge", steps, want, code.PurgePartRows)
	}
	if left := drops(); left != 0 {
		t.Fatalf("the purge left %d ref drops of the repository", left)
	}
	if _, found := n.repo("app"); found {
		t.Fatal("the repository still has a row after its purge")
	}
}
