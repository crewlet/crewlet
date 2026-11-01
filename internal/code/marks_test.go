package code_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PERSON'S MARKS OVER THEIR CODE NOTICES (marks.go) and THEIR CODE INBOX
// (inboxread.go): one gesture at a time, decided on the person's own record on
// the code log, and an inbox read filtered in its scan by them.

// markerOn is a marks writer over pub on n, its clock the harness's.
func markerOn(t *testing.T, n *node, pub code.Publisher) *code.Marks {
	t.Helper()
	m, err := code.NewMarks(code.MarksOptions{Publisher: pub, DB: n.db.Reader(), NodeID: n.id,
		Now: func() time.Time { return harnessAt }})
	if err != nil {
		t.Fatalf("NewMarks: %v", err)
	}
	return m
}

// marking is a gesture over founder's code notices, asked as ada.
func marking(g code.MarkGesture) code.MarkWrite {
	return code.MarkWrite{OpID: statelog.NewOpID(time.Now(), "code_mark_inbox"), Asker: ada, Handle: "founder",
		Gesture: g}
}

// mark asks m for w and requires it applied.
func mark(t *testing.T, m *code.Marks, w code.MarkWrite) code.MarkResult {
	t.Helper()
	res, err := m.Mark(t.Context(), w)
	if err != nil || res.Refusal != "" || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the mark %+v answered %+v, %v; want it applied", w.Gesture, res, err)
	}
	return res
}

// founderNotices opens a change on n that mentions founder and has dev answer
// founder's comment replies times — 1+replies notices of founder's, oldest
// first, by their notice ids (code.NoticeID) and their positions.
func founderNotices(t *testing.T, n *node, replies int) ([]string, []statelog.Position, string, string) {
	t.Helper()
	n.chart = testChart()
	// AN HOUR OLD when applied, inside every horizon: a record the harness
	// applies at its own instant years past the broker's writes no notice.
	storedBefore(n, time.Hour)
	c, _ := committerOn(t, n, n.pub)
	repo := changeRepo(t, n, "web")
	open := opening(repo, featureRef, mainRef, dev)
	open.Open.Mentions = []string{"founder"}
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	ids := []string{code.NoticeID(n.recordAt(opened.Position).OpID, opened.ChangeID)}
	at := []statelog.Position{opened.Position}
	w, _ := writerOn(t, n, n.pub)
	said := converse(t, w, saying(repo, opened.ChangeID, ada, "", "Why retry twice?"))
	for range replies {
		reply := converse(t, w, saying(repo, opened.ChangeID, dev, said.ID, "It races."))
		ids = append(ids, code.NoticeID(n.recordAt(reply.Position).OpID, opened.ChangeID))
		at = append(at, reply.Position)
	}
	return ids, at, repo, opened.ChangeID
}

// readInbox is a page of founder's code inbox as ada reads it.
func readInbox(t *testing.T, n *node, q code.InboxQuery, cursor string, limit int) (*code.InboxPage, string) {
	t.Helper()
	q.Handle = "founder"
	q.Now = harnessAt
	if q.Snoozed == "" {
		q.Snoozed = inbox.SnoozeExclude
	}
	answer, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadInbox, Asker: ada, Inbox: &q,
		Cursor: cursor, Limit: limit})
	if err != nil || answer.Inbox == nil {
		t.Fatalf("read founder's code inbox %+v: %+v, %v", q, answer.Inbox, err)
	}
	return answer.Inbox, answer.Next
}

// idsOf is a page's record ids, in its order.
func idsOf(page *code.InboxPage) []string {
	var out []string
	for _, n := range page.Notices {
		out = append(out, n.RecordID)
	}
	return out
}

