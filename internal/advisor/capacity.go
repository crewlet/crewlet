package advisor

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// CAPACITY LOST TO MODEL LIMITS, per model entry (ADR-0166): a seat whose
// chain's every member refused for a usage limit is parked until the limit
// lifts, whatever it holds, so the hours its entry was out are hours its
// seats could not work. Read from the seats' own park and release records,
// each park stretch once (its first record names the entry), never from the
// items in progress, which miss a seat that had nothing open.

// ClassModelLimit is the owner's: a model entry whose usage limit kept its
// seats out for hours, one finding per entry with the seats it held inside.
const ClassModelLimit Class = "capacity.model_limit"

// SubjectModel is a providers.llm entry, by its key in Name.
const SubjectModel SubjectKind = "model"

// ParkSpan is one stretch a seat sat parked on its model's limit: from its
// first park to its release, or to the window's end where it still stands,
// on Entry, the head of its chain when it parked.
type ParkSpan struct {
	Seat, Entry string
	From, To    time.Time
}

// Capacity is the hours each model entry was out over whole company days.
type Capacity struct {
	From, To time.Time
	// Days are the company days' starts, oldest first.
	Days    []time.Time
	Entries []EntryOut
}

// EntryOut is one entry's stretches out: hours a day, how many stretches,
// and the seats it held with each one's hours.
type EntryOut struct {
	Entry string
	// Out is each day's hours out, aligned with [Capacity.Days].
	Out   []time.Duration
	Total time.Duration
	// Hits is how many separate stretches the entry was out.
	Hits int
	// HitsByDay is how many stretches began on each day.
	HitsByDay []int
	Seats     []SeatHeld
}

// SeatHeld is one seat's hours parked on an entry.
type SeatHeld struct {
	Seat string
	Held time.Duration
}

// CapacityOf is spans over the days company days in zone that end at to, the
// entries out longest first.
func CapacityOf(spans []ParkSpan, to time.Time, days int, zone *time.Location) Capacity {
	local := to.In(zone)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	out := Capacity{From: today.AddDate(0, 0, 1-days), To: to}
	for d := range days {
		out.Days = append(out.Days, out.From.AddDate(0, 0, d))
	}
	byEntry := map[string][]ParkSpan{}
	for _, s := range spans {
		from, end := maxTime(s.From, out.From), s.To
		if end.After(to) {
			end = to
		}
		if s.Entry == "" || !end.After(from) {
			continue
		}
		byEntry[s.Entry] = append(byEntry[s.Entry], ParkSpan{Seat: s.Seat, Entry: s.Entry, From: from, To: end})
	}
	for entry, ss := range byEntry {
		e := EntryOut{Entry: entry, Out: make([]time.Duration, days), HitsByDay: make([]int, days)}
		held := map[string]time.Duration{}
		for _, s := range ss {
			held[s.Seat] += s.To.Sub(s.From)
		}
		for seat, h := range held {
			e.Seats = append(e.Seats, SeatHeld{Seat: seat, Held: h})
		}
		slices.SortFunc(e.Seats, func(a, b SeatHeld) int { return cmp.Or(cmp.Compare(b.Held, a.Held), cmp.Compare(a.Seat, b.Seat)) })
		for _, st := range unionOf(ss) {
			e.Hits++
			e.Total += st.To.Sub(st.From)
			for d, start := range out.Days {
				end := start.AddDate(0, 0, 1)
				if !st.From.Before(start) && st.From.Before(end) {
					e.HitsByDay[d]++
				}
				if f, t := maxTime(st.From, start), minOf(st.To, end); t.After(f) {
					e.Out[d] += t.Sub(f)
				}
			}
		}
		out.Entries = append(out.Entries, e)
	}
	slices.SortFunc(out.Entries, func(a, b EntryOut) int {
		return cmp.Or(cmp.Compare(b.Total, a.Total), cmp.Compare(a.Entry, b.Entry))
	})
	return out
}

