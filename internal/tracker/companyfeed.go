package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The tracker's half of the company feed — the completions, the creates and
// the hand-offs, newest first, as `company_feed` merges them with the pages'
// changes and the schedules' runs.
//
// # Ordered by WHEN, not by the log
//
// The activity feed ([Reader.Activity]) is ordered by log position because it
// is one log's account of itself. This page is merged with two other domains'
// rows, and the only order three logs share is a clock — so it is ordered by
// `effective_at`, the fleet-agreed instant, with the position breaking a tie,
// and its cursor is that pair. `effective_at` is only ever RAISED (a late
// record lifts its successors to its own instant, never lowers anything), so a
// row not yet paged can move above a cursor already handed out and be missed
// by that one reader's scroll; nothing can drop below it and repeat.
//
// # One classification per row
//
// A single commit can create a task already done, or finish one while handing
// it on. A row is filed under the FIRST of the kinds the caller asked for, in
// the order created → completed → handoff: a create is the most specific thing
// that happened to a task, and a completion outranks the reassignment that
// came with it. A kind the caller did not ask for never hides a row that
// matches one they did.

// FeedKind is one kind of company-feed row this domain answers.
type FeedKind string

// The three tracker kinds, in classification order.
const (
	FeedCreated   FeedKind = "created"
	FeedCompleted FeedKind = "completed"
	FeedHandoff   FeedKind = "handoff"
)

// FeedKinds are the three, in the order a row is classified.
var FeedKinds = []FeedKind{FeedCreated, FeedCompleted, FeedHandoff}

// Valid reports whether a kind off the wire is one this domain answers.
func (k FeedKind) Valid() bool { return slices.Contains(FeedKinds, k) }

// Origin is where a task was filed from: the chat surface a seat's turn was
// woken on, and the conversation on it.
//
// STATED BY THE WRITER on the create record ([TaskCreate.Origin]), because it
// is a fact about the turn that filed the task and the turn is gone by the time
// anything reads it. Absent for a task filed by anything that was not woken on
// a chat surface — a schedule, an assignment, a person at the dashboard — and
// on every create a build before record version 9 wrote.
type Origin struct {
	// Surface is the chat backend's transport name (`slack`, `mattermost`)
	// — the value [notify.TransportField] carries.
	Surface string `json:"surface"`

	// Conversation is the durable conversation identity the turn served.
	Conversation string `json:"conversation,omitempty"`
}

// FeedCursor is where one reader's scroll stopped: the instant and position
// of the last row it was handed.
type FeedCursor struct {
	At  time.Time
	Seq uint64
}

// String renders the cursor as `<unix micros>:<log seq>`.
func (c FeedCursor) String() string {
	return strconv.FormatInt(store.EncodeTime(c.At), 10) + ":" +
		strconv.FormatUint(c.Seq, 10)
}

// ParseFeedCursor reads [FeedCursor.String] back.
func ParseFeedCursor(raw string) (FeedCursor, error) {
	at, seq, ok := strings.Cut(raw, ":")
	micros, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil {
		return FeedCursor{}, invalid("%q is not a feed cursor", raw)
	}
	position, err := strconv.ParseUint(seq, 10, 64)
	if err != nil {
		return FeedCursor{}, invalid("%q is not a feed cursor", raw)
	}
	return FeedCursor{At: store.DecodeTime(micros), Seq: position}, nil
}

// MaxFeedPage bounds one page of any company-feed source.
const MaxFeedPage = 50

// FeedQuery asks for one page.
type FeedQuery struct {
	// Kinds narrows to these; empty is all three.
	Kinds []FeedKind

	// Actor narrows to one writer: the name a commit was made under — a
	// seat (a person bound to one writes AS it, see iam.ActorFor), or the
	// login of somebody bound to none.
	Actor string

	// Before resumes a scroll; nil is the newest page.
	Before *FeedCursor

	// Limit is 1..[MaxFeedPage].
	Limit int

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration
	MaxLagSeq   uint64
}

// FeedSpend is what a completed task cost: its running totals as they stand,
// tokens only.
type FeedSpend struct {
	Tokens int `json:"tokens"`
	Turns  int `json:"turns"`
}

