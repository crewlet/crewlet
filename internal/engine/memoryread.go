package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/learning/memread"
)

// A seat's memory is read from the node holding the seat (internal/learning/memread).
// A seat's holder may serve no API, so every node answers for the seats it
// holds, from its own store, and every API node asks. A node in maintenance
// holds no seat, and answering is not publishing, so the maintenance gate has
// nothing to stop here.

// armMemoryReads builds this node's reader and makes the node an answerer. It
// runs after the node is built: a seat's lease names the node's incarnation,
// and its attached set says when a seat's memory here is current.
func (e *Engine) armMemoryReads(ctx context.Context) error {
	if e.backends == nil || e.backends.Store == nil || e.node == nil {
		return nil
	}
	db := e.backends.Store
	stores := &memread.Stores{
		Diary:      learning.NewDiary(db),
		Episodes:   learning.NewEpisodes(db),
		Skills:     learning.NewSkills(db),
		Profiles:   learning.NewCounterparties(db),
		Onboarding: learning.NewOnboarding(db),
	}
	owner := e.node.Owner()
	// Keyed by seat ID, as a read names its seat.
	attached := e.node.AttachedSeats
	e.memoryReads = &memread.Reader{Owner: owner, Local: stores}
	if e.backends.Queue == nil {
		// A node with no broker is the whole fleet: its store is the only copy.
		return nil
	}
	e.memoryReads.Queue = e.backends.Queue
	e.memoryReads.Leases = e.backends.Coord
	e.memoryReads.Attached = attached
	stop, err := memread.Serve(ctx, e.backends.Queue, owner, attached, stores)
	if err != nil {
		return err
	}
	e.stopMemoryServe = stop
	return nil
}

// stopMemoryReads withdraws this node as an answerer, before the broker and the
// store close, so a read arriving during the teardown is declined rather than
// answered from a closing file.
func (e *Engine) stopMemoryReads(ctx context.Context) {
	if e.stopMemoryServe == nil {
		return
	}
	if err := e.stopMemoryServe(context.WithoutCancel(ctx)); err != nil {
		log.WarnContext(ctx, "memory_answerer_not_withdrawn", "error", err)
	}
	e.stopMemoryServe = nil
}

// MemoryReads reads a seat's memory from the node holding the seat; the API
// answers `agent_memory` from it. Nil on a node with no store.
func (e *Engine) MemoryReads() *memread.Reader { return e.memoryReads }
