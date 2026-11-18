package worklet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The asks put to one person and closed recently (answered, by anyone, or
// resolved without an answer): the other half of [Reader.Decisions], which
// lists the open ones. Both read an ask through [askRowsOf], so they describe
// it identically. What closed it is the answer comment its `answered_by`
// names (author, instant, chosen option), or its own `resolved_by` and
// `resolved_at`.
//
// Bounded by [AnsweredWindow] (an older answer is read on its task's thread)
// and by [MaxAnswered] rows, [AnsweredAnswer.Truncated] saying the window held
// more. [AnsweredAsk.FollowedUp] is the tracker's fact of what the asker did
// next; whether it is working on the task now is live seat state, read from
// the overview rather than from a copy here that would be stale.

// AnsweredWindow is how far back the answered read reaches: seven days, so a
// person back from a long weekend still finds what they decided before it,
// while the list stays recent decisions rather than the company's history.
const AnsweredWindow = 7 * 24 * time.Hour

// MaxAnswered bounds one page of answered asks: [MaxDecisions], the ceiling
// every listing page in this API shares.
const MaxAnswered = MaxDecisions

// AskAnswer is what closed an ask.
type AskAnswer struct {
	// By is who answered, or who resolved it: a record name, which is not
	// always the person the ask was put to.
	By     string     `json:"by"`
	ByKind AuthorKind `json:"by_kind,omitempty"`
	At     time.Time  `json:"at"`

	// Comment is the answer's own comment id; absent for an ask resolved
	// without one.
	Comment string `json:"comment,omitempty"`

	// Choice is the option the answer chose, by id, for an ask that
	// offered options and an answer that chose one; Body is the answer's
	// words either way (the option's label where nothing else was written).
	Choice string `json:"choice,omitempty"`
	Body   string `json:"body"`

	// Resolved says the ask was closed without an answer: by its asker, who
	// alone resolves a question, withdrawing it.
	Resolved bool `json:"resolved,omitempty"`

	// Withdrawn is why the asker withdrew it and what covers it now, where
	// the withdrawal recorded them ([Withdrawal]).
	Withdrawn *Withdrawn `json:"withdrawn,omitempty"`
}

// AskFollowUp is the asker's first record on the task after the ask closed.
type AskFollowUp struct {
	// At is the AUTHORED instant, as every displayed instant of the
	// history is ([ActivityRecord.At]).
	At        time.Time        `json:"at"`
	Kind      ChangeKind       `json:"kind"`
	Excerpt   string           `json:"excerpt,omitempty"`
	Fields    map[string]Delta `json:"fields,omitempty"`
	CommentID string           `json:"comment_id,omitempty"`
}

// AnsweredAsk is one closed ask.
type AnsweredAsk struct {
	// At is when it was closed: [AskAnswer.At], beside the ask so a list
	// of them sorts and draws on one field, as `decisions` does.
	At     time.Time `json:"at"`
	Ask    AskRow    `json:"ask"`
	Answer AskAnswer `json:"answer"`

	// FollowedUp is absent while the asker has written nothing on the task
	// since.
	FollowedUp *AskFollowUp `json:"followed_up,omitempty"`
}

// AnsweredAnswer is the page of asks closed within the window.
type AnsweredAnswer struct {
	Asks []AnsweredAsk `json:"asks"`

	// Since is the start of the window the page covers.
	Since time.Time `json:"since"`

	// Truncated says the window held more than the page.
	Truncated bool `json:"truncated,omitempty"`

	Level          statelog.ReadLevel `json:"read_level,omitempty"`
	LogSeq         uint64             `json:"log_seq,omitempty"`
	AppliedThrough uint64             `json:"applied_through,omitempty"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// answeredAsksFrom and answeredAsksWhere are the closed asks put to one
// person, bound to their record name. The predicate is
// `tracker_comments_answered_asks_idx`'s term for term so the planner uses it
// (TestTheAnsweredReadSearchesItsIndex): `c.ask <> ”` stays beside
// `c.ask = ?` because a bound parameter proves nothing about the empty string.
const (
	answeredAsksFrom = `tracker_comments c
		JOIN tracker_tasks t ON t.id = c.task_id
		LEFT JOIN tracker_comments a ON a.id = c.answered_by`
	answeredAsksWhere = `c.ask = ? AND c.ask <> '' AND c.removed = 0
		  AND (c.resolved = 1 OR c.answered_by IS NOT NULL)
		  AND t.removed_at IS NULL`
	// answeredAt is when an ask closed: its answer's instant, or the
	// resolution's for one resolved without an answer.
	answeredAt = `COALESCE(a.created_at, c.resolved_at)`
)

// answeredStatement is the page: the closed asks whose closing falls in the
// window, newest first, the comment id breaking a tie so a page is stable.
const answeredStatement = `
	SELECT c.id, c.task_id, c.author, c.body, c.created_at, c.document,
	       c.resolved, c.resolved_by, c.resolved_at, c.record_id,
	       a.id, a.author, a.author_kind, a.body, a.created_at, a.document,
	       a.record_id
	FROM ` + answeredAsksFrom + `
	WHERE ` + answeredAsksWhere + `
	  AND ` + answeredAt + ` >= ?
	ORDER BY ` + answeredAt + ` DESC, c.id
	LIMIT ?`

// followUpStatement is the asker's first record on the task after the commit
// that closed the ask, in log order: two writers' authored instants can tie or
// run backwards. That commit is named by its record, the history row's key, so
// it is one primary-key read even for a commit that woke nobody (only a loud
// one names its comment). For an answer it is the answer row's `record_id`,
// which an edit leaves alone; for an ask resolved without one, the record
// [resolvedStatement] finds.
const followUpStatement = `
	SELECT h.created_at, h.kind, h.excerpt, h.fields_json, h.comment_id
	FROM tracker_history h
	WHERE h.subject_id = ? AND h.subject_kind = 'task' AND h.actor = ?
	  AND h.log_seq > (SELECT x.log_seq FROM tracker_history x WHERE x.id = ?)
	ORDER BY h.log_seq
	LIMIT 1`

// resolvedStatement is every record on one task that resolved a remark after
// the ask's own post, newest first, each with its mutation; [readResolve] takes
// the first naming the ask as the commit that closed it. Read from the history
// because no row says where a resolve landed: [resolveComment] writes only the
// resolution, and the ask's `record_id` is its post.
const resolvedStatement = `
	SELECT h.id, h.document
	FROM tracker_history h
	WHERE h.subject_id = ? AND h.subject_kind = 'task' AND h.kind = ?
	  AND h.log_seq > (SELECT x.log_seq FROM tracker_history x WHERE x.id = ?)
	ORDER BY h.log_seq DESC`

// AnsweredAsks answers the asks put to one person and closed within
// [AnsweredWindow] of now.
func (r *Reader) AnsweredAsks(ctx context.Context, q DecisionsQuery, now time.Time,
	loc *time.Location) (AnsweredAnswer, error) {

	switch {
	case q.Level == "":
		return AnsweredAnswer{}, fmt.Errorf("worklet: this answered read names " +
			"no level: a surface resolves an absent read_level to its own " +
			"default before it reads")
	case q.Handle == "":
		return AnsweredAnswer{}, invalid("an answered read names nobody")
	}
	out := AnsweredAnswer{Since: now.Add(-AnsweredWindow).UTC()}
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// THE DOMAIN, for [Reader.Decisions]' reason: an ask can be on any
		// task in any container.
		Scope:     statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session:   q.Session,
		Floors:    q.Floors,
		MaxLag:    q.MaxLag,
		MaxLagSeq: q.MaxLagSeq,
		Set:       true,
	}, func(tx *sql.Tx) error {
		anchor, err := period.Resolve("today", now, loc)
		if err != nil {
			return err
		}
		if out.Asks, out.Truncated, err = readAnswered(ctx, tx, q.Handle, out.Since,
			anchor.At); err != nil {
			return err
		}
		out.LogSeq, out.AppliedThrough, err = readCheckpoint(ctx, tx)
		return err
	})
	if err != nil {
		return AnsweredAnswer{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	out.LogLag = served.Lag
	if served.Incomplete != nil {
		out.Incomplete = incompleteFrom(served.Incomplete)
	}
	return out, nil
}

// readAnswered reads one page of the closed asks put to this person.
func readAnswered(ctx context.Context, tx *sql.Tx, handle string, since,
	dayStart time.Time) ([]AnsweredAsk, bool, error) {

	rows, err := tx.QueryContext(ctx, answeredStatement, handle,
		store.EncodeTime(since), MaxAnswered+1)
	if err != nil {
		return nil, false, fmt.Errorf("worklet: read the asks %s answered: %w", handle, err)
	}
	defer func() { _ = rows.Close() }()

	type closing struct {
		resolved   bool
		resolvedBy sql.NullString
		resolvedAt sql.NullInt64
		askRecord  string
		id, author sql.NullString
		authorKind sql.NullString
		body       sql.NullString
		at         sql.NullInt64
		document   []byte
		record     sql.NullString
	}
	var pending []askRecord
	var closings []closing
	for rows.Next() {
		var a askRecord
		var c closing
		var resolved int
		if err = rows.Scan(&a.comment, &a.task, &a.author, &a.body, &a.at, &a.document,
			&resolved, &c.resolvedBy, &c.resolvedAt, &c.askRecord,
			&c.id, &c.author, &c.authorKind, &c.body, &c.at, &c.document,
			&c.record); err != nil {
			return nil, false, fmt.Errorf("worklet: scan an answered ask: %w", err)
		}
		c.resolved = resolved == 1
		pending = append(pending, a)
		closings = append(closings, c)
	}
	if err = rows.Err(); err != nil {
		return nil, false, fmt.Errorf("worklet: read the asks %s answered: %w", handle, err)
	}
	truncated := len(pending) > MaxAnswered
	if truncated {
		pending, closings = pending[:MaxAnswered], closings[:MaxAnswered]
	}
	asks, err := askRowsOf(ctx, tx, pending, dayStart, 0, false)
	if err != nil {
		return nil, false, err
	}
	byComment := make(map[string]AskRow, len(asks))
	for _, ask := range asks {
		byComment[ask.Comment] = ask
	}

	out := make([]AnsweredAsk, 0, len(asks))
	for i, rec := range pending {
		ask, held := byComment[rec.comment]
		if !held {
			continue
		}
		c := closings[i]
		var answer AskAnswer
		var closedBy string
		if c.id.Valid {
			answer = AskAnswer{
				By: c.author.String, ByKind: AuthorKind(c.authorKind.String),
				At: store.DecodeTime(c.at.Int64), Comment: c.id.String,
				Body: c.body.String,
			}
			if len(c.document) > 0 {
				var stored Comment
				if err := json.Unmarshal(c.document, &stored); err != nil {
					return nil, false, fmt.Errorf("worklet: decode the answer %s: %w",
						c.id.String, err)
				}
				answer.Choice = stored.Choice
			}
			closedBy = c.record.String
		} else {
			answer = AskAnswer{By: c.resolvedBy.String, At: store.DecodeTime(c.resolvedAt.Int64),
				Resolved: true}
			withdrawn, werr := withdrawnFrom(ctx, tx, rec)
			if werr != nil {
				return nil, false, werr
			}
			answer.Withdrawn = withdrawn
			var resolveErr error
			if closedBy, resolveErr = readResolve(ctx, tx, rec.task, rec.comment,
				c.askRecord); resolveErr != nil {
				return nil, false, resolveErr
			}
		}
		followUp, err := readFollowUp(ctx, tx, rec.task, rec.author, closedBy)
		if err != nil {
			return nil, false, err
		}
		out = append(out, AnsweredAsk{At: answer.At, Ask: ask, Answer: answer,
			FollowedUp: followUp})
	}
	return out, truncated, nil
}

// withdrawnFrom is how the withdrawal an ask's stored document records reads,
// nil for an ask withdrawn without one.
func withdrawnFrom(ctx context.Context, tx *sql.Tx, ask askRecord) (*Withdrawn, error) {
	if len(ask.document) == 0 {
		return nil, nil
	}
	var stored Comment
	if err := json.Unmarshal(ask.document, &stored); err != nil {
		return nil, fmt.Errorf("worklet: decode the ask %s: %w", ask.comment, err)
	}
	return readWithdrawn(ctx, tx, ask.task, stored.Withdrawal)
}

// readResolve answers the newest record on the task that resolved this ask
// after the record that posted it (posted), or "" when there is none. A row
// whose mutation does not decode names no remark rather than failing the read,
// for [statedHandOffs]' reason: where a resolve landed is shown, never a reason
// to refuse the answer.
func readResolve(ctx context.Context, tx *sql.Tx, task, ask, posted string) (string, error) {
	rows, err := tx.QueryContext(ctx, resolvedStatement, task,
		string(ChangeCommentResolved), posted)
	if err != nil {
		return "", fmt.Errorf("worklet: read what resolved the ask %s: %w", ask, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var document []byte
		if err = rows.Scan(&id, &document); err != nil {
			return "", fmt.Errorf("worklet: scan what resolved the ask %s: %w", ask, err)
		}
		var stated struct {
			Comment *struct {
				ID string `json:"id"`
			} `json:"comment"`
		}
		if json.Unmarshal(document, &stated) == nil && stated.Comment != nil &&
			stated.Comment.ID == ask {
			return id, nil
		}
	}
	if err = rows.Err(); err != nil {
		return "", fmt.Errorf("worklet: read what resolved the ask %s: %w", ask, err)
	}
	return "", nil
}

// readFollowUp reads the asker's first record on the task after the commit
// closedBy names (its history row's key), or nil when there is none yet.
func readFollowUp(ctx context.Context, tx *sql.Tx, task, asker,
	closedBy string) (*AskFollowUp, error) {

	var (
		authored  int64
		kind      string
		excerpt   string
		fields    string
		commentID string
	)
	err := tx.QueryRowContext(ctx, followUpStatement, task, asker, closedBy).
		Scan(&authored, &kind, &excerpt, &fields, &commentID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("worklet: read what %s did on %s after its answer: %w",
			asker, task, err)
	}
	follow := &AskFollowUp{At: store.DecodeTime(authored), Kind: ChangeKind(kind),
		Excerpt: excerpt, CommentID: commentID}
	if fields != "" && fields != "{}" {
		// A ROW WHOSE DELTAS DO NOT DECODE IS STILL A ROW, for the
		// activity feed's reason: the asker did act.
		if err := json.Unmarshal([]byte(fields), &follow.Fields); err != nil {
			follow.Fields = nil
		}
	}
	return follow, nil
}
