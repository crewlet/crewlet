package eventfan

import (
	"cmp"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
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
// more reports that rows exist past this page — a node said it holds more
// than it sent, or the merged page was cut at limit — which is what tells a
// caller to offer "older".
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

// MergeSpend merges several nodes' per-phase spend records into the fleet's
// newest `limit` (every record when limit is not positive), newest first.
//
// EXACT FOR [MergeListing]'s REASON, and cut at the same place: a node whose
// part is FULL holds older records nobody has seen, so nothing older than its
// last record can be placed, and the merge stops at the newest point any full
// part stopped at. A record is a ROW, and the union holds each row once by the
// event log's own identity — its instant and its event id — because a record
// is SUMMED and the cost of a duplicate is a double count rather than a
// repeated row: a stateless node's phase sits in two data nodes' stores while
// its custody batch is in flight (see the package doc), and both send it. By
// the id alone the union would drop a second row sharing an id at another
// instant, which the log's key allows.
func MergeSpend(parts []spendPart, limit int) []tokens.Record {
	var all []tokens.Record
	var horizon *tokens.Record
	type key struct{ at, id string }
	seen := map[key]bool{}
	for _, p := range parts {
		for _, r := range p.Records {
			if r.EventID != "" {
				k := key{at: r.Timestamp, id: r.EventID}
				if at, err := time.Parse(time.RFC3339Nano, r.Timestamp); err == nil {
					k.at = at.UTC().Format(time.RFC3339Nano)
				}
				if seen[k] {
					continue
				}
				seen[k] = true
			}
			all = append(all, r)
		}
		if !p.Full || len(p.Records) == 0 {
			continue
		}
		last := p.Records[len(p.Records)-1]
		if horizon == nil || newestSpendFirst(last, *horizon) < 0 {
			horizon = &last
		}
	}
	slices.SortFunc(all, newestSpendFirst)
	if horizon != nil {
		cut := len(all)
		for i, r := range all {
			if newestSpendFirst(r, *horizon) > 0 {
				cut = i
				break
			}
		}
		all = all[:cut]
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}

// newestSpendFirst orders spend records by instant, newest first, and by event
// id within one instant — the store's own (event_time, event_id) order. The
// stamps are PARSED, never compared as text: RFC3339Nano trims trailing zeros,
// so two instants' strings do not sort as the instants do.
func newestSpendFirst(a, b tokens.Record) int {
	at, aerr := time.Parse(time.RFC3339Nano, a.Timestamp)
	bt, berr := time.Parse(time.RFC3339Nano, b.Timestamp)
	if aerr == nil && berr == nil {
		if c := bt.Compare(at); c != 0 {
			return c
		}
	} else if c := cmp.Compare(b.Timestamp, a.Timestamp); c != 0 {
		return c
	}
	return cmp.Compare(b.EventID, a.EventID)
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

// Counted is one node's part of an answer that COUNTS — the axis, a trace's or
// a turn's extent, a page of turns' sums, the outcome counts — beside which of
// the rows the fleet's parts named unsettled the node keeps.
//
// Each such part counts the rows its node KEEPS and NAMES the rows it holds of
// a custody batch it has written and not settled, which a second data node may
// hold too (see the package doc and internal/store's unsettled.go). The merge
// adds what every part counted and then each named row once ([once]), by the
// rule the part's own count applies to a row.
type Counted[T any] struct {
	Node string
	Part T

	// Kept is the node's answer to [QuestionKept]: which of every part's
	// named rows it keeps. Nil when nothing was named, or it did not answer.
	Kept []store.UnsettledRow
}

// once is every row a part named unsettled, once each, that no part counted
// already — what a merge adds to the counts it summed, by `named`, which reads
// a part's named rows.
//
// EXACT, for two reasons that are both needed. A row a node COUNTED is one it
// keeps, and no two nodes keep one row: its own rows are written once, inline,
// by the node that published them, and a stateless node's by the one data node
// the fleet's custody claim chose (internal/observe). And a row a node holds
// WITHOUT knowing it keeps it is NAMED rather than counted, and counted here
// once — unless a node that keeps it, and did not name it, counted it already.
// That node is the batch's keeper, which settled first; a node that named the
// row and keeps it by the second question settled between the two, and counted
// it in neither, so the row is still counted here.
//
// What that leaves is a batch written and settled by its keeper BETWEEN the two
// questions while another node held it unsettled: the keeper counted it in
// neither answer, and keeps it by the second — so it is in no count. That is a
// redelivery landing inside one read, and the next read counts it.
//
// Only the parts handed in are counted from and judged by: a node whose part a
// merge left out — one that refused the first question, or whose axis was cut
// over another window — counted nothing the merge holds, so what it keeps
// cannot say a row was counted, and the merge does not hand its part in.
func once[T any](parts []Counted[T], named func(T) []store.UnsettledRow) []store.UnsettledRow {
	rows := map[store.RowKey]store.UnsettledRow{}
	var order []store.RowKey
	namedBy := map[store.RowKey]map[string]bool{}
	for _, p := range parts {
		for _, r := range named(p.Part) {
			k := r.Key()
			if _, seen := rows[k]; !seen {
				rows[k], namedBy[k] = r, map[string]bool{}
				order = append(order, k)
			}
			namedBy[k][p.Node] = true
		}
	}
	counted := map[store.RowKey]bool{}
	for _, p := range parts {
		for _, r := range p.Kept {
			if by, isNamed := namedBy[r.Key()]; isNamed && !by[p.Node] {
				counted[r.Key()] = true
			}
		}
	}
	out := make([]store.UnsettledRow, 0, len(order))
	for _, k := range order {
		if !counted[k] {
			out = append(out, rows[k])
		}
	}
	return out
}

// identities is every row any part named unsettled, once each, by identity
// alone — what the second question asks every node about.
func identities[T any](parts []Counted[T], named func(T) []store.UnsettledRow) []store.UnsettledRow {
	var out []store.UnsettledRow
	seen := map[store.RowKey]bool{}
	for _, p := range parts {
		for _, r := range named(p.Part) {
			if !seen[r.Key()] {
				seen[r.Key()] = true
				out = append(out, r.Identity())
			}
		}
	}
	return out
}

// MergeSeries sums several nodes' histograms of ONE window into the fleet's —
// the first part's window, which is the asker's — and names the nodes whose
// part it could not sum.
//
// Summable because the window is PINNED — every node was handed the asker's
// clock, so every node snapped the same edges and produced the same bars — and
// because every row is counted once: each part counts the rows its node keeps,
// and the rows a custody batch still in flight puts on two nodes are named and
// added once, by the rule the counts placed them by ([once],
// [store.EventHistogram.Count]). Every node cuts that window alike
// ([store.HistogramQuery.Window]), a window the history clips included: down
// to the bucket the floor falls in, partial first bar and all, which the asker
// drops after this sum ([store.EventHistogram.InsideHistory]). A part whose
// window differs anyway cannot be summed bar for bar without adding one node's
// minute to another's next one; it is left out and its node reported, so the
// caller names that node rather than drawing a wrong bar — and its named rows
// and what it keeps are left out with it.
func MergeSeries(q store.HistogramQuery, parts []Counted[store.EventHistogram]) (store.EventHistogram, []string) {
	if len(parts) == 0 {
		return store.EventHistogram{}, nil
	}
	base := parts[0].Part
	out := base
	out.Bars = slices.Clone(base.Bars)
	out.ByCategory = make(map[string]int, len(base.ByCategory))
	for k, v := range base.ByCategory {
		out.ByCategory[k] = v
	}
	out.Unsettled = nil
	summed := []Counted[store.EventHistogram]{parts[0]}
	var refused []string
	for _, p := range parts[1:] {
		h := p.Part
		if !aligned(base, h) {
			refused = append(refused, p.Node)
			continue
		}
		summed = append(summed, p)
		for j := range out.Bars {
			out.Bars[j].Count += h.Bars[j].Count
			out.Bars[j].Failed += h.Bars[j].Failed
		}
		out.Total += h.Total
		out.Failed += h.Failed
		for k, v := range h.ByCategory {
			out.ByCategory[k] += v
		}
	}
	for _, r := range once(summed, histogramNamed) {
		out.Count(q, r, 1)
	}
	return out, refused
}

func histogramNamed(h store.EventHistogram) []store.UnsettledRow { return h.Unsettled }

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

// MergeOutcomes sums several nodes' notification outcome counts into the
// fleet's, counting every row once.
//
// EXACT, for [once]'s two reasons and one more: every node counted the SAME
// window. The asker sent both its edges, the bottom one and its own instant,
// which is also where every node floored the history, so no node's count
// reaches a second further than another's.
//
// The maps are never nil, so an answer that counted nothing still marshals as
// two empty objects; Unsettled is empty, every row of it having been counted.
func MergeOutcomes(parts []Counted[store.NotificationOutcomes]) store.NotificationOutcomes {
	out := store.NotificationOutcomes{Skipped: map[string]int{}, Coalesced: map[string]int{}}
	for _, p := range parts {
		for app, n := range p.Part.Skipped {
			out.Skipped[app] += n
		}
		for app, n := range p.Part.Coalesced {
			out.Coalesced[app] += n
		}
	}
	for _, r := range once(parts, outcomesNamed) {
		out.Add(r)
	}
	return out
}

func outcomesNamed(o store.NotificationOutcomes) []store.UnsettledRow { return o.Unsettled }

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
// EXACT FOR [MergeListing]'s REASON, and cut where it cuts. Each node sent its
// own oldest rows, so the fleet's oldest are among them — up to the point a
// node stops being known. A node holding rows it did NOT send ([tracePart.unsent])
// holds every one of them after the last row it sent, so no row past that one
// can be placed: another node's newer row would be shown while the unsent rows
// before it were not, a hole inside a view that presents itself as the
// trace's opening. So the union stops at the OLDEST such last row — kept,
// since its node sent it — and the cap applies after that; a node that sent
// no row at all while holding some leaves nothing placeable.
//
// While every node sends its whole capped read, that stop is never above the
// cap and cutting at the cap is already exact. What makes it necessary is a
// part that sent FEWER rows than the cap and holds more: a reply cut to fit
// the transport ([fit]) — whose unsent rows sit inside the cap's cut, where
// the obvious merge filled the gap with other nodes' newer rows. The total is what
// every node keeps of the trace and every row a custody batch in flight puts on
// two nodes once ([once]), and it is never cut, so the answer still says what
// it does not show.
func MergeTrace(parts []Counted[tracePart]) (rows []store.EventRecord, total int) {
	groups := make([][]store.EventRecord, 0, len(parts))
	var known opening
	for _, c := range parts {
		p := c.Part
		groups = append(groups, p.Rows)
		total += p.Total
		if last, held := p.unsent(); held {
			known.stopAt(last)
		}
	}
	total += len(once(parts, func(p tracePart) []store.UnsettledRow { return p.Unsettled }))
	rows = union(groups...)
	slices.SortFunc(rows, oldestFirst)
	rows = known.cut(rows)
	if len(rows) > store.MaxTraceEvents {
		rows = rows[:store.MaxTraceEvents]
	}
	return rows, max(total, len(rows))
}

// opening is how far an oldest-first view of several nodes' rows is known:
// through the OLDEST last row sent by a node that holds rows it did not send,
// or not at all when such a node sent none.
type opening struct {
	through *store.EventRecord
	nothing bool
}

// stopAt records one node's last sent row — nil when it sent none.
func (o *opening) stopAt(last *store.EventRecord) {
	switch {
	case last == nil:
		o.nothing = true
	case o.through == nil || oldestFirst(*last, *o.through) < 0:
		o.through = last
	}
}

// cut keeps the rows, oldest first, not newer than the bound — the bound row
// itself included, since its own node sent it.
func (o opening) cut(rows []store.EventRecord) []store.EventRecord {
	switch {
	case o.nothing:
		return rows[:0]
	case o.through == nil:
		return rows
	}
	for i, r := range rows {
		if oldestFirst(r, *o.through) > 0 {
			return rows[:i]
		}
	}
	return rows
}

// MergeTurn is one turn from several nodes: its oldest [store.MaxTurnEvents]
// rows, its newest [TurnClosingEvents], how many the fleet holds, and every
// trace it touched in the order it first touched them.
//
// Exact on both ends. The OPENING is [MergeTrace]'s, cut where it cuts: each
// node sent its own oldest rows, so the fleet's oldest are among them up to
// the oldest last opening row of a node holding rows it sent in neither its
// opening nor its ending — every one of those lies after that row, so nothing
// past it can be placed. It is taken from every row sent, endings included,
// because a node holding nothing it did not send is known whole. The newest
// rows are among the nodes' closings — or their heads, where a head held the
// whole of that node's share and no closing was needed. What lies between is
// the gap the total reports, which is never cut — and which counts each row
// once, for [MergeTrace]'s reason.
func MergeTurn(parts []Counted[turnPart]) (rows []store.EventRecord, total int, traces []string) {
	everything := make([][]store.EventRecord, 0, 2*len(parts))
	firsts := map[string]time.Time{}
	var known opening
	total = len(once(parts, func(p turnPart) []store.UnsettledRow { return p.Unsettled }))
	for _, c := range parts {
		p := c.Part
		everything = append(everything, p.Head, p.Closing)
		total += p.Total
		if last, held := p.unsent(); held {
			known.stopAt(last)
		}
		for _, t := range p.Traces {
			if at, seen := firsts[t.TraceID]; !seen || t.FirstAt.Before(at) {
				firsts[t.TraceID] = t.FirstAt
			}
		}
	}
	sent := union(everything...)
	slices.SortFunc(sent, oldestFirst)
	start := known.cut(sent)
	if len(start) > store.MaxTurnEvents {
		start = start[:store.MaxTurnEvents]
	}
	ending := sent
	if len(ending) > TurnClosingEvents {
		ending = ending[len(ending)-TurnClosingEvents:]
	}
	rows = union(start, ending)
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

// MergeTurnShares folds every node's share of a page's turns into one partial
// per turn, in the order the turns were first seen ([MergeTurnPartials]), and
// adds to each turn's sums the rows of it a custody batch in flight puts on two
// nodes, once each ([once], [store.TurnPartial.Add]) — every share's sums
// being of the rows its node keeps.
func MergeTurnShares(parts []Counted[turnsPart]) []store.TurnPartial {
	shares := make([][]store.TurnPartial, 0, len(parts))
	for _, p := range parts {
		shares = append(shares, p.Part.Turns)
	}
	merged := MergeTurnPartials(shares...)
	byID := make(map[string]int, len(merged))
	for i, p := range merged {
		byID[p.TurnID] = i
	}
	for _, r := range once(parts, func(p turnsPart) []store.UnsettledRow { return p.Unsettled }) {
		// A NAMED ROW IS OF A TURN ITS NODE'S SHARE FOLDED — the store names
		// none other, and a share cut to fit the transport drops the names
		// of the turns it cut ([turnsPart.keep]) — so its turn is merged.
		if i, ok := byID[r.TurnID]; ok {
			merged[i].Add(r, 1)
		}
	}
	return merged
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
