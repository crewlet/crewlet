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

// The asks put to one person that were closed recently: answered, by them or
// by somebody else, or resolved without an answer. It is the other half of
// [Reader.Decisions], which lists what is still open: an ask leaves that list
// at the read after its answer, and without this one a person could not see
// what they decided last Tuesday, or that a colleague answered a question put
// to them.
//
// # The same rows, closed
//
// An ask is the comment that asked it (`tracker_comments.ask`), read through
// the same function as an open one ([askRowsOf]), so the two lists describe an
// ask identically. What closes it is the answer comment that names it
// (the ask's `answered_by` holds its id), whose author and instant are who
// answered and when, and whose document holds the option chosen; or, for an
// ask resolved without an answer, the ask's own `resolved_by` and
// `resolved_at`.
//
// # Bounded twice
//
// By [AnsweredWindow], because this is "what did I decide lately" rather than
// an archive: every answer stays on its task's thread for good, and the
// thread is where an older one is read. And by [MaxAnswered] rows, the page
// every listing in this API shares, with [AnsweredAnswer.Truncated] saying the
// window held more.
//
// # What the asker did next
//
// [AnsweredAsk.FollowedUp] is the asker's first record on the task after the
// commit that closed the ask: the change the answer led to, or at least the
// sign it was read. It is the tracker's fact. Whether the asker is working on
// the task right now, or is paused, is live seat state, which the overview
// already pushes; a surface reads it there rather than from a copy here that
// would be stale by the time it was drawn.

// AnsweredWindow is how far back the answered read reaches.
//
// SEVEN DAYS, a working week and its weekend: long enough that a person back
// from a long weekend still finds what they decided before it, and short
// enough that the list stays a list of recent decisions rather than the
// company's history. An older answer is on its task's thread.
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

	// Resolved says the ask was closed without an answer.
	Resolved bool `json:"resolved,omitempty"`
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
// person, bound to their record name as the one argument.
//
// THE PREDICATE IS THE INDEX'S, term for term, so the planner proves the
// implication and searches `tracker_comments_answered_asks_idx` (asserted by
// TestTheAnsweredReadSearchesItsIndex): `c.ask <> ”` is stated even beside
// `c.ask = ?`, because a bound parameter proves nothing about the empty
// string.
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
// that closed the ask.
//
// THAT COMMIT IS NAMED BY ITS RECORD, the history row's key, so the position
// is one primary-key read whether or not the commit woke anybody (only a loud
// commit names its comment in the history). For an answer it is the record
// that posted it, which the answer's row carries (`record_id`, which an edit
// leaves alone, so an answer reworded later still closed its ask when it was
// posted). For an ask resolved without one it is the newest record that
// resolved it, which no row carries ([resolvedStatement]).
//
// IN LOG ORDER, not by clock: "after" is the position, which is a total order
// over the task's commits, where two writers' authored instants can tie or run
// backwards.
const followUpStatement = `
	SELECT h.created_at, h.kind, h.excerpt, h.fields_json, h.comment_id
	FROM tracker_history h
	WHERE h.subject_id = ? AND h.subject_kind = 'task' AND h.actor = ?
	  AND h.log_seq > (SELECT x.log_seq FROM tracker_history x WHERE x.id = ?)
	ORDER BY h.log_seq
	LIMIT 1`

// resolvedStatement is every record on one task that resolved a remark after
// a commit, newest first, each with its mutation: the commit that closed an
// ask resolved without an answer is the newest of them about that ask.
//
// READ FROM THE HISTORY because no row says where a resolve landed: it writes
// the resolution and nothing else ([resolveComment]), and the ask's own
// `record_id` is the record that posted it. A history row keeps its record's
// mutation, and a resolve's names the remark it resolved, so the rows are read
// newest first until one names this ask ([readResolve]). After the ask's own
// post, since nothing resolved before it was posted closed it.
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
		Scope:       statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		MaxLagSeq:   q.MaxLagSeq,
		Set:         true,
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

// readResolve answers the newest record on the task that resolved this ask
// after the record that posted it (posted), or none where the history holds
// no such record.
//
// A ROW WHOSE MUTATION DOES NOT DECODE NAMES NO REMARK rather than failing the
// read, for [statedHandOffs]' reason: its record decoded once already, when it
// was applied, and where a resolve landed is what the answer shows, never a
// reason to refuse it.
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
