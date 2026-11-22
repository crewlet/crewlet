package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"testing"
)

// fakeInterrupter records each interrupt the cap asks for, with whether the
// marker was on disk as it was asked, and answers errs in turn.
type fakeInterrupter struct {
	marker string
	calls  record[interruptCall]
	errs   []error
}

func (f *fakeInterrupter) interrupt(_ context.Context, turn string) error {
	_, err := os.Stat(f.marker)
	n := len(f.calls.all())
	f.calls.add(interruptCall{TurnID: turn, marked: err == nil})
	if n < len(f.errs) {
		return f.errs[n]
	}
	return nil
}

// THE CAP FIRES AT EXACTLY ITS NTH RESPONSE, ONCE A TURN: N-1 responses fire
// nothing, the Nth fires its turn, later ones of the same turn fire nothing
// more, and a new turn counts from one.
func TestTheCapFiresAtExactlyItsNthResponse(t *testing.T) {
	t.Parallel()
	c := newStepCap(3, "")
	c.step("turn-1")
	c.step("turn-1")
	if len(c.fire) != 0 {
		t.Fatal("the cap of 3 fired at the second response")
	}
	c.step("turn-1")
	if len(c.fire) != 1 || <-c.fire != "turn-1" {
		t.Fatal("the cap of 3 did not fire turn-1 at its third response")
	}
	c.step("turn-1")
	if len(c.fire) != 0 {
		t.Error("the cap fired turn-1 again at its fourth response")
	}
	c.step("turn-2")
	c.step("turn-2")
	if len(c.fire) != 0 {
		t.Error("turn-2 fired at its second response: a new turn counts from one")
	}
	c.step("turn-2")
	if len(c.fire) != 1 || <-c.fire != "turn-2" {
		t.Error("turn-2 did not fire at its own third response")
	}
}

// THE MARKER IS WRITTEN BEFORE THE INTERRUPT, AND ONLY WHEN THE CAP FIRED: it
// is on disk as the interrupt is asked for and names the turn, and a turn
// short of the cap leaves none. A failed interrupt is asked again at the
// turn's next response.
func TestTheMarkerIsWrittenBeforeTheInterruptAndOnlyWhenTheCapFired(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "capped")
	h := &fakeInterrupter{marker: marker, errs: []error{errors.New("the fake failed it")}}
	c := newStepCap(2, marker)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var logged record[string]
	go func() {
		c.run(ctx, h, log.New(writerFunc(func(p []byte) { logged.add(string(p)) }), "", 0))
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	c.step("turn-1")
	if len(c.fire) != 0 {
		t.Fatal("the cap of 2 fired at the first response")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a turn short of its cap left a marker (%v)", err)
	}
	c.step("turn-1")
	calls := h.calls.await(func(c []interruptCall) bool { return len(c) == 1 })
	if calls[0].TurnID != "turn-1" || !calls[0].marked {
		t.Errorf("interrupt %+v: want turn-1, with the marker already on disk", calls[0])
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "turn-1\n" {
		t.Errorf("marker %q (%v), want the turn it ended", data, err)
	}
	// The first interrupt failed, logged once the cap may fire again: the
	// next response asks again.
	logged.await(func(l []string) bool { return len(l) == 1 })
	c.step("turn-1")
	h.calls.await(func(c []interruptCall) bool { return len(c) == 2 })
}

// writerFunc is an io.Writer that hands each write to f.
type writerFunc func(p []byte)

func (f writerFunc) Write(p []byte) (int, error) {
	f(append([]byte(nil), p...))
	return len(p), nil
}
