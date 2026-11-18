package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN EVICTED NODE'S POSITION IS NOT THE FLEET'S. The positions register keeps
// every node that ever ran (a readmission is judged by its row), and a node's
// published trim floors stay. Read as the fleet's, a node that re-anchored and
// was decommissioned before any peer adopted from it strands every peer: each
// sees a generation ahead, asks for a donor that no longer exists, and has even
// a forced reanchor refused. Eviction retires such a machine, so an evicted
// node's row and floor count toward neither the fleet's generation nor the
// reanchor guards, and a reanchor opens the generation after the one it held,
// abandoning its records ([statelog.ReanchorInputs.Abandoned]). Eviction is
// asked of the log first ([statelog.EvictedOnLog]), as the peers it stranded
// stopped applying before it; the rows answer only where the trim removed the
// record, and what it removed the rows keep.

// evictedOn reports which of nodes are evicted from one domain's log.
//
// A READ THAT FAILS IS AN ERROR, never "nobody is evicted" (which strands a
// fleet on a dead peer's generation) nor "every one is" (which lets a live
// peer's generation be abandoned).
func (s *stateLog) evictedOn(ctx context.Context, domain statelog.Domain,
	spec statelog.StreamSpec, log statelog.StandingLog, nodes []string) (map[string]bool, error) {

	out := make(map[string]bool, len(nodes))
	if len(nodes) == 0 || !domain.ClaimsIdentity() {
		// ONLY AN IDENTITY-CLAIMING DOMAIN CARRIES EVICTIONS, and only its
		// fleet generation and reanchor guards count anybody.
		return out, nil
	}
	var rows []statelog.EvictionRow
	if lister, ok := domain.(evictionLister); ok {
		var err error
		if rows, err = lister.Evictions(ctx, s.estate().Reader()); err != nil {
			return nil, fmt.Errorf("engine: read the evictions on %s's rows: %w",
				domain.Name(), err)
		}
	}
	for _, node := range nodes {
		onLog, found, err := statelog.EvictedOnLog(ctx, domain, spec, log, node)
		if err != nil {
			return nil, err
		}
		if found {
			out[node] = onLog
			continue
		}
		for _, row := range rows {
			if row.NodeID == node {
				out[node] = !row.Back
			}
		}
	}
	return out, nil
}

// fleetGenerations is the generation the FLEET is on for each domain, for a
// node whose own rows cannot say or may have been left behind: the MAXIMUM of
// every live node's published position and the trim's published floor. Either
// may be the only source (every peer restarting; never trimmed), and a
// re-anchor moves the fleet one node at a time, so the highest is where the
// company is going and anything below it would be adopted again. Evicted
// nodes' rows and floors are left out (this file's top comment); only nodes
// ABOVE the caller's own generation (above) can put the fleet ahead, so only
// they are asked about. An absent domain reads as zero, a company that never
// re-anchored, so a new fleet replays from the start. rows and floors are read
// once by the caller. A domain that cannot be judged is answered in the second
// map, the rest in the first: only the join, which needs every domain, refuses.
func (s *stateLog) fleetGenerations(ctx context.Context, rows []coord.NodePositions,
	floors publishedFloors, logs map[string]*jetstream.DomainLog,
	above map[string]uint32) (map[string]uint32, map[string]error) {

	newest := map[string]uint32{}
	unread := map[string]error{}
	for _, domain := range registeredDomains() {
		name := domain.Name()
		floor, published, err := floors.row(name)
		if err != nil {
			unread[name] = fmt.Errorf("engine: establish which generation %s is on: %w",
				name, err)
			continue
		}
		var candidates []string
		for _, row := range rows {
			if at, runs := row.Domains[name]; runs && at.Generation > above[name] &&
				row.NodeID != s.nodeID {
				candidates = append(candidates, row.NodeID)
			}
		}
		if published && floor.Generation > above[name] && floor.By != "" &&
			floor.By != s.nodeID {
			candidates = append(candidates, floor.By)
		}
		var evicted map[string]bool
		if len(candidates) > 0 {
			log, held := logs[name]
			if !held {
				unread[name] = fmt.Errorf("engine: %s's log is not open, so whether "+
					"the nodes ahead of this one on it are evicted cannot be read", name)
				continue
			}
			// THROUGH THE DOMAIN'S VERIFIER, since the standing is decoded
			// from a record's body; derived from the keyring, not a running
			// log, because the join asks this before any log has started.
			verifier, verr := s.verifierFor(domain)
			if verr != nil {
				unread[name] = verr
				continue
			}
			if evicted, err = s.evictedOn(ctx, domain, domain.Stream(),
				statelog.StandingOf(log, verifier), candidates); err != nil {
				unread[name] = err
				continue
			}
		}
		for _, row := range rows {
			if at, runs := row.Domains[name]; runs && !evicted[row.NodeID] {
				newest[name] = max(newest[name], at.Generation)
			}
		}
		if published && !evicted[floor.By] {
			newest[name] = max(newest[name], floor.Generation)
		}
	}
	return newest, unread
}

// openLogs is every log this node runs, keyed as the register keys them —
// what [stateLog.fleetGenerations] reads the evicted peers off.
func (s *stateLog) openLogs() map[string]*jetstream.DomainLog {
	running := s.running()
	logs := make(map[string]*jetstream.DomainLog, len(running))
	for _, r := range running {
		logs[r.domain.Name()] = r.log
	}
	return logs
}
