package jetstreamtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A LEAF OF A FLEET READS A DEAD LETTER AT THE KEEP'S CEILING WHOLE, AND SEES
// ITS DROP AT ONCE.
//
// On a fleet the dead letters keep a copy on each of three members. A leaf
// reading an entry too large for the stream leader's reply to cross its link
// reads it through a consumer instead, which the broker places on one of the
// three copies at random — and a copy behind the leader still holds an entry
// a peer has just dropped, or has not heard of one just kept. So the read
// keeps only a consumer placed on the leader's server, and makes it again
// elsewhere: here a leaf of three members reads each of two entries at the
// ceiling whole, a member drops it, and the leaf reads it at once as gone —
// the leader's answer, which acknowledged the drop.
//
// The placement the read insists on is held deterministically where it is
// decided (internal/queue/jetstream's TestAReadKeepsOnlyAConsumerTheStreamsLeaderRuns,
// TestAReadKeepsTheConsumerItsBrokerPlacedOnTheLeader and
// TestAReadNoConsumerOfWhichIsPlacedOnTheLeaderFailsAfterItsPlacements); this
// case holds the whole read against a real fleet, whose placements and create
// answers that decision reads.
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
func listedWhole(ctx context.Context, t *testing.T, q *js.Queue, subject string, want int) []queue.Kept {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		kept, err := q.DeadLettersOn(ctx, subject)
		if err != nil {
			t.Fatalf("DeadLettersOn: %v", err)
		}
		if len(kept) == want {
			return kept
		}
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatalf("the subject lists %d entries, want the %d kept", len(kept), want)
		}
		time.Sleep(20 * time.Millisecond) //fixedwait:liveness
	}
}
