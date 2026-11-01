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
// order, each write it was asked for, and an error it answers every write
// with while one is set.
type rowWriter struct {
	mu     sync.Mutex
	keys   []string
	writes [][]string
	fail   error
}

func (w *rowWriter) write(_ context.Context, keys []string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, slices.Clone(keys))
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

// asked is every write the row was asked for, in order.
func (w *rowWriter) asked() [][]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.writes)
}

// ONE WRITE A CALL: a call that takes several keys — a comparison's two seats'
// branches — writes every one the set does not hold in ONE write to its run
// row, each once, and a key the set already holds is not written again; a
// write the row refuses adds none of the call's keys.
//
// Mutations: write each new key in a write of its own → three writes for the
// first call, red; write a call's held keys too → the second call writes
// [a d], red.
func TestATakeWritesEveryNewKeyOfACallInOneWrite(t *testing.T) {
	t.Parallel()
	row := &rowWriter{}
	h := turnctx.NewHolds(nil)
	h.Attach(row.write)
	if err := h.Take(t.Context(), "a", "b", "a", "c"); err != nil {
		t.Fatalf("take: %v", err)
	}
	if err := h.Take(t.Context(), "a", "d"); err != nil {
		t.Fatalf("take: %v", err)
	}
	if err := h.Take(t.Context(), "c", "a"); err != nil {
		t.Fatalf("take: %v", err)
	}
	want := [][]string{{"a", "b", "c"}, {"d"}}
	if got := row.asked(); !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("three calls wrote %v, want %v: one write a call, of the keys the set did not hold", got, want)
	}
	if got := h.IDs(); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Errorf("the turn holds %v, want [a b c d]", got)
	}

	refused := errors.New("the run's row is gone or ending, so it can hold nothing new")
	row.mu.Lock()
	row.fail = refused
	row.mu.Unlock()
	if err := h.Take(t.Context(), "e", "f"); !errors.Is(err, refused) {
		t.Fatalf("a call the row refused = %v, want its refusal", err)
	}
	if got := h.IDs(); !slices.Equal(got, []string{"a", "b", "c", "d"}) {
		t.Errorf("the turn holds %v after its row refused a call, want none of the call's keys", got)
	}
}

