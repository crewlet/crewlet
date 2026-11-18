package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// What a comment's routing needs from the conversation it lands in: who is in
// THIS thread, who is owed an answer, and whether it answers an open ask. A
// read of its own rather than the task detail, so a comment pays for at most
// [MaxThreadParticipants] handles, not the length of the discussion it joins.

// ThreadQuery is what a comment about to be written needs resolved.
type ThreadQuery struct {
	// Task is the item, by id.
	Task string

	// ReplyTo is the comment this one replies to, empty for a top-level
	// remark. It bounds the participant read to ONE thread: waking everybody
	// who ever spoke on the task is the broadcast this design refuses.
	ReplyTo string

	// Ask is the handle this comment asks, as the caller stated it.
	Ask string

	// Answers is the comment this one answers, as the caller stated it. Empty,
	// [Reader.Thread] INFERS it when exactly one open ask on this task is
	// addressed to the author, unless the comment is already posted (Comment).
	Answers string

	// Author is who is writing, which the inference needs: an ask is
	// answered by the person it was addressed to.
	Author string

	// Comment is the id the comment being written will carry; empty treats
	// it as new. A remark already on the task under it is this call made
	// again (a retried write, a redelivered turn, an approval retried after
	// an unknown outcome) whose first attempt landed. The write answers it
	// from that row ([PostedRemark]), so the read does too, before inferring
	// anything: it asked and answers exactly what the row says, however the
	// author's open asks have moved since. Inferred again, a re-run would find
	// its own question closed, or claim one asked since that it never closed.
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

	// AnswersDecision is the decision the answered ask carries, or nil: what
	// a wake and the caller show beside the choice. Immutable once asked, so
	// reading it outside the write's snapshot cannot go stale.
	AnswersDecision *decision.Decision
}

// AlreadyAnsweredError reports an `answers` naming a question that already
// has its answer, naming WHO gave it and WHEN so the caller can read it. It
// is [ErrAlreadyAnswered].
type AlreadyAnsweredError struct {
	Task    string
	Comment string

	// By is the answering comment's author, or who resolved the question:
	// a seat handle as the rows hold it, which never changes (ADR-0013).
	By string
	At time.Time

	// Resolved is a question closed by a resolve rather than an answer.
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
	return fmt.Sprintf("worklet: the question in comment %s on task %s was "+
		"already %s%s%s — read that answer rather than giving a second one",
		e.Comment, e.Task, how, who, when)
}

// Unwrap makes the typed error [ErrAlreadyAnswered].
func (e *AlreadyAnsweredError) Unwrap() error { return ErrAlreadyAnswered }

// ErrAmbiguousAnswer reports a comment whose `answers` could not be inferred
// because more than one open ask is addressed to its author.
//
// It carries the candidates, because the caller has to name one, and a
// refusal that does not list them leaves a model guessing at ids.
type ErrAmbiguousAnswer struct {
	Task  string
	Asks  []AskCandidate
	Actor string
}

// AskCandidate is one open ask, as a refusal names it.
//
// WHOLE, never an excerpt: two questions sharing an opening are exactly the
// two an excerpt cannot tell apart. The renderer decides how many it carries
// whole and names the rest by id (the builtin's ambiguousText).
type AskCandidate struct {
	Comment string
	Author  string
	// Question is a structured ask's own question ([Decision.Question]),
	// empty for an ask in prose.
	Question string
	// Body is the comment the ask was written as.
	Body string
}

func (e *ErrAmbiguousAnswer) Error() string {
	return fmt.Sprintf("worklet: %d open questions on task %s are addressed to "+
		"%s, so `answers` cannot be inferred — name the one you are answering",
		len(e.Asks), e.Task, e.Actor)
}

