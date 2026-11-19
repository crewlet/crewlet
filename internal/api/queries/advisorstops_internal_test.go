package queries

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
)

// pagedLog is a fleet's event stores as the fleet lists them: by type, newest
// first, a page of at most the limit asked before the cursor, saying whether
// more follow. It keeps the largest page it was asked for.
type pagedLog struct {
	FleetEvents
	rows     map[string][]store.EventRecord
	payloads map[string]json.RawMessage
	largest  *int
	reads    *int
}

func newPagedLog(rows map[string][]store.EventRecord) pagedLog {
	for _, typed := range rows {
		slices.SortFunc(typed, func(a, b store.EventRecord) int {
			//fixedwait:stated records' instants the fixture states, ordered as the fleet lists them
			return cmp.Or(b.Time.Compare(a.Time), cmp.Compare(b.ID, a.ID))
		})
	}
	return pagedLog{rows: rows, payloads: map[string]json.RawMessage{}, largest: new(int), reads: new(int)}
}

func (l pagedLog) List(_ context.Context, q store.ListQuery) (eventfan.Listing, eventfan.Coverage, error) {
	*l.largest = max(*l.largest, q.Limit)
	*l.reads++
	var page []store.EventRecord
	for _, r := range l.rows[q.Type] {
		//fixedwait:stated a record's instant the fixture states, against the window the read asks
		if r.Time.Before(q.Since) || r.Time.After(q.Until) {
			continue
		}
		//fixedwait:stated a record's instant against the cursor, an instant the fixture stated
		if q.Before != nil && cmp.Or(r.Time.Compare(q.Before.Time), cmp.Compare(r.ID, q.Before.ID)) >= 0 {
			continue
		}
		if len(page) == q.Limit {
			return eventfan.Listing{Rows: page, More: true}, eventfan.Coverage{Complete: true}, nil
		}
		page = append(page, r)
	}
	return eventfan.Listing{Rows: page}, eventfan.Coverage{Complete: true}, nil
}

func (l pagedLog) ByID(_ context.Context, id string) (store.EventRecord, eventfan.Coverage, error) {
	payload, ok := l.payloads[id]
	if !ok {
		return store.EventRecord{}, eventfan.Coverage{}, fmt.Errorf("no event %s", id)
	}
	return store.EventRecord{ID: id, Payload: payload}, eventfan.Coverage{Complete: true}, nil
}

// parkedEvery is a seat parked on its model's limit and released n times,
// every period from start, each park standing for held.
func parkedEvery(agent uuid.UUID, start time.Time, n int, period, held time.Duration) map[string][]store.EventRecord {
	rows := map[string][]store.EventRecord{}
	for i := range n {
		at := start.Add(time.Duration(i) * period)
		rows[types.SeatProviderParked{}.EventType()] = append(rows[types.SeatProviderParked{}.EventType()],
			store.EventRecord{ID: fmt.Sprintf("p-%06d", i), AgentID: agent.String(), Time: at})
		rows[types.SeatProviderReleased{}.EventType()] = append(rows[types.SeatProviderReleased{}.EventType()],
			store.EventRecord{ID: fmt.Sprintf("r-%06d", i), AgentID: agent.String(), Time: at.Add(held)})
	}
	return rows
}

// A SEAT PARKED THOUSANDS OF TIMES IS STILL MEASURED: a model entry on its
// limit parks and releases the seat on every attempt, and every stop is read
// however many records the history holds. Mutation: put a ceiling back on the
// stop records, and the seat's staffing is unread past it.
func TestASeatParkedThousandsOfTimesIsStillMeasured(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 14, 9, 0, 0, 0, time.UTC)
	to := time.Date(2030, 1, 14, 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -7)
	seat := uuid.New()
	// 3,000 parks of a minute each, one every three minutes, from the window's start.
	log := newPagedLog(parkedEvery(seat, from, 3_000, 3*time.Minute, time.Minute))
	s := Sources{Events: log}
	stops, err := s.seatStops(t.Context(), from, to, now)
	if err != nil {
		t.Fatalf("6,000 stop records: %v", err)
	}
	if got, whole := stoppedIn(stops[seat.String()], from, to); got != 3_000*time.Minute || !whole {
		t.Errorf("stopped %s (whole %v), want 3,000 minutes", got, whole)
	}
}

