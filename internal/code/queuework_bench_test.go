package code_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
)

// BenchmarkTheHomesQueueWork times ONE PAGE of a `queue_due` read — what a
// repository's home reads every turn of its queue worker, and a turn runs on
// every record of the repository's subject and its checks subject, so on
// every queued, claimed and finished check of a train — over a target holding
// [code.MaxCandidateEntries] live entries, each change's description at
// [code.MaxChangeBodyBytes], under the default gate (depth 4, batch 2) and the
// widest ([code.MaxQueueDepth] × [code.MaxQueueBatch]):
//
//   - build: no train stands, so the page names depth × batch entries WITH
//     their descriptions, which the build writes its commit messages from;
//   - test: a train of depth × batch entries stands on the tip, so the page
//     names them and their changes and no description — the turn every
//     check record of the train nudges.
//
// It reports the page's encoded answer as `answer-bytes` beside its time.
// Bounded: -benchtime 20x -count 1 on the runner's two E-cores; internal/code's
// package doc records the figures, and those of the read it replaced.
func BenchmarkTheHomesQueueWork(b *testing.B) {
	for _, gate := range []struct{ depth, batch int }{{4, 2}, {code.MaxQueueDepth, code.MaxQueueBatch}} {
		for _, shape := range []string{"build", "test"} {
			b.Run(fmt.Sprintf("D=%d/B=%d/%s", gate.depth, gate.batch, shape), func(b *testing.B) {
				n := newNode(b)
				repo := seedQueueWork(b, n, gate.depth, gate.batch, shape == "test")
				q := code.ReadQuery{Kind: code.ReadQueueDue, QueueDue: &code.QueueDueQuery{RepoID: repo}}
				var answer code.ReadAnswer
				for b.Loop() {
					var err error
					if answer, err = n.reader.Read(context.Background(), q); err != nil || answer.DueQueue == nil {
						b.Fatalf("the queue work reads %+v (%v), want main's", answer.DueQueue, err)
					}
				}
				d := answer.DueQueue
				if want := gate.depth * gate.batch; len(d.Entries) != want || d.Builds() != (shape == "build") {
					b.Fatalf("the page names %d entries (builds %t), want %d", len(d.Entries), d.Builds(), want)
				}
				encoded, err := json.Marshal(answer)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportMetric(float64(len(encoded)), "answer-bytes")
				b.ReportMetric(float64(b.Elapsed().Microseconds())/float64(b.N), "µs/read")
			})
		}
	}
}

// seedQueueWork writes, straight into n's estate, an active repository whose
// main is at A under a gate requiring unit at depth and batch, holding
// MaxCandidateEntries live entries of as many changes, each with a body of
// MaxChangeBodyBytes and one author — and, where standing, a train of its
// first depth × batch entries built onto A — and answers its id.
func seedQueueWork(b *testing.B, n *node, depth, batch int, standing bool) string {
	b.Helper()
	ctx := context.Background()
	repo := uuid.Must(uuid.NewV7()).String()
	gate, err := json.Marshal(code.MergeGate{MergeBy: code.MergeByWriters, Approvals: code.ApprovalsNone,
		Checks: []string{"unit"}, Landing: code.Landing{Depth: depth, Batch: batch},
		DeleteSource: code.DeleteSeats})
	if err != nil {
		b.Fatal(err)
	}
	body := strings.Repeat("b", code.MaxChangeBodyBytes)
	train := uuid.Must(uuid.NewV7()).String()
	var candidates [][]code.CandidateEntry
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		exec := func(query string, args ...any) error {
			_, err := tx.ExecContext(ctx, query, args...)
			return err
		}
		if err := exec(`INSERT INTO code_repos (id, name, status, managed_by, owner_kind, owner, default_branch,
			created_at) VALUES (?, 'web', 'active', 'config', 'unit', 'ENG', 'main', 0)`, repo); err != nil {
			return err
		}
		if err := exec(`INSERT INTO code_refs (repo_id, ref, target, position, at)
			VALUES (?, 'refs/heads/main', ?, 1, 0)`, repo, oidOf("A")); err != nil {
			return err
		}
		if err := exec(`INSERT INTO code_protections (repo_id, pattern, allow_force, allow_delete, direct_push,
			gate) VALUES (?, 'refs/heads/main', 0, 0, 'persons', ?)`, repo, string(gate)); err != nil {
			return err
		}
		for i := range code.MaxCandidateEntries {
			change := fmt.Sprintf("%s-change-%06d", repo, i)
			if err := exec(`INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref,
				target_ref, head_oid, revision, owner, owner_kind, opened_at, opened_position, version, position)
				VALUES (?, ?, ?, 'a change', ?, 'open', ?, 'refs/heads/main', ?, 1, 'seat', 'agent', 0, ?, 1, ?)`,
				change, repo, i+1, len(body), fmt.Sprintf("refs/heads/change-%06d", i), fmt.Sprintf("%040d", i),
				i+1, i+1); err != nil {
				return err
			}
			if err := exec(`INSERT INTO code_change_bodies (change_id, body) VALUES (?, ?)`, change,
				body); err != nil {
				return err
			}
			if err := exec(`INSERT INTO code_change_authors (change_id, author, author_kind, first_revision,
				position) VALUES (?, 'seat', 'agent', 1, 1)`, change); err != nil {
				return err
			}
			entry := uuid.Must(uuid.NewV7()).String()
			state, trainOf, candidate, commit := "waiting", "", "", ""
			if standing && i < depth*batch {
				ordinal := i/batch + 1
				state, trainOf, candidate = "testing", train, code.QueueCandidateID(train, ordinal)
				commit = oidOf(fmt.Sprint("built", i))
				if i%batch == 0 {
					candidates = append(candidates, nil)
				}
				candidates[ordinal-1] = append(candidates[ordinal-1], code.CandidateEntry{EntryID: entry,
					Commit: commit})
			}
			if err := exec(`INSERT INTO code_queue (repo_id, entry_id, target_ref, change_id, seq, head_oid,
				method, delete_source, close_task, enqueued_by, enqueued_kind, enqueued_operator, state, train,
				candidate_id, commit_oid, enqueued_at) VALUES (?, ?, 'refs/heads/main', ?, ?, ?, 'merge', 1, 0,
				'dev', 'agent', '', ?, ?, ?, ?, 1)`, repo, entry, change, i+1, fmt.Sprintf("%040d", i), state,
				trainOf, candidate, commit); err != nil {
				return err
			}
		}
		for i, entries := range candidates {
			list, err := json.Marshal(entries)
			if err != nil {
				return err
			}
			tip := entries[len(entries)-1].Commit
			if err := exec(`INSERT INTO code_candidates (repo_id, candidate_id, target_ref, train, ordinal,
				base_oid, tip_oid, state, position, entries) VALUES (?, ?, 'refs/heads/main', ?, ?, ?, ?,
				'testing', 1, ?)`, repo, code.QueueCandidateID(train, i+1), train, i+1, oidOf("A"), tip,
				string(list)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatalf("seed the queue work: %v", err)
	}
	return repo
}
