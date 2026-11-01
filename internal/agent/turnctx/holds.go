package turnctx

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// Holds is what a turn has named that the code host could delete from under it
// on nobody's request — the scratch repositories it reached, by id, which
// expire idle, and the seats' branches it read, wrote or launched on, each by
// its code.RefHoldKey — and where the turn's hold on each is kept. A key is
// OPAQUE here: this package never decodes one, so every kind of hold a turn
// takes rides the same set, row and reader.
//
// CONFIRMED ONCE A TURN AND PROCESS: beside the set, which keys THIS PROCESS
// has confirmed for the turn ([Holds.Confirm]) — a seam took the hold here
// having read what it holds, and found it ordered against every deletion in
// flight at that Take — so a confirmed key's seam sends nothing more for the
// rest of the turn. In memory only (see [Holds.Confirmed] for why).
//
// TAKEN BEFORE THE REQUEST: whatever takes a hold — the code tools' one seam,
// which every request a turn makes about a repository passes — takes it here
// BEFORE that request is sent, so no request reaches a repository the expiry
// could already be deciding about unheld.
//
// ONE WRITE A CALL, NOT ONE A KEY: [Holds.Take] takes every key it is handed
// in one write through the attached writer — a call that resolves several
// seats' branches (a comparison's base and head, a change's source and
// target) pays one compare-and-set on its run row, not one each — and a
// launch's resolution of its repositories takes its keys for the launch's
// own row write ([Holds.ForLaunch]), so a relaunch's holds ride the
// BeginLaunch it makes anyway and write nothing of their own (M3-boxes
// §10.6.5).
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
	// owed is the keys of the set the attached row does not hold yet: taken
	// by a launch's resolution ([Holds.ForLaunch]) for the row write that
	// launch is about to make. A Take naming one outside that resolution
	// writes it first, since its request may outlive the launch; the
	// launch's Attach settles the ones its row write carried (carried).
	owed map[string]bool
	// carried is the owed keys a launch read with the set ([Holds.Seal]),
	// which its row write unions onto the row: settled by its Attach,
	// forgotten by its Unseal or Detach.
	carried []string
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

// MaxHolds bounds one turn's set — its scratch repositories' and its seat
// branches' keys together: a Take past it is refused, naming the bound, and
// never a hold dropped. It is above config.CodeScratchMaxPerSeatCeiling
// (1 000), so a turn working through every scratch repository its seat may own
// is never refused for their number alone; such a turn meets it past 24 seat
// branches besides, and one naming no scratch repository past 1 024 seat
// branches — a JUDGMENT CALL (M3-decisions M3-O61): a turn naming that many is
// past anything a turn's work needs, and is refused naming the bound rather
// than holding a row past what a run row should carry. On a run row a key is
// at most 72 bytes (a ref's key, 69, quoted and with its comma; a
// repository's id 39), so the set is at most about 72 KiB: the largest field
// the row keeps, since its conversation and tool log are beside it rather
// than on it (internal/sandbox). sandbox.MaxCodeHolds is
// the same number (this package and that one import neither, and engine's
// TestATurnHoldsWhatItsRowMay holds the two equal).
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

// launchKey is the context key [Holds.ForLaunch] sets: the set whose Takes
// under that context are a launch's.
type launchKey struct{}

// ForLaunch is ctx for a launch's resolution of the repositories its run is
// granted (internal/engine's resolveRunRepos): a Take on h under it adds its
// new keys to the set WITHOUT writing them through the attached row's writer,
// marked owed, because the launch writes the set it reads ([Holds.Seal]) on
// its row as it opens it — BeginLaunch unions it with what the row holds — and
// its Attach settles them (M3-boxes §10.6.5: a launch's branches ride the
// BeginLaunch it already makes). Without it a resumed executor's relaunch, its
// row's writer attached, wrote one compare-and-set per key it resolved and
// then the same keys again in BeginLaunch.
//
// SAFE BECAUSE THE LAUNCH'S REQUEST IS ITS BOX, sent only once the row holds
// the set: what the resolution sends before that — its reads, a hold's record
// — is the turn's own while it runs, and the running turn is asked from memory
// (an owed key is in [Holds.IDs]), exactly as a first launch, which has no row
// to write to, holds its resolution's keys. And an owed key never goes
// unwritten behind ANOTHER request: a Take outside the resolution naming it
// writes it first, and a launch that fails before its row write leaves it owed
// for the next.
//
// Only a launch that writes the set it reads on its row may resolve under it.
func (h *Holds) ForLaunch(ctx context.Context) context.Context {
	if h == nil {
		return ctx
	}
	return context.WithValue(ctx, launchKey{}, h)
}

// launching reports whether ctx is a launch's resolution on h ([Holds.ForLaunch]).
func (h *Holds) launching(ctx context.Context) bool {
	set, _ := ctx.Value(launchKey{}).(*Holds)
	return set == h
}

