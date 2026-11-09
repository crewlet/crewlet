package worklet

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// PatternsQuery is the window a [Reader.WorkPatterns] read covers.
type PatternsQuery struct {
	Since, Until time.Time
	Level        statelog.ReadLevel
	// Reviewers are the agent seats whose held reviews are read; one with no
	// turn by its holder since QuietSince is idle.
	Reviewers  []string
	QuietSince time.Time
	// People are the persons whose notices in the window are counted.
	People []string
}

// NoticeLoad is how many notices one person was sent in the window, and how
// many of them asked something of them.
type NoticeLoad struct {
	Handle    string `json:"handle"`
	Notices   int    `json:"notices"`
	Addressed int    `json:"addressed"`
}

// IdleReview is an item in review its agent holder has taken no turn on.
type IdleReview struct {
	Task   string    `json:"task"`
	Key    string    `json:"key"`
	Holder string    `json:"holder"`
	Since  time.Time `json:"since"`
}

// MaxPatternExamples bounds the records a pattern names as its evidence.
const MaxPatternExamples = 5

// WorkPatterns is what the company's comments and answers did in a window,
// read from the records alone, for the advisor's engine findings.
type WorkPatterns struct {
	// AgentComments counts every comment an agent wrote.
	AgentComments int `json:"agent_comments"`
	// Named are the seats an agent's comment that asked nothing named, each
	// with how many such notices it was sent and how many obliged an answer.
	Named []NamedWithoutAsk `json:"named"`
	// Answers counts a person's answers to an agent's question; Superseded
	// those whose asker had asked the same person again on the item first,
	// and Moot those given after the item finished.
	Answers    int      `json:"answers"`
	Superseded int      `json:"superseded"`
	Moot       int      `json:"moot"`
	Overtaken  []string `json:"overtaken"`
	// IdleReviews are the reviewers' items in review idle since QuietSince.
	IdleReviews []IdleReview `json:"idle_reviews"`
	// Notices are the people's notice loads, in the order they were asked for.
	Notices []NoticeLoad `json:"notices"`
}

// NamedWithoutAsk is one seat agents' comments named without a question.
type NamedWithoutAsk struct {
	Handle string `json:"handle"`
	// Told counts the notices; Asked those whose author said the comment asks;
	// Woken those that obliged an answer though its author said it asks nothing.
	// A notice whose record kept no statement (an older build's) is neither.
	Told  int `json:"told"`
	Asked int `json:"asked"`
	Woken int `json:"woken"`
	// Comments and WokenComments are at most five of each, by id.
	Comments      []string `json:"comments"`
	WokenComments []string `json:"woken_comments"`
}

// WorkPatterns reads q's window of comments and answers.
func (r *Reader) WorkPatterns(ctx context.Context, q PatternsQuery) (WorkPatterns, error) {
	if q.Level == "" || !q.Since.Before(q.Until) || q.QuietSince.After(q.Until) {
		return WorkPatterns{}, invalid("a patterns read names a level, a window that ends "+
			"after it starts and a quiet instant inside it, not %q from %s to %s, quiet since %s",
			q.Level, q.Since, q.Until, q.QuietSince)
	}
	var out WorkPatterns
	_, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, func(tx *sql.Tx) error {
		since, until := store.EncodeTime(q.Since), store.EncodeTime(q.Until)
		if err := readNamedWithoutAsk(ctx, tx, since, until, &out); err != nil {
			return err
		}
		if err := readAnswers(ctx, tx, since, until, &out); err != nil {
			return err
		}
		if err := readIdleReviews(ctx, tx, q.Reviewers, q.QuietSince, &out); err != nil {
			return err
		}
		return readNoticeLoads(ctx, tx, q.People, since, until, &out)
	})
	return out, err
}

