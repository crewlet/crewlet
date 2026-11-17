package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ChangeQuery is one search of changes' words.
type ChangeQuery struct {
	Terms                []string // the text's terms (textindex.Terms); a change is held where every one is among its words
	RepoID, State, Owner string   // the scope: one repository by id, one state, one owner; "" is any
	Limit                int      // the most ids answered
}

// ChangeHeld is the index's answer, read in one snapshot of this node's own file. Its zero BackPos and
// BackFloor are an index built whole.
type ChangeHeld struct {
	IDs        []string // newest minted first, by each document's key, at most Limit
	Truncated  bool     // the read stopped at the scan of what it drove on, or more than Limit matched
	Through    int64    // the forward walk's watermark: a change moved past BackPos and no further is indexed
	BackPos    int64    // where the first build began: a change at or below it is indexed where its id is at or above BackFloor
	BackFloor  string   // the least change id the first build has reached back to; "" once it reached the oldest
	Seen       int64    // the code log's settled position in the newest estate snapshot an index write read
	BuiltFrom  string   // the estate instance it was built from
	Rebuilding bool     // a rebuild asked for, or the first build not begun: the index answers nothing
}

// changeScopes are the scope keys a document keeps, each with its index, in the order a tie between
// their counts is broken.
var changeScopes = []string{"repo_id", "owner", "state"}

// changeScopeIndex is the index of change_docs a scope key leads.
var changeScopeIndex = map[string]string{
	"repo_id": "change_docs_repo_idx",
	"owner":   "change_docs_owner_idx",
	"state":   "change_docs_state_idx",
}

// The statements a search counts and bounds by, each named for the plan gate.
const (
	changeTermCountStatement = `SELECT COUNT(*) FROM (SELECT 1 FROM change_postings
		INDEXED BY change_postings_term_idx WHERE term = ? LIMIT ?)`
	// changeTermFloorStatement is the key of the n-th newest document holding a word.
	changeTermFloorStatement = `SELECT slot >> 16 FROM change_postings INDEXED BY change_postings_term_idx
		WHERE term = ? ORDER BY slot DESC LIMIT 1 OFFSET ?`
)

// changeScopeCountStatement counts, up to a bound, the documents of one value of a scope key.
func changeScopeCountStatement(key string) string {
	return `SELECT COUNT(*) FROM (SELECT 1 FROM change_docs INDEXED BY ` + changeScopeIndex[key] +
		` WHERE ` + key + ` = ? LIMIT ?)`
}

// changeScopeFloorStatement is the key of the n-th newest document of one value of a scope key.
func changeScopeFloorStatement(key string) string {
	return `SELECT doc FROM change_docs INDEXED BY ` + changeScopeIndex[key] + ` WHERE ` + key +
		` = ? ORDER BY doc DESC LIMIT 1 OFFSET ?`
}

// changeHoldsTerm is a test that document d holds one more word: one seek of the term key over d's
// slot range.
func changeHoldsTerm(alias string) string {
	return ` AND EXISTS (SELECT 1 FROM change_postings AS ` + alias + ` INDEXED BY change_postings_term_idx WHERE ` +
		alias + `.term = ? AND ` + alias + `.slot >= (d.doc << 16) AND ` + alias + `.slot < ((d.doc + 1) << 16))`
}

// changeScopeResiduals is the scope tests a statement makes on a document d beside what it drives on.
func changeScopeResiduals(repo, state, owner bool) string {
	var b strings.Builder
	for _, on := range []struct {
		named bool
		key   string
	}{{repo, "repo_id"}, {state, "state"}, {owner, "owner"}} {
		if on.named {
			b.WriteString(` AND d.` + on.key + ` = ?`)
		}
	}
	return b.String()
}

// changeHeldByTermStatement is the documents holding a search's words, driven on one word's postings
// newest first: each reaches its document by its key, which answers the id and the named scopes, and
// others more words are each one seek of the term key.
func changeHeldByTermStatement(others int, repo, state, owner, floored bool) string {
	var b strings.Builder
	b.WriteString(`SELECT d.id FROM change_postings AS p INDEXED BY change_postings_term_idx` +
		` CROSS JOIN change_docs AS d WHERE p.term = ?`)
	if floored {
		b.WriteString(` AND p.slot >= ?`)
	}
	b.WriteString(` AND d.doc = (p.slot >> 16)` + changeScopeResiduals(repo, state, owner))
	for i := range others {
		b.WriteString(changeHoldsTerm("o" + strconv.Itoa(i+1)))
	}
	b.WriteString(` ORDER BY p.slot DESC LIMIT ?`)
	return b.String()
}

// changeHeldByScopeStatement is the documents holding a search's words, driven on one scope's
// documents newest first: words more words, each one seek of the term key, and the other named scopes
// tested.
func changeHeldByScopeStatement(key string, words int, repo, state, owner, floored bool) string {
	var b strings.Builder
	b.WriteString(`SELECT d.id FROM change_docs AS d INDEXED BY ` + changeScopeIndex[key] +
		` WHERE d.` + key + ` = ?`)
	if floored {
		b.WriteString(` AND d.doc >= ?`)
	}
	b.WriteString(changeScopeResiduals(repo && key != "repo_id", state && key != "state",
		owner && key != "owner"))
	for i := range words {
		b.WriteString(changeHoldsTerm("t" + strconv.Itoa(i+1)))
	}
	b.WriteString(` ORDER BY d.doc DESC LIMIT ?`)
	return b.String()
}

