package queuetest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// absentLetter is a sequence no case keeps an entry under: what a drop of an
// entry already gone names, and what the lifecycle probe drops.
const absentLetter = math.MaxUint64

// lettersTopic is the head every case's dead-letter subjects lie under, and
// lettersUnder the subtree it names: what a case counts the marks of.
const lettersTopic = "changefeed"

var lettersUnder = topics.DeadLettersUnder(lettersTopic)

// runDeadLetters certifies the dead-letter verbs: KeepDeadLetter,
// DeadLettersOn, DeadLetterFrame, DeadLetterMarks and DropDeadLetter.
//
// # Why they are certified at all
//
// Because the reader that keeps one acts on every answer they give. A wake
// feed dead-letters a record no node could read for an hour and acks the
// record only once the entry is kept (internal/changefeed): a keep that
// returned before the entry was durable, or a frame read back other than it
// was kept, is a wake lost with nothing reporting it. And the alarm's reading
// counts an entry that VANISHED between two listings as let go by the
// retention, lists again only when the marks move, and trusts a listing only
// once its count is the marks': marks that lagged a keep, or still counted a
// drop, would hide an entry from the alarm or count an expiry that never
// happened. And a listing that carried an entry's body would move every
// record a feed gave up on to every data node on every beat it lists. Each
// case below is one way a plausible backend answers wrongly while every other
// case in this suite passes.
//
// # A listing is read as its reader reads it
//
// The contract lets a listing come from a copy behind the broker, and holds
// the marks to the broker itself — so every listing a case asserts on is the
// one that agrees with the marks ([listedAsMarked]), which is how the alarm's
// reading takes one. A backend whose listing never catches up fails there,
// and one whose marks lag a keep or a drop fails the cases that read the
// marks with no wait.
func (s *suite) runDeadLetters(t *testing.T) {
	ctx := t.Context()

	t.Run("a_kept_dead_letter_is_listed_by_its_header_and_read_back_by_its_sequence", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := topics.DeadLetter(lettersTopic+".kept", "grp")
		// BYTES, NOT AN EVENT: a newline, a byte that is not UTF-8 and a
		// NUL in each body, which a backend that re-encoded it as text or
		// as an event would not hand back as it was kept.
		first := letterOf([]byte(`{"v":1,"seq":1}`), []byte("\x00\xff frame\n one"))
		second := letterOf([]byte(`{"v":1,"seq":2}`), []byte("frame two"))
		keep(ctx, t, q, subject, first)
		keep(ctx, t, q, subject, second)

		got := listedAsMarked(ctx, t, q, lettersUnder, subject)
		if len(got) != 2 {
			t.Fatalf("the subject lists %d entries, want the two kept", len(got))
		}
		if !bytes.Equal(got[0].Header, first.header) || !bytes.Equal(got[1].Header, second.header) {
			t.Errorf("the subject lists the headers %q and %q, want %q then %q verbatim, oldest first",
				got[0].Header, got[1].Header, first.header, second.header)
		}
		for i, k := range got {
			switch {
			case k.Seq == 0:
				t.Errorf("entry %d has no sequence, so no drop can name it", i)
			case k.StoredAt.IsZero():
				t.Errorf("entry %d carries no instant it was kept at", i)
			case !k.Expires.After(k.StoredAt): //fixedwait:stated Expires is StoredAt plus the retention, computed once from that stamp (memory's DeadLettersOn, jetstream's letterStream.kept)
				t.Errorf("entry %d expires at %v, not after it was kept at %v: a reader "+
					"could not say how long it has left", i, k.Expires, k.StoredAt)
			}
		}
		if got[1].Seq <= got[0].Seq {
			t.Errorf("the second entry's sequence %d is not after the first's %d",
				got[1].Seq, got[0].Seq)
		}
		for i, want := range []letter{first, second} {
			if frame := frameOf(ctx, t, q, subject, got[i].Seq); !bytes.Equal(frame, want.body) {
				t.Errorf("entry %d reads back as %q, want %q verbatim", i, frame, want.body)
			}
		}
	})

	t.Run("a_listing_never_carries_a_body", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := topics.DeadLetter(lettersTopic+".large", "grp")
		// A FRAME AS LARGE AS A WAKE FEED KEEPS, whose bytes a backend that
		// listed bodies would hand back where the header belongs or beside
		// it; and an entry kept with no header at all, which lists none.
		large := letterOf([]byte(`{"v":1,"seq":1}`), bytes.Repeat([]byte("frame "), 1<<17))
		bare := letterOf(nil, []byte("a body with no header"))
		keep(ctx, t, q, subject, large)
		keep(ctx, t, q, subject, bare)

		got := listedAsMarked(ctx, t, q, lettersUnder, subject)
		if len(got) != 2 {
			t.Fatalf("the subject lists %d entries, want the two kept", len(got))
		}
		if !bytes.Equal(got[0].Header, large.header) {
			t.Errorf("the large entry is listed with %d bytes where its %d-byte header belongs: "+
				"a listing carries the header the entry was kept under and nothing of its body",
				len(got[0].Header), len(large.header))
		}
		if len(got[1].Header) != 0 {
			t.Errorf("an entry kept with no header is listed with %q", got[1].Header)
		}
		if frame := frameOf(ctx, t, q, subject, got[0].Seq); !bytes.Equal(frame, large.body) {
			t.Errorf("the large entry reads back as %d bytes, want its %d-byte body", len(frame),
				len(large.body))
		}
	})

	t.Run("a_frame_is_read_only_from_its_own_subject_and_not_once_it_is_gone", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		mine := topics.DeadLetter(lettersTopic+".mine", "grp")
		theirs := topics.DeadLetter(lettersTopic+".theirs", "grp")
		keep(ctx, t, q, mine, letterOf([]byte(`{"v":1}`), []byte("mine")))
		keep(ctx, t, q, theirs, letterOf([]byte(`{"v":1}`), []byte("theirs")))
		seq := listedAsMarked(ctx, t, q, lettersUnder, mine)[0].Seq

		// NAMED UNDER ANOTHER SUBJECT, an entry is refused and nothing of
		// it answered, as a drop refuses it: a caller that names the wrong
		// subject reads nobody else's bytes.
		if frame, held, err := q.DeadLetterFrame(ctx, theirs, seq); err == nil || held || frame != nil {
			t.Errorf("a frame read under another subject's name answered %q, %v, %v: want a "+
				"refusal and nothing read", frame, held, err)
		}
		if frame, held, err := q.DeadLetterFrame(ctx, mine, 0); err == nil || held || frame != nil {
			t.Errorf("a frame read of sequence 0, which names no entry, answered %q, %v, %v",
				frame, held, err)
		}
		for name, seq := range map[string]uint64{"never kept": absentLetter, "dropped": seq} {
			if name == "dropped" {
				if err := q.DropDeadLetter(ctx, mine, seq); err != nil {
					t.Fatalf("DropDeadLetter: %v", err)
				}
			}
			// NOT HELD, AND NO ERROR: a peer's replay or the retention took
			// it, which is the answer, not a failure.
			if frame, held, err := q.DeadLetterFrame(ctx, mine, seq); err != nil || held || frame != nil {
				t.Errorf("a frame read of an entry %s answered %q, %v, %v: want it not held, "+
					"with no error", name, frame, held, err)
			}
		}
	})

	t.Run("the_marks_count_a_subtree_and_move_with_every_keep_and_no_drop", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		a, b := topics.DeadLetter(lettersTopic+".a", "grp"), topics.DeadLetter(lettersTopic+".b", "grp")
		// A SUBSCRIPTION'S POISON beside the feeds' entries: outside the
		// subtree, so it is in no count, and in the same dead letters, so
		// its keep moves the last sequence.
		outside := topics.DeadLetter("crewlet.agent.ana.inbox", "agent-ana")
		keep(ctx, t, q, a, letterOf([]byte(`{"v":1}`), []byte("a1")))
		keep(ctx, t, q, a, letterOf([]byte(`{"v":1}`), []byte("a2")))
		keep(ctx, t, q, b, letterOf([]byte(`{"v":1}`), []byte("b1")))

		// READ ONCE, WITH NO WAIT, after every write: the marks are the
		// broker's own, so a keep or a drop that returned is in them.
		marks := marksOf(ctx, t, q, lettersUnder)
		lastB := listedAsMarked(ctx, t, q, lettersUnder, b)[0].Seq
		wantCounts(t, "after three keeps", marks, map[string]uint64{a: 2, b: 1})
		if marks.Last != lastB {
			t.Errorf("after three keeps the marks' last sequence is %d, want %d, the last kept",
				marks.Last, lastB)
		}

		keep(ctx, t, q, outside, letterOf(nil, []byte("poison")))
		poison := listedAsMarked(ctx, t, q, topics.DeadLettersUnder("crewlet"), outside)
		after := marksOf(ctx, t, q, lettersUnder)
		wantCounts(t, "after a keep outside the subtree", after, map[string]uint64{a: 2, b: 1})
		if len(poison) != 1 || after.Last != poison[0].Seq {
			t.Errorf("a keep outside the subtree left the last sequence at %d, want it moved to "+
				"that keep's (%v): a reader that lists again only when it moves would miss "+
				"a feed's keep beside it", after.Last, poison)
		}

		first := listedAsMarked(ctx, t, q, lettersUnder, a)[0]
		if err := q.DropDeadLetter(ctx, a, first.Seq); err != nil {
			t.Fatalf("DropDeadLetter: %v", err)
		}
		dropped := marksOf(ctx, t, q, lettersUnder)
		wantCounts(t, "after a drop", dropped, map[string]uint64{a: 1, b: 1})
		if dropped.Last != after.Last {
			t.Errorf("a drop moved the last sequence from %d to %d: only a keep moves it",
				after.Last, dropped.Last)
		}
		if err := q.DropDeadLetter(ctx, b, lastB); err != nil {
			t.Fatalf("DropDeadLetter: %v", err)
		}
		wantCounts(t, "after the subject's last entry was dropped", marksOf(ctx, t, q, lettersUnder),
			map[string]uint64{a: 1})
	})

	t.Run("a_dropped_dead_letter_is_gone_and_dropping_it_again_is_no_error", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := topics.DeadLetter(lettersTopic+".dropped", "grp")
		keep(ctx, t, q, subject, letterOf([]byte(`"doomed"`), []byte("doomed")))
		keep(ctx, t, q, subject, letterOf([]byte(`"kept"`), []byte("kept")))
		doomed := listedAsMarked(ctx, t, q, lettersUnder, subject)[0]

		if err := q.DropDeadLetter(ctx, subject, doomed.Seq); err != nil {
			t.Fatalf("DropDeadLetter: %v", err)
		}
		if got := listedAsMarked(ctx, t, q, lettersUnder, subject); len(got) != 1 || string(got[0].Header) != `"kept"` {
			t.Fatalf("after the drop the subject lists %v, want the other entry alone", headersOf(got))
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
		owner := topics.DeadLetter(lettersTopic+".owner", "grp")
		other := topics.DeadLetter(lettersTopic+".other", "grp")
		keep(ctx, t, q, owner, letterOf([]byte(`"owned"`), []byte("owned")))
		entry := listedAsMarked(ctx, t, q, lettersUnder, owner)[0]
		if err := q.DropDeadLetter(ctx, other, entry.Seq); err == nil {
			t.Error("a drop under another subject's name was accepted")
		}
		if err := q.DropDeadLetter(ctx, owner, 0); err == nil {
			t.Error("a drop of sequence 0, which names no entry, was accepted")
		}
		if got := marksOf(ctx, t, q, lettersUnder).Counts[owner]; got != 1 {
			t.Errorf("a refused drop removed the entry: the subject counts %d", got)
		}
	})

	t.Run("a_dead_letter_is_the_brokers_and_outlives_the_client_that_kept_it", func(t *testing.T) {
		t.Parallel()
		peerOf := s.needPeer(t)
		q := s.start(ctx, t)
		peer := s.startQueue(ctx, t, peerOf(t, q))
		subject := topics.DeadLetter(lettersTopic+".shared", "grp")
		kept := letterOf([]byte(`"kept by one node"`), []byte("its frame"))
		keep(ctx, t, q, subject, kept)
		if err := q.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		// THE NODE THAT REPLAYS AN ENTRY IS RARELY THE ONE THAT KEPT IT:
		// a peer that cannot list it, read it or count it, or loses it with
		// its keeper, would leave a wake no newer node could ever derive.
		got := listedAsMarked(ctx, t, peer, lettersUnder, subject)
		if len(got) != 1 || !bytes.Equal(got[0].Header, kept.header) {
			t.Fatalf("a peer lists %v after the keeper stopped, want its entry", headersOf(got))
		}
		if frame := frameOf(ctx, t, peer, subject, got[0].Seq); !bytes.Equal(frame, kept.body) {
			t.Errorf("a peer reads the entry back as %q, want %q", frame, kept.body)
		}
		if err := peer.DropDeadLetter(ctx, subject, got[0].Seq); err != nil {
			t.Fatalf("a peer's drop: %v", err)
		}
		if got := listedAsMarked(ctx, t, peer, lettersUnder, subject); len(got) != 0 {
			t.Errorf("after a peer's drop the subject lists %v", headersOf(got))
		}
	})

	t.Run("a_subscriptions_retired_event_is_one_more_dead_letter", func(t *testing.T) {
		t.Parallel()
		// ONE PLACE, whoever gave up: an event its subscription's budget
		// retired is listed, read and dropped by the same verbs as a
		// reader's entry, so a reader of one subject never has to know
		// which kind it holds — kept with no header, its body the event.
		newQueueWithAttempts := s.needAttempts(t)
		deadLetters := s.needDeadLetters(t)
		q := s.startQueue(ctx, t, newQueueWithAttempts(t, 1))
		subscribe(ctx, t, q, "retired.t", "grp", func(context.Context, *events.Event) queue.Result {
			return queue.Nak(errors.New("permanent failure"))
		})
		publish(ctx, t, q, "retired.t", newEvent("poison"))
		awaitState(t, "the event to be retired to its dead-letter subject", func() bool {
			return len(deadLetters(q, "retired.t", "grp")) == 1
		})

		subject := topics.DeadLetter("retired.t", "grp")
		got := listedAsMarked(ctx, t, q, topics.DeadLettersUnder("retired"), subject)
		if len(got) != 1 || len(got[0].Header) != 0 {
			t.Fatalf("the retired event's subject lists %v, want one entry with no header", headersOf(got))
		}
		var ev events.Event
		if err := json.Unmarshal(frameOf(ctx, t, q, subject, got[0].Seq), &ev); err != nil || labelOf(&ev) != "poison" {
			t.Fatalf("the listed entry reads back as no event (%v), want the retired event's own bytes", err)
		}
		if err := q.DropDeadLetter(ctx, subject, got[0].Seq); err != nil {
			t.Fatalf("DropDeadLetter: %v", err)
		}
		if left := deadLetters(q, "retired.t", "grp"); len(left) != 0 {
			t.Errorf("a dropped retired event is still reported: %v", labelsOf(left))
		}
	})

	// ONE ENTRY FOR ONE IDENTITY. A reader that settles its own deliveries
	// is handed one record twice whenever its keep outlasts the delivery's
	// window — a keep across a slow link waits as long as its bytes take —
	// and it cannot hold the delivery meanwhile, so the second keep is what
	// must come to nothing. A backend that kept it would hold two entries of
	// one record; one that collapsed two identities, or one identity across
	// two subjects, would lose an entry somebody else kept.
	t.Run("a_second_keep_of_one_identity_keeps_nothing_and_is_answered_already_kept", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := topics.DeadLetter(lettersTopic+".once", "grp")
		elsewhere := topics.DeadLetter(lettersTopic+".elsewhere", "grp")
		if keepAs(ctx, t, q, subject, "the record", letterOf([]byte(`"first"`), []byte("frame"))) {
			t.Fatal("the first keep of an identity was answered already kept")
		}
		// THE SAME IDENTITY AGAIN, with a header of its own — a peer handed
		// the record while the first keep was still crossing — keeps
		// nothing, and says so.
		if !keepAs(ctx, t, q, subject, "the record", letterOf([]byte(`"second"`), []byte("frame"))) {
			t.Error("a second keep of the identity on its subject was not answered already kept")
		}
		// ANOTHER IDENTITY on the subject, and THE SAME IDENTITY on another
		// subject, are each an entry of their own.
		if keepAs(ctx, t, q, subject, "another record", letterOf([]byte(`"another"`), []byte("frame"))) {
			t.Error("a keep of another identity on the subject was answered already kept")
		}
		if keepAs(ctx, t, q, elsewhere, "the record", letterOf([]byte(`"elsewhere"`), []byte("frame"))) {
			t.Error("a keep of the identity on another subject was answered already kept")
		}
		wantCounts(t, "after four keeps of three things", marksOf(ctx, t, q, lettersUnder),
			map[string]uint64{subject: 2, elsewhere: 1})
		if got := listedAsMarked(ctx, t, q, lettersUnder, subject); len(got) != 2 ||
			string(got[0].Header) != `"first"` || string(got[1].Header) != `"another"` {
			t.Errorf("the subject lists %q, want the first keep's entry and the other identity's",
				headersOf(got))
		}
		// AN EMPTY IDENTITY names nothing, and a keep of it is refused as the
		// caller's mistake, keeping nothing.
		if _, err := q.KeepDeadLetter(ctx, subject, "", []byte(`"nameless"`), []byte("frame")); err == nil ||
			errors.Is(err, queue.ErrTooLarge) || errors.Is(err, queue.ErrUnavailable) {
			t.Errorf("a keep of no identity was answered %v, want it refused as the caller's mistake", err)
		}
		wantCounts(t, "after a keep of no identity", marksOf(ctx, t, q, lettersUnder),
			map[string]uint64{subject: 2, elsewhere: 1})
	})

	t.Run("a_dead_letter_the_transport_cannot_carry_is_refused_as_too_large", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := topics.DeadLetter(lettersTopic+".large", "grp")
		header := []byte(`{"v":1}`)
		// AT THE CONTRACT'S CEILING FOR A KEEP the entry is kept, and a
		// byte past it refused — on every backend at the same byte, so a
		// twin never keeps what the broker refuses.
		//
		// THE KEEP AT THE CEILING WAITS FOR ITS ACKNOWLEDGEMENT for as long
		// as an entry that size takes to reach the broker — across a leaf's
		// link, its eight mebibytes at the slowest pace the engine allows a
		// link (internal/jsapi's WriteBound) — since its context carries no
		// deadline: so a slow link, or a loaded runner slowing the one a
		// leaf's suite crosses, reads as slow and never as an outage, which
		// is what the keep at five seconds read as (crewlet/crewlet#1005).
		room := queue.MaxPayloadBytes - queue.LetterFraming - len(header)
		keep(ctx, t, q, subject, letterOf(header, make([]byte, room)))
		_, err := q.KeepDeadLetter(ctx, subject, "a byte past", header, make([]byte, room+1))
		if !errors.Is(err, queue.ErrTooLarge) {
			t.Errorf("an entry a byte past the ceiling answered %v, want queue.ErrTooLarge: "+
				"a keeper that cannot tell it from an outage retries it for ever", err)
		}
		if got := marksOf(ctx, t, q, lettersUnder).Counts[subject]; got != 1 {
			t.Errorf("the subject counts %d entries, want the one at the ceiling: a refused "+
				"entry was kept, or the one at the ceiling was not", got)
		}
	})

	t.Run("a_header_the_transport_would_rewrite_is_refused", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := topics.DeadLetter(lettersTopic+".headers", "grp")
		// ONE LINE OF PRINTABLE ASCII is all a header crosses the wire as:
		// a line ending would end it, a byte past the ASCII is not a
		// header's, and a space at either end is trimmed — each kept, it
		// would be listed as bytes other than the ones handed over.
		for _, header := range []string{
			"line\nfeed", "carriage\rreturn", "tab\there", "caf\xc3\xa9", "del\x7f",
			" leading", "trailing ",
		} {
			_, err := q.KeepDeadLetter(ctx, subject, header, []byte(header), []byte("body"))
			if err == nil || errors.Is(err, queue.ErrTooLarge) {
				t.Errorf("a header %q was answered %v, want it refused as the caller's mistake",
					header, err)
			}
		}
		if got := marksOf(ctx, t, q, lettersUnder).Counts[subject]; got != 0 {
			t.Errorf("refused headers left %d entries kept", got)
		}
	})

	t.Run("a_subject_outside_the_dead_letter_namespace_is_refused", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		// RAW BYTES ON AN EVENT SUBJECT would reach every consumer of it
		// as an event nobody can decode, and a pattern names no one
		// subject to keep on or count under.
		for _, subject := range []string{"crewlet.events.kept", topics.DeadLetterPrefix + ">", ""} {
			if _, err := q.KeepDeadLetter(ctx, subject, "x", []byte("x"), []byte("x")); err == nil {
				t.Errorf("KeepDeadLetter(%q) was accepted", subject)
			}
			if _, err := q.DeadLettersOn(ctx, subject); err == nil {
				t.Errorf("DeadLettersOn(%q) was accepted", subject)
			}
			if _, _, err := q.DeadLetterFrame(ctx, subject, 1); err == nil {
				t.Errorf("DeadLetterFrame(%q) was accepted", subject)
			}
			if _, err := q.DeadLetterMarks(ctx, subject); err == nil {
				t.Errorf("DeadLetterMarks(%q) was accepted", subject)
			}
			if err := q.DropDeadLetter(ctx, subject, 1); err == nil {
				t.Errorf("DropDeadLetter(%q) was accepted", subject)
			}
		}
	})
}

