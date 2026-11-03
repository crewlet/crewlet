package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// countingTx is a transaction that counts the statements issued through it.
type countingTx struct {
	tx *sql.Tx
	n  int
}

func (c *countingTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.n++
	return c.tx.ExecContext(ctx, query, args...)
}

func (c *countingTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.n++
	return c.tx.QueryContext(ctx, query, args...)
}

func (c *countingTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	c.n++
	return c.tx.QueryRowContext(ctx, query, args...)
}

// reviewRaising is a review of change at revision by seat-r asking for
// changes, raising n blocker findings, each with a title and a body, and
// carrying carries.
func reviewRaising(change string, revision int64, n int, carries []string) Review {
	r := Review{RepoID: "repo-a", ChangeID: change, ReviewID: fmt.Sprintf("review-%d", revision),
		Revision: revision, Verdict: VerdictRequestChanges, By: Principal{Kind: PrincipalAgent, Seat: "seat-r"},
		Summary: "The retry loop.", Carries: carries}
	for i := range n {
		r.Findings = append(r.Findings, Finding{ID: fmt.Sprintf("finding-%d-%03d", revision, i), Path: "a.go",
			Side: SideNew, LineStart: i + 1, LineEnd: i + 1, CommitOID: oid("c"), BlobOID: oid("b"),
			Severity: SeverityBlocker, Category: CategoryCorrectness, Title: "A retry that never ends",
			Body: "The loop retries without a bound."})
	}
	return r
}

// reviewApplyStatements is how many statements a review's apply issues
// whatever it raises and supersedes: its reviewer's standing verdict, the
// review, its reviewer's standing findings, their supersession, its findings,
// their texts, its reviewer as a participant and the ask it answers.
const reviewApplyStatements = 8

// THE WIDEST REVIEW APPLIES IN A FIXED NUMBER OF STATEMENTS: a review's
// findings and their texts are each one collection, written through as few
// multi-row statements as the engine's bind limit allows, and the findings it
// supersedes one statement driven from their list — so a review raising
// MaxFindingsPerReview findings and superseding one, and one raising one and
// superseding MaxFindingsPerReview, each issue reviewApplyStatements, where a
// statement a row issued two a finding. This apply runs in the code domain's
// one applier on every data node, which the pushes queue behind.
//
// Mutation: write the findings, or their texts, a statement a row → the
// widest review issues 63 more.
func TestTheWidestReviewAppliesInAFixedNumberOfStatements(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	e.seedChangeRows("repo-a", 1, 3, 1, 0)
	const change = "repo-a-change-000000"
	statements := func(r Review) int {
		t.Helper()
		var c countingTx
		if err := e.db.Tx(t.Context(), func(tx *sql.Tx) error {
			c.tx = tx
			_, _, err := applyReview(t.Context(), &c, applyContext{packed: r.Revision,
				maxVariables: e.db.Caps().MaxVariables}, r)
			return err
		}); err != nil {
			t.Fatalf("apply review %s: %v", r.ReviewID, err)
		}
		return c.n
	}
	statements(reviewRaising(change, 1, 1, nil))
	for _, tc := range []struct {
		what string
		r    Review
	}{
		{"raising the most a review raises and superseding one", reviewRaising(change, 2, MaxFindingsPerReview, nil)},
		{"raising one and superseding the most a review raises", reviewRaising(change, 3, 1, nil)},
	} {
		if got := statements(tc.r); got != reviewApplyStatements {
			t.Errorf("a review %s issued %d statements, want %d whatever it raises and supersedes", tc.what, got,
				reviewApplyStatements)
		}
	}
	if got := e.num(`SELECT COUNT(*) FROM code_findings WHERE change_id = ? AND state = 'open'`, change); got != 1 {
		t.Fatalf("the change holds %d open findings once the last review superseded the rest, want its one", got)
	}
	if got := e.num(`SELECT COUNT(*) FROM code_finding_texts WHERE change_id = ?`, change); got !=
		int64(2+MaxFindingsPerReview) {
		t.Fatalf("the change holds %d findings' texts, want every finding's, %d", got, 2+MaxFindingsPerReview)
	}
}

// errRolledBack ends a benchmark's transaction undone, so its next round reads
// the rows its first did.
var errRolledBack = errors.New("rolled back")

// BenchmarkTheWidestReview is the widest review's decide and apply in one
// transaction, rolled back: its reviewer holding MaxStandingFindings standing
// findings on the change, of which it carries half and supersedes the rest,
// raising MaxFindingsPerReview more — what MaxStandingFindings's doc reads the
// cost of its bound from. Run it bounded:
// -run '^$' -bench '^BenchmarkTheWidestReview$' -benchtime 20x -count 1.
func BenchmarkTheWidestReview(b *testing.B) {
	e := openApplyEstate(b)
	e.apply(create(b, "repo-a", "a"))
	e.seedChangeRows("repo-a", 1, 2, 1, 0)
	const change = "repo-a-change-000000"
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_findings (id, change_id, review_id, raiser, raiser_kind, revision, commit_oid, path,
			blob_oid, side, line_start, line_end, severity, category, state, position)
		SELECT printf('standing-%04d', i), ?, 'review-0', 'seat-r', 'agent', 1, ?, 'a.go', ?, 'new', i + 1, i + 1,
			'blocker', 'correctness', 'open', 1 FROM k`, MaxStandingFindings, change, oid("c"), oid("b"))
	carries := make([]string, MaxStandingFindings/2)
	for i := range carries {
		carries[i] = fmt.Sprintf("standing-%04d", 2*i)
	}
	asker := Principal{Kind: PrincipalAgent, Seat: "seat-r"}
	r := reviewRaising(change, 2, MaxFindingsPerReview, carries)
	w := ConversationWrite{OpID: statelog.NewOpID(time.Now(), "code_review_change"), Asker: asker, Review: &r}
	grant := Grant{Principal: asker, RepoID: "repo-a", RepoName: "a", Access: AccessReview}
	at := applyContext{packed: 2, maxVariables: e.db.Caps().MaxVariables}
	ctx := b.Context()
	for b.Loop() {
		err := e.db.Tx(ctx, func(tx *sql.Tx) error {
			row, found, err := changeRowIn(ctx, tx, change)
			if err != nil || !found {
				return fmt.Errorf("read the change: found %v, %w", found, err)
			}
			d, err := decideReview(ctx, tx, w, "seat-r", PartyAgent, row, "a!1", grant, conversationDecision{})
			switch {
			case err != nil:
				return err
			case d.refusal != "":
				return fmt.Errorf("the widest review was refused %s: %s", d.refusal, d.detail)
			}
			if _, _, err := applyReview(ctx, tx, at, d.payload.(Review)); err != nil {
				return err
			}
			return errRolledBack
		})
		if !errors.Is(err, errRolledBack) {
			b.Fatalf("decide and apply the widest review: %v", err)
		}
	}
}
