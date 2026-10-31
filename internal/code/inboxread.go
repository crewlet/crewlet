package code

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/textcut"
)

// ONE PERSON'S CODE INBOX — the `inbox` read: the notices the applier wrote
// for them, one row per record that reached them (code_notifications,
// apply_notices.go), newest first on the code log, read beside their own
// marks (code_person_marks, marks.go) in ONE snapshot, each notice marked by
// internal/inbox's one rule and carrying what its change says.
//
// # Every narrowing is in the scan
//
// The snooze scope, `unread`, the reasons and `primary_only` are predicates
// on the one index range the read walks — code_notifications_inbox_idx,
// (recipient, log_seq DESC) — each bound from the person's own record, which
// the same transaction read first: so a page holds `limit` notices whenever
// the table holds them, and its cursor means what it says. Filtered on the
// page after it was read, a person with sixty snoozed notices among their
// newest fifty would open an inbox of nothing (the tracker's own lesson,
// internal/tracker's inboxread.go). Every set it binds is the Classifier's
// ([inbox.Classifier]), the lookups the page is then marked by, so a notice
// the scan hides is exactly one the page would have marked. ONE STATEMENT
// whatever is asked: a filter not asked is bound to say so ([inboxScanSQL]),
// never left out of a statement built per call.
//
// # Held to the repository's grant
//
// A notice is about a change on a repository, and is answered only where its
// asker may read that repository ([JudgeRead]) — a person reads every one
// (ADR-0058); a seat leading the person reads by its own grant. That is the
// one narrowing the scan cannot state, since the grant is the chart's: a page
// leaves out a notice its asker may not read, and its cursor is still the
// last notice it scanned, as every listing across repositories pages
// ([Reader.repos]).
//
// # What each notice carries of its change
//
// Its title, its state, the work item it implements, the first
// [MaxNoticeBody] bytes of its description and the agent findings standing
// open on it by severity — read for the page's changes in the same snapshot,
// one statement each over the page's changes: what a person decides a notice
// by without opening the change.

// MaxNoticeBody is how much of a change's description a notice carries: ONE
// KIBIBYTE, cut at a character — a card's worth of the intent behind the
// change, beside its title; the change's own page reads the whole.
const MaxNoticeBody = 1 << 10

// InboxQuery is an `inbox` read's question.
type InboxQuery struct {
	// Handle is whose inbox: the human seat a person holds. Required.
	Handle string `json:"handle"`

	// Since is a lower bound on the code log, exclusive: a caller resuming
	// from where they marked read.
	Since statelog.Position `json:"since,omitzero"`

	// Reasons narrows to those reasons, empty for every one; PrimaryOnly to
	// the person's primary half. Each FILTERS, where the primary split
	// classifies.
	Reasons     []Reason `json:"reasons,omitempty"`
	PrimaryOnly bool     `json:"primary_only,omitempty"`

	// Unread drops what the person's marks read as read.
	Unread bool `json:"unread,omitempty"`

	// Snoozed is what the read does with notices the person put off:
	// REQUIRED, one of [inbox.SnoozeScopes] — a surface resolves an absent
	// one to [inbox.SnoozeExclude] before it reads.
	Snoozed inbox.SnoozeScope `json:"snoozed"`

	// Now is the instant a snooze is judged live at, the asking node's —
	// zero takes the serving node's clock.
	Now time.Time `json:"now,omitzero"`
}

// InboxNotice is one thing the code host told a person.
type InboxNotice struct {
	// RecordID is the record that reached them, which a mark names.
	RecordID string `json:"record_id"`

	// The record's position on the code log, the triple every cursor uses.
	LogSeq        uint64 `json:"log_seq"`
	LogStream     string `json:"log_stream"`
	LogGeneration uint32 `json:"log_generation"`

	// At is the record's broker instant.
	At time.Time `json:"at"`

	// Reason is the one reason the person hears under, Addressed whether it
	// asks something of them, Fallback whether it reached them only because
	// nobody better was found, and Primary which half of their split it
	// fell in.
	Reason    Reason `json:"reason"`
	Primary   bool   `json:"primary"`
	Addressed bool   `json:"addressed"`
	Fallback  bool   `json:"fallback,omitempty"`

	// Repo is the change's repository by name, ChangeID and ChangeRef the
	// change (web!42) as its record named it, and Excerpt what the record
	// said, its first MaxNoticeExcerpt bytes.
	Repo      string `json:"repo"`
	ChangeID  string `json:"change_id"`
	ChangeRef string `json:"change_ref"`
	Excerpt   string `json:"excerpt,omitempty"`

	// Change is what the change says now, read in the page's snapshot;
	// nil for a change no row holds any more.
	Change *NoticeChange `json:"change,omitempty"`

	// Read, Snoozed and SnoozedUntil are the person's own mark over it.
	Read         bool       `json:"read"`
	Snoozed      bool       `json:"snoozed,omitempty"`
	SnoozedUntil *time.Time `json:"snoozed_until,omitzero"`
}

