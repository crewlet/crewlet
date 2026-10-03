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
type FleetHistory interface {
	List(ctx context.Context, q store.ListQuery) (eventfan.Listing, eventfan.Coverage, error)
	PhaseTokens(ctx context.Context, q store.PhaseTokenQuery) ([]tokens.Record, eventfan.Coverage, error)
	Turns(ctx context.Context, q store.TurnQuery) (eventfan.TurnPage, eventfan.Coverage, error)
}

// Seeded is the projection history is folded into. Satisfied by
// *livestate.LiveState.
type Seeded interface {
	Seed(livestate.History) livestate.Change
}

// SeedTurnsPerRole is how many of a seat's newest turns the seed reads to find
// the last one it ENDED.
//
// THREE, because a seat runs one turn at a time: its newest turn may be the one
// in flight, and the one before it may be parked on a coding run — a seat with
// a run in flight takes no new work until it is collected — so the newest
// ended turn is among the newest three unless the seat's recent turns died
// mid-flight, which leaves nothing the store could honestly call an end. It is
// asked per seat, rather than as one company-wide page, because a page of the
// company's newest turns says nothing about a seat that has been quiet longer
// than everyone else's recent work.
const SeedTurnsPerRole = 3

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

// Seed fills a freshly started projection from the FLEET's event stores.
//
// THREE BOUNDED READS, each bound the projection's own, so a seed can never
// hold more than the live stream could have built:
//
//   - the feed: the newest [livestate.EventFeedLimit] persisted events, inside
//     the store's own read floor ([store.EventHistory]). Payload-free, because
//     a feed row carries none;
//   - the spend: the newest [livestate.SpendRecordLimit] phase records inside
//     [livestate.LiveSpendWindow], read from the promoted token columns rather
//     than the payloads. The count is the projection's record cap, applied at
//     the READ, on every node: a busy day past it was otherwise read in full
//     inside the seed's time budget, only to be cut to the cap on arrival;
//   - each seat's newest [SeedTurnsPerRole] turns, for the seat's last turn
//     and a turn it left parked on a coding run.
//
// FROM EVERY NODE. Each node's event store holds what that node published, so a
// seed read from this node's alone showed a restarted node only its own share
// of the company, and a node that joined a fleet showed nothing its peers had
// done. Which nodes answered is recorded on the projection
// ([livestate.LiveState.SeededFrom]) rather than hidden.
//
// THE SPEND WINDOW IS ASKED FOR AS AN INSTANT, never as a day count. The two
// are the same window only while [livestate.LiveSpendWindow] is a whole number
// of days, and a window of 36 hours asked for in days would seed 24 of them
// while the projection kept all 36 — less history than the screen claims to
// cover, with nothing to say so.
//
// Called AFTER the broadcast subscription is attached, so an event published
// between the read and the subscription cannot fall into the gap; the
// projection recognises one that arrives both ways.
//
// BEST EFFORT per read: one that cannot be answered does not cost the others,
// and every failure is returned for the caller to report, because what it
// costs is visible nowhere else. The screens still render; they start at this
// process's boot.
//
// CONCURRENT, which is what makes each read independent in time as well as in
// failure. The caller bounds this whole call so a fleet that will not answer
// cannot hold the listener shut; read one after another, a SLOW first read
// spent that whole budget and handed the next an expired context, so one slow
// read cost them all. Side by side, each has the caller's whole budget and the
// seed costs the slowest of them rather than their sum.
func Seed(ctx context.Context, history FleetHistory, roles []string, live Seeded) error {
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

	wg.Go(func() {
		listing, c, err := history.List(ctx, store.ListQuery{Limit: livestate.EventFeedLimit})
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
		records, c, err := history.PhaseTokens(ctx, store.PhaseTokenQuery{
			Since: time.Now().UTC().Add(-livestate.LiveSpendWindow),
			Limit: livestate.SpendRecordLimit,
		})
		if report(c, err, "the live spend window's history") {
			h.Spend = records
		}
	})
	wg.Go(func() {
		turns, cs, asked, err := seedTurns(ctx, history, roles)
		// A company with no agent seat asks nobody anything, and a read
		// that was never made says nothing about who answered it.
		if !asked {
			return
		}
		// NOT THROUGH report: that is the shape of ONE read, all or
		// nothing, and this is one read per seat. A seat whose read
		// failed costs that seat — its error is reported and the seed is
		// incomplete — while every seat that WAS read keeps its last turn
		// and its parked turn. Routed through report, one silent role
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
	return errors.Join(errs...)
}

// seedTurns reads each role's newest turns, [seedTurnReads] roles at a time,
// and reports whether it asked anybody anything.
//
// A role whose read fails costs that role, not the others: its turns are
// missing and its error is returned, while the turns and the coverage of every
// read that succeeded are returned beside it. The coverage is returned read by
// read rather than combined, so the caller folds it into the seed's own
// combination exactly as it does its other reads.
func seedTurns(ctx context.Context, history FleetHistory, roles []string) (
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
	for _, role := range slices.Compact(slices.Sorted(slices.Values(roles))) {
		if role == "" {
			continue
		}
		asked = true
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				errs = append(errs, fmt.Errorf("role %s: %w", role, ctx.Err()))
				mu.Unlock()
				return
			}
			defer func() { <-slots }()
			page, c, err := history.Turns(ctx, store.TurnQuery{
				AgentRole: role, Sort: store.TurnSortStarted, Limit: SeedTurnsPerRole,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("role %s: %w", role, err))
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
