package jetstreamtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A LEAF OF A FLEET READS A DEAD LETTER AT THE KEEP'S CEILING WHOLE, AND SEES
// ITS DROP AT ONCE.
//
// A leaf reading an entry too large for the leader's reply to cross its link
// reads through a consumer the broker places on a random copy, and a copy
// behind the leader still holds an entry a peer just dropped. So the read keeps
// only a consumer on the leader's server, retrying elsewhere. The placement
// rule is held where decided (internal/queue/jetstream's
// TestAReadKeepsOnlyAConsumerTheStreamsLeaderRuns); this holds the whole read.
//
// Mutation: read the frame across the leaf's link with the leader's message
// read, and the leaf's server closes its link on the reply: the read fails.
func TestALeafOfAFleetReadsADeadLetterAtTheCeilingWholeAndSeesItsDropAtOnce(t *testing.T) {
	t.Parallel()
	c := startCluster(t, 3, js.Config{Replicas: 3}, true)
	leaf, err := js.StartServer(t.Context(), js.Config{ServerName: "leaf", LeafURLs: c.LeafURLs(),
		Replicas: 3, FleetAuth: fleetAuth()})
	if err != nil {
		t.Fatalf("start a leaf of the fleet: %v", err)
	}
	t.Cleanup(leaf.Shutdown)
	q, err := leaf.Client(t.Context())
	if err != nil {
		t.Fatalf("a client of the leaf: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	member := c.Client(t, 0)

	ctx := t.Context()
	subject := topics.DeadLetter("fleet.letters", "grp")
	header := []byte(`{"v":1}`)
	var bodies [][]byte
	for i := range 2 {
		body := make([]byte, queue.MaxPayloadBytes-queue.LetterFraming-len(header))
		if _, err := rand.Read(body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		if _, err := q.KeepDeadLetter(ctx, subject, string(rune('a'+i)), header, body); err != nil {
			t.Fatalf("keep a dead letter at the ceiling through the leaf: %v", err)
		}
	}
	kept := listedWhole(ctx, t, member, subject, len(bodies))

	for i, k := range kept {
		frame, held, err := q.DeadLetterFrame(ctx, subject, k.Seq)
		if err != nil || !held || !bytes.Equal(frame, bodies[i]) {
			t.Fatalf("entry %d through the leaf = %d bytes, held %t, %v: want its %d-byte body",
				i, len(frame), held, err, len(bodies[i]))
		}
		if err := member.DropDeadLetter(ctx, subject, k.Seq); err != nil {
			t.Fatalf("a member's drop of entry %d: %v", i, err)
		}
		if frame, held, err := q.DeadLetterFrame(ctx, subject, k.Seq); err != nil || held || frame != nil {
			t.Fatalf("entry %d through the leaf, after a member's drop returned = %d bytes, held "+
				"%t, %v: want it gone, as the leader that acknowledged the drop says", i,
				len(frame), held, err)
		}
	}
}

// listedWhole is subject's listing once it holds want entries, read through q:
// a listing reads whichever copy the broker places its consumer on, and a copy
// may be a moment behind the keeps.
//
// AND A FLEET STILL SETTLING ITS ROUTES may answer the read with no responders,
// which the queue reports as the broker unavailable: what every reader of the
// dead letters asks again after, so this one does too, until its deadline. Any
// other error fails the case at once.
func listedWhole(ctx context.Context, t *testing.T, q *js.Queue, subject string, want int) []queue.Kept {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		kept, err := q.DeadLettersOn(ctx, subject)
		switch {
		case errors.Is(err, queue.ErrUnavailable):
		case err != nil:
			t.Fatalf("DeadLettersOn: %v", err)
		case len(kept) == want:
			return kept
		}
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatalf("the subject lists %d entries (%v), want the %d kept", len(kept), err, want)
		}
		time.Sleep(20 * time.Millisecond) //fixedwait:liveness
	}
}
