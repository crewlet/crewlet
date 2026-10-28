package turnctx

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// Holds is what a turn has named that an expiry could take from under it — the
// scratch repositories it reached, by id — and where the turn's hold on each
// is kept. A key is OPAQUE here: this package never decodes one, so whatever
// later kinds of hold a turn takes ride the same set, row and reader.
//
// TAKEN BEFORE THE REQUEST: whatever takes a hold — the code tools' one seam,
// which every request a turn makes about a repository passes — takes it here
// BEFORE that request is sent, so no request reaches a repository the expiry
// could already be deciding about unheld.
//
// KEPT WHERE THE TURN LIVES: in this value, in the memory of the process
// running the turn, which that node answers for when an expiry asks it (the
// engine's crewlet.code.holds); and, once the turn has a run row, ALSO on the
// row (sandbox.PendingRun.CodeHolds), through the writer [Holds.Attach]
// installs, because a detached run outlives this process. Memory is the right
// home for a running turn's hold — the turn lives only in this process, and a
// durable copy would need a rule to release the hold of a turn whose process
// crashed — and the row the right home for a parked one, which nothing else
// remembers.
//
// IT ONLY GROWS AND AUTHORIZES NOTHING, Turn's rule for what a Turn points at:
// a hold keeps a repository from an expiry and decides no access. Nothing is
// released before the turn ends: a run that ends deletes its row
// (internal/sandbox), and a turn that returns leaves the engine's registry of
// running turns, which is the whole release. Every Turn derived from a turn
// shares its set, so a delegated worker's hold is its parent's.
//
// THE ZERO VALUE IS NOT USED: [NewHolds] builds one. A nil *Holds holds
// nothing, and a Take on it is refused rather than dropped — a seam that took
// a hold it could not keep would send a request the expiry does not know of.
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
	// write is the attached writer — a compare-and-set of new keys onto the
	// turn's run row — or nil where the turn has no row (yet).
	write func(ctx context.Context, keys []string) error
	// sealed is closed while no seal is in force, and open while a launch is
	// between reading the set and its row existing: a Take then waits on it.
	sealed chan struct{}
}

// MaxHolds bounds one turn's set: a Take past it is refused, naming the bound,
// and never a hold dropped. It is above config.CodeScratchMaxPerSeatCeiling
// (1 000), so a turn working through every scratch repository its seat may own
// is never refused for their number. On a run row a key is at most 39 bytes
// (a quoted id and its comma), so the set is at most about 39 KiB on a row
// that already carries the whole suspended conversation.
// sandbox.MaxCodeHolds is the same number (this package and that one import
// neither, and engine's TestATurnHoldsWhatItsRowMay holds the two equal).
const MaxHolds = 1024

// ErrHoldsFull is a Take past [MaxHolds]: the request is refused, naming the
// bound.
var ErrHoldsFull = errors.New("turnctx: this turn already holds the most repositories one turn may (1 024)")

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

// Take adds key to the set and answers whether it was NEW. A new key is
// written through the attached writer first, where there is one, and is NOT
// added where that write fails: the error is returned, and the request it was
// taken for is refused with nothing sent. While a launch's seal is in force,
// Take waits for it to end — [Holds.Attach] or [Holds.Unseal] — bounded by
// ctx, so a key taken in that window reaches the row the launch is opening.
func (h *Holds) Take(ctx context.Context, key string) (bool, error) {
	if h == nil {
		return false, ErrNoHolds
	}
	for {
		h.mu.Lock()
		if h.has[key] {
			h.mu.Unlock()
			return false, nil
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
				return false, ctx.Err()
			}
		}
		if len(h.keys) >= MaxHolds {
			h.mu.Unlock()
			return false, ErrHoldsFull
		}
		if h.write == nil {
			h.add(key)
			h.mu.Unlock()
			return true, nil
		}
		h.mu.Unlock()
		if taken, again, err := h.takeWritten(ctx, key); !again {
			return taken, err
		}
	}
}

// takeWritten takes key through the attached writer: WRITTEN BEFORE IT IS
// HELD, one write at a time, so the set never holds a key the row was refused
// — the row is the set's durable copy, and a key it does not hold is no hold
// at all once this process is gone. again reports a set that changed under
// the wait for the write's turn — a seal in force, the writer detached, the
// set at its bound — which Take decides afresh.
func (h *Holds) takeWritten(ctx context.Context, key string) (taken, again bool, err error) {
	h.writing.Lock()
	defer h.writing.Unlock()
	h.mu.Lock()
	write := h.write
	switch {
	case h.has[key]:
		h.mu.Unlock()
		return false, false, nil
	case write == nil || len(h.keys) >= MaxHolds:
		h.mu.Unlock()
		return false, true, nil
	}
	h.mu.Unlock()
	if err := write(ctx, []string{key}); err != nil {
		return false, false, err
	}
	h.mu.Lock()
	h.add(key)
	h.mu.Unlock()
	return true, false, nil
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
