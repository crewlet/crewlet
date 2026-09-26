package upkeep

import (
	"context"
	"errors"
	"fmt"
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
// pass at once, so this is the ceiling on noticing a chunk that rotted rather
// than on following the map.
const RepairInterval = OutGrace

// CollectInterval is how often a data node deletes what nothing needs it to
// hold.
//
// AN HOUR: the pass waits on a barrier in every domain that refers to chunks
// and walks the whole disk, and the only cost of running it less often is
// garbage kept a little longer beside a grace that is already a day.
const CollectInterval = time.Hour

// fetchConcurrency is how many chunks one repair pass copies at once.
//
// FOUR: a copy is a round trip and a mebibyte, so four overlap the round
// trips without letting a node that has just been handed a large share
// saturate the broker every other request in the fleet also rides.
const fetchConcurrency = 4

// Chunks is this node's own chunk store.
type Chunks interface {
	Put(h objstore.Hash, data []byte) error
	Has(h objstore.Hash) bool
	Delete(h objstore.Hash) error
	Collect(h objstore.Hash, before time.Time) (bool, error)
	WalkGroup(pg int, visit func(disk.Held) error) error
}

// Peers is how this node reaches the others.
type Peers interface {
	Fetch(ctx context.Context, h objstore.Hash) ([]byte, error)
	Has(ctx context.Context, node string, hashes []objstore.Hash) (transfer.Holding, error)
}

// Maps answers the map this node places by.
type Maps func() (placement.Map, bool)

// NodeOptions are one data node's passes' dependencies.
type NodeOptions struct {
	Self       string
	Local      Chunks
	Peers      Peers
	Maps       Maps
	References References

	// Now is the clock the collector's grace is measured on, injected for
	// tests.
	Now func() time.Time
}

// Node runs one data node's repair and collection passes.
type Node struct {
	opts NodeOptions

	mu     sync.Mutex
	status Status
}

// Status is what the last passes found, for the node's gauges and alarm.
type Status struct {
	// Repaired is when the last repair pass finished, zero before one has.
	Repaired time.Time
	// Epoch is the map epoch the last repair pass placed by.
	Epoch uint64
	// Placed is how many referenced chunks the map places on this node.
	Placed int
	// Fetched is how many the last pass copied here.
	Fetched int
	// Missing is how many referenced chunks placed here no member could
	// supply — objects nobody can read in full until one comes back.
	Missing int

	// Collected is when the last collection pass finished.
	Collected time.Time
	// Deleted is how many unreferenced chunks it removed, and Strays how
	// many copies beyond this node's placement.
	Deleted, Strays int
	// Skipped says why the last collection pass deleted nothing
	// unreferenced, empty when it ran in full.
	Skipped string
}

// NewNode builds a node's passes.
func NewNode(opts NodeOptions) (*Node, error) {
	if opts.Self == "" || opts.Local == nil || opts.Peers == nil || opts.Maps == nil {
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
	return &Node{opts: opts}, nil
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
// FROM WHATEVER THIS NODE'S ESTATE SAYS, without a barrier: a view that is a
// little behind repairs a little less this pass and the rest next, and costs
// nothing that a barrier in every domain every ten minutes would.
func (n *Node) Repair(ctx context.Context) error {
	m, ok := n.opts.Maps()
	if !ok {
		return nil
	}
	placed, fetched, missing := 0, 0, 0
	var failures []error
	for pg := range placement.PGCount {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !slices.Contains(m.Up(pg), n.opts.Self) {
			continue
		}
		refs, _, err := n.opts.References.referenced(ctx, pg, nil)
		if err != nil {
			return err
		}
		var want []objstore.Hash
		for h := range refs {
			placed++
			if !n.opts.Local.Has(h) {
				want = append(want, h)
			}
		}
		got, lost, errs := n.fetch(ctx, want)
		fetched += got
		missing += lost
		failures = append(failures, errs...)
	}
	n.mu.Lock()
	n.status.Repaired, n.status.Epoch = n.opts.Now().UTC(), m.Epoch
	n.status.Placed, n.status.Fetched, n.status.Missing = placed, fetched, missing
	n.mu.Unlock()
	if fetched > 0 || missing > 0 {
		log.InfoContext(ctx, "object_repair", "epoch", m.Epoch, "placed", placed,
			"fetched", fetched, "missing", missing)
	}
	return errors.Join(failures...)
}

// fetch copies chunks here, a few at a time. A chunk no member holds is
// counted missing rather than failed: nothing this node does can find it.
func (n *Node) fetch(ctx context.Context, want []objstore.Hash) (int, int, []error) {
	var (
		mu       sync.Mutex
		got      int
		lost     int
		failures []error
		wg       sync.WaitGroup
	)
	slots := make(chan struct{}, fetchConcurrency)
	for _, h := range want {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return got, lost, append(failures, ctx.Err())
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			data, err := n.opts.Peers.Fetch(ctx, h)
			if err == nil {
				err = n.opts.Local.Put(h, data)
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				got++
			case errors.Is(err, transfer.ErrNotFound):
				lost++
			default:
				failures = append(failures, fmt.Errorf("objstore/upkeep: repair %s: %w", h, err))
			}
		}()
	}
	wg.Wait()
	return got, lost, failures
}

// Collect deletes what nothing needs this node to hold: chunks no row refers
// to, past their grace, and referenced copies beyond this node's placement
// once the members that place them have confirmed theirs. See the package
// doc for why each rule is safe.
func (n *Node) Collect(ctx context.Context) error {
	m, ok := n.opts.Maps()
	if !ok {
		// NO MAP, NO DELETION: without one this node cannot say what it
		// should hold, and "nothing" is the one answer that would empty
		// it.
		return nil
	}
	v, err := n.opts.References.pin(ctx)
	if err != nil {
		n.skipped(err.Error())
		return err
	}
	cutoff := n.opts.Now().Add(-PendingGrace)
	deleted, strays := 0, 0
	skipped := ""
	for pg := range placement.PGCount {
		if err := ctx.Err(); err != nil {
			return err
		}
		var held []disk.Held
		if err := n.opts.Local.WalkGroup(pg, func(h disk.Held) error {
			held = append(held, h)
			return nil
		}); err != nil {
			return fmt.Errorf("objstore/upkeep: walk group %d: %w", pg, err)
		}
		if len(held) == 0 {
			continue
		}
		refs, complete, err := n.opts.References.referenced(ctx, pg, v)
		if err != nil {
			return err
		}
		up := m.Up(pg)
		here := slices.Contains(up, n.opts.Self)
		var beyond []objstore.Hash
		for _, h := range held {
			if _, referenced := refs[h.Hash]; referenced {
				if !here {
					beyond = append(beyond, h.Hash)
				}
				continue
			}
			if !complete {
				// A RECORD THIS NODE COULD NOT APPLY may be the one naming
				// this chunk, so "unreferenced" is not a fact yet.
				skipped = "a record this node could not apply may refer to chunks it holds"
				continue
			}
			// THE AGE IS JUDGED BY THE STORE, under the lock a writer's
			// touch takes: a copy judged here from the walk's reading
			// could have been written again since.
			removed, collectErr := n.opts.Local.Collect(h.Hash, cutoff)
			if collectErr != nil {
				return collectErr
			}
			if removed {
				deleted++
			}
		}
		removed, err := n.dropBeyond(ctx, m, up, beyond)
		if err != nil {
			return err
		}
		strays += removed
	}
	n.mu.Lock()
	n.status.Collected = n.opts.Now().UTC()
	n.status.Deleted, n.status.Strays, n.status.Skipped = deleted, strays, skipped
	n.mu.Unlock()
	if deleted > 0 || strays > 0 {
		log.InfoContext(ctx, "object_collect", "epoch", m.Epoch, "deleted", deleted, "strays", strays)
	}
	return nil
}

func (n *Node) skipped(why string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.status.Skipped = why
}

// dropBeyond deletes the referenced copies this node holds beyond its
// placement that every member of the group's up set holds and places, at
// this node's own epoch.
func (n *Node) dropBeyond(ctx context.Context, m placement.Map, up []string, beyond []objstore.Hash) (int, error) {
	if len(beyond) == 0 || len(up) == 0 || len(up) < m.Size() {
		return 0, nil
	}
	removed := 0
	for batch := range slices.Chunk(beyond, transfer.MaxHas) {
		confirmed := make([]bool, len(batch))
		for i := range confirmed {
			confirmed[i] = true
		}
		for _, member := range up {
			holding, err := n.opts.Peers.Has(ctx, member, batch)
			if err != nil || holding.Epoch != m.Epoch {
				// A MEMBER THAT DID NOT ANSWER, or answered by another
				// map, confirms nothing: keep every copy and ask again
				// next pass.
				confirmed = nil
				break
			}
			for i := range batch {
				confirmed[i] = confirmed[i] && holding.Held[i] && holding.Placed[i]
			}
		}
		for i, h := range batch {
			if confirmed == nil || !confirmed[i] {
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
