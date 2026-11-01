package live

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

var (
	conversation = uuid.MustParse("0192f0aa-61c2-7d1e-9b40-3c5d7e8f9a01")
	turn         = uuid.MustParse("0192f0aa-61c2-7d1e-9b40-3c5d7e8f9a02")
	epoch        = time.Date(2026, 10, 24, 9, 41, 0, 0, time.UTC)
)

func status() wire.Frame {
	return wire.Frame{Kind: wire.EventStatus, Status: &wire.StatusEvent{State: wire.StatusThinking}}
}

// A SLOW CLIENT IS DISCONNECTED, NOT DROPPED FROM.
//
// A transcript rebuilt from deltas cannot survive a hole, so a subscriber that
// falls [SubscriberFrames] behind is closed, having been handed every frame up
// to the point it fell behind and nothing past a gap; its client resumes from
// its last Last-Event-ID and the log replays the rest.
//
// Mutation: drop the frame instead of closing (a `default:` that does nothing)
// and the subscriber sees a gap and stays open.
func TestASlowClientIsDisconnectedNotDroppedFrom(t *testing.T) {
	t.Parallel()
	l := newLog(uuid.Nil, conversation, turn, 1, func() time.Time { return epoch }, nil)
	_, sub, _ := l.Subscribe(0)
	for range SubscriberFrames + 10 {
		if _, err := l.Append(status()); err != nil {
			t.Fatal(err)
		}
	}
	var got []uint64
	for f := range sub.C() {
		got = append(got, f.Seq)
	}
	if !sub.Overflowed() {
		t.Fatal("a subscriber past its buffer was not marked overflowed")
	}
	if len(got) != SubscriberFrames {
		t.Fatalf("drew %d frames, want the %d it could hold", len(got), SubscriberFrames)
	}
	for i, seq := range got {
		if seq != uint64(i+1) {
			t.Fatalf("frame %d is seq %d: a hole in what the client drew", i, seq)
		}
	}
	replay, again, gap := l.Subscribe(got[len(got)-1])
	defer again.Cancel()
	if gap || len(replay) != 10 || replay[0].Seq != SubscriberFrames+1 {
		t.Errorf("the resume replays %d frames from %v (gap %v), want the 10 it missed",
			len(replay), replay, gap)
	}
}

// A FRAME THE PROTOCOL REFUSES TAKES NO SEQ, so the numbering a client draws
// never has a hole for a frame that was never sent.
func TestARefusedFrameTakesNoSeq(t *testing.T) {
	t.Parallel()
	l := newLog(uuid.Nil, conversation, turn, 1, func() time.Time { return epoch }, nil)
	if _, err := l.Append(wire.Frame{Kind: wire.EventStatus}); err == nil {
		t.Fatal("a status frame with no payload was appended")
	}
	f, err := l.Append(status())
	if err != nil || f.Seq != 1 {
		t.Fatalf("the next frame is seq %d (%v), want 1", f.Seq, err)
	}
}

// AN ENDED LOG TAKES NO FRAME and closes every subscription after its replay;
// a resumed segment's log continues the turn's numbering.
func TestAResumedSegmentContinuesTheNumbering(t *testing.T) {
	t.Parallel()
	now := epoch
	h := NewHub(func() time.Time { return now }, time.Hour, nil)
	l := h.Open(uuid.Nil, conversation, turn, 0)
	for range 3 {
		if _, err := l.Append(status()); err != nil {
			t.Fatal(err)
		}
	}
	l.End()
	if _, err := l.Append(status()); !errors.Is(err, ErrEnded) {
		t.Fatalf("an ended log appended: %v", err)
	}
	replay, sub, _ := l.Subscribe(1)
	if _, open := <-sub.C(); open || len(replay) != 2 {
		t.Errorf("a subscription to an ended log: %d replayed, still open %v", len(replay), open)
	}
	now = now.Add(EndedRetention + time.Second)
	if _, ok := h.Get(turn); ok {
		t.Fatal("an ended log outlived its retention")
	}
	next := h.Open(uuid.Nil, conversation, turn, 0)
	f, err := next.Append(status())
	if err != nil || f.Seq != 4 {
		t.Errorf("the resumed segment's first frame is seq %d (%v), want 4", f.Seq, err)
	}
}

// THE LOG KEEPS WITHIN ITS BOUNDS, oldest first, and says so as a gap.
func TestTheLogKeepsWithinItsBounds(t *testing.T) {
	t.Parallel()
	l := newLog(uuid.Nil, conversation, turn, 1, func() time.Time { return epoch }, nil)
	for range FrameLogFrames + 5 {
		if _, err := l.Append(status()); err != nil {
			t.Fatal(err)
		}
	}
	replay, sub, gap := l.Subscribe(0)
	defer sub.Cancel()
	if !gap || len(replay) != FrameLogFrames || replay[0].Seq != 6 {
		t.Errorf("replay = %d frames from %d (gap %v), want the newest %d and a gap",
			len(replay), replay[0].Seq, gap, FrameLogFrames)
	}
}
