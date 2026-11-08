package queuetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// absentLetter is a sequence no case keeps an entry under: what a drop of an
// entry already gone names, and what the lifecycle probe drops.
const absentLetter = math.MaxUint64

// runDeadLetters certifies the dead-letter verbs: KeepDeadLetter, DeadLettersOn
// and DropDeadLetter.
//
// # Why they are certified at all
//
// Because the reader that keeps one acts on the listing's every answer. A wake
// feed dead-letters a record no node could read for an hour and acks the
// record only once the entry is kept (internal/changefeed): a keep that
// returned before the entry was durable, or a listing that missed it, is a
// wake lost with nothing reporting it. And the alarm reading counts an entry
// that VANISHED between two listings as let go by the retention, so a listing
// that lagged a keep, or still showed a drop, would be counted as an expiry
// that never happened — or hide one that did. Each case below is one way a
// plausible backend answers wrongly while every other case in this suite
// passes.
func (s *suite) runDeadLetters(t *testing.T) {
	ctx := t.Context()

	t.Run("a_kept_dead_letter_is_listed_verbatim_with_its_instants", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := topics.DeadLetter("changefeed.kept", "grp")
		// BYTES, NOT AN EVENT: a newline, a byte that is not UTF-8 and a
		// NUL, which a backend that re-encoded the body as text or as an
		// event would not hand back as they were.
		first, second := []byte("{\"v\":1}\n\x00\xff frame"), []byte("second")
		keep(ctx, t, q, subject, first)
		keep(ctx, t, q, subject, second)

		// READ ONCE, WITH NO WAIT: the contract promises an entry whose
		// keep returned is listed, and a reader that counts what
		// vanished acts on the first listing it takes.
		got := lettersOn(ctx, t, q, subject)
		if len(got) != 2 {
			t.Fatalf("the subject lists %d entries, want the two kept", len(got))
		}
		if !bytes.Equal(got[0].Body, first) || !bytes.Equal(got[1].Body, second) {
			t.Errorf("the subject lists %q and %q, want %q then %q verbatim, oldest first",
				got[0].Body, got[1].Body, first, second)
		}
		for i, k := range got {
			switch {
			case k.Seq == 0:
				t.Errorf("entry %d has no sequence, so no drop can name it", i)
			case k.StoredAt.IsZero():
				t.Errorf("entry %d carries no instant it was kept at", i)
			case !k.Expires.After(k.StoredAt):
				t.Errorf("entry %d expires at %v, not after it was kept at %v: a reader "+
					"could not say how long it has left", i, k.Expires, k.StoredAt)
			}
		}
		if got[1].Seq <= got[0].Seq {
			t.Errorf("the second entry's sequence %d is not after the first's %d",
				got[1].Seq, got[0].Seq)
		}
	})

	t.Run("a_listing_holds_its_own_subject_alone", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		mine, theirs := topics.DeadLetter("changefeed.mine", "grp"), topics.DeadLetter("changefeed.theirs", "grp")
		keep(ctx, t, q, mine, []byte("mine"))
		keep(ctx, t, q, theirs, []byte("theirs"))
		if got := lettersOn(ctx, t, q, mine); len(got) != 1 || string(got[0].Body) != "mine" {
			t.Errorf("one subject lists %v, want its own entry alone", bodiesOf(got))
		}
		if got := lettersOn(ctx, t, q, topics.DeadLetter("changefeed.nobody", "grp")); len(got) != 0 {
			t.Errorf("a subject nothing was kept on lists %v", bodiesOf(got))
		}
	})

	t.Run("a_dropped_dead_letter_is_gone_and_dropping_it_again_is_no_error", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := topics.DeadLetter("changefeed.dropped", "grp")
		keep(ctx, t, q, subject, []byte("doomed"))
		keep(ctx, t, q, subject, []byte("kept"))
		doomed := lettersOn(ctx, t, q, subject)[0]

		if err := q.DropDeadLetter(ctx, subject, doomed.Seq); err != nil {
			t.Fatalf("DropDeadLetter: %v", err)
		}
		if got := lettersOn(ctx, t, q, subject); len(got) != 1 || string(got[0].Body) != "kept" {
			t.Fatalf("after the drop the subject lists %v, want the other entry alone", bodiesOf(got))
		}
		// TWO NODES THAT BOTH FINISHED WITH ONE ENTRY BOTH SUCCEED: the
		// second drop finds what it asked for already true.
		if err := q.DropDeadLetter(ctx, subject, doomed.Seq); err != nil {
			t.Errorf("a second drop of the same entry answered %v, want nil: the entry is gone, "+
				"which is all a drop asks", err)
		}
		if err := q.DropDeadLetter(ctx, subject, absentLetter); err != nil {
			t.Errorf("a drop of an entry never kept answered %v, want nil", err)
		}
	})

	t.Run("a_drop_naming_another_subjects_entry_is_refused_and_removes_nothing", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		owner, other := topics.DeadLetter("changefeed.owner", "grp"), topics.DeadLetter("changefeed.other", "grp")
		keep(ctx, t, q, owner, []byte("owned"))
		entry := lettersOn(ctx, t, q, owner)[0]
		if err := q.DropDeadLetter(ctx, other, entry.Seq); err == nil {
			t.Error("a drop under another subject's name was accepted")
		}
		if err := q.DropDeadLetter(ctx, owner, 0); err == nil {
			t.Error("a drop of sequence 0, which names no entry, was accepted")
		}
		if got := lettersOn(ctx, t, q, owner); len(got) != 1 {
			t.Errorf("a refused drop removed the entry: the subject lists %v", bodiesOf(got))
		}
	})

	t.Run("a_dead_letter_is_the_brokers_and_outlives_the_client_that_kept_it", func(t *testing.T) {
		t.Parallel()
		peerOf := s.needPeer(t)
		q := s.start(ctx, t)
		peer := startQueue(ctx, t, peerOf(t, q))
		subject := topics.DeadLetter("changefeed.shared", "grp")
		keep(ctx, t, q, subject, []byte("kept by one node"))
		if err := q.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		// THE NODE THAT REPLAYS AN ENTRY IS RARELY THE ONE THAT KEPT IT:
		// a peer that cannot list it, or loses it with its keeper, would
		// leave a wake no newer node could ever derive.
		got := lettersOn(ctx, t, peer, subject)
		if len(got) != 1 || string(got[0].Body) != "kept by one node" {
			t.Fatalf("a peer lists %v after the keeper stopped, want its entry", bodiesOf(got))
		}
		if err := peer.DropDeadLetter(ctx, subject, got[0].Seq); err != nil {
			t.Fatalf("a peer's drop: %v", err)
		}
		if got := lettersOn(ctx, t, peer, subject); len(got) != 0 {
			t.Errorf("after a peer's drop the subject lists %v", bodiesOf(got))
		}
	})

	t.Run("a_subscriptions_retired_event_is_one_more_dead_letter", func(t *testing.T) {
		t.Parallel()
		// ONE PLACE, whoever gave up: an event its subscription's budget
		// retired is listed and dropped by the same verbs as a reader's
		// entry, so a reader of one subject never has to know which kind
		// it holds.
		newQueueWithAttempts := s.needAttempts(t)
		deadLetters := s.needDeadLetters(t)
		q := startQueue(ctx, t, newQueueWithAttempts(t, 1))
		subscribe(ctx, t, q, "retired.t", "grp", func(context.Context, *events.Event) queue.Result {
			return queue.Nak(errors.New("permanent failure"))
		})
		publish(ctx, t, q, "retired.t", newEvent("poison"))
		awaitState(t, "the event to be retired to its dead-letter subject", func() bool {
			return len(deadLetters(q, "retired.t", "grp")) == 1
		})

		subject := topics.DeadLetter("retired.t", "grp")
		got := lettersOn(ctx, t, q, subject)
		if len(got) != 1 {
			t.Fatalf("the retired event's subject lists %d entries, want 1", len(got))
		}
		var ev events.Event
		if err := json.Unmarshal(got[0].Body, &ev); err != nil || labelOf(&ev) != "poison" {
			t.Fatalf("the listed entry is %q (%v), want the retired event's own bytes", got[0].Body, err)
		}
		if err := q.DropDeadLetter(ctx, subject, got[0].Seq); err != nil {
			t.Fatalf("DropDeadLetter: %v", err)
		}
		if left := deadLetters(q, "retired.t", "grp"); len(left) != 0 {
			t.Errorf("a dropped retired event is still reported: %v", labelsOf(left))
		}
	})

	t.Run("a_dead_letter_the_transport_cannot_carry_is_refused_as_too_large", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := topics.DeadLetter("changefeed.large", "grp")
		err := q.KeepDeadLetter(ctx, subject, make([]byte, queue.MaxPayloadBytes+1))
		if !errors.Is(err, queue.ErrTooLarge) {
			t.Errorf("an entry past the transport's ceiling answered %v, want queue.ErrTooLarge: "+
				"a keeper that cannot tell it from an outage retries it for ever", err)
		}
		if got := lettersOn(ctx, t, q, subject); len(got) != 0 {
			t.Errorf("a refused entry was kept: %d listed", len(got))
		}
	})

	t.Run("a_subject_outside_the_dead_letter_namespace_is_refused", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		// RAW BYTES ON AN EVENT SUBJECT would reach every consumer of it
		// as an event nobody can decode, and a pattern names no one
		// subject to keep on.
		for _, subject := range []string{"crewlet.events.kept", topics.DeadLetterPrefix + ">", ""} {
			if err := q.KeepDeadLetter(ctx, subject, []byte("x")); err == nil {
				t.Errorf("KeepDeadLetter(%q) was accepted", subject)
			}
			if _, err := q.DeadLettersOn(ctx, subject); err == nil {
				t.Errorf("DeadLettersOn(%q) was accepted", subject)
			}
			if err := q.DropDeadLetter(ctx, subject, 1); err == nil {
				t.Errorf("DropDeadLetter(%q) was accepted", subject)
			}
		}
	})
}

// keep keeps body on subject, failing the case on an error.
func keep(ctx context.Context, t *testing.T, q queue.EventQueue, subject string, body []byte) {
	t.Helper()
	if err := q.KeepDeadLetter(ctx, subject, body); err != nil {
		t.Fatalf("KeepDeadLetter(%s): %v", subject, err)
	}
}

// lettersOn is what subject holds, failing the case on an error: a listing
// that could not be read is never an empty one.
func lettersOn(ctx context.Context, t *testing.T, q queue.EventQueue, subject string) []queue.Kept {
	t.Helper()
	kept, err := q.DeadLettersOn(ctx, subject)
	if err != nil {
		t.Fatalf("DeadLettersOn(%s): %v", subject, err)
	}
	return kept
}

// bodiesOf is each entry's bytes as text, for a failure message.
func bodiesOf(kept []queue.Kept) []string {
	out := make([]string, len(kept))
	for i, k := range kept {
		out[i] = string(k.Body)
	}
	return out
}

// keptErr is second for the listing of dead letters.
func keptErr(_ []queue.Kept, err error) error { return err }
