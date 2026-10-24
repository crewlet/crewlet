package usage

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/store"
)

// EVERY DAY AGAIN, after a reanchor of the usage log on any data node.
//
// # Why a reanchor owes the fleet every day
//
// Each data node re-anchors its own copy of this log (see reanchor.go), and a
// RESTORED log is followed from its END: replayed into the new generation, the
// copy's older records would outrank every row and roll each day back. So the
// node that re-anchored never applies what was published after the restore
// and before its reanchor. A day another data node republished in that tail —
// yesterday's late turn, a custody settle of an older day — and has not moved
// since stays at its pre-restore value on the re-anchoring node, because a
// publisher sends a day only when it moves. It would stay so for the 181 days
// the history holds it.
//
// So every data node publishes every day its own event log still holds again
// — the floor below is the only limit — once any data node's generation of
// this log rises past the generations it last answered. The move is seen in
// the positions register, the one place each node's generation of each log is
// published (internal/engine's heartbeat reads it, and hands it here as
// [PublisherDeps.Generations]); the re-anchoring node's own entry is its
// runner's, so its own reanchor is seen at once, and the applier that
// reanchor relaunches wakes the publisher on its first drain — the
// transition's last step, and the first moment it can publish
// ([Publisher.Nudge]).
//
// # Under operation ids no earlier copy carries
//
// A record's operation id is its content's digest, which is what lets the
// broker collapse a repeat — and here that is exactly the failure: a day a
// node published a minute before the reanchor, sent again unchanged, would be
// collapsed onto the copy the re-anchoring node skipped, and nothing would
// land. So the id also covers the generations the publisher publishes under
// ([Publisher.salt]): a pass answering a move carries ids no copy before the
// move does, and every publish after it carries them too, so a repeat inside
// one set of generations still collapses as it always did.
//
// # Answered once, across a restart
//
// The generations a pass answered are recorded once every day it derived has
// settled ([store.DB.SetUsageRepublished]), and read before the first
// observation — so a node down while a peer re-anchored, whose first beat
// already reads the peer's new generation, still answers it, and a node that
// answered it before a restart does not answer it again. A pass that has not
// settled is not recorded, so a restart answers the same move again from the
// watermark the pass lowered. A node that never answered any republishes once,
// which on a fresh node is nothing.
//
// A RISE IS A REANCHOR, a mere change is not: a data node joining appears in
// the register at whatever generation its donor's snapshot held, and nothing
// it lacks is in a tail, so a node first seen is a move only above zero. A
// change that moves nothing is recorded as answered without a pass.
//
// # What it cannot reach
//
// A day older than the event log is read back ([store.EventHistory]) is never
// derived again (the floor in publish.go), so a day republished in the tail
// and now past the floor keeps its pre-restore value on the re-anchoring node:
// a reanchor run within the event log's history of the restore loses nothing,
// and one run later loses those days to it.

// Generations is the usage log's generation on each data node, keyed by node
// id, as the positions register states it.
type Generations map[string]uint32

// String is the canonical spelling: `node@generation`, by node, comma-joined —
// what [store.DB.SetUsageRepublished] records and what [ParseGenerations]
// reads back.
func (g Generations) String() string {
	nodes := slices.Sorted(maps.Keys(g))
	parts := make([]string, 0, len(nodes))
	for _, node := range nodes {
		parts = append(parts, node+"@"+strconv.FormatUint(uint64(g[node]), 10))
	}
	return strings.Join(parts, ",")
}

// ParseGenerations reads back what [Generations.String] wrote.
func ParseGenerations(s string) (Generations, error) {
	out := Generations{}
	if s == "" {
		return out, nil
	}
	for part := range strings.SplitSeq(s, ",") {
		at := strings.LastIndexByte(part, '@')
		if at <= 0 {
			return nil, fmt.Errorf("usage: %q is not a node and a generation", part)
		}
		gen, err := strconv.ParseUint(part[at+1:], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("usage: %q names no generation: %w", part, err)
		}
		out[part[:at]] = uint32(gen)
	}
	return out, nil
}

// Rises reports whether any node's generation in g is above its generation in
// than — a node absent from than standing at zero, so a node first seen rises
// only above it.
func (g Generations) Rises(than Generations) bool {
	for node, gen := range g {
		if gen > than[node] {
			return true
		}
	}
	return false
}

