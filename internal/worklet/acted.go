package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// ActedDecision is the newest question a seat asked on a task that a person
// answered: the decision a turn of the seat's on that task acts on, which a
// call it holds for approval names, so the person approving it reads which
// decision the call carries out.
type ActedDecision struct {
	// Ask is the question's comment id, and Question what it asked.
	Ask      string
	Question string
	// Choice is the label of the option the answer chose, empty for an answer
	// in words; AnsweredBy and AnsweredAt are who answered and when.
	Choice     string
	AnsweredBy string
	AnsweredAt time.Time
}

// actedDecisionStatement is [Reader.DecisionActedOn]'s read, named for the plan
// test: the asker's answered questions on one task, newest answer first.
const actedDecisionStatement = `
	SELECT q.id, q.document, a.author, a.created_at, a.document
	FROM tracker_comments q JOIN tracker_comments a ON a.id = q.answered_by
	WHERE q.task_id = ? AND q.author = ? AND q.ask <> '' AND q.removed = 0
	  AND q.answered_by IS NOT NULL
	ORDER BY a.created_at DESC, a.id DESC
	LIMIT 1`

// DecisionActedOn is the newest question asker put on task that a person
// answered, nil for none, read at level.
func (r *Reader) DecisionActedOn(ctx context.Context, task, asker string,
	level statelog.ReadLevel) (*ActedDecision, error) {

	if task == "" || asker == "" {
		return nil, invalid("a decision read names its task and its asker")
	}
	var out *ActedDecision
	_, err := r.log.Read(ctx, statelog.Query{
		Level: level,
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, func(tx *sql.Tx) error {
		var (
			ask, answerer  string
			askDoc, ansDoc []byte
			answeredAt     int64
		)
		err := tx.QueryRowContext(ctx, actedDecisionStatement, task, asker).Scan(&ask, &askDoc,
			&answerer, &answeredAt, &ansDoc)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("worklet: read the decision %s acts on in %s: %w", asker, task, err)
		}
		var question, answer Comment
		if err := json.Unmarshal(askDoc, &question); err != nil {
			return fmt.Errorf("worklet: decode the question %s: %w", ask, err)
		}
		if err := json.Unmarshal(ansDoc, &answer); err != nil {
			return fmt.Errorf("worklet: decode the answer to %s: %w", ask, err)
		}
		acted := &ActedDecision{Ask: ask, AnsweredBy: answerer, AnsweredAt: store.DecodeTime(answeredAt)}
		if question.Decision != nil {
			acted.Question = question.Decision.Question
			for _, o := range question.Decision.Options {
				if o.ID == answer.Choice && answer.Choice != "" {
					acted.Choice = o.Label
				}
			}
		}
		if acted.Question == "" {
			acted.Question = question.Body
		}
		out = acted
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
