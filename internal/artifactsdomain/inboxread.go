package artifactsdomain

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// ONE PERSON'S ARTIFACTS INBOX: their notices newest first, with their marks
// and the head of each live artifact named, in ONE snapshot; the caller holds
// each notice to that head, and every filter is in the one index scan.

// InboxQuery is an artifacts inbox read's question.
type InboxQuery struct {
	// Handle is whose inbox: the human seat a person holds. Required.
	Handle string `json:"handle"`

	// Unread drops what the person's marks read as read; PrimaryOnly keeps
	// their primary half.
	Unread      bool `json:"unread,omitempty"`
	PrimaryOnly bool `json:"primary_only,omitempty"`

	// Snoozed is what the read does with notices the person put off:
	// REQUIRED, one of [inbox.SnoozeScopes].
	Snoozed inbox.SnoozeScope `json:"snoozed"`

	// Limit is the page's size, at most [inbox.MaxRows]; zero is that.
	// Cursor is the last notice's place of the page before, "" for the
	// first.
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`

	// Now is the instant a snooze is judged live at, the asking node's;
	// zero takes the serving node's clock.
	Now time.Time `json:"now,omitzero"`

	// Fresh is how fresh the page must be; an absent level is `stale`.
	Fresh statelog.Freshness `json:"fresh,omitzero"`
}

// InboxNotice is one comment record that told a person.
type InboxNotice struct {
	// RecordID is the notice's identity, which a mark names: the record's
	// operation id.
	RecordID string `json:"record_id"`

	// The record's position on the artifacts log.
	LogSeq        uint64 `json:"log_seq"`
	LogStream     string `json:"log_stream"`
	LogGeneration uint32 `json:"log_generation"`

	// At is the record's broker instant.
	At time.Time `json:"at"`

	// Reason is why it reached them, Addressed whether it asks something of
	// them, and Primary which half of their split it fell in.
	Reason    NotifyReason `json:"reason"`
	Primary   bool         `json:"primary"`
	Addressed bool         `json:"addressed"`

	// Op is what the record did to the thread: opened it, replied in it,
	// resolved or reopened it.
	Op OpKind `json:"op"`

	// Artifact, Thread and Remark are what it is about; Remark is empty for
	// a resolve or a reopen.
	Artifact string `json:"artifact"`
	Thread   string `json:"thread"`
	Remark   string `json:"remark,omitempty"`

	// Excerpt is the first [MaxNoticeExcerpt] bytes of the remark it made,
	// empty for none and once the remark was removed.
	Excerpt string `json:"excerpt,omitempty" untrusted:"fence"`

	// Actor is who wrote the record.
	Actor     string        `json:"actor"`
	ActorKind iam.ActorKind `json:"actor_kind"`

	// Title and ArtifactKind are the artifact as it stands, from the page's
	// heads: what the surface draws once it has decided the notice is shown.
	Title        string         `json:"title,omitempty" untrusted:"name"`
	ArtifactKind artifacts.Kind `json:"artifact_kind,omitempty"`

	// ThreadState is the thread as it stands: a card's alone.
	ThreadState *NoticeThread `json:"thread_state,omitempty"`

	// Read, Snoozed and SnoozedUntil are the person's own mark over it.
	Read         bool       `json:"read"`
	Snoozed      bool       `json:"snoozed,omitempty"`
	SnoozedUntil *time.Time `json:"snoozed_until,omitzero"`
}

// NoticeThread is what a notice's card draws of its thread, as it stands.
type NoticeThread struct {
	Status ThreadStatus `json:"status"`

	// Version is the version the thread was made on, and Quote the text its
	// commenter selected, empty for none.
	Version int    `json:"version"`
	Quote   string `json:"quote,omitempty" untrusted:"fence"`

	// Remarks is how many remarks of the thread are not removed.
	Remarks int `json:"remarks"`

	// ResolvedBy and ResolvedVersion are the last resolve's, while resolved.
	ResolvedBy      string `json:"resolved_by,omitempty"`
	ResolvedVersion int    `json:"resolved_version,omitempty"`
}

// ThreadRef names one thread of one artifact.
type ThreadRef struct {
	Artifact string `json:"artifact"`
	Thread   string `json:"thread"`
}

// MaxNoticeThreads is the most threads one card read names: what one composed
// page of an inbox can name, a notice a thread at the most.
const MaxNoticeThreads = inbox.MaxRows

// InboxPage is a page of one person's artifacts inbox.
type InboxPage struct {
	Handle  string        `json:"handle"`
	Notices []InboxNotice `json:"notices"`

	// Heads is every LIVE artifact the page's notices name, by id, as the
	// snapshot held it: what the surface decides each notice by.
	Heads map[string]Head `json:"heads"`

	// PrimaryReasons is the split APPLIED: the person's own, or
	// [DefaultPrimaryReasons] where they declared none.
	PrimaryReasons []NotifyReason `json:"primary_reasons"`

	// SeenThrough is where the person's marks say they have read to.
	SeenThrough inbox.Position `json:"seen_through,omitzero"`

	// Next is the place of the last notice SCANNED, "" when the scan
	// reached the end.
	Next string `json:"next,omitempty"`

	Level    statelog.ReadLevel `json:"read_level"`
	Position statelog.Position  `json:"position"`
	Complete bool               `json:"complete"`
}

// validate refuses a question no node could answer, naming the field.
func (q InboxQuery) validate() error {
	stream := Domain{}.Stream().Name
	switch {
	case strings.TrimSpace(q.Handle) == "":
		return fmt.Errorf("%w: handle: an inbox read names whose inbox it reads", ErrInvalid)
	case !q.Snoozed.Valid():
		return fmt.Errorf("%w: snoozed: %q is not one of %s", ErrInvalid, q.Snoozed, words.Or(inbox.SnoozeScopes))
	case q.Limit < 0:
		return fmt.Errorf("%w: limit: %d is not a page size", ErrInvalid, q.Limit)
	}
	if q.Cursor != "" {
		if at, err := statelog.ParsePosition(q.Cursor); err != nil || at.Stream != stream {
			return fmt.Errorf("%w: %q is not a notice's place on the artifacts log", ErrBadCursor, q.Cursor)
		}
	}
	return nil
}

// Inbox answers one page of q.Handle's artifacts inbox, in one snapshot.
func (r *Reader) Inbox(ctx context.Context, q InboxQuery) (InboxPage, error) {
	if err := q.validate(); err != nil {
		return InboxPage{}, err
	}
	now := q.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	limit := q.Limit
	if limit == 0 || limit > inbox.MaxRows {
		limit = inbox.MaxRows
	}
	var page InboxPage
	served, err := r.log.Read(ctx, staleFresh(q.Fresh).Query(DomainScope(), true), func(tx *sql.Tx) error {
		var err error
		page, err = inboxIn(ctx, tx, q, limit, now)
		return err
	})
	if err != nil {
		return InboxPage{}, err
	}
	page.Level, page.Position, page.Complete = served.Level, served.Position, served.Complete
	return page, nil
}

// inboxIn is one page of q's inbox in tx.
func inboxIn(ctx context.Context, tx *sql.Tx, q InboxQuery, limit int, now time.Time) (InboxPage, error) {
	marks, _, err := marksIn(ctx, tx, q.Handle)
	if err != nil {
		return InboxPage{}, err
	}
	primary := marks.PrimaryReasons
	if len(primary) == 0 {
		primary = slices.Clone(DefaultPrimaryReasons)
	}
	classifier := marks.Classifier(primary)
	page := InboxPage{Handle: q.Handle, Notices: []InboxNotice{}, Heads: map[string]Head{},
		PrimaryReasons: primary, SeenThrough: marks.SeenThrough}
	scan, selects, err := inboxScanOf(q, classifier, now)
	if err != nil || !selects {
		return page, err
	}
	scanned, err := scan.run(ctx, tx, limit)
	if err != nil {
		return InboxPage{}, err
	}
	if len(scanned) == limit {
		last := scanned[len(scanned)-1]
		page.Next = statelog.Position{Stream: last.LogStream, Generation: last.LogGeneration,
			Seq: last.LogSeq}.String()
	}
	for _, n := range scanned {
		if _, seen := page.Heads[n.Artifact]; !seen {
			head, found, err := readHeadTx(ctx, tx, n.Artifact) //nolint:govet // shadow: scoped to this loop
			if err != nil {
				return InboxPage{}, err
			}
			if found && head.Status == StatusLive {
				page.Heads[n.Artifact] = head
			}
		}
		mark := classifier.Classify(inbox.Ref{RecordID: n.RecordID,
			At:     inbox.Position{Stream: n.LogStream, Generation: n.LogGeneration, Seq: n.LogSeq},
			Reason: string(n.Reason)}, now)
		n.Primary, n.Read, n.Snoozed, n.SnoozedUntil = mark.Primary, mark.Read, mark.Snoozed, mark.SnoozedUntil
		if head, live := page.Heads[n.Artifact]; live {
			n.Title, n.ArtifactKind = head.Title, head.Kind
		}
		page.Notices = append(page.Notices, n)
	}
	return page, nil
}

// inboxScan is one inbox read's scan: whose notices, the exclusive bounds on
// the artifacts log it reads between, and every filter its statements bind,
// each set ONE JSON array, the Classifier's.
type inboxScan struct {
	handle        string
	before, after int64

	restricted bool
	reasons    string

	unread                 bool
	unreadMarks, readMarks string
	seenStream             string
	seenAt                 int64

	scope  string
	asleep string
}

// floor is the range's exclusive lower bound, raised under `unread` to the
// seen-through position on the artifacts log, which nothing below is read
// through but what the unread list names.
func (s inboxScan) floor() int64 {
	if s.unread && s.seenStream == (Domain{}).Stream().Name && s.seenAt > s.after {
		return s.seenAt
	}
	return s.after
}

// run is up to limit of the scan's notices, newest first: the range above
// [inboxScan.floor], and where the floor was raised, the notices the unread
// list names below it, merged in.
func (s inboxScan) run(ctx context.Context, tx *sql.Tx, limit int) ([]InboxNotice, error) {
	flag := func(on bool) int {
		if on {
			return 1
		}
		return 0
	}
	floor := s.floor()
	out, err := scanInbox(ctx, tx, s.handle, inboxScanSQL, s.handle, s.before, floor, flag(s.restricted),
		s.reasons, flag(s.unread), s.unreadMarks, s.readMarks, s.seenStream, s.seenAt, s.scope, s.asleep,
		s.asleep, limit)
	if err != nil || floor == s.after || s.unreadMarks == "[]" {
		return out, err
	}
	marked, err := scanInbox(ctx, tx, s.handle, inboxMarkedSQL, s.unreadMarks, s.handle, min(s.before, floor+1),
		s.after, flag(s.restricted), s.reasons, s.scope, s.asleep, s.asleep)
	if err != nil || len(marked) == 0 {
		return out, err
	}
	out = append(out, marked...)
	slices.SortStableFunc(out, func(a, b InboxNotice) int {
		return cmp.Or(cmp.Compare(b.LogGeneration, a.LogGeneration), cmp.Compare(b.LogSeq, a.LogSeq))
	})
	return out[:min(len(out), limit)], nil
}

// scanInbox runs statement, [inboxScanSQL] or [inboxMarkedSQL], with args for
// handle's inbox.
func scanInbox(ctx context.Context, tx *sql.Tx, handle, statement string, args ...any) ([]InboxNotice, error) {
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: read %s's inbox: %w", handle, err)
	}
	defer func() { _ = rows.Close() }()
	var out []InboxNotice
	for rows.Next() {
		var (
			n                     InboxNotice
			packed, at            int64
			reason, op, actorKind string
			addressed             int
		)
		if err := rows.Scan(&n.RecordID, &packed, &n.LogStream, &n.LogGeneration, &at, &reason, &addressed, &op,
			&n.Artifact, &n.Thread, &n.Remark, &n.Excerpt, &n.Actor, &actorKind); err != nil {
			return nil, fmt.Errorf("artifactsdomain: read %s's inbox: %w", handle, err)
		}
		n.LogSeq = uint64(packed) % statelog.GenerationStride
		n.At = store.DecodeTime(at)
		n.Reason, n.Addressed, n.Op, n.ActorKind = NotifyReason(reason), addressed != 0, OpKind(op),
			iam.ActorKind(actorKind)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifactsdomain: read %s's inbox: %w", handle, err)
	}
	return out, nil
}

// inboxScanOf is the scan q asks for, its filters bound from the person's
// marks, and false where they select nothing (a primary half with no reason,
// or the snoozed notices of a person who snoozed none).
func inboxScanOf(q InboxQuery, marks inbox.Classifier, now time.Time) (inboxScan, bool, error) {
	s := inboxScan{handle: q.Handle, before: math.MaxInt64, restricted: q.PrimaryOnly, unread: q.Unread,
		scope: string(q.Snoozed)}
	if q.Cursor != "" {
		at, err := statelog.ParsePosition(q.Cursor)
		if err != nil {
			return inboxScan{}, false, fmt.Errorf("%w: %q is not a notice's place on the artifacts log",
				ErrBadCursor, q.Cursor)
		}
		s.before = at.Packed()
	}
	var reasons []string
	for _, reason := range NotifyReasons {
		if !q.PrimaryOnly || marks.Primary(string(reason)) {
			reasons = append(reasons, string(reason))
		}
	}
	if s.restricted && len(reasons) == 0 {
		return inboxScan{}, false, nil
	}
	asleep := marks.Asleep(now)
	if q.Snoozed == inbox.SnoozeOnly && len(asleep) == 0 {
		return inboxScan{}, false, nil
	}
	if seen := marks.Seen(); seen.Seq != 0 && seen.Stream != "" {
		s.seenStream, s.seenAt = seen.Stream, int64(seen.Packed())
	}
	for _, list := range []struct {
		ids []string
		to  *string
	}{{reasons, &s.reasons}, {marks.UnreadMarks(), &s.unreadMarks}, {marks.ReadMarks(), &s.readMarks},
		{asleep, &s.asleep}} {
		if list.ids == nil {
			list.ids = []string{}
		}
		b, err := json.Marshal(list.ids)
		if err != nil {
			return inboxScan{}, false, fmt.Errorf("artifactsdomain: bind %s's inbox filters: %w", q.Handle, err)
		}
		*list.to = string(b)
	}
	return s, true, nil
}

// NoticeThreads answers the thread each ref names, as a card draws it, in the
// refs' order: nil where no row holds the thread any more. At most
// [MaxNoticeThreads] refs, each once.
func (r *Reader) NoticeThreads(ctx context.Context, refs []ThreadRef, fresh statelog.Freshness) (
	[]*NoticeThread, error) {

	switch {
	case len(refs) > MaxNoticeThreads:
		return nil, fmt.Errorf("%w: %d threads named, past the %d one page of an inbox can name", ErrInvalid,
			len(refs), MaxNoticeThreads)
	case len(refs) == 0:
		return []*NoticeThread{}, nil
	}
	list, err := json.Marshal(refs)
	if err != nil {
		return nil, fmt.Errorf("artifactsdomain: name the threads an inbox page's cards read: %w", err)
	}
	held := map[ThreadRef]*NoticeThread{}
	_, err = r.log.Read(ctx, staleFresh(fresh).Query(DomainScope(), true), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, noticeThreadsSQL, string(list)) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var (
				ref    ThreadRef
				t      NoticeThread
				status string
			)
			if err := rows.Scan(&ref.Artifact, &ref.Thread, &status, &t.Version, &t.Quote, &t.ResolvedBy,
				&t.ResolvedVersion, &t.Remarks); err != nil {
				return err
			}
			t.Status = ThreadStatus(status)
			if t.Status != ThreadResolved {
				t.ResolvedBy, t.ResolvedVersion = "", 0
			}
			held[ref] = &t
		}
		return rows.Err()
	})
	if err != nil {
		if errors.Is(err, ErrInvalid) {
			return nil, err
		}
		return nil, fmt.Errorf("artifactsdomain: read the threads an inbox page's cards name: %w", err)
	}
	out := make([]*NoticeThread, len(refs))
	for i, ref := range refs {
		out[i] = held[ref]
	}
	return out, nil
}

// The inbox's statements: inboxScanSQL is one range of the inbox index with
// every filter bound (`unread` is internal/inbox's rule in SQL), inboxMarkedSQL
// the unread-listed notices below it, noticeThreadsSQL each named thread.
const (
	inboxColumns = `n.record_id, n.log_seq, n.log_stream, n.log_generation, n.created_at, n.reason,
			n.addressed, n.op, n.artifact_id, n.thread_id, n.remark_id, n.excerpt, n.actor, n.actor_kind`
	inboxScanSQL = `SELECT ` + inboxColumns + `
		FROM artifact_notifications AS n INDEXED BY artifact_notifications_inbox_idx
		WHERE n.recipient = ? AND n.log_seq < ? AND n.log_seq > ?
			AND (NOT ? OR n.reason IN (SELECT value FROM json_each(?)))
			AND (NOT ? OR n.record_id IN (SELECT value FROM json_each(?))
				OR (n.record_id NOT IN (SELECT value FROM json_each(?))
					AND NOT (n.log_stream = ? AND n.log_seq <= ?)))
			AND CASE ? WHEN 'exclude' THEN n.record_id NOT IN (SELECT value FROM json_each(?))
				WHEN 'only' THEN n.record_id IN (SELECT value FROM json_each(?)) ELSE 1 END
		ORDER BY n.log_seq DESC
		LIMIT ?`
	inboxMarkedSQL = `SELECT ` + inboxColumns + `
		FROM json_each(?) AS j CROSS JOIN artifact_notifications AS n
		WHERE n.record_id = CAST(j.value AS TEXT) AND n.recipient = ? AND n.log_seq < ? AND n.log_seq > ?
			AND (NOT ? OR n.reason IN (SELECT value FROM json_each(?)))
			AND CASE ? WHEN 'exclude' THEN n.record_id NOT IN (SELECT value FROM json_each(?))
				WHEN 'only' THEN n.record_id IN (SELECT value FROM json_each(?)) ELSE 1 END`
	noticeThreadsSQL = `SELECT t.artifact_id, t.id, t.status, t.version, t.quote, t.resolved_by,
			t.resolved_version,
			(SELECT COUNT(*) FROM artifact_comments AS c
				WHERE c.artifact_id = t.artifact_id AND c.thread_id = t.id AND c.removed = 0)
		FROM json_each(?) AS j CROSS JOIN artifact_comment_threads AS t
		WHERE t.artifact_id = CAST(j.value ->> 'artifact' AS TEXT) AND t.id = CAST(j.value ->> 'thread' AS TEXT)`
)
