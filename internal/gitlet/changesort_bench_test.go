package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// BenchmarkALiveListingByUpdate is the walk of the open changes by last move, a first page and
// every page to the end at the shipped bound, over 500 open changes and 50 000 merged ones, in one
// repository and across 20, every merged change's conversation spoken after the open ones last
// moved (the bound steps over them) or before (the floor never reads them).
func BenchmarkALiveListingByUpdate(b *testing.B) {
	const open, merged = 500, 50_000
	for _, repos := range []int{1, 20} {
		for _, spoken := range []string{"after", "before"} {
			e := openApplyEstate(b)
			ids := make([]string, repos)
			for r := range repos {
				ids[r] = fmt.Sprintf("repo-%02d", r)
				e.apply(create(b, ids[r], fmt.Sprintf("r%02d", r)))
			}
			// THE OPEN CHANGES moved at 1 000 000 and on, the merged ones spoken of after them at
			// 2 000 000 and on, or before them at 1 and on; change i is of repository i % repos.
			spokenFrom := 2_000_000
			if spoken == "before" {
				spokenFrom = 1
			}
			for r, id := range ids {
				e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
					INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref,
						target_ref, head_oid, revision, owner, owner_kind, opened_at, opened_position, version,
						position, updated_at)
					SELECT printf('%s-change-%06d', ?, i), ?, i + 1, 'a change', 0,
						CASE WHEN i < ? THEN 'open' ELSE 'merged' END, printf('refs/heads/change-%06d', i),
						'refs/heads/main', printf('%040d', i), 1, 'seat', 'agent', 0, i + 1, 1, i + 1,
						CASE WHEN i < ? THEN 1000000 + i * ? + ? ELSE 500 END FROM k`,
					(open+merged)/repos, id, id, open/repos, open/repos, repos, r)
				e.exec(`WITH RECURSIVE k(i) AS (SELECT ? UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
					INSERT INTO code_conversations (change_id, repo_id, comments, text_bytes, version, position,
						updated_at)
					SELECT printf('%s-change-%06d', ?, i), ?, 1, 10, 1, 1, ? + i * ? + ? FROM k`,
					open/repos, (open+merged)/repos, id, id, spokenFrom, repos, r)
			}
			for _, scope := range []string{"company", "repository"} {
				q := ReadQuery{Kind: ReadChanges, State: ChangeStateOpen, ChangeSort: ChangeSortUpdated}
				repoID := ""
				if scope == "repository" {
					repoID = ids[0]
				}
				w := walkLedBy(q, repoID)
				w.scan = MaxSpokenScanned
				name := fmt.Sprintf("%d repos/spoken %s/%s", repos, spoken, scope)
				b.Run(name+"/first page", func(b *testing.B) {
					for b.Loop() {
						listLive(b, e, w, false)
					}
				})
				b.Run(name+"/every page", func(b *testing.B) {
					for b.Loop() {
						listLive(b, e, w, true)
					}
				})
			}
		}
	}
}

// listLive reads w's first page, or every page to the end, in one transaction of e's.
func listLive(b *testing.B, e *estate, w changeWalk, every bool) {
	b.Helper()
	if err := e.db.Tx(context.Background(), func(tx *sql.Tx) (err error) {
		cursor := ""
		for {
			if _, cursor, err = w.read(context.Background(), tx, cursor, defaultChangeLimit); err != nil {
				return err
			}
			if cursor == "" || !every {
				return nil
			}
		}
	}); err != nil {
		b.Fatalf("list the open changes by last move: %v", err)
	}
}
