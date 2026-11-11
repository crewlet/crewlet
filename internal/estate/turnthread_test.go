package estate

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/chatlet"
)

// A TURN'S THREAD READ THAT NO DATA NODE SERVES FALLS BACK TO THE PLAIN THREAD,
// AND SAYS SO. A data node of an older build does not declare the turn's own
// read and answers it unserved, so the facade reads the plain thread, which
// carries nothing (Carried false): the turn then marks no waiting message as
// worked through, the safe direction. One read runs, never two. And it falls
// back on nothing else: a walk in which a node went silent, and a node refusing
// keys its build does not declare, are returned as they are, with no second
// walk.
//
// Mutation: fall back on any error, or on a refusal of undeclared keys, or
// answer the fallback's thread as carried, and a case goes red.
func TestATurnThreadNoDataNodeServesFallsBackToThePlainThreadAndSaysSo(t *testing.T) {
	t.Parallel()
	q := chatlet.ThreadQuery{ChannelID: "room-1", RootID: "m-1"}
	requests := func(f *fleet) int {
		total := 0
		for _, node := range f.nodes {
			node.mu.Lock()
			total += node.requests
			node.mu.Unlock()
		}
		return total
	}

	t.Run("data nodes that do not declare the turn's read", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, "data-a", "data-b")
		desks := withRooms(f)
		for _, node := range f.nodes {
			node.set(func(n *fakeNode) {
				n.olderBuild = func(r request) request {
					// AN OPERATION THE OLDER BUILD DOES NOT DECLARE: its
					// server looks the name up and finds nothing.
					if r.Op == opChatTurnThread.spec.name {
						r.Op += ".undeclared"
					}
					return r
				}
			})
		}
		got, err := f.client.TurnThreads().TurnThread(t.Context(), "jane", q, linear)
		if err != nil {
			t.Fatalf("TurnThread: %v, want the plain thread", err)
		}
		if got.Carried || len(got.WorkingFor) != 0 || got.Thread.Root.Message.ID != "m-1" {
			t.Fatalf("the fallback answered %+v, want the plain thread, carrying nothing", got)
		}
		var reads []string
		for _, desk := range desks {
			reads = append(reads, desk.asked()...)
		}
		if !slices.Equal(reads, []string{"thread jane"}) {
			t.Errorf("the nodes ran %q, want the plain thread read once", reads)
		}
	})

	t.Run("a refusal of keys a build does not declare", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, "data-a", "data-b")
		withRooms(f)
		for _, node := range f.nodes {
			node.set(func(n *fakeNode) { n.undeclared = []string{"args.Viewer"} })
		}
		_, err := f.client.TurnThreads().TurnThread(t.Context(), "jane", q, linear)
		var refused *ErrUnknownKeys
		if !errors.As(err, &refused) {
			t.Fatalf("err = %v, want the refusal of the undeclared key", err)
		}
		if got := requests(f); got != len(f.nodes) {
			t.Errorf("the nodes were asked %d requests, want %d: one walk, and no second read "+
				"after a refusal that is not an estate nobody served", got, len(f.nodes))
		}
	})

	t.Run("a node that went silent", func(t *testing.T) {
		t.Parallel()
		f := newFleet(t, "data-a")
		withRooms(f)
		f.nodes["data-a"].set(func(n *fakeNode) { n.silent = true })
		if _, err := f.client.TurnThreads().TurnThread(t.Context(), "jane", q, linear); err == nil {
			t.Fatal("a read no node answered was answered")
		}
		if got := requests(f); got != 1 {
			t.Errorf("the node was asked %d requests, want 1: a silent node may have run the read, "+
				"so nothing is asked again", got)
		}
	})
}

// ONLY AN ESTATE NOBODY SERVED, IN WHICH NOBODY WENT SILENT, IS ONE THE TURN'S
// READ FALLS BACK FROM. A refusal that is no such thing — a viewer who may not
// read the room, a thread that is not there — is the answer, and so is a
// refusal of keys a build does not declare, though it unwraps to the walk it
// found unserved.
//
// Mutation: fall back on any error, or on a refusal of undeclared keys, and a
// row goes red.
func TestOnlyAnEstateNobodyServedIsOneATurnsReadFallsBackFrom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"an estate nobody served", &ErrUnserved{Detail: "data-a does not know the operation"}, true},
		{"an estate nobody served, wrapped", fmt.Errorf("estate: chat.turn_thread: %w",
			&ErrUnserved{Detail: "data-a is behind"}), true},
		{"a walk in which a node went silent", &ErrUnserved{NodeSilent: true}, false},
		{"a refusal of keys a build does not declare", &ErrUnknownKeys{Op: "chat.turn_thread",
			Keys: []string{"args.Viewer"}, Unserved: &ErrUnserved{}}, false},
		{"a thread that is not there", fmt.Errorf("%w: there's no such thread in this room",
			chatlet.ErrNotFound), false},
	} {
		if got := servedByNobody(tc.err); got != tc.want {
			t.Errorf("%s: falls back = %v, want %v", tc.name, got, tc.want)
		}
	}
}
