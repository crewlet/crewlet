package upkeep

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	"github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/objstore/transfer"
)

// PendingGrace is how long a chunk nothing refers to is kept.
//
// A DAY. Bytes are uploaded before the record naming them is written, so every
// chunk starts life unreferenced; the grace has to outlast the slowest upload
// from its first chunk to its record, which is minutes, and it costs nothing
// but a day's worth of abandoned uploads on disk. It is not what covers a node
// whose estate is behind — the pass's barrier is — so it need not be sized
// for a node that was down.
const PendingGrace = 24 * time.Hour

// RepairInterval is how often a data node checks it holds what it should.
//
// TEN MINUTES, the map's own [OutGrace]: a member taken out of the map leaves
// its groups one copy short until their new holders repair them, so a repair
// that ran less often than members can be taken out would let that window
// grow past the one the grace already accepted. A map change also starts a
// pass at once, and a pass that left work a retry may do is retried sooner,
// so this is the ceiling on noticing a chunk lost from this disk rather than
// on following the map.
const RepairInterval = OutGrace

// CollectInterval is how often a data node deletes what nothing needs it to
// hold.
//
// AN HOUR: the pass waits on a barrier in every domain that refers to chunks
// and walks the whole disk, and the only cost of running it less often is
// garbage kept a little longer beside a grace that is already a day.
//
// IT IS NOT WHAT A MAP CHANGE WAITS ON. The copies an epoch moved away from a
// node — a member taken out holds its whole share of them — can go only once
// the members the map now places them on hold theirs, which is the moment the
// fleet is [Settled] at that epoch; the caller runs one collection then, on
// every node, rather than leaving a decommission to wait out the hour.
const CollectInterval = time.Hour

// fetchConcurrency is how many chunks one repair pass copies at once.
//
// FOUR, a RECOVERY THROTTLE in the sense of Ceph's osd_recovery_max_active
// (three on a spinning disk there): a copy is a round trip and a mebibyte, so
// four overlap the round trips, while a node that has just been handed a large
// share — a new member, or every member after a split — cannot saturate the
// broker that every upload and read in the fleet also rides. Recovery that
// starves serving turns one node's repair into everybody's outage.
const fetchConcurrency = 4

// readSlots is the most slots one read of the references covers.
//
// THE SLOTS OF ONE GROUP AT THE FEWEST GROUP BITS, a 256th of the corpus: a
// pass holds one read's chunks in memory at a time, and a node that holds
// every group — a fleet of fewer members than copies — would otherwise read
// the company's whole reference list at once. It is also one directory of the
// disk, so a collection walks and reads the same span.
const readSlots = placement.Slots >> placement.MinPGBits

// Chunks is this node's own chunk store.
type Chunks interface {
	Put(h objstore.Hash, data []byte) error
	Has(h objstore.Hash) bool
	Delete(h objstore.Hash) error
	Collect(h objstore.Hash, before time.Time) (bool, error)
	Verify(h objstore.Hash) (bool, error)
	WalkSlots(lo, hi int, visit func(disk.Held) error) error

	// Root is the store's directory, which holds the scrub's cursor.
	Root() string
}

// Peers is how this node reaches the others.
type Peers interface {
	Fetch(ctx context.Context, h objstore.Hash) ([]byte, error)
	Has(ctx context.Context, node string, hashes []objstore.Hash, verify bool) (transfer.Holding, error)
}

// Layouts answers the layout of the map this node places by, and false while
// it has none.
type Layouts func() (*placement.Layout, bool)

// NodeOptions are one data node's passes' dependencies.
type NodeOptions struct {
	Self       string
	Local      Chunks
	Peers      Peers
	Layouts    Layouts
	References References

	// Now is the clock the collector's grace is measured on and the
	// scrub is paced by, injected for tests.
	Now func() time.Time
}