// FeedRow is one tracker row of the company feed.
type FeedRow struct {
	ID     string     `json:"id"`
	Kind   FeedKind   `json:"kind"`
	At     time.Time  `json:"at"`
	Cursor string     `json:"cursor"`
	Actor  string     `json:"actor,omitempty"`
	ActorK AuthorKind `json:"actor_kind,omitempty"`

	Task string `json:"task"`
	Key  string `json:"key,omitempty"`

	// KeyCollision says [FeedRow.Key] does not open [FeedRow.Task]: the key
	// directory names another task for it, so a link built from the key
	// reaches THAT task and this row's is reached by its id. See
	// [ItemAddress].
	KeyCollision bool `json:"key_collision,omitempty"`

	Title   string `json:"title,omitempty"`
	Project string `json:"project,omitempty"`

	// Spend and FirstPass ride a completion: the task's running totals,
	// and whether it finished without a reviewer ever sending it back —
	// absent when no turn was ever charged to it, which says nothing
	// about review either way.
	Spend     *FeedSpend `json:"spend,omitempty"`
	FirstPass *bool      `json:"first_pass,omitempty"`

	// Origin rides a create — see [Origin].
	Origin *Origin `json:"origin,omitempty"`

	// From, To, Reassignments and ReassignmentBudget ride a hand-off: who
	// held the task and who holds it now, the task's hand-off counter as
	// that commit left it, and the budget it counts against.
	From               string `json:"from,omitempty"`
	To                 string `json:"to,omitempty"`
	Reassignments      *int   `json:"reassignments,omitempty"`
	ReassignmentBudget int    `json:"reassignment_budget,omitempty"`
}

// FeedPage is one page, with the framework's verdict on the read.
type FeedPage struct {
	Rows []FeedRow `json:"rows"`

	// More says the source holds rows past this page.
	More bool `json:"more"`

	Level      statelog.ReadLevel `json:"read_level,omitempty"`
	LogLag     *uint64            `json:"log_lag,omitempty"`
	Complete   bool               `json:"complete"`
	Incomplete *Incomplete        `json:"incomplete,omitempty"`
}

// deliveredSQL is [Delivered]'s statuses as a quoted IN list, rendered from
// the status table rather than typed again — see [openGroupsSQL].
var deliveredSQL = func() string {
	out := make([]string, 0, len(Statuses))
	for _, s := range Statuses {
		if Delivered(s) {
			out = append(out, "'"+string(s)+"'")
		}
	}
	return strings.Join(out, ",")
}()

// The three kinds as SQL, each over the history row `h`.
var feedKindSQL = map[FeedKind]string{
	FeedCreated: `h.kind = 'created'`,
	FeedCompleted: `json_extract(h.fields_json, '$.status.to') IN (` + deliveredSQL + `)
		AND COALESCE(json_extract(h.fields_json, '$.status.from'), '') NOT IN (` + deliveredSQL + `)`,
	FeedHandoff: `COALESCE(json_extract(h.fields_json, '$.assignee.from'), '') <> ''
		AND COALESCE(json_extract(h.fields_json, '$.assignee.to'), '') <> ''`,
}

