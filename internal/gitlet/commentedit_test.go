package gitlet_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// editing is an edit of comment's words on change by asker.
func editing(repo, change string, asker gitlet.Principal, comment, body string) gitlet.ConversationWrite {
	return conversing(asker, func(w *gitlet.ConversationWrite) {
		w.CommentEdit = &gitlet.CommentEdit{RepoID: repo, ChangeID: change, CommentID: comment, Body: body}
	})
}

// deleting is a delete of comment on change by asker.
func deleting(repo, change string, asker gitlet.Principal, comment string) gitlet.ConversationWrite {
	return conversing(asker, func(w *gitlet.ConversationWrite) {
		w.CommentDelete = &gitlet.CommentDelete{RepoID: repo, ChangeID: change, CommentID: comment}
	})
}

// A COMMENT IS EDITED BY ITS AUTHOR ALONE: anybody else, a maintainer and a
// person among them, is refused not_permitted naming whose it is; the words
// replace the old ones in the comment's own place, it reads as edited, and the
// conversation's text_bytes moves by what the words gained; words it holds
// already are unchanged, and a comment the change does not hold is
// comment_gone.
//
// Mutation: drop the author rule, and the maintainer's edit lands.
func TestACommentIsEditedByItsAuthorAlone(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	other, err := c.ProposeChange(t.Context(), opening(repo, releaseRef, mainRef, dev))
	requireChanged(t, other, err)
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	root, err := w.Converse(ctx, saying(repo, change, platLead, "", "Why a minte?"))
	requireConversed(t, root, err)
	reply, err := w.Converse(ctx, saying(repo, change, dev, root.ID, "The upstream limit."))
	requireConversed(t, reply, err)
	elsewhere, err := w.Converse(ctx, saying(repo, other.ChangeID, platLead, "", "And here?"))
	requireConversed(t, elsewhere, err)
	position := n.count(`SELECT position FROM code_comments WHERE id = ?`, root.ID)
	weighed := n.count(`SELECT text_bytes FROM code_conversations WHERE change_id = ?`, change)

	for _, tc := range []struct {
		name    string
		asker   gitlet.Principal
		comment string
		refusal gitlet.ConversationRefusal
		says    string
	}{
		{"a maintainer", cto, root.ID, gitlet.ConversationRefusalNotPermitted, "is plat-lead's"},
		{"a person", ada, root.ID, gitlet.ConversationRefusalNotPermitted, "is plat-lead's"},
		{"the reply's author, on the root", dev, root.ID, gitlet.ConversationRefusalNotPermitted, "is plat-lead's"},
		{"another change's comment", platLead, elsewhere.ID, gitlet.ConversationRefusalCommentGone,
			"holds no comment " + elsewhere.ID},
		{"no comment", platLead, "019a0c2e-0000-7000-8000-00000000dead", gitlet.ConversationRefusalCommentGone,
			"holds no comment"},
	} {
		before := n.end()
		res, err := w.Converse(ctx, editing(repo, change, tc.asker, tc.comment, "Why a minute?"))
		requireConversationRefused(t, n, before, res, err, tc.refusal, tc.says)
	}

	res, err := w.Converse(ctx, editing(repo, change, platLead, root.ID, "Why a minute?"))
	requireConversed(t, res, err)
	if res.ID != root.ID {
		t.Fatalf("the edit answered comment %q, want %s", res.ID, root.ID)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_comments WHERE id = ? AND body = 'Why a minute?'
		AND body_bytes = 13 AND edited_at > 0 AND position = ? AND deleted_at = 0`, root.ID, position); got != 1 {
		t.Fatal("the edited comment does not hold its new words, edited, in its own place")
	}
	if got := n.count(`SELECT text_bytes FROM code_conversations WHERE change_id = ?`, change); got != weighed+1 {
		t.Fatalf("the conversation weighs %d bytes after an edit that added one, want %d", got, weighed+1)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_comments WHERE id = ? AND edited_at = 0`, reply.ID); got != 1 {
		t.Fatal("an edit of the root marked its reply edited")
	}

	before := n.end()
	res, err = w.Converse(ctx, editing(repo, change, platLead, root.ID, "Why a minute?"))
	if err != nil || !res.Unchanged || n.end() != before {
		t.Fatalf("an edit to the words a comment holds answered %+v, %v and appended %d records; want unchanged",
			res, err, n.end()-before)
	}
}