// changeDriver is what a search reads first: a scope key and its value, or a term.
type changeDriver struct {
	key   string // a scope key, "" for a term
	value string
	term  int // the term's place in the query, where it is one
	count int
}

// Holding answers the newest minted changes this index holds every word of q for, in q's scope, with
// how far each walk reached and what its writes were decided from, which the reader compares with its
// own snapshot (ADR-0156). It reads the narrowest of the scope and the words first and cuts within it.
func (x *ChangeIndexer) Holding(ctx context.Context, q ChangeQuery) (ChangeHeld, error) {
	if len(q.Terms) == 0 || q.Limit <= 0 {
		return ChangeHeld{}, errors.New("search: a search of changes names its words and a limit")
	}
	var held ChangeHeld
	err := x.db.Read(ctx, func(tx *sql.Tx) (err error) {
		held = ChangeHeld{}
		state, err := readChangeIndexState(ctx, tx)
		if err != nil {
			return err
		}
		held.Through, held.Seen, held.BuiltFrom = state.through, state.seen, state.builtFrom
		held.BackPos, held.BackFloor = state.backPos, state.backFloor
		held.Rebuilding = x.rebuild.Load() || !state.began()
		if held.Rebuilding {
			return nil
		}
		held.IDs, held.Truncated, err = x.holding(ctx, tx, q)
		return err
	})
	if err != nil {
		return ChangeHeld{}, fmt.Errorf("search: read the index of changes' words: %w", err)
	}
	return held, nil
}

// holding is [ChangeIndexer.Holding]'s ids, read inside tx.
func (x *ChangeIndexer) holding(ctx context.Context, tx *sql.Tx, q ChangeQuery) ([]string, bool, error) {
	scope := map[string]string{"repo_id": q.RepoID, "owner": q.Owner, "state": q.State}
	var driver *changeDriver
	consider := func(key, value string, term int, statement string) error {
		var n int
		if err := tx.QueryRowContext(ctx, statement, value, x.scan+1).Scan(&n); err != nil {
			return err
		}
		if driver == nil || n < driver.count {
			driver = &changeDriver{key: key, value: value, term: term, count: n}
		}
		return nil
	}
	for _, key := range changeScopes {
		if scope[key] != "" {
			if err := consider(key, scope[key], -1, changeScopeCountStatement(key)); err != nil {
				return nil, false, err
			}
		}
	}
	for i, term := range q.Terms {
		if err := consider("", term, i, changeTermCountStatement); err != nil {
			return nil, false, err
		}
	}
	if driver.count == 0 {
		return nil, false, nil
	}
	truncated := false
	var floor *int64
	if driver.count > x.scan {
		statement := changeTermFloorStatement
		if driver.key != "" {
			statement = changeScopeFloorStatement(driver.key)
		}
		var doc int64
		if err := tx.QueryRowContext(ctx, statement, driver.value, x.scan-1).Scan(&doc); err != nil {
			return nil, false, err
		}
		floor, truncated = &doc, true
	}

	var (
		statement string
		args      = []any{driver.value}
	)
	// A word's postings are bounded by the floor document's first slot, a scope's documents by its key.
	switch {
	case floor != nil && driver.key == "":
		args = append(args, *floor<<changeSlotBits)
	case floor != nil:
		args = append(args, *floor)
	}
	repo, state, owner := q.RepoID != "", q.State != "", q.Owner != ""
	if driver.key == "" {
		statement = changeHeldByTermStatement(len(q.Terms)-1, repo, state, owner, floor != nil)
		args = append(args, scopeArgs(q, "")...)
		for i, term := range q.Terms {
			if i != driver.term {
				args = append(args, term)
			}
		}
	} else {
		statement = changeHeldByScopeStatement(driver.key, len(q.Terms), repo, state, owner, floor != nil)
		args = append(args, scopeArgs(q, driver.key)...)
		for _, term := range q.Terms {
			args = append(args, term)
		}
	}
	args = append(args, q.Limit+1)
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var key []byte
		if err = rows.Scan(&key); err != nil {
			return nil, false, err
		}
		if len(ids) == q.Limit {
			truncated = true
			break
		}
		id, idErr := changeID(key)
		if idErr != nil {
			return nil, false, idErr
		}
		ids = append(ids, id)
	}
	return ids, truncated, rows.Err()
}

// scopeArgs is the residual scope values a statement binds, in [changeScopeResiduals]' order, leaving
// out the key it drives on.
func scopeArgs(q ChangeQuery, driven string) []any {
	var out []any
	for _, on := range []struct{ key, value string }{{"repo_id", q.RepoID}, {"state", q.State},
		{"owner", q.Owner}} {
		if on.value != "" && on.key != driven {
			out = append(out, on.value)
		}
	}
	return out
}
