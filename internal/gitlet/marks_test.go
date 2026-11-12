package gitlet_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PERSON'S MARKS OVER THEIR CODE NOTICES (marks.go) and THEIR CODE INBOX
// (inboxread.go): one gesture at a time, decided on the person's own record on
// the code log, and an inbox read filtered in its scan by them.

// markerOn is a marks writer over pub on n, its clock the harness's.
func markerOn(t *testing.T, n *node, pub gitlet.Publisher) *gitlet.Marks {
	t.Helper()
	m, err := gitlet.NewMarks(gitlet.MarksOptions{Publisher: pub, DB: n.db.Reader(), NodeID: n.id,
		Now: func() time.Time { return harnessAt }})
	if err != nil {
		t.Fatalf("NewMarks: %v", err)
	}
	return m
}

// marking is a gesture over founder's code notices, asked as ada.
func marking(g gitlet.MarkGesture) gitlet.MarkWrite {
	return gitlet.MarkWrite{OpID: statelog.NewOpID(time.Now(), "code_mark_inbox"), Asker: ada, Handle: "founder",
		Gesture: g}
}

// mark asks m for w and requires it applied.
func mark(t *testing.T, m *gitlet.Marks, w gitlet.MarkWrite) gitlet.MarkResult {
	t.Helper()
	res, err := m.Mark(t.Context(), w)
	if err != nil || res.Refusal != "" || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the mark %+v answered %+v, %v; want it applied", w.Gesture, res, err)
	}
	return res
}

// founderNotices opens a change on n that mentions founder and has dev answer
// founder's comment replies times — 1+replies notices of founder's, oldest
// first, by their notice ids (gitlet.NoticeID) and their positions.
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
	ids := []string{gitlet.NoticeID(n.recordAt(opened.Position).OpID, opened.ChangeID)}
	at := []statelog.Position{opened.Position}
	w, _ := writerOn(t, n, n.pub)
	said := converse(t, w, saying(repo, opened.ChangeID, ada, "", "Why retry twice?"))
	for range replies {
		reply := converse(t, w, saying(repo, opened.ChangeID, dev, said.ID, "It races."))
		ids = append(ids, gitlet.NoticeID(n.recordAt(reply.Position).OpID, opened.ChangeID))
		at = append(at, reply.Position)
	}
	return ids, at, repo, opened.ChangeID
}

// readInbox is a page of founder's code inbox as ada reads it.
func readInbox(t *testing.T, n *node, q gitlet.InboxQuery, cursor string, limit int) (*gitlet.InboxPage, string) {
	t.Helper()
	q.Handle = "founder"
	q.Now = harnessAt
	if q.Snoozed == "" {
		q.Snoozed = inbox.SnoozeExclude
	}
	answer, err := n.reader.Read(t.Context(), gitlet.ReadQuery{Kind: gitlet.ReadInbox, Asker: ada, Inbox: &q,
		Cursor: cursor, Limit: limit})
	if err != nil || answer.Inbox == nil {
		t.Fatalf("read founder's code inbox %+v: %+v, %v", q, answer.Inbox, err)
	}
	return answer.Inbox, answer.Next
}

// idsOf is a page's record ids, in its order.
func idsOf(page *gitlet.InboxPage) []string {
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
	read := mark(t, m, marking(gitlet.MarkGesture{Read: []string{ids[0]}}))
	until := harnessAt.Add(48 * time.Hour)
	snoozed := mark(t, m, marking(gitlet.MarkGesture{Snooze: []gitlet.MarkSnooze{{RecordID: ids[1], Until: until}}}))

	rec := n.recordAt(read.Position)
	if rec.Subject != gitlet.PersonSubject("founder") || rec.Op != gitlet.OpMarks ||
		!slices.Equal([]string(rec.Scope), []string{gitlet.PersonPath("founder")}) ||
		rec.Actor != "founder" || rec.ActorKind != gitlet.ActorOperator || rec.OperatorID != "pat:ada" {
		t.Fatalf("the mark's record is %+v, want op marks on person.founder scoped to its path, by founder "+
			"through pat:ada", rec.RecordEnvelope)
	}
	page, _ := readInbox(t, n, gitlet.InboxQuery{Snoozed: inbox.SnoozeInclude}, "", 0)
	marks := map[string]gitlet.InboxNotice{}
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
	again, err := m.Mark(t.Context(), marking(gitlet.MarkGesture{Read: []string{ids[0]}}))
	// UNCHANGED AT WHERE THE PERSON'S RECORD LAST MOVED — the snooze's record,
	// the newest of their marks — never at the read's own.
	if err != nil || !again.Unchanged || again.Outcome != statelog.OutcomeApplied ||
		again.Position != snoozed.Position {
		t.Errorf("marking a read notice read again answered %+v, %v; want it unchanged at %s", again, err,
			snoozed.Position)
	}
}