// NoticeChange is what a notice carries of its change.
type NoticeChange struct {
	Title    string          `json:"title"`
	State    ChangeState     `json:"state"`
	WorkItem *types.WorkItem `json:"work_item,omitempty"`
	// Body is the description's first [MaxNoticeBody] bytes, and BodyCut
	// whether there is more of it.
	Body    string `json:"body,omitempty"`
	BodyCut bool   `json:"body_cut,omitempty"`
	// AgentFindings counts the findings seats raised that stand open on the
	// change, by severity; absent for none.
	AgentFindings map[Severity]int `json:"agent_findings,omitempty"`
}

// InboxPage is a page of one person's code inbox.
type InboxPage struct {
	Handle  string        `json:"handle"`
	Notices []InboxNotice `json:"notices"`

	// PrimaryReasons is the split APPLIED — the person's own, or
	// [DefaultPrimaryReasons] where they declared none.
	PrimaryReasons []Reason `json:"primary_reasons"`

	// SeenThrough is where the person's marks say they have read to on the
	// code log.
	SeenThrough inbox.Position `json:"seen_through,omitzero"`

	// Unread and Primary count the PAGE's notices.
	Unread  int `json:"unread"`
	Primary int `json:"primary"`
}

// validateInbox is [ReadQuery.validate] for an `inbox` read.
func (q ReadQuery) validateInbox() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	in := q.Inbox
	switch {
	case in == nil:
		return refused("inbox", "an inbox read names whose inbox it reads")
	case strings.TrimSpace(in.Handle) == "":
		return refused("inbox.handle", "an inbox read names whose inbox it reads, by the seat they hold")
	case !in.Snoozed.Valid():
		return refused("inbox.snoozed", fmt.Sprintf("%q is not one of %v — a surface resolves an absent scope "+
			"to %q before it reads", in.Snoozed, inbox.SnoozeScopes, inbox.SnoozeExclude))
	case !in.Since.IsZero() && in.Since.Stream != topics.CodeLogStream:
		return refused("inbox.since", fmt.Sprintf("%s is a position on another log than %s", in.Since,
			topics.CodeLogStream))
	}
	for _, r := range in.Reasons {
		if !r.Valid() {
			return refused("inbox.reasons", fmt.Sprintf("%q is not one of %v", r, Reasons()))
		}
	}
	if q.Cursor != "" {
		if at, err := statelog.ParsePosition(q.Cursor); err != nil || at.Stream != topics.CodeLogStream {
			return fmt.Errorf("%w: %q is not a notice's place on the code log", ErrBadCursor, q.Cursor)
		}
	}
	return nil
}