// A DELETED COMMENT KEEPS ITS PLACE AND EVERY REPLY (doc.go, "A deleted
// comment"): its author, a maintainer or a person deletes it, anybody else is
// refused not_permitted; the row stays in its thread with its words cleared,
// naming who deleted it and when; every reply, anyone's, stands and the thread
// still takes one; the excerpt each notice of it quoted is cleared; it takes no
// edit; and deleting it again is unchanged.
//
// Mutations: delete the row rather than clear it, and its reply loses its root;
// skip the notices, and the inbox still quotes the deleted words.
func TestADeletedCommentKeepsItsPlaceAndEveryReply(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	storedBefore(n, time.Hour)
	repo, change := discussed(t, n, "web")
	w, _ := writerOn(t, n, n.pub)
	ctx := t.Context()
	say := func(asker gitlet.Principal, parent, body string) gitlet.ConversationResult {
		res, err := w.Converse(ctx, saying(repo, change, asker, parent, body))
		requireConversed(t, res, err)
		return res
	}
	root := say(dev, "", "Rotate the staging token?")
	secret := say(ada, root.ID, "The token is hunter2, rotate it.")
	kept := say(platLead, root.ID, "Rotated.")
	mine := say(platLead, "", "Looks fine.")
	if got := n.count(`SELECT COUNT(*) FROM code_notifications WHERE change_id = ? AND log_seq = ?
		AND excerpt LIKE '%hunter2%'`, change, secret.Position.Packed()); got == 0 {
		t.Fatal("no notice quotes the reply before its delete: the case proves nothing")
	}
	weighed := n.count(`SELECT text_bytes FROM code_conversations WHERE change_id = ?`, change)

	before := n.end()
	res, err := w.Converse(ctx, deleting(repo, change, platLead, secret.ID))
	requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalNotPermitted,
		`seat "plat-lead" did not write comment `+secret.ID)

	// A MAINTAINER deletes another's reply, an AUTHOR their own comment and a
	// PERSON a seat's thread's first comment.
	for _, tc := range []struct {
		asker      gitlet.Principal
		comment    string
		by, byKind string
		forgotten  int
	}{
		{cto, secret.ID, "cto", "agent", len("The token is hunter2, rotate it.")},
		{platLead, mine.ID, "plat-lead", "agent", len("Looks fine.")},
		{ada, root.ID, "founder", "person", len("Rotate the staging token?")},
	} {
		res, err := w.Converse(ctx, deleting(repo, change, tc.asker, tc.comment))
		requireConversed(t, res, err)
		if got := n.count(`SELECT COUNT(*) FROM code_comments WHERE id = ? AND body = '' AND body_bytes = 0
			AND deleted_at > 0 AND deleted_by = ? AND deleted_by_kind = ?`, tc.comment, tc.by,
			tc.byKind); got != 1 {
			t.Fatalf("comment %s is not a tombstone naming %s as who deleted it", tc.comment, tc.by)
		}
		weighed -= tc.forgotten
		if got := n.count(`SELECT text_bytes FROM code_conversations WHERE change_id = ?`, change); got != weighed {
			t.Fatalf("the conversation weighs %d bytes after the delete, want %d", got, weighed)
		}
	}
	if got := n.count(`SELECT COUNT(*) FROM code_comments WHERE id = ? AND thread_id = ? AND body = 'Rotated.'
		AND deleted_at = 0`, kept.ID, root.ID); got != 1 {
		t.Fatal("the deleted root's reply does not stand in its thread")
	}
	if got := n.count(`SELECT COUNT(*) FROM code_notifications WHERE change_id = ? AND excerpt LIKE '%hunter2%'`,
		change); got != 0 {
		t.Fatalf("%d notices still quote the deleted reply", got)
	}
	if got := n.count(`SELECT comments FROM code_conversations WHERE change_id = ?`, change); got != 4 {
		t.Fatalf("the conversation counts %d comments, want the 4 said, tombstones in their places", got)
	}

	res, err = w.Converse(ctx, deleting(repo, change, ada, root.ID))
	if err != nil || !res.Unchanged {
		t.Fatalf("a delete of a deleted comment answered %+v, %v; want unchanged", res, err)
	}
	before = n.end()
	res, err = w.Converse(ctx, editing(repo, change, ada, secret.ID, "Back again."))
	requireConversationRefused(t, n, before, res, err, gitlet.ConversationRefusalCommentGone,
		"is deleted, and a deleted comment takes no edit")
	again := say(ada, root.ID, "Thanks for rotating.")

	// READ BACK: the tombstones with who, the replies in the deleted root's thread.
	stale := statelog.Freshness{Level: statelog.ReadStale}
	answer, err := n.reader.Read(ctx, gitlet.ReadQuery{Asker: cto, Kind: gitlet.ReadChange, Repo: "web",
		Change: 1, Part: gitlet.PartComments, Fresh: stale})
	if err != nil {
		t.Fatalf("read web!1's comments: %v", err)
	}
	read := map[string]gitlet.CommentRow{}
	for _, cm := range answer.Comments {
		read[cm.ID] = cm
	}
	if cm := read[root.ID]; cm.Body != "" || cm.DeletedBy != "founder" || cm.DeletedByKind != gitlet.PartyPerson ||
		cm.DeletedAt.IsZero() || cm.Author != "dev" || !cm.EditedAt.IsZero() {
		t.Errorf("the deleted root reads %+v, want a tombstone naming founder, its author kept", cm)
	}
	if cm := read[secret.ID]; cm.DeletedBy != "cto" || cm.DeletedByKind != gitlet.PartyAgent || cm.Body != "" {
		t.Errorf("the reply a maintainer deleted reads %+v", cm)
	}
	for _, id := range []string{kept.ID, again.ID} {
		if cm := read[id]; cm.ThreadID != root.ID || cm.Body == "" || !cm.DeletedAt.IsZero() {
			t.Errorf("a reply in the deleted root's thread reads %+v", cm)
		}
	}
}

