package tracker_test

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// crowdedInbox files n notices at bob, oldest first, and returns their record
// ids NEWEST FIRST — the order an inbox page lists them in.
func crowdedInbox(t *testing.T, r *roundTrip, n int) []string {
	t.Helper()
	for i := range n {
		routeTo(t, r, "t-"+itoa(i+1), "ENG-"+itoa(i+1), "bob")
	}
	var ids []string
	cursor := ""
	for {
		page := r.inbox(tracker.InboxQuery{
			Who: tracker.PartyOf("bob"), Snoozed: tracker.SnoozeInclude,
			Cursor: cursor,
		})
		for _, notice := range page.Notices {
			ids = append(ids, notice.RecordID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(ids) != n {
		t.Fatalf("bob's inbox holds %d notices, want the %d filed", len(ids), n)
	}
	return ids
}

// snoozeAll puts the named notices off until the given instant, in one gesture.
func snoozeAll(t *testing.T, r *roundTrip, ids []string, until time.Time) {
	t.Helper()
	gesture := tracker.InboxGesture{}
	for _, id := range ids {
		gesture.Snooze = append(gesture.Snooze, tracker.Snooze{RecordID: id, Until: until})
	}
	if _, err := asBob(r).MarkInbox(t.Context(), "op-snooze-"+itoa(len(ids)),
		"bob", gesture); err != nil {

		t.Fatalf("snooze %d notices: %v", len(ids), err)
	}
	r.drain()
}

// THE SNOOZED SCOPE IS ONLY WHAT WAS PUT OFF, AND ITS PAGE IS FULL.
//
// Two defects in one read. The flag this scope replaced, `include_snoozed`,
// returned EVERY notice with the snoozed ones among them — so the dashboard's
// Snoozed tab listed the whole inbox. And the snooze narrowing ran over the
// page AFTER it was read while the cursor was computed from the page BEFORE,
// so any narrowing short-paged. Half of 120 notices snoozed, interleaved with
// the rest: `only` must answer fifty snoozed notices and a cursor to the other
// ten, and nothing else.
func TestTheSnoozedScopeReturnsOnlySnoozedNoticesAndAFullPage(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ids := crowdedInbox(t, r, 120)
	var asleep []string
	for i, id := range ids {
		if i%2 == 0 {
			asleep = append(asleep, id)
		}
	}
	snoozeAll(t, r, asleep, wednesday.Add(48*time.Hour))

	first := r.inbox(tracker.InboxQuery{
		Who: tracker.PartyOf("bob"), Snoozed: tracker.SnoozeOnly, Limit: 50,
	})
	if len(first.Notices) != 50 {
		t.Fatalf("the snoozed scope's first page holds %d notices, want a full "+
			"page of 50 — 60 are snoozed", len(first.Notices))
	}
	if first.NextCursor == "" {
		t.Fatal("the snoozed scope's first page carries no cursor, and ten " +
			"snoozed notices lie behind it")
	}
	for _, notice := range first.Notices {
		if !notice.Snoozed || !slices.Contains(asleep, notice.RecordID) {
			t.Fatalf("the snoozed scope returned %s, which is not snoozed",
				notice.RecordID)
		}
	}
	second := r.inbox(tracker.InboxQuery{
		Who: tracker.PartyOf("bob"), Snoozed: tracker.SnoozeOnly, Limit: 50,
		Cursor: first.NextCursor,
	})
	if len(second.Notices) != 10 || second.NextCursor != "" {
		t.Fatalf("the second page holds %d notices with cursor %q, want the "+
			"last 10 and no cursor", len(second.Notices), second.NextCursor)
	}
}

// THE DEFAULT SCOPE FILLS ITS PAGE DESPITE SNOOZES.
//
// The sixty NEWEST notices are snoozed, which is the arrangement that emptied
// the landing page: filtered after the read, the newest fifty rows were all
// dropped and the person was shown nothing, with a cursor, while sixty unread
// notices waited behind it. In the scan, the page is the fifty newest notices
// NOT snoozed.
func TestTheDefaultScopeFillsItsPageDespiteSnoozes(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ids := crowdedInbox(t, r, 120)
	snoozeAll(t, r, ids[:60], wednesday.Add(48*time.Hour))

	got := r.inbox(tracker.InboxQuery{
		Who: tracker.PartyOf("bob"), Snoozed: tracker.SnoozeExclude, Limit: 50,
	})
	if len(got.Notices) != 50 {
		t.Fatalf("the default scope's page holds %d notices, want a full page "+
			"of 50 — 60 notices are awake", len(got.Notices))
	}
	if got.NextCursor == "" {
		t.Error("the page carries no cursor, and ten awake notices lie behind it")
	}
	if !slices.Equal(recordIDs(got.Notices), ids[60:110]) {
		t.Errorf("the default page is not the fifty newest awake notices")
	}
}

// AND SO DOES `unread`, the other half of the landing page's question.
//
// The sixty newest notices are marked read out of order — a person working
// their queue from the top. An unread page applied after the read was empty;
// in the scan it is the fifty newest notices the person has not read, and the
// rule it selects by is [markInbox]'s own: every notice returned reads unread.
func TestTheUnreadFilterFillsItsPage(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ids := crowdedInbox(t, r, 120)
	if _, err := asBob(r).MarkInbox(t.Context(), "op-read", "bob",
		tracker.InboxGesture{Read: ids[:60]}); err != nil {

		t.Fatalf("mark read: %v", err)
	}
	r.drain()

	got := r.inbox(tracker.InboxQuery{
		Who: tracker.PartyOf("bob"), Unread: true, Limit: 50,
	})
	if len(got.Notices) != 50 || got.NextCursor == "" {
		t.Fatalf("the unread page holds %d notices with cursor %q, want a full "+
			"page of 50 and a cursor", len(got.Notices), got.NextCursor)
	}
	if !slices.Equal(recordIDs(got.Notices), ids[60:110]) {
		t.Error("the unread page is not the fifty newest unread notices")
	}
	if got.Unread != 50 {
		t.Errorf("the page counts %d unread, want all 50 — the scan and the "+
			"marks disagree about what is read", got.Unread)
	}

	// AND THE SEEN-THROUGH POSITION IS THE SAME RULE: read through the
	// 20th oldest, and nothing at or below it comes back — while an
	// unread mark on one of those outranks the position, in the scan as
	// in the marks.
	through := positionOf(noticeOf(t, r, ids[100]))
	if _, err := asBob(r).MarkInbox(t.Context(), "op-through", "bob",
		tracker.InboxGesture{ReadThrough: through}); err != nil {

		t.Fatalf("read through: %v", err)
	}
	r.drain()
	if _, err := asBob(r).MarkInbox(t.Context(), "op-unread", "bob",
		tracker.InboxGesture{Unread: []string{ids[110]}}); err != nil {

		t.Fatalf("mark unread: %v", err)
	}
	r.drain()
	all := r.inbox(tracker.InboxQuery{Who: tracker.PartyOf("bob"), Unread: true})
	if !slices.Equal(recordIDs(all.Notices), append(slices.Clone(ids[60:100]), ids[110])) {
		t.Errorf("unread after a read-through is %d notices, want the forty "+
			"above the position and the one marked unread below it",
			len(all.Notices))
	}
}

// A DUE SNOOZE IS NOT SNOOZED, under any scope.
//
// Its time has come, so it is back in the inbox without a write to promote it
// — which means the scan's idea of a live snooze and the page's must be one
// test: `only` must not list it, `exclude` must, and neither may mark it.
func TestADueSnoozeIsNotSnoozed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "t-1", "ENG-1", "bob")
	notice := r.inbox(tracker.InboxQuery{Who: tracker.PartyOf("bob")}).Notices[0]
	until := wednesday.Add(time.Hour)
	snoozeAll(t, r, []string{notice.RecordID}, until)

	later := until.Add(time.Minute)
	for scope, want := range map[tracker.SnoozeScope]int{
		tracker.SnoozeOnly: 0, tracker.SnoozeExclude: 1, tracker.SnoozeInclude: 1,
	} {
		got, err := r.reader.Inbox(t.Context(), tracker.InboxQuery{
			Who: tracker.PartyOf("bob"), Level: statelog.ReadStale, Snoozed: scope,
		}, later)
		if err != nil {
			t.Fatalf("Inbox(%s): %v", scope, err)
		}
		if len(got.Notices) != want {
			t.Errorf("snoozed=%s after the snooze ran out holds %d, want %d",
				scope, len(got.Notices), want)
		}
		for _, n := range got.Notices {
			if n.Snoozed {
				t.Errorf("snoozed=%s marks a snooze whose time has come", scope)
			}
		}
	}
}

// A NOTICE CARRIES ITS COMMENT, ITS TURN, THE PERSON BEHIND A TOKEN, AND THE
// ASK IT IS ABOUT.
//
// Without them a card could not open the thread or the trace, could not say
// whether the question it announces is still open, and drew an operator's
// change as the token's id — `founder` — where the person belonged. The ask is
// read NOW: the `asked` notice ana received reads closed once she answered.
func TestANoticeCarriesItsCommentTurnSeatAndAsk(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	assign(t, r, "decide", "ana")
	task := r.task(t, "decide").Task

	dev := r.writer.As("dev", tracker.AuthorAgent, tracker.Provenance{TurnID: "turn-7"})
	ask := tracker.Comment{
		ID: "c-ask", Task: "decide", Author: "dev", AuthorKind: tracker.AuthorAgent,
		Body: "Friday?", Ask: "ana", Decision: decisionFixture(), CreatedAt: wednesday,
	}
	if _, err := dev.UpdateTask(t.Context(), "op-ask", "decide", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &ask}, tracker.ChangeComment,
		tracker.Wake{Kind: tracker.ChangeComment, Before: task, After: task,
			Comment: &ask, Thread: tracker.ThreadParties{Asked: "ana"}}.Notify(nil),
	); err != nil {
		t.Fatalf("ask: %v", err)
	}
	r.drain()

	asked := r.inbox(tracker.InboxQuery{Who: tracker.PartyOf("ana")}).Notices
	if len(asked) == 0 || asked[0].Reason != tracker.ReasonAsked {
		t.Fatalf("ana's inbox is %+v, want the ask first", asked)
	}
	switch got := asked[0]; {
	case got.CommentID != "c-ask" || got.TurnID != "turn-7":
		t.Errorf("the notice names comment %q and turn %q, want c-ask and turn-7",
			got.CommentID, got.TurnID)
	case got.Ask == nil || got.Ask.Comment != "c-ask" || got.Ask.AskedOf != "ana" ||
		!got.Ask.Open || got.Ask.Decision == nil || got.Ask.Decision.Recommended != "ship":
		t.Errorf("the notice's ask is %+v, want c-ask, open, put to ana, with "+
			"its decision", got.Ask)
	}

	// ANA ANSWERS THROUGH HER OWN TOKEN, which is bound to her seat.
	founder := r.writer.As("founder", tracker.AuthorOperator, tracker.Provenance{
		OperatorID: "founder", Seat: "ana",
	})
	askID := "c-ask"
	answer := tracker.Comment{
		ID: "c-answer", Task: "decide", Author: "founder",
		AuthorKind: tracker.AuthorOperator, Body: "hold it", Answers: &askID,
		Choice: "hold", CreatedAt: wednesday.Add(time.Hour),
	}
	if _, err := founder.UpdateTask(t.Context(), "op-answer", "decide", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Comment: &answer}, tracker.ChangeComment,
		tracker.Wake{Kind: tracker.ChangeComment, Before: task, After: task,
			Comment: &answer, Thread: tracker.ThreadParties{AnsweredAuthor: "dev"}}.Notify(nil),
	); err != nil {
		t.Fatalf("answer: %v", err)
	}
	r.drain()

	answered := r.inbox(tracker.InboxQuery{Who: tracker.PartyOf("dev")}).Notices
	if len(answered) == 0 || answered[0].Reason != tracker.ReasonAnswered {
		t.Fatalf("dev's inbox is %+v, want the answer first", answered)
	}
	switch got := answered[0]; {
	case got.Actor != "founder" || got.ActorSeat != "ana":
		t.Errorf("the answer is drawn as %q behind seat %q — the author stays "+
			"the token and the person behind it is ana", got.Actor, got.ActorSeat)
	case got.CommentID != "c-answer":
		t.Errorf("the answer's notice names comment %q", got.CommentID)
	case got.Ask == nil || got.Ask.Comment != "c-ask" || got.Ask.Open ||
		got.Ask.AnsweredBy != "founder" || got.Ask.Choice != "hold" ||
		got.Ask.AnsweredAt == nil:
		t.Errorf("the answer's ask is %+v, want c-ask closed by founder "+
			"choosing hold", got.Ask)
	}
	again := r.inbox(tracker.InboxQuery{Who: tracker.PartyOf("ana")}).Notices
	if again[0].Ask == nil || again[0].Ask.Open {
		t.Errorf("ana's `asked` notice still reads open after she answered: %+v",
			again[0].Ask)
	}

	// AND THE FEED CARRIES THE SAME, from the same column and the same read.
	feed, err := r.reader.Activity(t.Context(), tracker.ActivityQuery{
		Task: "decide", Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("Activity: %v", err)
	}
	if len(feed.Records) == 0 {
		t.Fatal("the feed is empty")
	}
	head := feed.Records[0]
	if head.ActorSeat != "ana" || head.Ask == nil || head.Ask.Choice != "hold" {
		t.Errorf("the feed's newest row is seat %q with ask %+v, want ana and "+
			"the choice", head.ActorSeat, head.Ask)
	}
}

