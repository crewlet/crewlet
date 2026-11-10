package doclet_test

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
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

// A PAGE ACTOR CROSSES THE WIRE AS IT WAS STATED (ADR-0109), under the rule of
// the arguments it crosses in, as the estate's wire encodes and decodes them
// (crewlet#910). Through the caller's own encoder every field at its zero is
// left off, so an agent's actor stated for nobody goes out without the key,
// byte for byte as a build without the field sends it, and one stated for
// somebody carries them. Through the caller's own decoder it arrives stated,
// whatever members did, so an older node's actor, which knows no carrier,
// arrives as stated nobody; and a member nothing declares, in the actor or in
// its carrier, is refused where it is rather than dropped below where that
// rule can see. (That no node asks a page write whose actor may write nothing
// is internal/estate's, and held there.)
//
// Mutation: give the actor an UnmarshalJSON over encoding/json in place of
// its UnmarshalJSONFrom, and the member inside it is dropped; state the actor
// only where a carrier arrives, and the older node's actor arrives stating
// nothing.
func TestAPageActorCrossesTheWireAsItWasStated(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	wire := jsonv2.OmitZeroStructFields(true)
	for name, c := range map[string]struct {
		writtenFor types.WrittenFor
		want       string
	}{
		"an agent stated for nobody": {types.ForTurn(nil), `{"Handle":"dev","Kind":"agent"}`},
		"an agent stated for ada": {types.ForTurn(ada),
			`{"Handle":"dev","Kind":"agent","WrittenFor":{"kind":"seat","seat":"ada"}}`},
	} {
		got, err := jsonv2.Marshal(doclet.Actor{Handle: "dev", Kind: doclet.AuthorAgent,
			WrittenFor: c.writtenFor}, wire)
		var back doclet.Actor
		if err == nil {
			err = jsonv2.Unmarshal(got, &back, jsonv2.RejectUnknownMembers(true))
		}
		if err != nil || string(got) != c.want || !back.WrittenFor.Stated() ||
			!reflect.DeepEqual(back.WrittenFor.Carrier(), c.writtenFor.Carrier()) {
			t.Errorf("%s goes out as %s and arrives written for %+v, stated %v (%v), want %s, stated",
				name, got, back.WrittenFor.Carrier(), back.WrittenFor.Stated(), err, c.want)
		}
	}

	var older doclet.Actor
	if err := jsonv2.Unmarshal([]byte(`{"Handle":"dev","Kind":"agent"}`), &older,
		jsonv2.RejectUnknownMembers(true)); err != nil || !older.WrittenFor.Stated() ||
		older.WrittenFor.Carrier() != nil {
		t.Errorf("an older node's actor arrives written for %+v, stated %v (%v), want nobody, stated",
			older.WrittenFor.Carrier(), older.WrittenFor.Stated(), err)
	}

	for _, c := range []struct{ raw, at string }{
		{`{"Actor":{"Handle":"dev","Kind":"agent","Later":1}}`, "/Actor/Later"},
		{`{"Actor":{"Handle":"dev","Kind":"agent","WrittenFor":{"kind":"seat","seat":"ada","later":1}}}`,
			"/Actor/WrittenFor/later"},
	} {
		var args struct{ Actor doclet.Actor }
		err := jsonv2.Unmarshal([]byte(c.raw), &args, jsonv2.RejectUnknownMembers(true))
		var semantic *jsonv2.SemanticError
		if !errors.As(err, &semantic) || !errors.Is(err, jsonv2.ErrUnknownName) ||
			string(semantic.JSONPointer) != c.at {
			t.Errorf("%s decoded as %v, want it refused at %s", c.raw, err, c.at)
		}
	}
}
