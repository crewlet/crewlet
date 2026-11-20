package queries

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
)

// CAPACITY LOST TO MODEL LIMITS (ADR-0166), read from the seats' park and
// release records: merged newest first, paired per seat as they arrive into
// stretches, and each stretch's first park read once for the entry it named.
// A park is published again at every probe while the limit stands, so the
// records number thousands where the stretches number dozens; only the
// stretches' records are read whole.

// parkSpans is every seat's park stretch over [since, to], by the seat's
// handle (its agent id where the chart no longer holds it).
func (s Sources) parkSpans(ctx context.Context, company *config.Company, since, to time.Time) ([]advisor.ParkSpan, error) {
	parked := s.streamOf(since, to, types.SeatProviderParked{}.EventType(), nil)
	released := s.streamOf(since, to, types.SeatProviderReleased{}.EventType(), nil)
	type run struct {
		// end is a release read whose park is not read yet.
		end    time.Time
		hasEnd bool
		// from and id are the run of parks being read: the earliest so far,
		// and its record, which names the entry.
		from, until time.Time
		id          string
		inRun       bool
	}
	type stretch struct {
		agent, id string
		from, to  time.Time
	}
	runs := map[string]*run{}
	var stretches []stretch
	finish := func(agent string, r *run) {
		stretches = append(stretches, stretch{agent: agent, id: r.id, from: r.from, to: r.until})
		r.inRun = false
	}
	for {
		p, pok, err := parked.peek(ctx)
		if err != nil {
			return nil, err
		}
		q, qok, err := released.peek(ctx)
		if err != nil {
			return nil, err
		}
		if !pok && !qok {
			break
		}
		// NEWEST FIRST, a release before a park at one instant: read oldest
		// first, the park came first.
		if qok && (!pok || !q.Time.Before(p.Time)) {
			if _, _, err := released.next(ctx); err != nil {
				return nil, err
			}
			r := runs[q.AgentID]
			if r == nil {
				r = &run{}
				runs[q.AgentID] = r
			}
			if r.inRun {
				finish(q.AgentID, r)
			}
			r.end, r.hasEnd = q.Time.UTC(), true
			continue
		}
		if _, _, err := parked.next(ctx); err != nil {
			return nil, err
		}
		r := runs[p.AgentID]
		if r == nil {
			r = &run{}
			runs[p.AgentID] = r
		}
		switch {
		case r.hasEnd:
			r.from, r.until, r.id, r.inRun, r.hasEnd = p.Time.UTC(), r.end, p.ID, true, false
		case r.inRun:
			// A RENEWAL OF THE SAME PARK: the stretch began earlier.
			r.from, r.id = p.Time.UTC(), p.ID
		default:
			// A PARK NOTHING RELEASED: it stands to the window's end.
			r.from, r.until, r.id, r.inRun = p.Time.UTC(), to, p.ID, true
		}
	}
	for agent, r := range runs {
		if r.inRun {
			finish(agent, r)
		}
		if r.hasEnd {
			// A RELEASE WHOSE PARK IS OLDER THAN THE WINDOW: the stretch
			// stood from its start, on the entry the seat's chain heads now.
			stretches = append(stretches, stretch{agent: agent, from: since, to: r.end})
		}
	}
	chart := s.chartHandles()
	entries := map[string]string{}
	out := make([]advisor.ParkSpan, 0, len(stretches))
	for _, st := range stretches {
		seat := st.agent
		if c, ok := chart[st.agent]; ok && c.handle != "" {
			seat = c.handle
		}
		entry := ""
		switch {
		case st.id != "":
			if e, read := entries[st.id]; read {
				entry = e
				break
			}
			full, _, err := s.Events.ByID(ctx, st.id)
			if err != nil {
				return nil, fmt.Errorf("queries: read park %s: %w", st.id, err)
			}
			var park types.SeatProviderParked
			if err := json.Unmarshal(full.Payload, &park); err != nil {
				return nil, fmt.Errorf("queries: decode park %s: %w", st.id, err)
			}
			if len(park.ProviderKeys) > 0 {
				entry = park.ProviderKeys[0]
			}
			entries[st.id] = entry
		case company != nil:
			if role, _ := roleByHandle(company, seat); role != nil {
				if chain := company.ExecutorChain(role); len(chain) > 0 {
					entry = chain[0]
				}
			}
		}
		out = append(out, advisor.ParkSpan{Seat: seat, Entry: entry, From: st.from, To: st.to})
	}
	return out, nil
}

