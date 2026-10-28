package code

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// THE LISTINGS SEEK THEIR INDEXES: every combination of the four filters a
// listing of changes may name walks the index its filters lead — a work
// item's; an owner's in a state; a repository's in a state; a repository's;
// the company's in a state — and every filter the listing names is either
// sought by that index or tested on each row the page read: none is dropped,
// which would list more than its caller asked for and say nothing. A listing
// naming no repository, work item or state is refused, since no index holds
// every change the company has.
//
// And each walk READS ITS INDEX: a page of it is exactly the rows its index
// holds for the walk's keys, newest first in the index's order, and a page
// after its own cursor the rows that follow — on the plan estate, against the
// rows read whole. TestEveryCodeIndexServesARegisteredQuery holds each
// statement to its seek and to reading in its index's order, sorting nothing;
// this holds which statements a walk issues, which no plan of one statement
// sees.
//
// Mutations: walk a repository's changes in one state by the number index →
// the walk names another index than its filters lead; drop the owner test
// from a repository's walk → the owner is neither sought nor tested; hand a
// walk another's statements (an owner's walk the state's) → its page holds
// rows of other owners.
func TestTheListingsSeekTheirIndexes(t *testing.T) {
	t.Parallel()
	db := seededPlanEstate(t)
	person := Principal{Kind: PrincipalOperator, OperatorID: "pat:ada", Seat: "founder"}
	// seeks is which of a listing's filters each index seeks.
	seeks := map[string]struct{ repo, state, owner, work bool }{
		changesWorkKey:      {work: true},
		changesOwnerKey:     {state: true, owner: true},
		changesRepoStateKey: {repo: true, state: true},
		changesNumberKey:    {repo: true},
		changesStateKey:     {state: true},
	}
	walks := map[string]changeWalk{}
	for _, repo := range []string{"", "repo-1"} {
		for _, state := range []ChangeState{"", ChangeStateOpen} {
			for _, owner := range []string{"", "seat-1"} {
				for _, work := range []string{"", "native:work-3"} {
					q := ReadQuery{Kind: ReadChanges, Asker: person, Repo: repo, State: state, Owner: owner,
						Work: work}
					name := fmt.Sprintf("repo %q, state %q, owner %q, work %q", repo, state, owner, work)
					err := q.validate()
					if repo == "" && state == "" && work == "" {
						if !errors.Is(err, ErrInvalid) {
							t.Errorf("%s: validate = %v, want it refused", name, err)
						}
						continue
					}
					if err != nil {
						t.Errorf("%s: validate = %v", name, err)
						continue
					}
					w := walkOf(q, repo)
					var want string
					switch {
					case work != "":
						want = changesWorkKey
					case owner != "" && state != "":
						want = changesOwnerKey
					case repo != "" && state != "":
						want = changesRepoStateKey
					case repo != "":
						want = changesNumberKey
					default:
						want = changesStateKey
					}
					if w.index != want {
						t.Errorf("%s walks %s, want %s", name, w.index, want)
						continue
					}
					walks[w.index] = w
					sought := seeks[w.index]
					for filter, f := range map[string]struct{ named, sought, tested bool }{
						"repo":  {repo != "", sought.repo, w.repo},
						"state": {state != "", sought.state, w.state},
						"owner": {owner != "", sought.owner, w.owner},
						"work":  {work != "", sought.work, false},
					} {
						switch {
						case f.named && !f.sought && !f.tested:
							t.Errorf("%s: its %s is neither sought by %s nor tested on a row", name, filter, w.index)
						case f.tested && !f.named:
							t.Errorf("%s: its walk tests a %s it does not name", name, filter)
						case f.tested && f.sought:
							t.Errorf("%s: its walk tests a %s its index seeks already", name, filter)
						}
					}
				}
			}
		}
	}

	// EACH WALK READS ITS INDEX, a page and the page after its cursor.
	var every []changeRow
	if err := db.Read(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(), `SELECT `+changeColumns+` FROM code_changes`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			row, scanErr := scanChange(rows.Scan)
			if scanErr != nil {
				return scanErr
			}
			every = append(every, row)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the plan estate's changes whole: %v", err)
	}
	for index, w := range walks {
		// held is every change the walk's index holds for its keys, in the
		// index's order, newest first.
		var held []changeRow
		for _, row := range every {
			var match bool
			switch index {
			case changesWorkKey:
				match = row.work.ID != "" && string(row.work.Backend) == w.keys[0] && row.work.ID == w.keys[1]
			case changesOwnerKey:
				match = row.owner == w.keys[0] && string(row.state) == w.keys[1]
			case changesRepoStateKey:
				match = row.repoID == w.keys[0] && string(row.state) == w.keys[1]
			case changesNumberKey:
				match = row.repoID == w.keys[0]
			case changesStateKey:
				match = string(row.state) == w.keys[0]
			}
			if match {
				held = append(held, row)
			}
		}
		slices.SortFunc(held, func(a, b changeRow) int {
			if w.order == byNumber {
				return cmp.Compare(b.number, a.number)
			}
			return cmp.Or(cmp.Compare(b.opened, a.opened), cmp.Compare(b.id, a.id))
		})
		if len(held) == 0 {
			t.Fatalf("the plan estate holds no change %s's walk reads (%v)", index, w.keys)
		}
		ids := func(rows []changeRow) []string {
			var out []string
			for _, r := range rows {
				out = append(out, r.id)
			}
			return out
		}
		const limit = 3
		var first, next []changeRow
		if err := db.Read(context.Background(), func(tx *sql.Tx) error {
			var err error
			if first, err = w.read(context.Background(), tx, "", limit); err != nil || len(first) == 0 {
				return err
			}
			next, err = w.read(context.Background(), tx, w.cursor(first[len(first)-1]), limit)
			return err
		}); err != nil {
			t.Fatalf("%s's walk: %v", index, err)
		}
		if want := ids(held[:min(limit, len(held))]); !slices.Equal(ids(first), want) {
			t.Errorf("%s's walk reads %v as its first page, and its index holds %v first", index, ids(first), want)
		}
		if want := ids(held[min(limit, len(held)):min(2*limit, len(held))]); !slices.Equal(ids(next), want) {
			t.Errorf("%s's walk reads %v after its first page, and its index holds %v next", index, ids(next), want)
		}
	}
	for index := range seeks {
		if _, ok := walks[index]; !ok {
			t.Errorf("no combination of filters walks %s", index)
		}
	}
}
