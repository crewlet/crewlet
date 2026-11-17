package gitlet

// THE CHANGE A PUSH LANDED: each history row names the change its own record merged on a branch it set.

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// pushMergedSQL is each change merged at one commit of a JSON list, with its branch and the record that merged
// it for the caller to keep to the command's: one seek of the merge index a commit.
const pushMergedSQL = `SELECT CAST(j.key AS INTEGER), c.target_ref, c.position, c.number, c.title, c.merged_via
	FROM json_each(?2) AS j CROSS JOIN code_changes AS c INDEXED BY ` + commitMergedKey + `
	WHERE c.repo_id = ?1 AND c.merge_oid = CAST(j.value AS TEXT) AND c.merge_oid != ''`

// mergedKey is a command a push could have merged a change by: the branch, the commit it set it to, and the
// position of the push's record.
type mergedKey struct {
	ref, commit string
	position    int64
}

// mergedNames gathers a page's commands that set a branch to a commit, each with the page's rows it is one of.
type mergedNames struct {
	keys  []mergedKey
	index map[mergedKey]int
	rows  [][]int
}

// add gathers row i's ok commands that set a branch to a commit: from its commands whole, before any cut.
func (m *mergedNames) add(i int, row PushRow) {
	for _, c := range row.Commands {
		if c.Result != ResultOK || c.New == ZeroOID || !strings.HasPrefix(c.Ref, branchPrefix) {
			continue
		}
		key := mergedKey{ref: c.Ref, commit: c.New, position: row.Position.Packed()}
		if m.index == nil {
			m.index = map[mergedKey]int{}
		}
		k, found := m.index[key]
		if !found {
			k = len(m.keys)
			m.index[key] = k
			m.keys = append(m.keys, key)
			m.rows = append(m.rows, nil)
		}
		if !slices.Contains(m.rows[k], i) {
			m.rows[k] = append(m.rows[k], i)
		}
	}
}

// mergedChange is a change one of a row's commands merged, as the row ranks it.
type mergedChange struct {
	number int64
	title  string
	landed bool
}

// name sets each gathered row's Change and ChangesMore from one statement, repo the rows' repository, and
// answers what each of rows grew by ([PushRow.changeBytes]).
func (m *mergedNames) name(ctx context.Context, tx *sql.Tx, repo RepoRef, rows []PushRow) ([]int, error) {
	grew := make([]int, len(rows))
	if len(m.keys) == 0 {
		return grew, nil
	}
	list := make([]string, len(m.keys))
	for i, k := range m.keys {
		list[i] = k.commit
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	found, err := tx.QueryContext(ctx, pushMergedSQL, repo.ID, string(encoded))
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the changes %s's pushes merged: %w", repo.Name, err)
	}
	defer func() { _ = found.Close() }()
	merged := make(map[int][]mergedChange, len(rows))
	for found.Next() {
		var (
			k        int
			c        mergedChange
			ref, via string
			position int64
		)
		if err = found.Scan(&k, &ref, &position, &c.number, &c.title, &via); err != nil {
			return nil, fmt.Errorf("gitlet: read the changes %s's pushes merged: %w", repo.Name, err)
		}
		if ref != m.keys[k].ref || position != m.keys[k].position {
			continue
		}
		c.landed = MergedVia(via) == MergedViaDirect || MergedVia(via) == MergedViaQueue
		for _, i := range m.rows[k] {
			if !slices.ContainsFunc(merged[i], func(o mergedChange) bool { return o.number == c.number }) {
				merged[i] = append(merged[i], c)
			}
		}
	}
	if err = found.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read the changes %s's pushes merged: %w", repo.Name, err)
	}
	for i, changes := range merged {
		// A MERGE'S OR A LANDING'S OWN CHANGE FIRST, then the lowest number: the one the row says it landed.
		first := slices.MinFunc(changes, func(a, b mergedChange) int {
			if a.landed != b.landed {
				if a.landed {
					return -1
				}
				return 1
			}
			return cmp.Compare(a.number, b.number)
		})
		rows[i].Change = &ChangeName{Repo: repo.Name, Ref: ChangeRef(repo.Name, first.number), Title: first.title,
			State: ChangeStateMerged}
		rows[i].ChangesMore = len(changes) - 1
		grew[i] = rows[i].changeBytes()
	}
	return grew, nil
}

// recut is how many of a page's rows a page of maxRows and maxB keeps once each row's charge carries what
// its name grew it by: all of them, or up to the one that fills it.
func recut(charges, grew []int, maxRows, maxB int) int {
	pg := page{maxRows: maxRows, maxB: maxB}
	for i, c := range charges {
		if pg.add(c+grew[i]) && i < len(charges)-1 {
			return i + 1
		}
	}
	return len(charges)
}
