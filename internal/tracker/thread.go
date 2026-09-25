package tracker

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

// What a comment's routing needs to know about the conversation it lands in.
//
// # Why this is a read of its own rather than part of the task detail
//
// A task's comments are a thread that can be hundreds of rows, and a comment
// needs exactly three facts from them: who is already in THIS thread, who is
// owed an answer, and whether this comment answers an open ask. Loading the
// whole conversation to find three handles would make every comment pay for
// the length of the discussion it joins — and the cap on what travels is
// sixteen ([MaxThreadParticipants]), so the read is bounded where the thread
// is not.

// ThreadQuery is what a comment about to be written needs resolved.
type ThreadQuery struct {
	// Task is the item, by id.
	Task string

	// ReplyTo is the comment this one replies to, empty for a top-level
	// remark. It is what bounds the participant read to ONE thread: a task
	// is a set of conversations, and waking everybody who ever spoke on it
	// is the broadcast this design refuses.
	ReplyTo string

	// Ask is the handle this comment asks, as the caller stated it.
	Ask string

	// Answers is the comment this one answers, as the caller stated it —
	// or empty, in which case [Reader.Thread] INFERS it when exactly one
	// open ask on this task is addressed to the author.
	Answers string

	// Author is who is writing, which the inference needs: an ask is
	// answered by the person it was addressed to.
	Author string

	// Comment is the id the comment being written will carry, so that an
	// ask this very comment already answered — a retried write whose
	// first attempt landed — is not reported as answered by somebody
	// else. Empty treats every answer as another's.
	Comment string

	// Choice is the option the comment chooses, checked against the
	// decision of the ask it answers.
	Choice string
}

// ResolvedThread is the answer, ready to travel on a wake.
type ResolvedThread struct {
	ThreadParties

	// Answers is the comment id the write should stamp, which may be the
	// one the caller named or the one that was inferred.
	Answers string

	// AnswersDecision is the decision the answered ask carries, or nil —
	// what a wake needs to say which option was chosen, and what the
	// caller shows beside the choice. Immutable once asked, so reading it
	// here rather than inside the write's snapshot cannot go stale.
	AnswersDecision *Decision
}

// AlreadyAnsweredError reports an `answers` naming a question that already
// has its answer, and WHO gave it and WHEN — because the useful move is to
// read that answer, and a refusal that does not name it sends the caller
// looking. It is [ErrAlreadyAnswered].
type AlreadyAnsweredError struct {
	Task    string
	Comment string

	// By is who answered — the author of the answering comment — or who
	// resolved the question without an answer.
	By string
	At time.Time

	// Resolved is a question closed by a resolve rather than by an
	// answer, which the sentence has to tell apart.
	Resolved bool
}

