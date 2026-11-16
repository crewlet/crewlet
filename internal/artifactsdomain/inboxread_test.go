package artifactsdomain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// inboxOf is one page of handle's artifacts inbox at the session level.
func (n *node) inboxOf(q artifactsdomain.InboxQuery) artifactsdomain.InboxPage {
	n.t.Helper()
	if q.Snoozed == "" {
		q.Snoozed = inbox.SnoozeExclude
	}
	q.Fresh = statelog.Freshness{Level: statelog.ReadSession}
	page, err := n.reader.Inbox(n.t.Context(), q)
	if err != nil {
		n.t.Fatalf("read %s's inbox: %v", q.Handle, err)
	}
	return page
}

// excerpts is a page's notices by their excerpts, in its order.
func excerpts(p artifactsdomain.InboxPage) []string {
	out := make([]string, 0, len(p.Notices))
	for _, n := range p.Notices {
		out = append(out, n.Excerpt)
	}
	return out
}

// A PERSON'S ARTIFACTS INBOX reads their notices newest first, each with the
// artifact as it stands and the person's own marks, split by the default
// primary reasons; a page closes at its limit with a cursor past its last
// notice; and the heads it carries are the LIVE artifacts' alone, so a notice
// on one in the trash is shown to nobody.
//
// Mutation: read oldest first, drop the default split, page from the wrong
// end, or carry a trashed artifact's head, and a line goes red.
func TestAPersonsArtifactsInboxReadsNewestFirstWithItsArtifacts(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane, bo := person("jane", artifacts.RoleOwner), person("bo", artifacts.RoleCommenter)
	kept := n.commented(jane, "Kept")
	binned := n.create(jane, "Binned", entryOf(t, "index.html", "<p>Binned</p>", keyOf("binned.html"))).Artifact
	opened, err := n.writer.OpenThread(ctx, bo, artifactsdomain.ThreadInput{Artifact: kept, Body: "one",
		Notify: told(as("jane", artifactsdomain.NotifyOwner, true))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.Reply(ctx, bo, artifactsdomain.ReplyInput{Artifact: kept, Thread: opened.Thread,
		Body: "two", Notify: told(as("jane", artifactsdomain.NotifyParticipant, false))}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.OpenThread(ctx, bo, artifactsdomain.ThreadInput{Artifact: binned, Body: "three",
		Notify: told(as("jane", artifactsdomain.NotifyMention, true))}); err != nil {
		t.Fatal(err)
	}
	page := n.inboxOf(artifactsdomain.InboxQuery{Handle: "jane"})
	if got, want := excerpts(page), []string{"three", "two", "one"}; !slices.Equal(got, want) {
		t.Fatalf("jane's inbox reads %v, want %v", got, want)
	}
	first := page.Notices[2]
	if first.Op != artifactsdomain.OpCommentOpen || first.Artifact != kept.String() ||
		first.Thread != opened.Thread.String() || first.Remark != opened.Remark.String() ||
		first.Actor != "bo" || first.Title != "Kept" || !first.Primary || !first.Addressed || first.Read {
		t.Errorf("the opening notice reads %+v", first)
	}
	if page.Notices[1].Primary {
		t.Errorf("a participant's notice is primary by default: %+v", page.Notices[1])
	}
	if !slices.Equal(page.PrimaryReasons, artifactsdomain.DefaultPrimaryReasons) {
		t.Errorf("the split applied is %v, want the default %v", page.PrimaryReasons,
			artifactsdomain.DefaultPrimaryReasons)
	}
	two := n.inboxOf(artifactsdomain.InboxQuery{Handle: "jane", Limit: 2})
	if got := excerpts(two); !slices.Equal(got, []string{"three", "two"}) || two.Next == "" {
		t.Fatalf("a page of two reads %v next %q", got, two.Next)
	}
	rest := n.inboxOf(artifactsdomain.InboxQuery{Handle: "jane", Limit: 2, Cursor: two.Next})
	if got := excerpts(rest); !slices.Equal(got, []string{"one"}) || rest.Next != "" {
		t.Errorf("the page after reads %v next %q", got, rest.Next)
	}
	if _, err := n.writer.Trash(ctx, jane, binned); err != nil {
		t.Fatal(err)
	}
	page = n.inboxOf(artifactsdomain.InboxQuery{Handle: "jane"})
	if _, held := page.Heads[binned.String()]; held || len(page.Heads) != 1 {
		t.Errorf("the page carries heads %v, want only %s", page.Heads, kept)
	}
	if page.Heads[kept.String()].Title != "Kept" || page.Notices[0].Title != "" {
		t.Errorf("the page's artifacts read %+v and the trashed one's title %q", page.Heads, page.Notices[0].Title)
	}
	if bos := n.inboxOf(artifactsdomain.InboxQuery{Handle: "bo"}); len(bos.Notices) != 0 {
		t.Errorf("the author's own inbox holds %v", excerpts(bos))
	}
}

// A PERSON MARKS THEIR OWN NOTICES, one gesture at a time, by internal/inbox's
// rules on the artifacts log: read, snoozed and read through, each read back
// by the page and by its filters in the scan; a gesture naming a notice that is
// not theirs, or asked by a seat that is no person, is refused; one that moves
// nothing publishes nothing.
//
// Mutation: ignore a mark in the scan, accept another person's notice, or let
// an agent mark, and a line goes red.
func TestAPersonMarksTheirOwnCommentNotices(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane, bo := person("jane", artifacts.RoleOwner), person("bo", artifacts.RoleCommenter)
	id := n.commented(jane, "Marked")
	var ids []string
	for _, body := range []string{"one", "two", "three"} {
		if _, err := n.writer.OpenThread(ctx, bo, artifactsdomain.ThreadInput{Artifact: id, Body: body,
			Notify: told(as("jane", artifactsdomain.NotifyOwner, true))}); err != nil {
			t.Fatal(err)
		}
	}
	page := n.inboxOf(artifactsdomain.InboxQuery{Handle: "jane"})
	for _, notice := range page.Notices {
		ids = append(ids, notice.RecordID)
	}
	mark := func(c artifactsdomain.Call, g artifactsdomain.MarkGesture) error {
		_, err := n.writer.MarkInbox(ctx, c, artifactsdomain.MarkInput{Gesture: g})
		return err
	}
	until := monday.Add(time.Hour)
	if err := mark(jane, artifactsdomain.MarkGesture{Read: []string{ids[0]},
		Snooze: []artifactsdomain.MarkSnooze{{RecordID: ids[1], Until: until}}}); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if got := excerpts(n.inboxOf(artifactsdomain.InboxQuery{Handle: "jane", Unread: true})); !slices.Equal(got,
		[]string{"one"}) {
		t.Errorf("jane's unread notices read %v, want [one]", got)
	}
	if got := excerpts(n.inboxOf(artifactsdomain.InboxQuery{Handle: "jane",
		Snoozed: inbox.SnoozeOnly})); !slices.Equal(got, []string{"two"}) {
		t.Errorf("jane's snoozed notices read %v, want [two]", got)
	}
	newest := page.Notices[0]
	through := statelog.Position{Stream: newest.LogStream, Generation: newest.LogGeneration, Seq: newest.LogSeq}
	if err := mark(jane, artifactsdomain.MarkGesture{ReadThrough: &through, Unsnooze: []string{ids[1]},
		Unread: []string{ids[2]}}); err != nil {
		t.Fatalf("read through: %v", err)
	}
	if got := excerpts(n.inboxOf(artifactsdomain.InboxQuery{Handle: "jane", Unread: true})); !slices.Equal(got,
		[]string{"one"}) {
		t.Errorf("past a read-through with one marked unread again, jane's unread read %v, want [one]", got)
	}
	before := n.inboxOf(artifactsdomain.InboxQuery{Handle: "jane"})
	if w, err := n.writer.MarkInbox(ctx, jane, artifactsdomain.MarkInput{Gesture: artifactsdomain.MarkGesture{
		ReadThrough: &through}}); err != nil || !w.Result.Position.IsZero() {
		t.Errorf("a read-through that moves nothing published at %s (%v)", w.Result.Position, err)
	}
	if after := n.inboxOf(artifactsdomain.InboxQuery{Handle: "jane"}); after.SeenThrough != before.SeenThrough {
		t.Errorf("a gesture that moved nothing moved the position to %+v", after.SeenThrough)
	}
	if err := mark(bo, artifactsdomain.MarkGesture{Read: []string{ids[0]}}); !errors.Is(err,
		artifactsdomain.ErrInvalid) {
		t.Errorf("bo marking jane's notice answered %v", err)
	}
	if err := mark(agent("agent-pm", artifacts.RoleEditor), artifactsdomain.MarkGesture{
		ReadThrough: &through}); !errors.Is(err, artifactsdomain.ErrInvalid) {
		t.Errorf("an agent marking an inbox answered %v", err)
	}
	other := statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1, Seq: 3}
	if err := mark(jane, artifactsdomain.MarkGesture{ReadThrough: &other}); !errors.Is(err,
		artifactsdomain.ErrInvalid) {
		t.Errorf("a read-through on another log answered %v", err)
	}
}

// A CARD READS ITS THREAD AS IT STANDS: its status and who resolved it, its
// quote and how many remarks are not removed, in the order named, and nothing
// for a thread no row holds.
//
// Mutation: count removed remarks, keep a reopened thread's resolver, or
// answer out of order, and a line goes red.
func TestACardReadsItsThreadAsItStands(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane, bo := person("jane", artifacts.RoleOwner), person("bo", artifacts.RoleCommenter)
	id := n.commented(jane, "Carded")
	opened, err := n.writer.OpenThread(ctx, bo, artifactsdomain.ThreadInput{Artifact: id, Body: "one",
		Quote: "Total: 42", Anchor: summary})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := n.writer.Reply(ctx, bo, artifactsdomain.ReplyInput{Artifact: id, Thread: opened.Thread, Body: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.RemoveRemark(ctx, bo, artifactsdomain.RemarkRemoval{RemarkRef: artifactsdomain.RemarkRef{
		Artifact: id, Thread: opened.Thread, Remark: reply.Remark}}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.Resolve(ctx, jane, artifactsdomain.ResolveInput{Artifact: id,
		Thread: opened.Thread}); err != nil {
		t.Fatal(err)
	}
	gone := artifactsdomain.ThreadRef{Artifact: id.String(), Thread: "00000000-0000-0000-0000-000000000000"}
	got, err := n.reader.NoticeThreads(ctx, []artifactsdomain.ThreadRef{gone,
		{Artifact: id.String(), Thread: opened.Thread.String()}}, statelog.Freshness{Level: statelog.ReadSession})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != nil || got[1] == nil {
		t.Fatalf("the cards read %v", got)
	}
	want := artifactsdomain.NoticeThread{Status: artifactsdomain.ThreadResolved, Version: 1, Quote: "Total: 42",
		Remarks: 1, ResolvedBy: "jane", ResolvedVersion: 1}
	if *got[1] != want {
		t.Errorf("the card reads %+v, want %+v", *got[1], want)
	}
}
