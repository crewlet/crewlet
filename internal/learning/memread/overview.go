package memread

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// Every seat's memory at a glance — the `memory_overview` answer.
//
// # One scatter, not one read per seat
//
// The overview is the same question [Reader.Memory] answers, asked of every
// agent seat at once and cut to what a list row draws: the three totals and the
// newest reflection. Asked per seat it would be one lease read and one scatter
// for EACH seat on every poll — fifty round trips for a fifty-seat company to
// draw one list. Instead the asker reads every seat lease in ONE listing,
// groups the seats by the incarnation holding them, and puts ONE request on
// the broker naming each holder's seats; every holder answers for its own in
// one reply. What the asker holds itself it reads from its own store, beside
// the scatter.
//
// # A row is answered by its holder or says why it is not
//
// The rules are [Reader.Memory]'s, per seat: a seat no node holds has no
// current copy and is sent with `held_by: none` and nothing counted; a seat
// whose holder did not answer — silent, on a build that cannot, or still
// taking it — is sent with its `unavailable` reason rather than with zeros,
// because "this seat remembers nothing" and "its holder did not say" are
// opposite facts on a screen. The COVERAGE names every holder that was asked
// and whether it answered, in the one shape every fleet answer carries
// ([eventfan.Coverage]), so the screen's partial callout is the same callout
// the history screens draw.

// OverviewSeat is one seat's row of the overview.
type OverviewSeat struct {
	// ID is the seat's agent id, which a row is matched by; Handle its
	// handle, which a list draws.
	ID     string `json:"agent_id"`
	Handle string `json:"handle"`

	// The totals the holder counted — the same counts [Memory] carries
	// beside its pages. Zero on a row that was not answered, which
	// Unavailable (or HeldBy none) says.
	DiaryTotal    int `json:"diary_total"`
	EpisodesTotal int `json:"episodes_total"`
	SkillsTotal   int `json:"skills_total"`

	// LastReflectionAt is when the newest live diary entry was written, or
	// "" when the diary holds none; LatestReflection is that entry, or null.
	// Both travel because a list sorts by the instant and draws the entry.
	LastReflectionAt string    `json:"last_reflection_at"`
	LatestReflection *DiaryRow `json:"latest_reflection"`

	// HeldBy is the node holding the seat — whose store counted this row
	// when it was answered — or [HolderNone].
	HeldBy string `json:"held_by"`

	// Unavailable is why the holder's count is not here, in words an
	// operator can act on; "" when it is (or when nobody holds the seat,
	// which HeldBy says).
	Unavailable string `json:"unavailable"`
}

// Overview is every agent seat's memory totals, each counted by its holder.
type Overview struct {
	// Seats is one row per seat asked about, in the order asked.
	Seats []OverviewSeat `json:"seats"`

	// Coverage names this node and every holder that was asked, and whether
	// each answered.
	Coverage eventfan.Coverage `json:"coverage"`
}

