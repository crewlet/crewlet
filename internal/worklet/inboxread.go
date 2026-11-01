package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
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
// [inbox.Entry] carries a record id and a position and no content at all.
//
// The two halves compose here: the notification rows are the SOURCE, the
// person record is the READ STATE, and this reader is the only place that
// holds both. That split is deliberate and is what makes the inbox survive a
// seat that never writes: a person who has marked nothing still has an inbox,
// because the applier — which outlives every writer — wrote it.
//
// # The primary split
//
// [inbox.Marks.PrimaryReasons] is the person's own answer to "which of these
// are mine to act on". Empty means the shipped default, [DefaultPrimaryReasons],
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
	// names: [inbox.Entry.RecordID] is this value.
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

	// Task is the id of the task this notice is about, and empty when it
	// names none: the subject for a task commit, and the task a lead put at
	// the top of the list for a `prioritised` notice — whose SUBJECT is the
	// person, while [InboxNotice.SubjectKey] is the task's key. It is what
	// a reader opens when that key opens another task.
	Task string `json:"task,omitempty"`

	// SubjectKeyCollision says [InboxNotice.SubjectKey] does not open the
	// task this notice is about: the key directory names another task for
	// it, so a link built from the key reaches THAT task and this notice's
	// is reached by [InboxNotice.Task]. See [ItemAddress]. Never set on a
	// notice that names no task, whose key is the only address it carries.
	//
	// ASKED OF THE STORED KEY, not read off the task's own flag, because
	// the two can name different keys: the key here is the one the task
	// held when the notice was written, and a task moved since answers to
	// a new one. What a reader follows is THIS key, so the question is
	// whether THIS key resolves to the task — one probe of the directory's
	// primary key per row of a bounded page.
	//
	// AGAINST [InboxNotice.Task], NEVER [InboxNotice.SubjectID]: a
	// prioritised notice's subject is a person, so asked of the subject
	// every one of them read as a collision and was sent to the person's
	// handle as though it were a task.
	SubjectKeyCollision bool `json:"subject_key_collision,omitempty"`

	Excerpt string `json:"excerpt,omitempty" untrusted:"fence"`

	// Actor is who made the change, joined from the history row. A notice
	// whose history row has been reanchored away carries none rather than
	// dropping the notice: what was said outlives who said it.
	Actor     string     `json:"actor,omitempty"`
	ActorKind AuthorKind `json:"actor_kind,omitempty"`

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
	SeenThrough inbox.Position `json:"seen_through,omitzero"`

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

	// Unread drops what this person's own record marks read — by the
	// same rule [inbox.Classifier.Classify] marks a row read, stated once
	// more as SQL in [inboxFilter.unread], because a filter applied to the
	// page AFTER it was read short-pages it. See [inboxFilter].
	Unread bool

	// Snoozed is what this read does with the notices this person put off,
	// and it is REQUIRED: the zero value is refused rather than read as one
	// of the three, because each of them is a sensible default for SOME
	// caller and a reader that picked one silently would answer the other
	// two's question wrong. A surface defaults it — to
	// [inbox.SnoozeExclude], which is what "my inbox" means.
	Snoozed inbox.SnoozeScope

	Limit  int
	Cursor string

	Level     statelog.ReadLevel
	Session   statelog.Position
	Floors    []statelog.Position
	MaxLag    time.Duration
	MaxLagSeq uint64
}

