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

// Who one change woke, and under which reason.
//
// # The fact no other tracker records
//
// Every work tracker can tell you that somebody was notified. This one records,
// per change and per recipient, the ONE reason of eighteen that reached them,
// whether it ASKS something of them, and whether they were reached only because
// nobody better was found. The applier has written exactly that set since the
// domain landed — it is what [Candidates] resolved — and its only trace on any
// surface was `tracker_history.notified`: a single boolean saying that
// somebody, somewhere, was told.
//
// That boolean is the whole reason this reader exists. "Did my comment reach
// the person I meant?" is the question a person actually has, and a company
// whose answer is `true` has no answer at all.
//
// # It is the OTHER axis of the same rows
//
// [Reader.Inbox] reads `tracker_notifications` by RECIPIENT: one person, every
// change. This reads the same table by RECORD: one change, every person. The
// table's primary key is `(record_id, recipient)`, so this direction is a
// primary-key prefix scan and needs no index of its own — the two readers are
// the two orders the key was already written in.
//
// # An absent set is three facts, not one
//
// `tracker_history.notified` does NOT mean somebody was woken. It means the
// commit CARRIED a notification — see [ActivityRecord.Notified], which says so
// explicitly, because the applier deliberately does not hold the roster [Route]
// would need. So a change with `notified = 1` and no recipient rows is
// genuinely ambiguous between three things:
//
//   - the routing resolved to NOBODY: every candidate was the actor, or has
//     left the company;
//   - the rows were SWEPT, because the notification rows are cleared on the
//     inbox retention horizon and the history rows never are;
//   - and neither can be told from the other, which is its own answer.
//
// Collapsing those into a boolean is the exact failure this reader exists to
// end one level up, so [RoutingAnswer.Delivery] is a value: "announced and
// reached nobody" sends a reader to look at who has left, and "swept" sends
// them to look at a retention horizon. They are not the same errand. The rule
// that tells them apart is [routing.DeliveryOf], every native domain's, and
// what this reader adds is the evidence it decides on: the history row's
// `notified`, the notification rows it found, and the caller's horizon.
//
// They are separated by the HORIZON, which the caller states: a change older
// than `inbox.retention_days` is one whose rows the sweep is entitled to have
// taken and the applier is entitled never to have written, so their absence
// is not evidence. A change newer than it would still have its rows, so their
// absence IS evidence — nobody was reached.
//
// The horizon travels on the QUERY rather than on the reader, for the reason
// `now` does: it is a company setting, the epoch moves under a reader that
// outlives it, and a reader holding a stale horizon would date a set against a
// retention nobody is running. A caller that states none gets `unknown`, which
// is the honest answer to "is this gone?" from somebody who does not know how
// long anything is kept.

// RoutingRecipient is one person a change reached, and why.
type RoutingRecipient struct {
	// Handle is whose inbox the row landed in.
	Handle string `json:"handle"`

	// Reason is the ONE reason this handle heard under — the first in
	// [Reasons] that named them. See [Candidates]: the resolution is
	// ordered and a handle appears once, so this is a value rather than
	// a set.
	Reason Reason `json:"reason"`

	// Addressed marks a notice that ASKS something rather than informing.
	// It is the property that decides whether a wake obliges a seat to
	// answer — see [Reason.Addressed] — and it is the column a person
	// scanning this list is actually looking for.
	Addressed bool `json:"addressed"`

	// Fallback marks a recipient reached only because nobody better was
	// found: a lead who hears about their report's task because the
	// report has left. Rank orders the fallbacks among themselves, so a
	// surface can say WHICH substitute this was rather than only that it
	// was one.
	Fallback     bool `json:"fallback,omitempty"`
	FallbackRank int  `json:"fallback_rank,omitempty"`

	// Excerpt is what the row carried into their inbox, which can differ
	// per recipient: a mention's excerpt is the sentence naming them.
	Excerpt string `json:"excerpt,omitempty"`
}