// Node runs one data node's repair, collection and scrub.
type Node struct {
	opts NodeOptions

	// wait sleeps for d or until ctx ends, reporting whether it slept the
	// whole of it: a timer, but for the tests.
	wait func(ctx context.Context, d time.Duration) bool

	// scrubbing is held by the one scrub a node runs: two would share one
	// cursor file and each verify what the other had.
	scrubbing sync.Mutex

	mu     sync.Mutex
	status Status
}

// Status is what the last passes found, for the node's lease, its gauges and
// its alarms.
type Status struct {
	// Repair is the last repair pass, and Repaired the last one that
	// reached every group — zero before one has. They differ after a pass
	// that stopped short: what that pass saw is not a claim about the
	// node, and what the last complete one found still is.
	Repair, Repaired RepairStatus

	// Collect is the last collection pass, and Collected the last one
	// that walked every slot — zero before one has. They differ after a
	// pass that stopped short, whose counts cover only the slots it
	// reached: its Strays is not what this node holds, and a zero from it
	// would tell an operator a member taken out is empty when most of its
	// disk was never looked at.
	Collect, Collected CollectStatus

	Scrub ScrubStatus
}

// RepairStatus is what the last repair pass found.
type RepairStatus struct {
	// Epoch is the map epoch the pass placed by.
	Epoch uint64 `json:"epoch"`

	// Completed is whether the pass reached every group this node holds.
	// The counts below cover the groups it reached.
	Completed bool `json:"completed"`

	// Placed is how many referenced chunks the map places on this node,
	// and Held how many of them it held when the pass ended; Pending is
	// the difference — what this node should hold and does not.
	Placed  int `json:"placed"`
	Held    int `json:"held"`
	Pending int `json:"pending"`

	// Fetched is how many the pass copied here. Unreachable is how many it
	// could not, because a member that may hold one did not answer or
	// could not read it; Missing how many every member ANSWERED it does
	// not hold — the only count that is a lost chunk.
	Fetched     int `json:"fetched"`
	Unreachable int `json:"unreachable"`
	Missing     int `json:"missing"`

	// At is when the pass ended, and Error why it did not finish cleanly.
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// Report is what the map's maintainer reads from a pass: its epoch and what it
// left pending — and the zero report, which no map's epoch matches, for a pass
// that did not reach every group, since a node that has not looked at all of
// its groups cannot say it holds them.
func (r RepairStatus) Report() RepairReport {
	if !r.Completed {
		return RepairReport{}
	}
	return RepairReport{Epoch: r.Epoch, Pending: r.Pending}
}

// CollectStatus is what the last collection pass found.
type CollectStatus struct {
	// Epoch is the map epoch the pass placed by.
	Epoch uint64 `json:"epoch"`

	// Completed is whether the pass walked every slot. The counts below
	// cover the slots it reached.
	Completed bool `json:"completed"`

	// Deleted is how many chunks nothing refers to it removed.
	Deleted int `json:"deleted"`

	// Dropped is how many referenced copies beyond this node's placement
	// it removed, once every member placing each had confirmed an intact
	// copy of its own; Strays how many such copies it still holds,
	// awaiting that confirmation. A node being emptied is done when a
	// completed pass at the map's current epoch counts zero strays.
	Dropped int `json:"dropped"`
	Strays  int `json:"strays"`

	// Skipped says why the pass deleted nothing unreferenced, or kept
	// every stray, empty when it ran in full.
	Skipped string `json:"skipped,omitempty"`

	// At is when the pass ended, and Error what stopped it.
	At    time.Time `json:"at"`
	Error string    `json:"error,omitempty"`
}

// NewNode builds a node's passes.
func NewNode(opts NodeOptions) (*Node, error) {
	if opts.Self == "" || opts.Local == nil || opts.Peers == nil || opts.Layouts == nil {
		return nil, errors.New("objstore/upkeep: the passes need this node, its chunks, its peers and a map")
	}
	if len(opts.References) == 0 {
		// REFUSED, never run with nothing: a collector with no source
		// reads every chunk on the disk as unreferenced and deletes the
		// lot a day later.
		return nil, errors.New("objstore/upkeep: the passes need at least one source of references")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Node{opts: opts, wait: sleep}, nil
}

// sleep waits for d or until ctx ends, reporting whether it waited all of it.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Status is what the last passes found.
func (n *Node) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.status
}

// Repair copies here every referenced chunk the map places here and this
// node does not hold.
//
// PINNED FIRST, on the same barrier collection takes: a node that has just
// come back reads an estate at least as new as each log's tip when the pass
// began, so it repairs what was written while it was away rather than what it
// last saw — and a pass that reported itself complete while missing a day of
// writes is what lets the map split groups whose copies are not all there.
//
// It answers an error when it left work a retry may do: it did not reach every
// group, a chunk would not store here, or a member that may hold a chunk could
// not be asked. A chunk every member answered it does not hold is not an
// error — nothing a retry does can find it — and is counted Missing.
func (n *Node) Repair(ctx context.Context) error {
	l, ok := n.opts.Layouts()
	if !ok {
		return nil
	}
	st := RepairStatus{Epoch: l.Map().Epoch}
	err := n.repair(ctx, l, &st)
	st.Pending = st.Placed - st.Held
	st.At = n.opts.Now().UTC()
	if err != nil {
		st.Error = err.Error()
	}
	n.mu.Lock()
	n.status.Repair = st
	if st.Completed {
		n.status.Repaired = st
	}
	n.mu.Unlock()
	if st.Fetched > 0 || st.Pending > 0 || err != nil {
		log.InfoContext(ctx, "object_repair", "epoch", st.Epoch, "completed", st.Completed,
			"placed", st.Placed, "held", st.Held, "pending", st.Pending, "fetched", st.Fetched,
			"unreachable", st.Unreachable, "missing", st.Missing, "error", st.Error)
	}
	return err
}

func (n *Node) repair(ctx context.Context, l *placement.Layout, st *RepairStatus) error {
	v, err := n.opts.References.pin(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, run := range placedOn(l, n.opts.Self) {
		if err := ctx.Err(); err != nil {
			return err
		}
		refs, _, err := n.opts.References.referenced(ctx, run.Lo, run.Hi, v)
		if err != nil {
			return err
		}
		var want []objstore.Hash
		for h := range refs {
			st.Placed++
			if n.opts.Local.Has(h) {
				st.Held++
			} else {
				want = append(want, h)
			}
		}
		got := n.fetch(ctx, want)
		st.Fetched += got.fetched
		st.Held += got.fetched
		st.Missing += got.missing
		st.Unreachable += got.unreachable
		failures = append(failures, got.failures...)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	st.Completed = true
	if st.Unreachable > 0 {
		failures = append(failures, fmt.Errorf("objstore/upkeep: %d chunks placed here could "+
			"not be fetched: a member that may hold them did not answer or could not read them",
			st.Unreachable))
	}
	return errors.Join(failures...)
}

// placedOn is every run of slots the layout places on node, in slot order:
// adjacent groups merged, but never across a [readSlots] boundary, so each
// run is one bounded read.
func placedOn(l *placement.Layout, node string) []placement.Range {
	m := l.Map()
	var out []placement.Range
	for pg := range m.Groups() {
		if !slices.Contains(l.Up(pg), node) {
			continue
		}
		lo, hi := m.SlotRange(pg)
		if n := len(out); n > 0 && out[n-1].Hi == lo && lo%readSlots != 0 {
			out[n-1].Hi = hi
			continue
		}
		out = append(out, placement.Range{Lo: lo, Hi: hi})
	}
	return out
}

// fetched is what one batch of fetches did.
type fetched struct {
	fetched, missing, unreachable int
	failures                      []error
}

// fetch copies chunks here, a few at a time ([fetchConcurrency]). A chunk no
// member holds is counted missing and one no member could be asked about
// unreachable, rather than failed: nothing this node does can find the first,
// and the second is a peer's to answer. What fails is storing it here.
func (n *Node) fetch(ctx context.Context, want []objstore.Hash) fetched {
	var (
		mu  sync.Mutex
		out fetched
		wg  sync.WaitGroup
	)
	slots := make(chan struct{}, fetchConcurrency)
	for _, h := range want {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return out
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			data, err := n.opts.Peers.Fetch(ctx, h)
			if err != nil {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case errors.Is(err, transfer.ErrNotFound):
					out.missing++
				case ctx.Err() == nil:
					out.unreachable++
				}
				return
			}
			err = n.opts.Local.Put(h, data)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				out.failures = append(out.failures, fmt.Errorf("objstore/upkeep: store %s here: %w", h, err))
				return
			}
			out.fetched++
		}()
	}
	wg.Wait()
	return out
}