// Take adds keys to the set, every one it does not hold yet in ONE write
// through the attached writer where there is one — a call's every key in one
// compare-and-set on its run row — and NONE of them where that write fails:
// the error is returned, and the request they were taken for is refused with
// nothing sent. The bound ([MaxHolds]) is judged over the whole call: a Take
// that would carry the set past it adds none of its keys. A key the set holds
// owed ([Holds.ForLaunch]) is written in the same write, outside a launch's
// resolution. While a launch's seal is in force, Take waits for it to end —
// [Holds.Attach] or [Holds.Unseal] — bounded by ctx, so a key taken in that
// window reaches the row the launch is opening.
func (h *Holds) Take(ctx context.Context, keys ...string) error {
	if h == nil {
		return ErrNoHolds
	}
	launch := h.launching(ctx)
	for {
		h.mu.Lock()
		fresh, owed := h.needs(keys, launch)
		if len(fresh)+len(owed) == 0 {
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
		if h.write == nil || launch {
			// IN MEMORY: no row yet, or a launch's resolution, whose keys
			// its own row write carries — owed to the row until it does.
			for _, key := range fresh {
				h.add(key)
				if h.write != nil {
					h.owe(key)
				}
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

// needs is what a Take of keys must do: the keys the set does not hold, once
// each in their first order, and — outside a launch's resolution, with a
// writer attached — the keys it holds owed, which the Take's write carries.
// Called under mu.
func (h *Holds) needs(keys []string, launch bool) (fresh, owed []string) {
	for i, key := range keys {
		switch {
		case slices.Contains(keys[:i], key):
		case !h.has[key]:
			fresh = append(fresh, key)
		case h.owed[key] && h.write != nil && !launch:
			owed = append(owed, key)
		}
	}
	return fresh, owed
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
	fresh, owed := h.needs(keys, false)
	switch {
	case len(fresh)+len(owed) == 0:
		h.mu.Unlock()
		return false, nil
	case write == nil || len(h.keys)+len(fresh) > MaxHolds:
		h.mu.Unlock()
		return true, nil
	}
	h.mu.Unlock()
	written := append(fresh, owed...)
	if err := write(ctx, written); err != nil {
		return false, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, key := range fresh {
		// A launch's resolution may have taken it, owed, while the write
		// was in flight: the write carried it, so it is held and settled.
		if !h.has[key] {
			h.add(key)
		}
	}
	for _, key := range written {
		delete(h.owed, key)
	}
	return false, nil
}

// add puts key in the set. Called under mu.
func (h *Holds) add(key string) {
	h.has[key] = true
	h.keys = append(h.keys, key)
}

// owe marks key owed to the attached row. Called under mu.
func (h *Holds) owe(key string) {
	if h.owed == nil {
		h.owed = map[string]bool{}
	}
	h.owed[key] = true
}

// Seal answers the set and, where no writer is attached, makes every Take
// wait: a launch's window, from reading the set — it becomes the run row's —
// to the row existing ([Holds.Attach]) or the launch failing first
// ([Holds.Unseal]). With a writer attached Seal only answers the set: the row
// exists, and the launch's reset unions what it is handed with what the row
// holds — the keys owed to it among them ([Holds.ForLaunch]), which the
// launch's Attach then settles.
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
	h.carried = nil
	for _, key := range h.keys {
		if h.owed[key] {
			h.carried = append(h.carried, key)
		}
	}
	return slices.Clone(h.keys)
}

// Attach installs write — a compare-and-set of new keys onto the turn's run
// row — and ends a seal: called by a launch's Opened, once the row exists and
// before any box does, and for a turn the engine builds from a row it already
// has (a resumed one, a bridged run's session). A launch's Attach settles the
// owed keys its Seal read, which its row write carried.
func (h *Holds) Attach(write func(ctx context.Context, keys []string) error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.write = write
	for _, key := range h.carried {
		delete(h.owed, key)
	}
	h.carried = nil
	h.unseal()
}

// Unseal ends a seal and opens no row: the launch failed before any row was
// opened, and the turn runs on — holding in memory, or with the row it
// already had, which still owes what the launch's resolution took
// ([Holds.ForLaunch]).
func (h *Holds) Unseal() {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.carried = nil
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
	// NO ROW IS ATTACHED, so nothing is owed to one: the next launch writes
	// the whole set on the row it opens.
	h.owed, h.carried = nil, nil
	h.unseal()
}

// IDs answers the set, in the order each key was first taken — the keys owed
// to the row included ([Holds.ForLaunch]): what this node answers an expiry
// with for a running turn.
func (h *Holds) IDs() []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.keys)
}

// Confirmed reports whether THIS PROCESS has confirmed the turn's hold on key:
// a seam (internal/agent/builtin's HoldRef and HoldRepo) took it here and
// found, from a read of what it holds, the hold ordered against every deletion
// in flight at that Take — no record needed, or the record applied. A
// confirmed key's seam sends nothing more for the rest of the turn: every
// later reader of the holds sees the key, since a set only grows, and every
// earlier one was ordered by the confirmation (M3-decisions M3-O65).
//
// IN MEMORY ONLY: never on the run row, which carries keys taken unconfirmed
// too — a hold with no read, a hold whose record failed after its Take — so it
// could vouch for a confirmation only through a field every confirmation
// writes, a coordination write per key to save one read per resumed turn.
// So [NewHolds] confirms none of its seed, and a resumed turn, or the same
// turn in any other process, confirms again.
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