// patternNamedStatement is every agent comment in the window, with each
// mention notice it sent and its author's statement of whether it asks.
const patternNamedStatement = `SELECT c.id, c.ask, n.recipient, n.addressed, n.asks
FROM tracker_comments c
LEFT JOIN tracker_notifications n ON n.record_id = c.record_id AND n.reason = 'mention'
WHERE c.author_kind = 'agent' AND c.created_at >= ? AND c.created_at < ? AND c.removed = 0
ORDER BY c.created_at, c.id`

func readNamedWithoutAsk(ctx context.Context, tx *sql.Tx, since, until int64, out *WorkPatterns) error {
	rows, err := tx.QueryContext(ctx, patternNamedStatement, since, until)
	if err != nil {
		return fmt.Errorf("worklet: read the window's agent comments: %w", err)
	}
	byHandle := map[string]*NamedWithoutAsk{}
	last := ""
	for rows.Next() {
		var id, ask string
		var recipient sql.NullString
		var addressed, asks sql.NullBool
		if err := rows.Scan(&id, &ask, &recipient, &addressed, &asks); err != nil {
			_ = rows.Close()
			return fmt.Errorf("worklet: scan an agent comment: %w", err)
		}
		if id != last {
			out.AgentComments++
			last = id
		}
		if ask != "" || !recipient.Valid {
			continue
		}
		named := byHandle[recipient.String]
		if named == nil {
			named = &NamedWithoutAsk{Handle: recipient.String, Comments: []string{},
				WokenComments: []string{}}
			byHandle[recipient.String] = named
		}
		named.Told++
		switch {
		case !asks.Valid:
			// An older build's record kept no statement: unknown, so neither.
		case asks.Bool:
			named.Asked++
		case addressed.Bool:
			named.Woken++
			if len(named.WokenComments) < MaxPatternExamples {
				named.WokenComments = append(named.WokenComments, id)
			}
		}
		if len(named.Comments) < MaxPatternExamples {
			named.Comments = append(named.Comments, id)
		}
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("worklet: read the window's agent comments: %w", err)
	}
	out.Named = make([]NamedWithoutAsk, 0, len(byHandle))
	for _, named := range byHandle {
		out.Named = append(out.Named, *named)
	}
	slices.SortFunc(out.Named, func(a, b NamedWithoutAsk) int { return cmp.Compare(a.Handle, b.Handle) })
	return nil
}

// patternAnswersStatement is every person's answer in the window to an agent's
// question, with whether the item had finished first and whether the asker had
// asked the same person again on it, still open when this answer came.
const patternAnswersStatement = `SELECT a.id,
	t.finished_at IS NOT NULL AND t.finished_at <= r.created_at,
	EXISTS (SELECT 1 FROM tracker_comments b
		WHERE b.task_id = a.task_id AND b.author = a.author AND b.ask = a.ask
		AND b.removed = 0 AND b.created_at > a.created_at AND b.created_at < r.created_at
		AND NOT EXISTS (SELECT 1 FROM tracker_comments ba
			WHERE ba.id = b.answered_by AND ba.created_at <= r.created_at))
FROM tracker_comments r
JOIN tracker_comments a ON a.id = r.answers
JOIN tracker_tasks t ON t.id = a.task_id
WHERE r.author_kind IN ('human', 'operator') AND r.created_at >= ? AND r.created_at < ?
	AND r.removed = 0 AND a.author_kind = 'agent' AND a.ask <> ''
ORDER BY r.created_at, r.id`

func readAnswers(ctx context.Context, tx *sql.Tx, since, until int64, out *WorkPatterns) error {
	rows, err := tx.QueryContext(ctx, patternAnswersStatement, since, until)
	if err != nil {
		return fmt.Errorf("worklet: read the window's answers: %w", err)
	}
	out.Overtaken = []string{}
	for rows.Next() {
		var ask string
		var moot, superseded bool
		if err := rows.Scan(&ask, &moot, &superseded); err != nil {
			_ = rows.Close()
			return fmt.Errorf("worklet: scan an answer: %w", err)
		}
		out.Answers++
		switch {
		case moot:
			out.Moot++
		case superseded:
			out.Superseded++
		default:
			continue
		}
		if len(out.Overtaken) < MaxPatternExamples {
			out.Overtaken = append(out.Overtaken, ask)
		}
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("worklet: read the window's answers: %w", err)
	}
	return nil
}