func minOf(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// unionOf is the stretches spans cover together, oldest first.
func unionOf(spans []ParkSpan) []ParkSpan {
	ss := slices.Clone(spans)
	slices.SortFunc(ss, func(a, b ParkSpan) int { return cmp.Or(a.From.Compare(b.From), a.To.Compare(b.To)) })
	var out []ParkSpan
	for _, s := range ss {
		if n := len(out); n > 0 && !s.From.After(out[n-1].To) {
			if s.To.After(out[n-1].To) {
				out[n-1].To = s.To
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

// The bar an entry clears to be a finding: out three hours or more over the
// week, so one short limit is not one.
const minEntryOut = 3 * time.Hour

// worst is the day an entry was out longest, by its index.
func (e EntryOut) worst() int {
	at := 0
	for d, h := range e.Out {
		if h > e.Out[at] {
			at = d
		}
	}
	return at
}

// modelLimit is one finding per model entry out [minEntryOut] or more over
// the week: its worst day leads, the seats it held inside, and a fallback on
// another account the fix.
func modelLimit(c *config.Company, ev Evidence) []Finding {
	if ev.Capacity == nil {
		return nil
	}
	capa := ev.Capacity
	var out []Finding
	for _, e := range capa.Entries {
		if e.Total < minEntryOut {
			continue
		}
		w := e.worst()
		day := capa.Days[w].Format("Mon 2 Jan")
		f := stallFinding(ClassModelLimit, AreaSetup, Subject{Kind: SubjectModel, Name: e.Entry},
			fmt.Sprintf("%s out %s over the week, %d times", e.Entry, hoursSaid(e.Total), e.Hits),
			[]Fact{{Key: "out_minutes", Value: int64(e.Total / time.Minute)},
				{Key: "limit_hits", Value: int64(e.Hits)},
				{Key: "seats_held", Value: int64(len(e.Seats))},
				{Key: "worst_day_minutes", Value: int64(e.Out[w] / time.Minute)}},
			nil, limitFix(c, e))
		f.Headline = fmt.Sprintf("%s sat out %s on %s: %s hit its usage limit %s.",
			seatsCount(len(e.Seats)), hoursSaid(e.Out[w]), day, e.Entry, timesSaid(e.HitsByDay[w]))
		f.Path = "providers.llm." + e.Entry
		for _, s := range e.Seats {
			name := s.Seat
			if role, _ := roleOf(c, s.Seat); role != nil && role.Name != "" {
				name = role.Name
			}
			f.Seats = append(f.Seats, SeatShare{Seat: s.Seat, Name: name, Value: hoursOf(s.Held), Unit: UnitHours})
		}
		out = append(out, f)
	}
	return out
}

// seatsCount is "1 seat" or "n seats".
func seatsCount(n int) string {
	if n == 1 {
		return "1 seat"
	}
	return fmt.Sprintf("%d seats", n)
}

// timesSaid is how many times, in words a person reads.
func timesSaid(n int) string {
	switch n {
	case 0:
		return "from the day before"
	case 1:
		return "once"
	case 2:
		return "twice"
	}
	return fmt.Sprintf("%d times", n)
}

// limitFix is the change that keeps an entry's seats working through its
// limit: a fallback on another account or harness, chosen in the providers
// setting, since which account to add is the owner's.
func limitFix(c *config.Company, e EntryOut) Fix {
	seats := make([]string, 0, len(e.Seats))
	for _, s := range e.Seats {
		name := s.Seat
		if role, _ := roleOf(c, s.Seat); role != nil && role.Name != "" {
			name = role.Name
		}
		seats = append(seats, name)
	}
	return Fix{Kind: FixOpenSetting, Setting: "providers.llm", Options: []Option{},
		Sentence: fmt.Sprintf("Give %s a fallback model on another account, or spread them across entries, "+
			"so a limit on %s does not stop them all.", strings.Join(seats, ", "), e.Entry)}
}