// A CODE MARK IS DECIDED ON THE PERSON'S RECORD: two screens each marking one
// notice — one read, one snoozed — both land, on the person's own subject and
// scope, as the person who holds the seat; the inbox reads both marks and the
// third notice untouched; and the same gesture again moves nothing, landing no
// record and answering where the person's record last moved.
//
// Mutation: apply a mark's lists as the whole record rather than its gesture
// to the row before — the snooze's record erases the read mark, red.
func TestACodeMarkIsDecidedOnThePersonsRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	ids, _, _, change := founderNotices(t, n, 2)
	m := markerOn(t, n, n.pub)
	read := mark(t, m, marking(code.MarkGesture{Read: []string{ids[0]}}))
	until := harnessAt.Add(48 * time.Hour)
	snoozed := mark(t, m, marking(code.MarkGesture{Snooze: []code.MarkSnooze{{RecordID: ids[1], Until: until}}}))

	rec := n.recordAt(read.Position)
	if rec.Subject != code.PersonSubject("founder") || rec.Op != code.OpMarks ||
		!slices.Equal([]string(rec.Scope), []string{code.PersonPath("founder")}) ||
		rec.Actor != "founder" || rec.ActorKind != code.ActorOperator || rec.OperatorID != "pat:ada" {
		t.Fatalf("the mark's record is %+v, want op marks on person.founder scoped to its path, by founder "+
			"through pat:ada", rec.RecordEnvelope)
	}
	page, _ := readInbox(t, n, code.InboxQuery{Snoozed: inbox.SnoozeInclude}, "", 0)
	marks := map[string]code.InboxNotice{}
	for _, notice := range page.Notices {
		marks[notice.RecordID] = notice
	}
	switch {
	case len(page.Notices) != 3:
		t.Fatalf("founder's inbox holds %v, want the three notices", idsOf(page))
	case !marks[ids[0]].Read || marks[ids[0]].Snoozed:
		t.Errorf("the notice marked read reads %+v", marks[ids[0]])
	case !marks[ids[1]].Snoozed || marks[ids[1]].SnoozedUntil == nil || !marks[ids[1]].SnoozedUntil.Equal(until):
		t.Errorf("the notice snoozed reads %+v, want snoozed until %s", marks[ids[1]], until)
	case marks[ids[2]].Read || marks[ids[2]].Snoozed:
		t.Errorf("the notice nobody marked reads %+v", marks[ids[2]])
	case marks[ids[0]].ChangeID != change || marks[ids[0]].Repo != "web" || marks[ids[0]].Change == nil:
		t.Errorf("a notice names %+v, want change %s of web with what it says", marks[ids[0]], change)
	}
	again, err := m.Mark(t.Context(), marking(code.MarkGesture{Read: []string{ids[0]}}))
	// UNCHANGED AT WHERE THE PERSON'S RECORD LAST MOVED — the snooze's record,
	// the newest of their marks — never at the read's own.
	if err != nil || !again.Unchanged || again.Outcome != statelog.OutcomeApplied ||
		again.Position != snoozed.Position {
		t.Errorf("marking a read notice read again answered %+v, %v; want it unchanged at %s", again, err,
			snoozed.Position)
	}
}

// A CODE MARK NAMES ONLY THEIR NOTICES: a mark naming a record that reached
// somebody else is refused naming it; a person marking another's record, and a
// seat marking its own, are refused as not theirs to make — and none of them
// lands a record.
//
// Mutation: find a notice's position by its record alone, whoever it reached
// — the ask to plat-lead lands as one of founder's marks, red.
func TestACodeMarkNamesOnlyTheirNotices(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	_, _, repo, change := founderNotices(t, n, 0)
	w, _ := writerOn(t, n, n.pub)
	ask := converse(t, w, asking(repo, change, dev, "plat-lead"))
	theirs := code.NoticeID(n.recordAt(ask.Position).OpID, change)
	m := markerOn(t, n, n.pub)
	for name, tc := range map[string]struct {
		w    code.MarkWrite
		want code.MarkRefusal
	}{
		"another's notice": {marking(code.MarkGesture{Read: []string{theirs}}), code.MarkInvalid},
		"another's record": {func() code.MarkWrite {
			w := marking(code.MarkGesture{Read: []string{theirs}})
			w.Handle = "cto"
			return w
		}(), code.MarkNotPermitted},
		"a seat's own": {func() code.MarkWrite {
			w := marking(code.MarkGesture{Read: []string{theirs}})
			w.Asker, w.Handle = dev, "dev"
			return w
		}(), code.MarkNotPermitted},
	} {
		res, err := m.Mark(t.Context(), tc.w)
		if err != nil || res.Refusal != tc.want || !res.Position.IsZero() {
			t.Errorf("%s: the mark answered %+v, %v; want it refused %s with nothing landed", name, res, err,
				tc.want)
		}
	}
}

