package code_test

import (
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A TURN MOVED CODE WHERE IT PUSHED, OPENED A CHANGE OR GAVE A REVIEW: a turn
// that only commented on a change did not, and one that wrote nothing is not
// named.
func TestATurnMovedCodeWhereItPushedOpenedAChangeOrReviewed(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.chart = testChart()
	c, _ := mergeStream(t, n)
	w, _ := writerOn(t, n, n.pub)
	s := suiteSettings()
	s.Owner = code.Owner{Kind: code.OwnerUnit, Ref: "Engineering"}
	s.Protections[0].DirectPush, s.Protections[0].Gate = code.DirectPersons, mergeGate()
	repo := n.ensure("web", s, t1).RepoID
	push := entry(uuid.Must(uuid.NewV7()).String(), set(mainRef, code.ZeroOID, oidOf("A")),
		set(seatRef, code.ZeroOID, oidOf("B")))
	push.Principal.Run = code.RunProvenance{TurnID: "turn-push"}
	n.push(repo, push)
	open := opening(repo, seatRef, mainRef, dev)
	open.Open.TurnID = "turn-open"
	opened, err := c.ProposeChange(t.Context(), open)
	requireChanged(t, opened, err)
	comment := saying(repo, opened.ChangeID, dev, "", "this retries forever")
	comment.Comment.TurnID = "turn-words"
	converse(t, w, comment)
	review := reviewing(repo, opened.ChangeID, platLead, 1, code.VerdictApprove, nil)
	review.Review.TurnID = "turn-review"
	converse(t, w, review)

	got, err := n.reader.TurnsMovedCode(t.Context(), code.TurnsMovedQuery{
		Turns: []string{"turn-push", "turn-open", "turn-words", "turn-review", "turn-idle"},
		Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"turn-push": true, "turn-open": true, "turn-review": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("turns that moved code = %v, want %v", got, want)
	}
}