// READING THE STOPS HOLDS A PAGE PER TYPE AND THE SEAT'S STRETCHES, NOT ITS
// RECORDS: 20,000 parks over the history before the window and one inside it
// keep one stretch, and no read asks for more than a page. Mutation: keep
// every record, or every stop unclipped, and the read holds thousands.
func TestReadingTheStopsHoldsAPageAndTheStretchesNotTheRecords(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 14, 9, 0, 0, 0, time.UTC)
	to := time.Date(2030, 1, 14, 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -7)
	seat := uuid.New()
	rows := parkedEvery(seat, now.Add(-store.EventHistory).Add(time.Hour), 20_000, 90*time.Second, time.Minute)
	parked, released := types.SeatProviderParked{}.EventType(), types.SeatProviderReleased{}.EventType()
	rows[parked] = append(rows[parked], store.EventRecord{ID: "in", AgentID: seat.String(), Time: from.Add(time.Hour)})
	rows[released] = append(rows[released], store.EventRecord{ID: "in", AgentID: seat.String(),
		Time: from.Add(3 * time.Hour)})
	log := newPagedLog(rows)
	stops, err := Sources{Events: log}.seatStops(t.Context(), from, to, now)
	if err != nil {
		t.Fatal(err)
	}
	if spans := stops[seat.String()]; len(spans) != 1 {
		t.Errorf("the read kept %d stretches, want the one inside the window", len(spans))
	}
	if got, whole := stoppedIn(stops[seat.String()], from, to); got != 2*time.Hour || !whole {
		t.Errorf("stopped %s (whole %v), want the two hours inside the window", got, whole)
	}
	if *log.largest > stopPage {
		t.Errorf("a read asked for %d records at once, want a page of %d at most", *log.largest, stopPage)
	}
	if want := 2 * 20_000 / stopPage; *log.reads < want {
		t.Errorf("the 40,002 records were read in %d pages, want at least %d", *log.reads, want)
	}
}

// oldestFirst is how a read that held every mark paired them: sorted oldest
// first, a begin opening a stop where none is open, an end closing it, an end
// with none open read from the history's floor, and a stop still open at the
// end standing to it.
func oldestFirst(marks []stopMark, floor, to time.Time) []span {
	//fixedwait:stated marks' instants the fixture states, ordered as the oldest-first read ordered them
	slices.SortStableFunc(marks, func(a, b stopMark) int { return a.at.Compare(b.at) })
	var out []span
	open := map[string]time.Time{}
	for _, m := range marks {
		began, isOpen := open[m.cause]
		switch {
		case m.begins && !isOpen:
			open[m.cause] = m.at
		case !m.begins && isOpen:
			out = append(out, span{began, m.at})
			delete(open, m.cause)
		case !m.begins:
			out = append(out, span{floor, m.at})
		}
	}
	for _, began := range open {
		out = append(out, span{began, to})
	}
	return out
}

type stopMark struct {
	at     time.Time
	cause  string
	begins bool
	kind   int
}