// THE CODE INBOX FILTERS IN THE SCAN: with the three newest of six notices
// snoozed, the first page of three is the three that are not — a full page,
// never an empty one with a cursor behind it — the snoozed scope lists exactly
// the three put off, and `unread` past a read-through position lists only what
// sits above it, every page carrying its cursor where more remain.
//
// Mutation: filter the page after the scan — the first page reads none of the
// snoozed three and answers empty, red.
func TestTheCodeInboxFiltersInTheScan(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	ids, at, _, _ := founderNotices(t, n, 5)
	m := markerOn(t, n, n.pub)
	until := harnessAt.Add(24 * time.Hour)
	var snoozes []code.MarkSnooze
	for _, id := range ids[3:] {
		snoozes = append(snoozes, code.MarkSnooze{RecordID: id, Until: until})
	}
	mark(t, m, marking(code.MarkGesture{Snooze: snoozes}))

	newestFirst := func(in []string) []string {
		out := slices.Clone(in)
		slices.Reverse(out)
		return out
	}
	page, next := readInbox(t, n, code.InboxQuery{}, "", 3)
	if got, want := idsOf(page), newestFirst(ids[:3]); !slices.Equal(got, want) {
		t.Fatalf("the first page of three with three snoozed is %v, want the three awake %v", got, want)
	}
	if page.Unread != 3 || next == "" {
		t.Errorf("the first page counts %d unread with cursor %q, want 3 and the cursor of a full page",
			page.Unread, next)
	}
	only, _ := readInbox(t, n, code.InboxQuery{Snoozed: inbox.SnoozeOnly}, "", 0)
	if got, want := idsOf(only), newestFirst(ids[3:]); !slices.Equal(got, want) {
		t.Errorf("the snoozed scope lists %v, want the three put off %v", got, want)
	}
	through := at[1]
	mark(t, m, marking(code.MarkGesture{ReadThrough: &through}))
	unread, _ := readInbox(t, n, code.InboxQuery{Unread: true, Snoozed: inbox.SnoozeInclude}, "", 0)
	if got, want := idsOf(unread), newestFirst(ids[2:]); !slices.Equal(got, want) {
		t.Errorf("unread past %s lists %v, want what sits above it %v", through, got, want)
	}
	primary, _ := readInbox(t, n, code.InboxQuery{Reasons: []code.Reason{code.ReasonMention},
		Snoozed: inbox.SnoozeInclude}, "", 0)
	if got := idsOf(primary); !slices.Equal(got, ids[:1]) {
		t.Errorf("the mentions are %v, want the open's %v", got, ids[:1])
	}
}

// READ-THROUGH IS A POSITION ON THE CODE LOG: a position on the tracker's log is
// refused naming the log a code inbox is read through, and one on the code log
// reads every notice at or below it and none above, the person's row keeping
// it on the code log's stream.
//
// Mutation: check read_through against any stream — the tracker's position
// lands and reads nothing, red.
func TestReadThroughIsAPositionOnTheCodeLog(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	ids, at, _, _ := founderNotices(t, n, 2)
	m := markerOn(t, n, n.pub)
	tracker := statelog.Position{Stream: topics.TrackerLogStream, Generation: at[2].Generation, Seq: at[2].Seq}
	res, err := m.Mark(t.Context(), marking(code.MarkGesture{ReadThrough: &tracker}))
	if err != nil || res.Refusal != code.MarkInvalid {
		t.Fatalf("reading through the tracker's position answered %+v, %v; want it refused invalid", res, err)
	}
	through := at[1]
	mark(t, m, marking(code.MarkGesture{ReadThrough: &through}))
	page, _ := readInbox(t, n, code.InboxQuery{}, "", 0)
	read := map[string]bool{}
	for _, notice := range page.Notices {
		read[notice.RecordID] = notice.Read
	}
	if !read[ids[0]] || !read[ids[1]] || read[ids[2]] {
		t.Errorf("read through %s, the notices read %v; want the two at or below it read and %s not", through,
			read, ids[2])
	}
	if page.SeenThrough.Stream != topics.CodeLogStream || page.SeenThrough.Packed() != uint64(through.Packed()) {
		t.Errorf("founder's code inbox has read through %+v, want %s on the code log", page.SeenThrough, through)
	}
}
