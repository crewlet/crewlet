package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
)

// candidateWords answers every candidate a page at the bound reads.
type candidateWords struct{ held search.ChangeHeld }

func (w candidateWords) Holding(context.Context, search.ChangeQuery) (search.ChangeHeld, error) {
	return w.held, nil
}

// BenchmarkAWordsPageAtItsCandidateBound reads the first page of a listing its words lead at the
// shipped bounds: MaxChangeTextCandidates candidates and MaxChangeTextTail changes moved past the
// watermark, every one trusted where covered, or every one's 2 KiB description read where not.
func BenchmarkAWordsPageAtItsCandidateBound(b *testing.B) {
	e := openApplyEstate(b)
	e.apply(create(b, "repo-a", "web"))
	total := MaxChangeTextCandidates + MaxChangeTextTail
	// Change i opened and moved at position i + 1, its title holding "fix" and its description "login".
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref, target_ref,
			head_oid, revision, owner, owner_kind, opened_at, opened_position, version, position, updated_at)
		SELECT printf('repo-a-change-%06d', i), 'repo-a', i + 1, 'fix the retry', 2048, 'merged',
			printf('refs/heads/change-%06d', i), 'refs/heads/main', printf('%040d', i), 1, 'seat', 'agent', 0,
			i + 1, 1, i + 1, i + 1 FROM k`, total)
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_change_bodies (change_id, body)
		SELECT printf('repo-a-change-%06d', i), 'login ' || printf('%.2042c', 'x') FROM k`, total)
	var instance string
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) (err error) {
		instance, err = statelog.EstateInstance(context.Background(), tx)
		return err
	}); err != nil {
		b.Fatal(err)
	}
	ids := make([]string, MaxChangeTextCandidates)
	for i := range ids {
		ids[i] = fmt.Sprintf("repo-a-change-%06d", MaxChangeTextCandidates-1-i)
	}
	q := ReadQuery{Kind: ReadChanges, Asker: Principal{Kind: PrincipalOperator, OperatorID: "pat:ada", Seat: "founder"},
		State: ChangeStateMerged, Text: "fix login"}
	for _, arm := range []struct {
		name string
		seen int64
	}{{"covered", 0}, {"judged", 1 << 62}} {
		r := &Reader{limits: DefaultLimits, now: time.Now, pageRows: MaxSnapshotItems, pageBytes: MaxPageBytes,
			textCandidates: MaxChangeTextCandidates, textTail: MaxChangeTextTail,
			words: candidateWords{search.ChangeHeld{IDs: ids, Through: int64(MaxChangeTextCandidates),
				Seen: arm.seen, BuiltFrom: instance}}}
		b.Run(arm.name, func(b *testing.B) {
			for b.Loop() {
				var out ReadAnswer
				if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
					return r.changesByWords(context.Background(), tx, q, nil, statelog.Position{}, &out)
				}); err != nil {
					b.Fatal(err)
				}
				if len(out.Changes) != defaultChangeLimit || out.Next == "" {
					b.Fatalf("the page listed %d changes, next %q", len(out.Changes), out.Next)
				}
			}
		})
	}
}
