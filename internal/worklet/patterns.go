package worklet

import (
	"cmp"
	"context"
	"database/sql"
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
}

// NamedWithoutAsk is one seat an agent's comment named without asking it.
type NamedWithoutAsk struct {
	Handle string `json:"handle"`
	// Told counts the notices, Woken those that obliged an answer.
	Told     int      `json:"told"`
	Woken    int      `json:"woken"`
	Comments []string `json:"comments"`
}

// WorkPatterns reads q's window of comments and answers.
func (r *Reader) WorkPatterns(ctx context.Context, q PatternsQuery) (WorkPatterns, error) {
	if q.Level == "" || !q.Since.Before(q.Until) {
		return WorkPatterns{}, invalid("a patterns read names a level and a window "+
			"that ends after it starts, not %q from %s to %s", q.Level, q.Since, q.Until)
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
		return readAnswers(ctx, tx, since, until, &out)
	})
	return out, err
}

// patternNamedStatement is every agent comment in the window, with each
// mention notice an agent comment that asked nothing sent.
const patternNamedStatement = `SELECT c.id, c.ask, n.recipient, n.addressed
FROM tracker_comments c
LEFT JOIN tracker_notifications n ON n.record_id = c.record_id AND n.reason = 'mention'
WHERE c.author_kind = 'agent' AND c.created_at >= ? AND c.created_at < ? AND c.removed = 0
ORDER BY c.created_at, c.id`

func readNamedWithoutAsk(ctx context.Context, tx *sql.Tx, since, until int64, out *WorkPatterns) error {
	rows, err := tx.QueryContext(ctx, patternNamedStatement, since, until)
	if err != nil {
		return fmt.Errorf("worklet: read the window's agent comments: %w", err)
	}
	defer closeRows(rows)
	byHandle := map[string]*NamedWithoutAsk{}
	last := ""
	for rows.Next() {
		var id, ask string
		var recipient sql.NullString
		var addressed sql.NullBool
		if err := rows.Scan(&id, &ask, &recipient, &addressed); err != nil {
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
			named = &NamedWithoutAsk{Handle: recipient.String, Comments: []string{}}
			byHandle[recipient.String] = named
		}
		named.Told++
		if addressed.Bool {
			named.Woken++
		}
		if len(named.Comments) < MaxPatternExamples {
			named.Comments = append(named.Comments, id)
		}
	}
	if err := rows.Err(); err != nil {
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
// question, with whether the item had finished first and whether the asker
// had asked the same person again on it before the answer.
const patternAnswersStatement = `SELECT a.id,
	t.finished_at IS NOT NULL AND t.finished_at <= r.created_at,
	EXISTS (SELECT 1 FROM tracker_comments b
		WHERE b.task_id = a.task_id AND b.author = a.author AND b.ask = a.ask
		AND b.created_at > a.created_at AND b.created_at < r.created_at)
FROM tracker_comments r
JOIN tracker_comments a ON a.id = r.answers
JOIN tracker_tasks t ON t.id = a.task_id
WHERE r.author_kind = 'human' AND r.created_at >= ? AND r.created_at < ? AND r.removed = 0
	AND a.author_kind = 'agent' AND a.ask <> ''
ORDER BY r.created_at, r.id`

func readAnswers(ctx context.Context, tx *sql.Tx, since, until int64, out *WorkPatterns) error {
	rows, err := tx.QueryContext(ctx, patternAnswersStatement, since, until)
	if err != nil {
		return fmt.Errorf("worklet: read the window's answers: %w", err)
	}
	defer closeRows(rows)
	out.Overtaken = []string{}
	for rows.Next() {
		var ask string
		var moot, superseded bool
		if err := rows.Scan(&ask, &moot, &superseded); err != nil {
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
	if err := rows.Err(); err != nil {
		return fmt.Errorf("worklet: read the window's answers: %w", err)
	}
	return nil
}
