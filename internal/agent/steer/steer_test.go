package steer_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/steer"
)

func note(id string) steer.Note { return steer.Note{ID: id, Text: "use staging", By: "ops"} }

// THE TRANSITION TABLE: every state a box can be in, and what an offer is
// answered there. Each answer is a different fact a person acts on, so a box
// that answered `full` where it meant `closed` would tell somebody to wait for
// a turn that has already ended.
func TestTheSteerTransitionTable(t *testing.T) {
	t.Parallel()
	full := func() *steer.Box {
		b := steer.New()
		for i := range steer.MaxPendingNotes {
			if got := b.Offer(note(string(rune('a' + i)))); got != steer.StatusAccepted {
				t.Fatalf("filling: offer %d answered %s", i, got)
			}
		}
		return b
	}
	for _, tc := range []struct {
		name string
		box  func() *steer.Box
		want steer.Status
	}{
		{"an open box takes a note", steer.New, steer.StatusAccepted},
		{"a box at its bound refuses the next", full, steer.StatusFull},
		{"a drained box takes notes again", func() *steer.Box {
			b := full()
			b.Drain()
			return b
		}, steer.StatusAccepted},
		{"a closed box takes nothing", func() *steer.Box {
			b := steer.New()
			b.Close()
			return b
		}, steer.StatusClosed},
		{"an unsupported runtime takes nothing", steer.Unsupported, steer.StatusUnsupported},
		{"a closed unsupported box says closed", func() *steer.Box {
			b := steer.Unsupported()
			b.Close()
			return b
		}, steer.StatusClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.box().Offer(note("new")); got != tc.want {
				t.Errorf("offer answered %s, want %s", got, tc.want)
			}
		})
	}
}

// A RETRIED NOTE IS ONE NOTE. The id is the person's request id, so a retry
// after an answer lost in transit carries the same one — and a turn that read
// the same instruction twice would act on it twice. The retry is answered
// `accepted` in every later state, the box's end included: the first answer
// was the true one about that note.
func TestARetriedNoteIsOneNote(t *testing.T) {
	t.Parallel()
	b := steer.New()
	for range 3 {
		if got := b.Offer(note("req-1")); got != steer.StatusAccepted {
			t.Fatalf("a retry answered %s", got)
		}
	}
	if got := b.Drain(); len(got) != 1 {
		t.Fatalf("drained %d notes for one request retried three times", len(got))
	}
	if got := b.Offer(note("req-1")); got != steer.StatusAccepted {
		t.Errorf("a retry after the turn read the note answered %s", got)
	}
	if got := b.Drain(); len(got) != 0 {
		t.Errorf("a retry after the turn read the note was read again: %+v", got)
	}
	b.Close()
	if got := b.Offer(note("req-1")); got != steer.StatusAccepted {
		t.Errorf("a retry after the turn ended answered %s, not the note's first answer", got)
	}
}

// CLOSING RETURNS WHAT NOBODY READ, ONCE. Those notes expired, and a caller
// that closes on two paths must not report them twice.
func TestCloseReturnsTheUnreadNotesOnce(t *testing.T) {
	t.Parallel()
	b := steer.New()
	b.Offer(note("read"))
	b.Drain()
	b.Offer(note("missed"))
	expired := b.Close()
	if len(expired) != 1 || expired[0].ID != "missed" {
		t.Fatalf("close returned %+v, want only the note nobody read", expired)
	}
	if again := b.Close(); again != nil {
		t.Errorf("a second close returned %+v", again)
	}
	if got := b.Drain(); got != nil {
		t.Errorf("a closed box drained %+v", got)
	}
}

func TestANoteIsBounded(t *testing.T) {
	t.Parallel()
	if err := steer.Validate("  "); err == nil {
		t.Error("an empty note is valid")
	}
	if err := steer.Validate(strings.Repeat("é", steer.MaxNoteRunes)); err != nil {
		t.Errorf("a note at the bound, in multi-byte runes, is refused: %v", err)
	}
	var long *steer.TooLongError
	if err := steer.Validate(strings.Repeat("a", steer.MaxNoteRunes+1)); !errors.As(err, &long) {
		t.Errorf("a note past the bound is %v", err)
	}
}

func TestStatusesAreClosed(t *testing.T) {
	t.Parallel()
	for _, s := range steer.Statuses {
		if !s.Valid() {
			t.Errorf("%s is not valid", s)
		}
	}
	if steer.Status("maybe").Valid() {
		t.Error("an unknown status off the wire is valid")
	}
}
