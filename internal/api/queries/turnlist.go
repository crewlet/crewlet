// The list of turns — the one view of a working company that did not exist.

package queries

import (
	"context"
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
	// THE CURSOR IS A PAIR, (start, turn id), for the reason
	// [store.TurnQuery.Before] gives: a start is an event's time and is not
	// unique, so a cursor on it alone steps over every other turn that began
	// at the boundary's instant. [beforeCursor] refuses half of one rather
	// than reading it as the first page, which a pager would follow for ever.
	before, err := beforeCursor(p)
	if err != nil {
		return nil, err
	}
	q.Before = before

	rows, more, err := s.Events.Turns(ctx, q)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"turns": rows,
		// WHETHER THE WINDOW HOLDS MORE than this page, read by the store as
		// one row past the limit rather than inferred from a page that
		// filled: a window of exactly `limit` turns and one of ten thousand
		// answer with the same rows. Anything a reader draws from the page —
		// a count, a histogram — describes the newest `limit` turns when
		// this is true, and the rest is `next`.
		"truncated": more,
		"next":      nil,
	}
	if more {
		// THE CURSOR THE CALLER RESUMES FROM, echoed rather than left for
		// a client to assemble — the same rule the event list follows,
		// because a client that built it from the last row's fields would
		// be reimplementing the one thing that must not drift. Offered only
		// when the store read a row past this page, so a pager stops at the
		// end of the window rather than asking for an empty page.
		last := rows[len(rows)-1]
		out["next"] = map[string]string{
			"before_time": last.StartedAt.UTC().Format(time.RFC3339Nano),
			"before_id":   last.TurnID,
		}
	}
	return out, nil
}
