package observe

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
)

// EventHistory is what seeding reads from this node's event store. Satisfied
// by *store.EventLog.
type EventHistory interface {
	List(ctx context.Context, q store.ListQuery) ([]store.EventRecord, error)
	PhaseTokens(ctx context.Context, q store.PhaseTokenQuery) ([]tokens.Record, error)
}

// Seeded is the projection history is folded into. Satisfied by
// *livestate.LiveState.
type Seeded interface {
	Seed(livestate.History) livestate.Change
}

// Seed fills a freshly started projection from this node's event store.
//
// TWO BOUNDED READS, and each bound is the projection's own, so a seed can
// never hold more than the live stream could have built:
//
//   - the feed: the newest [livestate.EventFeedLimit] persisted events, inside
//     the store's own read floor ([store.EventHistory]). Payload-free, because
//     a feed row carries none;
//   - the spend: the newest [livestate.SpendRecordLimit] phase records inside
//     [livestate.LiveSpendWindow], read from the promoted token columns rather
//     than the payloads. The count is the projection's record cap, applied
//     at the READ: a busy day past it was otherwise read in full inside the
//     seed's time budget, only to be cut to the cap on arrival, and a read
//     that ran out of budget seeded no spend at all.
//
// THE SPEND WINDOW IS ASKED FOR AS AN INSTANT, never as a day count. The two
// are the same window only while [livestate.LiveSpendWindow] is a whole number
// of days, and a window of 36 hours asked for in days would seed 24 of them
// while the projection kept all 36 — less history than the screen claims to
// cover, with nothing to say so.
//
// Called AFTER the broadcast subscription is attached, so an event published
// between the read and the subscription cannot fall into the gap; the
// projection recognises one that arrives both ways. See [livestate.History]
// for why the seeded history is this node's own on a fleet.
//
// BEST EFFORT per half: a feed that cannot be read does not cost the spend,
// and either failure is returned for the caller to report, because what it
// costs is visible nowhere else. The screens still render; they start at
// this process's boot.
//
// INDEPENDENT IN TIME TOO, which one shared deadline could not give. The
// caller bounds this whole call so a store that will not answer cannot hold
// the listener shut, and the reads are sequential — so on the failure that
// budget actually exists for, a store that is SLOW rather than broken, the
// feed read spent the whole of it and the spend read was handed an expired
// context and returned without touching the database. One slow half cost
// both. The feed is given half of what is left and the spend keeps the rest,
// so neither can starve the other and the caller's ceiling still binds: a
// feed that runs long is cut at its own half, and a feed that returns in
// milliseconds leaves the spend almost all of the budget.
func Seed(ctx context.Context, history EventHistory, live Seeded) error {
	var h livestate.History
	var errs []error

	feedCtx, cancelFeed := halfTheBudget(ctx)
	rows, err := history.List(feedCtx, store.ListQuery{Limit: livestate.EventFeedLimit})
	cancelFeed()
	if err != nil {
		errs = append(errs, fmt.Errorf("observe: read the activity feed's history: %w", err))
	}
	h.Events = make([]livestate.FeedRow, 0, len(rows))
	for _, rec := range rows {
		h.Events = append(h.Events, FeedRow(rec))
	}

	h.Spend, err = history.PhaseTokens(ctx, store.PhaseTokenQuery{
		Since: time.Now().UTC().Add(-livestate.LiveSpendWindow),
		Limit: livestate.SpendRecordLimit,
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("observe: read the live spend window's history: %w", err))
	}

	live.Seed(h)
	return errors.Join(errs...)
}

// halfTheBudget bounds the first of the two reads by half the time left, so a
// slow one cannot spend the whole budget the caller set for both.
//
// A context with no deadline is handed back untouched: there is nothing to
// halve, and the two reads are bounded by whatever bounds the caller instead.
func halfTheBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx, func() {}
	}
	left := time.Until(deadline)
	if left <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, left/2)
}