// Thread resolves a comment's conversation.
//
// Best effort on the participants, strict on the answer: a short participant
// list wakes one person as a watcher instead of under `thread`, while a wrong
// `answers` closes somebody else's question.
//
// A comment already posted is resolved from its row before anything is
// inferred ([ThreadQuery.Comment]): its write is answered from that row
// ([postedComment], the one read both take), and the thread as it stands now
// would describe a write that was never made.
func (r *Reader) Thread(ctx context.Context, q ThreadQuery,
	fresh statelog.Freshness) (ResolvedThread, error) {

	if q.Task == "" {
		return ResolvedThread{}, fmt.Errorf("worklet: a thread read names no task")
	}
	out := ResolvedThread{
		ThreadParties: ThreadParties{Asked: q.Ask},
		Answers:       q.Answers,
	}
	// The task's own term, like every point read here: an undecodable record
	// covering it means the comment rows may be wrong, and a wake routed from
	// them wakes the wrong people. Resolved from the rows, because the task
	// is filed under its project and the query names only the task.
	_, err := r.log.Read(ctx, fresh.Resolved(func(ctx context.Context,
		tx *sql.Tx) (statelog.ScopeSet, error) {
		return taskReadScope(ctx, tx, q.Task)
	}, false), func(tx *sql.Tx) error {
		if q.ReplyTo != "" {
			participants, err := threadParticipants(ctx, tx, q.Task, q.ReplyTo)
			if err != nil {
				return err
			}
			owing, awaiting, err := threadStakes(ctx, tx, q.Task, q.ReplyTo, q.Author)
			if err != nil {
				return err
			}
			out.Participants, out.Owing, out.Awaiting = participants, owing, awaiting
		}
		var posted *PostedRemark
		if q.Comment != "" {
			var err error
			if posted, err = postedComment(ctx, tx, q.Task, q.Comment,
				q.Author); err != nil {
				return err
			}
		}
		switch {
		case posted != nil:
			// This call made again, whose post landed: nothing is inferred.
			out.Asked, out.Answers = posted.Asked, posted.Answers
		case out.Answers == "" && q.Author != "":
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
		// A posted answer was given, and the decide answers it from its row
		// whatever became of the question since; only its choice is checked,
		// against a decision that never changes once asked.
		if posted == nil {
			if err := ask.answerableBy(q.Author); err != nil {
				return err
			}
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
// The root and its replies, from whichever end the caller named: a reply to a
// reply is still in the thread off the root. Cut at the cap in conversation
// order, so those carried are the ones who spoke FIRST, whom the thread is
// between, not the latest arrivals.
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
		return nil, fmt.Errorf("worklet: read the thread under %s: %w", replyTo, err)
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

// threadStakes is who has a stake in ONE thread (ADR-0113): the handles its
// open asks are addressed to (owing) and the authors of those asks (awaiting),
// less a co-asker of author's when author owes nothing there, each cut at
// [MaxThreadParticipants].
func threadStakes(ctx context.Context, tx *sql.Tx, task, replyTo, author string) (owing,
	awaiting []string, err error) {

	rows, err := tx.QueryContext(ctx, `
		WITH root AS (
			SELECT COALESCE(NULLIF(reply_to, ''), id) AS id
			FROM tracker_comments WHERE id = ? AND task_id = ?
		)
		SELECT c.ask, c.author
		FROM tracker_comments c, root
		WHERE c.task_id = ? AND c.removed = 0 AND c.ask <> ''
		  AND c.resolved = 0 AND c.answered_by IS NULL
		  AND (c.id = root.id OR c.reply_to = root.id)
		ORDER BY c.created_at, c.id`, replyTo, task, task)
	if err != nil {
		return nil, nil, fmt.Errorf("worklet: read the open asks under %s: %w", replyTo, err)
	}
	defer func() { _ = rows.Close() }()
	asked := map[string][]string{}
	var askers []string
	for rows.Next() {
		var ask, asker string
		if err := rows.Scan(&ask, &asker); err != nil {
			return nil, nil, err
		}
		if !slices.Contains(owing, ask) && len(owing) < MaxThreadParticipants {
			owing = append(owing, ask)
		}
		if asker == "" {
			continue
		}
		if _, seen := asked[asker]; !seen {
			askers = append(askers, asker)
		}
		asked[asker] = append(asked[asker], ask)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	owes := slices.Contains(owing, author)
	for _, asker := range askers {
		if len(awaiting) == MaxThreadParticipants {
			break
		}
		// A co-asker's chatter: an author whose only stake is awaiting the
		// addressee this asker awaits does not wake it.
		if author != "" && asker != author && !owes && slices.ContainsFunc(asked[asker],
			func(a string) bool { return slices.Contains(asked[author], a) }) {
			continue
		}
		awaiting = append(awaiting, asker)
	}
	return owing, awaiting, nil
}

// openAsksStatement is the open asks on one task addressed to one author,
// oldest first, at most its last argument of them: [inferAnswer]'s candidates.
//
// The non-empty ask is stated though `ask = ?` implies it: the index is PARTIAL
// (task_id, ask over open comments with a non-empty ask), and a planner
// matches one by SYNTACTIC implication, so without the literal it reads every
// comment on the task. Named so TestTheOpenAskInferenceIsIndexServed plans
// this text rather than a copy a deletion here would leave green.
const openAsksStatement = `
		SELECT id, author, body, document
		FROM tracker_comments
		WHERE task_id = ? AND ask = ? AND ask <> '' AND resolved = 0
		  AND answered_by IS NULL AND removed = 0 AND overtaken = ''
		ORDER BY created_at, id
		LIMIT ?`

// inferAnswer is the one open ask addressed to this author, or nothing (most
// comments answer nothing). Several are refused by name, never guessed:
// answering the wrong question closes somebody's ask silently.
func inferAnswer(ctx context.Context, tx *sql.Tx, task, author string) (string, error) {
	rows, err := tx.QueryContext(ctx, openAsksStatement, task, author, MaxOpenAsksNamed+1)
	if err != nil {
		return "", fmt.Errorf("worklet: read the open asks on %s for %s: %w",
			task, author, err)
	}
	defer func() { _ = rows.Close() }()
	var found []AskCandidate
	for rows.Next() {
		var one AskCandidate
		var document []byte
		if err := rows.Scan(&one.Comment, &one.Author, &one.Body, &document); err != nil {
			return "", err
		}
		var stored Comment
		if len(document) > 0 && json.Unmarshal(document, &stored) == nil && stored.Decision != nil {
			one.Question = stored.Decision.Question
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
	decision                *decision.Decision
	act                     *delegated.Act

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
// One read for both callers, the thread read that routes the wake and the
// write's decide, so their "is this ask still open" cannot disagree.
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
		return openAsk{}, fmt.Errorf("worklet: task %s has no comment %s, so "+
			"there is nothing for this one to answer: %w", task, comment,
			ErrNoComment)
	case err != nil:
		return openAsk{}, fmt.Errorf("worklet: read the ask %s: %w", comment, err)
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
			return openAsk{}, fmt.Errorf("worklet: decode the ask %s: %w",
				comment, err)
		}
		out.decision, out.act = stored.Decision, stored.Act
	}
	return out, nil
}

// answerableBy checks that this ask is an open question addressed to author,
// or with author empty to anybody: the decide's re-check of what can move
// between the thread read and the append.
//
// Refused rather than ignored: an `answers` naming another's ask would close
// their question for them, a non-ask would stamp a remark answered, and an
// answered question would get a second answer the applier's `answered_by IS
// NULL` guard keeps off the ask while the comment claims it. An answer this
// comment already posted never reaches here (both callers resolve it from its
// row, [postedComment]), so an answered ask was answered by another comment.
// Every refusal names the seat handles, so the caller can find the person.
func (a openAsk) answerableBy(author string) error {
	switch {
	case a.asked == "":
		return invalidSaying(caveat.NotAQuestion(), "comment %s on task %s asked nobody a "+
			"question, so it cannot be answered — `answers` names an open ask",
			a.id, a.task)
	case author != "" && a.asked != author:
		return forbiddenSaying(caveat.AskedSomebodyElse(a.asked), "comment %s on task %s asked %s rather than %s, and "+
			"answering it would close somebody else's question",
			a.id, a.task, a.asked,
			author)
	case a.removed:
		return invalidSaying(caveat.QuestionRemoved(), "comment %s on task %s was removed",
			a.id, a.task)
	case a.answeredBy != "":
		return &AlreadyAnsweredError{Task: a.task, Comment: a.id,
			By: a.answerAuthor, At: a.answeredAt}
	case a.resolved:
		return &AlreadyAnsweredError{Task: a.task, Comment: a.id,
			By: a.resolvedBy, At: a.resolvedAt,
			Resolved: true}
	}
	return nil
}

// MaxOpenAsksNamed is how many candidates an ambiguous-answer refusal lists.
//
// Five is what a refusal a model reads carries without becoming a wall; past
// it the message says "read the task". It bounds the query too: the read
// takes one more than it names, to tell "five" from "more than five".
const MaxOpenAsksNamed = 5
