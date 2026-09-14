package observe

import (
	"context"
	"errors"
	"fmt"

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
//   - the spend: every phase record inside [livestate.LiveSpendWindow], read
//     from the promoted token columns rather than the payloads, and cut to the
//     projection's record cap by the projection itself.
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
func Seed(ctx context.Context, history EventHistory, live Seeded) error {
	var h livestate.History
	var errs []error

	rows, err := history.List(ctx, store.ListQuery{Limit: livestate.EventFeedLimit})
	if err != nil {
		errs = append(errs, fmt.Errorf("observe: read the activity feed's history: %w", err))
	}
	h.Events = make([]livestate.FeedRow, 0, len(rows))
	for _, rec := range rows {
		h.Events = append(h.Events, FeedRow(rec))
	}

	h.Spend, err = history.PhaseTokens(ctx, store.PhaseTokenQuery{
		SinceDays: livestate.LiveSpendWindowDays(),
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("observe: read the live spend window's history: %w", err))
	}

	live.Seed(h)
	return errors.Join(errs...)
}
