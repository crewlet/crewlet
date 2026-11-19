package turnctx

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// Holds is what a turn has named that Gitlet could delete on nobody's request
// (scratch repositories by id, seat branches by gitlet.RefHoldKey) and where
// the hold on each is kept. Keys are OPAQUE: this package never decodes one.
//
// Take runs BEFORE the request it covers, so none reaches a repository the
// expiry could already be deciding about unheld; one call is one write through
// the attached writer. A launch takes no key (ADR-0093); it seals the set it
// reads onto the row it opens ([Holds.Seal]). Kept in memory and, once a run
// row exists, on it (sandbox.PendingRun.CodeHolds) for a detached run. The set
// only grows and authorizes nothing; the turn ending is the whole release.
// Use [NewHolds]; a Take on a nil *Holds is refused, not dropped.
type Holds struct {
	// writing serializes the writes to the row, so the set and the row hold
	// the same keys whatever order two calls of one turn take them in. Never
	// held by a reader: the expiry's question ([Holds.IDs]) is answered from
	// mu alone while a write is in flight.
	writing sync.Mutex
	mu      sync.Mutex
	// keys is the set, in the order each was first taken, and has its index.
	keys []string
	has  map[string]bool
	// confirmed is the keys of the set this process has confirmed
	// ([Holds.Confirm]): never written to the row, never seeded.
	confirmed map[string]bool
	// write is the attached writer — a compare-and-set of new keys onto the
	// turn's run row — or nil where the turn has no row (yet).
	write func(ctx context.Context, keys []string) error
	// sealed is closed while no seal is in force, and open while a launch is
	// between reading the set and its row existing: a Take then waits on it.
	sealed chan struct{}
}

// MaxHolds bounds one turn's set (scratch repositories and seat branches
// together): a Take past it is refused, naming the bound, never a hold dropped.
// It is above config.CodeScratchMaxPerSeatCeiling (1 000), so a turn using every
// scratch repository its seat may own is not refused for their number alone. A
// JUDGMENT CALL (M3-decisions M3-O61): a turn naming more is past anything its
// work needs. A key is at most 72 bytes on the run row, so the set is about
// 72 KiB, the largest field the row keeps. sandbox.MaxCodeHolds is the same
// number (neither package imports the other; engine's
// TestATurnHoldsWhatItsRowMay holds them equal).
const MaxHolds = 1024

// ErrHoldsFull is a Take past [MaxHolds]: the request is refused, naming the
// bound.
var ErrHoldsFull = errors.New("turnctx: this turn already holds the most repositories and branches one " +
	"turn may (1 024)")

// ErrNoHolds is a Take on a nil *Holds: a turn built outside the engine, which
// keeps no hold anywhere an expiry asks.
var ErrNoHolds = errors.New("turnctx: this turn keeps no holds, so it reaches no repository an expiry could take")

// NewHolds starts a set from seed — a resumed turn's row's CodeHolds, or none —
// with no writer and no seal.
func NewHolds(seed []string) *Holds {
	h := &Holds{has: map[string]bool{}, sealed: closed()}
	for _, key := range seed {
		if !h.has[key] {
			h.has[key] = true
			h.keys = append(h.keys, key)
		}
	}
	return h
}

// closed is a channel no wait blocks on.
func closed() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}