// letter is one entry a case keeps: its header and its body.
type letter struct {
	header, body []byte
}

// letterOf is an entry of header and body.
func letterOf(header, body []byte) letter { return letter{header: header, body: body} }

// entries numbers every entry the suite keeps through [keep], each the
// identity of its own no other keep names.
var entries atomic.Uint64

// keep keeps l on subject under an identity of its own, failing the case on an
// error or on an answer that it was already kept.
func keep(ctx context.Context, t *testing.T, q queue.EventQueue, subject string, l letter) {
	t.Helper()
	id := fmt.Sprintf("entry %d", entries.Add(1))
	if already := keepAs(ctx, t, q, subject, id, l); already {
		t.Fatalf("KeepDeadLetter(%s, %q) of an entry no keep named before answered already kept",
			subject, id)
	}
}

// keepAs keeps l on subject under the identity id, answering whether it was
// already kept, and failing the case on an error.
func keepAs(ctx context.Context, t *testing.T, q queue.EventQueue, subject, id string, l letter) bool {
	t.Helper()
	already, err := q.KeepDeadLetter(ctx, subject, id, l.header, l.body)
	if err != nil {
		t.Fatalf("KeepDeadLetter(%s, %q): %v", subject, id, err)
	}
	return already
}

// listed is what subject's listing answers, failing the case on an error: a
// listing that could not be read is never an empty one.
func listed(ctx context.Context, t *testing.T, q queue.EventQueue, subject string) []queue.Kept {
	t.Helper()
	kept, err := q.DeadLettersOn(ctx, subject)
	if err != nil {
		t.Fatalf("DeadLettersOn(%s): %v", subject, err)
	}
	return kept
}

