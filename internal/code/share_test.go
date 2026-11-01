package code_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// SEAT GIT WORK IS HALF THE READ LANE. A node's seats together hold at most
// ShareOf its read lane's slots — half, at least one — because the other half
// is what a person's push scans, a sync round's peel and a fetch's want check
// wait in, FIFO, for at most gitproc.SlotWait: sized to the lane, a burst of
// seat reads would refuse a person's push busy. Over four read slots two units
// are held, a third taker waits, and past GitShareWait (scaled here) it is
// answered unready — about the node, so the asker's walk moves on — rather
// than queued longer; a unit given back is taken by the waiter; and however
// many takers race, never more than two run at once. Every take is timed by
// its outcome — the one that ran out its whole wait, which is a term of the
// attempt its asker was given — and the gauge follows what is held.
//
// Mutation: ShareOf answering the whole lane — four in flight, red. An
// unready take recorded as no wait — red.
func TestSeatGitWorkIsHalfTheReadLane(t *testing.T) {
	t.Parallel()
	for slots, want := range map[int]int{0: 1, 1: 1, 2: 1, 3: 1, 4: 2, 5: 2, 64: 32} {
		if got := code.ShareOf(slots); got != want {
			t.Errorf("ShareOf(%d) = %d, want %d: half the read lane, at least one", slots, got, want)
		}
	}

	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	s := code.NewGitShare("git-a", 4, rec)
	code.SetShareWait(s, 50*time.Millisecond)
	if s.Units() != 2 {
		t.Fatalf("a share of a four-slot read lane holds %d units, want 2", s.Units())
	}
	if got := gauge(rec, metrics.CodeSeatworkHeld); got != 0 {
		t.Errorf("a share nobody holds reads %v held, want 0 — present, and zero", got)
	}
	ctx := t.Context()
	one, err := s.Take(ctx, code.SeatWorkRead)
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.Take(ctx, code.SeatWorkRead)
	if err != nil {
		t.Fatal(err)
	}
	if s.Held() != 2 || gauge(rec, metrics.CodeSeatworkHeld) != 2 {
		t.Errorf("held %d (gauge %v), want 2", s.Held(), gauge(rec, metrics.CodeSeatworkHeld))
	}

	// THE THIRD WAITS ITS WHILE, THEN IS UNREADY — about the node.
	_, err = s.Take(ctx, code.SeatWorkRead)
	var unready *code.ErrGitUnready
	if !errors.As(err, &unready) || unready.Reason != code.UnreadyShare {
		t.Fatalf("a third take over a full share = %v, want ErrGitUnready{share}", err)
	}

	// A UNIT GIVEN BACK IS THE WAITER'S, and a release twice is one.
	got := make(chan error, 1)
	go func() {
		release, err := s.Take(ctx, code.SeatWorkRead)
		if err == nil {
			release()
		}
		got <- err
	}()
	one()
	one()
	if err := <-got; err != nil {
		t.Errorf("a waiter after a release: %v, want the released unit", err)
	}
	two()
	if s.Held() != 0 {
		t.Errorf("held %d after every release, want 0", s.Held())
	}

	// A CALLER THAT LEAVES is its own answer, and no count.
	full1, _ := s.Take(ctx, code.SeatWorkRead)
	full2, _ := s.Take(ctx, code.SeatWorkRead)
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Take(gone, code.SeatWorkRead); !errors.Is(err, context.Canceled) {
		t.Errorf("a take whose caller left = %v, want context.Canceled", err)
	}
	full1()
	full2()
	if got, _ := waits(rec, "granted"); got != 5 {
		t.Errorf("waits{outcome=granted} counts %d, want 5", got)
	}
	if got, waited := waits(rec, "unready"); got != 1 || waited < 50 {
		t.Errorf("waits{outcome=unready} counts %d waiting %.1f ms, want 1 waiting its whole 50 ms — the one "+
			"that waited its while", got, waited)
	}

	// NEVER MORE THAN THE SHARE AT ONCE, however many race.
	code.SetShareWait(s, time.Minute)
	var running, most atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			release, err := s.Take(ctx, code.SeatWorkRead)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			n := running.Add(1)
			for {
				m := most.Load()
				if n <= m || most.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			running.Add(-1)
		})
	}
	wg.Wait()
	if most.Load() > 2 {
		t.Errorf("%d takers ran at once, past the share's 2", most.Load())
	}

	// A kind of work this build does not know is refused by name.
	if _, err := s.Take(ctx, "compact"); !errors.Is(err, code.ErrInvalid) {
		t.Errorf("an unknown kind of seat work = %v, want ErrInvalid", err)
	}
}

// AN ENGINE WRITE'S BUILD IS SEAT GIT WORK OF THE SAME SHARE: two builds hold
// a four-slot node's whole share, so a seat's read waits its while and is
// unready — about the node — and a unit a build gives back is a read's; each
// take is counted by the work it is (`work=write`).
//
// Mutation: a share of its own for writes — the read is granted, red.
func TestAWritesBuildDrawsOnTheSeatShare(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	s := code.NewGitShare("git-a", 4, rec)
	code.SetShareWait(s, 50*time.Millisecond)
	ctx := t.Context()
	one, err := s.Take(ctx, code.SeatWorkWrite)
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.Take(ctx, code.SeatWorkWrite)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Take(ctx, code.SeatWorkRead)
	var unready *code.ErrGitUnready
	if !errors.As(err, &unready) || unready.Reason != code.UnreadyShare {
		t.Fatalf("a read beside two builds holding the share = %v, want ErrGitUnready{share}", err)
	}
	one()
	read, err := s.Take(ctx, code.SeatWorkRead)
	if err != nil {
		t.Fatalf("a read once a build gave its unit back: %v", err)
	}
	read()
	two()
	for _, row := range []struct {
		work, outcome string
		want          uint64
	}{{"write", "granted", 2}, {"read", "unready", 1}, {"read", "granted", 1}} {
		got := uint64(0)
		for _, snap := range rec.Read() {
			if snap.Name == metrics.CodeSeatworkWaits && snap.Attrs["work"] == row.work &&
				snap.Attrs["outcome"] == row.outcome {
				got = snap.Count
			}
		}
		if got != row.want {
			t.Errorf("waits{work=%s, outcome=%s} counts %d, want %d", row.work, row.outcome, got, row.want)
		}
	}
}

// gauge is the gauge name's reading.
func gauge(rec *metrics.Recorder, name string) float64 {
	for _, s := range rec.Read() {
		if s.Name == name {
			return s.Value
		}
	}
	return -1
}

// waits is crewlet.code.seatwork.waits{outcome, work=read}: how many takes it
// holds, and how long they waited in all, in milliseconds.
func waits(rec *metrics.Recorder, outcome string) (count uint64, waitedMs float64) {
	for _, s := range rec.Read() {
		if s.Name == metrics.CodeSeatworkWaits && s.Attrs["outcome"] == outcome && s.Attrs["work"] == "read" {
			return s.Count, s.Sum
		}
	}
	return 0, 0
}