// A CODE MARK NAMES ONLY THEIR NOTICES: a mark naming a record that reached
// somebody else is refused naming it — alone, or among founder's own notices
// in one gesture; a person marking another's record, and a seat marking its
// own, are refused as not theirs to make — and none of them lands a record.
// A gesture naming several of founder's own notices finds every one, in the
// one statement that reads where they sit, and lands.
//
// Mutation: find a notice's position by its record alone, whoever it reached
// — the ask to plat-lead lands as one of founder's marks, red; take the first
// notice the statement answers and no more — the gesture over founder's two
// notices is refused naming the second, red.
func TestACodeMarkNamesOnlyTheirNotices(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	own, _, repo, change := founderNotices(t, n, 1)
	w, _ := writerOn(t, n, n.pub)
	ask := converse(t, w, asking(repo, change, dev, "plat-lead"))
	theirs := gitlet.NoticeID(n.recordAt(ask.Position).OpID, change)
	m := markerOn(t, n, n.pub)
	for name, tc := range map[string]struct {
		w    gitlet.MarkWrite
		want gitlet.MarkRefusal
	}{
		"another's notice": {marking(gitlet.MarkGesture{Read: []string{theirs}}), gitlet.MarkInvalid},
		"another's among their own": {marking(gitlet.MarkGesture{Read: []string{own[0], theirs, own[1]}}),
			gitlet.MarkInvalid},
		"another's record": {func() gitlet.MarkWrite {
			w := marking(gitlet.MarkGesture{Read: []string{theirs}})
			w.Handle = "cto"
			return w
		}(), gitlet.MarkNotPermitted},
		"a seat's own": {func() gitlet.MarkWrite {
			w := marking(gitlet.MarkGesture{Read: []string{theirs}})
			w.Asker, w.Handle = dev, "dev"
			return w
		}(), gitlet.MarkNotPermitted},
	} {
		res, err := m.Mark(t.Context(), tc.w)
		if err != nil || res.Refusal != tc.want || !res.Position.IsZero() {
			t.Errorf("%s: the mark answered %+v, %v; want it refused %s with nothing landed", name, res, err,
				tc.want)
		}
		if tc.want == gitlet.MarkInvalid && !strings.Contains(res.Detail, theirs) {
			t.Errorf("%s: the refusal says %q, want it to name %s", name, res.Detail, theirs)
		}
	}
	mark(t, m, marking(gitlet.MarkGesture{Read: own}))
	page, _ := readInbox(t, n, gitlet.InboxQuery{}, "", 0)
	if len(page.Notices) != len(own) {
		t.Fatalf("founder's inbox holds %v, want their %d notices", idsOf(page), len(own))
	}
	for _, notice := range page.Notices {
		if !notice.Read {
			t.Errorf("after founder marked both their notices read, %s reads unread", notice.RecordID)
		}
	}
}