// Take adds keys to the set, every one it does not hold yet in ONE write
// through the attached writer where there is one — a call's every key in one
// compare-and-set on its run row — and NONE of them where that write fails:
// the error is returned, and the request they were taken for is refused with
// nothing sent. The bound ([MaxHolds]) is judged over the whole call: a Take
// that would carry the set past it adds none of its keys. While a launch's
// seal is in force, Take waits for it to end — [Holds.Attach] or
// [Holds.Unseal] — bounded by ctx, so a key taken in that window reaches the
// row the launch is opening.
func (h *Holds) Take(ctx context.Context, keys ...string) error {
	if h == nil {
		return ErrNoHolds
	}
	for {
		h.mu.Lock()
		fresh := h.needs(keys)
		if len(fresh) == 0 {
			h.mu.Unlock()
			return nil
		}
		seal := h.sealed
		select {
		case <-seal:
		default:
			// SEALED: the launch has read the set and its row does not exist
			// yet, so a key added now would be on neither — wait for the row
			// (or for the launch to give up), then take it.
			h.mu.Unlock()
			select {
			case <-seal:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if len(h.keys)+len(fresh) > MaxHolds {
			h.mu.Unlock()
			return ErrHoldsFull
		}
		if h.write == nil {
			// IN MEMORY: no row yet — the launch that opens one writes the
			// set it reads onto it ([Holds.Seal]).
			for _, key := range fresh {
				h.add(key)
			}
			h.mu.Unlock()
			return nil
		}
		h.mu.Unlock()
		if again, err := h.takeWritten(ctx, keys); !again {
			return err
		}
	}
}

// needs is the keys a Take of keys must add: the ones the set does not hold,
// once each in their first order. Called under mu.
func (h *Holds) needs(keys []string) (fresh []string) {
	for i, key := range keys {
		if !h.has[key] && !slices.Contains(keys[:i], key) {
			fresh = append(fresh, key)
		}
	}
	return fresh
}

// takeWritten takes keys through the attached writer: WRITTEN BEFORE THEY ARE
// HELD, in one write, one write at a time, so the set never holds a key the
// row was refused — the row is the set's durable copy, and a key it does not
// hold is no hold at all once this process is gone. again reports a set that
// changed under the wait for the write's turn — a seal in force, the writer
// detached, the set at its bound — which Take decides afresh.
func (h *Holds) takeWritten(ctx context.Context, keys []string) (again bool, err error) {
	h.writing.Lock()
	defer h.writing.Unlock()
	h.mu.Lock()
	write := h.write
	fresh := h.needs(keys)
	switch {
	case len(fresh) == 0:
		h.mu.Unlock()
		return false, nil
	case write == nil || len(h.keys)+len(fresh) > MaxHolds:
		h.mu.Unlock()
		return true, nil
	}
	h.mu.Unlock()
	if err := write(ctx, fresh); err != nil {
		return false, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, key := range fresh {
		// A Take while no writer was attached (a Detach, then this write's
		// turn) may have added it meanwhile: the set holds each key once.
		if !h.has[key] {
			h.add(key)
		}
	}
	return false, nil
}

// add puts key in the set. Called under mu.
func (h *Holds) add(key string) {
	h.has[key] = true
	h.keys = append(h.keys, key)
}

// Seal answers the set and, where no writer is attached, makes every Take
// wait: a launch's window, from reading the set — it becomes the run row's —
// to the row existing ([Holds.Attach]) or the launch failing first
// ([Holds.Unseal]). With a writer attached Seal only answers the set: the row
// exists, and the launch's reset unions what it is handed with what the row
// holds.
func (h *Holds) Seal() []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.write == nil {
		select {
		case <-h.sealed:
			h.sealed = make(chan struct{})
		default:
		}
	}
	return slices.Clone(h.keys)
}

// Attach installs write — a compare-and-set of new keys onto the turn's run
// row — and ends a seal: called by a launch's Opened, once the row exists and
// before any box does, and for a turn the engine builds from a row it already
// has (a resumed one, a bridged run's session).
func (h *Holds) Attach(write func(ctx context.Context, keys []string) error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.write = write
	h.unseal()
}

// Unseal ends a seal with no writer: the launch failed before any row was
// opened, and the turn runs on, holding in memory.
func (h *Holds) Unseal() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unseal()
}

// unseal closes a seal in force. Called under mu.
func (h *Holds) unseal() {
	select {
	case <-h.sealed:
	default:
		close(h.sealed)
	}
}

// Detach removes the writer and ends a seal: the launch's job failed after
// its row opened — the coordinator abandons that row — and the turn runs on,
// holding in memory.
func (h *Holds) Detach() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.write = nil
	h.unseal()
}

// IDs answers the set, in the order each key was first taken: what this
// node answers an expiry with for a running turn.
func (h *Holds) IDs() []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.keys)
}

// Confirmed reports whether THIS PROCESS has confirmed the turn's hold on key:
// a seam (internal/agent/builtin's HoldRef and HoldRepo) took it and found,
// from a read, the hold ordered against every deletion in flight at that Take.
// Its seam then sends nothing more for the turn: later readers see the key
// (a set only grows) and earlier ones were ordered by the confirmation
// (M3-decisions M3-O65).
//
// IN MEMORY ONLY: the run row also carries keys taken unconfirmed, so vouching
// from it would cost a coordination write per key to save one read per resumed
// turn. [NewHolds] confirms none of its seed; a resumed turn confirms again.
func (h *Holds) Confirmed(key string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.confirmed[key]
}

// Confirm marks key confirmed and answers true; a key not in the set is not
// marked, and answers false — a hold nothing took cannot have been ordered by
// its Take. It writes nothing through the writer [Holds.Attach] installed.
func (h *Holds) Confirm(key string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.has[key] {
		return false
	}
	if h.confirmed == nil {
		h.confirmed = map[string]bool{}
	}
	h.confirmed[key] = true
	return true
}

// Unconfirm clears key's mark: its seam ended in an error after its Take — a
// record refused, answered unknown or unserved — so a mark another call of the
// same turn set meanwhile (two delegated workers at once) never outlives the
// failure. That costs at most one more read, the safe direction.
func (h *Holds) Unconfirm(key string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.confirmed, key)
}
