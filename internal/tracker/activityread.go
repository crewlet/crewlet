package tracker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The activity feed — every commit this company made, in the order the log
// made them.
//
// # ONE DURABLE TABLE AT ANY AGE
//
// `tracker_history` holds one row per applied commit, quiet ones included: a
// "quiet" commit is one that woke nobody, not one that did not happen, and a
// feed assembled from the notifications would be an account of what was
// ANNOUNCED rather than of what was done. Nothing is swept, so a question
// about last week and a question about three years ago are the same query
// against the same table, with no live/archive boundary to cross.
//
// # THE ORDER IS THE LOG'S, and so is the cursor
//
// Rows are ordered by the COMPOSED POSITION — `(generation << 40) | seq` — not
// by any clock. That is a total order over exactly the records this question
// returns, which a clock is not: two nodes' authored instants can tie, can run
// backwards, and say nothing about which commit the broker accepted first.
//
// So `since:` and the page cursor are both positions, rendered as the
// `stream@generation:seq` triple the log itself uses. The generation is in the
// ordering rather than beside it, which is what lets a cursor SPAN A REANCHOR
// with no gap and no repeat — a sequence from a previous generation is below
// every sequence in the current one however large it is.
//
// # The instants it RENDERS are the authored ones
//
// Every displayed instant here is what the writer typed (D113): a person
// filtering "since Monday" means their Monday, and a card saying "yesterday"
// must not move because a record was redelivered. The EFFECTIVE instant is
// carried too, because that is what every duration and every report window is
// measured on — the two are different facts and the answer names both.

// ActivityRecord is one commit as the feed renders it.
type ActivityRecord struct {
	ID string `json:"id"`

	// The composed position, and its three parts. The triple is what a
	// caller stores: a bare sequence names no stream and no generation,
	// so a cursor built from one cannot survive a reanchor.
	LogSeq        uint64 `json:"log_seq"`
	LogStream     string `json:"log_stream"`
	LogGeneration uint32 `json:"log_generation"`

	// At is the AUTHORED instant — what the writer's own clock said — and
	// EffectiveAt is the fleet-agreed one every duration is measured on.
	// Both, because they are different facts and a surface that carried
	// one of them silently answers a different question than it looks like.
	At          time.Time `json:"at"`
	EffectiveAt time.Time `json:"effective_at"`

	Kind       ChangeKind `json:"kind"`
	Actor      string     `json:"actor,omitempty"`
	ActorKind  AuthorKind `json:"actor_kind,omitempty"`
	OperatorID string     `json:"operator_id,omitempty"`

	SubjectKind ObjectKind `json:"subject_kind"`
	SubjectID   string     `json:"subject_id"`

	// SubjectKey is the human-readable key of the task a row is about,
	// resolved here because a feed of uuids is a feed nobody reads. Empty
	// for a subject that has no key — a project, a view, a person.
	SubjectKey string `json:"subject_key,omitempty"`
	Project    string `json:"project,omitempty"`

	Excerpt   string           `json:"excerpt,omitempty"`
	Fields    map[string]Delta `json:"fields,omitempty"`
	CommentID string           `json:"comment_id,omitempty"`
	BatchID   string           `json:"batch_id,omitempty"`
	TurnID    string           `json:"turn_id,omitempty"`

	// Notified says whether the commit carried a notification — whether
	// this change was ANNOUNCED. It is how a reader tells "nothing was
	// announced" from "nothing happened", which is the whole reason a
	// quiet commit still writes a row.
	//
	// IT IS DELIBERATELY NOT "SOMEBODY WAS WOKEN", and the applier could
	// not answer that even if the column meant it: who was actually told
	// is [Route]'s answer, and Route needs the company's CURRENT roster —
	// which the applier deliberately does not hold, because two nodes
	// briefly on different epochs would then write different rows for one
	// record. An announced change can reach nobody: every candidate may
	// be the actor, or may have left.
	Notified bool `json:"notified"`

	// Late marks a record the broker accepted well after it was authored.
	Late bool `json:"late,omitempty"`
}

