package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/crewlet/crewlet/internal/words"
)

// ONE PERSON'S CODE INBOX — the `inbox` read: the notices the applier wrote
// for them, one row per record and change that reached them
// (code_notifications, apply_notices.go), newest first on the code log, a page
// never closing inside one record's notices, read beside their own
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
// internal/worklet's inboxread.go). Every set it binds is the Classifier's
// ([inbox.Classifier]), the lookups the page is then marked by, so a notice
// the scan hides is exactly one the page would have marked. ONE RANGE
// STATEMENT whatever is asked: a filter not asked is bound to say so ([inboxScanSQL]),
// never left out of a statement built per call. Under `unread` the range
// starts at the person's seen-through position, and the few notices below it
// an unread mark brings back are sought by their key beside it ([inboxScan]):
// what the person has read through is never walked.
//
// # Held to the repository's grant
//
// A notice is about a change on a repository, and is answered only where its
// asker may read that repository ([JudgeRead]) — a person reads every one
// (ADR-0088); a seat leading the person reads by its own grant. That is the
// one narrowing the scan cannot state, since the grant is the chart's: a page
// leaves out a notice its asker may not read, and its cursor is still the
// last notice it scanned, as every listing across repositories pages
// ([Reader.repos]).
//
// # What a notice carries of its change, and what its card reads
//
// A notice carries its change's title, its state and the work item it
// implements — its row's own columns, read for the page's changes in the same
// snapshot by one statement — which is every word a count, a badge or a row
// of a list is drawn from. What a person decides a notice by without opening
// the change — the first [MaxNoticeBody] bytes of its description, the agent
// findings standing open on it by severity and, while it is live, where its
// merge stands — is its CARD, which a `notice_cards` read answers
// ([ReadNoticeCards], [Reader.noticeCards]) and an `inbox` read never does.
//
// A CARD IS READ ONLY WHERE ONE IS DRAWN. The busiest reads of an inbox draw
// none: the frame's count of what waits on a person asks a page of fifty
// every minute and on every push of their inbox, behind every screen of every
// tab they have open, and Home's Updates and the Snoozed link's count do the
// same; each reads only how many notices came back and their titles. Judged
// on every one of them, a page's readiness alone was ten statements or more
// and a merge-gate judgment per live change, for an answer nobody drew.
//
// AND ONLY FOR THE NOTICES THE DRAWN PAGE KEEPS. A composed inbox asks each
// domain for the page's whole limit and cuts the merge back to it
// (internal/inbox's Compose), so Gitlet's page holds notices that the
// tracker's newer ones push off the page; their cards would be judged for
// nothing. So the composition asks for the cards of the code notices it
// took, after the cut ([inbox.Carded]), floored at the position
// Gitlet's page was read at — a card never describes its change as it stood
// before the notice it is drawn beside.
//
// The readiness is JUDGED in the card read, as a listing of changes judges
// it ([readinessIn]: [ReadinessOf] over that read's transaction and its one
// chart), never the `ready` a change's last mergeability record stored: that
// is a refresh's input, current only as of the home's last pass, and a card
// that read it would say "ready" past a review that just asked for changes.
// A card carries the judgment's counts and its first sentence and not its
// preconditions: fifty cards a page, each with up to [MaxReadinessBlockers]
// blockers named, would be a page of what the change's own page reads in
// full, one change at a time.
//
// Measured on this tree (BenchmarkACodeInbox, -benchtime 20x -count 1 on two
// pinned cores of a shared machine, one repository of 50 live changes behind
// one merge gate, every change with a notice to one person): an `inbox` read
// of the fifty is 3.9 ms; the same read beside the `notice_cards` read of
// their fifty changes is 8.7 ms — the cards alone 4.8 ms, 1.2 times what the
// list read costs, which every count of what waits and every Home poll no
// longer pays.

// MaxNoticeBody is how much of a change's description a card carries: ONE
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

