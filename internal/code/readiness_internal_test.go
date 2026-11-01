package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"testing"
)

// bindingTx is a transaction that counts the statements issued through it and
// keeps the arguments each query was last bound with.
type bindingTx struct {
	countingTx
	bound map[string][]any
}

func (b *bindingTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	b.bound[query] = args
	return b.countingTx.QueryContext(ctx, query, args...)
}

// A PAGE'S READINESS IS READ IN A FIXED NUMBER OF STATEMENTS: a listing judges
// the readiness of every live change of its page, so what each judges is read
// a kind of row at a time over the whole page — one change or forty, each
// holding standing blockers, cost the same statements, readinessStatements
// where one holds more blockers than a readiness names and one fewer where
// none does, where a statement a change for the blockers cost forty-six. And
// each change's blockers are its oldest raised, at most MaxReadinessBlockers
// of them in that order, however many stand: the change holding twenty-four,
// raised five, five, five, five and four a review, each review's in the
// reverse of their ids' order, names its first sixteen, and its read is cut
// at the position of the sixteenth — the fourth review's — so its fifth
// review is never read, while a change holding three is read uncut.
//
// Mutations: read each change's blockers with a statement of its own → forty
// changes issue forty-six statements; bind no change's cut → the crowded
// change is read uncut; leave the blockers past MaxReadinessBlockers on the
// answer → the crowded change names twenty; name them newest first → the
// order is wrong.
func TestAPageOfReadinessesIsReadInAFixedNumberOfStatements(t *testing.T) {
	t.Parallel()
	if want := strconv.Itoa(MaxReadinessBlockers - 1); lastBlockerOffset != want {
		t.Fatalf("the blockers' cut is read %s entries in, want MaxReadinessBlockers-1, %s", lastBlockerOffset,
			want)
	}
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"))
	const changes = 40
	e.seedChangeRows("repo-a", changes, 1, 0, 0)
	ctx := t.Context()
	// raise is a review of change raising n blockers numbered from first,
	// at position at, written with its counters as a record's apply moves
	// them.
	raise := func(tx *sql.Tx, change string, n, first int, at int64) error {
		r := reviewRaising(change, 1, n, nil)
		r.ReviewID = fmt.Sprintf("review-%s-%d", change, at)
		for j := range r.Findings {
			r.Findings[j].ID = fmt.Sprintf("%s-finding-%03d", change, first+n-1-j)
		}
		applied := applyContext{packed: at, maxVariables: e.db.Caps().MaxVariables}
		_, counts, err := applyReview(ctx, tx, applied, r)
		if err != nil {
			return err
		}
		return touchConversation(ctx, tx, applied, "repo-a", change, counts)
	}
	const crowded = "repo-a-change-000000"
	var subjects []readinessSubject
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		first := 0
		for k, n := range []int{5, 5, 5, 5, 4} {
			if err := raise(tx, crowded, n, first, int64(101+k)); err != nil {
				return err
			}
			first += n
		}
		for i := 1; i < changes; i++ {
			if err := raise(tx, fmt.Sprintf("repo-a-change-%06d", i), 1+i%3, 0, int64(i)); err != nil {
				return err
			}
		}
		for i := range changes {
			row, found, err := changeRowIn(ctx, tx, fmt.Sprintf("repo-a-change-%06d", i))
			if err != nil || !found {
				return fmt.Errorf("read change %d: found %v, %w", i, found, err)
			}
			subjects = append(subjects, readinessSubject{repo: RepoRef{ID: "repo-a", Name: "a"}, row: row})
		}
		return nil
	}); err != nil {
		t.Fatalf("raise the changes' blockers: %v", err)
	}
	read := func(subjects []readinessSubject) ([]ReadinessRows, *bindingTx) {
		t.Helper()
		c := &bindingTx{bound: map[string][]any{}}
		var rows []ReadinessRows
		if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
			c.tx = tx
			var err error
			rows, err = readinessRowsIn(ctx, c, subjects)
			return err
		}); err != nil {
			t.Fatalf("read the readiness rows of %d changes: %v", len(subjects), err)
		}
		return rows, c
	}
	for _, tc := range []struct {
		what     string
		subjects []readinessSubject
		want     int
	}{
		{"the crowded change alone", subjects[:1], readinessStatements},
		{"forty changes, one crowded", subjects, readinessStatements},
		{"thirty-nine changes, none crowded", subjects[1:], readinessStatements - 1},
	} {
		if _, c := read(tc.subjects); c.n != tc.want {
			t.Errorf("the readiness of %s issued %d statements, want %d whatever the page holds", tc.what, c.n,
				tc.want)
		}
	}

	rows, c := read(subjects)
	var bounds [][2]any
	if args := c.bound[standingBlockersSQL]; len(args) != 1 {
		t.Fatalf("the standing blockers were read with %v, want one list of bounds", args)
	} else if err := json.Unmarshal([]byte(args[0].(string)), &bounds); err != nil {
		t.Fatalf("decode the blockers' bounds %v: %v", args[0], err)
	}
	cuts := map[string]float64{}
	for _, b := range bounds {
		cuts[b[0].(string)] = b[1].(float64)
	}
	if cut := cuts[crowded]; cut != 104 {
		t.Errorf("the crowded change's blockers are read up to position %v, want 104: its sixteenth oldest "+
			"was raised by its fourth review", cut)
	}
	if cut := cuts["repo-a-change-000001"]; cut != math.MaxInt64 {
		t.Errorf("a change holding two blockers is read up to position %v, want uncut", cut)
	}
	var named []string
	for _, b := range rows[0].Blockers {
		named = append(named, b.FindingID)
	}
	var want []string
	for j := range MaxReadinessBlockers {
		want = append(want, fmt.Sprintf("%s-finding-%03d", crowded, j))
	}
	if rows[0].Standing != 24 || !slices.Equal(named, want) {
		t.Errorf("the change holding 24 blockers names %q of %d standing, want its oldest %d in order: %q",
			named, rows[0].Standing, MaxReadinessBlockers, want)
	}
	for i, r := range rows[1:] {
		if want := 1 + (i+1)%3; len(r.Blockers) != want || r.Standing != int64(want) {
			t.Errorf("change %d names %d of %d standing blockers, want all %d", i+1, len(r.Blockers), r.Standing,
				want)
		}
	}
}
