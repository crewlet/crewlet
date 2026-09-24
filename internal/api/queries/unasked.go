// Four readers of records the engine keeps for its own use, answered for a
// screen: the item search a seat calls with `search_work_items`, the
// conversation ledger that stops a seat replying twice in one thread, the
// counterparty profiles the learning loop writes, and the per-recipient
// routing the tracker's applier records.

package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/tracker"
)

// WorkSearcher is the ranked item search, as this surface needs it.
//
// Consumer-defined like every other seam here, and the READ half only: the
// index is this node's own and the rows are the fleet's, which the searcher
// already reconciles.
type WorkSearcher interface {
	Search(ctx context.Context, text string, limit int) (tracker.Ranking, error)
}

// Conversations is the seat's own thread ledger, as this surface needs it.
//
// Consumer-defined like every other seam here, and over the ledger's own named
// types for the reason [ScheduleRuns] is over `schedule.Run`: the dependency is
// on the two METHODS, so the SQL ledger and a test twin satisfy it alike, while
// the rows stay one shape rather than a copy this package would have to keep
// matching.
type Conversations interface {
	Threads(ctx context.Context, handle string, limit int) ([]ledgerstore.Thread, error)
	History(ctx context.Context, handle, conversation string, limit int) ([]ledger.Session, error)
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
// its own unavailable states, so a screen says "try again in a moment"
// instead of "nothing matches" — which a reader acts on by filing a duplicate.
//
// ONE SHAPE FOR EVERY ANSWER: `hits`, `available`, `reason`, `note` and
// `partial`, the last null unless the ranking is missing part of what it
// searched — see [tracker.SearchPartial].
func (s Sources) workSearch(ctx context.Context, p Params) (any, error) {
	text := strings.TrimSpace(p.String("q"))
	if text == "" {
		return nil, badParams("q", "", nil)
	}
	ranking, err := s.WorkSearch.Search(ctx, text, p.Int("limit", DefaultSearchLimit))
	switch {
	case errors.Is(err, tracker.ErrIndexBuilding):
		return map[string]any{
			"hits":      []tracker.Ranked{},
			"available": false,
			"reason":    "building",
			"note": "this node joined recently and is still indexing the company's work — " +
				"items that exist are simply not findable from here yet",
			"partial": nil,
		}, nil
	case errors.Is(err, tracker.ErrSearchLimit):
		// A REFUSAL OF THE REQUEST, in the tracker's own words, which
		// name `limit` and the most it takes. Left to the default it
		// reaches a client as a 500 — a fault of this node, for a number
		// the caller has to change.
		return nil, fmt.Errorf("%w: %w", ErrBadParams, err)
	case err != nil:
		return nil, err
	}
	hits := ranking.Items
	if hits == nil {
		// An EMPTY SLICE, never null: a client that renders `hits.length`
		// on the answer should not have to guard the field as well.
		hits = []tracker.Ranked{}
	}
	out := map[string]any{
		"hits": hits, "available": true, "reason": "", "note": "",
		"partial": nil,
	}
	if ranking.Partial != nil {
		// WHAT THE RANKING IS MISSING, beside what it found: a short list
		// with nothing beside it reads as every match there is.
		out["partial"] = ranking.Partial
		out["note"] = partialNote(knowledgePartial(ranking.Partial), "items")
	}
	return out, nil
}

// knowledgePartial is a ranked item search's partial on the knowledge seam's
// terms, so both search answers here say one state in one sentence.
//
// A CONVERSION, which the compiler refuses the day the two stop carrying the
// same four facts: both come from the same fan-out's count.
func knowledgePartial(p *tracker.SearchPartial) *knowledge.Partial {
	converted := knowledge.Partial(*p)
	return &converted
}

// The THREAD-LIST page, and its ceiling.
//
// LOAD-BEARING, because that list is UNBOUNDED WITHOUT IT: `Threads` applies a
// `LIMIT` only when one is positive, so a zero or a negative reads every key a
// seat has ever spoken under through this process, and a seat on a busy chat
// workspace accumulates one per channel for as long as the retention sweep
// keeps them.
//
// IT DOES NOT GOVERN A THREAD'S HISTORY: one page size served both reads, and
// they are not the same question. A thread's entries are bounded by the
// WRITER — the ledger trims to `turn_engine.conversation_session.max_entries`
// on every write — so a second cut here could only drop a conversation's
// OPENING on a company that raised the knob, since `History` orders
// newest-first to make `LIMIT` keep the recent turns and reverses afterwards.
// It also tied the two together: asking for a longer channel roster changed
// how much of a thread came back with it.
//
// FIFTY is the default, which is [tracker.MaxInboxRows]' own figure for the
// same reason: this list is WORKED rather than scrolled — a reader is looking
// for one thread — and a page longer than one sitting is a page whose tail is
// never reached.
//
// TWO HUNDRED is the ceiling, which is [learning.MaxProfilesListed]' figure
// for its own reason: it is the size of the company this engine is built for
// plus the correspondents one seat accumulates on the surfaces it works. Below
// it a seat's whole set is one answer; above it the set is a report, and this
// screen is not one.
const (
	DefaultConversationPage = 50
	MaxConversationPage     = 200
)

// conversationPage clamps what a caller asked for, in the tracker's own idiom:
// an absent or nonsensical limit takes the default, and one past the ceiling
// is served the ceiling rather than refused — a page size is not a filter, so
// answering less than was asked for costs a reader nothing they can act on.
func conversationPage(asked int) int {
	switch {
	case asked <= 0:
		return DefaultConversationPage
	case asked > MaxConversationPage:
		return MaxConversationPage
	}
	return asked
}

// conversations answers one seat's external threads.
//
// The ledger is what stops a seat replying twice in one chat thread, and it is
// the only record of what a seat said on a surface this engine does not own.
// It has been typed on the client since the client had types and registered
// nowhere, so the screen that reads it rendered an empty list for every seat.
//
// TWO SHAPES IN ONE ANSWER, because the screen asks two questions with one
// navigation: which threads this seat is in, and — when one is named — what it
// said in that one. Split into two questions the second would need the first's
// answer to know what to ask for.
func (s Sources) conversations(ctx context.Context, p Params) (any, error) {
	handle, err := s.viewerHandle(ctx, strings.TrimSpace(p.String("handle")))
	if err != nil {
		return nil, err
	}
	limit := conversationPage(p.Int("limit", 0))
	// ONE ROW PAST THE PAGE, the evidence idiom this tree uses everywhere a
	// read is cut: a page that filled is otherwise indistinguishable from a
	// seat holding exactly that many threads, and the probe row is dropped
	// before anything renders it.
	threads, err := s.Conversations.Threads(ctx, handle, limit+1)
	if err != nil {
		return nil, err
	}
	truncated := len(threads) > limit
	if truncated {
		threads = threads[:limit]
	}
	rows := make([]map[string]any, 0, len(threads))
	for _, t := range threads {
		rows = append(rows, map[string]any{
			"key":     t.Key,
			"turns":   t.Entries,
			"last_at": t.LastAt,
		})
	}
	out := map[string]any{
		"handle":        handle,
		"conversations": rows,
		"entries":       []ledger.Session{},
		"available":     true,
		// SAYS WHAT IS MISSING. Only the roster is cut here — the entries
		// below are what the ledger holds. The threads past the page are
		// the ones this seat spoke in least recently: the roster is ordered
		// by each thread's newest entry. A larger `limit` lists more of
		// them, up to [MaxConversationPage]; PAST THAT NO ANSWER LISTS THEM,
		// because the roster read takes no cursor, and `conversation=<key>`
		// reads a thread only for a caller that already holds its key.
		"truncated": truncated,
	}
	if key := strings.TrimSpace(p.String("conversation")); key != "" {
		// WHAT THE SEAT ITSELF WOULD SEE: the same unbounded read the turn
		// path takes on every turn, because the bound is the trim rather
		// than a page. A number here could only ever be wrong — the value
		// that would make it right is
		// `turn_engine.conversation_session.max_entries`, which this layer
		// does not hold — and being wrong low cuts a thread's opening off a
		// screen whose whole purpose is reading the thread.
		//
		// WHAT THE TRIM AND THE AGE SWEEP REMOVED IS COUNTED, not lost
		// silently: each entry carries its `ordinal` in the conversation's
		// whole record, so the oldest one's ordinal minus one is how many
		// earlier turns this ledger no longer holds. Those are readable
		// only in the thread itself, on the surface it was held on.
		entries, err := s.Conversations.History(ctx, handle, key, 0)
		if err != nil {
			return nil, err
		}
		if entries != nil {
			out["entries"] = entries
		}
	}
	return out, nil
}

// workRouting answers who one change woke, and under which reason.
//
// THE FACT NO OTHER TRACKER RECORDS. Every tracker can tell you that somebody
// was notified; this one records, per change and per recipient, the ONE reason
// of twenty it reached them under, whether it ASKS something of them, and
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

// Counterparties is the profiles the learning loop keeps about WHO a seat has
// worked with — the one memory object that is about somebody else.
type Counterparties interface {
	List(ctx context.Context, observer string) ([]learning.Profile, bool, error)
}

// counterpartiesFor is the memory answer's own half of this, kept beside the
// seam so a nil reader and an empty list are told apart in one place.
//
// AN ERROR IS NOT AN EMPTY LIST. Everything in `learning` is best effort by
// design and a failed read answers empty — but that rule is about the TURN,
// which must not die because a diary was slow. A screen reporting "this seat
// has worked with nobody" when the truth is "the store could not be reached"
// is the same collapse the coordination layer's three-valued answers exist to
// prevent, so the failure is returned and the surface says so.
func (s Sources) counterpartiesFor(ctx context.Context, observer string) ([]learning.Profile, bool, error) {
	if s.Counterparties == nil || observer == "" {
		return nil, false, nil
	}
	profiles, truncated, err := s.Counterparties.List(ctx, observer)
	if err != nil {
		return nil, false, fmt.Errorf("counterparties for %s: %w", observer, err)
	}
	return profiles, truncated, nil
}
