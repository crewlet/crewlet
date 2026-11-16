package artifactsdomain_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/inbox"
)

// told is the people a routing snapshot tells.
func told(people ...artifactsdomain.Recipient) artifactsdomain.CommentNotify {
	return artifactsdomain.CommentNotify{People: people}
}

// as is one person told for reason, addressed or not.
func as(seat string, reason artifactsdomain.NotifyReason, addressed bool) artifactsdomain.Recipient {
	return artifactsdomain.Recipient{Seat: seat, Reason: reason, Addressed: addressed}
}

// notices is this node's artifact_notifications, one sorted line a row:
// recipient, op, reason, whether it asks, the excerpt and the actor.
func (n *node) notices() []string {
	n.t.Helper()
	var out []string
	if err := n.db.Read(n.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(n.t.Context(), `SELECT recipient, op, reason, addressed, excerpt, actor
			FROM artifact_notifications`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var recipient, op, reason, excerpt, actor string
			var addressed int
			if err := rows.Scan(&recipient, &op, &reason, &addressed, &excerpt, &actor); err != nil {
				return err
			}
			out = append(out, fmt.Sprintf("%s %s %s %d %q by %s", recipient, op, reason, addressed, excerpt, actor))
		}
		return rows.Err()
	}); err != nil {
		n.t.Fatalf("read the notices: %v", err)
	}
	slices.Sort(out)
	return out
}

