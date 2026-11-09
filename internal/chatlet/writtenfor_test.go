package chatlet_test

import (
	"encoding/json"
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

// A CHAT ACTOR THAT CANNOT WRITE IS NEVER SENT (ADR-0109). The node that built
// it refuses to encode an agent's actor that states nobody it writes for, and
// a person's handed a carrier, alone or inside the arguments a chat gesture
// crosses in: the data node it would cross to reads an absent carrier as
// nobody, so the turn the message woke would work for the writing seat, every
// app. An agent's actor stated for nobody goes out without the key, byte for
// byte as a build without the field sends it, and one stated for somebody
// carries them.
//
// Mutation: drop the check in Actor.MarshalJSON, and the agent's actor that
// states nothing is encoded.
func TestAChatActorThatCannotWriteIsNeverSent(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	unstated := seat("pm", "run-1")
	unstated.WrittenFor = types.WrittenFor{}
	handed := person("jane")
	handed.WrittenFor = types.ForTurn(ada)
	for name, c := range map[string]struct {
		actor chatlet.Actor
		cause error
	}{
		"an agent that states nothing": {unstated, types.ErrWrittenForUnstated},
		"a person handed a carrier":    {handed, types.ErrWrittenForNotAnAgents},
	} {
		if _, err := json.Marshal(c.actor); !errors.Is(err, c.cause) {
			t.Errorf("%s is encoded (%v), want it refused for %v", name, err, c.cause)
		}
		if _, err := json.Marshal(struct{ Actor chatlet.Actor }{c.actor}); !errors.Is(err, c.cause) {
			t.Errorf("arguments carrying %s are encoded (%v), want them refused for %v", name, err, c.cause)
		}
	}

	stated := chatlet.Actor{Handle: "pm", Kind: chatlet.AuthorAgent, WrittenFor: types.ForTurn(nil)}
	got, err := json.Marshal(stated)
	want := `{"Handle":"pm","Kind":"agent","OperatorID":"","TurnID":"","Chain":null,"OpKey":""}`
	if err != nil || string(got) != want {
		t.Errorf("an agent stated for nobody goes out as %s (%v), want %s", got, err, want)
	}
	stated.WrittenFor = types.ForTurn(ada)
	got, err = json.Marshal(stated)
	var back chatlet.Actor
	if err == nil {
		err = json.Unmarshal(got, &back)
	}
	if err != nil || !reflect.DeepEqual(back.WrittenFor.Carrier(), ada) {
		t.Errorf("an agent stated for ada goes out as %s and arrives written for %+v (%v), want ada",
			got, back.WrittenFor.Carrier(), err)
	}
}
