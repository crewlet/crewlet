package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/routing"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// Who one change woke, and why: per recipient, the ONE reason that reached
// them, whether it asks something of them and whether they were only a
// fallback, where `tracker_history.notified` says only that the commit
// carried a notification ([ActivityRecord.Notified]).
//
// It reads `tracker_notifications` by RECORD, [Reader.Inbox] by recipient; the
// key `(record_id, recipient)` makes this a prefix scan with no index of its own.
//
// An empty set means the routing reached nobody, the inbox retention swept the
// rows (history rows are never swept), or neither can be told:
// [routing.DeliveryOf] decides from `notified`, the rows and the retention
// horizon the caller states ([RoutingQuery.Retention]).

// RoutingRecipient is one person a change reached, and why.
type RoutingRecipient struct {
	// Handle is whose inbox the row landed in.
	Handle string `json:"handle"`

	// Reason is the ONE reason this handle heard under: the first in
	// [Reasons] that named them ([Candidates] lists a handle once).
	Reason Reason `json:"reason"`

	// Addressed marks a notice that ASKS something rather than informing,
	// which decides whether a wake obliges a seat to answer
	// ([Reason.Addressed]).
	Addressed bool `json:"addressed"`

	// Fallback marks a recipient reached only because nobody better was
	// found (a lead, because the report has left); FallbackRank says WHICH
	// substitute this was.
	Fallback     bool `json:"fallback,omitempty"`
	FallbackRank int  `json:"fallback_rank,omitempty"`

	// Excerpt is what the row carried into their inbox; it differs per
	// recipient (a mention's is the sentence naming them).
	Excerpt string `json:"excerpt,omitempty"`
}