// patternIdleReviewsStatement is every item in review one reviewer holds, in
// review since before the quiet instant, with no turn by its holder since then.
const patternIdleReviewsStatement = `SELECT t.id, t.key, t.assignee, t.status_entered_at
FROM tracker_tasks t
WHERE t.assignee = ? AND t.status_group = ?
	AND t.status = 'in_review' AND t.removed_at IS NULL AND t.status_entered_at < ?
	AND NOT EXISTS (SELECT 1 FROM tracker_turns u
		WHERE u.task_id = t.id AND u.effective_at >= ? AND u.seat = t.assignee)
ORDER BY t.status_entered_at, t.id`

func readIdleReviews(ctx context.Context, tx *sql.Tx, reviewers []string, quiet time.Time,
	out *WorkPatterns) error {
	out.IdleReviews = []IdleReview{}
	at := store.EncodeTime(quiet)
	for _, reviewer := range reviewers {
		if err := readIdleReviewsOf(ctx, tx, reviewer, at, out); err != nil {
			return err
		}
	}
	return nil
}

// patternAssignedStatement is when an item last changed hands, a seek of its own
// history; an item never reassigned has no row and was held from its filing.
const patternAssignedStatement = `SELECT h.effective_at FROM tracker_history h
	WHERE h.subject_id = ? AND json_extract(h.fields_json, '$.assignee.to') IS NOT NULL
	ORDER BY h.log_seq DESC LIMIT 1`

// readIdleReviewsOf reads one reviewer's idle reviews, a seek of its own queue,
// leaving out one it was handed after the quiet instant: its wait starts then.
func readIdleReviewsOf(ctx context.Context, tx *sql.Tx, reviewer string, at int64, out *WorkPatterns) error {
	rows, err := tx.QueryContext(ctx, patternIdleReviewsStatement, reviewer,
		string(StatusInReview.Group()), at, at)
	if err != nil {
		return fmt.Errorf("worklet: read %s's idle reviews: %w", reviewer, err)
	}
	var candidates []IdleReview
	for rows.Next() {
		var r IdleReview
		var since int64
		if err := rows.Scan(&r.Task, &r.Key, &r.Holder, &since); err != nil {
			_ = rows.Close()
			return fmt.Errorf("worklet: scan an idle review: %w", err)
		}
		r.Since = store.DecodeTime(since)
		candidates = append(candidates, r)
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("worklet: read %s's idle reviews: %w", reviewer, err)
	}
	for _, r := range candidates {
		var assigned int64
		err := tx.QueryRowContext(ctx, patternAssignedStatement, r.Task).Scan(&assigned)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("worklet: read when %s changed hands: %w", r.Task, err)
		case assigned >= at:
			continue
		default:
			if t := store.DecodeTime(assigned); t.After(r.Since) {
				r.Since = t
			}
		}
		out.IdleReviews = append(out.IdleReviews, r)
	}
	return nil
}

// patternNoticesStatement is one person's notices in the window, a seek of
// their own rows, which the inbox horizon already bounds.
const patternNoticesStatement = `SELECT COUNT(*), COALESCE(SUM(addressed), 0)
FROM tracker_notifications
WHERE recipient = ? AND created_at >= ? AND created_at < ?`

func readNoticeLoads(ctx context.Context, tx *sql.Tx, people []string, since, until int64, out *WorkPatterns) error {
	out.Notices = make([]NoticeLoad, 0, len(people))
	for _, handle := range people {
		load := NoticeLoad{Handle: handle}
		if err := tx.QueryRowContext(ctx, patternNoticesStatement, handle, since, until).Scan(
			&load.Notices, &load.Addressed); err != nil {
			return fmt.Errorf("worklet: count %s's notices: %w", handle, err)
		}
		out.Notices = append(out.Notices, load)
	}
	return nil
}
