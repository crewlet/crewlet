package codetest

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CHANGE CASES: a change's lifecycle is proposed to its repository's
// committer as a push is, and survives what a push's proposal does.

// changeAsker is who a change case opens and writes as: a person, through a
// credential bound to their human seat — what every data node of this package
// decides with no chart, which judges no seat.
var changeAsker = code.Principal{Kind: code.PrincipalOperator, OperatorID: "codetest-changes", Seat: "ceo"}

// openOf is an open of source into main on the repository, asked by
// [changeAsker] under an operation id of its own.
func openOf(repoID, source, title string) code.ChangeProposal {
	opID := statelog.NewOpID(time.Now(), "code_open_change")
	return code.ChangeProposal{OpID: opID, Asker: changeAsker, Open: &code.ChangeOpen{RepoID: repoID,
		ChangeID: code.OpenChangeID(opID), SourceRef: source, TargetRef: "refs/heads/main", Title: title}}
}

// F31: an open whose committer dies after appending its record and before it
// answered is asked of the repository's runner-up under its OWN operation id,
// which the ledger collapses onto the record that landed: the open is answered
// applied, with the change and the number that record took, and no second
// change is opened — asked again it is the same change, appending nothing, and
// the repository's next open takes the next number. The dead committer, back,
// applies the one record and holds the rows every member does.
func aChangeOpenSurvivesItsCommitter(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f31")
	committer := rc.committer()
	r := rc.other(committer)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, r, "main")
	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	rc.mustPush(p, work, r, "HEAD:refs/heads/feature")
	p.Commit(work, map[string]string{"README.md": "three\n"}, "three")
	rc.mustPush(p, work, r, "HEAD:refs/heads/other")

	open := openOf(rc.id, "refs/heads/feature", "Two")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Faults().Arm(codefault.CommitterAfterAppend, codefault.Action{Once: true, Block: held, Reached: reached})
	defer close(held)
	type answer struct {
		res code.ChangeResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := c.ChangeThrough(rc.ctx, r, open)
		done <- answer{res, err}
	}()
	select {
	case <-reached:
	case got := <-done:
		t.Fatalf("the open ended before its committer appended it: %+v, %v", got.res, got.err)
	}
	c.Kill(t, committer)
	restarted := false
	defer func() {
		if !restarted {
			c.Restart(t, committer)
		}
	}()
	got := <-done
	if got.err != nil || got.res.Outcome != statelog.OutcomeApplied || got.res.ChangeID != open.Open.ChangeID ||
		got.res.Number != 1 {
		t.Fatalf("an open whose committer died after appending it answered %+v, %v; want change %s applied as "+
			"fleet-f31!1", got.res, got.err, open.Open.ChangeID)
	}

	appended := 0
	for _, n := range c.DataNodes() {
		appended += c.Appends(n)
	}
	again, err := c.ChangeThrough(rc.ctx, rc.other(committer, r), open)
	if err != nil || again.Outcome != statelog.OutcomeApplied || again.ChangeID != open.Open.ChangeID ||
		again.Number != 1 {
		t.Fatalf("the open asked again answered %+v, %v; want the same change, fleet-f31!1", again, err)
	}
	after := 0
	for _, n := range c.DataNodes() {
		after += c.Appends(n)
	}
	if after != appended {
		t.Fatalf("the open asked again appended %d records, want none", after-appended)
	}
	next, err := c.ChangeThrough(rc.ctx, r, openOf(rc.id, "refs/heads/other", "Three"))
	if err != nil || next.Outcome != statelog.OutcomeApplied || next.Number != 2 {
		t.Fatalf("the repository's next open answered %+v, %v; want fleet-f31!2 — one change opened before it",
			next, err)
	}
	rc.converged()
	c.Restart(t, committer)
	restarted = true
	rc.converged()
}

// commentOf is a comment on a change of the repository, asked by
// [changeAsker] under an operation id of its own — a reply to parent where one
// is named.
func commentOf(repoID, changeID, parent, body string) code.ConversationWrite {
	return code.ConversationWrite{OpID: statelog.NewOpID(time.Now(), "code_comment_on_change"),
		Asker: changeAsker, Comment: &code.Comment{RepoID: repoID, ChangeID: changeID, ParentID: parent,
			Body: body}}
}

// F32: a comment whose writer dies after appending its record and before it
// answered is asked of the repository's runner-up under its OWN operation id,
// which the ledger collapses onto the record that landed: the comment is
// answered applied, by the id that record gave it, and no second comment is
// said — asked again it is the same comment, appending nothing. A reply to it,
// whose decide reads the thread's root, lands through another member; and the
// dead writer, back, applies the records every member does.
func aConversationSurvivesItsWriter(t *testing.T, c Cluster) {
	rc := newRepoCase(t, c, "fleet-f32")
	writer := rc.committer()
	r := rc.other(writer)
	p := NewPerson(t, c, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	rc.mustPush(p, work, r, "main")
	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	rc.mustPush(p, work, r, "HEAD:refs/heads/feature")
	opened, err := c.ChangeThrough(rc.ctx, r, openOf(rc.id, "refs/heads/feature", "Two"))
	if err != nil || opened.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the change to discuss answered %+v, %v; want it opened", opened, err)
	}

	say := commentOf(rc.id, opened.ChangeID, "", "Why two?")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Faults().Arm(codefault.CommitterAfterAppend, codefault.Action{Once: true, Block: held, Reached: reached})
	defer close(held)
	type answer struct {
		res code.ConversationResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := c.ConverseThrough(rc.ctx, r, say)
		done <- answer{res, err}
	}()
	select {
	case <-reached:
	case got := <-done:
		t.Fatalf("the comment ended before its writer appended it: %+v, %v", got.res, got.err)
	}
	c.Kill(t, writer)
	restarted := false
	defer func() {
		if !restarted {
			c.Restart(t, writer)
		}
	}()
	got := <-done
	if got.err != nil || got.res.Outcome != statelog.OutcomeApplied || got.res.ID != code.CommentID(say.OpID) {
		t.Fatalf("a comment whose writer died after appending it answered %+v, %v; want comment %s applied",
			got.res, got.err, code.CommentID(say.OpID))
	}

	appended := 0
	for _, n := range c.DataNodes() {
		appended += c.Appends(n)
	}
	again, err := c.ConverseThrough(rc.ctx, rc.other(writer, r), say)
	if err != nil || again.Outcome != statelog.OutcomeApplied || again.ID != got.res.ID ||
		again.Position != got.res.Position {
		t.Fatalf("the comment asked again answered %+v, %v; want the same comment at %s", again, err,
			got.res.Position)
	}
	after := 0
	for _, n := range c.DataNodes() {
		after += c.Appends(n)
	}
	if after != appended {
		t.Fatalf("the comment asked again appended %d records, want none", after-appended)
	}
	reply, err := c.ConverseThrough(rc.ctx, r, commentOf(rc.id, opened.ChangeID, got.res.ID,
		"Because one is not."))
	if err != nil || reply.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a reply to the comment answered %+v, %v; want it applied in the comment's thread", reply, err)
	}
	rc.converged()
	c.Restart(t, writer)
	restarted = true
	rc.converged()
}
