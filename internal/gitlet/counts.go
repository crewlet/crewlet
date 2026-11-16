package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/words"
)

// A SCOPE'S COUNTS, the `counts` read: the parts it asks of one repository or
// of every one in use the asker reads, each read from counters the applies keep
// (apply_counts.go) in one snapshot, never by walking the changes.

// CountPart names a part of a counts read at its surface; on the wire each is a key of
// [CountsQuery] and of [Counts] by the same JSON name.
type CountPart string

const (
	// CountChanges is the scope's changes by state ([ChangeCounts]).
	CountChanges CountPart = "changes"
)

// CountParts is the closed set, in the order the parts were added.
func CountParts() []CountPart { return []CountPart{CountChanges} }

// Valid reports a part this build counts.
func (p CountPart) Valid() bool { return slices.Contains(CountParts(), p) }

// CountsQuery is the parts a counts read asks, each a key of its own: a part
// added later is a key an older build refuses unread, so the walk asks another node.
type CountsQuery struct {
	Changes bool `json:"changes,omitempty"`
}

// Ask sets p's key and reports whether this build counts p.
func (q *CountsQuery) Ask(p CountPart) bool {
	switch p {
	case CountChanges:
		q.Changes = true
	default:
		return false
	}
	return true
}

// asks reports a query asking at least one part.
func (q CountsQuery) asks() bool { return q.Changes }

// Counts is a counts read's answer: a part each it asked, absent where not asked.
type Counts struct {
	Changes *ChangeCounts `json:"changes,omitempty"`
}

// ChangeCounts is changes by state; Live is those in a live state ([ChangeState.Live]).
// Every field is a count, a zero included; absence is the pointer's that holds it.
type ChangeCounts struct {
	Live   int64 `json:"live"`
	Draft  int64 `json:"draft"`
	Open   int64 `json:"open"`
	Closed int64 `json:"closed"`
	Merged int64 `json:"merged"`
}

// add counts n more in s, and in Live where s is live; a state this build does not
// know moves nothing.
func (c *ChangeCounts) add(s ChangeState, n int64) {
	switch s {
	case ChangeStateDraft:
		c.Draft += n
	case ChangeStateOpen:
		c.Open += n
	case ChangeStateClosed:
		c.Closed += n
	case ChangeStateMerged:
		c.Merged += n
	default:
		return
	}
	if s.Live() {
		c.Live += n
	}
}

// move counts one change moved from one state to another; nothing where they are one.
func (c *ChangeCounts) move(from, to ChangeState) {
	if from == to {
		return
	}
	c.add(from, -1)
	c.add(to, 1)
}

// plus is c and o added field by field.
func (c ChangeCounts) plus(o ChangeCounts) ChangeCounts {
	return ChangeCounts{Live: c.Live + o.Live, Draft: c.Draft + o.Draft, Open: c.Open + o.Open,
		Closed: c.Closed + o.Closed, Merged: c.Merged + o.Merged}
}

// negated is c with every count's sign turned.
func (c ChangeCounts) negated() ChangeCounts {
	return ChangeCounts{Live: -c.Live, Draft: -c.Draft, Open: -c.Open, Closed: -c.Closed, Merged: -c.Merged}
}

// zero reports counts holding no change in any state.
func (c ChangeCounts) zero() bool {
	return c.Draft == 0 && c.Open == 0 && c.Closed == 0 && c.Merged == 0
}

// stateCounts is the counts of the four count columns, in [ChangeStates]' order.
func stateCounts(draft, open, closed, merged int64) ChangeCounts {
	var c ChangeCounts
	for i, n := range []int64{draft, open, closed, merged} {
		c.add(ChangeStates()[i], n)
	}
	return c
}

// validateCounts is [ReadQuery.validate] for a `counts` read: at least one part,
// and one answer, never paged.
func (q ReadQuery) validateCounts() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Counts == nil || !q.Counts.asks():
		return refused("counts", "a counts read names at least one part: "+words.Or(CountParts()))
	case q.Cursor != "":
		return refused("cursor", "counts are one answer, never paged")
	case q.Limit != 0:
		return refused("limit", "counts are one answer, never paged")
	}
	return nil
}

