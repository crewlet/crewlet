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

	// ActorSeat is the seat an operator token was bound to when it made
	// this change — the PERSON behind the credential, and what a card
	// shows in place of the token's id. See [ActivityRecord.ActorSeat].
	ActorSeat string `json:"actor_seat,omitempty"`

	// CommentID is the comment this change wrote, and TurnID the agent
	// turn that made it — both from the history row, so a card can open
	// the thread or the trace without a second read.
	CommentID string `json:"comment_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`

	// Ask is the question this notice is about, where it is about one: the
	// ask itself for an `asked` notice, the ask it answered for an
	// `answered` one. See [AskView].
	Ask *AskView `json:"ask,omitempty"`

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
// Everything else — a thread you are in, a task you watch — is context, and
// context that competes with action turns an inbox into a feed nobody reads.
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

// SnoozeScope is what an inbox read does with the notices a person snoozed.
//
// THREE VALUES, because there are three questions. "My inbox" hides what the
// person said "not now" to — a snooze that the default read returned anyway
// would be a gesture that does nothing. "Everything" keeps them, marked. And
// "what have I put off" is ONLY them, which the two-valued flag this replaced
// could not ask: `include_snoozed` returned every notice with the snoozed ones
// among it, so the dashboard's Snoozed tab listed the whole inbox.
//
// A snooze whose time has come is NOT snoozed under any scope: it is back in
// the inbox, reported unsnoozed, and absent from `only` — see [markInbox].
type SnoozeScope string

const (
	// SnoozeExclude hides every live snooze — the inbox as a person works it.
	SnoozeExclude SnoozeScope = "exclude"
	// SnoozeInclude returns every notice, the snoozed ones marked.
	SnoozeInclude SnoozeScope = "include"
	// SnoozeOnly returns only the notices still asleep.
	SnoozeOnly SnoozeScope = "only"
)

// SnoozeScopes are the three, in the order a surface lists them.
var SnoozeScopes = []SnoozeScope{SnoozeExclude, SnoozeInclude, SnoozeOnly}

// Valid reports whether a scope off the wire is one this build knows. The
// empty string is not: see [InboxQuery.Snoozed].
func (s SnoozeScope) Valid() bool { return slices.Contains(SnoozeScopes, s) }