// salt is what an operation id covers beside the content: the generations the
// publisher publishes under — those of a pass in progress, or the last ones it
// answered. See the file's header.
func (p *Publisher) salt() string {
	if p.republishing != nil {
		return p.republishing.String()
	}
	if p.answered != nil {
		return p.answered.String()
	}
	return ""
}

// observeGenerations reads, once per process, the generations this publisher
// last answered, and then holds what the engine last read of the register
// against them: a rise starts a pass over every day ([Publisher.republishAll]),
// a change that rises nothing is recorded as answered. The caller holds mu.
func (p *Publisher) observeGenerations(ctx context.Context, now time.Time,
	loc *time.Location) error {

	if !p.answeredRead {
		recorded, ok, err := p.deps.Store.UsageRepublished(ctx)
		if err != nil {
			return fmt.Errorf("usage: read the generations this node last "+
				"republished under: %w", err)
		}
		if ok {
			if p.answered, err = ParseGenerations(recorded); err != nil {
				return fmt.Errorf("usage: the generations this node last republished "+
					"under do not read back: %w", err)
			}
		}
		p.answeredRead = true
	}
	seen, known := p.deps.Generations()
	if !known {
		return nil
	}
	current := p.answered
	if p.republishing != nil {
		current = p.republishing
	}
	switch {
	case current == nil || seen.Rises(current):
		return p.republishAll(ctx, seen, now, loc)
	case seen.String() != current.String() && p.republishing == nil:
		if err := p.deps.Store.SetUsageRepublished(ctx, seen.String()); err != nil {
			return fmt.Errorf("usage: record the generations this node answered: %w", err)
		}
		p.answered = seen
	}
	return nil
}

// republishAll starts a pass that publishes every day this node's event log
// still holds again, under seen: what this process remembers of what it
// published is forgotten, so every object of every day is sent, and its bound
// is lowered to the first day the floor still lets it derive, so every older
// day is derived in full. The caller holds mu.
func (p *Publisher) republishAll(ctx context.Context, seen Generations, now time.Time,
	loc *time.Location) error {

	first, err := p.floorDay(ctx, now, loc)
	if err != nil {
		return err
	}
	detail := "a data node's usage log was re-anchored since this node last " +
		"published every day, and a node re-anchoring a restored log never " +
		"applies what was published after the restore, so every day this " +
		"node's event log still holds is published again"
	if p.answered == nil {
		// NO REANCHOR IS CLAIMED where this node's store records no pass at
		// all: that is every data node's first start — whose event log holds
		// nothing yet, so the pass costs nothing — or one that stopped before
		// its first pass settled. Told a reanchor, an operator looking at a
		// fresh fleet would go looking for one that never happened.
		detail = "this node has not recorded publishing every day under any of " +
			"the fleet's usage generations, so every day its event log holds is " +
			"published under them"
	}
	p.deps.Logger.InfoContext(ctx, "usage_republish_started",
		"generations", seen.String(), "answered", p.answered.String(), "from", first.Label,
		"detail", detail)
	p.republishing = seen
	clear(p.published)
	clear(p.marks)
	p.lower(first.Start)
	return nil
}

// settleRepublish records the generations a pass answered once every day it
// derived has settled. The caller holds mu.
func (p *Publisher) settleRepublish(ctx context.Context) error {
	if p.republishing == nil {
		return nil
	}
	if err := p.deps.Store.SetUsageRepublished(ctx, p.republishing.String()); err != nil {
		return fmt.Errorf("usage: record the generations this node republished "+
			"every day under: %w", err)
	}
	p.deps.Logger.InfoContext(ctx, "usage_republish_settled",
		"generations", p.republishing.String())
	p.answered, p.republishing = p.republishing, nil
	return nil
}

// floorDay is the first day that starts at or after the event log's floor — the
// oldest day this node can still derive whole — as this node cuts it.
func (p *Publisher) floorDay(ctx context.Context, now time.Time, loc *time.Location) (
	period.Window, error) {

	floor := now.Add(-store.EventHistory)
	day, err := p.dayAt(ctx, floor, loc)
	if err != nil {
		return period.Window{}, err
	}
	if day.Start.Before(floor) {
		return p.step(ctx, day, 1)
	}
	return day, nil
}