// Collect deletes what nothing needs this node to hold: chunks no row refers
// to, past their grace, and referenced copies beyond this node's placement
// once the members that place them have confirmed theirs. See the package
// doc for why each rule is safe.
func (n *Node) Collect(ctx context.Context) error {
	l, ok := n.opts.Layouts()
	if !ok {
		// NO MAP, NO DELETION: without one this node cannot say what it
		// should hold, and "nothing" is the one answer that would empty
		// it.
		return nil
	}
	st := CollectStatus{Epoch: l.Map().Epoch}
	err := n.collect(ctx, l, &st)
	st.At = n.opts.Now().UTC()
	if err != nil {
		st.Error = err.Error()
	}
	n.mu.Lock()
	n.status.Collect = st
	if st.Completed {
		n.status.Collected = st
	}
	n.mu.Unlock()
	if st.Deleted > 0 || st.Dropped > 0 || st.Strays > 0 {
		log.InfoContext(ctx, "object_collect", "epoch", st.Epoch, "deleted", st.Deleted,
			"dropped", st.Dropped, "strays", st.Strays, "skipped", st.Skipped)
	}
	return err
}

func (n *Node) collect(ctx context.Context, l *placement.Layout, st *CollectStatus) error {
	v, err := n.opts.References.pin(ctx)
	if err != nil {
		st.Skipped = err.Error()
		return err
	}
	m := l.Map()
	keep, why := KeepsEveryCopy(m, n.opts.Self)
	if keep {
		st.Skipped = why
	}
	cutoff := n.opts.Now().Add(-PendingGrace)
	unconfirmable := map[string]bool{}
	for lo := 0; lo < placement.Slots; lo += readSlots {
		if err := ctx.Err(); err != nil {
			return err
		}
		hi := lo + readSlots
		var held []disk.Held
		if err := n.opts.Local.WalkSlots(lo, hi, func(h disk.Held) error {
			held = append(held, h)
			return nil
		}); err != nil {
			return fmt.Errorf("objstore/upkeep: walk slots [%d, %d): %w", lo, hi, err)
		}
		if len(held) == 0 {
			continue
		}
		refs, complete, err := n.opts.References.referenced(ctx, lo, hi, v)
		if err != nil {
			return err
		}
		beyond := map[int][]objstore.Hash{}
		for _, h := range held {
			if _, referenced := refs[h.Hash]; referenced {
				if pg := l.Group(h.Hash.Slot()); !slices.Contains(l.Up(pg), n.opts.Self) {
					beyond[pg] = append(beyond[pg], h.Hash)
				}
				continue
			}
			if !complete {
				// A RECORD THIS NODE COULD NOT APPLY may be the one naming
				// this chunk, so "unreferenced" is not a fact yet.
				st.Skipped = "a record this node could not apply may refer to chunks it holds"
				continue
			}
			// THE AGE IS JUDGED BY THE STORE, under the lock a writer's
			// touch takes: a copy judged here from the walk's reading
			// could have been written again since.
			removed, err := n.opts.Local.Collect(h.Hash, cutoff)
			if err != nil {
				return err
			}
			if removed {
				st.Deleted++
			}
		}
		for _, pg := range slices.Sorted(maps.Keys(beyond)) {
			strays := beyond[pg]
			if keep {
				st.Strays += len(strays)
				continue
			}
			dropped, err := n.dropBeyond(ctx, m.Epoch, l.Up(pg), strays, unconfirmable)
			if err != nil {
				return err
			}
			st.Dropped += dropped
			st.Strays += len(strays) - dropped
		}
	}
	st.Completed = true
	return nil
}

