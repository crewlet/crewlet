package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/learning/memread"
)

// A seat's memory, read from the node that holds the seat
// (internal/learning/memread).
//
// # Every node serves, in every mode
//
// A seat can be held by a node that serves no API, so the answer cannot be the
// API's to build: every node answers for the seats it holds, from its own
// store, and every node that serves the API asks. A node in maintenance holds
// no seat and so is never addressed — and answering is not publishing, so the
// maintenance gate has nothing here to stop either.

// armMemoryReads builds this node's reader and makes the node an answerer.
//
// After the node, whose incarnation is what a seat's lease names and whose
// attached set is when a seat's memory here is current.
func (e *Engine) armMemoryReads(ctx context.Context) error {
	if e.backends == nil || e.backends.Store == nil || e.node == nil {
		return nil
	}
	db := e.backends.Store
	stores := &memread.Stores{
		Diary:         learning.NewDiary(db),
		Episodes:      learning.NewEpisodes(db),
		Skills:        learning.NewSkills(db),
		Profiles:      learning.NewCounterparties(db),
		Onboarding:    learning.NewOnboarding(db),
		Conversations: ledgerstore.NewConversations(db),
		AgentID:       e.agentIDOf,
	}
	owner := e.node.Owner()
	attached := e.node.Attached
	e.memoryReads = &memread.Reader{Owner: owner, Local: stores}
	if e.backends.Queue == nil {
		// A NODE WITH NO BROKER IS THE FLEET, and its store the only copy
		// of every seat's memory there is.
		return nil
	}
	e.memoryReads.Queue = e.backends.Queue
	e.memoryReads.Leases = e.backends.Coord
	e.memoryReads.Features = coord.FeatureReader{Leases: e.backends.Coord}
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

// MemoryReads is a seat's memory and conversation ledger, answered by the node
// holding the seat — what the API answers `agent_memory` and `conversations`
// from. Nil on a node with no store.
func (e *Engine) MemoryReads() *memread.Reader { return e.memoryReads }

// agentIDOf derives a seat's agent id — what its diary and onboarding marker
// are keyed by — from its handle, through the CURRENT epoch; "" for a handle
// the company has no agent seat for, or before any company is running.
func (e *Engine) agentIDOf(handle string) string {
	c := e.Company()
	if c == nil {
		return ""
	}
	id, ok := c.Org.AgentIDFor(c.Org.AgentSeatByHandle(handle))
	if !ok {
		return ""
	}
	return id.String()
}