// RoutingAnswer is one change's whole routing.
type RoutingAnswer struct {
	RecordID string `json:"record_id"`

	// Held is false when no history row has this id (another company's, a
	// typo, or one a reanchor has not replayed yet): NOT a change that woke
	// nobody, which has a different remedy.
	Held bool `json:"held"`

	Kind       ChangeKind `json:"kind,omitempty"`
	SubjectID  string     `json:"subject_id,omitempty"`
	SubjectKey string     `json:"subject_key,omitempty"`

	// Actor is who made the change, and At the AUTHORED instant the inbox
	// renders, so a card and this page agree.
	Actor     string     `json:"actor,omitempty"`
	ActorKind AuthorKind `json:"actor_kind,omitempty"`
	At        time.Time  `json:"at,omitzero"`

	// Notified is the history row's own flag, kept beside [Recipients]
	// because the two disagreeing is evidence for Delivery.
	Notified bool `json:"notified"`

	// Recipients is everybody the change reached, ordered for a reader
	// rather than by the key: addressed first, fallbacks last, each group
	// by handle.
	Recipients []RoutingRecipient `json:"recipients"`

	// Delivery is what this node can honestly say about who the change
	// reached: what a surface renders when [Recipients] is empty, whose
	// states send a reader to different places.
	Delivery routing.Delivery `json:"delivery"`

	// RetainedFrom is the stated horizon's instant, the sweep's cutoff
	// Delivery is decided against; zero, and Delivery `unknown`, when the
	// caller stated none.
	RetainedFrom time.Time `json:"retained_from,omitzero"`

	// Truncated says the list was cut at [MaxRoutingRows], which only a
	// peer writing a larger set can cause; a short list must not look
	// complete.
	Truncated bool `json:"truncated,omitempty"`

	// Addressed and Fallback count the returned list.
	Addressed int `json:"addressed"`
	Fallback  int `json:"fallback"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// MaxRoutingRows bounds one change's recipient list.
//
// DERIVED, NEVER CHOSEN: every source a snapshot names is capped at the WRITE
// (`Notify.checkSnapshot` refuses rather than trims), so this is their sum,
// plus slack for the uncapped role handles (assignee, reporter, lead, the
// container's owner). Written as arithmetic so raising a cap raises it; a
// literal would silently truncate.
const MaxRoutingRows = MaxWatchers + // watchers
	MaxWatchers + // removed_watchers
	MaxCollaborators +
	MaxDependents + // unblocked
	MaxDependents + // dependents
	MaxThreadParticipants +
	MaxChecklists + // checklist_assignees
	MaxMentions +
	16 // the role handles, which have no cap of their own

// RoutingQuery asks who one change woke.
type RoutingQuery struct {
	// RecordID is the history row, which an activity card, an item's feed
	// and an inbox notice all carry.
	RecordID string

	// Retention is the company's `inbox.retention_days` as the caller runs
	// it, on the query rather than the reader because the setting moves
	// under a reader that outlives it. Zero states none, and an absent set
	// is then `unknown`.
	Retention time.Duration

	Level     statelog.ReadLevel
	Session   statelog.Position
	Floors    []statelog.Position
	MaxLag    time.Duration
	MaxLagSeq uint64
}

// Routing answers who one change woke, and under which reason.
//
// THERE IS NO CURSOR: the set is bounded at the write by [MaxRoutingRows], so
// it is always one page.
func (r *Reader) Routing(ctx context.Context, q RoutingQuery, now time.Time) (
	RoutingAnswer, error) {

	record := q.RecordID
	switch {
	case record == "":
		return RoutingAnswer{}, fmt.Errorf("worklet: a routing read names no " +
			"record — pass the `record_id` an activity card, an item's feed " +
			"or an inbox notice carries")
	case q.Level == "":
		return RoutingAnswer{}, fmt.Errorf("worklet: this routing read names " +
			"no level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}

	answer := RoutingAnswer{RecordID: record}
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// BOTH FAMILIES ([Reader.Inbox]'s reason): one family's closure
		// would certify the answer complete while a deferred record in
		// the other still held the rest.
		Scope:     routingScope(),
		Session:   q.Session,
		Floors:    q.Floors,
		MaxLag:    q.MaxLag,
		MaxLagSeq: q.MaxLagSeq,
		Set:       true,
	}, func(tx *sql.Tx) error {
		if err := readRoutingRecord(ctx, tx, &answer); err != nil {
			return err
		}
		recipients, key, truncated, err := readRoutingRecipients(ctx, tx, record)
		if err != nil {
			return err
		}
		answer.Recipients, answer.Truncated = recipients, truncated
		answer.SubjectKey = key
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		answer.LogSeq, answer.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return RoutingAnswer{}, err
	}

	for _, recipient := range answer.Recipients {
		if recipient.Addressed {
			answer.Addressed++
		}
		if recipient.Fallback {
			answer.Fallback++
		}
	}
	if q.Retention > 0 {
		answer.RetainedFrom = now.Add(-q.Retention)
	}
	// `notified` IS THE ANNOUNCEMENT, never "somebody was woken"
	// ([ActivityRecord.Notified]).
	answer.Delivery = routing.DeliveryOf(routing.Evidence{
		Held:         answer.Held,
		Announced:    answer.Notified,
		Recipients:   len(answer.Recipients),
		At:           answer.At,
		RetainedFrom: answer.RetainedFrom,
	})

	answer.Level = served.Level
	answer.Complete = served.Complete
	answer.LogLag = served.Lag
	if served.Incomplete != nil {
		answer.Incomplete = incompleteFrom(served.Incomplete)
	}
	return answer, nil
}

// routingScope is the whole domain: the answer's two halves live in
// different families ([Reader.Routing]).
func routingScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised()
}

// routingRecordStatement is [readRoutingRecord]'s one statement, named for the
// plan test.
const routingRecordStatement = `
		SELECT kind, subject_id, actor, actor_kind, notified, created_at
		  FROM tracker_history
		 WHERE id = ?`

// readRoutingRecord fills in the change's own facts. A MISSING ROW IS NOT AN
// ERROR but `held: false` (an old link, an unreplayed reanchor), the shape
// [Reader.Person] gives a person nobody has written.
func readRoutingRecord(ctx context.Context, tx *sql.Tx, answer *RoutingAnswer) error {
	var kind, actor, actorKind string
	var createdAt int64
	var notified int
	err := tx.QueryRowContext(ctx, routingRecordStatement, answer.RecordID).
		Scan(&kind, &answer.SubjectID, &actor, &actorKind, &notified, &createdAt)
	switch {
	case err == sql.ErrNoRows:
		return nil
	case err != nil:
		return fmt.Errorf("worklet: read the change %s: %w", answer.RecordID, err)
	}
	answer.Held = true
	answer.Kind = ChangeKind(kind)
	answer.Actor, answer.ActorKind = actor, AuthorKind(actorKind)
	answer.Notified = notified != 0
	answer.At = store.DecodeTime(createdAt)
	return nil
}

// readRoutingRecipients reads the notification rows for one change.
//
// THE SUBJECT KEY COMES FROM HERE: resolved at the write, it is what the
// recipient's inbox shows; the task row may hold a key renamed since.
func readRoutingRecipients(ctx context.Context, tx *sql.Tx, record string) (
	recipients []RoutingRecipient, subjectKey string, truncated bool, err error) {

	rows, err := tx.QueryContext(ctx, `
		SELECT recipient, reason, addressed, fallback_only, fallback_rank,
		       excerpt, subject_key
		  FROM tracker_notifications
		 WHERE record_id = ?
		 ORDER BY addressed DESC, fallback_only ASC, fallback_rank ASC,
		          recipient ASC
		 LIMIT ?`, record, MaxRoutingRows+1)
	if err != nil {
		return nil, "", false, fmt.Errorf("worklet: read the routing of %s: %w",
			record, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]RoutingRecipient, 0, 8)
	for rows.Next() {
		if len(out) == MaxRoutingRows {
			// THE EXTRA ROW IS EVIDENCE, never an answer.
			truncated = true
			break
		}
		var recipient RoutingRecipient
		var reason, key string
		var addressed, fallback int
		if err := rows.Scan(&recipient.Handle, &reason, &addressed, &fallback,
			&recipient.FallbackRank, &recipient.Excerpt, &key); err != nil {

			return nil, "", false, fmt.Errorf("worklet: scan the routing of %s: %w",
				record, err)
		}
		// EVERY ROW CARRIES THE SAME KEY, resolved once per commit.
		if subjectKey == "" {
			subjectKey = key
		}
		recipient.Reason = Reason(reason)
		recipient.Addressed = addressed != 0
		recipient.Fallback = fallback != 0
		if !recipient.Fallback {
			// A RANK WITHOUT A FALLBACK IS NOISE a surface would
			// render as "1st substitute" beside an assignee.
			recipient.FallbackRank = 0
		}
		out = append(out, recipient)
	}
	if err := rows.Err(); err != nil {
		return nil, "", false, fmt.Errorf("worklet: read the routing of %s: %w",
			record, err)
	}
	return out, subjectKey, truncated, nil
}
