package queries

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// A SEAT'S STOPS ARE FOLDED AS THEY ARE READ, never held as records: a seat
// whose model entry sits on its limit parks and releases on every attempt,
// thousands of times a week, and a read that kept every record had to stop at
// a ceiling and leave the staffing classes unread exactly when a seat could
// not work. Each stop type is read a page at a time with no ceiling, the
// types merged newest first, and each mark paired into a stop as it arrives;
// a stop is kept only as the part of it inside the window, merged with the
// seat's others. What a read holds is a page per type and each seat's
// disjoint stopped stretches inside the window, whatever the records number.

// stopPage is how many records one read of a stream takes.
const stopPage = failedRunsPage

// recordStream is every record of one type in [since, until], newest first,
// read a page at a time as it is consumed.
type recordStream struct {
	s         Sources
	q         store.ListQuery
	eventType string
	page      []store.EventRecord
	at        int
	last      bool
	// pages is how many pages were read, and maxPages, where set, the most
	// the stream reads before it says the span holds more than it will hold.
	pages, maxPages int
}

func (s Sources) streamOf(since, until time.Time, eventType string, failed *bool) *recordStream {
	return &recordStream{s: s, eventType: eventType, q: store.ListQuery{Type: eventType, Failed: failed,
		Since: since, Until: until, Limit: stopPage, At: until}}
}

// next is the stream's next record, false once it holds no more.
func (r *recordStream) next(ctx context.Context) (store.EventRecord, bool, error) {
	rec, ok, err := r.peek(ctx)
	if ok {
		r.at++
	}
	return rec, ok, err
}

// peek is the stream's next record without taking it.
func (r *recordStream) peek(ctx context.Context) (store.EventRecord, bool, error) {
	if r.at < len(r.page) {
		return r.page[r.at], true, nil
	}
	if r.last {
		return store.EventRecord{}, false, nil
	}
	if r.s.Events == nil {
		return store.EventRecord{}, false, errors.New("queries: this node reads no event log")
	}
	if r.maxPages > 0 && r.pages == r.maxPages {
		return store.EventRecord{}, false, fmt.Errorf("queries: the week holds more than %d %s records",
			r.q.Limit*r.maxPages, r.eventType)
	}
	r.pages++
	if len(r.page) > 0 {
		oldest := r.page[len(r.page)-1]
		r.q.Before = &store.Cursor{Time: oldest.Time, ID: oldest.ID}
	}
	listing, coverage, err := r.s.Events.List(ctx, r.q)
	if err != nil {
		return store.EventRecord{}, false, fmt.Errorf("queries: list the %s records: %w", r.eventType, err)
	}
	if !coverage.Complete {
		return store.EventRecord{}, false, fmt.Errorf("queries: not every node answered for its %s records",
			r.eventType)
	}
	r.page, r.at = listing.Rows, 0
	r.last = !listing.More || len(listing.Rows) == 0
	if len(r.page) == 0 {
		return store.EventRecord{}, false, nil
	}
	return r.page[0], true, nil
}

// eachOf hands every record of eventType in [since, until] to fold, newest
// first, with no ceiling: what the caller keeps of them is its own.
func (s Sources) eachOf(ctx context.Context, since, until time.Time, eventType string,
	fold func(store.EventRecord)) error {
	stream := s.streamOf(since, until, eventType, nil)
	for {
		rec, ok, err := stream.next(ctx)
		if err != nil || !ok {
			return err
		}
		fold(rec)
	}
}

// spanSet is one seat's stopped stretches inside [lo, hi), disjoint, not
// touching, and kept newest first, the order a newest-first read adds them in.
type spanSet struct {
	lo, hi time.Time
	spans  []span
	// unknown is a stop whose end the records do not give.
	unknown bool
}

