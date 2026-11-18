package gitlet_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/store"
)

// BenchmarkQueueSweepListing times ONE PASS'S LISTING of the queue sweep, every
// page of queueSweepDueSQL, over config.CodeMaxRepos repositories with NOTHING
// DUE: each holds an ended entry and two protected branches' green rows with a
// landing below each, all inside the history, so the walk makes every seek and
// lists none, what a fleet of working queues pays every RetentionInterval.
//
// The decision rule is the ref expiry listing's (BenchmarkExpiryListing): past
// a tenth of gitlet.RetentionPassBudget (6 s) at this size, the listing moves
// to seeks of the due rows by instant, an index (ended_at, repo_id) partial on
// the ended states and the landings below green marked to be indexed. Run with
// -benchtime 3x -count 1; the figure and decision are in queueSweepDue.
func BenchmarkQueueSweepListing(b *testing.B) {
	n := newNode(b)
	ctx := context.Background()
	recent := store.EncodeTime(harnessAt.Add(-time.Hour))
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		for range config.CodeMaxRepos {
			id := uuid.Must(uuid.NewV7()).String()
			if _, err := tx.ExecContext(ctx, `INSERT INTO code_repos (id, name, status, managed_by, owner_kind,
				owner, default_branch, created_at) VALUES (?, ?, 'active', 'config', 'unit', 'ENG', 'main', 0)`,
				id, "r-"+id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO code_queue (repo_id, entry_id, target_ref, change_id,
				seq, head_oid, method, delete_source, close_task, enqueued_by, enqueued_kind,
				enqueued_operator, state, enqueued_at, ended_at, ended_position, end_reason)
				VALUES (?, ?, 'refs/heads/main', ?, 1, ?, 'merge', 0, 0, 'dev', 'agent', '', 'landed', ?, ?, 2,
					'')`, id, uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String(), oidOf("B"),
				recent, recent); err != nil {
				return err
			}
			for _, target := range []string{"refs/heads/main", "refs/heads/release"} {
				if _, err := tx.ExecContext(ctx, `INSERT INTO code_landings (repo_id, target_ref, position,
					ordinal, kind, base_oid, commit_oid, merged_by, at)
					VALUES (?, ?, 500, 1, 'push', ?, ?, 'founder', ?)`,
					id, target, oidOf("A"), oidOf("B"), recent); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO code_green (repo_id, target_ref, commit_oid, position,
					at) VALUES (?, ?, ?, 1000, ?)`, id, target, oidOf("B"), recent); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		b.Fatalf("seed %d repositories: %v", config.CodeMaxRepos, err)
	}
	cutoff := harnessAt.Add(-time.Hour * 24 * 31)
	for b.Loop() {
		pages, repos, err := gitlet.QueueSweepListing(ctx, n.reader, cutoff)
		if err != nil || repos != 0 {
			b.Fatalf("the listing named %d repositories in %d pages (%v), want none", repos, pages, err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N), "ms/pass")
}
