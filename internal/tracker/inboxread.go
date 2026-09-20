package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// One person's inbox — what the company asked of them, in the order the log
// asked it.
//
// # The rows were always here
//
// The applier has written `tracker_notifications` since the domain landed: one
// row per (commit, recipient), carrying the reason that handle hears under,
// the subject, the excerpt and the log position. The table ships two indexes
// naming this reader — `(recipient, log_seq DESC)` and `(recipient, reason,
// log_seq DESC)` for the primary/other split — and until now NOTHING read a
// single row of it. A company routed every change to the people it concerned,
// wrote them all down, replicated them, snapshotted them, and offered no way
// to ask what was in them.
//
// # Why it is not [Person]
//
// [Person] is the person's OWN state: which entries they have read, what they
// snoozed, how far they have got. It is written by the seat bound to them and
// it is a MARK over this feed, not the feed itself — which is why an
// [InboxEntry] carries a record id and a position and no content at all.
//
// The two halves compose here: the notification rows are the SOURCE, the
// person record is the READ STATE, and this reader is the only place that
// holds both. That split is deliberate and is what makes the inbox survive a
// seat that never writes: a person who has marked nothing still has an inbox,
// because the applier — which outlives every writer — wrote it.
//
// # The primary split
//
// [Person.PrimaryReasons] is the person's own answer to "which of these are
// mine to act on". Empty means the shipped default, [DefaultPrimaryReasons],
// rather than "nothing is primary": a person who has never expressed a
// preference wants a useful inbox, not an empty primary list. The split is a
// classification of the SAME rows rather than a filter — `other` is still
// returned, because a notice nobody can see is a notice that was not delivered.
//
// # What is NOT here
//
// An unread COUNT over the whole table. The rows a person has read are named
// by their own [Person] record as entries, so "unread" is a set difference
// this reader computes over the page it returns and cannot compute over rows
// it did not read. A caller that wants "how many since I last looked" asks
// with `since` at their own seen-through position, which is one index range.

// InboxNotice is one thing the company told somebody.
type InboxNotice struct {
	// RecordID is the history row this came from, and it is what a mark
	// names: [InboxEntry.RecordID] is this value.
	RecordID string `json:"record_id"`

	// The position, as the triple every cursor in this domain uses.
	LogSeq        uint64 `json:"log_seq"`
	LogStream     string `json:"log_stream"`
	LogGeneration uint32 `json:"log_generation"`

	// At is the AUTHORED instant, for the reason the activity feed gives:
	// a card saying "yesterday" must not move because a record was
	// redelivered.
	At time.Time `json:"at"`

	// Reason is the ONE reason this handle hears under — the first in
	// [Reasons] that named them. See [Candidates].
	Reason Reason `json:"reason"`

	// Primary says which half of the split this fell in, so a caller
	// rendering one list can still group it.
	Primary bool `json:"primary"`

	// Addressed marks a notice that ASKS something rather than informing:
	// a turn that must answer. It is the strongest signal on the row and
	// it is what an agent sorts its own queue by.
	Addressed bool `json:"addressed"`

	// Fallback marks a notice delivered only because nobody better was
	// found — a lead who hears about their report's task because the
	// report has left. A person sorting their own queue wants to know.
	Fallback bool `json:"fallback,omitempty"`

	Kind      ChangeKind `json:"kind"`
	SubjectID string     `json:"subject_id"`

	// SubjectKey is the human-readable key, resolved by the applier so
	// this read is an index range rather than a join.
	SubjectKey string `json:"subject_key,omitempty"`

	Excerpt string `json:"excerpt,omitempty"`

	// Actor is who made the change, joined from the history row. A notice
	// whose history row has been reanchored away carries none rather than
	// dropping the notice: what was said outlives who said it.
	Actor     string     `json:"actor,omitempty"`
	ActorKind AuthorKind `json:"actor_kind,omitempty"`

	// Read, Snoozed and SnoozedUntil are this person's own mark over the
	// row, from their [Person] record. A notice the person has never
	// touched is neither.
	Read         bool       `json:"read"`
	Snoozed      bool       `json:"snoozed,omitempty"`
	SnoozedUntil *time.Time `json:"snoozed_until,omitzero"`
}

