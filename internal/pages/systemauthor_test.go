package pages_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
)

// THE ENGINE WRITES AS ITSELF: a container the company's configuration names
// and a draft the skill-promotion pass files are nobody's decision, so they
// carry the author kind `system`, no seat handle and a name no seat can hold.

// engine is the actor the engine's own writes carry.
func engine() pages.Actor { return pages.Actor{Kind: pages.AuthorSystem} }

// A PAGE THE ENGINE WROTE IS WATCHED BY NOBODY, so a seat that happens to be
// named `system` is not woken when somebody later saves it.
//
// On a create the author's handle becomes the page's watcher, and a later
// save wakes the watchers; an engine write under a handle subscribed whichever
// seat held it.
func TestAPageTheEngineWroteIsWatchedByNobody(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(engine(), pages.NewPage{Title: "Drafted", Body: "prose"})
	got := r.get(page.Page.ID).Page
	if len(got.Watchers) != 0 {
		t.Errorf("the engine's page is watched by %v, want nobody", got.Watchers)
	}
	if got.Author != pages.SystemName {
		t.Errorf("the engine's page is authored by %q, want %q", got.Author, pages.SystemName)
	}

	if _, err := r.store.SavePage(t.Context(), author("jane"), page.Page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("reviewed")}); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	save := r.recordAt(2)
	if routed := r.routeVia(save, nil, registry(t, "system", "jane")); len(routed) != 0 {
		t.Errorf("a save of the engine's page woke %+v; nobody asked to watch it", routed)
	}
}

// A PAGE THE ENGINE WROTE IN A TEAM'S CONTAINER REACHES THAT TEAM'S LEAD, even
// one whose handle is `system`.
//
// Nobody is named on such a page, so its creation reaches the container's lead
// — which is how a lead hears about a draft to review — and the feed skips a
// lead who is the write's own author. A name a seat could hold would make the
// engine that seat's own author.
func TestAPageTheEngineWroteReachesTheLeadWhateverTheLeadIsCalled(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.write(engine(), pages.NewPage{Title: "Drafted", Body: "prose"})
	create := r.recordAt(1)
	for _, lead := range []string{"lead", "system"} {
		routed := r.routeVia(create, pages.Leads{"ENG": lead}, registry(t, lead))
		if len(routed) != 1 || routed[0].To.Handle != lead {
			t.Errorf("the engine's page in ENG, whose lead is %q, woke %+v", lead, routed)
		}
	}
}

// AN AUDIT OF WHAT PEOPLE DID DOES NOT CARRY WHAT THE ENGINE DID, and the
// engine's writes are readable as its own.
func TestTheEnginesWritesAreTheirOwnKindInTheFeed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.write(engine(), pages.NewPage{Title: "Drafted", Body: "prose"})
	r.write(pages.Actor{Kind: pages.AuthorOperator, OperatorID: "ops-1"},
		pages.NewPage{Title: "Rotation", Body: "prose"})

	people := r.activity(pages.PageActivityQuery{
		ActorKinds: []pages.AuthorKind{pages.AuthorOperator, pages.AuthorHuman},
	})
	if len(people.Changes) != 1 || people.Changes[0].Title != "Rotation" {
		t.Errorf("the audit of people's changes holds %+v, want only the token's", people.Changes)
	}
	system := r.activity(pages.PageActivityQuery{
		ActorKinds: []pages.AuthorKind{pages.AuthorSystem},
	})
	if len(system.Changes) != 1 || system.Changes[0].Title != "Drafted" {
		t.Fatalf("the engine's changes are %+v, want its one create", system.Changes)
	}
	if got := system.Changes[0]; got.ActorKind != string(pages.AuthorSystem) ||
		got.Actor != pages.SystemName {
		t.Errorf("the engine's change is recorded as %q of kind %q", got.Actor, got.ActorKind)
	}
}

// A CONTAINER THE ENGINE ENSURES IS THE ENGINE'S WRITE.
func TestAnEnsuredContainerIsWrittenByTheEngine(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, _, err := r.store.EnsureContainer(t.Context(), "PROD", "Prod", ""); err != nil {
		t.Fatalf("EnsureContainer: %v", err)
	}
	r.drain()
	record, err := pages.Decode(r.recordAt(1))
	if err != nil {
		t.Fatalf("decode the container's record: %v", err)
	}
	if record.ActorKind != pages.AuthorSystem || record.Actor != pages.SystemName {
		t.Errorf("the container was written by %q of kind %q, want the engine",
			record.Actor, record.ActorKind)
	}
}

// AN ENGINE WRITE NAMING A SEAT OR A TOKEN IS REFUSED: either would record
// the change as somebody's who never made it, and a handle would make that
// seat the page's watcher.
func TestAnEngineWriteNamingSomebodyIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, actor := range []pages.Actor{
		{Kind: pages.AuthorSystem, Handle: "system"},
		{Kind: pages.AuthorSystem, OperatorID: "ops-1"},
	} {
		if _, err := r.store.Create(t.Context(), actor,
			pages.NewPage{Container: "ENG", Title: "Drafted", Body: "prose"}); err == nil {
			t.Errorf("an engine write naming %+v was accepted", actor)
		}
	}
}
