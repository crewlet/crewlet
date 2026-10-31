package code_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/store"
)

// THE RECONCILE'S WALK READS EVERY REPOSITORY IT ACTS ON, ONCE, IN NAME ORDER,
// across pages — importing, active, archived and deleted, whoever manages them
// — and no purge, which is the code-retention duty's: two and a half pages of rows,
// seeded in every status and two managers, come back whole, each with the
// status and manager its row holds. A visit's error ends the walk with it.
//
// Mutation: resume a page at its last name inclusively and a row repeats;
// drop deleted from the statuses and a restore is reported as a create; drop
// importing and a repository code.repos stops declaring while its import runs
// is never deleted.
func TestTheReconcilesWalkReadsEveryRepositoryItActsOn(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	statuses := []code.RepoStatus{code.RepoStatusImporting, code.RepoStatusActive,
		code.RepoStatusArchived, code.RepoStatusDeleted, code.RepoStatusPurging}
	const count = 2_500
	want := map[string]code.RepoLife{}
	ctx := context.Background()
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, n.db.Caps().MaxVariables,
			`INSERT INTO code_repos (id, name, status, managed_by, owner_kind, owner,
				default_branch, created_at) VALUES `,
			`(?, ?, ?, ?, 'unit', 'ENG', 'main', 0)`, ``, count, func(i int) []any {
				l := code.RepoLife{ID: uuid.Must(uuid.NewV7()).String(),
					Name: fmt.Sprintf("walk-%05d", i), Status: statuses[i%len(statuses)],
					ManagedBy: code.ManagedConfig}
				if i%7 == 0 {
					l.ManagedBy = code.ManagedBy("tools")
				}
				if l.Status != code.RepoStatusPurging {
					want[l.Name] = l
				}
				return []any{l.ID, l.Name, string(l.Status), string(l.ManagedBy)}
			})
		return err
	}); err != nil {
		t.Fatalf("seed %d repositories: %v", count, err)
	}

	var got []code.RepoLife
	if err := n.reader.WalkLives(t.Context(), func(l code.RepoLife) error {
		got = append(got, l)
		return nil
	}); err != nil {
		t.Fatalf("WalkLives: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("the walk read %d repositories, want the %d a reconcile acts on", len(got), len(want))
	}
	for i, l := range got {
		if i > 0 && got[i-1].Name >= l.Name {
			t.Fatalf("the walk read %s after %s: not once each in name order", l.Name, got[i-1].Name)
		}
		if l != want[l.Name] {
			t.Fatalf("the walk read %+v, want the row's own %+v", l, want[l.Name])
		}
	}

	stop := errors.New("enough")
	visited := 0
	if err := n.reader.WalkLives(t.Context(), func(code.RepoLife) error {
		visited++
		if visited == 3 {
			return stop
		}
		return nil
	}); !errors.Is(err, stop) || visited != 3 {
		t.Fatalf("a visit's error ended the walk with %v after %d visits, want it at the third",
			err, visited)
	}
}