// InboxNotice is one thing Gitlet told a person.
type InboxNotice struct {
	// RecordID is the notice's identity, which a mark names: the record that
	// reached them and the change it told them about ([NoticeID]), since one
	// record — a batched push, a landing of several entries — can tell one
	// person about several changes.
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

// NoticeChange is what a notice carries of its change — its title, its state
// and its work item — and, answered by a `notice_cards` read alone, the rest
// of what its card draws (this file's head).
type NoticeChange struct {
	Title    string          `json:"title" untrusted:"name"`
	State    ChangeState     `json:"state"`
	WorkItem *types.WorkItem `json:"work_item,omitempty"`
	// Body is the description's first [MaxNoticeBody] bytes, and BodyCut
	// whether there is more of it: a card's alone.
	Body    string `json:"body,omitempty" untrusted:"fence"`
	BodyCut bool   `json:"body_cut,omitempty"`
	// AgentFindings counts the findings seats raised that stand open on the
	// change, by severity; absent for none, and on a notice — a card's alone.
	AgentFindings map[Severity]int `json:"agent_findings,omitempty"`
	// Readiness is where the change's merge stands, judged at the card's
	// read for its asker — this file's head; nil for a change no longer
	// live, which merges nothing, and on a notice — a card's alone.
	Readiness *NoticeReadiness `json:"readiness,omitempty"`
}

// MaxNoticeCards is the most changes one `notice_cards` read names: what one
// composed page of an inbox can name — [inbox.MaxRows] notices, and past
// them the rest of the last record's, at most [MaxRecordRows] — each notice
// about one change at the most.
const MaxNoticeCards = inbox.MaxRows + MaxRecordRows

// NoticeReadiness is what a notice carries of its change's [Readiness]: how
// many of its preconditions are met of how many, and its FIRST sentence —
// the first unmet precondition's, else the first computing one's, else that
// the change is ready.
type NoticeReadiness struct {
	Met   int    `json:"met"`
	Total int    `json:"total"`
	First string `json:"first"`
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
		return refused("inbox.snoozed", fmt.Sprintf("%q is not one of %s — a surface resolves an absent scope "+
			"to %q before it reads", in.Snoozed, words.Or(inbox.SnoozeScopes), inbox.SnoozeExclude))
	case !in.Since.IsZero() && in.Since.Stream != topics.CodeLogStream:
		return refused("inbox.since", fmt.Sprintf("%s is a position on another log than %s", in.Since,
			topics.CodeLogStream))
	}
	for _, r := range in.Reasons {
		if !r.Valid() {
			return refused("inbox.reasons", fmt.Sprintf("%q is not one of %s", r, words.Or(Reasons())))
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

	scan, selects, err := inboxScanOf(q, classifier, now)
	if err != nil || !selects {
		return err
	}
	limit := min(limitOr(q.Limit, inbox.MaxRows), r.pageRows)
	scanned, err := scan.run(ctx, tx, limit)
	if err != nil {
		return err
	}
	// THE CURSOR IS THE LAST NOTICE SCANNED, whatever the grant keeps of
	// the page — this file's head — and A PAGE CLOSES BETWEEN TWO RECORDS:
	// the cursor is a position on the code log, which every notice of one
	// record shares, so a page that ended inside a record told one person
	// about several changes would leave the rest of it behind a cursor that
	// resumes below it. The rest of the last record's notices are read by the
	// same statement narrowed to its one position — at most [MaxRecordRows]
	// of them, since every notice is a row of its record.
	if len(scanned) == limit {
		last := scanned[len(scanned)-1]
		at := statelog.Position{Stream: last.LogStream, Generation: last.LogGeneration, Seq: last.LogSeq}
		out.Next = at.String()
		rest := scan
		rest.before, rest.after = at.Packed()+1, at.Packed()-1
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		more, err := rest.run(ctx, tx, MaxRecordRows)
		if err != nil {
			return err
		}
		for _, n := range more {
			if !slices.ContainsFunc(scanned, func(s InboxNotice) bool { return s.RecordID == n.RecordID }) {
				scanned = append(scanned, n)
			}
		}
	}
	repoIDs := make([]string, 0, len(scanned))
	for _, n := range scanned {
		repoIDs = append(repoIDs, n.Repo)
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

// inboxScan is one inbox read's scan: whose notices, the exclusive bounds on
// the code log it reads between, and every filter [inboxScanSQL] binds, each
// set ONE JSON array — the Classifier's, so the scan and the page's marks are
// one rule.
//
// # `unread` reads nothing the person has read through
//
// Under `unread`, a notice at or below the seen-through position is read
// unless an unread mark names it ([inbox.Classifier.Classify]), so the rows
// such a page can hold are those above the position and the ones the unread
// list names. The scan reads exactly those: the range of the person's notices
// starts at the position ([inboxScan.floor]), and the notices the unread list
// names below it — at most [inbox.MaxEntries] — are looked up by their key
// ([inboxMarkedSQL]) and merged in. Bounded only by a predicate on each row,
// a person who had marked everything read walked every notice they hold, the
// whole of `inbox.retention_days`, to answer nothing — on every badge poll.
// The position bounds the range only ON THE CODE LOG'S STREAM, as it covers a
// notice only there; every notice the applier writes is on it.
type inboxScan struct {
	handle        string
	before, after int64

	restricted bool
	reasons    string

	unread                 bool
	unreadMarks, readMarks string
	seenStream             string
	seenAt                 int64
	// marked is the unread list as [record, change] pairs, the key
	// [inboxMarkedSQL] seeks.
	marked string

	scope  string
	asleep string
}

// floor is the range's exclusive lower bound: the caller's `since`, raised
// under `unread` to the seen-through position on the code log.
func (s inboxScan) floor() int64 {
	if s.unread && s.seenStream == topics.CodeLogStream && s.seenAt > s.after {
		return s.seenAt
	}
	return s.after
}

// run is up to limit of the scan's notices, newest first: the range above
// [inboxScan.floor], and where the floor was raised past the scan's own lower
// bound, the notices the unread list names below it, merged in.
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
	if err != nil || floor == s.after || s.marked == "[]" {
		return out, err
	}
	marked, err := scanInbox(ctx, tx, s.handle, inboxMarkedSQL, s.marked, s.handle, min(s.before, floor+1),
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

// scanInbox runs statement — [inboxScanSQL] or [inboxMarkedSQL] — with args
// for handle's inbox, each notice named by its [NoticeID].
func scanInbox(ctx context.Context, tx *sql.Tx, handle, statement string, args ...any) ([]InboxNotice, error) {
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("code: read %s's code inbox: %w", handle, err)
	}
	defer func() { _ = rows.Close() }()
	var out []InboxNotice
	for rows.Next() {
		var (
			n                   InboxNotice
			record              string
			packed, at          int64
			reason              string
			addressed, fallback int
		)
		if err := rows.Scan(&record, &packed, &n.LogStream, &n.LogGeneration, &at, &reason, &addressed,
			&fallback, &n.Repo, &n.ChangeID, &n.ChangeRef, &n.Excerpt); err != nil {
			return nil, fmt.Errorf("code: read %s's code inbox: %w", handle, err)
		}
		n.RecordID = NoticeID(record, n.ChangeID)
		n.LogSeq = uint64(packed) % statelog.GenerationStride
		n.At = store.DecodeTime(at)
		n.Reason, n.Addressed, n.Fallback = Reason(reason), addressed != 0, fallback != 0
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("code: read %s's code inbox: %w", handle, err)
	}
	return out, nil
}

// inboxScanOf is the scan q asks for, its filters bound from the person's
// marks, and false where the filters select nothing at all — a reason filter
// whose intersection with the primary split is empty, or only the snoozed
// notices of a person who snoozed none.
func inboxScanOf(q ReadQuery, marks inbox.Classifier, now time.Time) (inboxScan, bool, error) {
	in := q.Inbox
	s := inboxScan{handle: in.Handle, before: math.MaxInt64, restricted: len(in.Reasons) > 0 || in.PrimaryOnly,
		unread: in.Unread, scope: string(in.Snoozed)}
	if q.Cursor != "" {
		at, err := statelog.ParsePosition(q.Cursor)
		if err != nil {
			return inboxScan{}, false, fmt.Errorf("%w: %q is not a notice's place on the code log", ErrBadCursor,
				q.Cursor)
		}
		s.before = at.Packed()
	}
	if !in.Since.IsZero() {
		s.after = in.Since.Packed()
	}
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
	if s.restricted && len(reasons) == 0 {
		return inboxScan{}, false, nil
	}
	asleep := marks.Asleep(now)
	if in.Snoozed == inbox.SnoozeOnly && len(asleep) == 0 {
		return inboxScan{}, false, nil
	}
	if seen := marks.Seen(); seen.Seq != 0 && seen.Stream != "" {
		s.seenStream, s.seenAt = seen.Stream, int64(seen.Packed())
	}
	unread := marks.UnreadMarks()
	pairs := make([][2]string, 0, len(unread))
	for _, id := range unread {
		if record, change, ok := splitNoticeID(id); ok {
			pairs = append(pairs, [2]string{record, change})
		}
	}
	marked, err := json.Marshal(pairs)
	if err != nil {
		return inboxScan{}, false, fmt.Errorf("code: name the notices %s marked unread: %w", in.Handle, err)
	}
	s.marked = string(marked)
	for _, list := range []struct {
		ids []string
		to  *string
	}{{reasons, &s.reasons}, {unread, &s.unreadMarks}, {marks.ReadMarks(), &s.readMarks}, {asleep, &s.asleep}} {
		if *list.to, err = jsonList(list.ids); err != nil {
			return inboxScan{}, false, err
		}
	}
	return s, true, nil
}

// noticeChangesIn is what each change of ids says now on its notices, by its
// id — its title, its state and its work item, off its row: ONE statement over
// the page's changes. A change no row holds any more says nothing.
func noticeChangesIn(ctx context.Context, tx *sql.Tx, ids []string) (map[string]*NoticeChange, error) {
	out := map[string]*NoticeChange{}
	if len(ids) == 0 {
		return out, nil
	}
	held, err := changesByIDIn(ctx, tx, ids)
	if err != nil {
		return nil, fmt.Errorf("code: read the changes an inbox page names: %w", err)
	}
	for id, row := range held {
		out[id] = noticeChangeOf(row)
	}
	return out, nil
}

// noticeChangeOf is what a notice carries of the change row is.
func noticeChangeOf(row changeRow) *NoticeChange {
	c := &NoticeChange{Title: row.title, State: row.state}
	if row.work.ID != "" {
		work := row.work
		c.WorkItem = &work
	}
	return c
}

// validateNoticeCards is [ReadQuery.validate] for a `notice_cards` read: the
// changes it names, each once by its id, at most [MaxNoticeCards] of them —
// the read's rows, which no cursor or limit pages.
func (q ReadQuery) validateNoticeCards() error {
	refused := func(field, why string) error { return fmt.Errorf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case len(q.NoticeCards) == 0:
		return refused("notice_cards", "a notice_cards read names the changes whose cards it reads")
	case len(q.NoticeCards) > MaxNoticeCards:
		return refused("notice_cards", fmt.Sprintf("%d changes named, past the %d one page of an inbox can "+
			"name", len(q.NoticeCards), MaxNoticeCards))
	case q.Cursor != "":
		return refused("cursor", "the changes a notice_cards read names are its rows, which no cursor pages")
	case q.Limit != 0:
		return refused("limit", "the changes a notice_cards read names are its rows, which no limit cuts")
	}
	for i, id := range q.NoticeCards {
		switch {
		case !canonicalUUID(id):
			return refused("notice_cards", fmt.Sprintf("%q is not a change's id in its canonical lowercase "+
				"spelling", id))
		case slices.Contains(q.NoticeCards[:i], id):
			return refused("notice_cards", fmt.Sprintf("%s is named twice: a card is read once a change", id))
		}
	}
	return nil
}

// noticeCards serves a `notice_cards` read ([ReadNoticeCards]): the card of
// each change q names that a row holds, on a repository the asker — and the
// seat it reads for — may read ([listedReposAmong], as an inbox page holds
// its notices), by id. A change no row holds, or on a repository either may
// not read, has none.
func (r *Reader) noticeCards(ctx context.Context, tx *sql.Tx, q ReadQuery, chart Roster, out *ReadAnswer) error {
	held, err := changesByIDIn(ctx, tx, q.NoticeCards)
	if err != nil {
		return fmt.Errorf("code: read the changes an inbox page's cards name: %w", err)
	}
	repoIDs := make([]string, 0, len(held))
	for _, row := range held {
		repoIDs = append(repoIDs, row.repoID)
	}
	repos, err := listedReposAmong(ctx, tx, repoIDs, q.Asker, q.CeilingSeat, chart, "an inbox page's cards")
	if err != nil {
		return err
	}
	// IN THE QUERY'S ORDER, never the map's: the readiness reads its rows a
	// kind at a time over these, in one order whatever a map hands back.
	named := make([]changeRow, 0, len(held))
	for _, id := range q.NoticeCards {
		row, found := held[id]
		if _, listed := repos[row.repoID]; found && listed {
			named = append(named, row)
		}
	}
	out.NoticeCards, err = noticeCardsIn(ctx, tx, named, repos, chart, q.Asker, r.limits().MergeByFloor)
	return err
}

// noticeCardsIn is the card of each change of rows, by its id — what its notice
// carries of it, its description cut at [MaxNoticeBody], the agent findings
// standing open on it by severity, and a live one's readiness judged against
// chart for asker under floor ([readinessIn]), every repository a row names in
// repos — one statement each over the rows' changes, the readiness's own a
// kind at a time.
func noticeCardsIn(ctx context.Context, tx *sql.Tx, rows []changeRow, repos map[string]listedRepo, chart Roster,
	asker Principal, floor MergeBy) (map[string]*NoticeChange, error) {

	out := make(map[string]*NoticeChange, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		out[row.id] = noticeChangeOf(row)
		ids = append(ids, row.id)
	}
	judged, err := readinessIn(ctx, tx, rows, repos, chart, asker, floor)
	if err != nil {
		return nil, fmt.Errorf("code: judge the readiness of the changes an inbox page's cards name: %w", err)
	}
	for id, readiness := range judged {
		out[id].Readiness = &NoticeReadiness{Met: readiness.Met, Total: readiness.Total, First: readiness.First}
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rowsOf, err := tx.QueryContext(ctx, noticeBodiesSQL, list)
	if err != nil {
		return nil, fmt.Errorf("code: read the descriptions of the changes an inbox page's cards name: %w", err)
	}
	for rowsOf.Next() {
		var id, body string
		if err = rowsOf.Scan(&id, &body); err != nil {
			_ = rowsOf.Close()
			return nil, fmt.Errorf("code: read the descriptions of the changes an inbox page's cards name: %w",
				err)
		}
		if c, found := out[id]; found {
			c.Body = textcut.Within(body, MaxNoticeBody)
			c.BodyCut = len(c.Body) < len(body)
		}
	}
	if err = rowsOf.Err(); err != nil {
		_ = rowsOf.Close()
		return nil, fmt.Errorf("code: read the descriptions of the changes an inbox page's cards name: %w", err)
	}
	_ = rowsOf.Close()
	rowsOf, err = tx.QueryContext(ctx, noticeFindingsSQL, list)
	if err != nil {
		return nil, fmt.Errorf("code: count the findings of the changes an inbox page's cards name: %w", err)
	}
	defer func() { _ = rowsOf.Close() }()
	for rowsOf.Next() {
		var id, severity string
		if err := rowsOf.Scan(&id, &severity); err != nil {
			return nil, fmt.Errorf("code: count the findings of the changes an inbox page's cards name: %w", err)
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
	return out, rowsOf.Err()
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
// A mark names a notice by its [NoticeID], the record's id and the change's
// joined by "/", which the statement spells from the two columns. Under
// `unread` its range starts at the seen-through position ([inboxScan]).
//
// inboxMarkedSQL is the notices an unread `unread` scan reads below that
// position: those the person's unread list names, one seek of the notice
// table's key a [record, change] pair — at most inbox.MaxEntries — each held
// to the read's bounds, reasons and snooze scope. No order and no limit: the
// read merges them into the range's page.
//
// noticeBodiesSQL reads each change's description by its key, for a card: the
// change's row is changesByIDSQL's, which the readiness judges too, so a card
// read reads code_changes once; noticeFindingsSQL reads each change's open agent
// findings' severities by one covering seek of code_findings_open_agent_idx,
// which holds those alone (replicated 0137) — never the change's every
// finding of every revision, superseded and dismissed ones included — counted
// here rather than grouped by the statement, the rows being a page's changes'
// open agent findings.
const (
	inboxScanSQL = `SELECT n.record_id, n.log_seq, n.log_stream, n.log_generation, n.created_at, n.reason,
			n.addressed, n.fallback_only, n.repo_id, n.change_id, n.change_ref, n.excerpt
		FROM code_notifications AS n INDEXED BY code_notifications_inbox_idx
		WHERE n.recipient = ? AND n.log_seq < ? AND n.log_seq > ?
			AND (NOT ? OR n.reason IN (SELECT value FROM json_each(?)))
			AND (NOT ? OR n.record_id || '/' || n.change_id IN (SELECT value FROM json_each(?))
				OR (n.record_id || '/' || n.change_id NOT IN (SELECT value FROM json_each(?))
					AND NOT (n.log_stream = ? AND n.log_seq <= ?)))
			AND CASE ? WHEN 'exclude' THEN n.record_id || '/' || n.change_id NOT IN (SELECT value FROM json_each(?))
				WHEN 'only' THEN n.record_id || '/' || n.change_id IN (SELECT value FROM json_each(?)) ELSE 1 END
		ORDER BY n.log_seq DESC
		LIMIT ?`
	inboxMarkedSQL = `SELECT n.record_id, n.log_seq, n.log_stream, n.log_generation, n.created_at, n.reason,
			n.addressed, n.fallback_only, n.repo_id, n.change_id, n.change_ref, n.excerpt
		FROM json_each(?) AS j CROSS JOIN code_notifications AS n INDEXED BY sqlite_autoindex_code_notifications_1
		WHERE n.record_id = CAST(j.value ->> 0 AS TEXT) AND n.change_id = CAST(j.value ->> 1 AS TEXT)
			AND n.recipient = ? AND n.log_seq < ? AND n.log_seq > ?
			AND (NOT ? OR n.reason IN (SELECT value FROM json_each(?)))
			AND CASE ? WHEN 'exclude' THEN n.record_id || '/' || n.change_id NOT IN (SELECT value FROM json_each(?))
				WHEN 'only' THEN n.record_id || '/' || n.change_id IN (SELECT value FROM json_each(?)) ELSE 1 END`
	noticeBodiesSQL = `SELECT b.change_id, b.body
		FROM json_each(?) AS j CROSS JOIN code_change_bodies AS b INDEXED BY sqlite_autoindex_code_change_bodies_1
		WHERE b.change_id = CAST(j.value AS TEXT)`
	noticeFindingsSQL = `SELECT f.change_id, f.severity
		FROM json_each(?) AS j CROSS JOIN code_findings AS f INDEXED BY ` + openAgentFindingsKey + `
		WHERE f.change_id = CAST(j.value AS TEXT) AND ` + openAgentFindings
)

// openAgentFindingsKey is code_findings_open_agent_idx, (change_id, severity,
// state, raiser_kind) WHERE [openAgentFindings] (replicated 0137): a change's
// open agent findings alone, every column the read names carried, so its
// seek is covering.
const openAgentFindingsKey = "code_findings_open_agent_idx"

// openAgentFindings is the predicate code_findings_open_agent_idx is partial
// on, spelled as the index declares it — the pinned planner takes a partial
// index only for a statement that implies its predicate in its own words — of
// [FindingOpen] and [PartyAgent], which a test holds the two to.
const openAgentFindings = `state = 'open' AND raiser_kind = 'agent'`