// THE STOPS READ NEWEST FIRST ARE THE STOPS READ OLDEST FIRST: random pauses,
// resumes, parks and releases, repeated begins and ends and marks at one
// instant included, measure the same stopped time whichever way they are
// paired. Mutation: pair a repeated begin with its own end, or drop an end
// whose begin aged out, and a run differs.
func TestTheStopsReadNewestFirstAreTheStopsReadOldestFirst(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 14, 9, 0, 0, 0, time.UTC)
	to := time.Date(2030, 1, 14, 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -7)
	floor := now.Add(-store.EventHistory)
	seat := uuid.New()
	for run := range 300 {
		random := rand.New(rand.NewPCG(uint64(run), 7))
		var marks []stopMark
		rows := map[string][]store.EventRecord{}
		for i := range 1 + random.IntN(60) {
			kind := random.IntN(len(stopEvents))
			// WHOLE HOURS over the history, so marks share instants.
			//fixedwait:stated the window and the history's floor, instants the test states
			at := floor.Add(time.Duration(random.IntN(int(to.Sub(floor).Hours()))) * time.Hour)
			marks = append(marks, stopMark{at: at, cause: stopEvents[kind].cause, begins: stopEvents[kind].begins,
				kind: kind})
			rows[stopEvents[kind].kind] = append(rows[stopEvents[kind].kind],
				store.EventRecord{ID: fmt.Sprintf("m-%04d", i), AgentID: seat.String(), Time: at})
		}
		// AS THE OLDEST-FIRST READ TOOK THEM: by type in [stopEvents]' order.
		slices.SortStableFunc(marks, func(a, b stopMark) int { return cmp.Compare(a.kind, b.kind) })
		want, _ := stoppedIn(oldestFirst(marks, floor, to), from, to)
		stops, err := Sources{Events: newPagedLog(rows)}.seatStops(t.Context(), from, to, now)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := stoppedIn(stops[seat.String()], from, to); got != want {
			t.Fatalf("run %d: newest first measured %s, oldest first %s", run, got, want)
		}
	}
}

// A WINDOW'S REFUSALS READ ITS PAYLOAD ONCE: every refusal inside a budget
// window the company's clock places stands until that window resets, the
// payload read for the newest of them alone, and one whose reset is not given
// leaves the seat unmeasured. Mutation: read each refusal's payload, and a
// seat refused all day reads thousands.
func TestAWindowsRefusalsReadItsPayloadOnce(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte("name: Acme\nroles:\n  - name: Dev\n    handle: dev\n"))
	if err != nil {
		t.Fatal(err)
	}
	roster, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	dev, ok := roster.AgentIDFor(roster.AgentSeatByHandle("dev"))
	if !ok {
		t.Fatal("no dev seat")
	}
	now := time.Date(2030, 1, 14, 9, 0, 0, 0, time.UTC)
	to := time.Date(2030, 1, 14, 0, 0, 0, 0, time.UTC)
	from := to.AddDate(0, 0, -7)
	day := from.AddDate(0, 0, 2)
	var refusals []store.EventRecord
	for i := range 3_000 {
		refusals = append(refusals, store.EventRecord{ID: fmt.Sprintf("b-%05d", i), AgentID: dev.String(),
			Time: day.Add(12*time.Hour + time.Duration(i)*10*time.Second)})
	}
	log := newPagedLog(map[string][]store.EventRecord{types.BudgetExhausted{}.EventType(): refusals})
	reads := 0
	payload, err := json.Marshal(types.BudgetExhausted{Period: "day", Window: "2030-01-09",
		ResetsAt: day.AddDate(0, 0, 1).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refusals {
		log.payloads[r.ID] = payload
	}
	counting := countingLog{pagedLog: log, reads: &reads}
	s := Sources{Events: counting, Company: func() (*config.Company, *org.Organization) { return cfg, roster }}
	stops, err := s.seatStops(t.Context(), from, to, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, whole := stoppedIn(stops[dev.String()], from, to); got != 12*time.Hour || !whole {
		t.Errorf("stopped %s (whole %v), want noon to the day's reset", got, whole)
	}
	if reads != 1 {
		t.Errorf("read %d payloads of one window's refusals, want one", reads)
	}
	// A RESET NOT GIVEN is a stop of unknown end.
	unknown, err := json.Marshal(types.BudgetExhausted{Period: "day"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range refusals {
		log.payloads[r.ID] = unknown
	}
	stops, err = s.seatStops(t.Context(), from, to, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, whole := stoppedIn(stops[dev.String()], from, to); whole {
		t.Error("a refusal whose reset is not given left the seat measured")
	}
}

// countingLog counts the payloads read.
type countingLog struct {
	pagedLog
	reads *int
}

func (l countingLog) ByID(ctx context.Context, id string) (store.EventRecord, eventfan.Coverage, error) {
	*l.reads++
	return l.pagedLog.ByID(ctx, id)
}
