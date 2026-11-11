package worklet_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ownAsks reads asker's open asks to person, at most limit.
func (r *roundTrip) ownAsks(asker, person string, limit int) worklet.OwnAsks {
	r.t.Helper()
	out, err := r.reader.OwnAsks(r.t.Context(), worklet.OwnAsksQuery{Asker: asker, AskedOf: person,
		Limit: limit, Level: statelog.ReadStale}, wednesday)
	if err != nil {
		r.t.Fatalf("OwnAsks(%s, %s): %v", asker, person, err)
	}
	return out
}

// A SEAT'S OWN OPEN ASKS TO A PERSON, newest first: another seat's question,
// a question to somebody else, one the person answered and one the seat
// withdrew are not among them, and each row carries the call that withdraws it
// and its decision's question.
//
// Mutation: drop the author term and cy's question is listed as bo's; drop the
// answered term and the answered question still waits.
func TestASeatsOwnOpenAsksToAPersonAreReadNewestFirst(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		assign(t, r, id, "bo")
	}
	ask := func(id, task, author, person string, at time.Duration) {
		askOn(t, r, "op-"+id, task, worklet.Comment{ID: id, Task: task, Author: author,
			AuthorKind: worklet.AuthorAgent, Body: "Merge it?", Ask: person, Decision: shipOrHold,
			CreatedAt: wednesday.Add(-at)})
	}
	ask("c-old", "a", "bo", "jane", 3*time.Hour)
	ask("c-new", "b", "bo", "jane", time.Hour)
	ask("c-cy", "a", "cy", "jane", 2*time.Hour)
	ask("c-kim", "c", "bo", "kim", time.Hour)
	ask("c-answered", "c", "bo", "jane", 2*time.Hour)
	answerOn(t, r, "op-answer", "c", "c-answer", "jane", "c-answered", "ship", "Ship it",
		wednesday.Add(-time.Hour))
	ask("c-gone", "d", "bo", "jane", 2*time.Hour)
	bo := r.writer.As("bo", worklet.AuthorAgent, aTurnNobodyWoke())
	if _, err := bo.ResolveComment(t.Context(), "op-withdraw", "d", "ENG", "c-gone", nil,
		nil); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	r.drain()

	got := r.ownAsks("bo", "jane", 0)
	var ids []string
	for _, a := range got.Asks {
		ids = append(ids, a.Comment)
	}
	if !slices.Equal(ids, []string{"c-new", "c-old"}) || got.Total.Total != 2 {
		t.Fatalf("bo's open asks to jane = %v of %d, want c-new then c-old", ids, got.Total.Total)
	}
	first := got.Asks[0]
	if first.ID != "b" || first.Question != shipOrHold.Question || first.AskedOf != "jane" ||
		!strings.Contains(first.Withdraw, `withdraw_ask(item: "`+first.Address()+`", ask: "c-new"`) {
		t.Errorf("the newest ask reads %+v, want b's question with its withdraw call", first)
	}
	if page := r.ownAsks("bo", "jane", 1); len(page.Asks) != 1 || page.Total.Total != 2 {
		t.Errorf("a page of one holds %d of %d, want one of two", len(page.Asks), page.Total.Total)
	}
	if _, err := r.reader.OwnAsks(t.Context(), worklet.OwnAsksQuery{Asker: "bo",
		Level: statelog.ReadStale}, wednesday); err == nil {
		t.Error("a read naming no person answered")
	}
}
