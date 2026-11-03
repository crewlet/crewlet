package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// What a node cannot see that may name an object.
//
// # Why the collector has to ask
//
// The object store's passes judge an object by every declared table naming it
// (internal/objstore/references), and the declarations are THIS BUILD's. A
// newer build can add a domain whose rows name objects, and this build has
// never heard of it: its records are not records this node met and failed to
// decode — which the collector already refuses to judge past — because this
// node does not run that log at all. So a collector on an older build, during
// a rolling upgrade or after a rollback, would read every object the newer
// domain names as nobody's and delete it a day after its upload. CLAUDE.md asks
// every build to be a good older peer to the next one; this is how the
// collector is.
//
// Two places say a newer build's rows exist, and the answer reads both:
//
//   - THE POSITION REGISTER, where every data node publishes its position on
//     every log it runs: a node the fleet counts that runs a log this build
//     does not register is a newer build mid-rollout. Counted exactly as the
//     trim counts a log's readers ([Engine.countedOn]) — the register's rows,
//     every live data node, less a node evicted past the fence window — so a
//     departed node an operator evicted does not hold collection back for
//     ever, and one that is merely offline does, as it holds the trim.
//   - THIS NODE'S OWN COPY of the estate, which a newer build may have
//     migrated before the node was rolled back: the migration runner steps
//     over a version this binary does not carry, so the node boots, and the
//     tables that version made are ones it cannot read. The register cannot
//     say so — a rolled-back node republishes its row with the logs it runs
//     now — and on a fleet with one data node it is the only witness.
//
// Asked by every collection before its first judgement
// ([collect.Options.Unseen]) and by an object migration's final pass, which
// would otherwise leave such objects behind in the store it is leaving.

// unseen is what may name an object that this node cannot read, in a
// sentence, or "" where there is nothing — see the file comment.
//
// AN ANSWER IT CANNOT HAVE IS AN ERROR, never "": the register unreadable, the
// live data nodes unlisted, this node's copy of the estate not open. The
// caller reads an error as a reason to judge nothing, so a store blip costs a
// pass and never an object.
func (e *Engine) unseen(ctx context.Context) (string, error) {
	if e.backends == nil || e.backends.Fleet == nil || e.backends.Coord == nil {
		return "", errors.New("this node has no coordination store to read the fleet's logs from")
	}
	rows, err := e.backends.Fleet.Positions(ctx)
	if err != nil {
		return "", fmt.Errorf("read the position register: %w", err)
	}
	// THE TRIM'S READING OF WHO IS LIVE, listed now ([Engine.holdersOf]),
	// not the watched view the snapshot loop counts with: this answer
	// licenses deleting objects, and a listing up to a heartbeat old may
	// miss a live node whose row has not landed.
	live, err := e.holdersOf().LiveData(ctx)
	if err != nil {
		return "", fmt.Errorf("list the live data nodes: %w", err)
	}
	var why []string
	if logs := foreignLogs(rows, e.countedOn(ctx, rows, live, e.now()), registeredNames()); len(logs) > 0 {
		why = append(why, logs...)
	}
	if e.backends.Store != nil {
		versions, err := e.backends.Store.Replicated().Reader().ForeignMigrations(ctx)
		if err != nil {
			return "", fmt.Errorf("read this node's copy of the estate: %w", err)
		}
		if len(versions) > 0 {
			why = append(why, fmt.Sprintf("this node's copy of the estate was migrated "+
				"by a newer build (%s), whose tables this build cannot read; run that "+
				"build again here, or restore a backup taken by this one",
				words.And(versions)))
		}
	}
	return strings.Join(why, "; "), nil
}

// foreignLogs is every state log a node in counted runs that known does not
// name, each as the sentence the collector reports, in log order — see the
// file comment. A row of a node the fleet does not count is not read: an
// evicted node's newer logs hold nothing back.
func foreignLogs(rows []coord.NodePositions, counted map[string]bool, known []string) []string {
	runs := map[string][]string{}
	for _, row := range rows {
		if !counted[row.NodeID] {
			continue
		}
		for name := range row.Domains {
			if !slices.Contains(known, name) {
				runs[name] = append(runs[name], row.NodeID)
			}
		}
	}
	out := make([]string, 0, len(runs))
	for _, name := range slices.Sorted(maps.Keys(runs)) {
		nodes := runs[name]
		slices.Sort(nodes)
		who := "data node " + words.And(nodes) + " runs"
		if len(nodes) > 1 {
			who = "data nodes " + words.And(nodes) + " run"
		}
		out = append(out, fmt.Sprintf("%s the state log %q, which this build does not; "+
			"its rows may name objects until every data node runs a build that knows it",
			who, name))
	}
	return out
}

// countedOn is every node the fleet counts on at least one log this build
// registers, from the register's rows and the live data nodes given: each log's
// [statelog.CountedSet], less the nodes this node's copy of the estate has
// evicted on that log. ONE READING for [Engine.counted] and [Engine.unseen], so
// the snapshot loop and the collector never disagree about who the fleet is.
//
// A log whose eviction rows cannot be read subtracts none, which counts more
// rather than less — for the collector, the direction that holds a deletion
// back rather than letting one through.
func (e *Engine) countedOn(ctx context.Context, rows []coord.NodePositions,
	live []statelog.Presence, now time.Time) map[string]bool {

	var db *store.DB
	if e.backends != nil {
		db = e.backends.Store
	}
	counted := map[string]bool{}
	for _, domain := range registeredDomains() {
		tombs, _ := logTombstones(ctx, db, domain, 0)
		for _, n := range statelog.CountedSet(now, reportedPositions(rows, domain.Name()), live, tombs) {
			counted[n.NodeID] = true
		}
	}
	return counted
}
