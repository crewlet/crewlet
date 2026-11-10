package worklet_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN AGENT'S WRITE THAT WAKES SOMEBODY CARRIES WHOM IT WAS WRITTEN FOR, AND
// NOTHING ELSE DOES (ADR-0109). A task an agent files in a turn ada woke
// records, beside the agent, that the turn worked for ada, at the version that
// carries it, which an older build retains rather than waking its assignee for
// the agent. The same agent's task that wakes nobody carries nobody, and so
// does a task a turn nobody woke files: both stay at the base version every
// build reads.
//
// AND THE TYPE REFUSES THE ACTOR THAT FORGOT: an agent's writer that states
// nobody it writes for writes nothing, and a person's writer handed a carrier
// writes nothing either, since a person writes for nobody but themselves. An
// actor decoded off the estate's wire states it already, whatever its
// provenance held, so a data node never refuses a write for a key an older
// node never sends: that is the actor's own decoder's, held by
// internal/estate's TestATrackerActorCrossesTheWireAsItWasStated and
// TestAnAgentsWriteFromABuildWithoutTheCarrierRunsAsItsSeatsOwn, and on this
// writer behind a data node by internal/engine's
// TestAnAgentsTrackerWriteFromABuildWithoutTheCarrierRunsForNobody.
//
// Mutation: leave the carrier off the record the writer builds — ada's task
// carries nobody; keep it on a record that wakes nobody — the quiet task
// carries ada; drop the refusal in recordedFor — the unstated agent's task
// lands.
func TestAnAgentsWriteThatWakesSomebodyCarriesWhomItWasWrittenFor(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ada := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	for _, c := range []struct {
		id         string
		kind       worklet.AuthorKind
		writtenFor types.WrittenFor
		woken      bool
		want       *types.WorkingFor
		version    int
	}{
		{"t-for-ada", worklet.AuthorAgent, types.ForTurn(ada), true, ada, 12},
		{"t-quiet-for-ada", worklet.AuthorAgent, types.ForTurn(ada), false, nil, 1},
		{"t-by-a-turn-nobody-woke", worklet.AuthorAgent, types.ForTurn(nil), true, nil, 1},
	} {
		w := r.writer.As("dev", c.kind, worklet.Provenance{WrittenFor: c.writtenFor})
		task := newTask(c.id)
		task.Key, task.Reporter = "", "dev"
		var wake *worklet.Notify
		if c.woken {
			wake = worklet.Wake{Kind: worklet.ChangeCreated, After: task}.Notify(nil)
		}
		if _, err := w.CreateTask(t.Context(), "op-"+c.id, task, wake); err != nil {
			t.Fatalf("dev files %s: %v", c.id, err)
		}
		r.drain()
		record, err := worklet.Decode(r.lastRecord(t))
		if err != nil {
			t.Fatalf("decode %s: %v", c.id, err)
		}
		if !reflect.DeepEqual(record.WorkingFor, c.want) || record.V != c.version {
			t.Errorf("%s is recorded written for %+v at version %d, want %+v at %d",
				c.id, record.WorkingFor, record.V, c.want, c.version)
		}
	}

	for _, c := range []struct {
		name       string
		kind       worklet.AuthorKind
		writtenFor types.WrittenFor
		cause      error
	}{
		{"an agent that states nobody", worklet.AuthorAgent, types.WrittenFor{}, types.ErrWrittenForUnstated},
		{"a person handed a carrier", worklet.AuthorHuman, types.ForTurn(ada), types.ErrWrittenForNotAnAgents},
	} {
		w := r.writer.As("dev", c.kind, worklet.Provenance{WrittenFor: c.writtenFor})
		task := newTask("t-refused")
		task.Key, task.Reporter = "", "dev"
		_, err := w.CreateTask(t.Context(), "op-refused-"+string(c.kind), task,
			worklet.Wake{Kind: worklet.ChangeCreated, After: task}.Notify(nil))
		if !errors.Is(err, c.cause) {
			t.Errorf("%s files a task: %v, want refused for %v", c.name, err, c.cause)
		}
	}
}