// InboxAnswer is a page of one person's inbox.
type InboxAnswer struct {
	Handle  string        `json:"handle"`
	Notices []InboxNotice `json:"notices"`

	// PrimaryReasons is the split that was APPLIED, defaulted, so a
	// caller can render "you are seeing these because" without repeating
	// the defaulting rule. See [DefaultPrimaryReasons].
	PrimaryReasons []Reason `json:"primary_reasons"`

	// SeenThrough is where this person's own record says they have read
	// to, so a caller can mark the page it just rendered without a second
	// read.
	SeenThrough Position `json:"seen_through,omitzero"`

	// Counts are over the PAGE, and say so: a total over the table would
	// be a second scan of rows this answer did not return.
	Unread  int `json:"unread"`
	Primary int `json:"primary"`

	NextCursor string `json:"next_cursor,omitempty"`

	Level          statelog.ReadLevel `json:"read_level"`
	LogSeq         uint64             `json:"log_seq"`
	AppliedThrough uint64             `json:"applied_through"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// MaxInboxRows is how many notices one page carries.
//
// FIFTY, against the activity feed's two hundred, because the two answer
// different questions. A feed is scrolled and an inbox is WORKED: a person or
// an agent reads this to decide what to do next, and a page longer than one
// sitting is a page whose tail is never reached. It is also the bound on what
// one turn's prompt can carry a summary of without displacing the work.
const MaxInboxRows = 50

// DefaultPrimaryReasons is the split a person who has expressed no preference
// gets.
//
// THE ADDRESSED ONES PLUS THE OWNERSHIP ONES. A mention, a question, an answer
// to a question you asked, work assigned to you, work you reported, and work
// that became unblocked are all things that change what you should do next.
// Everything else — a thread you are in, a goal you own, a task you watch
// — is context, and context that competes with action turns an inbox into a
// feed nobody reads.
//
// It is deliberately NOT every reason with `Addressed` set: addressed is a
// property of one ROW and this is a property of a REASON, so a default derived
// from the rows would differ between two people with the same preferences.
var DefaultPrimaryReasons = []Reason{
	ReasonMention,
	ReasonAsked,
	ReasonAnswered,
	ReasonAssignee,
	ReasonUnassigned,
	ReasonReporter,
	ReasonUnblocked,
	ReasonPrioritised,
}

// InboxQuery asks for a page of somebody's inbox.
type InboxQuery struct {
	// Handle is whose inbox this is. Required: an inbox with no person is
	// not a company-wide feed, it is a mistake — [Reader.Activity] is the
	// company-wide feed.
	Handle string

	// Since is a lower bound as a log position, which is what a caller
	// resumes from after marking a page read.
	Since statelog.Position

	// Reasons narrows to particular ones. Empty is every reason, which is
	// not the same as the primary split: the split CLASSIFIES and this
	// FILTERS.
	Reasons []Reason

	// PrimaryOnly drops the other half rather than classifying it, for a
	// caller with room for one list.
	PrimaryOnly bool

	// Unread drops what this person's own record marks read. It is
	// applied AFTER the page is read, so it narrows what is returned and
	// never what is scanned — a person with ten thousand read notices
	// pages through them rather than scanning to the first unread one.
	// `since` at their seen-through position is the cheap form of the
	// same question.
	Unread bool

	// IncludeSnoozed keeps entries this person snoozed. Off by default,
	// because a snooze means "not now" and an inbox that returned them
	// anyway would make the gesture do nothing. A snooze whose time has
	// come is returned either way — see [splitSnoozes].
	IncludeSnoozed bool

	Limit  int
	Cursor string

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration
	MaxLagSeq   uint64
}

// Inbox answers a page of one person's notices.
func (r *Reader) Inbox(ctx context.Context, q InboxQuery, now time.Time) (
	InboxAnswer, error) {

	handle := strings.TrimSpace(q.Handle)
	switch {
	case handle == "":
		return InboxAnswer{}, fmt.Errorf("tracker: an inbox read names nobody " +
			"— pass the handle whose inbox this is; the company-wide feed is " +
			"`work_activity`")
	case q.Level == "":
		return InboxAnswer{}, fmt.Errorf("tracker: this inbox read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	}
	for _, reason := range q.Reasons {
		if !slices.Contains(Reasons, reason) {
			return InboxAnswer{}, fmt.Errorf("tracker: %q is not a wake reason",
				reason)
		}
	}
	limit := q.Limit
	if limit <= 0 || limit > MaxInboxRows {
		limit = MaxInboxRows
	}

	answer := InboxAnswer{Handle: handle}
	var marks person
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// BOTH FAMILIES, because this answer is composed of both: the
		// notices come from the change records and the marks from the
		// person's own. A closure naming one would certify an answer
		// complete that a deferred record in the other was holding.
		Scope:       inboxScope(),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		MaxLagSeq:   q.MaxLagSeq,
		Set:         true,
	}, func(tx *sql.Tx) error {
		person, _, err := readPerson(ctx, tx, handle)
		if err != nil {
			return err
		}
		answer.SeenThrough = person.SeenThrough
		answer.PrimaryReasons = effectivePrimary(person.PrimaryReasons)
		marks = person.marks(answer.PrimaryReasons)

		notices, next, err := readInbox(ctx, tx, handle, q, limit)
		if err != nil {
			return err
		}
		answer.Notices, answer.NextCursor = notices, next
		position, applied, err := readCheckpoint(ctx, tx)
		if err != nil {
			return err
		}
		answer.LogSeq, answer.AppliedThrough = position, applied
		return nil
	})
	if err != nil {
		return InboxAnswer{}, err
	}
	answer.Notices = markInbox(answer.Notices, marks, now)
	answer.Notices, answer.Unread, answer.Primary = filterInbox(answer.Notices, q)

	answer.Level = served.Level
	answer.Complete = served.Complete
	answer.LogLag = served.Lag
	if served.Incomplete != nil {
		answer.Incomplete = incompleteFrom(served.Incomplete)
	}
	return answer, nil
}

// effectivePrimary is the person's split, or the shipped default.
//
// AN EMPTY LIST IS "I HAVE NOT SAID", not "nothing is primary". The two are
// only distinguishable by a sentinel nobody would type, and the cost of
// getting it wrong is asymmetric: defaulting to the shipped set gives a person
// who never configured anything a useful inbox, while defaulting to empty
// gives every person in the company an inbox whose primary half is blank.
func effectivePrimary(declared []Reason) []Reason {
	if len(declared) == 0 {
		return slices.Clone(DefaultPrimaryReasons)
	}
	return slices.Clone(declared)
}

// inboxScope is both families this answer composes.
func inboxScope() statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised()
}

// readInbox reads one page of the notification rows.
//
// THE JOIN IS LEFT, because the two tables have different lifetimes: the
// notification rows are swept on the inbox retention horizon and the history
// rows are never swept, but a reanchor rebuilds the history from the log and a
// notice can outlive the row it names. What was said outlives who said it.
func readInbox(ctx context.Context, tx *sql.Tx, handle string, q InboxQuery,
	limit int) ([]InboxNotice, string, error) {

	where := []string{"n.recipient = ?"}
	args := []any{handle}

	// THE KEYSET IS THE COMPOSED POSITION, for the reason the activity
	// feed gives: `log_seq` already carries `(generation << 40) | seq`, so
	// the generation is IN the ordering rather than beside it — which is
	// what lets a cursor span a reanchor with no gap and no repeat.
	if cursor := strings.TrimSpace(q.Cursor); cursor != "" {
		at, err := ParseLogPosition(cursor)
		if err != nil {
			return nil, "", err
		}
		// STRICTLY BEFORE: the inbox is newest-first, so the cursor
		// names the last row of the previous page.
		where = append(where, "n.log_seq < ?")
		args = append(args, at.Packed())
	}
	if !q.Since.IsZero() {
		where = append(where, "n.log_seq > ?")
		args = append(args, q.Since.Packed())
	}
	if len(q.Reasons) > 0 {
		holes := make([]string, 0, len(q.Reasons))
		for _, reason := range q.Reasons {
			holes = append(holes, "?")
			args = append(args, string(reason))
		}
		where = append(where, "n.reason IN ("+strings.Join(holes, ",")+")")
	}
	args = append(args, limit+1)

	rows, err := tx.QueryContext(ctx, `
		SELECT n.record_id, n.log_seq, n.log_stream, n.log_generation,
		       n.created_at, n.reason, n.addressed, n.fallback_only,
		       n.kind, n.subject_id, n.subject_key, n.excerpt,
		       COALESCE(h.actor, ''), COALESCE(h.actor_kind, '')
		  FROM tracker_notifications n
		  LEFT JOIN tracker_history h ON h.id = n.record_id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY n.log_seq DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("tracker: read %s's inbox: %w", handle, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]InboxNotice, 0, limit)
	more := false
	for rows.Next() {
		if len(out) == limit {
			// THE EXTRA ROW IS THE CURSOR'S EVIDENCE and never an
			// answer, which is the activity feed's own rule: a page
			// that returned it would overrun the caller's limit.
			more = true
			break
		}
		var notice InboxNotice
		var packed int64
		var at int64
		var reason, kind, actorKind string
		var addressed, fallback int
		if err := rows.Scan(&notice.RecordID, &packed, &notice.LogStream,
			&notice.LogGeneration, &at, &reason, &addressed, &fallback,
			&kind, &notice.SubjectID, &notice.SubjectKey, &notice.Excerpt,
			&notice.Actor, &actorKind); err != nil {

			return nil, "", fmt.Errorf("tracker: scan %s's inbox: %w", handle, err)
		}
		notice.LogSeq = uint64(packed) % statelog.GenerationStride
		notice.At = store.DecodeTime(at)
		notice.Reason = Reason(reason)
		notice.Kind = ChangeKind(kind)
		notice.ActorKind = AuthorKind(actorKind)
		notice.Addressed = addressed != 0
		notice.Fallback = fallback != 0
		out = append(out, notice)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("tracker: read %s's inbox: %w", handle, err)
	}

	var next string
	if more && len(out) > 0 {
		last := out[len(out)-1]
		next = statelog.Position{
			Stream: last.LogStream, Generation: last.LogGeneration,
			Seq: last.LogSeq,
		}.String()
	}
	return out, next, nil
}

