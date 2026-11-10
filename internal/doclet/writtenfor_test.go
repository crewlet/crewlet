package doclet_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
)

// AN AGENT'S WRITE THAT WAKES SOMEBODY CARRIES WHOM IT WAS WRITTEN FOR, AND
// NOTHING ELSE DOES (ADR-0109), on the knowledge base's store as on the
// tracker's. An agent's comment asking bob, made in a turn ada woke, records
// that the turn worked for ada, at the version that carries it, which an older
// build retains rather than waking bob for the agent. A quiet rename by the
// same agent carries nobody and stays at the base version. A comment a turn
// nobody woke makes carries nobody too, so it stays below the carrier's
// version, at the one every agent comment's statement of whether it asks needs.
//
// AND THE TYPE REFUSES THE ACTOR THAT FORGOT: an agent that states nobody it
// writes for writes nothing, and a person handed a carrier writes nothing
// either. An actor decoded off the estate's wire states it already.
//
// Mutation: leave the carrier off the record the store builds, and ada's
// comment carries nobody; keep it on a record that wakes nobody, and the quiet
// rename carries ada; drop the refusal in recordedFor, and the unstated
// agent's comment lands.
func TestAnAgentsPageWriteThatWakesSomebodyCarriesWhomItWasWrittenFor(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	page := r.write(author("jane"), doclet.NewPage{Title: "Runbook", Body: "prose"})
	forAda := agent("dev")
	forAda.WrittenFor = types.ForTurn(ada)
	mention := doclet.NewComment{Body: "@bob is this current?", Mentions: []string{"bob"}, Asks: ptr(true)}

	if _, _, err := r.store.Comment(t.Context(), forAda, page.Page.ID, mention); err != nil {
		t.Fatalf("dev comments for ada: %v", err)
	}
	expect := func(what string, want *types.WorkingFor, version int) {
		t.Helper()
		record, err := doclet.Decode(r.lastRecord())
		if err != nil {
			t.Fatalf("decode %s: %v", what, err)
		}
		if !reflect.DeepEqual(record.WorkingFor, want) || record.V != version {
			t.Errorf("%s is recorded written for %+v at version %d, want %+v at %d",
				what, record.WorkingFor, record.V, want, version)
		}
	}
	expect("the comment made for ada", ada, 4)

	if _, err := r.store.Rename(t.Context(), forAda, page.Page.ID, "Runbook, current", true); err != nil {
		t.Fatalf("dev renames quietly: %v", err)
	}
	expect("the quiet rename", nil, 1)

	if _, _, err := r.store.Comment(t.Context(), agent("dev"), page.Page.ID, mention); err != nil {
		t.Fatalf("dev comments in a turn nobody woke: %v", err)
	}
	expect("the comment of a turn nobody woke", nil, 3)

	unstated := agent("dev")
	unstated.WrittenFor = types.WrittenFor{}
	handed := author("jane")
	handed.WrittenFor = types.ForTurn(ada)
	for name, c := range map[string]struct {
		actor doclet.Actor
		cause error
	}{
		"an agent that states nobody": {unstated, types.ErrWrittenForUnstated},
		"a person handed a carrier":   {handed, types.ErrWrittenForNotAnAgents},
	} {
		if _, _, err := r.store.Comment(t.Context(), c.actor, page.Page.ID, mention); !errors.Is(err, c.cause) {
			t.Errorf("%s comments: %v, want refused for %v", name, err, c.cause)
		}
	}

	// AN ACTOR THAT CROSSED THE WIRE carrying nobody is stated: the node
	// that built it decided, and an older node sends exactly this.
	var crossed doclet.Actor
	if err := json.Unmarshal([]byte(`{"Handle":"dev","Kind":"agent"}`), &crossed); err != nil {
		t.Fatalf("decode an actor: %v", err)
	}
	if _, _, err := r.store.Comment(t.Context(), crossed, page.Page.ID, mention); err != nil {
		t.Errorf("an agent's comment that crossed the wire carrying nobody was refused: %v", err)
	}
}

// A PAGE ACTOR THAT CANNOT WRITE IS NEVER SENT (ADR-0109). The node that built
// it refuses to encode an agent's actor that states nobody it writes for, and
// a person's handed a carrier, alone or inside the arguments a page write
// crosses in: the data node it would cross to reads an absent carrier as
// nobody, so the turn the write woke would work for the writing seat, every
// app. An agent's actor stated for nobody goes out without the key, byte for
// byte as a build without the field sends it, and one stated for somebody
// carries them.
//
// Mutation: drop the check in Actor.MarshalJSON, and the agent's actor that
// states nothing is encoded.
func TestAPageActorThatCannotWriteIsNeverSent(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	unstated := agent("dev")
	unstated.WrittenFor = types.WrittenFor{}
	handed := author("jane")
	handed.WrittenFor = types.ForTurn(ada)
	for name, c := range map[string]struct {
		actor doclet.Actor
		cause error
	}{
		"an agent that states nothing": {unstated, types.ErrWrittenForUnstated},
		"a person handed a carrier":    {handed, types.ErrWrittenForNotAnAgents},
	} {
		if _, err := json.Marshal(c.actor); !errors.Is(err, c.cause) {
			t.Errorf("%s is encoded (%v), want it refused for %v", name, err, c.cause)
		}
		if _, err := json.Marshal(struct{ Actor doclet.Actor }{c.actor}); !errors.Is(err, c.cause) {
			t.Errorf("arguments carrying %s are encoded (%v), want them refused for %v", name, err, c.cause)
		}
	}

	stated := doclet.Actor{Handle: "dev", Kind: doclet.AuthorAgent, WrittenFor: types.ForTurn(nil)}
	got, err := json.Marshal(stated)
	want := `{"Handle":"dev","Kind":"agent","OperatorID":"","TurnID":"","Chain":null,"OpKey":""}`
	if err != nil || string(got) != want {
		t.Errorf("an agent stated for nobody goes out as %s (%v), want %s", got, err, want)
	}
	stated.WrittenFor = types.ForTurn(ada)
	got, err = json.Marshal(stated)
	var back doclet.Actor
	if err == nil {
		err = json.Unmarshal(got, &back)
	}
	if err != nil || !reflect.DeepEqual(back.WrittenFor.Carrier(), ada) {
		t.Errorf("an agent stated for ada goes out as %s and arrives written for %+v (%v), want ada",
			got, back.WrittenFor.Carrier(), err)
	}
}
