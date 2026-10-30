package turnctx_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
)

// rowWriter is a run row as a turn's holds write to it: the keys it took, in
// order, and an error it answers every write with while one is set.
type rowWriter struct {
	mu   sync.Mutex
	keys []string
	fail error
}

func (w *rowWriter) write(_ context.Context, keys []string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail != nil {
		return w.fail
	}
	w.keys = append(w.keys, keys...)
	return nil
}

func (w *rowWriter) held() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.keys)
}

// A TAKE DURING A LAUNCH WAITS FOR ITS ROW. A launch reads the turn's set
// (Seal) and opens the run's row from it; a key taken between the two would be
// on neither the set the launch read nor a row that does not exist yet, and
// an expiry reading the row after the turn parked would not see it. So a Take
// in that window waits: Attach — the row exists — lets it through the row's
// writer; Unseal — the launch failed before any row — lets it into memory
// only.
//
// Mutation: let Take go ahead while sealed and the key lands in memory after
// the launch read the set and is never written to the row — red.
func TestATakeDuringALaunchWaitsForItsRow(t *testing.T) {
	t.Parallel()
	for _, attach := range []bool{true, false} {
		t.Run(fmt.Sprintf("attach=%v", attach), func(t *testing.T) {
			t.Parallel()
			h := turnctx.NewHolds([]string{"repo-a"})
			if read := h.Seal(); !slices.Equal(read, []string{"repo-a"}) {
				t.Fatalf("the launch read %v, want the turn's set [repo-a]", read)
			}
			took := make(chan error, 1)
			go func() {
				_, err := h.Take(t.Context(), "repo-b")
				took <- err
			}()
			select {
			case err := <-took:
				t.Fatalf("a Take while the launch is between its read and its row returned (%v) — it "+
					"must wait for the row", err)
			case <-time.After(50 * time.Millisecond):
			}
			row := &rowWriter{}
			if attach {
				h.Attach(row.write)
			} else {
				h.Unseal()
			}
			if err := <-took; err != nil {
				t.Fatalf("the waiting Take: %v", err)
			}
			wantRow := []string(nil)
			if attach {
				wantRow = []string{"repo-b"}
			}
			if got := row.held(); !slices.Equal(got, wantRow) {
				t.Errorf("the row holds %v, want %v", got, wantRow)
			}
			if got := h.IDs(); !slices.Equal(got, []string{"repo-a", "repo-b"}) {
				t.Errorf("the turn holds %v, want [repo-a repo-b]", got)
			}
		})
	}
}

// A SEALED TAKE IS BOUNDED BY ITS CONTEXT: a launch that never reaches its row
// nor gives up leaves a request refused when its caller gives up, never one
// that waits for ever or goes ahead unheld.
func TestASealedTakeEndsWithItsContext(t *testing.T) {
	t.Parallel()
	h := turnctx.NewHolds(nil)
	h.Seal()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Take(ctx, "repo-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a Take sealed past its context = %v, want its deadline", err)
	}
	if got := h.IDs(); len(got) != 0 {
		t.Errorf("a Take that never ran holds %v", got)
	}
}

// A DERIVED TURN SHARES ITS HOLDS: a delegated worker's reads are its parent's
// to hold, since the worker ends with the parent's call and the parent's turn
// is what the expiry asks about.
//
// Mutation: copy the set in a derive — red.
func TestADerivedTurnSharesItsHolds(t *testing.T) {
	t.Parallel()
	parent := &turnctx.Turn{RunID: "run-1", Holds: turnctx.NewHolds(nil), Calls: turnctx.NewCallLog()}
	worker := parent.WithCalls(parent.Calls.Fork())
	phase := parent.InPhase("execute")
	for _, derived := range []*turnctx.Turn{worker, phase} {
		if _, err := derived.Holding().Take(t.Context(), "repo-"+string(rune('a'+len(parent.Holds.IDs())))); err != nil {
			t.Fatalf("a derived turn's Take: %v", err)
		}
	}
	if got := parent.Holds.IDs(); !slices.Equal(got, []string{"repo-a", "repo-b"}) {
		t.Errorf("the parent holds %v, want what its worker and its phase took: [repo-a repo-b]", got)
	}
}

