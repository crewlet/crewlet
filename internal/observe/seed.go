package observe

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// FleetHistory is what seeding reads: the fleet's event stores, every live
// node's, merged. Satisfied by *eventfan.Fleet.
//
// EVERY METHOD ANSWERS WITH A COVERAGE, and that is the point of the shape: a
// bare *store.EventLog does not satisfy it, so a seed that quietly read one
// node's store — which is what a restarted node's feed, rollup and seat rows
// used to describe, a third of a three-node fleet — does not compile.
//
// The windowed reads are measured from the instant each query carries
// ([store.PhaseTokenQuery.At], [store.TurnQuery.At]), which the seed reads
// ONCE: a second reading per read would seed windows a few microseconds apart,
// each longer than the projection's own.
type FleetHistory interface {
	List(ctx context.Context, q store.ListQuery) (eventfan.Listing, eventfan.Coverage, error)
	PhaseTokens(ctx context.Context, q store.PhaseTokenQuery) ([]tokens.Record, eventfan.Coverage, error)
	Turns(ctx context.Context, q store.TurnQuery) (eventfan.TurnPage, eventfan.Coverage, error)
}

// Seeded is the projection history is folded into. Satisfied by
// *livestate.LiveState, and by the stream service, which also corrects every
// open socket with a seed that lands after the node serves.
type Seeded interface {
	Seed(livestate.History) livestate.Change
}

// SeedTurnsPerSeat is how many of a seat's newest turns the seed reads to find
// the last one it ENDED.
//
// THREE, because a seat runs one turn at a time: its newest turn may be the one
// in flight, and the one before it may be parked on its executor — a seat with
// a run in flight takes no new work until it is collected — so the newest
// ended turn is among the newest three unless the seat's recent turns died
// mid-flight, which leaves nothing the store could honestly call an end. It is
// asked per seat, rather than as one company-wide page, because a page of the
// company's newest turns says nothing about a seat that has been quiet longer
// than everyone else's recent work.
const SeedTurnsPerSeat = 3

// seedTurnReads is how many seats' turn reads are in flight at once.
//
// SIXTEEN. Each read is two fleet scatters and a GROUP BY on every node, so an
// unbounded fan-out would put a whole roster's requests on the broker and
// every peer's reader pool in one instant at every boot. What bounds it from
// below is a peer that does not answer: every scatter then waits the fleet
// read budget out, so each ROUND of reads costs two budgets, and the seed's own
// ceiling fits one round. Sixteen covers a company of sixteen agent seats in
// that one round; past it, the seats a silent peer's rounds did not reach are
// reported unseeded rather than holding the listener shut, and on a fleet
// whose every node answers the rounds are milliseconds each.
const seedTurnReads = 16

// Seed fills a freshly started projection from the FLEET's event stores:
// THREE BOUNDED READS, each bound the projection's own, so a seed never holds
// more than the live stream could have built.
//
//   - the feed: the newest [livestate.EventFeedLimit] persisted events the feed
//     carries ([store.ListQuery.FeedOnly]), inside [store.EventHistory]'s floor;
//   - the spend: the newest [livestate.SpendRecordLimit] records inside
//     [livestate.LiveSpendWindow], capped at the READ on every node and paged;
//   - each agent seat's newest [SeedTurnsPerSeat] turns, by AGENT ID.
//
// FROM EVERY DATA NODE, since each also keeps custody ([Custody]) for nodes
// without `data`; who answered is recorded ([livestate.LiveState.SeededFrom]).
func Seed(ctx context.Context, history FleetHistory, organization *org.Organization, live Seeded) (
	eventfan.Coverage, error) {

	// Called AFTER the broadcast subscription is attached, so no event falls
	// in the gap; the projection recognises one arriving both ways. BEST EFFORT
	// per read: a failed one does not cost the others and every failure is
	// returned for the caller to report, being visible nowhere else; the
	// screens start at this process's boot until a later seed ([HistorySeed]).
	// The reads run CONCURRENTLY, since in sequence under a budget a SLOW first
	// one handed the next an expired context: the seed costs the slowest, not
	// the sum, and the listener waits at most ([Seeding.Wait]).
	var (
		h      livestate.History
		wg     sync.WaitGroup
		mu     sync.Mutex
		errs   []error
		covers []eventfan.Coverage
	)
	report := func(c eventfan.Coverage, err error, what string) bool {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("observe: read %s: %w", what, err))
			return false
		}
		covers = append(covers, c)
		return true
	}

	// ONE READING OF THE CLOCK for every windowed read: the store measures
	// each window from the instant it is handed, so a second reading would
	// seed a spend window a few microseconds longer than the projection's
	// own and a turn page cut from another "now".
	at := time.Now().UTC()
	wg.Go(func() {
		listing, c, err := history.List(ctx, store.ListQuery{Limit: livestate.EventFeedLimit, FeedOnly: true})
		if !report(c, err, "the activity feed's history") {
			return
		}
		rows := make([]livestate.FeedRow, 0, len(listing.Rows))
		for _, rec := range listing.Rows {
			rows = append(rows, FeedRow(rec))
		}
		h.Events = rows
	})
	wg.Go(func() {
		// The window is asked for as an INSTANT, never a day count: they match
		// only while LiveSpendWindow is whole days (36 hours in days seeds 24
		// while the projection keeps 36, silently).
		records, c, err := history.PhaseTokens(ctx, store.PhaseTokenQuery{
			Since: at.Add(-livestate.LiveSpendWindow),
			Limit: livestate.SpendRecordLimit,
			At:    at,
		})
		if report(c, err, "the live spend window's history") {
			h.Spend = records
		}
	})
	wg.Go(func() {
		turns, cs, asked, err := seedTurns(ctx, history, agentSeats(organization), at)
		// A company with no agent seat asks nobody anything, and a read
		// that was never made says nothing about who answered it.
		if !asked {
			return
		}
		// NOT THROUGH report: that is the shape of ONE read, all or
		// nothing, and this is one read per seat. A seat whose read
		// failed costs that seat — its error is reported and the seed is
		// incomplete — while every seat that WAS read keeps its last turn
		// and its parked turn. Routed through report, one silent seat
		// dropped every seat's turns.
		mu.Lock()
		defer mu.Unlock()
		h.Turns = turns
		covers = append(covers, cs...)
		if err != nil {
			errs = append(errs, fmt.Errorf("observe: read each seat's newest turns: %w", err))
		}
	})
	wg.Wait()

	h.Coverage = combine(covers, len(errs) == 0)
	live.Seed(h)
	return h.Coverage, errors.Join(errs...)
}

