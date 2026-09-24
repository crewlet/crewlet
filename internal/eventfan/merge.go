package eventfan

import (
	"cmp"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// THE MERGES, as pure functions over values.
//
// Pure for the reason internal/search keeps its fusion pure: a merge that can
// only be exercised through a broker and several databases is a merge nobody
// re-checks, and every one of these has a boundary case — a page that filled,
// a turn split across two logs, a count past the cap — where "concatenate and
// sort" is quietly wrong.

// identity is a row's key in the events table: (event_time, event_id) is the
// PRIMARY KEY, and the id alone is indexed but NOT unique. Microseconds rather
// than the time.Time, because a time.Time carries a monotonic reading and a
// location and is not a safe map key; micros is exactly what the column holds.
type identity struct {
	at int64
	id string
}

func identityOf(r store.EventRecord) identity {
	return identity{at: store.EncodeTime(r.Time), id: r.ID}
}

// newestFirst orders rows the way every keyset listing does: time descending,
// and within one instant the higher id first.
func newestFirst(a, b store.EventRecord) int {
	ka, kb := identityOf(a), identityOf(b)
	return cmp.Or(cmp.Compare(kb.at, ka.at), cmp.Compare(kb.id, ka.id))
}

// oldestFirst is the order a trace and a turn are read in.
func oldestFirst(a, b store.EventRecord) int { return newestFirst(b, a) }

// union is every row of every group once, by identity.
func union(groups ...[]store.EventRecord) []store.EventRecord {
	n := 0
	for _, g := range groups {
		n += len(g)
	}
	out := make([]store.EventRecord, 0, n)
	seen := make(map[identity]struct{}, n)
	for _, g := range groups {
		for _, r := range g {
			key := identityOf(r)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, r)
		}
	}
	return out
}

// MergeListing merges several nodes' keyset pages into the fleet's page.
//
// EXACT, and the boundary is why. A node whose part is FULL holds rows older
// than its last one that nobody has seen, so no row older than that last one
// can be placed: another node's older row might belong after one of the rows
// this node did not send. So the merged page stops at the NEWEST position any
// full part stopped at — every row at or above it is known on every node — and
// the next page resumes from there.
//
// While every node sends a whole page, that stop is never above the page size
// and cutting at the size is already exact: a row in the fleet's top N has
// fewer than N rows above it on its own node too. What makes the stop
// necessary is a reply CUT TO FIT THE TRANSPORT (see [fit]) — full, and
// shorter than the page — whose unsent rows sit above the size cut; the
// obvious k-way merge would page past them for good.
//
// more reports that rows exist past this page — a page filled, or the merged
// page was cut at limit — which is what tells a caller to offer "older".
func MergeListing(parts []listPart, limit int) (rows []store.EventRecord, more bool) {
	groups := make([][]store.EventRecord, 0, len(parts))
	var horizon *store.EventRecord
	for _, p := range parts {
		groups = append(groups, p.Rows)
		if !p.Full {
			continue
		}
		more = true
		if len(p.Rows) == 0 {
			continue
		}
		last := p.Rows[len(p.Rows)-1]
		if horizon == nil || newestFirst(last, *horizon) < 0 {
			horizon = &last
		}
	}
	rows = union(groups...)
	slices.SortFunc(rows, newestFirst)
	if horizon != nil {
		// KEEP EVERY ROW NOT OLDER THAN THE HORIZON, the horizon itself
		// included — it is a row its own node sent.
		cut := len(rows)
		for i, r := range rows {
			if newestFirst(r, *horizon) > 0 {
				cut = i
				break
			}
		}
		rows = rows[:cut]
	}
	if limit > 0 && len(rows) > limit {
		rows, more = rows[:limit], true
	}
	return rows, more
}

