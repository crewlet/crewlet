package a2a_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue"
)

// refusing is a node that cannot confirm the mailboxes of the seats it names.
type refusing struct {
	mu    sync.Mutex
	seats map[uuid.UUID]bool
}

func (r *refusing) refuse(handle string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seats[seatID(handle)] = true
}

func (r *refusing) ConfirmMailbox(_ context.Context, seat uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seats[seat] {
		return fmt.Errorf("the mailbox is not complete: %w", queue.ErrUnavailable)
	}
	return nil
}

func confirmingService(t *testing.T, seats dir, boxes *refusing) (*a2a.Service, a2a.Store, *recorder) {
	t.Helper()
	st := a2a.NewCoordStore(memory.NewFleet())
	rec := &recorder{}
	svc, err := a2a.New(st, rec, a2a.Options{
		Directory: seats, Mailboxes: boxes,
		Now:   func() time.Time { return clock },
		NewID: func() string { return "a2a-fixed" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, st, rec
}

// AN ASK TO A SEAT WHOSE MAILBOX IS NOT CONFIRMED RECORDS NOTHING. Asked after
// the channel was opened and announced, a refused wake left a channel open on
// a question its target was never woken for; asked first, the asker is told
// to ask again and the retry starts from nothing.
func TestAnAskToASeatThatCannotBeWokenRecordsNothing(t *testing.T) {
	t.Parallel()
	boxes := &refusing{seats: map[uuid.UUID]bool{}}
	boxes.refuse("bob")
	svc, st, rec := confirmingService(t, dir{"bob": true}, boxes)

	_, err := svc.Open(context.Background(), a2a.Ask{
		Requester: "alice", Target: "bob", Brief: "can you review this?",
	})
	if !errors.Is(err, queue.ErrUnavailable) {
		t.Fatalf("Open = %v, want an error that is queue.ErrUnavailable, so the asker retries", err)
	}
	if got := rec.topics(); len(got) != 0 {
		t.Fatalf("a refused ask published %v, want nothing", got)
	}
	if _, err := st.Get(context.Background(), "a2a-fixed"); err == nil {
		t.Fatal("a refused ask left a channel open for a question nobody was woken for")
	}
}

// AN ANSWER TO A SEAT WHOSE MAILBOX IS NOT CONFIRMED IS NOT COUNTED, so the
// answer sent again is the channel's one message rather than its second.
func TestAnAnswerToASeatThatCannotBeWokenIsNotCounted(t *testing.T) {
	t.Parallel()
	boxes := &refusing{seats: map[uuid.UUID]bool{}}
	svc, st, rec := confirmingService(t, dir{"alice": true, "bob": true}, boxes)
	id, err := svc.Open(context.Background(), a2a.Ask{
		Requester: "alice", Target: "bob", Brief: "can you review this?",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	before, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	boxes.refuse("alice")
	err = svc.Reply(context.Background(), a2a.Answer{
		ChannelID: id, Sender: "bob", Content: "looks good",
	})
	if !errors.Is(err, queue.ErrUnavailable) {
		t.Fatalf("Reply = %v, want an error that is queue.ErrUnavailable", err)
	}
	after, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Messages != before.Messages {
		t.Fatalf("a refused answer moved the channel from %d messages to %d", before.Messages,
			after.Messages)
	}
	if got := rec.onlyTo(inbox("alice")); len(got) != 0 {
		t.Fatalf("a refused answer woke the asker %d times", len(got))
	}
}
