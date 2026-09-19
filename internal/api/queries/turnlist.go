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
		Limit:     p.Int("limit", 0),
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
	before, err := instantParam(p, "before")
	if err != nil {
		return nil, err
	}
	q.Before = before

	rows, err := s.Events.Turns(ctx, q)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"turns": rows, "next": nil}
	if len(rows) > 0 {
		// THE CURSOR THE CALLER RESUMES FROM, echoed rather than left for
		// a client to assemble — the same rule the event list follows,
		// because a client that built it from the last row's fields would
		// be reimplementing the one thing that must not drift.
		out["next"] = rows[len(rows)-1].StartedAt.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}
