package chatlet_test

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// AN AGENT'S MESSAGE THAT WAKES SOMEBODY CARRIES WHOM IT WAS WRITTEN FOR, AND
// NOTHING ELSE DOES (ADR-0109), on chat's store as on the tracker's. An
// agent's message asking a colleague in the room, said in a turn ada woke,
// records that the turn worked for ada, at the version that carries it, which
// an older build retains rather than waking the colleague for the agent; and
// the wake the parser derives from it names the agent's seat as its writer,
// carrying ada, so the partition keys it as a seat's (`seat:`, as every
// first-party writer's). The room the same agent makes wakes nobody and
// carries nobody, at the base version, and the same ask a turn nobody woke
// says carries nobody either, at the version its ask alone needs.
//
// AND THE TYPE REFUSES THE ACTOR THAT FORGOT: an agent that states nobody it
// writes for says nothing, and a person handed a carrier says nothing either.
// An actor decoded off the estate's wire states it already.
//
// Mutation: leave the carrier off the record the store builds, and ada's
// message carries nobody; leave the writer off the inbound the parser builds,
// and the wake names no writer; drop the refusal in recordedFor, and the
// unstated agent's message lands.
func TestAnAgentsMessageThatWakesSomebodyCarriesWhomItWasWrittenFor(t *testing.T) {
	t.Parallel()
	r := newWriteRound(t, agents().withPeople("jane"))
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	forAda := seat("pm", "run-1")
	forAda.WrittenFor = types.ForTurn(ada)

	room := r.room(forAda, "launch", "pm", "eng", "jane")
	if got := r.lastRecord(); got.WorkingFor != nil || got.V != 1 {
		t.Errorf("the room ada's turn made is recorded written for %+v at version %d, want "+
			"nobody at 1: a room wakes nobody", got.WorkingFor, got.V)
	}

	ask := chatlet.NewMessage{Body: "@eng is the deploy done?", Mentions: []string{"eng"}, Asks: true}
	if _, err := r.store.Post(t.Context(), keyed(forAda, requested("k-for-ada")), room.Channel.ID,
		ask); err != nil {
		t.Fatalf("pm asks for ada: %v", err)
	}
	r.drain()
	rec := r.lastRecord()
	if !reflect.DeepEqual(rec.WorkingFor, ada) || rec.V != 3 || rec.Notify == nil {
		t.Fatalf("pm's ask for ada is recorded written for %+v at version %d waking %v, want ada "+
			"at 3", rec.WorkingFor, rec.V, rec.Notify)
	}
	routed := wakes(t, rec, company(t, []string{"pm", "eng", "jane"}, "jane"))
	want := types.NotificationWriter{Kind: iam.ActorAgent, Seat: "pm", WorkingFor: ada}
	if len(routed) != 1 || routed[0].Inbound.Writer == nil || !reflect.DeepEqual(*routed[0].Inbound.Writer, want) {
		t.Fatalf("the ask woke %+v, want eng woken by %+v", routed, want)
	}

	if _, err := r.store.Post(t.Context(), keyed(seat("pm", "run-2"), requested("k-own")), room.Channel.ID,
		ask); err != nil {
		t.Fatalf("pm asks in a turn nobody woke: %v", err)
	}
	r.drain()
	// VERSION 2, THE ASK'S OWN (MessagePost.Asks), and never the carrier's.
	if got := r.lastRecord(); got.WorkingFor != nil || got.V != 2 {
		t.Errorf("the ask of a turn nobody woke is recorded written for %+v at version %d, want "+
			"nobody at 2", got.WorkingFor, got.V)
	}

	unstated := seat("pm", "run-3")
	unstated.WrittenFor = types.WrittenFor{}
	handed := person("jane")
	handed.WrittenFor = types.ForTurn(ada)
	for name, c := range map[string]struct {
		actor chatlet.Actor
		cause error
	}{
		"an agent that states nobody": {unstated, types.ErrWrittenForUnstated},
		"a person handed a carrier":   {handed, types.ErrWrittenForNotAnAgents},
	} {
		if _, err := r.store.Post(t.Context(), keyed(c.actor, requested("k-"+name)), room.Channel.ID,
			ask); !errors.Is(err, c.cause) {
			t.Errorf("%s posts: %v, want refused for %v", name, err, c.cause)
		}
	}

	// AN ACTOR THAT CROSSED THE WIRE carrying nobody is stated: the node
	// that built it decided, and an older node sends exactly this.
	var crossed chatlet.Actor
	if err := json.Unmarshal([]byte(`{"Handle":"pm","Kind":"agent"}`), &crossed); err != nil {
		t.Fatalf("decode an actor: %v", err)
	}
	if _, err := r.store.Post(t.Context(), keyed(crossed, requested("k-crossed")), room.Channel.ID,
		ask); err != nil {
		t.Errorf("an agent's message that crossed the wire carrying nobody was refused: %v", err)
	}
}

