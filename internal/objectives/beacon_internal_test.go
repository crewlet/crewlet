package objectives

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// THE REMEMBERED FRAMES ARE BOUNDED: past the bound the frames whose tickets
// have ended are forgotten, and when none has, a report waits for the first to
// end rather than being remembered loosely and counted twice.
func TestTheRememberedFramesAreBounded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	b, err := NewBeacon(NewMeter(nil, clock), clock)
	if err != nil {
		t.Fatal(err)
	}
	b.maxFrames = 3
	accept := func(viewer, frame string, expires time.Time) error {
		return b.Accept(ReadyReport{Viewer: viewer, Frame: frame, Minted: now.Add(-time.Minute),
			Expires: expires, Outcome: ReadyOK, Elapsed: time.Second})
	}
	for i := range 3 {
		if err := accept(fmt.Sprintf("v%d", i), fmt.Sprintf("f%d", i), now.Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	err = accept("v9", "f9", now.Add(time.Hour))
	var throttled *Throttled
	if !errors.As(err, &throttled) || throttled.RetryAfter != time.Minute {
		t.Fatalf("a report past the bound: %v, want a wait until the first frame's ticket ends", err)
	}
	now = now.Add(time.Minute)
	if err := accept("v9", "f9", now.Add(time.Hour)); err != nil {
		t.Fatalf("once a ticket ended its frame is forgotten and there is room: %v", err)
	}
	if _, kept := b.frames["f0"]; kept {
		t.Error("the frame whose ticket ended is still remembered")
	}
}

// A CREDENTIAL WHOSE BUCKET HAS REFILLED IS DROPPED past the viewers' bound,
// which changes no answer.
func TestRefilledBucketsAreDroppedPastTheBound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	b, err := NewBeacon(NewMeter(nil, clock), clock)
	if err != nil {
		t.Fatal(err)
	}
	for i := range maxViewers {
		b.viewers[fmt.Sprintf("v%d", i)] = &bucket{tokens: ReadyReportsBurst, last: now}
	}
	b.viewers["busy"] = &bucket{tokens: 0, last: now}
	if wait := b.take("new", now); wait != 0 {
		t.Fatalf("a new credential waited %s", wait)
	}
	if len(b.viewers) != 2 {
		t.Errorf("%d buckets kept, want the busy one and the new one", len(b.viewers))
	}
	if wait := b.take("busy", now); wait <= 0 {
		t.Error("the busy credential's empty bucket was forgotten and refilled")
	}
}
