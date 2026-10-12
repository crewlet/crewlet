package code_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/store"
)

// BenchmarkAnUnchangedActivation is the reconcile of an activation that
// changes nothing, on the round-trip harness's real log and estate: ten
// thousand declared repositories — the most a configuration may declare — as
// an earlier activation left them, reconciled again by EnsureRepos, the
// reconcile's form; and EnsureRepo a repository at a time over a thousand of
// them, the per-repository cost the batch replaced (two publishes each, the
// claim and the settle, every one its own eviction read, session wait and
// snapshot). doc.go's measured section records both.
func BenchmarkAnUnchangedActivation(b *testing.B) {
	const repos = 10_000
	n := newNode(b)
	earlier := n.activate(t1)
	declared := seedDeclared(b, n, repos, earlier.Epoch())
	a := n.activate(t2)
	n.drain()

	b.Run(fmt.Sprintf("ensure-repos/%d", repos), func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got, err := n.store.EnsureRepos(b.Context(), a, declared)
			if err != nil {
				b.Fatal(err)
			}
			for i, r := range got {
				if r.Err != nil || r.Changed {
					b.Fatalf("declaration %d answered %+v, want unchanged", i, r)
				}
			}
		}
		b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N)/repos, "µs/repo")
	})
	b.Run("ensure-repo/1000", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, d := range declared[:1000] {
				if got, err := n.store.EnsureRepo(b.Context(), a, d); err != nil || got.Changed {
					b.Fatalf("EnsureRepo(%s) = %+v, %v, want unchanged", d.Name, got, err)
				}
			}
		}
		b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N)/1000, "µs/repo")
	})
}

// seedDeclared writes count repositories straight into n's estate as the
// activation epoch left them — each its name claim, its row and the suite's
// one rule — and answers their declarations: ten thousand creates through the
// log would take minutes and measure nothing the benchmark is about.
func seedDeclared(b *testing.B, n *node, count int, epoch int64) []code.Declared {
	b.Helper()
	declared := make([]code.Declared, count)
	ids := make([]string, count)
	for i := range declared {
		name := fmt.Sprintf("repo-%05d", i)
		declared[i] = code.Declared{Name: name, Settings: described(name)}
		ids[i] = uuid.Must(uuid.NewV7()).String()
	}
	ctx := b.Context()
	vars := n.db.Caps().MaxVariables
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_names (token, name, repo_id) VALUES `, `(?, ?, ?)`, ``, count,
			func(i int) []any { return []any{code.NameToken(declared[i].Name), declared[i].Name, ids[i]} }); err != nil {
			return err
		}
		if _, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_repos (id, name, status, managed_by, owner_kind, owner,
				default_branch, description, config_epoch, created_at, version) VALUES `,
			`(?, ?, 'active', 'config', 'unit', 'ENG', 'main', ?, ?, 0, 1)`, ``, count,
			func(i int) []any { return []any{ids[i], declared[i].Name, declared[i].Name, epoch} }); err != nil {
			return err
		}
		_, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_protections (repo_id, pattern, allow_force, allow_delete,
				direct_push) VALUES `, `(?, 'refs/heads/main', 0, 0, 'persons')`, ``, count,
			func(i int) []any { return []any{ids[i]} })
		return err
	}); err != nil {
		b.Fatalf("seed %d repositories: %v", count, err)
	}
	return declared
}
