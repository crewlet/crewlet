package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// AskView is the question a notice or a feed row is about, as it stands now.
//
// # Why a notice carries it
//
// An `asked` notice says somebody put a question to you and an `answered` one
// that your question came back — and neither could say WHICH question, whether
// it is still open, or what was chosen, without a second read per row. A card
// that has to ask "is this still waiting on me?" of another endpoint is a card
// that renders stale, and an inbox built around decisions needs the options on
// the row it is drawing.
//
// # It is the ASK, whichever comment the change wrote
//
// A change that asked writes the ask itself; a change that answered writes the
// answer, whose `answers` names the ask. Both resolve here to the ASK, because
// that is the object with a state — open, answered, resolved — and the thing a
// person decides on. The answer's own contribution is who answered, when, and
// the option it chose.
//
// # It is read NOW, not at the change
//
// `open` is the ask's state at the instant of this read, not when the notice
// was written: an `asked` notice for a question somebody has since answered
// reads `open: false` with the answer beside it, which is the whole point —
// the notice is history and the ask is live.
type AskView struct {
	// Comment is the ask's own id — what an answer names in `answers`.
	Comment string `json:"comment"`

	// AskedOf is the handle the question was put to.
	AskedOf string `json:"asked_of"`

	// Open is true while nobody has answered or resolved it and it has not
	// been removed.
	Open bool `json:"open"`

	// AnsweredBy and AnsweredAt are who closed it and when: the answering
	// comment's author and instant, or — with Resolved — who resolved it
	// without an answer. Both empty while it is open.
	AnsweredBy string     `json:"answered_by,omitempty"`
	AnsweredAt *time.Time `json:"answered_at,omitempty"`

	// Resolved says it was closed by a resolve rather than an answer, so a
	// card does not report a choice nobody made.
	Resolved bool `json:"resolved,omitempty"`

	// Choice is the option the answer chose, by id — see [Comment.Choice].
	Choice string `json:"choice,omitempty"`

	// Decision is the structure the ask carries when it asked somebody to
	// choose — see decision.go.
	Decision *Decision `json:"decision,omitempty"`
}

// readAskViews resolves comment ids to the asks they are about.
//
// ONE STATEMENT FOR A PAGE, whatever its size: the comment a row names, joined
// to the ask it is or answers, joined to the comment that answered that ask. A
// comment that is neither an ask nor an answer — an ordinary remark — joins
// nothing and is simply absent from the map, which is how a row about one
// carries no `ask`.
//
// Shared by the inbox and the activity feed, because "which question is this
// row about, and where does it stand" is one question and two readers of it
// written separately would disagree the first time the rule moved.
func readAskViews(ctx context.Context, tx *sql.Tx, comments []string) (
	map[string]*AskView, error) {

	out := make(map[string]*AskView, len(comments))
	if len(comments) == 0 {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, a.id, a.ask, a.resolved, a.removed,
		       a.answered_by IS NOT NULL, a.resolved_by, a.resolved_at,
		       a.document, ans.author, ans.created_at, ans.document
		  FROM tracker_comments c
		  JOIN tracker_comments a
		    ON a.id = CASE WHEN c.ask <> '' THEN c.id ELSE c.answers END
		  LEFT JOIN tracker_comments ans ON ans.id = a.answered_by
		 WHERE c.id IN (SELECT value FROM json_each(?))
		   AND a.ask <> ''`, idList(comments))
	if err != nil {
		return nil, fmt.Errorf("tracker: read the asks a page is about: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var view AskView
		var comment string
		var resolved, removed, answered int
		var resolvedBy, answerAuthor sql.NullString
		var resolvedAt, answeredAt sql.NullInt64
		var askDocument, answerDocument []byte
		if err := rows.Scan(&comment, &view.Comment, &view.AskedOf, &resolved,
			&removed, &answered, &resolvedBy, &resolvedAt, &askDocument,
			&answerAuthor, &answeredAt, &answerDocument); err != nil {

			return nil, fmt.Errorf("tracker: scan an ask: %w", err)
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
					return nil, fmt.Errorf("tracker: decode the answer to %s: %w",
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
				return nil, fmt.Errorf("tracker: decode the ask %s: %w",
					view.Comment, err)
			}
			view.Decision = ask.Decision
		}
		out[comment] = &view
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tracker: read the asks a page is about: %w", err)
	}
	return out, nil
}