// roleByHandle is the role a seat handle names in c.
func roleByHandle(c *config.Company, handle string) (*config.Role, bool) {
	for role := range c.EachRole() {
		if role.Seat().Handle() == handle {
			return role, true
		}
	}
	return nil, false
}

// advisorCapacityOf is the capacity lost over the days company days that end
// at now.
func (s Sources) advisorCapacityOf(ctx context.Context, company *config.Company, now time.Time,
	days int) (*advisor.Capacity, error) {
	zone := s.zone()
	local := now.In(zone)
	from := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone).AddDate(0, 0, 1-days)
	spans, err := s.parkSpans(ctx, company, from, now)
	if err != nil {
		return nil, err
	}
	c := advisor.CapacityOf(spans, now, days, zone)
	return &c, nil
}

// AdvisorCapacity is the capacity lost to model limits over a range, as the
// `advisor_capacity` question answers it.
type AdvisorCapacity struct {
	Range   advisor.SnapshotRange `json:"range"`
	Days    []string              `json:"days"`
	Entries []AdvisorEntryOut     `json:"entries"`
}

// AdvisorEntryOut is one model entry's hours out on its usage limit.
type AdvisorEntryOut struct {
	Entry      string            `json:"entry"`
	OutHours   []float64         `json:"out_hours"`
	TotalHours float64           `json:"total_hours"`
	Hits       int               `json:"hits"`
	HitsByDay  []int             `json:"hits_by_day"`
	Seats      []AdvisorSeatHeld `json:"seats"`
}

// AdvisorSeatHeld is one seat's hours parked on an entry.
type AdvisorSeatHeld struct {
	Handle string  `json:"handle"`
	Name   string  `json:"name"`
	Hours  float64 `json:"hours"`
}

// advisorCapacity answers `advisor_capacity`: the hours each model entry was
// out on its usage limit per company day over `range` (7d, 14d or 30d).
func (s Sources) advisorCapacity(ctx context.Context, p Params) (any, error) {
	days, err := snapshotDays(p.String("range"))
	if err != nil {
		return nil, err
	}
	company := s.company()
	c, err := s.advisorCapacityOf(ctx, company, s.clock(), days)
	if err != nil {
		return nil, fmt.Errorf("%w: the seats' park records could not be read: %w", ErrUnavailable, err)
	}
	names := map[string]string{}
	if company != nil {
		for role := range company.EachRole() {
			names[role.Seat().Handle()] = role.Name
		}
	}
	out := AdvisorCapacity{Range: advisor.SnapshotRange{From: c.From.UTC(), To: c.To.UTC(), Days: days},
		Days: []string{}, Entries: []AdvisorEntryOut{}}
	for _, d := range c.Days {
		out.Days = append(out.Days, d.Format(time.DateOnly))
	}
	for _, e := range c.Entries {
		w := AdvisorEntryOut{Entry: e.Entry, TotalHours: roundHours(e.Total), Hits: e.Hits, HitsByDay: e.HitsByDay,
			OutHours: []float64{}, Seats: []AdvisorSeatHeld{}}
		for _, h := range e.Out {
			w.OutHours = append(w.OutHours, roundHours(h))
		}
		for _, seat := range e.Seats {
			name := names[seat.Seat]
			if name == "" {
				name = seat.Seat
			}
			w.Seats = append(w.Seats, AdvisorSeatHeld{Handle: seat.Seat, Name: name, Hours: roundHours(seat.Held)})
		}
		out.Entries = append(out.Entries, w)
	}
	return out, nil
}

// roundHours is d in hours to one decimal.
func roundHours(d time.Duration) float64 { return math.Round(d.Hours()*10) / 10 }