// inbox serves one page of q.Inbox.Handle's code inbox.
func (r *Reader) inbox(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	in := q.Inbox
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	marks, _, err := marksIn(ctx, tx, in.Handle)
	if err != nil {
		return err
	}
	primary := marks.PrimaryReasons
	if len(primary) == 0 {
		primary = slices.Clone(DefaultPrimaryReasons)
	}
	classifier := marks.Classifier(primary)
	page := &InboxPage{Handle: in.Handle, PrimaryReasons: primary, SeenThrough: marks.SeenThrough,
		Notices: []InboxNotice{}}
	out.Inbox = page

	args, selects, err := inboxScanArgs(q, classifier, now)
	if err != nil || !selects {
		return err
	}
	limit := min(limitOr(q.Limit, inbox.MaxRows), r.pageRows)
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, inboxScanSQL, args...)
	if err != nil {
		return fmt.Errorf("code: read %s's code inbox: %w", in.Handle, err)
	}
	var scanned []InboxNotice
	var repoIDs []string
	for rows.Next() {
		var (
			n                   InboxNotice
			packed, at          int64
			reason, repoID      string
			addressed, fallback int
		)
		if err := rows.Scan(&n.RecordID, &packed, &n.LogStream, &n.LogGeneration, &at, &reason, &addressed,
			&fallback, &repoID, &n.ChangeID, &n.ChangeRef, &n.Excerpt); err != nil {
			_ = rows.Close()
			return fmt.Errorf("code: read %s's code inbox: %w", in.Handle, err)
		}
		n.LogSeq = uint64(packed) % statelog.GenerationStride
		n.At = store.DecodeTime(at)
		n.Reason, n.Addressed, n.Fallback = Reason(reason), addressed != 0, fallback != 0
		n.Repo = repoID
		scanned = append(scanned, n)
		repoIDs = append(repoIDs, repoID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("code: read %s's code inbox: %w", in.Handle, err)
	}
	_ = rows.Close()
	// THE CURSOR IS THE LAST NOTICE SCANNED, whatever the grant keeps of
	// the page — this file's head.
	if len(scanned) == limit {
		last := scanned[len(scanned)-1]
		out.Next = statelog.Position{Stream: last.LogStream, Generation: last.LogGeneration,
			Seq: last.LogSeq}.String()
	}
	repos, err := listedReposAmong(ctx, tx, repoIDs, q.Asker, q.CeilingSeat, chart, "a page of an inbox")
	if err != nil {
		return err
	}
	var kept []InboxNotice
	var changes []string
	for _, n := range scanned {
		repo, listed := repos[n.Repo]
		if !listed {
			continue
		}
		n.Repo = repo.ref.Name
		kept = append(kept, n)
		if !slices.Contains(changes, n.ChangeID) {
			changes = append(changes, n.ChangeID)
		}
	}
	said, err := noticeChangesIn(ctx, tx, changes)
	if err != nil {
		return err
	}
	for _, n := range kept {
		n.Change = said[n.ChangeID]
		mark := classifier.Classify(inbox.Ref{RecordID: n.RecordID,
			At:     inbox.Position{Stream: n.LogStream, Generation: n.LogGeneration, Seq: n.LogSeq},
			Reason: string(n.Reason)}, now)
		n.Primary, n.Read, n.Snoozed, n.SnoozedUntil = mark.Primary, mark.Read, mark.Snoozed, mark.SnoozedUntil
		if !n.Read {
			page.Unread++
		}
		if n.Primary {
			page.Primary++
		}
		page.Notices = append(page.Notices, n)
	}
	return nil
}

// inboxScanArgs binds [inboxScanSQL]'s filters from q and the person's marks,
// and false where the filters select nothing at all — a reason filter whose
// intersection with the primary split is empty, or only the snoozed notices
// of a person who snoozed none.
func inboxScanArgs(q ReadQuery, marks inbox.Classifier, now time.Time) ([]any, bool, error) {
	in := q.Inbox
	before := int64(math.MaxInt64)
	if q.Cursor != "" {
		at, err := statelog.ParsePosition(q.Cursor)
		if err != nil {
			return nil, false, fmt.Errorf("%w: %q is not a notice's place on the code log", ErrBadCursor, q.Cursor)
		}
		before = at.Packed()
	}
	var after int64
	if !in.Since.IsZero() {
		after = in.Since.Packed()
	}
	restricted := len(in.Reasons) > 0 || in.PrimaryOnly
	var reasons []string
	for _, reason := range Reasons() {
		if len(in.Reasons) > 0 && !slices.Contains(in.Reasons, reason) {
			continue
		}
		if in.PrimaryOnly && !marks.Primary(string(reason)) {
			continue
		}
		reasons = append(reasons, string(reason))
	}
	if restricted && len(reasons) == 0 {
		return nil, false, nil
	}
	asleep := marks.Asleep(now)
	if in.Snoozed == inbox.SnoozeOnly && len(asleep) == 0 {
		return nil, false, nil
	}
	seen := marks.Seen()
	var seenStream string
	var seenAt int64
	if seen.Seq != 0 && seen.Stream != "" {
		seenStream, seenAt = seen.Stream, int64(seen.Packed())
	}
	lists := make([]string, 0, 4)
	for _, ids := range [][]string{reasons, marks.UnreadMarks(), marks.ReadMarks(), asleep} {
		list, err := jsonList(ids)
		if err != nil {
			return nil, false, err
		}
		lists = append(lists, list)
	}
	flag := func(on bool) int {
		if on {
			return 1
		}
		return 0
	}
	return []any{in.Handle, before, after,
		flag(restricted), lists[0],
		flag(in.Unread), lists[1], lists[2], seenStream, seenAt,
		string(in.Snoozed), lists[3], lists[3]}, true, nil
}

