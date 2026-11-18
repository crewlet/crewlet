package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/store"
)

// AskView is the question a notice or a feed row is about, as it stands now.
//
// A notice carries it so a card can say which question, whether it is still
// open and what was chosen without a second read per row (a card that asks
// another endpoint renders stale). It is always the ASK, whichever comment
// the change wrote, since the ask holds the state and is what a person
// decides on; an answer contributes who answered, when, and its choice. And
// it is read NOW: an `asked` notice for a question since answered reads
// `open: false` with the answer beside it — the notice is history, the ask
// is live.
type AskView struct {
	// Comment is the ask's own id — what an answer names in `answers`.
	Comment string `json:"comment"`

	// AskedOf is the person the question was put to, by their seat's
	// handle.
	AskedOf string `json:"asked_of"`

	// Open is true while nobody has answered or resolved it and it has not
	// been removed.
	Open bool `json:"open"`

	// Overtaken says the facts settled an open ask before anybody answered
	// it ([OvertakenSuperseded], [OvertakenMoot]): it no longer waits on its
	// person, and an answer is still recorded if one comes.
	Overtaken string `json:"overtaken,omitempty"`

	// AnsweredBy and AnsweredAt are who closed it and when: the answering
	// comment's author and instant, or — with Resolved — who resolved it
	// without an answer. Both empty while it is open.
	AnsweredBy string     `json:"answered_by,omitempty"`
	AnsweredAt *time.Time `json:"answered_at,omitempty"`

	// Resolved says it was closed by a resolve rather than an answer, so a
	// card does not report a choice nobody made. Only its asker resolves a
	// question, so a resolved question is one its asker withdrew.
	Resolved bool `json:"resolved,omitempty"`

	// Withdrawn is why its asker withdrew it and what covers it now, where
	// the withdrawal recorded them ([Withdrawal]).
	Withdrawn *Withdrawn `json:"withdrawn,omitempty"`

	// Choice is the option the answer chose, by id — see [Comment.Choice].
	Choice string `json:"choice,omitempty"`

	// Decision is the structure the ask carries when it asked somebody to
	// choose — see decision.go.
	Decision *decision.Decision `json:"decision,omitempty"`
}

// askViewsStatement is [readAskViews]' one statement, bound to the page's
// comment ids as one JSON array.
//
// The ids are SOUGHT: `c.id IN (SELECT value FROM json_each(?))` puts
// json_each's column right of IN, which the pinned driver's planner cannot
// seek by, so it scanned every comment per page. An expression over the
// column is seekable, and CAST(… AS TEXT) changes nothing, every id being
// text (TestTheAsksAPageNamesAreSoughtByID).
const askViewsStatement = `
	SELECT c.id, a.id, a.ask, a.resolved, a.removed,
	       a.answered_by IS NOT NULL, a.resolved_by, a.resolved_at,
	       a.document, ans.author, ans.created_at, ans.document, a.overtaken, a.task_id
	  FROM tracker_comments c
	  JOIN tracker_comments a
	    ON a.id = CASE WHEN c.ask <> '' THEN c.id ELSE c.answers END
	  LEFT JOIN tracker_comments ans ON ans.id = a.answered_by
	 WHERE c.id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
	   AND a.ask <> ''`

// readAskViews resolves comment ids to the asks they are about.
//
// One statement for a page: the comment a row names, joined to the ask it is
// or answers, joined to the comment that answered that ask. An ordinary
// remark joins nothing and is absent from the map, so its row carries no
// `ask`. Shared by the inbox and the activity feed, so the two cannot
// disagree on which question a row is about and where it stands.
func readAskViews(ctx context.Context, tx *sql.Tx, comments []string) (
	map[string]*AskView, error) {

	out := make(map[string]*AskView, len(comments))
	if len(comments) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, askViewsStatement, idList(comments))
	if err != nil {
		return nil, fmt.Errorf("worklet: read the asks a page is about: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type withdrawnAsk struct {
		view       *AskView
		task       string
		withdrawal *Withdrawal
	}
	var withdrawn []withdrawnAsk
	for rows.Next() {
		var view AskView
		var comment, task string
		var resolved, removed, answered int
		var resolvedBy, answerAuthor sql.NullString
		var resolvedAt, answeredAt sql.NullInt64
		var askDocument, answerDocument []byte
		if err := rows.Scan(&comment, &view.Comment, &view.AskedOf, &resolved,
			&removed, &answered, &resolvedBy, &resolvedAt, &askDocument,
			&answerAuthor, &answeredAt, &answerDocument, &view.Overtaken, &task); err != nil {

			return nil, fmt.Errorf("worklet: scan an ask: %w", err)
		}
		view.Open = resolved == 0 && answered == 0 && removed == 0
		switch {
		case answered != 0:
			view.AnsweredBy = answerAuthor.String
			if answeredAt.Valid {
				at := store.DecodeTime(answeredAt.Int64)
				view.AnsweredAt = &at
			}
			if len(answerDocument) > 0 {
				var answer Comment
				if err := json.Unmarshal(answerDocument, &answer); err != nil {
					return nil, fmt.Errorf("worklet: decode the answer to %s: %w",
						view.Comment, err)
				}
				view.Choice = answer.Choice
			}
		case resolved != 0:
			view.Resolved = true
			view.AnsweredBy = resolvedBy.String
			if resolvedAt.Valid {
				at := store.DecodeTime(resolvedAt.Int64)
				view.AnsweredAt = &at
			}
		}
		if len(askDocument) > 0 {
			var ask Comment
			if err := json.Unmarshal(askDocument, &ask); err != nil {
				return nil, fmt.Errorf("worklet: decode the ask %s: %w",
					view.Comment, err)
			}
			view.Decision = ask.Decision
			if view.Resolved && ask.Withdrawal != nil {
				withdrawn = append(withdrawn, withdrawnAsk{&view, task, ask.Withdrawal})
			}
		}
		out[comment] = &view
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worklet: read the asks a page is about: %w", err)
	}
	// AFTER THE PAGE IS READ, one key read each: withdrawn questions are few,
	// and a statement inside the page's would hold two cursors on one handle.
	_ = rows.Close()
	for _, w := range withdrawn {
		var err error
		if w.view.Withdrawn, err = readWithdrawn(ctx, tx, w.task, w.withdrawal); err != nil {
			return nil, err
		}
	}
	return out, nil
}
