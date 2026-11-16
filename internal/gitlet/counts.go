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
	// CountRepos is the repositories by kind and status ([RepoKindCounts]), the company's alone.
	CountRepos CountPart = "repos"
)

// CountParts is the closed set, in the order the parts were added.
func CountParts() []CountPart { return []CountPart{CountChanges, CountRepos} }

// Valid reports a part this build counts.
func (p CountPart) Valid() bool { return slices.Contains(CountParts(), p) }

// CountsQuery is the parts a counts read asks, each a key of its own: a part
// added later is a key an older build refuses unread, so the walk asks another node.
type CountsQuery struct {
	Changes bool `json:"changes,omitempty"`
	Repos   bool `json:"repos,omitempty"`
}

// Ask sets p's key and reports whether this build counts p.
func (q *CountsQuery) Ask(p CountPart) bool {
	switch p {
	case CountChanges:
		q.Changes = true
	case CountRepos:
		q.Repos = true
	default:
		return false
	}
	return true
}

// asks reports a query asking at least one part.
func (q CountsQuery) asks() bool { return q.Changes || q.Repos }

// Counts is a counts read's answer: a part each it asked, absent where not asked.
type Counts struct {
	Changes *ChangeCounts   `json:"changes,omitempty"`
	Repos   *RepoKindCounts `json:"repos,omitempty"`
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

// RepoKindCounts is repositories by kind, each by status: a field a kind ([RepoKinds]).
type RepoKindCounts struct {
	Project RepoCounts `json:"project"`
	Scratch RepoCounts `json:"scratch"`
}

// of is k's counts, nil for a kind this build does not know.
func (c *RepoKindCounts) of(k RepoKind) *RepoCounts {
	switch k {
	case RepoProject:
		return &c.Project
	case RepoScratch:
		return &c.Scratch
	}
	return nil
}

// RepoCounts is repositories by status; Live is those in use ([RepoStatus.live]).
// Every field is a count, a zero included; absence is the pointer's that holds it.
type RepoCounts struct {
	Live      int64 `json:"live"`
	Importing int64 `json:"importing"`
	Active    int64 `json:"active"`
	Archived  int64 `json:"archived"`
	Deleted   int64 `json:"deleted"`
	Purging   int64 `json:"purging"`
}

// add counts n more in s, and in Live where s is in use; a status this build does not know
// moves nothing.
func (c *RepoCounts) add(s RepoStatus, n int64) {
	switch s {
	case RepoStatusImporting:
		c.Importing += n
	case RepoStatusActive:
		c.Active += n
	case RepoStatusArchived:
		c.Archived += n
	case RepoStatusDeleted:
		c.Deleted += n
	case RepoStatusPurging:
		c.Purging += n
	default:
		return
	}
	if s.live() {
		c.Live += n
	}
}

// plus is c and o added field by field.
func (c RepoCounts) plus(o RepoCounts) RepoCounts {
	return RepoCounts{Live: c.Live + o.Live, Importing: c.Importing + o.Importing, Active: c.Active + o.Active,
		Archived: c.Archived + o.Archived, Deleted: c.Deleted + o.Deleted, Purging: c.Purging + o.Purging}
}

// negated is c with every count's sign turned.
func (c RepoCounts) negated() RepoCounts {
	return RepoCounts{Live: -c.Live, Importing: -c.Importing, Active: -c.Active, Archived: -c.Archived,
		Deleted: -c.Deleted, Purging: -c.Purging}
}

// zero reports counts holding no repository in any status.
func (c RepoCounts) zero() bool {
	return c.Importing == 0 && c.Active == 0 && c.Archived == 0 && c.Deleted == 0 && c.Purging == 0
}

// in is how many repositories c counts in s; none for a status this build does not know.
func (c RepoCounts) in(s RepoStatus) int64 {
	switch s {
	case RepoStatusImporting:
		return c.Importing
	case RepoStatusActive:
		return c.Active
	case RepoStatusArchived:
		return c.Archived
	case RepoStatusDeleted:
		return c.Deleted
	case RepoStatusPurging:
		return c.Purging
	}
	return 0
}

// sum is how many repositories c counts in the statuses named, a status named twice once.
func (c RepoCounts) sum(statuses []RepoStatus) int64 {
	var n int64
	for _, s := range RepoStatuses() {
		if slices.Contains(statuses, s) {
			n += c.in(s)
		}
	}
	return n
}

// statusCounts is the counts of the five count columns, in [RepoStatuses]' order.
func statusCounts(importing, active, archived, deleted, purging int64) RepoCounts {
	var c RepoCounts
	for i, n := range []int64{importing, active, archived, deleted, purging} {
		c.add(RepoStatuses()[i], n)
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
	case q.Counts.Repos && q.Repo != "":
		return refused("repo", "repositories are counted across every one the asker reads; a counts read "+
			"asking for repos names none")
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
	if q.Counts.Repos {
		var (
			c   RepoKindCounts
			err error
		)
		if c, err = talliedRepos(ctx, tx, q.Asker, q.CeilingSeat, chart); err != nil {
			return err
		}
		counts.Repos = &c
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

// talliedRepos sums the repository tally rows asker and its ceiling read, by kind and status,
// each key judged as [talliedChanges] judges it.
func talliedRepos(ctx context.Context, tx *sql.Tx, asker Principal, ceilingSeat string,
	chart Roster) (RepoKindCounts, error) {

	var sum RepoKindCounts
	err := eachRepoTally(ctx, tx, func(key repoTallyKey, c RepoCounts) error {
		ref := RepoRef{Owner: key.owner, Visibility: key.visibility}
		if _, refused := JudgeRead(asker, ceilingSeat, ref, chart); refused == nil {
			if of := sum.of(key.kind); of != nil {
				*of = of.plus(c)
			}
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

// repoFacts is what [ReadQuery.RepoFacts] answers of one repository from its row.
type repoFacts struct {
	changes      ChangeCounts
	pushMirrored bool
}

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
			pushMirrored                bool
		)
		if err = rows.Scan(&id, &draft, &open, &closed, &merged, &pushMirrored); err != nil {
			return nil, fmt.Errorf("gitlet: read a repository's facts: %w", err)
		}
		facts[id] = repoFacts{changes: stateCounts(draft, open, closed, merged), pushMirrored: pushMirrored}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read the repositories' facts: %w", err)
	}
	return facts, nil
}

// withFacts sets each repository of repos' facts its row holds, each its own copy, read in one
// statement; its card's other parts are [Reader.carded]'s.
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
		f := facts[s.ID]
		s.ChangeCounts = &f.changes
		s.Card = &RepoCard{PushMirrored: f.pushMirrored}
	}
	return nil
}

// The counts' reads, each registered in plan_test.go.
const (
	// changeTalliesSQL is every tally row, read whole: the scan's bound is scanOK's.
	changeTalliesSQL = `SELECT visibility, owner_kind, owner, draft_changes, open_changes, closed_changes,
		merged_changes FROM code_change_tallies`
	// repoFactsOfSQL is the facts of the repositories a JSON list names, by id.
	repoFactsOfSQL = `SELECT id, draft_changes, open_changes, closed_changes, merged_changes, mirror_to != ''
		FROM code_repos WHERE id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
)