// A LAUNCH'S KEYS RIDE ITS ROW WRITE (M3-boxes §10.6.5): a resumed executor's
// set, its row's writer attached, takes its relaunch's keys under ForLaunch
// with NO write of their own — they are in the set, so the running turn holds
// them, and the launch reads them (Seal) for the row it resets, which unions
// them. Its Opened (Attach) settles them: a later call naming one writes
// nothing. But a key owed to the row is never left unwritten behind another
// request: a call outside the launch naming it writes it before it is sent,
// and a launch that fails before its row write (Unseal) leaves what it owed
// owed, for the next call naming it to write.
//
// Mutations: Take ignoring ForLaunch → the launch's keys written, red; a
// call outside the launch not writing an owed key → [b] missing, red; Attach
// not settling what its Seal read → c written again, red; Unseal settling →
// e never written, red.
func TestALaunchsKeysRideItsRowWrite(t *testing.T) {
	t.Parallel()
	row := &rowWriter{}
	h := turnctx.NewHolds([]string{"a"})
	h.Attach(row.write)
	launch := h.ForLaunch(t.Context())
	if err := h.Take(launch, "a", "b", "c"); err != nil {
		t.Fatalf("the launch's take: %v", err)
	}
	if got := row.asked(); len(got) != 0 {
		t.Fatalf("the launch's resolution wrote %v, want nothing before its row write", got)
	}
	if got := h.IDs(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("the turn holds %v, want its launch's keys too: [a b c]", got)
	}
	// ANOTHER CALL of the turn names b while the launch is still resolving:
	// its request may outlive the launch, so b is written first.
	if err := h.Take(t.Context(), "b"); err != nil {
		t.Fatalf("a call's take: %v", err)
	}
	if got := row.asked(); !slices.EqualFunc(got, [][]string{{"b"}}, slices.Equal[[]string]) {
		t.Fatalf("a call naming an owed key wrote %v, want [[b]]", got)
	}
	if read := h.Seal(); !slices.Equal(read, []string{"a", "b", "c"}) {
		t.Fatalf("the launch read %v, want [a b c] for its row", read)
	}
	h.Attach(row.write) // the row is reset with what the launch read: c is on it
	if err := h.Take(t.Context(), "c", "b"); err != nil {
		t.Fatalf("take after the launch: %v", err)
	}
	if got := row.asked(); len(got) != 1 {
		t.Errorf("after the launch's row write, calls naming its keys wrote %v, want nothing more", got[1:])
	}

	// A LAUNCH THAT FAILS BEFORE ITS ROW WRITE: what it took stays owed.
	if err := h.Take(h.ForLaunch(t.Context()), "e"); err != nil {
		t.Fatalf("the second launch's take: %v", err)
	}
	h.Seal()
	h.Unseal()
	if err := h.Take(t.Context(), "e"); err != nil {
		t.Fatalf("a call's take: %v", err)
	}
	if got := row.asked(); !slices.EqualFunc(got, [][]string{{"b"}, {"e"}}, slices.Equal[[]string]) {
		t.Errorf("a call naming a key a failed launch took wrote %v, want [[b] [e]]", got)
	}
	// ANOTHER SET's launch is not this set's: a context for one holds the
	// other to its writer.
	other := turnctx.NewHolds(nil)
	if err := h.Take(other.ForLaunch(t.Context()), "f"); err != nil {
		t.Fatalf("take: %v", err)
	}
	if got := row.asked(); len(got) != 3 || !slices.Equal(got[2], []string{"f"}) {
		t.Errorf("a Take under another set's launch wrote %v, want [f] written", got)
	}
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
				err := h.Take(t.Context(), "repo-b")
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
	if err := h.Take(ctx, "repo-a"); !errors.Is(err, context.DeadlineExceeded) {
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
		if err := derived.Holding().Take(t.Context(), "repo-"+string(rune('a'+len(parent.Holds.IDs())))); err != nil {
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
	if err := h.Take(t.Context(), "repo-a"); !errors.Is(err, refused) {
		t.Fatalf("a Take the row refused = %v, want its refusal", err)
	}
	if got := h.IDs(); len(got) != 0 {
		t.Fatalf("the turn holds %v after its row refused the write", got)
	}
	row.mu.Lock()
	row.fail = nil
	row.mu.Unlock()
	if err := h.Take(t.Context(), "repo-a"); err != nil {
		t.Fatalf("the Take again = %v, want it taken", err)
	}
	if got := row.held(); !slices.Equal(got, []string{"repo-a"}) {
		t.Errorf("the row holds %v, want [repo-a]", got)
	}
}

// A TURN HOLDS AT MOST MaxHolds, and the one past it is refused naming the
// bound — never a hold dropped. A key already held is no new hold, at the
// bound or not. The bound is judged over a whole call's keys: a Take whose
// new keys would carry the set past it adds none of them, and one that fits
// adds them all.
//
// Mutation: judge the bound per key (the set before the call) → the call of
// two at one below the bound adds them, red.
func TestATurnPastItsHoldBoundIsRefused(t *testing.T) {
	t.Parallel()
	seed := make([]string, turnctx.MaxHolds-1)
	for i := range seed {
		seed[i] = fmt.Sprintf("repo-%04d", i)
	}
	h := turnctx.NewHolds(seed)
	if err := h.Take(t.Context(), seed[7]); err != nil || len(h.IDs()) != len(seed) {
		t.Errorf("a held key = %v, the set %d long, want no new hold and no error", err, len(h.IDs()))
	}
	if err := h.Take(t.Context(), "repo-x", "repo-y"); !errors.Is(err, turnctx.ErrHoldsFull) {
		t.Errorf("a call of two new keys at one below the bound = %v, want ErrHoldsFull", err)
	}
	if got := h.IDs(); len(got) != len(seed) {
		t.Errorf("a refused call added %v", got[len(seed):])
	}
	if err := h.Take(t.Context(), "repo-x", seed[3]); err != nil || len(h.IDs()) != turnctx.MaxHolds {
		t.Errorf("a call whose one new key fits = %v, the set %d long, want it taken", err, len(h.IDs()))
	}
	if err := h.Take(t.Context(), seed[7]); err != nil {
		t.Errorf("a held key at the bound = %v, want no new hold and no error", err)
	}
	if err := h.Take(t.Context(), "repo-past"); !errors.Is(err, turnctx.ErrHoldsFull) {
		t.Errorf("a Take past the bound = %v, want ErrHoldsFull", err)
	}
	var none *turnctx.Holds
	if err := none.Take(t.Context(), "repo-a"); !errors.Is(err, turnctx.ErrNoHolds) {
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
	if err := h.Take(t.Context(), "taken"); err != nil {
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
