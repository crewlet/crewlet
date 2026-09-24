// The list of turns — the one view of a working company that did not exist.

package queries

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// turns lists one row per unit of agent work, newest first.
//
// A TURN IS THE UNIT OF WORK THIS ENGINE DOES — a wake, a decision, some tool
// rounds, a reply — and everything else is a projection of one: the spend
// rollup, the seat page, an item's history. None of them is a LIST of them.
// The dashboard faked one by paging the raw event feed sixty-one times and
// folding the rows in the browser: slow, capped at whatever the caller gave up
// on, and wrong at the page boundary, where a turn straddling two pages
// appeared twice.
//
// THE CURSOR IS THE TURN'S START, not an event's, because that is what the
// listing is ordered by — and it is exactly the defect the browser-side fold
// had: a keyset on any one event pages a turn twice.
func (s Sources) turns(ctx context.Context, p Params) (any, error) {
	q := store.TurnQuery{
		SinceDays: p.Int("days", 0),
		AgentRole: strings.TrimSpace(p.String("role")),
		AgentID:   strings.TrimSpace(p.String("agent_id")),
		Model:     strings.TrimSpace(p.String("model")),
		// EVERY ATTEMPT AT ONE TRIGGER. A turn id names one run now, so
		// a redelivered trigger is several rows here — and this is how a
		// reader asks for the others. See ADR-0017.
		WorkKey: strings.TrimSpace(p.String("work_key")),
		Limit:   p.Int("limit", 0),
	}
	// FAILED IS THREE-VALUED, and the third value is the default: nil is
	// every turn, true is the ones that carried a failure, false is the
	// ones that did not. Folding the absent case into `false` would make
	// an unparameterised list hide every failing turn — which is the one
	// an operator opens this screen for.
	if raw := strings.TrimSpace(p.String("failed")); raw != "" {
		switch raw {
		case "true":
			yes := true
			q.Failed = &yes
		case "false":
			no := false
			q.Failed = &no
		default:
			return nil, badParams("failed", raw, []string{"true", "false"})
		}
	}
	// THE ORDER, a closed set the store owns: newest first, or the most
	// tokens first — the spend screen's "which turns cost the most", which
	// the daily usage rows cannot answer because they hold no turn.
	if raw := strings.TrimSpace(p.String("sort")); raw != "" {
		q.Sort = store.TurnSort(raw)
		if !q.Sort.Valid() {
			return nil, badParams("sort", raw, names(store.TurnSorts))
		}
	}
	// THE TURNS ON ONE WORK ITEM, by its identity across trackers — see
	// workItemParam for why a malformed one is refused rather than matched.
	item, err := workItemParam(p)
	if err != nil {
		return nil, err
	}
	q.WorkItem = item
	before, err := instantParam(p, "before")
	if err != nil {
		return nil, err
	}
	q.Before = before
	if q.Sort == store.TurnSortTokens && !before.IsZero() {
		// A RANKING HAS NO POSITION TO RESUME FROM, and paging one by
		// start time would mix two orders on one screen.
		return nil, fmt.Errorf("%w: sort=%s is a ranking and takes no before=; "+
			"narrow the window with days= instead", ErrBadParams, q.Sort)
	}

	page, coverage, err := s.Events.Turns(ctx, q)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"turns": page.Turns, "next": nil, "coverage": coverage}
	if page.Next != nil {
		// THE CURSOR THE CALLER RESUMES FROM, echoed rather than left for
		// a client to assemble — the same rule the event list follows,
		// because a client that built it from the last row's fields would
		// be reimplementing the one thing that must not drift. It is the
		// FLEET's, and so it can be present on an empty page: when a
		// node's page filled before any turn above it could be shown,
		// the cursor is where that node stopped rather than the end.
		out["next"] = page.Next.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}
