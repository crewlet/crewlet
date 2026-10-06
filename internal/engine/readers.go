package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// countedReaders is which record version every node applying a log reads, as
// a writer asks it before publishing a record an older build cannot even
// defer — the semantic index's records on the vector log (ADR-0028), a
// person's day on the usage log.
//
// THE COUNTED SET, the trim's own: the positions register's rows for the log,
// and every live data node that has not reported yet — every node that applies
// it — less every node the fleet has EVICTED, an operator's word that a node
// is not coming back; without that, an old build's row on a machine nobody
// will start again would hold the writer back for the life of the deployment.
// ONE READING for every writer that asks, because two would disagree about who
// counts.
type countedReaders struct {
	// register is the positions register, holders the live data nodes.
	register func(context.Context) ([]coord.NodePositions, error)
	holders  liveData

	// identity is every log whose domain claims identity, whose eviction
	// records are how the fleet says a node is gone, and db the node whose
	// replicated estate they are read from.
	identity []*runningLog
	db       *store.DB
}

// countedReadersOf is this node's reading of the counted sets.
func (e *Engine) countedReadersOf(s *stateLog) countedReaders {
	return countedReaders{
		register: func(ctx context.Context) ([]coord.NodePositions, error) {
			return s.fleet.Positions(ctx)
		},
		holders:  e.holdersOf(),
		identity: s.identityDomains(),
		db:       e.backends.Store,
	}
}

// readers is the record version each counted node reads on the named log.
func (c countedReaders) readers(ctx context.Context, domain string) (map[string]int, error) {
	rows, err := c.register(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the positions register: %w", err)
	}
	var live []statelog.Presence
	if c.holders != nil {
		if live, err = c.holders.LiveData(ctx); err != nil {
			return nil, fmt.Errorf("read the live data nodes: %w", err)
		}
	}
	tombs, err := c.evicted(ctx)
	if err != nil {
		return nil, err
	}
	return statelog.Readers(statelog.CountedSet(time.Now().UTC(),
		reportedPositions(rows, domain), live, tombs)), nil
}

// evicted is every node the fleet has evicted and not readmitted, as a
// tombstone the counted set subtracts once its fence window has passed.
//
// FROM THE IDENTITY-CLAIMING LOGS, because the vector log carries no eviction
// of its own — a node behind on it is a coverage figure, never a node that
// cannot resume — and an eviction is the fleet's one gesture for a node that
// is gone. A node counts as evicted only where EVERY identity log holds its
// eviction, dated by the latest of them: an eviction still going round the
// logs is not yet the fleet's word. A log whose evictions cannot be read is
// an error rather than none, because "evicted nowhere" read off a table nobody
// read would hold a writer back for a node an operator released — or, read
// the other way, release it for one they did not.
func (c countedReaders) evicted(ctx context.Context) ([]statelog.Tombstone, error) {
	if len(c.identity) == 0 || c.db == nil {
		return nil, nil
	}
	type seen struct {
		logs int
		at   time.Time
	}
	evicted := map[string]*seen{}
	for _, running := range c.identity {
		lister, ok := running.domain.(evictionLister)
		if !ok {
			return nil, fmt.Errorf("the %s log lists no evictions", running.domain.Name())
		}
		rows, err := lister.Evictions(ctx, c.db.Replicated().Reader())
		if err != nil {
			return nil, fmt.Errorf("read the %s log's evictions: %w",
				running.domain.Name(), err)
		}
		for _, row := range rows {
			if row.Back {
				continue
			}
			s := evicted[row.NodeID]
			if s == nil {
				s = &seen{}
				evicted[row.NodeID] = s
			}
			s.logs++
			if row.At.After(s.at) {
				s.at = row.At
			}
		}
	}
	var out []statelog.Tombstone
	for node, s := range evicted {
		if s.logs == len(c.identity) {
			out = append(out, statelog.Tombstone{NodeID: node, At: s.at})
		}
	}
	return out, nil
}