// A FAILED HOLD WRITE ADDS NOTHING: a key the row refused is no hold once
// this process is gone, so the Take errs — refusing the request it was taken
// for — and the set does not hold it either; the next Take tries the row
// again.
//
// Mutation: add the key before writing it — red.
func TestAFailedHoldWriteAddsNothing(t *testing.T) {
	t.Parallel()
	refused := errors.New("the run's row is gone or ending, so it can hold nothing new")
	row := &rowWriter{fail: refused}
	h := turnctx.NewHolds(nil)
	h.Attach(row.write)
	if _, err := h.Take(t.Context(), "repo-a"); !errors.Is(err, refused) {
		t.Fatalf("a Take the row refused = %v, want its refusal", err)
	}
	if got := h.IDs(); len(got) != 0 {
		t.Fatalf("the turn holds %v after its row refused the write", got)
	}
	row.mu.Lock()
	row.fail = nil
	row.mu.Unlock()
	if taken, err := h.Take(t.Context(), "repo-a"); err != nil || !taken {
		t.Fatalf("the Take again = (%v, %v), want it taken", taken, err)
	}
	if got := row.held(); !slices.Equal(got, []string{"repo-a"}) {
		t.Errorf("the row holds %v, want [repo-a]", got)
	}
}

// A TURN HOLDS AT MOST MaxHolds, and the one past it is refused naming the
// bound — never a hold dropped. A key already held is no new hold, at the
// bound or not.
func TestATurnPastItsHoldBoundIsRefused(t *testing.T) {
	t.Parallel()
	seed := make([]string, turnctx.MaxHolds)
	for i := range seed {
		seed[i] = fmt.Sprintf("repo-%04d", i)
	}
	h := turnctx.NewHolds(seed)
	if taken, err := h.Take(t.Context(), seed[7]); err != nil || taken {
		t.Errorf("a held key at the bound = (%v, %v), want no new hold and no error", taken, err)
	}
	if _, err := h.Take(t.Context(), "repo-past"); !errors.Is(err, turnctx.ErrHoldsFull) {
		t.Errorf("a Take past the bound = %v, want ErrHoldsFull", err)
	}
	var none *turnctx.Holds
	if _, err := none.Take(t.Context(), "repo-a"); !errors.Is(err, turnctx.ErrNoHolds) {
		t.Errorf("a Take on no holds = %v, want ErrNoHolds — a request reaching a repository unheld", err)
	}
}

// ONLY A TAKEN KEY IS CONFIRMED, AND NO CONFIRMATION IS WRITTEN (M3-decisions
// M3-O65). A seam confirms a key once its Take is done and the hold is
// ordered, so the turn's later calls send nothing more for it: a key the set
// does not hold cannot have been ordered by a Take, and is not marked; a key
// whose seam failed after its Take is unmarked again; and the mark is the
// process's own, so a set built from a row's keys — a resumed turn, a bridged
// run's session — confirms none of them, and no mark ever reaches the row
// through the writer.
//
// Mutations: NewHolds confirming its seed → red; Confirm writing through the
// writer → red; Confirm accepting a key not in the set → red.
func TestOnlyATakenKeyIsConfirmedAndNoneIsWritten(t *testing.T) {
	t.Parallel()
	w := &rowWriter{}
	h := turnctx.NewHolds([]string{"seeded"})
	h.Attach(w.write)
	if h.Confirmed("seeded") {
		t.Error("a key the set was seeded with reads confirmed: a resumed turn would send nothing for it")
	}
	if h.Confirm("untaken") || h.Confirmed("untaken") {
		t.Error("a key nothing took was confirmed")
	}
	if _, err := h.Take(t.Context(), "taken"); err != nil {
		t.Fatalf("take: %v", err)
	}
	written := len(w.held())
	if !h.Confirm("taken") || !h.Confirmed("taken") {
		t.Fatal("a taken key did not confirm")
	}
	if h.Confirm("seeded"); !h.Confirmed("seeded") {
		t.Error("a seeded key the set holds did not confirm")
	}
	h.Unconfirm("taken")
	if h.Confirmed("taken") {
		t.Error("an unconfirmed key still reads confirmed")
	}
	if got := w.held(); len(got) != written {
		t.Errorf("confirming wrote %v to the row: a mark is the process's own", got[written:])
	}
	if !slices.Equal(h.IDs(), []string{"seeded", "taken"}) {
		t.Errorf("the set is %v after the marks moved, want [seeded taken]", h.IDs())
	}
	var none *turnctx.Holds
	if none.Confirm("taken") || none.Confirmed("taken") {
		t.Error("a nil set confirmed a key")
	}
	none.Unconfirm("taken")
}