// ActivityAnswer is the feed, with the framework's own verdict on the read.
type ActivityAnswer struct {
	Records []ActivityRecord `json:"records"`

	// NextCursor resumes exactly after the last row, as a log position.
	NextCursor string `json:"next_cursor,omitempty"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// MaxActivityRows is how many commits one page carries.
const MaxActivityRows = 200

// ActivityQuery asks for a slice of the feed.
type ActivityQuery struct {
	// Task narrows to one subject — a task id or any of its keys, former
	// ones included.
	Task string

	// Project narrows to one container. Empty with Workspace false is the
	// whole company, which is a valid question for every filter but `q`.
	Project   string
	Workspace bool

	// Since is a lower bound. EITHER a log position or an authored
	// instant, never both: a position is the total order this feed is in,
	// and an instant is what a person types.
	Since   statelog.Position
	SinceAt time.Time

	// From and To bound the AUTHORED instants, which is what a caller
	// typing a wall-clock range means (D113).
	From time.Time
	To   time.Time

	Kinds    []ChangeKind
	Actor    string
	Assignee string

	// Q is an escaped LIKE over the excerpt, and is GATED — see
	// [ActivityQuerySpanDays].
	Q string

	// Notified narrows to what was or was not ANNOUNCED — see
	// [ActivityRecord.Notified]. Not to what woke somebody: a change can
	// be announced and still reach nobody.
	Notified *bool

	Batch string

	Limit  int
	Cursor string

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration

	// MaxLagSeq is the same bound counted in RECORDS, which is what
	// the broker actually answers — the duration above is derived
	// from it through this node's own drain rate. Both may be set
	// and the read refuses past whichever is reached first.
	MaxLagSeq uint64
}

// Activity answers a slice of the company's own history.
func (r *Reader) Activity(ctx context.Context, q ActivityQuery, now time.Time) (
	ActivityAnswer, error) {

	if q.Level == "" {
		return ActivityAnswer{}, fmt.Errorf("tracker: this activity read names " +
			"no level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	if err := gateActivityQuery(q, now); err != nil {
		return ActivityAnswer{}, err
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxActivityRows {
		limit = MaxActivityRows
	}

	var answer ActivityAnswer
	served, err := r.log.Read(ctx, statelog.Query{
		Level:           q.Level,
		Scope:           activityScope(q),
		Session:         q.Session,
		MinPosition:     q.MinPosition,
		MaxLag:          q.MaxLag,
		MaxLagPositions: q.MaxLagSeq,
		Set:             true,
	}, func(tx *sql.Tx) error {
		records, next, err := readActivity(ctx, tx, q, limit)
		if err != nil {
			return err
		}
		answer.Records, answer.NextCursor = records, next
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		answer.LogSeq, answer.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return ActivityAnswer{}, err
	}
	answer.Level = served.Level
	answer.Complete = served.Complete
	answer.LogLag = served.Lag
	if served.Incomplete != nil {
		answer.Incomplete = incompleteFrom(served.Incomplete)
	}
	return answer, nil
}

// gateActivityQuery refuses what this feed cannot serve.
//
// THE GATE IS ON WHAT THE QUERY WOULD SCAN, never on which keys were named.
// Written as "requires `task`, `container` or a `since:` bound" it was a gate
// a caller satisfied in one attempt and learned nothing from:
// `container=workspace&q=` names a container, an unbounded `since:` names a
// bound, and both run the same full scan the gate exists to stop.
func gateActivityQuery(q ActivityQuery, now time.Time) error {
	if strings.TrimSpace(q.Q) == "" {
		return nil
	}
	if strings.TrimSpace(q.Task) != "" {
		// ONE SUBJECT IS AN INDEX RANGE on `(subject_id, log_seq DESC)`,
		// however old, so no window is needed.
		return nil
	}
	if ProjectKey(q.Project) == "" {
		return fmt.Errorf("tracker: searching the activity feed needs a "+
			"subject or a container: pass `task`, or `container=project:<KEY>` "+
			"with a `since` no further back than %d days — at company scope a "+
			"text search reads every commit this company has ever made and "+
			"times out rather than answering", ActivityQuerySpanDays)
	}
	span := activitySpan(q, now)
	if span <= 0 {
		return fmt.Errorf("tracker: searching a project's activity needs a "+
			"`since` bound no further back than %d days — without one this "+
			"reads the project's whole history", ActivityQuerySpanDays)
	}
	if span > ActivityQuerySpanDays*24*time.Hour {
		return fmt.Errorf("tracker: `since` reaches back %d days and a text "+
			"search over the activity feed covers at most %d — narrow the "+
			"window, or drop `q` and filter by `kinds` and `actor`, which are "+
			"indexed", int(span.Hours()/24), ActivityQuerySpanDays)
	}
	return nil
}

// activitySpan is how far back a text search would reach, or zero for unbounded.
//
// A LOG POSITION IS NOT A SPAN. A caller resuming from a cursor has named a
// point in the log rather than a wall-clock bound, and nothing here can say
// how old that point is without reading it — so a position alone does not
// satisfy the window, and the refusal above says which key does.
func activitySpan(q ActivityQuery, now time.Time) time.Duration {
	switch {
	case !q.From.IsZero():
		return now.Sub(q.From)
	case !q.SinceAt.IsZero():
		return now.Sub(q.SinceAt)
	}
	return 0
}

// activityScope is the closure this read is about.
//
// A SUBJECT narrows to that object, a container to that project, and anything
// wider is the domain — honestly, because a feed of the whole company is a
// question every deferred record concerns and the honest answer to "is this
// complete" is then no.
func activityScope(q ActivityQuery) statelog.ScopeSet {
	switch {
	case strings.TrimSpace(q.Task) != "":
		// THE CONTAINER rather than the object, because a row about a
		// task can be written by a record whose subject is the project
		// it sits in — a chart apply, a policy change — and a closure
		// naming only the task would certify a feed complete that one of
		// those was holding.
		if key := ProjectKey(q.Project); key != "" {
			return statelog.ScopeSet{Paths: []string{
				ScopeTerm{Kind: TermContainer, ID: key}.Path(),
			}}.Normalised()
		}
		return statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised()
	case ProjectKey(q.Project) != "":
		return statelog.ScopeSet{Paths: []string{
			ScopeTerm{Kind: TermContainer, ID: ProjectKey(q.Project)}.Path(),
		}}.Normalised()
	}
	return statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised()
}

// readActivity reads one page of the feed, newest first.
func readActivity(ctx context.Context, tx *sql.Tx, q ActivityQuery, limit int) (
	[]ActivityRecord, string, error) {

	where, args, err := compileActivity(ctx, tx, q)
	if err != nil {
		return nil, "", err
	}
	clause := ""
	if len(where) > 0 {
		clause = " WHERE " + joinAnd(where)
	}
	// NEWEST FIRST. A feed is read from the top, and the cursor pages
	// backwards through the log — which is why the keyset is `<` rather
	// than `>` and why it needs no tie-break: a composed position is
	// unique per record by construction.
	rows, err := tx.QueryContext(ctx, `
		SELECT h.id, h.log_seq, h.log_stream, h.log_generation,
		       h.created_at, h.effective_at, h.kind, h.actor, h.actor_kind,
		       h.operator_id, h.subject_kind, h.subject_id, h.project_key,
		       h.excerpt, h.fields_json, h.comment_id, h.batch_id, h.turn_id,
		       h.notified, h.late,
		       (SELECT k.key FROM tracker_tasks k WHERE k.id = h.subject_id)
		FROM tracker_history h`+clause+`
		ORDER BY h.log_seq DESC
		LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, "", fmt.Errorf("tracker: read the activity feed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]ActivityRecord, 0, limit)
	more := false
	for rows.Next() {
		if len(out) == limit {
			// THE EXTRA ROW IS THE CURSOR'S EVIDENCE and never an
			// answer: a page that returned it would overrun the limit
			// the caller asked for.
			more = true
			break
		}
		var record ActivityRecord
		var packed int64
		var authored, effective int64
		var kind, actorKind, subjectKind string
		var fields string
		var batch, key sql.NullString
		var notified, late int
		if err := rows.Scan(&record.ID, &packed, &record.LogStream,
			&record.LogGeneration, &authored, &effective, &kind, &record.Actor,
			&actorKind, &record.OperatorID, &subjectKind, &record.SubjectID,
			&record.Project, &record.Excerpt, &fields, &record.CommentID,
			&batch, &record.TurnID, &notified, &late, &key); err != nil {
			return nil, "", fmt.Errorf("tracker: scan an activity row: %w", err)
		}
		record.LogSeq = uint64(packed) % statelog.GenerationStride
		record.At = store.DecodeTime(authored)
		record.EffectiveAt = store.DecodeTime(effective)
		record.Kind = ChangeKind(kind)
		record.ActorKind = AuthorKind(actorKind)
		record.SubjectKind = ObjectKind(subjectKind)
		record.BatchID = batch.String
		record.SubjectKey = key.String
		record.Notified, record.Late = notified != 0, late != 0
		if fields != "" && fields != "{}" {
			if err := json.Unmarshal([]byte(fields), &record.Fields); err != nil {
				// A ROW WHOSE DELTAS DO NOT DECODE IS STILL A ROW. The
				// commit happened; dropping it because one column is
				// unreadable would make a feed silently skip history.
				record.Fields = nil
			}
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("tracker: read the activity feed: %w", err)
	}
	next := ""
	if more && len(out) > 0 {
		last := out[len(out)-1]
		next = statelog.Position{
			Stream: last.LogStream, Generation: last.LogGeneration,
			Seq: last.LogSeq,
		}.String()
	}
	return out, next, nil
}

// compileActivity turns the query into a predicate.
func compileActivity(ctx context.Context, tx *sql.Tx, q ActivityQuery) (
	[]string, []any, error) {

	var where []string
	var args []any
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}

	if task := strings.TrimSpace(q.Task); task != "" {
		// THE ID, RESOLVED FROM WHATEVER THE CALLER HELD. A feed asked
		// for by a FORMER key must answer the same rows — the history is
		// the one place a rename is most likely to be looked up from.
		id, err := resolveTaskID(ctx, tx, task)
		if err != nil {
			return nil, nil, err
		}
		add("h.subject_id = ?", id)
	}
	if project := ProjectKey(q.Project); project != "" {
		add("h.project_key = ?", project)
	}
	if !q.Since.IsZero() {
		add("h.log_seq > ?", q.Since.Packed())
	}
	if !q.SinceAt.IsZero() {
		add("h.created_at >= ?", store.EncodeTime(q.SinceAt))
	}
	if !q.From.IsZero() {
		add("h.created_at >= ?", store.EncodeTime(q.From))
	}
	if !q.To.IsZero() {
		add("h.created_at <= ?", store.EncodeTime(q.To))
	}
	if len(q.Kinds) > 0 {
		values := make([]any, 0, len(q.Kinds))
		for _, kind := range q.Kinds {
			values = append(values, string(kind))
		}
		add("h.kind IN ("+placeholders(len(values))+")", values...)
	}
	if actor := strings.TrimSpace(q.Actor); actor != "" {
		add("h.actor = ?", actor)
	}
	if batch := strings.TrimSpace(q.Batch); batch != "" {
		add("h.batch_id = ?", batch)
	}
	if assignee := strings.TrimSpace(q.Assignee); assignee != "" {
		// A JOIN TO THE TASK'S CURRENT ASSIGNEE, which is what a caller
		// asking "what happened to the work X holds" means — not "what
		// did X do", which is `actor`. The two are routinely different
		// and naming them apart is the whole reason both exist.
		add("EXISTS (SELECT 1 FROM tracker_tasks t WHERE t.id = h.subject_id "+
			"AND t.assignee = ?)", assignee)
	}
	if q.Notified != nil {
		add("h.notified = ?", boolInt(*q.Notified))
	}
	if text := strings.TrimSpace(q.Q); text != "" {
		add(`h.excerpt LIKE ? ESCAPE '\'`, "%"+likeEscape(text)+"%")
	}
	if cursor := strings.TrimSpace(q.Cursor); cursor != "" {
		at, err := ParseLogPosition(cursor)
		if err != nil {
			return nil, nil, err
		}
		// STRICTLY BEFORE, because the feed is newest-first: the cursor
		// names the last row of the previous page.
		add("h.log_seq < ?", at.Packed())
	}
	return where, args, nil
}

// ParseLogPosition reads back the `stream@generation:seq` triple a cursor is.
//
// THE TRIPLE RATHER THAN A BARE SEQUENCE, because a sequence names no stream
// and no generation: handed one from a previous generation, a reader cannot
// tell a position that is behind from one that is impossibly far ahead — and
// the whole reason the public cursor carries all three is that it then never
// needs a migration.
func ParseLogPosition(raw string) (statelog.Position, error) {
	refuse := func() (statelog.Position, error) {
		return statelog.Position{}, fmt.Errorf("tracker: %q is not a log "+
			"position — one reads `<stream>@<generation>:<sequence>`, and the "+
			"answer that produced it carries the value to send back", raw)
	}
	stream, rest, found := strings.Cut(raw, "@")
	if !found || stream == "" {
		return refuse()
	}
	generation, sequence, found := strings.Cut(rest, ":")
	if !found {
		return refuse()
	}
	gen, err := strconv.ParseUint(generation, 10, 32)
	if err != nil {
		return refuse()
	}
	seq, err := strconv.ParseUint(sequence, 10, 64)
	if err != nil {
		return refuse()
	}
	at := statelog.Position{Stream: stream, Generation: uint32(gen), Seq: seq}
	if err := at.Valid(); err != nil {
		return statelog.Position{}, fmt.Errorf("tracker: %q is not a usable "+
			"log position: %w", raw, err)
	}
	return at, nil
}
