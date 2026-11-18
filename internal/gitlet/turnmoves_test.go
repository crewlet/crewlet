package gitlet_test

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TURN MOVED CODE WHERE IT PUSHED, OPENED A CHANGE OR GAVE A REVIEW: a turn
// that only commented on a change did not, one whose only push a protection
// refused whole (an attempt) did not, and one that wrote nothing is not named.
// Mutation: count every push row, and the attempt's turn reads as moved.
func TestATurnMovedCodeWhereItPushedOpenedAChangeOrReviewed(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	s := suiteSettings()
	s.Owner = gitlet.Owner{Kind: gitlet.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].DirectPush, s.Protections[0].Gate = gitlet.DirectPersons, mergeGate()
	repo := n.ensure("web", s, t1).RepoID
	push := entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, gitlet.ZeroOID, oidOf("A")),
		set(seatRef, gitlet.ZeroOID, oidOf("B")))
	push.Principal.Run = gitlet.RunProvenance{TurnID: "turn-push"}
	n.push(repo, push)
	refused := set(mainRef, oidOf("A"), oidOf("C"))
	refused.Result, refused.Reason = gitlet.ResultNG, gitlet.RefusalProtectedDirect
	attempt := entry(uuid.Must(uuid.NewV7()).String(), refused)
	attempt.Pack, attempt.Offered = nil, 1
	attempt.Principal.Run = gitlet.RunProvenance{TurnID: "turn-refused"}
	n.push(repo, attempt)
	if rows := n.count(`SELECT COUNT(*) FROM code_pushes WHERE turn_id = 'turn-refused'`); rows != 1 {
		t.Fatalf("%d rows of the attempt, want its one", rows)
	}
	open := opening(repo, seatRef, mainRef, dev)
	open.Open.TurnID = "turn-open"
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	comment := saying(repo, opened.ChangeID, dev, "", "this retries forever")
	comment.Comment.TurnID = "turn-words"
	converse(t, w, comment)
	review := reviewing(repo, opened.ChangeID, platLead, 1, gitlet.VerdictApprove, nil)
	review.Review.TurnID = "turn-review"
	converse(t, w, review)

	got, err := n.reader.TurnsMovedCode(t.Context(), gitlet.TurnsMovedQuery{
		Turns: []string{"turn-push", "turn-refused", "turn-open", "turn-words", "turn-review", "turn-idle"},
		Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"turn-push": true, "turn-open": true, "turn-review": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("turns that moved code = %v, want %v", got, want)
	}
}
