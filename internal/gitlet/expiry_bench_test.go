package gitlet_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/store"
)

// BenchmarkExpiryListing times ONE PASS'S LISTING of the ref expiry (every
// page of code_refs_expiry_idx past the cutoff) over 100 000 idle seat
// branches of archived repositories, which every pass lists and proposes
// nothing for, beside 1 000 due ones of an active repository.
//
// THE DECISION RULE (M3-boxes §10.4): the listing scans the partial index from
// its oldest row, rereading the archived rows each pass. Past a tenth of
// gitlet.RetentionPassBudget (6 s) at this size, it moves to a
// per-active-repository seek on an index (repo_id, at, ref) partial on a set
// seat, in its own migration; under it, the one index serves. Measured with
// -benchtime 3x -count 1 on two E-cores; the figure is the commit's.
func BenchmarkExpiryListing(b *testing.B) {
	n := newNode(b)
	ctx := context.Background()
	seed := func(repos, refs int, status gitlet.RepoStatus) {
		if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
			for range repos {
				id := uuid.Must(uuid.NewV7()).String()
				if _, err := tx.ExecContext(ctx, `INSERT INTO code_repos (id, name, status, managed_by,
					owner_kind, owner, default_branch, created_at) VALUES (?, ?, ?, 'config', 'unit', 'ENG',
					'main', 0)`, id, "r-"+id, string(status)); err != nil {
					return err
				}
				if _, err := store.InsertRows(ctx, tx, n.db.Caps().MaxVariables,
					`INSERT INTO code_refs (repo_id, ref, target, position, at, seat) VALUES `,
					`(?, ?, ?, 1, 0, 'dev')`, ``, refs, func(i int) []any {
						return []any{id, fmt.Sprintf("refs/heads/seats/dev/b%06d", i), oidOf(fmt.Sprint(i))}
					}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatalf("seed %d %s repositories: %v", repos, status, err)
		}
	}
	seed(100, 1000, gitlet.RepoStatusArchived)
	seed(1, 1000, gitlet.RepoStatusActive)
	cutoff := time.Unix(0, 0).Add(time.Hour)
	for b.Loop() {
		pages, refs, err := gitlet.ExpiryListing(ctx, n.reader, cutoff)
		if err != nil || refs != 101_000 {
			b.Fatalf("the listing read %d refs in %d pages (%v), want 101 000", refs, pages, err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N), "ms/pass")
}
