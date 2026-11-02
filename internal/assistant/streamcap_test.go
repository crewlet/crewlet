package assistant_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// opened is a stream that says when its first frame arrived.
type opened struct {
	once sync.Once
	ch   chan struct{}
}

func newOpened() *opened { return &opened{ch: make(chan struct{})} }

func (o *opened) Frame(wire.Frame) error { o.once.Do(func() { close(o.ch) }); return nil }
func (o *opened) Keepalive() error       { return nil }

// A PERSON HOLDS AT MOST StreamsPerPerson STREAMS ON A NODE: past it a stream
// is refused assistant_busy with a Retry-After before anything is read, and a
// stream that closes gives its place back.
//
// Mutation: drop the cap from Stream and the fifth stream is served, so a
// script holds a goroutine and a frame buffer per request.
func TestAPersonsOpenStreamsAreCapped(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{{
		Text: "Done.", Input: 100, Output: 20, Hold: hold}}})
	defer close(hold)
	accepted, err := f.Ask("held", "Slow question", nil)
	if err != nil {
		t.Fatal(err)
	}
	type held struct {
		cancel context.CancelFunc
		done   chan error
	}
	open := func() held {
		ctx, cancel := context.WithCancel(context.Background())
		o, done := newOpened(), make(chan error, 1)
		go func() { done <- f.Svc.Stream(ctx, f.Person.ID, f.Conversation, accepted.TurnID, 0, o) }()
		select {
		case <-o.ch:
		case err := <-done:
			cancel()
			t.Fatalf("a stream inside the cap was refused: %v", err)
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("a stream inside the cap drew nothing")
		}
		return held{cancel: cancel, done: done}
	}
	streams := make([]held, 0, assistant.StreamsPerPerson)
	for range assistant.StreamsPerPerson {
		streams = append(streams, open())
	}
	defer func() {
		for _, s := range streams {
			s.cancel()
		}
	}()
	// Bounded, so a stream the cap fails to refuse ends the test rather than
	// following the held turn.
	over, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err = f.Svc.Stream(over, f.Person.ID, f.Conversation, accepted.TurnID, 0, newOpened())
	busy, ok := errorAs[*assistant.Busy](err)
	if !ok || busy.RetryAfter <= 0 {
		t.Fatalf("a stream past the cap answered %v, want Busy with a Retry-After", err)
	}
	streams[0].cancel()
	<-streams[0].done
	streams[0] = open()
}

// A PERSON'S STREAM OPENS ARE RATED: a loop reopening an ended turn's stream,
// which is answered by unsealing the transcript, is refused assistant_busy
// past StreamOpenBurst at once, and admitted again as the rate refills.
//
// Mutation: drop the open rate and every reopen unseals the transcript again.
func TestAPersonsStreamOpensAreRated(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{evals.Say("Done.")}})
	accepted, err := f.Ask("ended", "Quick question", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	for i := 1; i < assistant.StreamOpenBurst; i++ {
		if _, err := f.Await(t.Context(), accepted.TurnID, 0, 5*time.Second); err != nil {
			t.Fatalf("open %d inside the burst: %v", i+1, err)
		}
	}
	err = f.Svc.Stream(t.Context(), f.Person.ID, f.Conversation, accepted.TurnID, 0, newOpened())
	busy, ok := errorAs[*assistant.Busy](err)
	if !ok || busy.RetryAfter <= 0 {
		t.Fatalf("an open past the burst answered %v, want Busy with a Retry-After", err)
	}
	f.Clock.Advance(busy.RetryAfter)
	if _, err := f.Await(t.Context(), accepted.TurnID, 0, 5*time.Second); err != nil {
		t.Errorf("an open once the rate refilled: %v", err)
	}
}
