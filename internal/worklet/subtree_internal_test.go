package worklet

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"
)

// A SUBTREE READ ENDS AT ITS OWN ROOT, WHATEVER SHAPE THE ROWS ARE IN.
//
// The applier applies a parent cycle rather than stall the log over it (two
// re-parents on two subjects form one no single write sees), so the rows the
// subtree walk follows can loop — and a walk down the parent pointers that
// can step back onto its root never ends. Measured: [readSubtree] on a member
// of a two-task cycle did not return, past its context's deadline, and the
// move, removal or restore that asked for it held its transaction with it.
//
// The rows are written directly in the shapes the applier leaves: a two-task
// cycle with a tree hanging from it, a task that is its own parent, and an
// ordinary tree beside them, which must read exactly as it always has.
func TestASubtreeReadEndsAtItsOwnRoot(t *testing.T) {
	t.Parallel()
	parents := map[string]string{
		// The cycle, and a tree hanging from it.
		"t-0001": "t-0002", "t-0002": "t-0001",
		"t-0003": "t-0002", "t-0004": "t-0003",
		// A task left as its own parent.
		"t-0005": "t-0005",
		// An ordinary tree.
		"t-0007": "t-0006", "t-0008": "t-0006", "t-0009": "t-0008",
	}
	db := seededPlanStore(t, func(ctx context.Context, tx *sql.Tx, maxVariables int) error {
		if err := insertAll(ctx, tx, maxVariables, insertTask, taskValues,
			10, taskRow); err != nil {
			return err
		}
		// A DOCUMENT NAMING ITS TASK, which is what the walk decodes.
		if _, err := tx.ExecContext(ctx,
			`UPDATE tracker_tasks SET document = json_object('id', id)`); err != nil {
			return err
		}
		for child, parent := range parents {
			if _, err := tx.ExecContext(ctx,
				`UPDATE tracker_tasks SET parent_id = ? WHERE id = ?`,
				parent, child); err != nil {
				return err
			}
		}
		return nil
	})
	// IN THE WALK'S ORDER, depth and then id, every task once.
	for root, want := range map[string][]string{
		"t-0001": {"t-0002", "t-0003", "t-0004"},
		"t-0002": {"t-0001", "t-0003", "t-0004"},
		"t-0003": {"t-0004"},
		"t-0005": nil,
		"t-0006": {"t-0007", "t-0008", "t-0009"},
	} {
		t.Run(root, func(t *testing.T) {
			t.Parallel()
			type answer struct {
				got []string
				err error
			}
			// IN A GOROUTINE OF ITS OWN, because the failure this guards
			// against does not return at all and ignores its context.
			done := make(chan answer, 1)
			go func() {
				var a answer
				a.err = db.Read(context.Background(), func(tx *sql.Tx) error {
					subtree, err := readSubtree(context.Background(), tx, root)
					for _, task := range subtree {
						a.got = append(a.got, task.ID)
					}
					return err
				})
				done <- a
			}()
			select {
			case a := <-done:
				if a.err != nil {
					t.Fatalf("readSubtree(%s): %v", root, a.err)
				}
				if !slices.Equal(a.got, want) {
					t.Errorf("the subtree under %s is %v, want %v", root, a.got, want)
				}
			case <-time.After(time.Minute):
				t.Fatalf("readSubtree(%s) has not returned in a minute: the walk "+
					"is following a cycle back through its own root", root)
			}
		})
	}
}