// Inbox answers a page of one person's notices.
func (r *Reader) Inbox(ctx context.Context, q InboxQuery, now time.Time) (
	InboxAnswer, error) {

	handle := strings.TrimSpace(q.Handle)
	switch {
	case handle == "":
		return InboxAnswer{}, fmt.Errorf("worklet: an inbox read names nobody " +
			"— pass the handle whose inbox this is; the company-wide feed is " +
			"`work_activity`")
	case q.Level == "":
		return InboxAnswer{}, fmt.Errorf("worklet: this inbox read names no " +
			"level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	case !q.Snoozed.Valid():
		return InboxAnswer{}, fmt.Errorf("worklet: %q is not a snoozed scope; "+
			"pass one of %s; a surface resolves an absent `snoozed` to %q "+
			"before it reads", q.Snoozed, words.Or(inbox.SnoozeScopes), inbox.SnoozeExclude)
	}
	for _, reason := range q.Reasons {
		if !slices.Contains(Reasons, reason) {
			return InboxAnswer{}, fmt.Errorf("%w: %q is not a wake reason",
				ErrBadQuery, reason)
		}
	}
	if err := feedPositions(q.Cursor, q.Since); err != nil {
		return InboxAnswer{}, err
	}
	limit := q.Limit
	if limit <= 0 || limit > inbox.MaxRows {
		limit = inbox.MaxRows
	}

	answer := InboxAnswer{Handle: handle}
	var marks inbox.Classifier
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// BOTH FAMILIES, because this answer is composed of both: the
		// notices come from the change records and the marks from the
		// person's own. A closure naming one would certify an answer
		// complete that a deferred record in the other was holding.
		Scope:     inboxScope(),
		Session:   q.Session,
		Floors:    q.Floors,
		MaxLag:    q.MaxLag,
		MaxLagSeq: q.MaxLagSeq,
		Set:       true,
	}, func(tx *sql.Tx) error {
		person, _, err := readPerson(ctx, tx, handle)
		if err != nil {
			return err
		}
		answer.SeenThrough = person.SeenThrough
		answer.PrimaryReasons = effectivePrimary(person.PrimaryReasons)
		marks = person.Classifier(answer.PrimaryReasons)

		// THE MARKS ARE READ FIRST because the page is filtered BY them:
		// read, unread and snoozed are this person's record, and the
		// notices this page may return are a function of it. Read in the
		// same transaction, so the filter and the rows it selects are one
		// instant.
		notices, next, err := readInbox(ctx, tx, handle, q,
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
// record — each list at most [inbox.MaxEntries] ids, bound as ONE JSON array
// parameter — and everything it compares is a column of the row the index
// range is already reading.
//
// EVERY SET IT BINDS IS THE CLASSIFIER'S ([inbox.Classifier]), the same
// lookups the page is then marked by, so the scan and the marks are one rule:
// a notice this filter hides is exactly one the page would have marked.
type inboxFilter struct {
	// reasons is the set a row's reason must be in — `reasons` intersected
	// with the primary split under `primary_only` — and nil for every
	// reason. restricted distinguishes nil from an intersection that came
	// out EMPTY, which is a page of nothing rather than a page of all.
	reasons    []Reason
	restricted bool

	// unread keeps only what [markInbox] would mark unread.
	unread bool
	seen   inbox.Position
	read   []string
	marked []string

	// asleep is the live snoozes, and scope what to do with them.
	scope  inbox.SnoozeScope
	asleep []string
}

// inboxFilterOf resolves the query against the person's own record, as the
// classifier the page is marked by holds it.
//
// A SNOOZE IS LIVE BY THE SAME TEST THE PAGE IS MARKED BY
// ([inbox.Classifier.Asleep]) — no instant, or one after now — so a notice this
// filter hides is exactly a notice the page would have marked snoozed, and one
// whose time has come is back under every scope. Every set is SORTED there, so
// one question is one statement.
func inboxFilterOf(q InboxQuery, marks inbox.Classifier, now time.Time) inboxFilter {
	f := inboxFilter{unread: q.Unread, seen: marks.Seen(), scope: q.Snoozed,
		read: marks.ReadMarks(), marked: marks.UnreadMarks(), asleep: marks.Asleep(now)}
	if len(q.Reasons) > 0 || q.PrimaryOnly {
		f.restricted = true
		for _, reason := range Reasons {
			if len(q.Reasons) > 0 && !slices.Contains(q.Reasons, reason) {
				continue
			}
			if q.PrimaryOnly && !marks.Primary(string(reason)) {
				continue
			}
			f.reasons = append(f.reasons, reason)
		}
	}
	return f
}

// clauses is the filter as WHERE terms over `tracker_notifications n`, and
// false when it can select nothing at all.
func (f inboxFilter) clauses() ([]string, []any, bool) {
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
			coveredArgs = []any{f.seen.Stream, int64(f.seen.Packed())}
		}
		where = append(where, "(n.record_id IN (SELECT value FROM json_each(?)) "+
			"OR (n.record_id NOT IN (SELECT value FROM json_each(?)) "+
			"AND NOT "+covered+"))")
		args = append(args, idList(f.marked), idList(f.read))
		args = append(args, coveredArgs...)
	}
	switch f.scope {
	case inbox.SnoozeExclude:
		if len(f.asleep) > 0 {
			where = append(where, "n.record_id NOT IN (SELECT value FROM json_each(?))")
			args = append(args, idList(f.asleep))
		}
	case inbox.SnoozeOnly:
		if len(f.asleep) == 0 {
			return nil, nil, false
		}
		where = append(where, "n.record_id IN (SELECT value FROM json_each(?))")
		args = append(args, idList(f.asleep))
	case inbox.SnoozeInclude:
	}
	return where, args, true
}

// inboxStatement is [readInbox]'s one statement over its predicate, the last
// placeholder the page's limit — named so the plan test explains exactly what
// the reader issues.
func inboxStatement(where []string) string {
	return `
		SELECT n.record_id, n.log_seq, n.log_stream, n.log_generation,
		       n.created_at, n.reason, n.addressed, n.fallback_only,
		       n.kind, n.subject_id, n.subject_key, n.task_id,
		       ` + keyOpensAnother("n.subject_key", "n.task_id") + `,
		       n.excerpt, COALESCE(h.actor, ''), COALESCE(h.actor_kind, ''),
		       COALESCE(h.comment_id, ''), COALESCE(h.turn_id, '')
		  FROM tracker_notifications n
		  LEFT JOIN tracker_history h ON h.id = n.record_id
		 WHERE ` + strings.Join(where, " AND ") + `
		 ORDER BY n.log_seq DESC
		 LIMIT ?`
}

// readInbox reads one page of the notification rows.
//
// THE JOIN IS LEFT, because the two tables have different lifetimes: the
// notification rows are swept on the inbox retention horizon and the history
// rows are never swept, but a reanchor rebuilds the history from the log and a
// notice can outlive the row it names. What was said outlives who said it.
func readInbox(ctx context.Context, tx *sql.Tx, handle string, q InboxQuery,
	filter inboxFilter, limit int) ([]InboxNotice, string, error) {

	where := []string{"n.recipient = ?"}
	args := []any{handle}

	// THE KEYSET IS THE COMPOSED POSITION, for the reason the activity
	// feed gives: `log_seq` already carries `(generation << 40) | seq`, so
	// the generation is IN the ordering rather than beside it — which is
	// what lets a cursor span a reanchor with no gap and no repeat.
	if cursor := strings.TrimSpace(q.Cursor); cursor != "" {
		at, err := statelog.ParsePosition(cursor)
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
	narrowed, narrowedArgs, selects := filter.clauses()
	if !selects {
		return []InboxNotice{}, "", nil
	}
	where = append(where, narrowed...)
	args = append(args, narrowedArgs...)
	args = append(args, limit+1)

	rows, err := tx.QueryContext(ctx, inboxStatement(where), args...)
	if err != nil {
		return nil, "", fmt.Errorf("worklet: read %s's inbox: %w", handle, err)
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
			&kind, &notice.SubjectID, &notice.SubjectKey, &notice.Task,
			&notice.SubjectKeyCollision, &notice.Excerpt,
			&notice.Actor, &actorKind, &notice.CommentID,
			&notice.TurnID); err != nil {

			return nil, "", fmt.Errorf("worklet: scan %s's inbox: %w", handle, err)
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
		return nil, "", fmt.Errorf("worklet: read %s's inbox: %w", handle, err)
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

// markInbox classifies and marks the page against this person's own record —
// the classifier built from it in the snapshot the page was read in — by the
// one rule every domain's inbox reads by ([inbox.Classifier.Classify]).
func markInbox(notices []InboxNotice, marks inbox.Classifier, now time.Time) []InboxNotice {
	for i := range notices {
		notice := &notices[i]
		mark := marks.Classify(inbox.Ref{
			RecordID: notice.RecordID,
			At: inbox.Position{Stream: notice.LogStream,
				Generation: notice.LogGeneration, Seq: notice.LogSeq},
			Reason: string(notice.Reason),
		}, now)
		notice.Primary, notice.Read = mark.Primary, mark.Read
		notice.Snoozed, notice.SnoozedUntil = mark.Snoozed, mark.SnoozedUntil
	}
	return notices
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
// and the parameter ceiling is the ENGINE's: [store.Capabilities.MaxVariables]
// reports the probe's own 32 766 on the pinned driver, whose real limit lies
// past it, but an engine in this family may stop at the 999 the probe falls
// back to — and three full lists are 768 ids ([inbox.MaxEntries] each) before
// the page's own arguments. One bind is one parameter at any length, so the
// statement fits every engine and no list's length is a reason it fails. An
// empty list is "[]", the one spelling of an empty set: `NOT IN` over it keeps
// every row and `IN` none.
func idList(ids []string) string {
	if len(ids) == 0 {
		return "[]"
	}
	return jsonOf(ids)
}