// InboxQuery asks for a page of somebody's inbox.
type InboxQuery struct {
	// Who this inbox belongs to. Required: an inbox with no person is not
	// a company-wide feed, it is a mistake — [Reader.Activity] is the
	// company-wide feed.
	//
	// A [Party] RATHER THAN A HANDLE, because `tracker_notifications` is
	// keyed on the recipient the RECORD named, and a person's records name
	// every identity they write under. A founder is `reporter` on their own
	// item under the token their assistant used and `assignee` on the next
	// one under their seat: one person, two recipients, and an inbox asked
	// about one of them is missing half of what the company told them. The
	// page is one keyset over both, and a change that named two of this
	// person's identities is collapsed to one notice — see
	// [collapseNotices].
	Who Party

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

	// Unread drops what this person's own record marks read — by the
	// same rule [markInbox] marks a row read, stated once more as SQL in
	// [inboxFilter.unread], because a filter applied to the page AFTER it
	// was read short-pages it. See [inboxFilter].
	Unread bool

	// Snoozed is what this read does with the notices this person put off,
	// and it is REQUIRED: the zero value is refused rather than read as one
	// of the three, because each of them is a sensible default for SOME
	// caller and a reader that picked one silently would answer the other
	// two's question wrong. A surface defaults it — to [SnoozeExclude],
	// which is what "my inbox" means.
	Snoozed SnoozeScope

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

	who := Party{
		Handle:     strings.TrimSpace(q.Who.Handle),
		OperatorID: strings.TrimSpace(q.Who.OperatorID),
	}
	switch {
	case !who.Named():
		return InboxAnswer{}, fmt.Errorf("tracker: an inbox read names nobody " +
			"— pass the handle whose inbox this is; the company-wide feed is " +
			"`work_activity`")
	case q.Level == "":
		return InboxAnswer{}, fmt.Errorf("tracker: this inbox read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	case !q.Snoozed.Valid():
		return InboxAnswer{}, fmt.Errorf("tracker: %q is not a snoozed scope — "+
			"pass one of %v; a surface resolves an absent `snoozed` to %q "+
			"before it reads", q.Snoozed, SnoozeScopes, SnoozeExclude)
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

	answer := InboxAnswer{Handle: who.Handle}
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
		person, _, err := readPartyRecord(ctx, tx, who)
		if err != nil {
			return err
		}
		answer.SeenThrough = person.SeenThrough
		answer.PrimaryReasons = effectivePrimary(person.PrimaryReasons)
		marks = person.marks(answer.PrimaryReasons)

		// THE MARKS ARE READ FIRST because the page is filtered BY them:
		// read, unread and snoozed are this person's record, and the
		// notices this page may return are a function of it. Read in the
		// same transaction, so the filter and the rows it selects are one
		// instant.
		notices, next, err := readInbox(ctx, tx, who, q,
			inboxFilterOf(q, marks, now), limit)
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
	answer.Unread, answer.Primary = countInbox(answer.Notices)

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

// inboxFilter is every narrowing an inbox read applies, resolved against the
// person's own record so it can be stated as SQL.
//
// # EVERY FILTER IS IN THE SCAN, NEVER ON THE PAGE
//
// The snooze scope, `unread` and `primary_only` used to be applied to the page
// AFTER it was read, while the cursor was computed from the page BEFORE. So a
// person with sixty snoozed notices among their newest fifty opened an inbox
// of nothing with a cursor behind it; the default read — unread, snoozes
// hidden, which is the dashboard's landing question — returned short or empty
// pages that a screen could only describe as "none on this page, there are
// more"; and the snoozed scope could not be asked at all, because the flag it
// rode on returned every notice with the snoozed ones among it. Stated as
// predicates on the scan, a page holds `limit` notices whenever the table
// holds them, and `next_cursor` means what it says.
//
// It is affordable because everything it binds is bounded by the person's own
// record — each list at most [MaxInboxEntries] ids, bound as ONE JSON array
// parameter — and everything it compares is a column of the row the index
// range is already reading.
type inboxFilter struct {
	// reasons is the set a row's reason must be in — `reasons` intersected
	// with the primary split under `primary_only` — and nil for every
	// reason. restricted distinguishes nil from an intersection that came
	// out EMPTY, which is a page of nothing rather than a page of all.
	reasons    []Reason
	restricted bool

	// unread keeps only what [markInbox] would mark unread.
	unread bool
	seen   Position
	read   []string
	marked []string

	// asleep is the live snoozes, and scope what to do with them.
	scope  SnoozeScope
	asleep []string
}

// inboxFilterOf resolves the query against the person's own record.
//
// A SNOOZE IS LIVE BY THE SAME TEST [markInbox] USES — no instant, or one
// after now — so a notice this filter hides is exactly a notice the page would
// have marked snoozed, and one whose time has come is back under every scope.
func inboxFilterOf(q InboxQuery, p person, now time.Time) inboxFilter {
	f := inboxFilter{unread: q.Unread, seen: p.seen, scope: q.Snoozed}
	if len(q.Reasons) > 0 || q.PrimaryOnly {
		f.restricted = true
		for _, reason := range Reasons {
			if len(q.Reasons) > 0 && !slices.Contains(q.Reasons, reason) {
				continue
			}
			if q.PrimaryOnly && !p.primary[reason] {
				continue
			}
			f.reasons = append(f.reasons, reason)
		}
	}
	for id := range p.read {
		f.read = append(f.read, id)
	}
	for id := range p.unread {
		f.marked = append(f.marked, id)
	}
	for id, until := range p.snoozed {
		if until == nil || until.After(now) {
			f.asleep = append(f.asleep, id)
		}
	}
	// SORTED, so one question is one statement: the maps iterate in a
	// random order and the arguments would otherwise differ per call.
	slices.Sort(f.read)
	slices.Sort(f.marked)
	slices.Sort(f.asleep)
	return f
}

// clauses is the filter as WHERE terms over `tracker_notifications n`, and
// false when it can select nothing at all.
func (f inboxFilter) clauses(who Party) ([]string, []any, bool) {
	var where []string
	var args []any
	if f.restricted {
		if len(f.reasons) == 0 {
			// `reasons=mention` with `primary_only` for a person who
			// made mentions context: the intersection is empty, and the
			// honest answer is an empty page rather than an `IN ()`.
			return nil, nil, false
		}
		where = append(where, "n.reason IN ("+placeholders(len(f.reasons))+")")
		for _, reason := range f.reasons {
			args = append(args, string(reason))
		}
		// AND THE ROW MUST BE THE ONE THE CHANGE IS HEARD UNDER. A
		// party of two holds one row per identity for a change, each
		// with its own reason, and [collapseNotices] keeps the strongest.
		// Filtering the rows by reason BEFORE that collapse would let a
		// change heard as `assignee` surface under `primary_only` — or
		// under `reasons=watcher` — as the weaker `watcher` row of the
		// other identity: one change, two reasons, depending on the
		// filter. So a row is kept only when no sibling row of the same
		// change outranks it, which is the collapse's own rule.
		if ids := who.Handles(); len(ids) > 1 {
			rankOther, rankArgs := reasonRankSQL("o.reason")
			rankThis, thisArgs := reasonRankSQL("n.reason")
			where = append(where, "NOT EXISTS (SELECT 1 FROM "+
				"tracker_notifications o WHERE o.record_id = n.record_id "+
				"AND o.recipient IN ("+placeholders(len(ids))+") "+
				"AND "+rankOther+" < "+rankThis+")")
			args = append(args, who.args()...)
			args = append(args, rankArgs...)
			args = append(args, thisArgs...)
		}
	}
	if f.unread {
		// [markInbox]'s rule, as SQL: an unread mark outranks
		// everything; otherwise a notice is read when the read list
		// names it or the seen-through position covers it — ON THE SAME
		// STREAM, compared as the packed position, which orders the
		// generation before the sequence exactly as [readPast] does.
		covered := "0"
		var coveredArgs []any
		if f.seen.Seq != 0 && f.seen.Stream != "" {
			covered = "(n.log_stream = ? AND n.log_seq <= ?)"
			coveredArgs = []any{f.seen.Stream, int64(f.seen.packed())}
		}
		where = append(where, "(n.record_id IN (SELECT value FROM json_each(?)) "+
			"OR (n.record_id NOT IN (SELECT value FROM json_each(?)) "+
			"AND NOT "+covered+"))")
		args = append(args, idList(f.marked), idList(f.read))
		args = append(args, coveredArgs...)
	}
	switch f.scope {
	case SnoozeExclude:
		if len(f.asleep) > 0 {
			where = append(where, "n.record_id NOT IN (SELECT value FROM json_each(?))")
			args = append(args, idList(f.asleep))
		}
	case SnoozeOnly:
		if len(f.asleep) == 0 {
			return nil, nil, false
		}
		where = append(where, "n.record_id IN (SELECT value FROM json_each(?))")
		args = append(args, idList(f.asleep))
	case SnoozeInclude:
	}
	return where, args, true
}

// reasonRankSQL is [reasonRank] as a SQL expression over one column.
//
// DERIVED FROM [Reasons], never typed out: the precedence is the order of that
// list, and a second copy of it here would be a second opinion the day a
// reason is added. An unknown reason ranks last, for [reasonRank]'s reason.
func reasonRankSQL(column string) (string, []any) {
	var b strings.Builder
	args := make([]any, 0, len(Reasons)*2+1)
	b.WriteString("(CASE " + column)
	for i, reason := range Reasons {
		b.WriteString(" WHEN ? THEN ?")
		args = append(args, string(reason), i)
	}
	b.WriteString(" ELSE ? END)")
	args = append(args, len(Reasons))
	return b.String(), args
}

// readInbox reads one page of the notification rows.
//
// THE JOIN IS LEFT, because the two tables have different lifetimes: the
// notification rows are swept on the inbox retention horizon and the history
// rows are never swept, but a reanchor rebuilds the history from the log and a
// notice can outlive the row it names. What was said outlives who said it.
func readInbox(ctx context.Context, tx *sql.Tx, who Party, q InboxQuery,
	filter inboxFilter, limit int) ([]InboxNotice, string, error) {

	ids := who.Handles()
	where := []string{"n.recipient IN (" + placeholders(len(ids)) + ")"}
	args := who.args()

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
	narrowed, narrowedArgs, selects := filter.clauses(who)
	if !selects {
		return []InboxNotice{}, "", nil
	}
	where = append(where, narrowed...)
	args = append(args, narrowedArgs...)
	// ONE EXTRA ROW PER IDENTITY, because the page is counted in CHANGES
	// and the table is keyed in (change, recipient) pairs.
	//
	// `tracker_notifications` has (record_id, recipient) as its primary
	// key, so one change leaves at most one row per identity and a party
	// of two can hold two rows for one change. Reading `limit+1` rows
	// would then return as few as half a page. Reading `limit*n+1`
	// guarantees at least `limit+1` distinct changes came back whenever
	// the table holds them, so the page is full and `more` is honest —
	// and with a single identity it is exactly the `limit+1` this read
	// has always taken.
	over := limit*len(ids) + 1
	args = append(args, over)

	rows, err := tx.QueryContext(ctx, `
		SELECT n.record_id, n.log_seq, n.log_stream, n.log_generation,
		       n.created_at, n.reason, n.addressed, n.fallback_only,
		       n.kind, n.subject_id, n.subject_key, n.excerpt,
		       COALESCE(h.actor, ''), COALESCE(h.actor_kind, ''),
		       COALESCE(h.actor_seat, ''), COALESCE(h.comment_id, ''),
		       COALESCE(h.turn_id, '')
		  FROM tracker_notifications n
		  LEFT JOIN tracker_history h ON h.id = n.record_id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY n.log_seq DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("tracker: read %s's inbox: %w", who, err)
	}
	defer func() { _ = rows.Close() }()

	read := make([]InboxNotice, 0, over)
	for rows.Next() {
		var notice InboxNotice
		var packed int64
		var at int64
		var reason, kind, actorKind string
		var addressed, fallback int
		if err := rows.Scan(&notice.RecordID, &packed, &notice.LogStream,
			&notice.LogGeneration, &at, &reason, &addressed, &fallback,
			&kind, &notice.SubjectID, &notice.SubjectKey, &notice.Excerpt,
			&notice.Actor, &actorKind, &notice.ActorSeat, &notice.CommentID,
			&notice.TurnID); err != nil {

			return nil, "", fmt.Errorf("tracker: scan %s's inbox: %w", who, err)
		}
		notice.LogSeq = uint64(packed) % statelog.GenerationStride
		notice.At = store.DecodeTime(at)
		notice.Reason = Reason(reason)
		notice.Kind = ChangeKind(kind)
		notice.ActorKind = AuthorKind(actorKind)
		notice.Addressed = addressed != 0
		notice.Fallback = fallback != 0
		read = append(read, notice)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("tracker: read %s's inbox: %w", who, err)
	}

	out := collapseNotices(read)
	more := false
	if len(out) > limit {
		// THE EXTRA CHANGE IS THE CURSOR'S EVIDENCE and never an
		// answer, which is the activity feed's own rule: a page that
		// returned it would overrun the caller's limit. Cutting at a
		// CHANGE boundary is exact because every row of one change
		// carries that change's own `log_seq`, and the cursor below is
		// strictly-before — so the next page resumes at the change
		// after the last one returned, with no row of it left behind.
		out, more = out[:limit], true
	}

	// THE ASK EACH NOTICE IS ABOUT, in one read over the page's comments
	// — see [readAskViews].
	comments := make([]string, 0, len(out))
	for _, notice := range out {
		if notice.CommentID != "" {
			comments = append(comments, notice.CommentID)
		}
	}
	asks, askErr := readAskViews(ctx, tx, comments)
	if askErr != nil {
		return nil, "", askErr
	}
	for i := range out {
		out[i].Ask = asks[out[i].CommentID]
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

// collapseNotices reduces a change that named two of one person's identities
// to the one notice they hear under.
//
// # It is the one-reason-per-handle rule, extended to the party
//
// [Candidates] already gives each HANDLE exactly one reason, the first in
// [Reasons] that names them — "a person who is mentioned AND watching is told
// they were mentioned, which is the stronger fact and the one they will act
// on". A person with two identities gets one row per identity, so the same
// change can arrive twice: `reporter` under the credential they filed it with
// and `assignee` under the seat a colleague handed it to. Two cards for one
// change is the exact noise that rule exists to prevent, and neither card is
// wrong on its own, so the answer is the stronger reason — the same tie-break,
// one level up.
//
// The rows arrive newest-first and every row of one change carries that
// change's own `log_seq`, so collapsing preserves the order: the survivor sits
// where the change's first row was.
func collapseNotices(read []InboxNotice) []InboxNotice {
	out := make([]InboxNotice, 0, len(read))
	at := make(map[string]int, len(read))
	for _, notice := range read {
		i, seen := at[notice.RecordID]
		if !seen {
			at[notice.RecordID] = len(out)
			out = append(out, notice)
			continue
		}
		if reasonRank(notice.Reason) < reasonRank(out[i].Reason) {
			// THE WHOLE ROW, not just its reason: `addressed` and
			// `fallback_only` are properties of the (change,
			// recipient) pair the reason came from, and keeping one
			// row's reason beside another's flags would report a
			// wake that asks nothing as one that does.
			out[i] = notice
		}
	}
	return out
}

// reasonRank is a reason's place in [Reasons], which IS the precedence.
//
// A reason this build does not know ranks LAST rather than first: an older
// node's row is a fact about a company running two builds, and a value nothing
// can order must not be allowed to outrank one that can be.
func reasonRank(reason Reason) int {
	if i := slices.Index(Reasons, reason); i >= 0 {
		return i
	}
	return len(Reasons)
}

// person is the marks half, so [markInbox] takes one argument rather than
// four positional ones that are easy to swap.
type person struct {
	seen    Position
	primary map[Reason]bool

	// read is the entries ABOVE the seen-through position that this
	// person has nonetheless marked read, unread the entries AT OR BELOW
	// it they marked unread again, and snoozed the sleeping ones. All
	// three are keyed on the record id an [InboxEntry] names, which is the
	// notification row's own `record_id`.
	read    map[string]bool
	unread  map[string]bool
	snoozed map[string]*time.Time
}

// marks is this person's record as the two lookups the page is marked against.
func (p Person) marks(primary []Reason) person {
	out := person{
		seen:    p.SeenThrough,
		primary: make(map[Reason]bool, len(primary)),
		read:    make(map[string]bool, len(p.Read)),
		unread:  make(map[string]bool, len(p.Unread)),
		snoozed: make(map[string]*time.Time, len(p.Snoozed)),
	}
	for _, reason := range primary {
		out.primary[reason] = true
	}
	for _, entry := range p.Read {
		out.read[entry.RecordID] = true
	}
	for _, entry := range p.Unread {
		out.unread[entry.RecordID] = true
	}
	for _, entry := range p.Snoozed {
		out.snoozed[entry.RecordID] = entry.Until
	}
	return out
}

// markInbox classifies and marks the page against this person's own record.
//
// READ IS THE SEEN-THROUGH POSITION FIRST and the entry list second, which is
// the whole reason the position exists: the read list is PRUNED at every
// write to what sits above it, so a notice below the position carries no read
// entry at all. Reading only the lists would report every pruned notice unread —
// which is every notice older than the person's last visit, the exact set an
// inbox must not resurface. Reading only the position would miss what they
// marked read out of order, which is what a person working their queue does.
//
// AND AN UNREAD MARK OUTRANKS THE POSITION. It is the one way to bring back a
// notice the position already covers — after a "mark all read", that is every
// notice — and a reader that consulted only the position and the read list
// made that gesture a mark nothing could ever see.
//
// A SNOOZE WHOSE TIME HAS COME IS NOT SNOOZED, for the reason [splitSnoozes]
// gives: the entry is reported DUE rather than silently promoted, because
// putting one back is a write and a read that performed one would change the
// fleet's state from a path with no operation id and no record.
func markInbox(notices []InboxNotice, p person, now time.Time) []InboxNotice {
	for i := range notices {
		notice := &notices[i]
		notice.Primary = p.primary[notice.Reason]
		notice.Read = !p.unread[notice.RecordID] &&
			(p.read[notice.RecordID] || readPast(p.seen, *notice))
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

// countInbox counts the page's unread and primary notices.
//
// IT FILTERS NOTHING. Every narrowing is in the scan ([inboxFilter]); what is
// left here is the two figures an answer reports over the page it returns.
func countInbox(notices []InboxNotice) (int, int) {
	unread, primary := 0, 0
	for _, notice := range notices {
		if !notice.Read {
			unread++
		}
		if notice.Primary {
			primary++
		}
	}
	return unread, primary
}

// idList is ids as the one JSON array a `json_each(?)` term binds.
//
// ONE PARAMETER RATHER THAN ONE PER ID, because the lists are the person's own
// and three of them together approach the driver's bound-parameter ceiling
// ([store.Caps.MaxVariables] probes 2 000). An empty list is "[]", the one
// spelling of an empty set: `NOT IN` over it keeps every row and `IN` none.
func idList(ids []string) string {
	if len(ids) == 0 {
		return "[]"
	}
	return jsonOf(ids)
}
