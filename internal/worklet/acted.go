package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// ActedDecision is a question a seat asked on a task and the answer a person
// gave it, whose delivery woke the seat's turn: the decision that turn carries
// out, which a call it holds for approval names.
type ActedDecision struct {
	// Task and Key are the task the question was asked on.
	Task, Key string
	// Ask is the question's comment id, and Question what it asked.
	Ask      string
	Question string
	// Choice is the label of the option the answer chose, empty for an answer
	// in words; AnsweredBy and AnsweredAt are who answered and when.
	Choice     string
	AnsweredBy string
	AnsweredAt time.Time
}

// AnswersIn is the comments answering a question whose deliveries are among
// evs, a coalesced delivery's constituents included, each once: what woke a
// turn as an answer to its seat.
func AnswersIn(evs []*events.Event) []string {
	var out []string
	add := func(metadata map[string]string) {
		if Reason(metadata[MetaVia]) == ReasonAnswered && metadata[MetaCommentID] != "" &&
			!slices.Contains(out, metadata[MetaCommentID]) {
			out = append(out, metadata[MetaCommentID])
		}
	}
	for _, ev := range evs {
		n, ok := events.DataAs[*types.ExternalNotification](ev)
		if !ok || n == nil {
			continue
		}
		add(n.Metadata)
		for _, m := range n.Messages {
			add(m.Metadata)
		}
	}
	return out
}

// answeredDecisionStatement is [Reader.DecisionAnswered]'s read, named for the
// plan test: one answer by id, with the question it answers and its task.
const answeredDecisionStatement = `
	SELECT a.author, a.created_at, a.document, q.id, q.document, t.id, t.key
	FROM tracker_comments a
	JOIN tracker_comments q ON q.id = a.answers
	JOIN tracker_tasks t ON t.id = a.task_id
	WHERE a.id = ? AND a.author_kind = 'human' AND a.removed = 0
	  AND q.author = ? AND q.ask <> '' AND q.removed = 0`

// MaxActedAnswers bounds the answers one read judges: a turn woken by more
// answers than a coalesced delivery holds is still judged on its first ones.
const MaxActedAnswers = 8

// DecisionAnswered is the newest decision among answers that a person gave to
// a question asker put, nil for none: an agent's answer, or one to another
// seat's question, is no decision asker carries out.
func (r *Reader) DecisionAnswered(ctx context.Context, answers []string, asker string,
	level statelog.ReadLevel) (*ActedDecision, error) {

	if asker == "" {
		return nil, invalid("a decision read names its asker")
	}
	if len(answers) == 0 {
		return nil, nil
	}
	answers = answers[:min(len(answers), MaxActedAnswers)]
	var out *ActedDecision
	_, err := r.log.Read(ctx, statelog.Query{
		Level: level,
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, func(tx *sql.Tx) error {
		for _, answer := range answers {
			acted, err := readAnswerGiven(ctx, tx, answer, asker)
			if err != nil {
				return err
			}
			if acted != nil && (out == nil || acted.AnsweredAt.After(out.AnsweredAt)) {
				out = acted
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readAnswerGiven is the decision one answer gave asker, nil where it gave none.
func readAnswerGiven(ctx context.Context, tx *sql.Tx, answer, asker string) (*ActedDecision, error) {
	var (
		acted          ActedDecision
		answeredAt     int64
		ansDoc, askDoc []byte
	)
	err := tx.QueryRowContext(ctx, answeredDecisionStatement, answer, asker).Scan(&acted.AnsweredBy,
		&answeredAt, &ansDoc, &acted.Ask, &askDoc, &acted.Task, &acted.Key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("worklet: read the decision answer %s gave %s: %w", answer, asker, err)
	}
	var question, given Comment
	if err := json.Unmarshal(askDoc, &question); err != nil {
		return nil, fmt.Errorf("worklet: decode the question %s: %w", acted.Ask, err)
	}
	if err := json.Unmarshal(ansDoc, &given); err != nil {
		return nil, fmt.Errorf("worklet: decode the answer %s: %w", answer, err)
	}
	acted.AnsweredAt = store.DecodeTime(answeredAt)
	acted.Question = question.Body
	if question.Decision != nil {
		acted.Question = question.Decision.Question
		for _, o := range question.Decision.Options {
			if given.Choice != "" && o.ID == given.Choice {
				acted.Choice = o.Label
			}
		}
	}
	return &acted, nil
}