// KeepsEveryCopy reports whether a node keeps every referenced copy it holds
// under m — drops none beyond its placement, however well confirmed — and,
// when it does, why, in the words a collection's status carries. The
// collection asks it, and so does anything deciding whether a collection that
// kept strays is worth retrying: a node that keeps them BY RULE keeps them
// again on every retry.
//
// A NODE THE MAP DOES NOT HOLD keeps them. It has just come back and not yet
// been re-added, or it was removed: either way what it holds may be the copy
// the map's members have not repaired yet, and nobody has told it which
// groups it will hold again.
//
// NOR DOES A MEMBER ON PROBATION, unless an operator has also taken it out.
// It holds what it held when the map removed it, and the tick that trusts it
// gives it back most of the same groups — its draws are the ones it had, and
// the balance brings the shares back near where they were — so dropping them
// while it proves itself would be copying its whole share away only to copy
// most of it back a grace later. Taken out as well, it is never placed on
// again whatever its probation says, and it sheds its copies as any out member
// does.
func KeepsEveryCopy(m placement.Map, node string) (bool, string) {
	member, ok := m.Member(node)
	switch {
	case !ok:
		return true, "this node is not a member of the map, so it keeps every referenced copy it holds"
	case member.Probation && !member.Out:
		return true, "this node is on probation in the map, so it keeps every referenced copy it " +
			"holds for when the map places on it again"
	}
	return false, ""
}

