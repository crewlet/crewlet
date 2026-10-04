package engine

import (
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// LayoutZero is today's estate as a layout: one space, estate, with one
// partition carrying every registered domain, in the register's order, under
// the stream names and register keys they have always had.
//
// Built FROM the register rather than from a second list of names, so a
// domain registered is a domain layout 0 carries and the two orders cannot
// disagree.
func LayoutZero() statelog.Layout {
	return statelog.EstateLayout(registeredNames()...)
}

// layout is the layout this node runs: its state log's, or [LayoutZero] on a
// node running none.
func (e *Engine) layout() statelog.Layout {
	if n := e.native.Load(); n != nil && n.log != nil {
		return n.log.layout
	}
	return LayoutZero()
}

// domainEstate is the replicated estate as this node's DOMAIN-LEVEL consumers
// read it — a seat's tracker search, the search index and its coverage. A
// node without `data`, which holds none, answers [store.ErrNoEstate] through
// it.
//
// A READ handle, because every one of those consumers only reads: what may
// write the estate is the short list internal/store's applier gate reads, and
// a domain-level accessor answering the write handle would have put every
// caller of it on that list.
func (e *Engine) domainEstate() store.ReplicatedReader {
	if e.backends == nil || e.backends.Store == nil {
		return store.ReplicatedReader{}
	}
	return e.backends.Store.Replicated().Reader()
}

// estateLog is a registered domain's one log under [LayoutZero], and
// estateSpec its stream — named as it has always been, at the domain's whole
// declared budget.
func estateLog(domain statelog.Domain) statelog.LogID {
	return statelog.LogID{Domain: domain.Name(), Partition: statelog.EstatePartition}
}

func estateSpec(domain statelog.Domain) statelog.StreamSpec {
	return LayoutZero().StreamSpec(domain, estateLog(domain))
}