// person is the marks half, so [markInbox] takes one argument rather than
// four positional ones that are easy to swap.
type person struct {
	seen    Position
	primary map[Reason]bool

	// read is the entries ABOVE the seen-through position that this
	// person has nonetheless marked read, and snoozed is the same for
	// sleeping ones. Both are keyed on the record id an [InboxEntry]
	// names, which is the notification row's own `record_id`.
	read    map[string]bool
	snoozed map[string]*time.Time
}

// marks is this person's record as the two lookups the page is marked against.
func (p Person) marks(primary []Reason) person {
	out := person{
		seen:    p.SeenThrough,
		primary: make(map[Reason]bool, len(primary)),
		read:    make(map[string]bool, len(p.Read)),
		snoozed: make(map[string]*time.Time, len(p.Snoozed)),
	}
	for _, reason := range primary {
		out.primary[reason] = true
	}
	for _, entry := range p.Read {
		out.read[entry.RecordID] = true
	}
	for _, entry := range p.Snoozed {
		out.snoozed[entry.RecordID] = entry.Until
	}
	return out
}

// markInbox classifies and marks the page against this person's own record.
//
// READ IS THE SEEN-THROUGH POSITION FIRST and the entry list second, which is
// the whole reason the position exists: the entry lists are PRUNED at every
// write to what sits above it, so a notice below the position carries no entry
// at all. Reading only the lists would report every pruned notice unread —
// which is every notice older than the person's last visit, the exact set an
// inbox must not resurface. Reading only the position would miss what they
// marked read out of order, which is what a person working their queue does.
//
// A SNOOZE WHOSE TIME HAS COME IS NOT SNOOZED, for the reason [splitSnoozes]
// gives: the entry is reported DUE rather than silently promoted, because
// putting one back is a write and a read that performed one would change the
// fleet's state from a path with no operation id and no record.
func markInbox(notices []InboxNotice, p person, now time.Time) []InboxNotice {
	for i := range notices {
		notice := &notices[i]
		notice.Primary = p.primary[notice.Reason]
		notice.Read = p.read[notice.RecordID] || readPast(p.seen, *notice)
		if until, asleep := p.snoozed[notice.RecordID]; asleep {
			if until == nil || until.After(now) {
				notice.Snoozed = true
				notice.SnoozedUntil = until
			}
		}
	}
	return notices
}

// readPast reports whether a notice sits at or below the seen-through
// position.
//
// THE STREAM HAS TO MATCH, which is what the triple is for: a sequence from a
// dead stream compares as current against a live one, and a founder whose
// stream was recreated would open an inbox in which everything below their old
// position reads as already seen.
func readPast(seen Position, notice InboxNotice) bool {
	if seen.Seq == 0 || seen.Stream == "" || seen.Stream != notice.LogStream {
		return false
	}
	if seen.Generation != notice.LogGeneration {
		return seen.Generation > notice.LogGeneration
	}
	return seen.Seq >= notice.LogSeq
}

// filterInbox applies the narrowing the query asked for, and counts what
// survived.
func filterInbox(notices []InboxNotice, q InboxQuery) ([]InboxNotice, int, int) {
	out := notices[:0]
	unread, primary := 0, 0
	for _, notice := range notices {
		if q.PrimaryOnly && !notice.Primary {
			continue
		}
		if q.Unread && notice.Read {
			continue
		}
		if !q.IncludeSnoozed && notice.Snoozed {
			continue
		}
		if !notice.Read {
			unread++
		}
		if notice.Primary {
			primary++
		}
		out = append(out, notice)
	}
	return out, unread, primary
}
