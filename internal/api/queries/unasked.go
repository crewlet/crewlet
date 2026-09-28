// Four readers that were written, tested, and asked by nothing.
//
// Each of these has existed for as long as the subsystem behind it: the item
// search a seat calls with `search_work_items`, the conversation ledger that stops a
// seat replying twice in one thread, the counterparty profiles the learning
// loop writes, and the per-recipient routing the tracker's applier records.
// Every one of them is read by the engine itself and reaches no screen, which
// is the quietest way for a feature to be absent — it is not missing, it is
// merely unaddressable.

package queries

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning/memread"
	"github.com/crewlet/crewlet/internal/tracker"
)

// WorkSearcher is the ranked item search, as this surface needs it.
//
// Consumer-defined like every other seam here, and the READ half only: the
// index is this node's own and the rows are the fleet's, which the searcher
// already reconciles.
type WorkSearcher interface {
	Search(ctx context.Context, q tracker.SearchQuery) (tracker.SearchAnswer, error)
}

// SeatMemory is a seat's memory and its conversation ledger, answered by the
// node holding the seat — the two reads this surface makes of
// [memread.Reader].
//
// Consumer-defined like every other seam here, and over memread's own answer
// types for the reason [ScheduleRuns] is over `schedule.Run`: the rows stay one
// shape rather than a copy this package would have to keep matching, and the
// shape is built where every node — not only one serving this API — can build
// it.
type SeatMemory interface {
	Memory(ctx context.Context, handle string, limit int) (memread.Memory, error)
	Threads(ctx context.Context, handle, conversation string, limit int) (memread.Threads, error)
}

// DefaultSearchLimit is what a caller that names none gets.
//
// TWENTY-FIVE, against a seat's own [tracker.SearchLimit] of 20 and under the
// reader's [tracker.MaxSearchLimit] ceiling of 50 — so it is a real default
// rather than a second spelling of either. The two callers want different
// numbers: a tool's answer is read into a PROMPT, where every row costs
// context the turn could spend on the work, and a screen's is SCANNED, where a
// reader flicks past a row in a fraction of a second and the cost of one more
// is a pixel. Twenty-five is about two screenfuls at `--row-h`, which is where
// a reader stops scanning and re-types the query instead — past that the rows
// are paid for and never read.
const DefaultSearchLimit = 25

// workSearch ranks the company's work against plain text.
//
// THE REFUSAL THAT IS NOT A FAILURE: an index still catching up answers
// `building` rather than an error, because nothing is wrong — this node joined
// recently and the company's work is simply not all searchable from here yet.
// Reported as an empty result with a reason, exactly as `knowledge` reports
// its own four unavailable states, so a screen says "try again in a moment"
// instead of "nothing matches" — which a reader acts on by filing a duplicate.
//
// THE SAME THREE MODES AS `knowledge`, and the same four outcome fields —
// `served_mode`, `modes`, `coverage`, `degraded` — so one screen control
// drives both searches and says the same thing about each.
func (s Sources) workSearch(ctx context.Context, p Params) (any, error) {
	text := strings.TrimSpace(p.String("q"))
	if text == "" {
		return nil, badParams("q", "", nil)
	}
	mode, err := knowledge.ParseMode(p.String("mode"))
	if err != nil {
		return nil, badParams("mode", p.String("mode"), modeNames())
	}
	answer, err := s.WorkSearch.Search(ctx, tracker.SearchQuery{
		Text: text, Limit: p.Int("limit", DefaultSearchLimit), Mode: mode,
	})
	switch {
	case errors.Is(err, tracker.ErrIndexBuilding):
		out := map[string]any{
			"hits":      []tracker.Ranked{},
			"mode":      string(mode),
			"available": false,
			"reason":    "building",
			"note": "this node joined recently and is still indexing the company's work — " +
				"items that exist are simply not findable from here yet",
		}
		outcome(out, knowledge.Outcome{})
		return out, nil
	case err != nil:
		return nil, err
	}
	hits := answer.Hits
	if hits == nil {
		// An EMPTY SLICE, never null: a client that renders `hits.length`
		// on the answer should not have to guard the field as well.
		hits = []tracker.Ranked{}
	}
	out := map[string]any{"hits": hits, "mode": string(mode), "available": true}
	outcome(out, answer.Outcome)
	return out, nil
}

// conversations answers one seat's external threads, ANSWERED BY THE SEAT'S
// HOLDER — see [Sources.Memory].
//
// The ledger is what stops a seat replying twice in one chat thread, and it is
// the only record of what a seat said on a surface this engine does not own.
//
// TWO SHAPES IN ONE ANSWER, because the screen asks two questions with one
// navigation: which threads this seat is in, and — when one is named — what it
// said in that one. Split into two questions the second would need the first's
// answer to know what to ask for. The page is [memread.ThreadPage]'s, and the
// listing carries the seat's whole count beside it.
func (s Sources) conversations(ctx context.Context, p Params) (any, error) {
	handle, err := s.viewerHandle(ctx, strings.TrimSpace(p.String("handle")))
	if err != nil {
		return nil, err
	}
	return s.Memory.Threads(ctx, handle, strings.TrimSpace(p.String("conversation")),
		p.Int("limit", 0))
}

// workRouting answers who one change woke, and under which reason.
//
// THE FACT NO OTHER TRACKER RECORDS. Every tracker can tell you that somebody
// was notified; this one records, per change and per recipient, the ONE reason
// of eighteen it reached them under, whether it ASKS something of them, and
// whether they were reached only because nobody better was found. The applier
// has written exactly that set since the domain landed and its only trace on
// any surface was a boolean — `work_activity.notified` — saying that somebody,
// somewhere, was told.
func (s Sources) workRouting(ctx context.Context, p Params) (any, error) {
	record := strings.TrimSpace(p.String("record_id"))
	if record == "" {
		return nil, badParams("record_id", "", nil)
	}
	fresh, err := freshness(p)
	if err != nil {
		return nil, err
	}
	answer, err := s.Work.Routing(ctx, tracker.RoutingQuery{
		RecordID: record,
		// THE HORIZON THIS COMPANY IS ACTUALLY RUNNING, read per call
		// from the current epoch — which is the whole reason the
		// reader takes it rather than holding it. An absent recipient
		// set is dated against it: older than the horizon and its
		// absence is not evidence, newer and it is.
		Retention:   s.inboxRetention(),
		Level:       fresh.Level,
		MaxLag:      fresh.MaxLag,
		MaxLagSeq:   fresh.MaxLagSeq,
		MinPosition: fresh.MinPosition,
	}, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	return answer, nil
}

// inboxRetention is the company's own inbox horizon, or zero when this process
// cannot say.
//
// ZERO IS "I DO NOT KNOW", which the reader answers `unknown` to rather than
// guessing — see [tracker.DeliveryUnknown]. A registry wired without a company
// source genuinely cannot say how long this company keeps a notice, and
// answering with the shipped default would date a set against a retention
// nobody here is running.
func (s Sources) inboxRetention() time.Duration {
	if s.Company == nil {
		return 0
	}
	company := s.Company()
	if company == nil {
		return 0
	}
	return company.Tracker.Native.InboxRetention()
}