// listedAsMarked is subject's listing once it agrees with the marks of the
// subtree under, which subject lies in — its count of the subject's entries
// the marks' count — which is how a reader that must know its listing is
// current takes one: the contract lets a listing come from a copy behind the
// broker, and holds the marks to the broker.
func listedAsMarked(ctx context.Context, t *testing.T, q queue.EventQueue, under,
	subject string) []queue.Kept {

	t.Helper()
	var got []queue.Kept
	awaitState(t, "the listing of "+subject+" to agree with the marks", func() bool {
		want := marksOf(ctx, t, q, under).Counts[subject]
		got = listed(ctx, t, q, subject)
		return uint64(len(got)) == want
	})
	return got
}

// marksOf is what the subtree under holds, failing the case on an error.
func marksOf(ctx context.Context, t *testing.T, q queue.EventQueue, under string) queue.Marks {
	t.Helper()
	marks, err := q.DeadLetterMarks(ctx, under)
	if err != nil {
		t.Fatalf("DeadLetterMarks(%s): %v", under, err)
	}
	return marks
}

// wantCounts fails the case unless marks counts exactly want.
func wantCounts(t *testing.T, when string, marks queue.Marks, want map[string]uint64) {
	t.Helper()
	if !maps.Equal(marks.Counts, want) {
		t.Errorf("%s the marks count %v, want %v", when, marks.Counts, want)
	}
}

// frameOf is the body the entry seq of subject was kept with, failing the case
// unless the subject holds it.
func frameOf(ctx context.Context, t *testing.T, q queue.EventQueue, subject string, seq uint64) []byte {
	t.Helper()
	frame, held, err := q.DeadLetterFrame(ctx, subject, seq)
	if err != nil || !held {
		t.Fatalf("DeadLetterFrame(%s, %d) answered held %v, %v", subject, seq, held, err)
	}
	return frame
}

// headersOf is each entry's header as text, for a failure message.
func headersOf(kept []queue.Kept) []string {
	out := make([]string, len(kept))
	for i, k := range kept {
		out[i] = string(k.Header)
	}
	return out
}

// keptErr is second for the listing of dead letters.
func keptErr(_ []queue.Kept, err error) error { return err }

// frameErr is third for the read of a dead letter's frame.
func frameErr(_ []byte, _ bool, err error) error { return err }

// marksErr is second for the marks of a subtree.
func marksErr(_ queue.Marks, err error) error { return err }