// counts answers the parts q asks of its scope, judged once, in the read's one transaction:
// one repository by name, whatever its status, as a listing naming it serves it, or every
// repository in use the asker and its ceiling read.
func (r *Reader) counts(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	var repoID string
	if q.Repo != "" {
		repo, err := readableIn(ctx, tx, q.Repo, q.Asker, q.CeilingSeat, chart)
		if err != nil {
			return err
		}
		repoID = repo.ref.ID
	}
	var counts Counts
	if q.Counts.Changes {
		var c ChangeCounts
		if repoID != "" {
			facts, err := repoFactsIn(ctx, tx, []string{repoID})
			if err != nil {
				return err
			}
			c = facts[repoID].changes
		} else {
			var err error
			if c, err = talliedChanges(ctx, tx, q.Asker, q.CeilingSeat, chart); err != nil {
				return err
			}
		}
		counts.Changes = &c
	}
	out.Counts = &counts
	return nil
}

// talliedChanges sums the tally rows asker and its ceiling read. Who reads a repository is
// a function of its owner, its visibility and the chart alone ([GrantOf]), so a tally row,
// keyed by the two, is judged exactly as each of its repositories would be.
func talliedChanges(ctx context.Context, tx *sql.Tx, asker Principal, ceilingSeat string,
	chart Roster) (ChangeCounts, error) {

	var sum ChangeCounts
	err := eachTally(ctx, tx, func(key tallyKey, c ChangeCounts) error {
		ref := RepoRef{Owner: key.owner, Visibility: key.visibility}
		if _, refused := JudgeRead(asker, ceilingSeat, ref, chart); refused == nil {
			sum = sum.plus(c)
		}
		return nil
	})
	return sum, err
}

// eachTally hands fn every tally row, read whole first.
func eachTally(ctx context.Context, tx *sql.Tx, fn func(tallyKey, ChangeCounts) error) error {
	type tallied struct {
		key    tallyKey
		counts ChangeCounts
	}
	rows, err := tx.QueryContext(ctx, changeTalliesSQL)
	if err != nil {
		return fmt.Errorf("gitlet: read the change tallies: %w", err)
	}
	var all []tallied
	for rows.Next() {
		var (
			visibility, kind, owner     string
			draft, open, closed, merged int64
		)
		if err = rows.Scan(&visibility, &kind, &owner, &draft, &open, &closed, &merged); err != nil {
			_ = rows.Close()
			return fmt.Errorf("gitlet: read a change tally: %w", err)
		}
		all = append(all, tallied{key: tallyKey{visibility: Visibility(visibility),
			owner: Owner{Kind: OwnerKind(kind), Ref: owner}}, counts: stateCounts(draft, open, closed, merged)})
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("gitlet: read the change tallies: %w", err)
	}
	_ = rows.Close()
	for _, t := range all {
		if err = fn(t.key, t.counts); err != nil {
			return err
		}
	}
	return nil
}

// repoFacts is what [ReadQuery.RepoFacts] answers of one repository.
type repoFacts struct{ changes ChangeCounts }

// repoFactsIn is the facts of the repositories ids name, by id, in one statement.
func repoFactsIn(ctx context.Context, tx *sql.Tx, ids []string) (map[string]repoFacts, error) {
	facts := make(map[string]repoFacts, len(ids))
	if len(ids) == 0 {
		return facts, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, repoFactsOfSQL, list)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the repositories' facts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id                          string
			draft, open, closed, merged int64
		)
		if err = rows.Scan(&id, &draft, &open, &closed, &merged); err != nil {
			return nil, fmt.Errorf("gitlet: read a repository's facts: %w", err)
		}
		facts[id] = repoFacts{changes: stateCounts(draft, open, closed, merged)}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read the repositories' facts: %w", err)
	}
	return facts, nil
}

// withFacts sets each repository of repos' facts, each row its own copy, read in one statement.
func withFacts(ctx context.Context, tx *sql.Tx, repos []*RepoSummary) error {
	ids := make([]string, len(repos))
	for i, s := range repos {
		ids[i] = s.ID
	}
	facts, err := repoFactsIn(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, s := range repos {
		changes := facts[s.ID].changes
		s.ChangeCounts = &changes
	}
	return nil
}

// The counts' reads, each registered in plan_test.go.
const (
	// changeTalliesSQL is every tally row, read whole: the scan's bound is scanOK's.
	changeTalliesSQL = `SELECT visibility, owner_kind, owner, draft_changes, open_changes, closed_changes,
		merged_changes FROM code_change_tallies`
	// repoFactsOfSQL is the facts of the repositories a JSON list names, by id.
	repoFactsOfSQL = `SELECT id, draft_changes, open_changes, closed_changes, merged_changes FROM code_repos
		WHERE id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
)