// RoutingAnswer is one change's whole routing.
type RoutingAnswer struct {
	RecordID string `json:"record_id"`

	// Held is false when no history row with this id exists — a record id
	// from another company, a typo, or one a reanchor has not replayed
	// yet. It is NOT the same as a change that woke nobody, and the two
	// have different remedies.
	Held bool `json:"held"`

	Kind       ChangeKind `json:"kind,omitempty"`
	SubjectID  string     `json:"subject_id,omitempty"`
	SubjectKey string     `json:"subject_key,omitempty"`

	// Actor is who made the change, and At the AUTHORED instant — the
	// same instant the inbox renders, so a card and this page agree.
	Actor     string     `json:"actor,omitempty"`
	ActorKind AuthorKind `json:"actor_kind,omitempty"`
	At        time.Time  `json:"at,omitzero"`

	// Notified is the history row's own flag: what the applier concluded
	// at the time it routed. It is what [Recipients] replaces, and it is
	// kept because the two disagreeing is meaningful — see Swept.
	Notified bool `json:"notified"`

	// Recipients is everybody the change reached, ordered as a reader
	// wants them rather than as the key stores them: addressed first,
	// then fallbacks last, each group by handle.
	Recipients []RoutingRecipient `json:"recipients"`

	// Delivery is what this node can honestly say about who the change
	// reached — see the file head. It is the value a surface renders when
	// [Recipients] is empty, and the three empty states it distinguishes
	// send a reader to three different places.
	Delivery routing.Delivery `json:"delivery"`

	// RetainedFrom is the instant the stated horizon falls on — the
	// sweep's own cutoff — and it is what Delivery is decided against.
	// Zero when the caller stated no horizon, which is what makes
	// `unknown` reachable.
	RetainedFrom time.Time `json:"retained_from,omitzero"`

	// Truncated says the recipient list was cut at [MaxRoutingRows].
	// It cannot happen for a record this build wrote — every source of a
	// recipient is capped at the write and the bound is their sum — so it
	// means a peer wrote a larger set, and a reader must be told rather
	// than shown a short list that looks complete.
	Truncated bool `json:"truncated,omitempty"`

	// Addressed and Fallback are counts over the returned list, which is
	// the whole list unless Truncated says otherwise.
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
// DERIVED, NEVER CHOSEN. Every source a routing snapshot can name is capped at
// the WRITE — see `Notify.checkSnapshot`, which refuses rather than trims — so
// the recipient set is bounded before it is stored and this is their sum, plus
// a slack term for the role handles no cap governs (assignee, reporter, lead,
// the container's owner: a fixed handful that would have to be counted by hand
// and would drift the day one is added).
//
// Written as the arithmetic rather than as a number so that raising any one of
// those caps raises this with it. A literal here would silently start
// truncating the day somebody doubled [MaxWatchers].
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
	// RecordID is the history row, which is what every surface already
	// holds: an activity card, an item's own feed and an inbox notice all
	// carry it.
	RecordID string

	// Retention is the company's `inbox.retention_days` as the caller is
	// currently running it — see the file head. Zero states none, and an
	// absent recipient set is then `unknown` rather than dated against a
	// horizon nobody is keeping.
	Retention time.Duration

	Level     statelog.ReadLevel
	Session   statelog.Position
	Floors    []statelog.Position
	MaxLag    time.Duration
	MaxLagSeq uint64
}

// Routing answers who one change woke, and under which reason.
//
// THERE IS NO CURSOR, and that is a property of the data rather than an
// omission: the set is bounded at the write by [MaxRoutingRows] and read by a
// primary-key prefix, so the whole of it is one page. A cursor would be a
// second thing to keep correct for a list that cannot have a second page.
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
		// BOTH FAMILIES, for [Reader.Inbox]'s own reason: this answer is
		// composed of the history row and the notification rows, and a
		// closure naming one family would certify it complete while a
		// deferred record in the other was still holding the rest.
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
	// THE HISTORY ROW'S `notified` IS THE ANNOUNCEMENT, never "somebody was
	// woken" — see [ActivityRecord.Notified] — and a record id naming no
	// row is not held: the two facts the rule needs besides the rows.
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

// routingScope is the whole domain, because the two halves live in different
// families — see [Reader.Routing].
func routingScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised()
}

// routingRecordStatement is [readRoutingRecord]'s one statement, named for the
// plan test.
const routingRecordStatement = `
		SELECT kind, subject_id, actor, actor_kind, notified, created_at
		  FROM tracker_history
		 WHERE id = ?`

// readRoutingRecord fills in the change's own facts.
//
// A MISSING ROW IS NOT AN ERROR. A record id that names nothing is an ordinary
// thing for a caller to hold — a link from an old page, a reanchor that has
// not replayed this far — and the answer says `held: false` rather than
// failing, which is the same shape [Reader.Person] gives a person nobody has
// written.
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
// THE SUBJECT KEY COMES FROM HERE rather than from the history row, because
// the applier resolved it at the write and it is the value the recipient's own
// inbox shows. Reading it from the task row instead would show a key that has
// since been renamed, which is a different fact from what the person was told.
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
			// THE EXTRA ROW IS EVIDENCE, never an answer — the page
			// stays at the bound and the caller is told it was cut.
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
		// EVERY ROW CARRIES THE SAME KEY — the applier resolves it once
		// per commit — so the first non-empty one is the change's.
		if subjectKey == "" {
			subjectKey = key
		}
		recipient.Reason = Reason(reason)
		recipient.Addressed = addressed != 0
		recipient.Fallback = fallback != 0
		if !recipient.Fallback {
			// A RANK WITHOUT A FALLBACK IS NOISE: the column is zero
			// on every non-fallback row today, and a surface reading
			// it would render "1st substitute" beside an assignee.
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