// Overview answers every named seat's totals, each from its holder.
//
// AN UNREADABLE LEASE TABLE FAILS THE WHOLE ANSWER as [ErrUnavailable]: with
// no way to say who holds anything, every row would be a guess, and "held by
// nobody" is precisely the guess the three-valued lease read exists to refuse.
func (r *Reader) Overview(ctx context.Context, seats []Seat) (Overview, error) {
	self := NodeOf(r.Owner)
	out := Overview{
		Seats:    make([]OverviewSeat, len(seats)),
		Coverage: eventfan.Coverage{Nodes: []eventfan.NodeCoverage{{ID: self, Answered: true}}, Complete: true},
	}
	at := make(map[uuid.UUID]int, len(seats))
	byID := make(map[uuid.UUID]Seat, len(seats))
	for i, seat := range seats {
		out.Seats[i] = OverviewSeat{ID: seat.ID.String(), Handle: seat.Handle, HeldBy: HolderNone}
		at[seat.ID] = i
		byID[seat.ID] = seat
	}
	// place is where a holder's row goes, matched by the seat's id. Every
	// caller then puts the ASKER's handle on it: the row was counted by a
	// holder whose chart may call the seat something else.
	place := func(row OverviewSeat) (int, bool) {
		id, err := uuid.Parse(row.ID)
		if err != nil {
			return 0, false
		}
		i, ok := at[id]
		return i, ok
	}
	if r.Queue == nil {
		// A NODE WITH NO BROKER IS THE FLEET: every seat is its own.
		rows, err := r.Local.Overview(ctx, seats)
		if err != nil {
			return Overview{}, err
		}
		for _, row := range rows {
			if i, ok := place(row); ok {
				row.HeldBy = self
				row.Handle = out.Seats[i].Handle
				out.Seats[i] = row
			}
		}
		return out, nil
	}

	// ONE LISTING OF EVERY SEAT LEASE rather than a read per seat.
	leases, err := r.Leases.ListLive(ctx, coord.ClassSeat)
	if err != nil {
		return Overview{}, fmt.Errorf("%w: which nodes hold the seats could not be read: %w",
			ErrUnavailable, err)
	}
	now := r.now()
	byOwner := map[string][]Seat{}
	for _, lease := range leases {
		id, ok := coord.SeatID(lease.Resource)
		if _, asked := at[id]; !ok || !asked || !lease.Live(now) {
			continue
		}
		byOwner[lease.Owner] = append(byOwner[lease.Owner], byID[id])
	}

	mark := func(seats []Seat, node, why string) {
		for _, seat := range seats {
			out.Seats[at[seat.ID]] = OverviewSeat{
				ID: seat.ID.String(), Handle: seat.Handle, HeldBy: node, Unavailable: why,
			}
		}
	}
	fill := func(rows []OverviewSeat, node string, allowed []Seat) {
		for _, row := range rows {
			i, ok := place(row)
			if !ok || !slices.ContainsFunc(allowed, func(s Seat) bool { return s.ID.String() == row.ID }) {
				// A ROW FOR A SEAT THIS HOLDER WAS NOT ASKED ABOUT is not
				// its to give: the lease this read took names somebody
				// else, or nobody.
				continue
			}
			row.HeldBy = node
			row.Handle = out.Seats[i].Handle
			out.Seats[i] = row
		}
	}
	nodes := map[string]eventfan.NodeCoverage{self: {ID: self, Answered: true}}
	missing := func(node, why string) {
		nodes[node] = eventfan.NodeCoverage{ID: node, Error: why}
	}

	// ASKED: the holders whose build answers an overview. Named at once
	// otherwise, as [Reader.ask] names one for a single read.
	asked := map[string][]Seat{}
	for owner, seats := range byOwner {
		node := NodeOf(owner)
		if owner == r.Owner {
			continue
		}
		answers, err := r.Features.OwnerFeature(ctx, owner, coord.FeatureHeldRead)
		switch {
		case err != nil:
			why := "whether it can answer could not be read: " + err.Error()
			missing(node, why)
			mark(seats, node, why)
		case !answers:
			why := "it runs an older build that cannot answer a read of a seat's memory"
			missing(node, why)
			mark(seats, node, why)
		default:
			asked[owner] = seats
		}
	}

	type scattered struct {
		replies [][]byte
		err     error
	}
	var replies chan scattered
	if len(asked) > 0 {
		raw, err := json.Marshal(request{
			Version: WireVersion, Question: QuestionOverview, Seats: asked,
		})
		if err != nil {
			return Overview{}, fmt.Errorf("memread: encode a request: %w", err)
		}
		replies = make(chan scattered, 1)
		go func() {
			askCtx, cancel := context.WithTimeout(ctx, r.budget())
			defer cancel()
			//nolint:govet // shadow: the goroutine's own; the local read below writes the outer one.
			got, err := r.Queue.Ask(askCtx, topics.HeldRead, raw, len(asked))
			replies <- scattered{got, err}
		}()
	}

	// THIS NODE'S OWN SEATS, beside the scatter: attached is current, and
	// one still arriving is refused per row as [Reader.read] refuses it.
	if mine := byOwner[r.Owner]; len(mine) > 0 {
		attached := r.Attached()
		var ready, arriving []Seat
		for _, seat := range mine {
			if slices.Contains(attached, seat.ID) {
				ready = append(ready, seat)
			} else {
				arriving = append(arriving, seat)
			}
		}
		mark(arriving, self, "this node is taking the seat and its memory is still arriving")
		rows, err := r.Local.Overview(ctx, ready)
		if err != nil {
			return Overview{}, err
		}
		fill(rows, self, ready)
	}

	if replies != nil {
		for owner, seats := range asked {
			node := NodeOf(owner)
			missing(node, fmt.Sprintf("no answer within the %s read budget", r.budget()))
			mark(seats, node, "the node holding it did not answer")
		}
		got := <-replies
		if got.err != nil {
			for owner := range asked {
				missing(NodeOf(owner), "it could not be asked: "+got.err.Error())
			}
		}
		for _, body := range got.replies {
			var rep reply
			if json.Unmarshal(body, &rep) != nil {
				continue
			}
			seats, ok := asked[rep.Owner]
			if !ok {
				continue
			}
			node := NodeOf(rep.Owner)
			if rep.Error != "" {
				missing(node, rep.Error)
				mark(seats, node, rep.Error)
				continue
			}
			var rows []OverviewSeat
			if err := json.Unmarshal(rep.Answer, &rows); err != nil {
				why := "its answer could not be read: " + err.Error()
				missing(node, why)
				mark(seats, node, why)
				continue
			}
			nodes[node] = eventfan.NodeCoverage{ID: node, Answered: true}
			fill(rows, node, seats)
		}
	}

	out.Coverage.Nodes = out.Coverage.Nodes[:0]
	for _, n := range nodes {
		out.Coverage.Nodes = append(out.Coverage.Nodes, n)
		if !n.Answered {
			out.Coverage.Complete = false
		}
	}
	slices.SortFunc(out.Coverage.Nodes, func(a, b eventfan.NodeCoverage) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return out, nil
}

// Overview counts the named seats' memory in THIS node's store: the three
// totals and the newest live diary entry, one row per seat in the order
// named, each keyed as [Stores.Memory] keys it. HeldBy is left for the caller,
// which knows whose tenure it is.
//
// The same counts [Stores.Memory] carries, through the same store calls — so
// a list row and the seat's own page can never disagree about a total.
func (s *Stores) Overview(ctx context.Context, seats []Seat) ([]OverviewSeat, error) {
	out := make([]OverviewSeat, 0, len(seats))
	now := s.now()
	for _, seat := range seats {
		row := OverviewSeat{ID: seat.ID.String(), Handle: seat.Handle}
		if s.Diary != nil {
			if id := agentID(seat); id != "" {
				latest, err := s.Diary.Recent(ctx, id, now, 1)
				if err != nil {
					return nil, err
				}
				if len(latest) > 0 {
					entry := diaryRow(latest[0])
					row.LatestReflection = &entry
					row.LastReflectionAt = entry.CreatedAt
				}
				if row.DiaryTotal, err = s.Diary.Count(ctx, id, now); err != nil {
					return nil, err
				}
			}
		}
		if s.Episodes != nil {
			n, err := s.Episodes.Count(ctx, seat.Handle)
			if err != nil {
				return nil, err
			}
			row.EpisodesTotal = n
		}
		if s.Skills != nil {
			n, err := s.Skills.Count(ctx, seat.Handle, learning.ListOptions{})
			if err != nil {
				return nil, err
			}
			row.SkillsTotal = n
		}
		out = append(out, row)
	}
	return out, nil
}
