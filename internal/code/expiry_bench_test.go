package code_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/store"
)

// BenchmarkExpiryListing times ONE PASS'S LISTING of the ref expiry — every
// page of code_refs_expiry_idx past the cutoff, each with its repositories and
// its position — over 100 000 idle seat branches of archived repositories
// (a hundred repositories of a thousand each: listed every pass and proposed
// nothing, until a purge or a restore) beside 1 000 due ones of an active
// repository, all stamped at the epoch.
//
// THE DECISION RULE (M3-boxes §10.4): the listing is a scan of the partial
// index from its oldest row, so the archived rows are read again every pass.
// Past a tenth of code.RetentionPassBudget (6 s) at this size, the listing
// moves to a per-active-repository seek — an index (repo_id, at, ref) partial
// on a set seat, walked repository by repository from the active ones — in a
// migration of its own; under it, the one index serves. Measured with
// -benchtime 3x -count 1 on the runner's two E-cores; the figure and the
// decision are the commit's.
func BenchmarkExpiryListing(b *testing.B) {
	n := newNode(b)
	ctx := context.Background()
	seed := func(repos, refs int, status code.RepoStatus) {
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
	seed(100, 1000, code.RepoStatusArchived)
	seed(1, 1000, code.RepoStatusActive)
	cutoff := time.Unix(0, 0).Add(time.Hour)
	for b.Loop() {
		pages, refs, err := code.ExpiryListing(ctx, n.reader, cutoff)
		if err != nil || refs != 101_000 {
			b.Fatalf("the listing read %d refs in %d pages (%v), want 101 000", refs, pages, err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N), "ms/pass")
}
