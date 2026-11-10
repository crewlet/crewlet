package estate

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A TRACKER ACTOR THAT CANNOT WRITE IS NEVER SENT (ADR-0109). The actor a
// tracker write crosses in carries Worklet's provenance beside its kind, and
// the node that built it refuses to encode an agent's actor that states nobody
// it writes for, and a person's handed a carrier: the data node it would cross
// to reads an absent carrier as nobody, so the turn the write woke would work
// for the writing seat, every app. An agent's actor stated for nobody goes out
// without the key, byte for byte as a build without the field sends it, and
// one stated for somebody carries them.
//
// Mutation: drop the check in Actor.MarshalJSON, and the agent's actor that
// states nothing is encoded.
func TestATrackerActorThatCannotWriteIsNeverSent(t *testing.T) {
	t.Parallel()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	for name, c := range map[string]struct {
		actor Actor
		cause error
	}{
		"an agent that states nothing": {Actor{Handle: "swe", Kind: worklet.AuthorAgent},
			types.ErrWrittenForUnstated},
		"a person handed a carrier": {Actor{Handle: "jane", Kind: worklet.AuthorHuman,
			Provenance: worklet.Provenance{WrittenFor: types.ForTurn(ada)}}, types.ErrWrittenForNotAnAgents},
	} {
		if _, err := json.Marshal(c.actor); !errors.Is(err, c.cause) {
			t.Errorf("%s is encoded (%v), want it refused for %v", name, err, c.cause)
		}
		// AND AS THE ROUTER ENCODES A REQUEST'S ACTOR, through the wire's
		// own input encoder.
		if _, err := encodeInput(c.actor); !errors.Is(err, c.cause) {
			t.Errorf("the router encodes %s (%v), want it refused for %v", name, err, c.cause)
		}
	}

	stated := Actor{Handle: "swe", Kind: worklet.AuthorAgent,
		Provenance: worklet.Provenance{WrittenFor: types.ForTurn(nil)}}
	got, err := json.Marshal(stated)
	want := `{"handle":"swe","kind":"agent","provenance":{"OperatorID":"","TurnID":"","Chain":null,"Origin":null}}`
	if err != nil || string(got) != want {
		t.Errorf("an agent stated for nobody goes out as %s (%v), want %s", got, err, want)
	}
	stated.Provenance.WrittenFor = types.ForTurn(ada)
	got, err = json.Marshal(stated)
	var back Actor
	if err == nil {
		err = json.Unmarshal(got, &back)
	}
	if err != nil || !reflect.DeepEqual(back.Provenance.WrittenFor.Carrier(), ada) {
		t.Errorf("an agent stated for ada goes out as %s and arrives written for %+v (%v), want ada",
			got, back.Provenance.WrittenFor.Carrier(), err)
	}
}

// A WRITE WHOSE ACTOR CANNOT WRITE IS REFUSED BEFORE ANY PEER RUNS IT
// (ADR-0109). This router holds no data, so every write it makes is decided on
// another data node, which reads an absent carrier as nobody and would run an
// agent's write that states nothing as the writing seat's own work. So the
// node that built the actor refuses to send it, for the tracker, the knowledge
// base and chat alike, and the data node runs nothing; the same writes with
// their actors stated run there.
//
// Mutation: drop the check from any of the three actors' MarshalJSON, and that
// family's write runs on the data node.
func TestAWriteWhoseActorCannotWriteIsRefusedBeforeAnyPeerRunsIt(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	node, desk := f.nodes["data-a"], withRooms(f)["data-a"]
	ctx := t.Context()
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	writes := func(tracker Actor, page doclet.Actor, room chatlet.Actor) map[string]error {
		_, trackerErr := f.client.WriterAs(tracker).CreateTask(ctx, "op-"+tracker.Handle,
			worklet.Task{Project: "ENG"}, nil)
		_, _, pageErr := f.client.Pages().Comment(ctx, page, "p1", doclet.NewComment{Body: "is this current?"})
		_, roomErr := f.client.Chat().Post(ctx, room, "room-1", chatlet.NewMessage{Body: "is this current?"})
		return map[string]error{"tracker": trackerErr, "pages": pageErr, "chat": roomErr}
	}

	refused := writes(Actor{Handle: "swe", Kind: worklet.AuthorAgent},
		doclet.Actor{Handle: "swe", Kind: doclet.AuthorAgent},
		chatlet.Actor{Handle: "jane", Kind: chatlet.AuthorAgent})
	for family, err := range refused {
		if !errors.Is(err, types.ErrWrittenForUnstated) {
			t.Errorf("the %s write of an agent that states nothing answered %v, want it refused "+
				"for stating nobody", family, err)
		}
	}
	if _, _, err := f.client.Pages().Comment(ctx, doclet.Actor{Handle: "jane", Kind: doclet.AuthorHuman,
		WrittenFor: types.ForTurn(ada)}, "p1", doclet.NewComment{Body: "for ada"}); !errors.Is(err,
		types.ErrWrittenForNotAnAgents) {
		t.Errorf("a person's page write handed a carrier answered %v, want it refused", err)
	}
	if node.askedFor("create") || node.askedFor("comment") || len(desk.asked()) != 0 {
		t.Fatalf("the data node ran writes no actor may make: tracker %v, pages %v, chat %v",
			node.askedFor("create"), node.askedFor("comment"), desk.asked())
	}

	// CONTROL: the same writes, their actors stated, run on the data node.
	ran := writes(Actor{Handle: "swe", Kind: worklet.AuthorAgent,
		Provenance: worklet.Provenance{WrittenFor: types.ForTurn(ada)}},
		doclet.Actor{Handle: "swe", Kind: doclet.AuthorAgent, WrittenFor: types.ForTurn(nil)},
		chatlet.Actor{Handle: "jane", Kind: chatlet.AuthorAgent, WrittenFor: types.ForTurn(ada)})
	for family, err := range ran {
		if err != nil {
			t.Errorf("the %s write of a stated agent answered %v, want it run", family, err)
		}
	}
	if !node.askedFor("create") || !node.askedFor("comment") || !slices.Contains(desk.asked(), "post jane") {
		t.Errorf("the data node did not run the stated writes: tracker %v, pages %v, chat %v",
			node.askedFor("create"), node.askedFor("comment"), desk.asked())
	}
}