func (e *AlreadyAnsweredError) Error() string {
	how := "answered"
	if e.Resolved {
		how = "resolved"
	}
	who := ""
	if e.By != "" {
		who = " by " + e.By
	}
	when := ""
	if !e.At.IsZero() {
		when = " at " + e.At.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("tracker: the question in comment %s on task %s was "+
		"already %s%s%s — read that answer rather than giving a second one",
		e.Comment, e.Task, how, who, when)
}

// Unwrap makes the typed error [ErrAlreadyAnswered].
func (e *AlreadyAnsweredError) Unwrap() error { return ErrAlreadyAnswered }

// ErrAmbiguousAnswer reports a comment whose `answers` could not be inferred
// because more than one open ask is addressed to its author.
//
// A TYPED ERROR carrying the candidates, because the caller has to name them:
// "which one" is the whole of the question, and a refusal that does not list
// them leaves a model guessing at ids it has not read.
type ErrAmbiguousAnswer struct {
	Task  string
	Asks  []AskCandidate
	Actor string
}

// AskCandidate is one open ask, as a refusal names it.
type AskCandidate struct {
	Comment string
	Author  string
	Excerpt string
}

func (e *ErrAmbiguousAnswer) Error() string {
	return fmt.Sprintf("tracker: %d open questions on task %s are addressed to "+
		"%s, so `answers` cannot be inferred — name the one you are answering",
		len(e.Asks), e.Task, e.Actor)
}

// Thread resolves a comment's conversation.
//
// BEST EFFORT ON THE PARTICIPANTS and STRICT ON THE ANSWER, which are two
// different kinds of fact: a participant list that came up short means one
// person is woken as a watcher instead of under `thread`, while an `answers`
// resolved to the wrong comment closes somebody else's question. So a read
// failure empties the first and refuses the second.
func (r *Reader) Thread(ctx context.Context, q ThreadQuery,
	fresh statelog.Freshness) (ResolvedThread, error) {

	if q.Task == "" {
		return ResolvedThread{}, fmt.Errorf("tracker: a thread read names no task")
	}
	out := ResolvedThread{
		ThreadParties: ThreadParties{Asked: q.Ask},
		Answers:       q.Answers,
	}
	// THE TASK'S OWN TERM, like every other point read here: a record
	// this node cannot decode covering this task means the comment rows
	// it is about to read may already be wrong, and routing a wake from
	// them would wake the wrong people.
	_, err := r.log.Read(ctx, fresh.Query(statelog.ScopeSet{Paths: []string{ScopeTerm{
		Kind: TermObject, ID: q.Task,
	}.Path()}}.Normalised(), false), func(tx *sql.Tx) error {
		if q.ReplyTo != "" {
			participants, err := threadParticipants(ctx, tx, q.Task, q.ReplyTo)
			if err != nil {
				return err
			}
			out.Participants = participants
		}
		if out.Answers == "" && q.Author != "" {
			inferred, err := inferAnswer(ctx, tx, q.Task, q.Author)
			if err != nil {
				return err
			}
			out.Answers = inferred
		}
		if out.Answers == "" {
			return nil
		}
		ask, err := readAsk(ctx, tx, q.Task, out.Answers)
		if err != nil {
			return err
		}
		if err := ask.answerableBy(q.Author, q.Comment); err != nil {
			return err
		}
		if err := checkChoice(q.Task, ask.id, ask.decision, q.Choice); err != nil {
			return err
		}
		out.AnswersDecision = ask.decision
		out.AnsweredAuthor = ask.author
		return nil
	})
	if err != nil {
		return ResolvedThread{}, err
	}
	return out, nil
}

// threadParticipants is everyone already in ONE thread.
//
// THE ROOT AND ITS REPLIES, resolved from whichever end the caller named: a
// model replying to a reply names the reply, and the thread is still the one
// hanging off the root. Ordered by the conversation's own order and cut at the
// cap, so the sixteen carried are the sixteen who spoke FIRST — the people the
// thread is actually between, rather than the sixteen who happened to arrive
// last.
func threadParticipants(ctx context.Context, tx *sql.Tx, task, replyTo string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH root AS (
			SELECT COALESCE(NULLIF(reply_to, ''), id) AS id
			FROM tracker_comments WHERE id = ? AND task_id = ?
		)
		SELECT c.author, MIN(c.created_at) AS first
		FROM tracker_comments c, root
		WHERE c.task_id = ? AND c.removed = 0 AND c.author <> ''
		  AND (c.id = root.id OR c.reply_to = root.id)
		GROUP BY c.author
		ORDER BY first, c.author
		LIMIT ?`, replyTo, task, task, MaxThreadParticipants)
	if err != nil {
		return nil, fmt.Errorf("tracker: read the thread under %s: %w", replyTo, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var handle string
		var first int64
		if err := rows.Scan(&handle, &first); err != nil {
			return nil, err
		}
		out = append(out, handle)
	}
	return out, rows.Err()
}

// inferAnswer is the one open ask addressed to this author, or nothing.
//
// INFERRED ONLY WHEN THERE IS EXACTLY ONE, and refused by name when there are
// several: answering the wrong question closes somebody's ask silently, and
// the two failures a caller could make here — naming none and naming the wrong
// one — are not equally recoverable.
//
// NONE IS NOT AN ERROR. Most comments answer nothing, and a comment from
// somebody with no open ask is the ordinary case rather than a mistake.
func inferAnswer(ctx context.Context, tx *sql.Tx, task, author string) (string, error) {
	// `ask <> ''` IS STATED THOUGH `ask = ?` IMPLIES IT, because the index
	// this selects on is PARTIAL — `(task_id, ask) WHERE ask <> '' AND
	// resolved = 0 AND answered_by IS NULL AND removed = 0` — and a
	// planner matches a partial index by SYNTACTIC implication. A bound
	// parameter could be the empty string as far as it can tell, so
	// without the literal term the index is skipped and this reads every
	// comment on the task.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, author, substr(body, 1, 120)
		FROM tracker_comments
		WHERE task_id = ? AND ask = ? AND ask <> '' AND resolved = 0
		  AND answered_by IS NULL AND removed = 0
		ORDER BY created_at, id
		LIMIT ?`, task, author, MaxOpenAsksNamed+1)
	if err != nil {
		return "", fmt.Errorf("tracker: read the open asks on %s for %s: %w",
			task, author, err)
	}
	defer func() { _ = rows.Close() }()
	var found []AskCandidate
	for rows.Next() {
		var one AskCandidate
		if err := rows.Scan(&one.Comment, &one.Author, &one.Excerpt); err != nil {
			return "", err
		}
		found = append(found, one)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0].Comment, nil
	}
	return "", &ErrAmbiguousAnswer{Task: task, Asks: found, Actor: author}
}

// openAsk is the row an answer is checked against.
type openAsk struct {
	id, task, author, asked string
	removed                 bool
	decision                *Decision

	// answeredBy is the comment that answered it, with its author and
	// instant; resolvedBy and resolvedAt a resolve that closed it.
	answeredBy, answerAuthor string
	answeredAt               time.Time
	resolved                 bool
	resolvedBy               string
	resolvedAt               time.Time
}

// readAsk reads the comment an answer names, with what answered it.
//
// ONE READ FOR BOTH CALLERS — the thread read that routes the wake, and the
// write's own decide that makes the answer authoritative — because two
// spellings of "is this ask still open" are how one of them stops agreeing.
func readAsk(ctx context.Context, tx *sql.Tx, task, comment string) (openAsk, error) {
	out := openAsk{id: comment, task: task}
	var resolved, removed int
	var answered, answerAuthor, resolvedBy sql.NullString
	var answeredAt, resolvedAt sql.NullInt64
	var document []byte
	err := tx.QueryRowContext(ctx, `
		SELECT c.author, c.ask, c.resolved, c.removed, c.answered_by,
		       a.author, a.created_at, c.resolved_by, c.resolved_at, c.document
		FROM tracker_comments c
		LEFT JOIN tracker_comments a ON a.id = c.answered_by
		WHERE c.id = ? AND c.task_id = ?`,
		comment, task).Scan(&out.author, &out.asked, &resolved, &removed,
		&answered, &answerAuthor, &answeredAt, &resolvedBy, &resolvedAt,
		&document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return openAsk{}, fmt.Errorf("tracker: task %s has no comment %s, so "+
			"there is nothing for this one to answer: %w", task, comment,
			ErrNoComment)
	case err != nil:
		return openAsk{}, fmt.Errorf("tracker: read the ask %s: %w", comment, err)
	}
	out.removed = removed == 1
	out.resolved = resolved == 1
	out.answeredBy = answered.String
	out.answerAuthor = answerAuthor.String
	if answeredAt.Valid {
		out.answeredAt = store.DecodeTime(answeredAt.Int64)
	}
	out.resolvedBy = resolvedBy.String
	if resolvedAt.Valid {
		out.resolvedAt = store.DecodeTime(resolvedAt.Int64)
	}
	if len(document) > 0 {
		var stored Comment
		if err := json.Unmarshal(document, &stored); err != nil {
			return openAsk{}, fmt.Errorf("tracker: decode the ask %s: %w",
				comment, err)
		}
		out.decision = stored.Decision
	}
	return out, nil
}

// answerableBy is the check that this ask is an open question addressed to
// this author — or, with author empty, to anybody, which is the decide's
// re-check of the facts that can move between the thread read and the append.
//
// REFUSED RATHER THAN IGNORED. An `answers` naming a comment that asked
// somebody else would close that person's question on their behalf, one
// naming a comment that is not an ask would stamp a remark as answered, and
// one naming a question already answered would land a second answer beside
// the first — which the applier's `answered_by IS NULL` guard kept off the
// ask while the answering comment still claimed to answer it.
//
// self is the id of the comment being written: an ask THIS comment already
// answered is a retried write whose first attempt landed, not a second
// answer.
func (a openAsk) answerableBy(author, self string) error {
	switch {
	case a.asked == "":
		return invalid("tracker: comment %s on task %s asked nobody a "+
			"question, so it cannot be answered — `answers` names an open ask",
			a.id, a.task)
	case author != "" && a.asked != author:
		return fmt.Errorf("tracker: comment %s on task %s asked %s rather "+
			"than %s, and answering it would close somebody else's question: %w",
			a.id, a.task, a.asked, author, ErrForbidden)
	case a.removed:
		return invalid("tracker: comment %s on task %s was removed",
			a.id, a.task)
	case self != "" && a.answeredBy == self:
		return nil
	case a.answeredBy != "":
		return &AlreadyAnsweredError{Task: a.task, Comment: a.id,
			By: a.answerAuthor, At: a.answeredAt}
	case a.resolved:
		return &AlreadyAnsweredError{Task: a.task, Comment: a.id,
			By: a.resolvedBy, At: a.resolvedAt, Resolved: true}
	}
	return nil
}

// MaxOpenAsksNamed is how many candidates an ambiguous-answer refusal lists.
//
// FIVE, which is what a refusal a model READS can carry without becoming a
// wall: past that the answer is "read the task", and the message says so. It
// bounds the query too, which is why it is a constant rather than a slice
// length — the read takes one more than it will name, so it can tell "five"
// from "at least five".
const MaxOpenAsksNamed = 5