// agentSeats is every agent seat's id in the composed org, sorted: the seats
// the projection states and the turns are asked for.
//
// BY AGENT ID, the one identity a turn row carries for its seat — never the
// role name, which two seats may share.
func agentSeats(organization *org.Organization) []string {
	if organization == nil {
		return nil
	}
	var out []string
	for role := range organization.AllRoles() {
		if id, ok := organization.AgentIDFor(role); ok {
			out = append(out, id.String())
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// seedTurns reads each seat's newest turns, [seedTurnReads] seats at a time,
// every read measured from the one instant `at`, and reports whether it asked
// anybody anything.
//
// A seat whose read fails costs that seat, not the others: its turns are
// missing and its error is returned, while the turns and the coverage of every
// read that succeeded are returned beside it. The coverage is returned read by
// read rather than combined, so the caller folds it into the seed's own
// combination exactly as it does its other reads.
func seedTurns(ctx context.Context, history FleetHistory, seats []string, at time.Time) (
	[]store.Turn, []eventfan.Coverage, bool, error,
) {
	var (
		mu     sync.Mutex
		out    []store.Turn
		covers []eventfan.Coverage
		errs   []error
		wg     sync.WaitGroup
		slots  = make(chan struct{}, seedTurnReads)
	)
	asked := false
	for _, seat := range seats {
		if seat == "" {
			continue
		}
		asked = true
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				errs = append(errs, fmt.Errorf("seat %s: %w", seat, ctx.Err()))
				mu.Unlock()
				return
			}
			defer func() { <-slots }()
			page, c, err := history.Turns(ctx, store.TurnQuery{
				AgentID: seat, Sort: store.TurnSortStarted, Limit: SeedTurnsPerSeat, At: at,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("seat %s: %w", seat, err))
				return
			}
			out = append(out, page.Turns...)
			covers = append(covers, c)
		})
	}
	wg.Wait()
	if len(errs) > 0 {
		// ONE ERROR NAMING HOW MANY, rather than a line a seat: a silent
		// peer fails every seat past the first round the same way.
		return out, covers, asked, fmt.Errorf("%d of %d seats' turns: %w",
			len(errs), len(errs)+len(covers), errs[0])
	}
	return out, covers, asked, nil
}

// combine is the coverage of an answer assembled from several reads: a node
// counts as answered only if it answered every one, and the whole is complete
// only when every read was.
//
// NO READ AT ALL IS NOT COMPLETE. A seed with nothing to combine read nothing
// from anybody, and calling that the whole fleet would be the one claim this
// report exists to never make.
func combine(covers []eventfan.Coverage, allRead bool) eventfan.Coverage {
	if len(covers) == 0 {
		return eventfan.Coverage{Nodes: []eventfan.NodeCoverage{}, Complete: false}
	}
	out := covers[0]
	for _, c := range covers[1:] {
		out = out.And(c)
	}
	out.Complete = out.Complete && allRead
	return out
}
