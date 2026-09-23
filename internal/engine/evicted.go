package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN EVICTED NODE'S POSITION IS NOT THE FLEET'S.
//
// The positions register keeps a row for every node that ever ran — an eviction
// deliberately does not forget it, because a readmission is judged by it — and
// the trim floors a node published stay where it left them. Read as the fleet's
// own, a node that re-anchored a log and was decommissioned before any peer
// adopted from it held every other node in two traps for ever: each saw a
// generation ahead of its own, stopped, and asked for a donor at that
// generation that no longer existed; and each one's reanchor refused, force or
// no force, on a peer that had already re-anchored. The operator's gesture for
// a machine that is not coming back is the eviction, so an evicted node's row,
// and a floor it published, counts toward neither the generation the fleet is
// on nor the reanchor guards — and a reanchor opens the generation after the
// one it held, whose records are abandoned ([statelog.ReanchorInputs.Abandoned]).
//
// # Asked of the log, because the node asking may be stranded
//
// Whether a node is evicted is each domain's own fact, applied into its own
// table on every node — but the nodes that most need the answer are the ones
// the evicted peer stranded, whose appliers stopped before they could apply
// the eviction. So the log is asked first ([statelog.EvictedOnLog]): the last
// record on the node's eviction subject is its standing, whether or not this
// node's applier has reached it. The rows answer only where the log holds
// nothing, because the trim removed it — and then what it removed is exactly
// what the rows keep.

// evictedOn reports which of nodes are evicted from one domain's log.
//
// A READ THAT FAILS IS AN ERROR, never "nobody is evicted" and never "every one
// is": the first strands a fleet on a dead peer's generation and the second
// lets a live peer's generation be abandoned, so the caller decides nothing on
// an answer it could not get.
func (s *stateLog) evictedOn(ctx context.Context, domain statelog.Domain,
	log statelog.StandingLog, nodes []string) (map[string]bool, error) {

	out := make(map[string]bool, len(nodes))
	if len(nodes) == 0 || !domain.ClaimsIdentity() {
		// ONLY AN IDENTITY-CLAIMING DOMAIN CARRIES EVICTIONS, and it is
		// the only kind whose fleet generation and reanchor guards count
		// anybody.
		return out, nil
	}
	var rows []statelog.EvictionRow
	if lister, ok := domain.(evictionLister); ok {
		var err error
		if rows, err = lister.Evictions(ctx, s.db); err != nil {
			return nil, fmt.Errorf("engine: read the evictions on %s's rows: %w",
				domain.Name(), err)
		}
	}
	for _, node := range nodes {
		onLog, found, err := statelog.EvictedOnLog(ctx, domain, log, node)
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

// fleetGenerations is the number space the FLEET is on for each domain — for a
// node whose own rows cannot say, and for one whose rows the fleet may have
// left behind.
//
// TWO SOURCES BECAUSE EITHER MAY BE THE ONLY ONE. Every live node publishes
// its own generation per domain in the position register, and the trim
// publishes the one it concluded at; a fleet whose peers are all restarting
// has the floor and no positions, and one that has never trimmed has positions
// and no floor. The MAXIMUM is taken because a re-anchor moves the fleet one
// node at a time: the highest anybody reports is the space the company is
// moving into, and an artefact from below it is one this node would have to
// adopt again.
//
// MINUS EVERY EVICTED NODE — its row and any floor it published — for the
// reason this file gives. Only a node ABOVE the caller's own generation for
// the domain (above) is asked about: nothing at or below it can put the fleet
// ahead of the caller, so a fleet on one generation asks the log nothing at
// all.
//
// Zero is a real answer — a company that has never re-anchored — and it is
// what makes a genuinely new node in a genuinely new fleet replay from the
// beginning rather than ask for a snapshot nobody has. A domain nobody has
// published anything for is absent from the map, which reads as that zero.
func (s *stateLog) fleetGenerations(ctx context.Context,
	logs map[string]*jetstream.DomainLog, above map[string]uint32) (map[string]uint32, error) {

	rows, err := s.fleet.Positions(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: read the fleet's published positions to "+
			"establish which generation each domain is on: %w", err)
	}
	floors, err := s.fleet.Floors(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: read the fleet's published trim floors to "+
			"establish which generation each domain is on: %w", err)
	}
	newest := map[string]uint32{}
	for _, domain := range registeredDomains() {
		name := domain.Name()
		var candidates []string
		for _, row := range rows {
			if at, runs := row.Domains[name]; runs && at.Generation > above[name] &&
				row.NodeID != s.nodeID {
				candidates = append(candidates, row.NodeID)
			}
		}
		for _, f := range floors {
			if f.Domain == name && f.Generation > above[name] && f.By != "" &&
				f.By != s.nodeID {
				candidates = append(candidates, f.By)
			}
		}
		var evicted map[string]bool
		if len(candidates) > 0 {
			log, held := logs[name]
			if !held {
				return nil, fmt.Errorf("engine: %s's log is not open, so whether the "+
					"nodes ahead of this one on it are evicted cannot be read", name)
			}
			if evicted, err = s.evictedOn(ctx, domain, log, candidates); err != nil {
				return nil, err
			}
		}
		for _, row := range rows {
			if at, runs := row.Domains[name]; runs && !evicted[row.NodeID] {
				newest[name] = max(newest[name], at.Generation)
			}
		}
		for _, f := range floors {
			if f.Domain == name && !evicted[f.By] {
				newest[name] = max(newest[name], f.Generation)
			}
		}
	}
	return newest, nil
}

// openLogs is every domain's log this node runs, keyed as the register keys
// them — what [stateLog.fleetGenerations] reads the evicted peers off.
func (s *stateLog) openLogs() map[string]*jetstream.DomainLog {
	logs := make(map[string]*jetstream.DomainLog, len(s.domains))
	for name, running := range s.domains {
		logs[name] = running.log
	}
	return logs
}