// MergeRelated folds a related-agent page's trace siblings into it, newest
// first, capped at limit — [store.EventLog.List]'s own sibling merge, over
// rows that came from several logs.
//
// WHEN THE PAGE HAS MORE, NO SIBLING OLDER THAN ITS LAST DIRECT ROW IS KEPT.
// The next page resumes from this page's last row, so a sibling placed below
// the direct rows' boundary would move the cursor past direct rows nobody has
// shown yet. One store never meets this, because its direct page is a whole
// page and the cap cuts every older sibling; a fleet's is not always — a
// merged page stops at the newest point a node's reply cut to fit the
// transport stopped at (see [MergeListing]), which can leave it short of the
// limit with older siblings free to fill the gap.
func MergeRelated(direct, siblings []store.EventRecord, limit int, more bool) []store.EventRecord {
	if more && len(direct) > 0 {
		boundary := direct[len(direct)-1]
		siblings = slices.DeleteFunc(slices.Clone(siblings), func(r store.EventRecord) bool {
			return newestFirst(r, boundary) > 0
		})
	}
	out := union(direct, siblings)
	slices.SortStableFunc(out, newestFirst)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// MergeSeries sums several nodes' histograms of ONE window into the fleet's.
//
// Summable because the window is PINNED — every node was handed the asker's
// clock, so every node snapped the same edges and produced the same bars —
// and because no event is in two stores. A part whose window differs (a peer
// on a build that ignores the pinned clock) cannot be summed bar for bar
// without adding one node's minute to another's next one; it is left out and
// its index reported, so the caller names that node rather than drawing a
// wrong bar.
func MergeSeries(base store.EventHistogram, others []store.EventHistogram) (store.EventHistogram, []int) {
	out := base
	out.Bars = slices.Clone(base.Bars)
	out.ByCategory = make(map[string]int, len(base.ByCategory))
	for k, v := range base.ByCategory {
		out.ByCategory[k] = v
	}
	var refused []int
	for i, h := range others {
		if !aligned(base, h) {
			refused = append(refused, i)
			continue
		}
		for j := range out.Bars {
			out.Bars[j].Count += h.Bars[j].Count
		}
		out.Total += h.Total
		for k, v := range h.ByCategory {
			out.ByCategory[k] += v
		}
	}
	return out, refused
}

// aligned reports whether two histograms are bars of one window.
func aligned(a, b store.EventHistogram) bool {
	if a.Bucket != b.Bucket || a.Since != b.Since || a.Until != b.Until ||
		len(a.Bars) != len(b.Bars) {
		return false
	}
	for i := range a.Bars {
		if a.Bars[i].At != b.Bars[i].At {
			return false
		}
	}
	return true
}

// FirstFound is the one event several nodes were asked for: the NEWEST copy
// any of them holds, which is what a single store's lookup by id answers too
// — an id is indexed but not unique, and the newest match is the reading every
// link has always had.
func FirstFound(parts []eventPart) (store.EventRecord, bool) {
	var best *store.EventRecord
	for _, p := range parts {
		if p.Event == nil {
			continue
		}
		if best == nil || newestFirst(*p.Event, *best) < 0 {
			best = p.Event
		}
	}
	if best == nil {
		return store.EventRecord{}, false
	}
	return *best, true
}

// MergeTrace is one trace from several nodes: the oldest [store.MaxTraceEvents]
// of every node's rows, and how many the fleet holds.
//
// Exact because each node sent its OWN oldest rows up to the cap, so the
// fleet's oldest are among them; the total is a sum because no event is in two
// stores.
func MergeTrace(parts []tracePart) (rows []store.EventRecord, total int) {
	groups := make([][]store.EventRecord, 0, len(parts))
	for _, p := range parts {
		groups = append(groups, p.Rows)
		total += p.Total
	}
	rows = union(groups...)
	slices.SortFunc(rows, oldestFirst)
	if len(rows) > store.MaxTraceEvents {
		rows = rows[:store.MaxTraceEvents]
	}
	return rows, max(total, len(rows))
}

// MergeTurn is one turn from several nodes: its oldest [store.MaxTurnEvents]
// rows, its newest [TurnClosingEvents], how many the fleet holds, and every
// trace it touched in the order it first touched them.
//
// Exact on both ends. The oldest rows are among the nodes' heads, because each
// head is that node's own oldest. The newest are among the nodes' closings —
// or their heads, where a head held the whole of that node's share and no
// closing was needed. What lies between is the gap the total reports.
func MergeTurn(parts []turnPart) (rows []store.EventRecord, total int, traces []string) {
	heads := make([][]store.EventRecord, 0, len(parts))
	everything := make([][]store.EventRecord, 0, 2*len(parts))
	firsts := map[string]time.Time{}
	for _, p := range parts {
		heads = append(heads, p.Head)
		everything = append(everything, p.Head, p.Closing)
		total += p.Total
		for _, t := range p.Traces {
			if at, seen := firsts[t.TraceID]; !seen || t.FirstAt.Before(at) {
				firsts[t.TraceID] = t.FirstAt
			}
		}
	}
	opening := union(heads...)
	slices.SortFunc(opening, oldestFirst)
	if len(opening) > store.MaxTurnEvents {
		opening = opening[:store.MaxTurnEvents]
	}
	ending := union(everything...)
	slices.SortFunc(ending, oldestFirst)
	if len(ending) > TurnClosingEvents {
		ending = ending[len(ending)-TurnClosingEvents:]
	}
	rows = union(opening, ending)
	slices.SortFunc(rows, oldestFirst)

	traces = make([]string, 0, len(firsts))
	for id := range firsts {
		traces = append(traces, id)
	}
	slices.SortFunc(traces, func(a, b string) int {
		return cmp.Or(firsts[a].Compare(firsts[b]), cmp.Compare(a, b))
	})
	return rows, max(total, len(rows)), traces
}

// MergeTurnPartials folds every node's partials into one partial per turn, in
// the order the turns were first seen, each combined with the SQL's own
// aggregates ([store.CombineTurnPartials]).
func MergeTurnPartials(parts ...[]store.TurnPartial) []store.TurnPartial {
	byID := map[string][]store.TurnPartial{}
	order := idsOf(parts...)
	for _, group := range parts {
		for _, p := range group {
			byID[p.TurnID] = append(byID[p.TurnID], p)
		}
	}
	out := make([]store.TurnPartial, 0, len(order))
	for _, id := range order {
		out = append(out, store.CombineTurnPartials(byID[id]...))
	}
	return out
}