// AN EDIT AND A DELETE ARE VALIDATED BEFORE ANYTHING IS READ: a comment named
// by a whole id, an edit's words held to a comment's rules, and who writes
// stamped by the writer alone.
//
// Mutation: skip the edit's body rule, and an empty edit is proposed.
func TestACommentsEditAndDeleteAreValidatedFirst(t *testing.T) {
	t.Parallel()
	const repo, change, comment = "0192f6c1-6a7e-7c1b-9d4e-3f8a2b1c0d9e", "019a0c2e-0000-7000-8000-000000000001",
		"019a0c2e-0000-7000-8000-000000000002"
	for name, w := range map[string]gitlet.ConversationWrite{
		"an edit with no words":     editing(repo, change, ada, comment, "  "),
		"an edit of no comment":     editing(repo, change, ada, "c1", "Hi."),
		"a delete of no comment":    deleting(repo, change, ada, ""),
		"an edit naming its editor": editing(repo, change, ada, comment, "Hi."),
	} {
		if name == "an edit naming its editor" {
			w.CommentEdit.By = ada
		}
		if err := w.Validate(); err == nil {
			t.Errorf("%s validated", name)
		}
	}
	for name, w := range map[string]gitlet.ConversationWrite{
		"an edit":  editing(repo, change, ada, comment, "Hi."),
		"a delete": deleting(repo, change, ada, comment),
	} {
		w.OpID = statelog.NewOpID(time.Now(), "code_delete_comment")
		if err := w.Validate(); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}