// A CHAT ACTOR CROSSES THE WIRE AS IT WAS STATED (ADR-0109), under the rule of
// the arguments it crosses in, as the estate's wire encodes and decodes them
// (crewlet#910). Through the caller's own encoder every field at its zero is
// left off, so an agent's actor stated for nobody goes out without the key,
// byte for byte as a build without the field sends it, and one stated for
// somebody carries them. Through the caller's own decoder it arrives stated,
// whatever members did, so an older node's actor, which knows no carrier,
// arrives as stated nobody; and a member nothing declares, in the actor or in
// its carrier, is refused where it is rather than dropped below where that
// rule can see. (That no node asks a chat write whose actor may write nothing
// is internal/estate's, and held there.)
//
// Mutation: give the actor an UnmarshalJSON over encoding/json in place of
// its UnmarshalJSONFrom, and the member inside it is dropped; state the actor
// only where a carrier arrives, and the older node's actor arrives stating
// nothing.
func TestAChatActorCrossesTheWireAsItWasStated(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	wire := jsonv2.OmitZeroStructFields(true)
	for name, c := range map[string]struct {
		writtenFor types.WrittenFor
		want       string
	}{
		"an agent stated for nobody": {types.ForTurn(nil), `{"Handle":"pm","Kind":"agent"}`},
		"an agent stated for ada": {types.ForTurn(ada),
			`{"Handle":"pm","Kind":"agent","WrittenFor":{"kind":"seat","seat":"ada"}}`},
	} {
		got, err := jsonv2.Marshal(chatlet.Actor{Handle: "pm", Kind: chatlet.AuthorAgent,
			WrittenFor: c.writtenFor}, wire)
		var back chatlet.Actor
		if err == nil {
			err = jsonv2.Unmarshal(got, &back, jsonv2.RejectUnknownMembers(true))
		}
		if err != nil || string(got) != c.want || !back.WrittenFor.Stated() ||
			!reflect.DeepEqual(back.WrittenFor.Carrier(), c.writtenFor.Carrier()) {
			t.Errorf("%s goes out as %s and arrives written for %+v, stated %v (%v), want %s, stated",
				name, got, back.WrittenFor.Carrier(), back.WrittenFor.Stated(), err, c.want)
		}
	}

	var older chatlet.Actor
	if err := jsonv2.Unmarshal([]byte(`{"Handle":"pm","Kind":"agent"}`), &older,
		jsonv2.RejectUnknownMembers(true)); err != nil || !older.WrittenFor.Stated() ||
		older.WrittenFor.Carrier() != nil {
		t.Errorf("an older node's actor arrives written for %+v, stated %v (%v), want nobody, stated",
			older.WrittenFor.Carrier(), older.WrittenFor.Stated(), err)
	}

	for _, c := range []struct{ raw, at string }{
		{`{"Actor":{"Handle":"pm","Kind":"agent","Later":1}}`, "/Actor/Later"},
		{`{"Actor":{"Handle":"pm","Kind":"agent","WrittenFor":{"kind":"seat","seat":"ada","later":1}}}`,
			"/Actor/WrittenFor/later"},
	} {
		var args struct{ Actor chatlet.Actor }
		err := jsonv2.Unmarshal([]byte(c.raw), &args, jsonv2.RejectUnknownMembers(true))
		var semantic *jsonv2.SemanticError
		if !errors.As(err, &semantic) || !errors.Is(err, jsonv2.ErrUnknownName) ||
			string(semantic.JSONPointer) != c.at {
			t.Errorf("%s decoded as %v, want it refused at %s", c.raw, err, c.at)
		}
	}
}

// A POST KEEPS WHOM AN AGENT'S MESSAGE WAS WRITTEN FOR (ADR-0109), on its
// document and the same on every node: the carrier a version-3 record rode,
// and nobody for a record that carries none. Kept from the carrier's first
// build on, before anything reads it, because the applier writes a message's
// document once, at its post's position: a field a later build began to fill
// would differ between a node that applied the post before it upgraded and
// one that applied it after.
//
// Mutation: drop the fill, and the post made for ada keeps nobody on both
// nodes; fill a carrier into a post whose record carries none, and that post
// keeps one.
func TestAPostKeepsWhomAnAgentsMessageWasWrittenFor(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	for name, writtenFor := range map[string]*types.WorkingFor{"made for ada": ada, "made for nobody": nil} {
		documents := map[string]string{}
		for _, node := range []string{"node-a", "node-b"} {
			h := newApplyHarness(t)
			if _, _, err := h.apply(applyCreateRoom("room-1", "shipping")); err != nil {
				t.Fatalf("%s on %s: create the room: %v", name, node, err)
			}
			post := applyRecord(chatlet.MessageSubject("room-1"), chatlet.OpPost, "op-post-m-1",
				chatlet.MessagePost{V: chatlet.DocumentVersion, MessageID: "m-1", Body: "is this current?",
					Author: "dev", AuthorKind: chatlet.AuthorAgent}, chatlet.ScopeSet{Subject: true})
			post.V, post.Actor, post.ActorKind = chatlet.WorkingForVersion, "dev", chatlet.AuthorAgent
			post.WorkingFor = writtenFor
			if _, _, err := h.apply(post); err != nil {
				t.Fatalf("%s on %s: apply the post: %v", name, node, err)
			}
			document := h.scalar(`SELECT document FROM chat_messages WHERE id = ?`, "m-1")
			message, err := chatlet.DecodeMessage([]byte(document))
			if err != nil {
				t.Fatalf("%s on %s: decode the message: %v", name, node, err)
			}
			if !reflect.DeepEqual(message.WorkingFor, writtenFor) {
				t.Errorf("the post %s keeps written for %+v on %s, want %+v", name, message.WorkingFor, node,
					writtenFor)
			}
			documents[node] = document
		}
		if documents["node-a"] != documents["node-b"] {
			t.Errorf("the post %s is two documents on two nodes:\n%s\n%s", name, documents["node-a"],
				documents["node-b"])
		}
	}
}