// THE CODE INBOX FILTERS IN THE SCAN: with the three newest of six notices
// snoozed, the first page of three is the three that are not — a full page,
// never an empty one with a cursor behind it — the snoozed scope lists exactly
// the three put off, and `unread` past a read-through position lists only what
// sits above it, every page carrying its cursor where more remain — and, with
// the oldest notice marked unread again, that notice too, in its place below
// the four above the position, on the next page when a page of four holds
// those.
//
// Mutation: filter the page after the scan — the first page reads none of the
// snoozed three and answers empty, red; seek no unread mark below the
// position — the notice marked unread again is never listed, red.
func TestTheCodeInboxFiltersInTheScan(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	ids, at, _, _ := founderNotices(t, n, 5)
	m := markerOn(t, n, n.pub)
	until := harnessAt.Add(24 * time.Hour)
	var snoozes []gitlet.MarkSnooze
	for _, id := range ids[3:] {
		snoozes = append(snoozes, gitlet.MarkSnooze{RecordID: id, Until: until})
	}
	mark(t, m, marking(gitlet.MarkGesture{Snooze: snoozes}))

	newestFirst := func(in []string) []string {
		out := slices.Clone(in)
		slices.Reverse(out)
		return out
	}
	page, next := readInbox(t, n, gitlet.InboxQuery{}, "", 3)
	if got, want := idsOf(page), newestFirst(ids[:3]); !slices.Equal(got, want) {
		t.Fatalf("the first page of three with three snoozed is %v, want the three awake %v", got, want)
	}
	if page.Unread != 3 || next == "" {
		t.Errorf("the first page counts %d unread with cursor %q, want 3 and the cursor of a full page",
			page.Unread, next)
	}
	only, _ := readInbox(t, n, gitlet.InboxQuery{Snoozed: inbox.SnoozeOnly}, "", 0)
	if got, want := idsOf(only), newestFirst(ids[3:]); !slices.Equal(got, want) {
		t.Errorf("the snoozed scope lists %v, want the three put off %v", got, want)
	}
	through := at[1]
	mark(t, m, marking(gitlet.MarkGesture{ReadThrough: &through}))
	unread, _ := readInbox(t, n, gitlet.InboxQuery{Unread: true, Snoozed: inbox.SnoozeInclude}, "", 0)
	if got, want := idsOf(unread), newestFirst(ids[2:]); !slices.Equal(got, want) {
		t.Errorf("unread past %s lists %v, want what sits above it %v", through, got, want)
	}
	mark(t, m, marking(gitlet.MarkGesture{Unread: []string{ids[0]}}))
	again, _ := readInbox(t, n, gitlet.InboxQuery{Unread: true, Snoozed: inbox.SnoozeInclude}, "", 0)
	if got, want := idsOf(again), append(newestFirst(ids[2:]), ids[0]); !slices.Equal(got, want) {
		t.Errorf("unread past %s with %s marked unread again lists %v, want %v — an unread mark below the "+
			"position brings its notice back, in its place", through, ids[0], got, want)
	}
	page, next = readInbox(t, n, gitlet.InboxQuery{Unread: true, Snoozed: inbox.SnoozeInclude}, "", 4)
	if got, want := idsOf(page), newestFirst(ids[2:]); !slices.Equal(got, want) || next == "" {
		t.Errorf("a page of four of those five lists %v with cursor %q, want %v and a cursor", got, next, want)
	}
	rest, _ := readInbox(t, n, gitlet.InboxQuery{Unread: true, Snoozed: inbox.SnoozeInclude}, next, 4)
	if got := idsOf(rest); !slices.Equal(got, ids[:1]) {
		t.Errorf("the page after it lists %v, want the notice marked unread again, %v", got, ids[:1])
	}
	primary, _ := readInbox(t, n, gitlet.InboxQuery{Reasons: []gitlet.Reason{gitlet.ReasonMention},
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
	res, err := m.Mark(t.Context(), marking(gitlet.MarkGesture{ReadThrough: &tracker}))
	if err != nil || res.Refusal != gitlet.MarkInvalid {
		t.Fatalf("reading through the tracker's position answered %+v, %v; want it refused invalid", res, err)
	}
	through := at[1]
	mark(t, m, marking(gitlet.MarkGesture{ReadThrough: &through}))
	page, _ := readInbox(t, n, gitlet.InboxQuery{}, "", 0)
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

// A CODE MARK WAITS FOR THE ASKER'S FLOOR: a mark is asked of whichever data
// node the router reaches, and the inbox page that showed the notice may have
// been read from a peer ahead of it. Asked of a node that has not applied the
// notice's record, a mark with no floor finds no row for it and is refused as
// naming none of founder's notices; the same gesture carrying the position the
// asker read (the notice's own) waits for the node to reach it and lands. A
// node that cannot reach the floor within the write budget answers
// ErrNotServing — the router asks the next data node — and appends nothing,
// never the false "not one of their notices".
//
// Mutation: publish the mark without the asker's floor (drop `Session:
// w.Seen`), and the gesture carrying it is refused invalid on the node behind,
// red; the held node answers a refusal rather than ErrNotServing, red.
func TestACodeMarkWaitsForTheAskersFloor(t *testing.T) {
	t.Parallel()
	t.Run("past the asker's floor", func(t *testing.T) {
		t.Parallel()
		a, b := newTwoNodes(t)
		storedBefore(b, time.Hour)
		ids, at, _, _ := founderNotices(t, a, 0)
		m := markerOn(t, b, b.pub)
		blind, err := m.Mark(t.Context(), marking(gitlet.MarkGesture{Read: []string{ids[0]}}))
		if err != nil || blind.Refusal != gitlet.MarkInvalid {
			t.Fatalf("a mark with no floor on a node behind the notice answered %+v, %v; want it refused "+
				"invalid, or this case proves nothing", blind, err)
		}
		floored := marking(gitlet.MarkGesture{Read: []string{ids[0]}})
		floored.Seen = at[0]
		res := mark(t, m, floored)
		if rec := b.recordAt(res.Position); rec.Subject != gitlet.PersonSubject("founder") || rec.Op != gitlet.OpMarks {
			t.Errorf("the mark landed %+v, want op marks on person.founder", rec.RecordEnvelope)
		}
	})
	t.Run("past what the writer reaches in its budget", func(t *testing.T) {
		t.Parallel()
		a, b := newTwoNodes(t)
		storedBefore(b, time.Hour)
		ids, at, _, _ := founderNotices(t, a, 0)
		b.hold()
		m := markerOn(t, b, b.pub)
		before := b.end()
		w := marking(gitlet.MarkGesture{Read: []string{ids[0]}})
		w.Seen = at[0]
		if res, err := m.Mark(t.Context(), w); !errors.Is(err, gitlet.ErrNotServing) {
			t.Fatalf("a marks writer behind the asker's floor answered %+v, %v; want ErrNotServing", res, err)
		}
		if got := b.end() - before; got != 0 {
			t.Fatalf("%d records appended, want none", got)
		}
	})
}