// A RECORD FROM BEFORE VERSION 5 STORES NO SEAT, on any build.
//
// Records carried `actor_seat` for the wake before the history row had a
// column for it, so a build reading 4 applied them and wrote no seat. A build
// copying the seat from every record would disagree with that build about
// every operator's change, for good. So the column is copied only from a
// record whose version says every node applying it has the column.
func TestAnOlderRecordsSeatIsNeverStored(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		version int
		want    string
	}{
		"version 4": {4, ""},
		"version 5": {5, "jane-founder"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newApplyHarness(t)
			record := taskRecord("t-1", tracker.OpCreate, newTask("t-1"), nil)
			record.Kind = tracker.ChangeCreated
			record.V = c.version
			record.Actor, record.ActorKind = "founder", tracker.AuthorOperator
			record.OperatorID, record.ActorSeat = "founder", "jane-founder"
			if _, err := h.apply(record, time.Unix(1_700_000_100, 0).UTC()); err != nil {
				t.Fatalf("apply: %v", err)
			}
			var got string
			if err := h.db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
				return tx.QueryRowContext(t.Context(),
					`SELECT actor_seat FROM tracker_history`).Scan(&got)
			}); err != nil {
				t.Fatalf("read the history row: %v", err)
			}
			if got != c.want {
				t.Errorf("a %s record stored seat %q, want %q", name, got, c.want)
			}
		})
	}
}

