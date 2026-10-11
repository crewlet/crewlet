package pages_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
)

// boundOperator is a person writing through their own token: the token is
// `founder` and the seat it is bound to is `jane`.
func boundOperator() pages.Actor {
	return pages.Actor{Handle: "founder", Kind: pages.AuthorOperator, OperatorID: "founder", Seat: "jane"}
}

// unboundOperator is an assistant's token bound to nobody.
func unboundOperator() pages.Actor {
	return pages.Actor{Handle: "assistant", Kind: pages.AuthorOperator, OperatorID: "assistant"}
}

// A PERSON'S SUBSCRIPTION IS THEIR SEAT, NEVER THEIR TOKEN.
//
// A wake reaches a seat, and a token is not one: the page a founder created
// from the dashboard used to list `founder` as its watcher, so every change to
// it was routed to a handle the roster dropped, and the founder was told about
// nothing they followed.
func TestAPersonsWatchFollowsTheirSeatNotTheirToken(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.write(boundOperator(), pages.NewPage{Title: "Runbook", Body: "prose"})
	if !slices.Equal(created.Page.Watchers, []string{"jane"}) {
		t.Fatalf("the person's create subscribes %v, want their seat alone", created.Page.Watchers)
	}
	if created.Page.Author != "founder" {
		t.Fatalf("the author is %q, want the token: the author field is the audit trail",
			created.Page.Author)
	}

	if _, err := r.store.SavePage(t.Context(), boundOperator(), created.Page.ID,
		pages.Save{BaseVersion: 1, Watch: ptr(false)}); err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	r.drain()
	page := r.get(created.Page.ID).Page
	if !slices.Contains(page.Muted, "jane") || slices.Contains(page.Muted, "founder") {
		t.Fatalf("an unwatch mutes %v, want the person's seat", page.Muted)
	}
	if watching := r.list(pages.Filter{Container: "ENG", Watcher: "jane"}); len(watching.Pages) != 0 {
		t.Fatalf("a muted page is still listed as watched by its person: %v", titles(watching))
	}

	if _, err := r.store.SavePage(t.Context(), boundOperator(), created.Page.ID,
		pages.Save{BaseVersion: 1, Watch: ptr(true)}); err != nil {
		t.Fatalf("watch again: %v", err)
	}
	r.drain()
	if watching := r.list(pages.Filter{Container: "ENG", Watcher: "jane"}); len(watching.Pages) != 1 {
		t.Fatalf("watching again lists %v under the person, want the page", titles(watching))
	}
}

// A TOKEN BOUND TO NOBODY HOLDS NO SUBSCRIPTION: there is nobody a wake for it
// could reach, so neither its create nor its watch writes one.
func TestATokenBoundToNobodySubscribesNobody(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.write(unboundOperator(), pages.NewPage{Title: "Runbook", Body: "prose"})
	if len(created.Page.Watchers) != 0 {
		t.Fatalf("an unbound token's create subscribes %v, want nobody", created.Page.Watchers)
	}
	if _, err := r.store.SavePage(t.Context(), unboundOperator(), created.Page.ID,
		pages.Save{BaseVersion: 1, Watch: ptr(true)}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	r.drain()
	if page := r.get(created.Page.ID).Page; len(page.Watchers) != 0 || len(page.Muted) != 0 {
		t.Fatalf("an unbound token's watch moved watchers %v and muted %v", page.Watchers, page.Muted)
	}
}

// A PERSON IS NEVER WOKEN BY THEIR OWN WRITE, under either of their names.
//
// Their subscription is their seat and their write is recorded under their
// token, so a parser that compared the recipient with the actor alone woke a
// founder for every save they made to a page they follow, and gave them the
// lead's copy of every page they created in the space they lead.
func TestAPersonIsNotWokenByTheirOwnWrite(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	watched := r.write(boundOperator(), pages.NewPage{
		Title: "Runbook", Body: "prose", Watchers: []string{"carla"},
	})
	if _, err := r.store.SavePage(t.Context(), boundOperator(), watched.Page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("the deploy steps changed")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	var woken []string
	for _, routed := range r.route(r.lastRecord(), pages.Leads{"ENG": "jane"}) {
		woken = append(woken, routed.To.Handle)
	}
	if !slices.Equal(woken, []string{"carla"}) {
		t.Fatalf("the person's own save woke %v, want the other watcher alone", woken)
	}

	// A create in the space the person leads, which nobody else watches: the
	// lead's copy is the only one there is, and it is the writer's own.
	r.write(boundOperator(), pages.NewPage{Title: "Oncall", Body: "prose"})
	if routed := r.route(r.lastRecord(), pages.Leads{"ENG": "jane"}); len(routed) != 0 {
		t.Fatalf("the person's create woke %v in the space they lead, want nobody", routed)
	}
}

// THE RECORD CARRIES THE PERSON, and the history row a screen draws names
// them beside the token.
func TestAHistoryRowNamesThePersonBehindAToken(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.write(boundOperator(), pages.NewPage{Title: "Runbook", Body: "prose"})
	record, err := pages.Decode(r.lastRecord())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if record.ActorSeat != "jane" || record.V != 2 {
		t.Fatalf("the record carries seat %q at version %d, want jane at 2: a build "+
			"with no column for the seat must retain it", record.ActorSeat, record.V)
	}
	changes := r.activity(pages.PageActivityQuery{Page: created.Page.ID}).Changes
	if len(changes) != 1 || changes[0].Actor != "founder" || changes[0].ActorSeat != "jane" {
		t.Fatalf("the history reads %+v, want the token as actor and the person beside it", changes)
	}

	// A SEAT'S OWN WRITE carries nothing new: its actor is already the
	// person, and its record stays at the base version every build reads.
	r.write(agent("swe"), pages.NewPage{Title: "Design", Body: "prose"})
	if record, err := pages.Decode(r.lastRecord()); err != nil || record.ActorSeat != "" || record.V != 1 {
		t.Fatalf("a seat's record carries seat %q at version %d (err %v), want none at 1",
			record.ActorSeat, record.V, err)
	}
}