// add merges sp, clipped to the set's window, into the set.
func (set *spanSet) add(sp span) {
	if sp.to.IsZero() {
		set.unknown = true
		return
	}
	sp.from, sp.to = maxTime(sp.from, set.lo), minTime(sp.to, set.hi)
	if !sp.to.After(sp.from) {
		return
	}
	// THE STRETCHES ABOVE sp begin after it ends; from i on, each that
	// reaches it is merged into it.
	i := sort.Search(len(set.spans), func(k int) bool { return !set.spans[k].from.After(sp.to) })
	j := i
	for ; j < len(set.spans) && !set.spans[j].to.Before(sp.from); j++ {
		sp.from, sp.to = minTime(sp.from, set.spans[j].from), maxTime(sp.to, set.spans[j].to)
	}
	set.spans = slices.Replace(set.spans, i, j, sp)
}

// of is the set as [stoppedIn] reads it: a stop of unknown end leaves the
// seat unmeasured.
func (set *spanSet) of() []span {
	out := slices.Clone(set.spans)
	if set.unknown {
		out = append(out, span{from: set.lo})
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// stopPairing pairs one seat's marks of one cause read newest first, as an
// oldest-first read pairs them: a stop runs from the first begin after an end
// to the next end; an end with no begin before it in the history ran from the
// history's floor; and a begin with no end after it stands to the window's end.
type stopPairing struct {
	// end is an end read whose begin is not read yet.
	end    time.Time
	hasEnd bool
	// runFrom and runTo are the run of begins being read: the earliest so far,
	// and the end they began.
	runFrom, runTo time.Time
	inRun          bool
}

// stopFold is every seat's stops as the marks arrive.
type stopFold struct {
	from, to, floor time.Time
	pairs           map[string]map[string]*stopPairing
	sets            map[string]*spanSet
}

func newStopFold(from, to, floor time.Time) *stopFold {
	return &stopFold{from: from, to: to, floor: floor, pairs: map[string]map[string]*stopPairing{},
		sets: map[string]*spanSet{}}
}

func (f *stopFold) set(agent string) *spanSet {
	set, ok := f.sets[agent]
	if !ok {
		set = &spanSet{lo: f.from, hi: f.to}
		f.sets[agent] = set
	}
	return set
}

// mark takes one mark, each no newer than the last one taken.
func (f *stopFold) mark(agent, cause string, begins bool, at time.Time) {
	causes, ok := f.pairs[agent]
	if !ok {
		causes = map[string]*stopPairing{}
		f.pairs[agent] = causes
	}
	p, ok := causes[cause]
	if !ok {
		p = &stopPairing{}
		causes[cause] = p
	}
	switch {
	case !begins:
		if p.inRun {
			f.set(agent).add(span{p.runFrom, p.runTo})
			p.inRun = false
		}
		if p.hasEnd {
			// AN END AFTER AN END: the later one's stop began before the
			// history, read as from its floor.
			f.set(agent).add(span{f.floor, p.end})
		}
		p.end, p.hasEnd = at, true
	case p.hasEnd:
		p.runFrom, p.runTo, p.inRun, p.hasEnd = at, p.end, true, false
	case p.inRun:
		// A BEGIN WHILE STOPPED: the stop began at the earlier one.
		p.runFrom = at
	default:
		// A BEGIN NOTHING ENDED: the stop stands to the window's end.
		p.runFrom, p.runTo, p.inRun = at, f.to, true
	}
}

// done closes what the history ends with.
func (f *stopFold) done() {
	for agent, causes := range f.pairs {
		for _, p := range causes {
			if p.inRun {
				f.set(agent).add(span{p.runFrom, p.runTo})
			}
			if p.hasEnd {
				f.set(agent).add(span{f.floor, p.end})
			}
		}
	}
}

// seatStops is each seat's stops by agent id, read from its own records: its
// pause, open since before the event history where it is; its pause and model
// limit events over the history; and its budget refusals, each until its
// window reset. A stop whose end its records do not give leaves the seat
// unmeasured. Each seat's stops are clipped to [from, to) and merged.
func (s Sources) seatStops(ctx context.Context, from, to, now time.Time) (map[string][]span, error) {
	floor := now.Add(-store.EventHistory)
	fold := newStopFold(from, to, floor)
	streams := make([]*recordStream, len(stopEvents))
	for i, kind := range stopEvents {
		streams[i] = s.streamOf(floor, to, kind.kind, nil)
	}
	for {
		best := -1
		var newest store.EventRecord
		for i, stream := range streams {
			rec, ok, err := stream.peek(ctx)
			if err != nil {
				return nil, err
			}
			// AT ONE INSTANT, AN END BEFORE A BEGIN: read oldest first, the
			// begin came first ([stopEvents] lists each begin before its end).
			if ok && (best < 0 || rec.Time.After(newest.Time) || rec.Time.Equal(newest.Time)) {
				best, newest = i, rec
			}
		}
		if best < 0 {
			break
		}
		if _, _, err := streams[best].next(ctx); err != nil {
			return nil, err
		}
		kind := stopEvents[best]
		fold.mark(newest.AgentID, kind.cause, kind.begins, newest.Time.UTC())
	}
	fold.done()
	// A PAUSE OLDER THAN THE HISTORY has no event left, only its record.
	if s.Pauses != nil {
		pauses, err := s.Pauses.ListSeatPauses(ctx)
		if err != nil {
			return nil, fmt.Errorf("queries: list the seats' pauses: %w", err)
		}
		for _, p := range pauses {
			if p.At.Before(floor) {
				fold.set(p.Seat.String()).add(span{p.At, to})
			}
		}
	}
	if err := s.refusalStops(ctx, maxTime(from.AddDate(0, -1, 0), floor), to, fold); err != nil {
		return nil, err
	}
	out := make(map[string][]span, len(fold.sets))
	for agent, set := range fold.sets {
		out[agent] = set.of()
	}
	return out, nil
}

// refusalStops folds every budget refusal of a charted seat in [since, to]
// into fold, each standing until its window resets. When the window resets is
// in the record's payload alone, so it is read once per seat and window: a
// refusal inside a window already read, which the company's clock places by
// the window's period, is that window's.
func (s Sources) refusalStops(ctx context.Context, since, to time.Time, fold *stopFold) error {
	type window struct{ from, resets time.Time }
	known := map[string]window{}
	chart := s.chartHandles()
	zone := s.zone()
	stream := s.streamOf(since, to, types.BudgetExhausted{}.EventType(), nil)
	for {
		rec, ok, err := stream.next(ctx)
		if err != nil || !ok {
			return err
		}
		if _, charted := chart[rec.AgentID]; !charted {
			continue
		}
		at := rec.Time.UTC()
		w, read := known[rec.AgentID]
		if !read || at.Before(w.from) || !at.Before(w.resets) {
			full, _, err := s.Events.ByID(ctx, rec.ID)
			if err != nil {
				return fmt.Errorf("queries: read budget refusal %s: %w", rec.ID, err)
			}
			var refused types.BudgetExhausted
			if err := json.Unmarshal(full.Payload, &refused); err != nil {
				return fmt.Errorf("queries: decode budget refusal %s: %w", rec.ID, err)
			}
			w = window{}
			if resets, perr := time.Parse(time.RFC3339, refused.ResetsAt); perr == nil {
				w = window{from: windowStart(resets, refused.Period, zone), resets: resets.UTC()}
			}
			known[rec.AgentID] = w
		}
		if w.resets.IsZero() {
			fold.set(rec.AgentID).add(span{from: at})
			continue
		}
		fold.set(rec.AgentID).add(span{at, w.resets})
	}
}

// windowStart is when the budget window of period resetting at resets began on
// the company's clock; resets itself for a period this build does not know,
// so no other refusal is taken for that window's.
func windowStart(resets time.Time, period string, zone *time.Location) time.Time {
	local := resets.In(zone)
	switch period {
	case "day":
		return local.AddDate(0, 0, -1).UTC()
	case "week":
		return local.AddDate(0, 0, -7).UTC()
	case "month":
		return local.AddDate(0, -1, 0).UTC()
	}
	return resets.UTC()
}