// THE PERSON ANSWER SERVES THE SNOOZE BOUND, so a screen offers only the
// presets the write accepts — the engine's number, not a copy that drifts.
func TestThePersonAnswerServesTheSnoozeBound(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	got, err := r.reader.Person(t.Context(), tracker.PersonQuery{
		Who: tracker.PartyOf("bob"), Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("Person: %v", err)
	}
	if want := int64(tracker.MaxSnoozeAhead / time.Second); got.MaxSnoozeAhead != want {
		t.Errorf("max_snooze_ahead is %d seconds, want %d", got.MaxSnoozeAhead, want)
	}
}

func recordIDs(notices []tracker.InboxNotice) []string {
	out := make([]string, 0, len(notices))
	for _, n := range notices {
		out = append(out, n.RecordID)
	}
	return out
}

// noticeOf finds one of bob's notices by record id.
func noticeOf(t *testing.T, r *roundTrip, id string) tracker.InboxNotice {
	t.Helper()
	cursor := ""
	for {
		page := r.inbox(tracker.InboxQuery{
			Who: tracker.PartyOf("bob"), Snoozed: tracker.SnoozeInclude, Cursor: cursor,
		})
		for _, n := range page.Notices {
			if n.RecordID == id {
				return n
			}
		}
		if page.NextCursor == "" {
			t.Fatalf("bob holds no notice %s", id)
		}
		cursor = page.NextCursor
	}
}
