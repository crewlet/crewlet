package doclet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/doclet"
)

// personThroughToken is a person writing through their own machine token, as
// every surface without a turn writes one down ([iam.ActorFor]): AS their seat
// `jane`, kind human, with the credential riding beside the write as its
// operator id.
func personThroughToken() doclet.Actor {
	return doclet.Actor{Handle: "jane", Kind: doclet.AuthorHuman, OperatorID: "pat:4c1e"}
}

// unboundCredential is a credential nobody is bound through: a Tier A token,
// writing under its whole login with kind operator.
func unboundCredential() doclet.Actor {
	return doclet.Actor{
		Handle: "token:assistant", Kind: doclet.AuthorOperator, OperatorID: "token:assistant",
	}
}

// A PERSON'S SUBSCRIPTION IS THEIR SEAT, NEVER THEIR CREDENTIAL.
//
// A wake reaches a seat, and a token is not one. A person writes AS their seat
// whichever credential carried the write, so the page they create from the
// dashboard is watched by the seat a wake can reach, and their watch and
// unwatch move that seat's subscription, never one kept under the token.
func TestAPersonsWatchFollowsTheirSeatNotTheirToken(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.write(personThroughToken(), doclet.NewPage{Title: "Runbook", Body: "prose"})
	if !slices.Equal(created.Page.Watchers, []string{"jane"}) {
		t.Fatalf("the person's create subscribes %v, want their seat alone", created.Page.Watchers)
	}
	if created.Page.Author != "jane" {
		t.Fatalf("the author is %q, want the seat the person writes as: the "+
			"credential is the operator id beside it, never a second name for them",
			created.Page.Author)
	}

	if _, err := r.store.SavePage(t.Context(), personThroughToken(), created.Page.ID,
		doclet.Save{BaseVersion: 1, Watch: ptr(false)}); err != nil {
		t.Fatalf("unwatch: %v", err)
	}
	r.drain()
	page := r.get(created.Page.ID).Page
	if !slices.Equal(page.Muted, []string{"jane"}) {
		t.Fatalf("an unwatch mutes %v, want the person's seat alone", page.Muted)
	}
	if watching := r.list(doclet.Filter{Container: "ENG", Watcher: "jane"}); len(watching.Pages) != 0 {
		t.Fatalf("a muted page is still listed as watched by its person: %v", titles(watching))
	}

	if _, err := r.store.SavePage(t.Context(), personThroughToken(), created.Page.ID,
		doclet.Save{BaseVersion: 1, Watch: ptr(true)}); err != nil {
		t.Fatalf("watch again: %v", err)
	}
	r.drain()
	if watching := r.list(doclet.Filter{Container: "ENG", Watcher: "jane"}); len(watching.Pages) != 1 {
		t.Fatalf("watching again lists %v under the person, want the page", titles(watching))
	}
}

// A CREDENTIAL NOBODY IS BOUND THROUGH SUBSCRIBES NOBODY.
//
// Its login is no seat, so a subscription kept under it is a watcher no wake
// can reach ([Actor.subscriber]): its create is watched by whoever it named and
// nobody else, and its `watch: true` moves neither set. The unwatch half is
// [TestAnOperatorsWatchToggleChangesNothing]'s.
func TestATokenBoundToNobodySubscribesNobody(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.write(unboundCredential(), doclet.NewPage{Title: "Runbook", Body: "prose"})
	if len(created.Page.Watchers) != 0 {
		t.Fatalf("an unbound credential's create subscribes %v, want nobody",
			created.Page.Watchers)
	}
	before := r.consumed
	if _, err := r.store.SavePage(t.Context(), unboundCredential(), created.Page.ID,
		doclet.Save{BaseVersion: 1, Watch: ptr(true)}); err != nil {
		t.Fatalf("watch: %v", err)
	}
	r.drain()
	if r.consumed != before {
		t.Errorf("an unbound credential's watch appended a record (%d -> %d), for a "+
			"change to nobody's subscription", before, r.consumed)
	}
	if page := r.get(created.Page.ID).Page; len(page.Watchers) != 0 || len(page.Muted) != 0 {
		t.Fatalf("an unbound credential's watch moved watchers %v and muted %v",
			page.Watchers, page.Muted)
	}
}

// A PERSON IS NEVER WOKEN BY THEIR OWN WRITE, under the one name it is recorded
// under.
//
// Their subscription is their seat and so is the name their write carries,
// whichever credential carried it, so the parser's one comparison spares them
// both as a watcher and as the lead of the space they wrote in.
func TestAPersonIsNotWokenByTheirOwnWrite(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	watched := r.write(personThroughToken(), doclet.NewPage{
		Title: "Runbook", Body: "prose", Watchers: []string{"carla"},
	})
	if _, err := r.store.SavePage(t.Context(), personThroughToken(), watched.Page.ID,
		doclet.Save{BaseVersion: 1, Body: ptr("the deploy steps changed")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	var woken []string
	for _, routed := range r.route(r.lastRecord(), doclet.Leads{"ENG": "jane"}) {
		woken = append(woken, routed.To.Handle)
	}
	if !slices.Equal(woken, []string{"carla"}) {
		t.Fatalf("the person's own save woke %v, want the other watcher alone", woken)
	}

	// A create in the space the person leads, which nobody else watches: the
	// lead's copy is the only one there is, and it is the writer's own.
	r.write(personThroughToken(), doclet.NewPage{Title: "Oncall", Body: "prose"})
	if routed := r.route(r.lastRecord(), doclet.Leads{"ENG": "jane"}); len(routed) != 0 {
		t.Fatalf("the person's create woke %v in the space they lead, want nobody", routed)
	}
}

// THE RECORD AND THE HISTORY ROW NAME THE PERSON AS THEIR SEAT, with the
// credential beside it as the operator id, and carry nothing a base-version
// build cannot read: there is no second name for the person to store.
func TestAHistoryRowNamesThePersonAndTheCredential(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	created := r.write(personThroughToken(), doclet.NewPage{Title: "Runbook", Body: "prose"})
	record, err := doclet.Decode(r.lastRecord())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if record.Actor != "jane" || record.ActorKind != doclet.AuthorHuman ||
		record.OperatorID != "pat:4c1e" || record.V != doclet.RecordVersion {
		t.Fatalf("the record names %q (%s) through %q at version %d, want jane "+
			"(human) through pat:4c1e at %d", record.Actor, record.ActorKind,
			record.OperatorID, record.V, doclet.RecordVersion)
	}
	changes := r.activity(doclet.PageActivityQuery{Page: created.Page.ID}).Changes
	if len(changes) != 1 || changes[0].Actor != "jane" || changes[0].OperatorID != "pat:4c1e" {
		t.Fatalf("the history reads %+v, want the person's seat as actor and the "+
			"credential beside it", changes)
	}

	// A SEAT'S OWN WRITE is recorded on the same terms, at the same version.
	r.write(agent("swe"), doclet.NewPage{Title: "Design", Body: "prose"})
	if record, err := doclet.Decode(r.lastRecord()); err != nil || record.Actor != "swe" ||
		record.V != doclet.RecordVersion {
		t.Fatalf("a seat's record names %q at version %d (err %v), want swe at %d",
			record.Actor, record.V, err, doclet.RecordVersion)
	}
}
