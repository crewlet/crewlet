package usage_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// company is the shape of a window of usage a cost case seeds: how many days,
// nodes, seats and people, how many spend cells each seat-day and person-day
// carries, and how many pages each seat-day reads.
type company struct {
	days, nodes, seats, people, cells, pages int
}

// firstDay is the company day a seeded window starts on.
var firstDay = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

// span is the company days c's window covers.
func (c company) span() usage.SpendQuery {
	return usage.SpendQuery{From: firstDay.Format(time.DateOnly),
		To: firstDay.AddDate(0, 0, c.days-1).Format(time.DateOnly)}
}

// pageIDs is every page c's seats read.
func (c company) pageIDs() []string {
	ids := make([]string, c.pages)
	for p := range ids {
		ids[p] = fmt.Sprintf("page-%03d", p)
	}
	return ids
}

// seed writes c's window through the shipped applier, a seat-day and a
// person-day per record as the publisher sends them, five hundred records to
// a transaction — the applier's own statements, so the rows are exactly what
// a fleet's are, at a pace a benchmark can wait for.
func (c company) seed(tb testing.TB, db *store.DB) {
	tb.Helper()
	ctx := tb.Context()
	seq := uint64(0)
	var batch []usage.Record
	flush := func() {
		tb.Helper()
		err := db.Replicated().Tx(ctx, func(tx *sql.Tx) error {
			for _, r := range batch {
				seq++
				body, err := r.Encode()
				if err != nil {
					return err
				}
				env, err := usage.Domain{}.Envelope(body)
				if err != nil {
					return err
				}
				if _, err := usage.NewApplier().Apply(ctx, tx, statelog.Record{
					Envelope: env, Payload: body,
					Position: statelog.Position{Stream: usage.Domain{}.Stream().Name, Generation: 1, Seq: seq},
				}, statelog.ApplyOptions{MaxVariables: db.Replicated().Caps().MaxVariables}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			tb.Fatalf("seed the window: %v", err)
		}
		batch = batch[:0]
	}
	add := func(r usage.Record) {
		batch = append(batch, r)
		if len(batch) == 500 {
			flush()
		}
	}
	models := make([]string, c.cells)
	for m := range models {
		models[m] = fmt.Sprintf("model-%02d", m)
	}
	for d := range c.days {
		day := firstDay.AddDate(0, 0, d).Format(time.DateOnly)
		for n := range c.nodes {
			node := fmt.Sprintf("node-%02d", n)
			for s := range c.seats {
				r := seatRecord(node, day, fmt.Sprintf("seat-%04d", s), models...)
				r.Turns.Durations.Add(time.Duration(s+1) * time.Second)
				for _, page := range c.pageIDs() {
					r.Reads = append(r.Reads, usage.Read{PageID: page, Backend: "native",
						Via: "search", Count: 1, LastAt: firstDay, LastQuery: "who owns billing"})
				}
				add(r)
			}
			for p := range c.people {
				add(personRecord(node, day, fmt.Sprintf("person-%02d", p), models...))
			}
		}
	}
	flush()
}

// windowRead is one paged read of a seeded window: its name, and one page of
// it after a group at a budget — the whole read being the one page asked with
// no budget, as the package's own whole reads ask it.
type windowRead struct {
	name string
	page func(ctx context.Context, after usage.Group, budget int) (rows int,
		next usage.Group, done bool, err error)
}

// readOf is a windowRead over one of the package's paged reads.
func readOf[R any](name string,
	page func(ctx context.Context, after usage.Group, budget int) (usage.Page[R], error)) windowRead {

	return windowRead{name: name, page: func(ctx context.Context, after usage.Group, budget int) (
		int, usage.Group, bool, error) {

		p, err := page(ctx, after, budget)
		return len(p.Rows), p.Next, p.Done, err
	}}
}

// windowReads is every paged read of the package over c's window.
func windowReads(estate usage.Estate, c company) []windowRead {
	days := c.span()
	people := usage.PersonQuery{From: days.From, To: days.To}
	readers := usage.PageReadsQuery{From: days.From, To: days.To, PageIDs: c.pageIDs()}
	return []windowRead{
		readOf("spend", func(ctx context.Context, after usage.Group, budget int) (usage.Page[usage.SpendRow], error) {
			return usage.SpendPage(ctx, estate, days, after, budget)
		}),
		readOf("seat turns", func(ctx context.Context, after usage.Group, budget int) (usage.Page[usage.TurnsRow], error) {
			return usage.SeatTurnsPage(ctx, estate, days, after, budget)
		}),
		readOf("person spend", func(ctx context.Context, after usage.Group, budget int) (usage.Page[usage.PersonRow], error) {
			return usage.PersonSpendPage(ctx, estate, people, after, budget)
		}),
		readOf("seat days", func(ctx context.Context, after usage.Group, budget int) (usage.Page[usage.SeatDay], error) {
			return usage.SeatDaysPage(ctx, estate, days, after, budget)
		}),
		readOf("page reads", func(ctx context.Context, after usage.Group, budget int) (usage.Page[usage.PageRead], error) {
			return usage.PageReadsPage(ctx, estate, readers, after, budget)
		}),
	}
}

// walk reads every page of r at budget bytes a page, and answers how many
// rows and pages it took.
func (r windowRead) walk(ctx context.Context, budget int) (rows, pages int, err error) {
	var after usage.Group
	for {
		n, next, done, err := r.page(ctx, after, budget)
		if err != nil {
			return 0, 0, err
		}
		rows += n
		pages++
		if done {
			return rows, pages, nil
		}
		if !after.Before(next) {
			return 0, 0, fmt.Errorf("%s: a page ended at %v, not after %v", r.name, next, after)
		}
		after = next
	}
}

// routedPageBytes is the page a routed usage read is answered in
// (internal/estate's usagePageBytes): a quarter of the message.
const routedPageBytes = queue.MaxPayloadBytes / 4

// BenchmarkAUsageWindowInPages is what a window of usage costs read whole —
// the one page this node's own router asks for — and walked a page at a time
// at the routed page's budget, the way a peer answers it, side by side for
// every paged read: ninety company days of a three-node, 120-seat company with
// two people, six spend cells and four page reads to a seat-day (194 400 spend
// rows).
//
// A page that pays for its own rows walks the window for about what the whole
// read costs, the difference being the encoding a page measures its rows by.
// One that re-reads the window from where it resumed costs more for every page
// it is cut into. The package doc records the numbers.
func BenchmarkAUsageWindowInPages(b *testing.B) {
	db := openStore(b)
	shape := company{days: 90, nodes: 3, seats: 120, people: 2, cells: 6, pages: 4}
	shape.seed(b, db)
	for _, read := range windowReads(db.Replicated(), shape) {
		b.Run(read.name+"/whole", func(b *testing.B) {
			for b.Loop() {
				if _, _, done, err := read.page(b.Context(), nil, 0); err != nil || !done {
					b.Fatalf("the whole read: done %v, %v", done, err)
				}
			}
		})
		b.Run(read.name+"/paged", func(b *testing.B) {
			pages := 0
			for b.Loop() {
				_, n, err := read.walk(b.Context(), routedPageBytes)
				if err != nil {
					b.Fatal(err)
				}
				pages = n
			}
			b.ReportMetric(float64(pages), "pages/op")
		})
	}
}
