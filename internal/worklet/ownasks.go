package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A SEAT'S OWN OPEN ASKS TO ONE PERSON: the questions it put to somebody that
// still wait on them, read for the turn that seat holds with that person, so
// "consider anything you have open with me" is answerable from the turn's
// own context, and an ask other work now covers is withdrawn rather than left
// waiting in the person's Inbox.

// MaxOwnAsks bounds one page of a seat's own open asks to one person.
//
// TEN, [MaxDecisions]'s fifth: the page is read into a turn's prompt, where
// each row is a line the seat acts on with its own tools, and a seat with more
// open questions to one person than this has a queue the total beside the
// page names.
const MaxOwnAsks = 10

// OwnAsksQuery asks for one seat's open asks to one person.
type OwnAsksQuery struct {
	// Asker is the seat whose questions these are, and AskedOf the person
	// they were put to, both by record name.
	Asker   string
	AskedOf string

	// Limit is the page, at most [MaxOwnAsks]; zero reads that many.
	Limit int

	Level statelog.ReadLevel
}

// OwnAskRow is one open question a seat put to a person, with the call that
// withdraws it.
type OwnAskRow struct {
	TaskRow

	// Comment is the ask's id, which `withdraw_ask` names as `ask`.
	Comment string    `json:"comment"`
	AskedOf string    `json:"asked_of"`
	AskedAt time.Time `json:"asked_at"`

	// Question is the decision's one-sentence question; empty for an ask
	// put without one (an older ask, or one to an agent), which is read
	// with get_work_item rather than cut to fit.
	Question string `json:"question,omitempty" untrusted:"name"`

	// Withdraw is the literal call that withdraws it, on the task's address.
	Withdraw string `json:"withdraw_with"`
}

// OwnAsks is one page of a seat's open asks to one person, newest first, and
// how many there are.
type OwnAsks struct {
	Asks  []OwnAskRow `json:"asks"`
	Total ClaimTotal  `json:"total"`
}

// ownAsksFrom and ownAsksWhere are the asker's open asks to one person, bound
// to (asker, person). The ask, resolution and answer terms are the asker
// index's own predicate, so the read seeks it by author.
const (
	ownAsksFrom  = openAsksFrom
	ownAsksWhere = `c.author = ? AND c.ask = ? AND c.ask <> ''
		  AND c.resolved = 0 AND c.answered_by IS NULL
		  AND c.removed = 0 AND c.overtaken = '' AND t.removed_at IS NULL`

	// ownAsksPageStatement is [Reader.OwnAsks]' page, newest first.
	ownAsksPageStatement = `
		SELECT c.id, c.task_id, c.created_at, c.document
		FROM ` + ownAsksFrom + `
		WHERE ` + ownAsksWhere + `
		ORDER BY c.created_at DESC
		LIMIT ?`
)

// OwnAsks reads the open asks q's asker put to q's person, newest first.
func (r *Reader) OwnAsks(ctx context.Context, q OwnAsksQuery, now time.Time) (OwnAsks, error) {
	switch {
	case q.Asker == "" || q.AskedOf == "" || q.Level == "":
		return OwnAsks{}, invalid("an own-asks read names an asker, a person and a level, "+
			"not %q, %q at %q", q.Asker, q.AskedOf, q.Level)
	case q.Limit < 0 || q.Limit > MaxOwnAsks:
		return OwnAsks{}, invalid("an own-asks page holds 1 to %d asks, not %d", MaxOwnAsks, q.Limit)
	}
	limit := q.Limit
	if limit == 0 {
		limit = MaxOwnAsks
	}
	var out OwnAsks
	_, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// THE DOMAIN, for [Reader.MyWork]'s reason: an ask can be on any
		// task in any container.
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, func(tx *sql.Tx) error {
		var err error
		out, err = readOwnAsks(ctx, tx, q.Asker, q.AskedOf, limit, now.UTC())
		return err
	})
	if err != nil {
		return OwnAsks{}, fmt.Errorf("worklet: read %s's open asks to %s: %w", q.Asker, q.AskedOf, err)
	}
	return out, nil
}

// readOwnAsks is [Reader.OwnAsks]' read inside its transaction.
func readOwnAsks(ctx context.Context, tx *sql.Tx, asker, person string, limit int,
	now time.Time) (OwnAsks, error) {

	args := []any{asker, person}
	total, capped, err := countCapped(ctx, tx, ownAsksFrom, ownAsksWhere, args)
	if err != nil {
		return OwnAsks{}, fmt.Errorf("count them: %w", err)
	}
	rows, err := tx.QueryContext(ctx, ownAsksPageStatement, asker, person, limit)
	if err != nil {
		return OwnAsks{}, err
	}
	defer func() { _ = rows.Close() }()
	type pending struct {
		comment, task string
		at            int64
		document      []byte
	}
	var page []pending
	for rows.Next() {
		var p pending
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err := rows.Scan(&p.comment, &p.task, &p.at, &p.document); err != nil {
			return OwnAsks{}, fmt.Errorf("scan an ask: %w", err)
		}
		page = append(page, p)
	}
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := rows.Err(); err != nil {
		return OwnAsks{}, err
	}
	out := OwnAsks{Asks: []OwnAskRow{}, Total: ClaimTotal{Total: total, Capped: capped}}
	if len(page) == 0 {
		return out, nil
	}
	ids := make([]any, 0, len(page))
	for _, p := range page {
		ids = append(ids, p.task)
	}
	tasks, _, err := readTasks(ctx, tx, tasksWhere("t.id IN ("+placeholders(len(ids))+")", ids),
		[]sortTerm{{Column: "t.id"}}, len(ids), now)
	if err != nil {
		return OwnAsks{}, err
	}
	byID := make(map[string]TaskRow, len(tasks))
	for _, row := range tasks {
		byID[row.ID] = row
	}
	for _, p := range page {
		row, held := byID[p.task]
		if !held {
			continue
		}
		var stored Comment
		if len(p.document) > 0 {
			if err := json.Unmarshal(p.document, &stored); err != nil {
				return OwnAsks{}, fmt.Errorf("decode the ask %s: %w", p.comment, err)
			}
		}
		ask := OwnAskRow{TaskRow: row, Comment: p.comment, AskedOf: person,
			AskedAt: store.DecodeTime(p.at), Withdraw: withdrawCall(row.Address(), p.comment)}
		if stored.Decision != nil {
			ask.Question = stored.Decision.Question
		}
		out.Asks = append(out.Asks, ask)
	}
	return out, nil
}

// withdrawCall is the literal call that withdraws an ask, on the task's
// address ([ItemAddress]) for [answerCall]'s reason.
func withdrawCall(address, comment string) string {
	return fmt.Sprintf("%s(item: %q, ask: %q, reason: \"…\")", WithdrawAskTool, address, comment)
}