// A COMMENT RECORD TELLS THE PEOPLE ITS SNAPSHOT NAMES, AND NEVER ITS AUTHOR:
// one row a person for an open, a reply, a resolve and a reopen, with the
// reason, whether it asks, the remark's words and who wrote it; an edit tells
// nobody; and each committed batch says whose inbox its rows moved.
//
// Mutation: write a row for the author, skip a kind of record, drop the
// excerpt, or announce no movement, and a line goes red.
func TestACommentRecordTellsThePeopleItNamesButNeverItsAuthor(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane, bo := person("jane", artifacts.RoleOwner), person("bo", artifacts.RoleCommenter)
	pm := agent("agent-pm", artifacts.RoleEditor)
	id := n.commented(jane, "Told")
	opened, err := n.writer.OpenThread(ctx, bo, artifactsdomain.ThreadInput{Artifact: id, Body: "Look at @cy",
		Notify: told(as("jane", artifactsdomain.NotifyOwner, true), as("cy", artifactsdomain.NotifyMention, true),
			as("bo", artifactsdomain.NotifyParticipant, false))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.Reply(ctx, jane, artifactsdomain.ReplyInput{Artifact: id, Thread: opened.Thread,
		Body: "Done", Notify: told(as("bo", artifactsdomain.NotifyParticipant, false))}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.EditRemark(ctx, bo, artifactsdomain.RemarkEdit{RemarkRef: artifactsdomain.RemarkRef{
		Artifact: id, Thread: opened.Thread, Remark: opened.Remark}, Body: "Look again"}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.Resolve(ctx, pm, artifactsdomain.ResolveInput{Artifact: id, Thread: opened.Thread,
		Notify: told(as("jane", artifactsdomain.NotifyParticipant, false),
			as("bo", artifactsdomain.NotifyParticipant, false))}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.writer.Reopen(ctx, jane, artifactsdomain.ReopenInput{Artifact: id, Thread: opened.Thread,
		Notify: told(as("bo", artifactsdomain.NotifyParticipant, false))}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`bo comment_reopen participant 0 "" by jane`,
		`bo comment_reply participant 0 "Done" by jane`,
		`bo comment_resolve participant 0 "" by agent-pm`,
		`cy comment_open mention 1 "Look at @cy" by bo`,
		`jane comment_open owner 1 "Look at @cy" by bo`,
		`jane comment_resolve participant 0 "" by agent-pm`,
	}
	if got := n.notices(); !slices.Equal(got, want) {
		t.Errorf("the notices read\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	arrived := map[string]int{}
	for _, m := range n.inboxed {
		if m.Source != "artifacts" || m.Subject != id.String() {
			t.Errorf("a movement names %s and %s, want artifacts and %s", m.Source, m.Subject, id)
		}
		arrived[m.Handle] += m.UnreadDelta
	}
	if want := map[string]int{"bo": 3, "cy": 1, "jane": 2}; !mapsEqual(arrived, want) {
		t.Errorf("the batches announced %v arrivals, want %v", arrived, want)
	}
}

// mapsEqual reports two counts alike.
func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A REMOVED REMARK'S WORDS ARE KEPT NOWHERE: its removal clears the excerpt of
// every notice its record wrote, and tells the people it told that their inbox
// moved, counting nothing. A purge takes every notice about the artifact.
//
// Mutation: leave the excerpt, clear another remark's, count the change as an
// arrival, or let a purge keep its notices, and a line goes red.
func TestARemovedRemarkLeavesItsNoticesAndAPurgeTakesThem(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane, bo := person("jane", artifacts.RoleOwner), person("bo", artifacts.RoleCommenter)
	id := n.commented(jane, "Removed")
	opened, err := n.writer.OpenThread(ctx, bo, artifactsdomain.ThreadInput{Artifact: id, Body: "Keep",
		Notify: told(as("jane", artifactsdomain.NotifyOwner, true))})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := n.writer.Reply(ctx, bo, artifactsdomain.ReplyInput{Artifact: id, Thread: opened.Thread,
		Body: "a token I pasted by mistake", Notify: told(as("jane", artifactsdomain.NotifyOwner, true))})
	if err != nil {
		t.Fatal(err)
	}
	n.inboxed = nil
	if _, err := n.writer.RemoveRemark(ctx, bo, artifactsdomain.RemarkRemoval{RemarkRef: artifactsdomain.RemarkRef{
		Artifact: id, Thread: opened.Thread, Remark: reply.Remark}}); err != nil {
		t.Fatal(err)
	}
	want := []string{`jane comment_open owner 1 "Keep" by bo`, `jane comment_reply owner 1 "" by bo`}
	if got := n.notices(); !slices.Equal(got, want) {
		t.Errorf("after the removal the notices read\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(n.inboxed) != 1 || n.inboxed[0].Handle != "jane" || n.inboxed[0].UnreadDelta != 0 {
		t.Errorf("the removal announced %+v, want jane's inbox moved with nothing counted", n.inboxed)
	}
	n.inboxed = nil
	if _, err := n.writer.Trash(ctx, jane, id); err != nil {
		t.Fatal(err)
	}
	admin := person("root", artifacts.RoleOwner)
	admin.Authority.Elevated = true
	if _, err := n.writer.Purge(ctx, admin, id); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if got := n.notices(); len(got) != 0 {
		t.Errorf("a purged artifact's notices are kept:\n%s", strings.Join(got, "\n"))
	}
	if len(n.inboxed) != 1 || n.inboxed[0].Handle != "jane" || n.inboxed[0].UnreadDelta != 0 ||
		n.inboxed[0].Subject != id.String() {
		t.Errorf("the purge announced %+v, want jane's inbox moved off %s", n.inboxed, id)
	}
}

// NO NOTICE IS WRITTEN PAST THE INBOX HORIZON the epoch states: a record the
// batch applies more than that long after its broker instant tells nobody,
// so a replay rebuilds no notice the sweep would delete, while the thread is
// written all the same. The sweep deletes what has aged past the horizon.
//
// Mutation: ignore the epoch's horizon, or skip the thread with the notice,
// and a line goes red.
func TestNoNoticeIsWrittenPastTheInboxHorizon(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ctx := t.Context()
	jane, bo := person("jane", artifacts.RoleOwner), person("bo", artifacts.RoleCommenter)
	id := n.commented(jane, "Late")
	n.epoch = map[string]any{inbox.RetentionKey: 1}
	n.applyNow = time.Now().Add(48 * time.Hour)
	opened, err := n.writer.OpenThread(ctx, bo, artifactsdomain.ThreadInput{Artifact: id, Body: "Late",
		Notify: told(as("jane", artifactsdomain.NotifyOwner, true))})
	if err != nil {
		t.Fatalf("an open past the horizon: %v", err)
	}
	if got := n.notices(); len(got) != 0 {
		t.Errorf("a record applied past the horizon wrote\n%s", strings.Join(got, "\n"))
	}
	if th := n.thread(id, opened.Thread); len(th.Remarks) != 1 {
		t.Errorf("the thread past the horizon holds %d remarks, want 1", len(th.Remarks))
	}
	n.applyNow = time.Time{}
	if _, err := n.writer.Reply(ctx, bo, artifactsdomain.ReplyInput{Artifact: id, Thread: opened.Thread,
		Body: "Now", Notify: told(as("jane", artifactsdomain.NotifyOwner, true))}); err != nil {
		t.Fatal(err)
	}
	if got := n.notices(); len(got) != 1 {
		t.Fatalf("a record within the horizon wrote %v", got)
	}
	jobs := artifactsdomain.NoticeJobs(n.db, func() time.Duration { return time.Hour })
	if len(jobs) != 1 {
		t.Fatalf("the sweep is %d jobs, want one", len(jobs))
	}
	if swept, err := jobs[0].Run(ctx, time.Now(), time.Now().Add(time.Minute)); err != nil || swept != 1 {
		t.Errorf("the sweep past every notice swept %d (%v), want 1", swept, err)
	}
	if got := n.notices(); len(got) != 0 {
		t.Errorf("the sweep kept\n%s", strings.Join(got, "\n"))
	}
}