// noticeChangesIn is what each change of ids says now, by its id — its row,
// its description cut at [MaxNoticeBody], and the agent findings standing open
// on it by severity — one statement each over the page's changes.
func noticeChangesIn(ctx context.Context, tx *sql.Tx, ids []string) (map[string]*NoticeChange, error) {
	out := map[string]*NoticeChange{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, noticeChangesSQL, list)
	if err != nil {
		return nil, fmt.Errorf("code: read the changes an inbox page names: %w", err)
	}
	for rows.Next() {
		var (
			id, state, backend, workID, key, project, body string
			c                                              NoticeChange
		)
		if err := rows.Scan(&id, &c.Title, &state, &backend, &workID, &key, &project, &body); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("code: read the changes an inbox page names: %w", err)
		}
		c.State = ChangeState(state)
		if workID != "" {
			c.WorkItem = &types.WorkItem{Backend: types.WorkBackend(backend), ID: workID, Key: key, Project: project}
		}
		c.Body = textcut.Within(body, MaxNoticeBody)
		c.BodyCut = len(c.Body) < len(body)
		out[id] = &c
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("code: read the changes an inbox page names: %w", err)
	}
	_ = rows.Close()
	rows, err = tx.QueryContext(ctx, noticeFindingsSQL, list, string(FindingOpen), string(PartyAgent))
	if err != nil {
		return nil, fmt.Errorf("code: count the findings of the changes an inbox page names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, severity string
		if err := rows.Scan(&id, &severity); err != nil {
			return nil, fmt.Errorf("code: count the findings of the changes an inbox page names: %w", err)
		}
		c, found := out[id]
		if !found {
			continue
		}
		if c.AgentFindings == nil {
			c.AgentFindings = map[Severity]int{}
		}
		c.AgentFindings[Severity(severity)]++
	}
	return out, rows.Err()
}

// The inbox's statements, each registered in plan_test.go.
//
// inboxScanSQL is ONE range of code_notifications_inbox_idx — the person's
// notices below the cursor and above `since`, newest first — with every
// filter bound, and a filter not asked bound to pass: the reasons (a flag and
// the set), `unread` by internal/inbox's rule as SQL — an unread mark outranks
// everything, otherwise a notice is read when the read list names it or the
// seen-through position covers it ON THE SAME STREAM, compared packed — and
// the snooze scope, `exclude` hiding the live snoozes and `only` keeping
// nothing else. Each set is ONE JSON array parameter, the tracker's reason
// (its idList): the lists are the person's own, each at most inbox.MaxEntries.
//
// noticeChangesSQL reads each change of the page by its key, its body by its
// own; noticeFindingsSQL reads each change's open agent findings' severities
// from the raiser index's range of the change, counted here rather than
// grouped by the statement — a GROUP BY on a column the index does not lead
// with sorts what it found, and the rows are a page's changes' findings.
const (
	inboxScanSQL = `SELECT n.record_id, n.log_seq, n.log_stream, n.log_generation, n.created_at, n.reason,
			n.addressed, n.fallback_only, n.repo_id, n.change_id, n.change_ref, n.excerpt
		FROM code_notifications AS n INDEXED BY code_notifications_inbox_idx
		WHERE n.recipient = ? AND n.log_seq < ? AND n.log_seq > ?
			AND (NOT ? OR n.reason IN (SELECT value FROM json_each(?)))
			AND (NOT ? OR n.record_id IN (SELECT value FROM json_each(?))
				OR (n.record_id NOT IN (SELECT value FROM json_each(?))
					AND NOT (n.log_stream = ? AND n.log_seq <= ?)))
			AND CASE ? WHEN 'exclude' THEN n.record_id NOT IN (SELECT value FROM json_each(?))
				WHEN 'only' THEN n.record_id IN (SELECT value FROM json_each(?)) ELSE 1 END
		ORDER BY n.log_seq DESC
		LIMIT ?`
	noticeChangesSQL = `SELECT c.id, c.title, c.state, c.work_backend, c.work_id, c.work_key, c.work_project,
			COALESCE(b.body, '')
		FROM json_each(?) AS j CROSS JOIN code_changes AS c INDEXED BY sqlite_autoindex_code_changes_1
			LEFT JOIN code_change_bodies AS b ON b.change_id = c.id
		WHERE c.id = CAST(j.value AS TEXT)`
	noticeFindingsSQL = `SELECT f.change_id, f.severity
		FROM json_each(?) AS j CROSS JOIN code_findings AS f INDEXED BY code_findings_raiser_idx
		WHERE f.change_id = CAST(j.value AS TEXT) AND f.state = ? AND f.raiser_kind = ?`
)