// CompanyFeed answers one page of the tracker's half of the company feed.
func (r *Reader) CompanyFeed(ctx context.Context, q FeedQuery) (FeedPage, error) {
	switch {
	case q.Level == "":
		return FeedPage{}, fmt.Errorf("tracker: this feed read names no level " +
			"— a surface resolves an absent read_level to its own default " +
			"before it reads")
	case q.Limit < 1 || q.Limit > MaxFeedPage:
		return FeedPage{}, invalid("a feed page is 1 to %d rows, not %d",
			MaxFeedPage, q.Limit)
	}
	kinds := q.Kinds
	if len(kinds) == 0 {
		kinds = FeedKinds
	}
	for _, kind := range kinds {
		if !kind.Valid() {
			return FeedPage{}, invalid("%q is not a feed kind; the "+
				"tracker answers %v", kind, FeedKinds)
		}
	}
	var out FeedPage
	served, err := r.log.Read(ctx, statelog.Query{
		Level:       q.Level,
		Scope:       statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		MaxLagSeq:   q.MaxLagSeq,
		Set:         true,
	}, func(tx *sql.Tx) error {
		return readCompanyFeed(ctx, tx, q, kinds, &out)
	})
	if err != nil {
		return FeedPage{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	out.LogLag = served.Lag
	if served.Incomplete != nil {
		out.Incomplete = incompleteFrom(served.Incomplete)
	}
	return out, nil
}

func readCompanyFeed(ctx context.Context, tx *sql.Tx, q FeedQuery,
	kinds []FeedKind, out *FeedPage) error {

	statement, args := companyFeedStatement(q, kinds)
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return fmt.Errorf("tracker: read the company feed: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out.Rows = []FeedRow{}
	for rows.Next() {
		var (
			row                     FeedRow
			kind, fields, actorKind string
			effective               int64
			seq                     uint64
			reassignments           sql.NullInt64
			document                []byte
			tokens, turns, sentBack sql.NullInt64
			collision               int
		)
		if err = rows.Scan(&row.ID, &row.Task, &kind, &fields, &effective, &seq,
			&row.Actor, &actorKind, &reassignments, &document,
			&row.Key, &collision, &row.Title, &row.Project, &tokens,
			&turns, &sentBack); err != nil {
			return fmt.Errorf("tracker: scan a company-feed row: %w", err)
		}
		row.ActorK = AuthorKind(actorKind)
		row.KeyCollision = collision == 1
		row.At = store.DecodeTime(effective)
		row.Cursor = FeedCursor{At: row.At, Seq: seq}.String()
		moved := historyDeltas(fields)
		row.Kind = classifyFeedRow(ChangeKind(kind), moved, kinds)
		switch row.Kind {
		case FeedCreated:
			row.Origin = originOf(document)
		case FeedCompleted:
			if tokens.Valid && turns.Int64 > 0 {
				row.Spend = &FeedSpend{Tokens: int(tokens.Int64), Turns: int(turns.Int64)}
				first := sentBack.Int64 == 0
				row.FirstPass = &first
			}
		case FeedHandoff:
			row.From, row.To = moved["assignee"].From, moved["assignee"].To
			if reassignments.Valid {
				n := int(reassignments.Int64)
				row.Reassignments = &n
			}
			row.ReassignmentBudget = ReassignmentBudget
		}
		out.Rows = append(out.Rows, row)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("tracker: walk the company feed: %w", err)
	}
	if len(out.Rows) > q.Limit {
		out.Rows = out.Rows[:q.Limit]
		out.More = true
	}
	return nil
}

// companyFeedStatement is the one statement a feed page runs, built apart from
// the read so the plan test explains exactly what the reader issues.
func companyFeedStatement(q FeedQuery, kinds []FeedKind) (string, []any) {
	clauses := make([]string, 0, len(kinds))
	for _, kind := range FeedKinds {
		if slices.Contains(kinds, kind) {
			clauses = append(clauses, "("+feedKindSQL[kind]+")")
		}
	}
	where := historyMoves("h.")
	where += " AND (" + strings.Join(clauses, " OR ") + ")"
	var args []any
	if q.Before != nil {
		at := store.EncodeTime(q.Before.At)
		where += " AND h.effective_at <= ? AND NOT (h.effective_at = ? AND h.log_seq >= ?)"
		args = append(args, at, at, q.Before.Seq)
	}
	if actor := strings.TrimSpace(q.Actor); actor != "" {
		where += " AND h.actor = ?"
		args = append(args, actor)
	}
	args = append(args, q.Limit+1)
	return `
		SELECT h.id, h.subject_id, h.kind, h.fields_json, h.effective_at, h.log_seq,
		       h.actor, h.actor_kind, h.reassignments, h.document,
		       COALESCE(t.key, d.task_key, ''),
		       COALESCE(t.key_collision,
		                ` + keyOpensAnother("d.task_key", "h.subject_id") + `, 0),
		       COALESCE(t.title, ''),
		       COALESCE(t.project_key, d.project_key, h.project_key),
		       t.spend_tokens, t.spend_turns, t.spend_sent_back
		FROM tracker_history h
		LEFT JOIN tracker_tasks t ON t.id = h.subject_id
		LEFT JOIN tracker_deletions d ON d.task_id = h.subject_id
		WHERE ` + where + `
		ORDER BY h.effective_at DESC, h.log_seq DESC
		LIMIT ?`, args
}

// classifyFeedRow files a row under the first requested kind it is, in
// [FeedKinds] order. The SQL selected it for matching at least one, so the
// fallback is never reached for a row the query returned.
func classifyFeedRow(kind ChangeKind, moved map[string]Delta, asked []FeedKind) FeedKind {
	for _, candidate := range FeedKinds {
		if !slices.Contains(asked, candidate) {
			continue
		}
		switch candidate {
		case FeedCreated:
			if kind == ChangeCreated {
				return candidate
			}
		case FeedCompleted:
			if d, ok := moved["status"]; ok &&
				!Delivered(Status(d.From)) && Delivered(Status(d.To)) {
				return candidate
			}
		case FeedHandoff:
			if d, ok := moved["assignee"]; ok && d.From != "" && d.To != "" {
				return candidate
			}
		}
	}
	return asked[0]
}

// originOf reads a create's origin off the record it was applied from. A
// document that does not decode, or a create that stated none, has none.
func originOf(document []byte) *Origin {
	var create struct {
		Origin *Origin `json:"origin"`
	}
	if err := json.Unmarshal(document, &create); err != nil || create.Origin == nil ||
		create.Origin.Surface == "" {
		return nil
	}
	return create.Origin
}