// dropBeyond deletes the referenced copies this node holds beyond its
// placement that every member of the group's up set holds INTACT and places,
// at this node's own epoch, and answers how many it deleted.
//
// VERIFIED: each member reads its copy before vouching for it, so a copy that
// rotted can never be what lets a good one go. A member that did not answer,
// or answered by another map, confirms nothing for the rest of the pass
// (unconfirmable): every group it holds keeps its strays until the next pass,
// rather than every batch waiting it out again.
func (n *Node) dropBeyond(ctx context.Context, epoch uint64, up []string, strays []objstore.Hash,
	unconfirmable map[string]bool) (int, error) {

	// A GROUP PLACED ON NOBODY — a map whose every member is out — has no
	// member to confirm anything, and "every member confirmed" would be
	// vacuously true of it.
	if len(up) == 0 || slices.ContainsFunc(up, func(node string) bool { return unconfirmable[node] }) {
		return 0, nil
	}
	removed := 0
	for batch := range slices.Chunk(strays, transfer.MaxVerify) {
		confirmed := make([]bool, len(batch))
		for i := range confirmed {
			confirmed[i] = true
		}
		for _, member := range up {
			holding, err := n.opts.Peers.Has(ctx, member, batch, true)
			if err != nil || holding.Epoch != epoch {
				// A MEMBER THAT DID NOT ANSWER, or answered by another
				// map, confirms nothing: keep every copy and ask again
				// next pass.
				unconfirmable[member] = true
				return removed, nil
			}
			for i := range batch {
				confirmed[i] = confirmed[i] && holding.Held[i] && holding.Placed[i]
			}
		}
		for i, h := range batch {
			if !confirmed[i] {
				continue
			}
			if err := n.opts.Local.Delete(h); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}
