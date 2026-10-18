package engine

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// The chat prune duty's cases.
//
// WHAT THEY PROTECT, in one sentence each: that a company which asked to keep
// its conversation for ever keeps it, that a room's own setting is the one
// that decides, that a tick is bounded and picks up where the last one
// stopped, that two nodes never publish one ladder, and that the rungs of that
// ladder are the BROKER's instants rather than this node's clock — which is
// the whole reason chat retention is a record and not a sweep.
//
// Every case drives [chatPrune.tick] directly against stub seams. The loop
// around it is a ticker and a claim, and a test of those would be a test of
// [time.Ticker].

// fakeClock is the duty's clock, movable by the case.
//
// Read and written on the test's own goroutine only — these cases call tick
// synchronously rather than starting the loop — so it carries no mutex, which
// would otherwise imply a concurrency this harness does not have.
type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

// prunePublished is one record the duty published.
type prunePublished struct {
	channel string
	cutoff  time.Time
	actor   chatlet.Actor
	opID    string
}

// fakePruner records what was published and models the applier's effect: a
// prune deletes strictly below its cutoff, so the room's oldest surviving
// message afterwards is the cutoff itself. That is the DENSEST room — one with
// a message at every instant — which is the case the per-record bound exists
// for.
type fakePruner struct {
	rooms   *fakeRooms
	calls   []prunePublished
	outcome statelog.Outcome

	// refuse is the rooms this build cannot write to, by id. A refusal is
	// per ROOM rather than per call because that is what it is in the write
	// path: a kind this build cannot classify refuses every gesture in that
	// room and none anywhere else.
	refuse map[string]error

	// after, when set, is called with the number of records published so
	// far after each one — where a case lands the tick's deadline.
	after func(calls int)
}

func (p *fakePruner) Prune(_ context.Context, actor chatlet.Actor, opID, channelID string,
	cutoff time.Time) (chatlet.Written, error) {

	if err := p.refuse[channelID]; err != nil {
		return chatlet.Written{}, err
	}
	p.calls = append(p.calls, prunePublished{
		channel: channelID, cutoff: cutoff, actor: actor, opID: opID,
	})
	outcome := p.outcome
	if outcome == "" {
		outcome = statelog.OutcomeApplied
	}
	if outcome == statelog.OutcomeApplied && p.rooms != nil {
		p.rooms.advance(channelID, cutoff)
	}
	if p.after != nil {
		p.after(len(p.calls))
	}
	return chatlet.Written{Outcome: statelog.Result{Outcome: outcome}}, nil
}

// cutoffsFor is every cutoff published for one room, in order.
func (p *fakePruner) cutoffsFor(channel string) []time.Time {
	var out []time.Time
	for _, call := range p.calls {
		if call.channel == channel {
			out = append(out, call.cutoff)
		}
	}
	return out
}

// channels is every room a record was published for, in order, with repeats
// collapsed.
func (p *fakePruner) channels() []string {
	var out []string
	for _, call := range p.calls {
		if len(out) == 0 || out[len(out)-1] != call.channel {
			out = append(out, call.channel)
		}
	}
	return out
}

// fakeRooms is the company's rooms as the duty reads them: a page at a time,
// in room-id order, strictly after the room named — which is the keyset the
// real statement seeks.
type fakeRooms struct {
	rooms []chatRoom
	err   error
	reads int

	// afters is where each read began, in order.
	afters []string

	// fail, when set, is asked before each read with the read's ordinal
	// (1 for the first) and the read's own context; an error fails it.
	fail func(ctx context.Context, read int) error
}

func (f *fakeRooms) PruneRooms(ctx context.Context, after string, limit int) ([]chatRoom, error) {
	f.reads++
	f.afters = append(f.afters, after)
	if f.err != nil {
		return nil, f.err
	}
	if f.fail != nil {
		if err := f.fail(ctx, f.reads); err != nil {
			return nil, err
		}
	}
	sorted := append([]chatRoom(nil), f.rooms...)
	slices.SortFunc(sorted, func(a, b chatRoom) int { return cmp.Compare(a.ID, b.ID) })
	var out []chatRoom
	for _, room := range sorted {
		if room.ID > after && len(out) < limit {
			out = append(out, room)
		}
	}
	return out, nil
}

// advance moves a room's floor to where a prune left it.
func (f *fakeRooms) advance(channelID string, cutoff time.Time) {
	for i := range f.rooms {
		if f.rooms[i].ID == channelID && f.rooms[i].Oldest.Before(cutoff) {
			f.rooms[i].Oldest = cutoff
		}
	}
}

// room is one room for a case: an id, an optional override and a floor.
func room(id string, override *int, oldest time.Time) chatRoom {
	return chatRoom{ID: id, RetentionDays: override, Oldest: oldest}
}

// override is an addressable retention day count, which is the shape the
// nullable column has and the reason every carrier of it is a pointer.
func override(n int) *int { return &n }

// pruneHarness wires a duty over stubs, holding the duty this node already
// won so every case is about the sweep rather than about the lease.
//
// THE FAKE KEEPS A COPY OF THE ROOMS IT IS HANDED, because an applied prune
// moves a floor in place ([fakeRooms.advance]) and a case that spreads its own
// slice (`all...`) hands over that slice's backing array, not its elements:
// two parallel subtests built over one slice would each move the other's
// floors — a data race, and a room the other's first page no longer needs.
func pruneHarness(policy chatRetentionPolicy, now time.Time,
	rooms ...chatRoom) (*chatPrune, *fakePruner, *fakeRooms) {

	source := &fakeRooms{rooms: slices.Clone(rooms)}
	pruner := &fakePruner{rooms: source}
	clock := &fakeClock{at: now}
	return &chatPrune{
		pruner: pruner,
		rooms:  source,
		policy: func(context.Context) (chatRetentionPolicy, error) { return policy, nil },
		claim:  func(context.Context) (bool, error) { return true, nil },
		nodeID: "node-a",
		now:    clock.now,
		done:   make(chan struct{}),
	}, pruner, source
}

// anchor is a fixed instant, so a case's arithmetic is readable rather than
// relative to whenever the suite happened to run.
var anchor = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

// A COMPANY THAT KEEPS EVERYTHING FOR EVER PUBLISHES NOTHING.
//
// `message_retention_days: 0` is the one setting whose zero means the
// opposite of what a duration's zero means, and the accessor answers it with a
// SECOND RETURN for exactly that reason. A duty that read the duration and
// ignored the bool would subtract nothing from `now` and publish a cutoff of
// NOW — deleting the company's entire conversation on its first tick, with no
// inverse.
func TestACompanyWithNoHorizonPublishesNoPrune(t *testing.T) {
	t.Parallel()
	ancient := anchor.Add(-5 * 365 * 24 * time.Hour)
	d, pruner, rooms := pruneHarness(
		chatRetentionPolicy{Chatlet: true, CompanyDays: chatlet.RetentionForever},
		anchor,
		room("general", nil, ancient),
		room("random", nil, ancient),
	)

	d.tick(t.Context())

	if len(pruner.calls) != 0 {
		t.Fatalf("a company keeping its messages for ever had %d prune records "+
			"published against it: %v — the first one deletes everything said "+
			"before %s", len(pruner.calls), pruner.calls, pruner.calls[0].cutoff)
	}
	if rooms.reads != 1 {
		t.Errorf("the rooms were read %d times; the horizon is a per-tick read "+
			"of the epoch and the rooms are read once behind it", rooms.reads)
	}
}

// A ROOM'S OWN HORIZON DECIDES, IN BOTH DIRECTIONS.
//
// The second half is the one an early return on "the company has no horizon"
// would silently break: a company that keeps everything for ever may still
// have set thirty days on one room, and [chatlet.HorizonFor] is the single place
// that decides which setting wins.
func TestARoomsOwnHorizonBeatsTheCompanys(t *testing.T) {
	t.Parallel()
	// Below the company's year and below the room's month, so which
	// horizon is applied decides whether anything is published at all.
	floor := anchor.Add(-45 * 24 * time.Hour)

	cases := map[string]struct {
		company  int
		override *int
		want     bool
	}{
		"a shorter room horizon under a company year": {
			company:  config.DefaultMessageRetentionDays,
			override: override(config.MinMessageRetentionDays),
			want:     true,
		},
		"a room horizon under a company that keeps everything": {
			company:  chatlet.RetentionForever,
			override: override(config.MinMessageRetentionDays),
			want:     true,
		},
		"a room kept for ever under a company that prunes": {
			company:  config.MinMessageRetentionDays,
			override: override(chatlet.RetentionForever),
			want:     false,
		},
		"a room that inherits a company year": {
			company:  config.DefaultMessageRetentionDays,
			override: nil,
			want:     false, // 45 days is inside a year
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, pruner, _ := pruneHarness(
				chatRetentionPolicy{Chatlet: true, CompanyDays: tc.company},
				anchor, room("decisions", tc.override, floor))

			d.tick(t.Context())

			if got := len(pruner.calls) > 0; got != tc.want {
				t.Fatalf("pruned = %v, want %v (company %d days, override %v, "+
					"oldest message %s) — the room's own setting is what decides",
					got, tc.want, tc.company, tc.override, floor)
			}
		})
	}
}

// A ROOM ALREADY INSIDE ITS HORIZON IS NOT PUBLISHED FOR.
//
// A prune that deletes nothing is not free: it is one more record on the
// busiest log the company has, every tick, for every room it has ever created
// — replicated to every node and held for the stream's whole retention window.
func TestATickWithNothingBelowTheCutoffPublishesNothing(t *testing.T) {
	t.Parallel()
	horizon := config.MinMessageRetentionDays
	cases := map[string]time.Time{
		"a room whose oldest message is inside the horizon": anchor.Add(-29 * 24 * time.Hour),
		"a room holding no message at all":                  {},
	}
	for name, floor := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, pruner, _ := pruneHarness(
				chatRetentionPolicy{Chatlet: true, CompanyDays: horizon},
				anchor, room("quiet", nil, floor))

			d.tick(t.Context())

			if len(pruner.calls) != 0 {
				t.Fatalf("published %d records for a room with nothing below "+
					"its cutoff: %v", len(pruner.calls), pruner.calls)
			}
		})
	}
}

// ONE TICK IS BOUNDED, AND THE NEXT RESUMES WHERE IT STOPPED.
//
// The bound is the reason the prune is a ladder rather than one record: an
// undivided delete of a company's whole backlog is one transaction holding
// this node's only writer for the length of it, on every node. The resumption
// is what makes the bound safe — the rows themselves are the bookmark, so
// nothing has to remember how far a capped sweep had got.
func TestOneTicksPruneRecordsAreBoundedAndResume(t *testing.T) {
	t.Parallel()
	const backlogWindows = 2*chatPruneRecordsPerRoom + 3
	horizon := anchor.Add(-config.MinMessageRetentionDays * 24 * time.Hour)
	floor := horizon.Add(-backlogWindows * chatPruneWindow)

	d, pruner, _ := pruneHarness(
		chatRetentionPolicy{Chatlet: true, CompanyDays: config.MinMessageRetentionDays},
		anchor, room("busy", nil, floor))

	want := []int{chatPruneRecordsPerRoom, chatPruneRecordsPerRoom, 3, 0}
	published := 0
	for tick, expect := range want {
		before := len(pruner.calls)
		d.tick(t.Context())
		got := len(pruner.calls) - before
		if got != expect {
			t.Fatalf("tick %d published %d records, want %d — a tick is bounded "+
				"at %d per room and a %d-window backlog takes %v ticks",
				tick+1, got, expect, chatPruneRecordsPerRoom, backlogWindows, want)
		}
		published += got
	}

	// THE RUNGS ARE CONTIGUOUS WINDOWS ending exactly on the horizon: no
	// gap (rows left below a cutoff nobody publishes again) and no
	// overlap (records that delete nothing).
	cutoffs := pruner.cutoffsFor("busy")
	if len(cutoffs) != backlogWindows {
		t.Fatalf("published %d cutoffs for a %d-window backlog", len(cutoffs), backlogWindows)
	}
	for i, cutoff := range cutoffs {
		step := floor.Add(time.Duration(i+1) * chatPruneWindow)
		if !cutoff.Equal(step) {
			t.Fatalf("cutoff %d is %s, want %s — the ladder must step one "+
				"window at a time from the room's oldest message", i, cutoff, step)
		}
	}
	if last := cutoffs[len(cutoffs)-1]; !last.Equal(horizon) {
		t.Errorf("the last cutoff is %s and the horizon is %s — the ladder must "+
			"land exactly on the horizon rather than past it", last, horizon)
	}

	// AND EVERY RUNG IS ITS OWN OPERATION, CARRYING THE INSTANT IT WAS
	// MINTED: one with none is read as older than every loss the chat
	// ledger has had, so a node whose ledger ever lost a row would answer
	// every prune `unknown` and publish none — and retention would stop
	// with nothing to say why.
	seen := map[string]bool{}
	for _, call := range pruner.calls {
		if _, minted := statelog.OpMintedAt(call.opID); !minted {
			t.Fatalf("a prune was published under %q, which carries no mint "+
				"instant", call.opID)
		}
		if seen[call.opID] {
			t.Fatalf("two rungs were published under one operation %s — the "+
				"second is answered as the first's retry and never lands",
				call.opID)
		}
		seen[call.opID] = true
	}
}

// A NODE THAT DOES NOT HOLD THE DUTY PUBLISHES NOTHING, AND READS NOTHING.
//
// Two nodes publishing one ladder is not corruption — a range delete below an
// instant is idempotent — but it is a second copy of every prune record on the
// company's hottest log, for ever. The claim is also checked BEFORE the rooms
// are read, so a fleet of twenty nodes costs one company-wide room read a tick
// rather than twenty.
func TestOnlyTheNodeHoldingTheDutyPrunes(t *testing.T) {
	t.Parallel()
	floor := anchor.Add(-2 * 365 * 24 * time.Hour)
	for name, claim := range map[string]func(context.Context) (bool, error){
		"a peer holds the duty": func(context.Context) (bool, error) { return false, nil },
		"the store did not answer": func(context.Context) (bool, error) {
			return false, errors.New("coordination store unreachable")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, pruner, rooms := pruneHarness(
				chatRetentionPolicy{Chatlet: true, CompanyDays: config.MinMessageRetentionDays},
				anchor, room("general", nil, floor))
			d.claim = claim

			d.tick(t.Context())

			if len(pruner.calls) != 0 {
				t.Fatalf("a node that does not hold `worker:%s` published %d "+
					"prune records", chatPruneDutyName, len(pruner.calls))
			}
			if rooms.reads != 0 {
				t.Errorf("the rooms were read %d times without the duty; the "+
					"claim is what gates the read", rooms.reads)
			}
		})
	}
}

// THE LADDER'S RUNGS ARE THE BROKER'S INSTANTS, NOT THIS NODE'S CLOCK.
//
// This is the property the whole design rests on. A message's `created_at` is
// the broker's own stored instant, so a cutoff anchored on the oldest message
// in the room is a number every node computes identically — and one that does
// not move when a tick runs late, or when two nodes' clocks disagree. Anchor
// the ladder on `now` instead and every tick publishes a different set of
// cutoffs for the same rows.
//
// THE COMPLEMENT IS ASSERTED TOO, because a test that only shows something
// standing still passes just as well against a duty that publishes nothing at
// all: the FINAL rung — the one that brings a room inside its horizon — must
// move with the clock, since that is what a horizon is.
func TestThePruneCutoffIsTheBrokersInstantAndNotTheDutysClock(t *testing.T) {
	t.Parallel()
	horizon := anchor.Add(-config.MinMessageRetentionDays * 24 * time.Hour)
	// A backlog several windows deep, so the first rung is a step rather
	// than the horizon itself.
	backlogged := horizon.Add(-5 * chatPruneWindow)
	// And a room whose whole backlog fits inside one window, so its only
	// rung IS the horizon.
	shallow := horizon.Add(-chatPruneWindow / 2)

	first := func(now time.Time) (stepped, final time.Time) {
		d, pruner, _ := pruneHarness(
			chatRetentionPolicy{Chatlet: true, CompanyDays: config.MinMessageRetentionDays},
			now,
			room("backlogged", nil, backlogged),
			room("shallow", nil, shallow),
		)
		d.tick(t.Context())
		return pruner.cutoffsFor("backlogged")[0], pruner.cutoffsFor("shallow")[0]
	}

	steppedAt9, finalAt9 := first(anchor)
	steppedLater, finalLater := first(anchor.Add(30 * time.Minute))

	if !steppedAt9.Equal(backlogged.Add(chatPruneWindow)) {
		t.Fatalf("the first rung is %s, want the oldest message %s plus one "+
			"window", steppedAt9, backlogged)
	}
	if !steppedAt9.Equal(steppedLater) {
		t.Errorf("the duty's clock moved by 30m and the first cutoff moved "+
			"with it, from %s to %s — a rung below the horizon is anchored on "+
			"the BROKER's stored instant, so two nodes half a minute apart "+
			"must publish the same number", steppedAt9, steppedLater)
	}
	if !finalLater.Equal(finalAt9.Add(30 * time.Minute)) {
		t.Errorf("the clock moved by 30m and the horizon cutoff went from %s "+
			"to %s — the last rung is `now` minus the configured days, so it "+
			"must move exactly as far as the clock did", finalAt9, finalLater)
	}
}

// THE SWEEP REACHES EVERY ELIGIBLE ROOM, IN ROOM-ID ORDER, AND ONE ROOM'S
// REFUSAL DOES NOT STOP IT.
//
// A room whose kind this build cannot classify refuses every write, and a
// sweep that gave up there would hold the whole company's retention hostage to
// one row a newer peer wrote. The order is [chatlet.Eligible]'s, so two nodes
// that read the rooms in different orders publish the same sweep in the same
// sequence.
func TestThePruneSweepCoversEveryRoomAndSurvivesOnesRefusal(t *testing.T) {
	t.Parallel()
	floor := anchor.Add(-2 * 365 * 24 * time.Hour)
	d, pruner, _ := pruneHarness(
		chatRetentionPolicy{Chatlet: true, CompanyDays: config.MinMessageRetentionDays},
		anchor,
		room("zulu", nil, floor),
		room("alpha", nil, floor),
		room("mike", nil, floor),
	)
	// The middle room in id order refuses every write, so a sweep that
	// gave up on a refusal would never reach the last one.
	pruner.refuse = map[string]error{
		"mike": errors.New("that is a kind this build does not know"),
	}

	d.tick(t.Context())

	got := pruner.channels()
	want := []string{"alpha", "zulu"}
	if len(got) != len(want) {
		t.Fatalf("records were published for %v, want %v — every room the "+
			"selection named except the one that refused", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("records were published for %v, want %v — the selection's "+
				"order is the room id, so every node publishes the same sweep "+
				"in the same sequence", got, want)
		}
	}
	if n := len(pruner.cutoffsFor("mike")); n != 0 {
		t.Errorf("the refusing room took %d records", n)
	}
}

// THE SWEEP PAGES THROUGH EVERY ROOM THE COMPANY EVER MADE.
//
// Nothing bounds that set — an archive releases [chatlet.MaxChannels]' slot, a
// direct conversation is never refused by it, and no room is ever deleted — so a read
// of all of it is a snapshot and a slice that grow for the life of the
// deployment. Each read is one page, the next begins after the last room the
// previous returned, and every room on every page is swept.
func TestThePruneSweepPagesThroughEveryRoom(t *testing.T) {
	t.Parallel()
	floor := anchor.Add(-2 * 365 * 24 * time.Hour)
	// Two whole pages and a short one, so a sweep that read once, or that
	// stopped at the first full page, leaves rooms unpruned.
	const total = 2*chatPruneRoomsPage + 7
	var all []chatRoom
	for i := range total {
		all = append(all, room(fmt.Sprintf("room-%05d", i), nil, floor))
	}
	d, pruner, rooms := pruneHarness(
		chatRetentionPolicy{Chatlet: true, CompanyDays: config.MinMessageRetentionDays},
		anchor, all...)

	d.tick(t.Context())

	if got := len(pruner.channels()); got != total {
		t.Fatalf("records were published for %d rooms, want all %d — the "+
			"sweep must page past the first %d", got, total, chatPruneRoomsPage)
	}
	want := []string{"", all[chatPruneRoomsPage-1].ID, all[2*chatPruneRoomsPage-1].ID}
	if !slices.Equal(rooms.afters, want) {
		t.Errorf("the reads began after %q, want %q — one page each, each "+
			"beginning after the last room the one before returned", rooms.afters, want)
	}
	if d.resume != "" {
		t.Errorf("a lap that came round left the next one to begin after %q", d.resume)
	}
}

// A LAP THE DEADLINE CUT IS CONTINUED, NOT RESTARTED.
//
// A tick may not outlive its interval, so on a company with enough backlog a
// lap ends early. Begun at the first room id every time, the lap would spend
// every tick on the same low ids while the backlog lasted, and the rooms at
// the tail would keep their messages past the horizon a screen says they are
// pruned at. So the next tick begins after the last room the cut one
// finished, reaches the end, comes round from the first room and stops where
// it began — and a room whose ladder the deadline cut is taken again rather
// than skipped.
func TestALapTheDeadlineCutResumesWhereItStopped(t *testing.T) {
	t.Parallel()
	floor := anchor.Add(-2 * 365 * 24 * time.Hour)
	ids := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	var all []chatRoom
	for _, id := range ids {
		all = append(all, room(id, nil, floor))
	}
	d, pruner, rooms := pruneHarness(
		chatRetentionPolicy{Chatlet: true, CompanyDays: config.MinMessageRetentionDays},
		anchor, all...)

	// THE DEADLINE LANDS IN THE MIDDLE OF CHARLIE'S LADDER: two whole
	// rooms and part of a third.
	ctx, cancel := context.WithCancel(t.Context())
	pruner.after = func(calls int) {
		if calls == 2*chatPruneRecordsPerRoom+3 {
			cancel()
		}
	}
	d.tick(ctx)
	if got := pruner.channels(); !slices.Equal(got, []string{"alpha", "bravo", "charlie"}) {
		t.Fatalf("the cut tick published for %v", got)
	}
	if d.resume != "bravo" {
		t.Fatalf("the cut lap resumes after %q, want %q — charlie's ladder was "+
			"cut, so it is not finished", d.resume, "bravo")
	}

	pruner.after, pruner.calls = nil, nil
	reads := rooms.reads
	d.tick(t.Context())
	if got := pruner.channels(); !slices.Equal(got, []string{"charlie", "delta", "echo", "alpha", "bravo"}) {
		t.Fatalf("the next tick published for %v, want the lap continued from "+
			"charlie and come round to bravo", got)
	}
	if want := []string{"bravo", ""}; !slices.Equal(rooms.afters[reads:], want) {
		t.Errorf("the next tick's reads began after %q, want %q", rooms.afters[reads:], want)
	}
	if d.resume != "" {
		t.Errorf("a lap that came round left the next one to begin after %q", d.resume)
	}
}

// A LAP CUT BETWEEN PAGES IS CONTINUED AFTER THE LAST PAGE IT FINISHED.
//
// The other half of the case above: the deadline, the stop or a failed read
// lands while the next PAGE is being taken rather than inside a room's ladder.
// That read is most of the time between two pages, so it is where a deadline
// usually lands, and a lap that answered it by starting again at the first
// room would be the starvation the resume point exists to end. Every one of
// the three resumes after the last room of the page the lap finished.
func TestALapCutBetweenPagesResumesAfterTheLastPageItFinished(t *testing.T) {
	t.Parallel()
	floor := anchor.Add(-2 * 365 * 24 * time.Hour)
	const total = chatPruneRoomsPage + 3
	var all []chatRoom
	for i := range total {
		all = append(all, room(fmt.Sprintf("room-%05d", i), nil, floor))
	}
	finished := all[chatPruneRoomsPage-1].ID
	refused := errors.New("the estate did not answer")

	for _, tc := range []struct {
		name string
		// cut ends the second read: by cancelling the tick's context
		// inside it, or by failing it outright.
		cut func(cancel context.CancelFunc) func(context.Context, int) error
	}{
		{"the deadline lands inside the read", func(cancel context.CancelFunc) func(context.Context, int) error {
			return func(ctx context.Context, read int) error {
				if read == 2 {
					cancel()
					return ctx.Err()
				}
				return nil
			}
		}},
		{"the read fails", func(context.CancelFunc) func(context.Context, int) error {
			return func(_ context.Context, read int) error {
				if read == 2 {
					return refused
				}
				return nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, pruner, rooms := pruneHarness(
				chatRetentionPolicy{Chatlet: true, CompanyDays: config.MinMessageRetentionDays},
				anchor, all...)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rooms.fail = tc.cut(cancel)

			d.tick(ctx)

			if got := len(pruner.channels()); got != chatPruneRoomsPage {
				t.Fatalf("the cut tick published for %d rooms, want the first "+
					"page's %d", got, chatPruneRoomsPage)
			}
			if d.resume != finished {
				t.Fatalf("the lap cut on its second read resumes after %q, want "+
					"%q — the last room of the page it finished", d.resume, finished)
			}

			rooms.fail = nil
			pruner.calls = nil
			reads := rooms.reads
			d.tick(t.Context())
			if want := []string{finished, ""}; !slices.Equal(rooms.afters[reads:], want) {
				t.Errorf("the next tick's reads began after %q, want %q — the "+
					"rest of the lap, then round from the first room",
					rooms.afters[reads:], want)
			}
			if got := pruner.channels(); len(got) == 0 || got[0] != all[chatPruneRoomsPage].ID {
				t.Errorf("the next tick began its lap at %v, want %q", got, all[chatPruneRoomsPage].ID)
			}
		})
	}
}

// A CUT IS NOT A FAILURE, AND ONLY A FAILED READ IS WARNED ABOUT.
//
// `chat_prune_rooms_unread` tells an operator that rooms were left past their
// horizon because the estate did not answer. A deadline landing inside the
// read is the tick ending on time, which the next tick continues, and warning
// about it would put that line in the log of every company with a backlog,
// once an hour, where it reads as a store that keeps failing.
//
// NOT PARALLEL: it swaps the process-wide logger, which this package otherwise
// leaves at its boot default, and a parallel case's lines would be counted.
func TestOnlyAReadThatFailedIsWarnedAbout(t *testing.T) {
	logs := &stopLineBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	warned := func() int {
		n := 0
		for _, record := range logs.records(t) {
			if record["msg"] == "chat_prune_rooms_unread" {
				n++
			}
		}
		return n
	}
	floor := anchor.Add(-2 * 365 * 24 * time.Hour)
	policy := chatRetentionPolicy{Chatlet: true, CompanyDays: config.MinMessageRetentionDays}

	d, _, rooms := pruneHarness(policy, anchor, room("alpha", nil, floor))
	ctx, cancel := context.WithCancel(t.Context())
	rooms.fail = func(ctx context.Context, _ int) error {
		cancel()
		return ctx.Err()
	}
	d.tick(ctx)
	cancel()
	if n := warned(); n != 0 {
		t.Errorf("a deadline inside the read was warned about %d time(s) as a "+
			"read the estate did not answer", n)
	}

	d, _, rooms = pruneHarness(policy, anchor, room("alpha", nil, floor))
	rooms.fail = func(context.Context, int) error { return errors.New("the estate did not answer") }
	d.tick(t.Context())
	if n := warned(); n != 1 {
		t.Errorf("a read the estate refused was warned about %d time(s), want once", n)
	}
}

// A TICK THAT ENDS BEFORE ITS FIRST READ KEEPS THE LAP WHERE IT WAS.
//
// A claim that took the tick's whole budget, or a stop that arrived with it,
// leaves nothing read and nothing swept — so the lap the previous tick cut is
// still the lap to continue, and forgetting it here would restart it at the
// first room.
func TestATickEndedBeforeItsFirstReadKeepsTheLap(t *testing.T) {
	t.Parallel()
	floor := anchor.Add(-2 * 365 * 24 * time.Hour)
	d, pruner, rooms := pruneHarness(
		chatRetentionPolicy{Chatlet: true, CompanyDays: config.MinMessageRetentionDays},
		anchor, room("alpha", nil, floor), room("bravo", nil, floor))
	d.resume = "alpha"
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	d.tick(ctx)

	if rooms.reads != 0 || len(pruner.calls) != 0 {
		t.Fatalf("a tick ended before it began read %d pages and published %d "+
			"records", rooms.reads, len(pruner.calls))
	}
	if d.resume != "alpha" {
		t.Errorf("a tick ended before its first read left the lap to resume "+
			"after %q, want %q", d.resume, "alpha")
	}
}

// AN UNSETTLED WRITE ENDS THAT ROOM'S LADDER AND NOBODY ELSE'S.
//
// `pending` and `unknown` are not failures — they are the framework's other
// two answers — but the rungs above were computed from a floor this node has
// not seen move, so the honest move is to take the rest next tick against a
// floor that actually changed.
func TestAnUnsettledPruneStopsThatRoomsLadder(t *testing.T) {
	t.Parallel()
	floor := anchor.Add(-2 * 365 * 24 * time.Hour)
	for _, outcome := range []statelog.Outcome{statelog.OutcomePending, statelog.OutcomeUnknown} {
		t.Run(string(outcome), func(t *testing.T) {
			t.Parallel()
			d, pruner, _ := pruneHarness(
				chatRetentionPolicy{Chatlet: true, CompanyDays: config.MinMessageRetentionDays},
				anchor,
				room("alpha", nil, floor),
				room("bravo", nil, floor),
			)
			pruner.outcome = outcome

			d.tick(t.Context())

			if n := len(pruner.cutoffsFor("alpha")); n != 1 {
				t.Errorf("an %s write was followed by %d more records in the "+
					"same room; the ladder above it was computed from a floor "+
					"that has not moved", outcome, n-1)
			}
			if n := len(pruner.cutoffsFor("bravo")); n != 1 {
				t.Errorf("one room's %s write cost the next room its own "+
					"record (%d published)", outcome, n)
			}
		})
	}
}

// THE RECORD IS AUTHORED BY THE ENGINE, NAMING THIS NODE.
//
// A prune is machinery rather than a remark. Attributing it to a seat or to an
// operator token would put a participant's name on a deletion nobody asked
// for, and [chatlet.Store.Prune] refuses every other author kind in its own
// decide — so a duty signing as an agent would publish nothing at all.
func TestThePruneIsAuthoredBySystemNamingTheNode(t *testing.T) {
	t.Parallel()
	floor := anchor.Add(-2 * 365 * 24 * time.Hour)
	d, pruner, _ := pruneHarness(
		chatRetentionPolicy{Chatlet: true, CompanyDays: config.MinMessageRetentionDays},
		anchor, room("general", nil, floor))

	d.tick(t.Context())

	if len(pruner.calls) == 0 {
		t.Fatal("nothing was published, so there is no author to check")
	}
	actor := pruner.calls[0].actor
	if actor.Kind != chatlet.AuthorSystem {
		t.Errorf("the prune was authored as %q, want %q", actor.Kind, chatlet.AuthorSystem)
	}
	if actor.Handle != "node-a" {
		t.Errorf("the prune names %q, want this node — a system write signs "+
			"with the node id, exactly as the tracker's does", actor.Handle)
	}
}

// A COMPANY WHOSE ROOMS MOVED TO A VENDOR PUBLISHES NOTHING, EVEN WHERE THE
// ROWS ARE STILL HERE.
//
// The rows a node keeps after a company leaves Chatlet are the archive of
// a surface nobody writes to any more. Deleting them on a policy the company
// no longer expresses destroys the only copy of a conversation that has
// stopped being replaceable — and the rooms are not even read, because there
// is nothing this duty could do with them.
func TestACompanyOffChatletPrunesNothing(t *testing.T) {
	t.Parallel()
	d, pruner, rooms := pruneHarness(
		chatRetentionPolicy{CompanyDays: config.MinMessageRetentionDays},
		anchor, room("general", nil, anchor.Add(-2*365*24*time.Hour)))

	d.tick(t.Context())

	if len(pruner.calls) != 0 {
		t.Fatalf("a company that is not on Chatlet had %d prune records "+
			"published against its archive", len(pruner.calls))
	}
	if rooms.reads != 0 {
		t.Errorf("the rooms were read %d times on a company off Chatlet", rooms.reads)
	}
}

// A ROOM READ THAT FAILED PRUNES NOTHING RATHER THAN GUESSING.
//
// The alternative is a sweep that treats an unreadable estate as an empty
// company, which publishes nothing either — but silently, with no line saying
// the horizon went unenforced.
func TestAFailedRoomReadPrunesNothing(t *testing.T) {
	t.Parallel()
	d, pruner, rooms := pruneHarness(
		chatRetentionPolicy{Chatlet: true, CompanyDays: config.MinMessageRetentionDays},
		anchor, room("general", nil, anchor.Add(-2*365*24*time.Hour)))
	rooms.err = errors.New("this estate is not open")

	d.tick(t.Context())

	if len(pruner.calls) != 0 {
		t.Fatalf("published %d records from a room read that failed", len(pruner.calls))
	}
}

// THE POLICY IS THE DOCUMENT'S, WITH THE ACCESSOR'S OWN DEFAULT.
//
// Through [config.ChatletConfig.MessageRetention] rather than off the
// field, so the 365-day default is applied in one place rather than two that
// can disagree. Which document that is, is the fleet's pointer's, read per
// tick (TestTheChatPruneDecidesOnlyOnANodeServingTheFleetsRevision).
func TestTheChatRetentionPolicyIsTheDocumentsOwn(t *testing.T) {
	t.Parallel()
	const base = `
name: Nimbus
providers:
  sandbox: {fake: true}
  llm:
    scripted:
      model: claude-x
roles:
  - name: CEO
    handle: ceo
    llm: scripted
`
	cases := map[string]struct {
		doc  string
		want chatRetentionPolicy
	}{
		"a company that writes no chat block at all": {
			doc:  base,
			want: chatRetentionPolicy{Chatlet: true, CompanyDays: config.DefaultMessageRetentionDays},
		},
		"a company that named a horizon": {
			doc:  base + "chat:\n  chatlet:\n    message_retention_days: 90\n",
			want: chatRetentionPolicy{Chatlet: true, CompanyDays: 90},
		},
		"a company that keeps everything for ever": {
			doc:  base + "chat:\n  chatlet:\n    message_retention_days: 0\n",
			want: chatRetentionPolicy{Chatlet: true, CompanyDays: chatlet.RetentionForever},
		},
		"a company with no chat surface": {
			doc:  base + "chat:\n  backend: none\n",
			want: chatRetentionPolicy{},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := chatRetentionPolicyOf(companyWith(t, tc.doc)); got != tc.want {
				t.Fatalf("policy = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A NODE THAT DOES NOT RUN THE ROOMS ARMS NO DUTY — AND NEITHER DOES ONE THAT
// MAY NOT PUBLISH.
//
// The gate is the STORE rather than the policy: whether this node runs the
// chat domain is decided once at boot, and a loop armed without it would read
// tables that are not there once an hour for the life of the process.
//
// AND THE STORE IS THE WRITE SIDE'S ([Engine.writeSide]), never the runtime's
// own: a node started in `maintenance` or `seal` publishes nothing onto any
// log, and a prune is a record on chat's. Taken from the runtime, the duty
// would publish deletions into a capacity window whose whole point — a seal's
// acknowledgement being evidence — is that nothing is being written. Every
// other gate on that path is a caller's, so this case holds the duty's own.
func TestTheChatPruneIsArmedOnlyWhereTheRoomsAre(t *testing.T) {
	t.Parallel()
	node, _ := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = node.Close() })
	// rooms is a data node running the chat domain, in the given mode: its
	// core holds the logs, and its native half the chat store.
	rooms := func(mode statelog.MaintenanceMode) *Engine {
		e := &Engine{mode: mode, backends: &Backends{Store: node}}
		e.core.Store(&core{nodeID: "node-a", log: &stateLog{db: node}})
		e.native.Store(&native{chat: &chatlet.Store{}})
		return e
	}
	noChat := &Engine{mode: statelog.ModeNormal}
	noChat.native.Store(&native{})
	// A DATA NODE THAT RUNS NO WORKERS holds the rooms and never claims the
	// duty, so it arms none: armed, it would ask the fleet's pointer and
	// warn on every tick about a horizon it could never enforce.
	noWorkers := rooms(statelog.ModeNormal)
	noWorkers.profile = placement.NodeProfile{
		Roles: placement.Roles(placement.RoleData, placement.RoleSeats)}
	for name, e := range map[string]*Engine{
		"an engine with no native backends":     {},
		"a node running no chat domain":         noChat,
		"a node in a maintenance window":        rooms(statelog.ModeMaintenance),
		"a node sealed for a capacity decision": rooms(statelog.ModeSeal),
		"a data node that runs no workers":      noWorkers,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e.startChatRetention(t.Context())
			if e.chatPrune != nil {
				e.stopChatRetention()
				t.Fatal("a chat prune duty was armed on a node that has no rooms, " +
					"may not publish into them or never claims the duty")
			}
			// And the teardown is a no-op rather than a nil dereference,
			// which is what the boot path's failure arm reaches it as.
			e.stopChatRetention()
		})
	}

	// THE CONTROL: the same node, publishing, arms it — or every case
	// above would pass on a duty that is never armed anywhere.
	e := rooms(statelog.ModeNormal)
	e.startChatRetention(t.Context())
	if e.chatPrune == nil {
		t.Fatal("a publishing node running the chat domain armed no prune duty")
	}
	e.stopChatRetention()
	if e.chatPrune != nil {
		t.Fatal("the teardown left the duty armed")
	}
}

// THE ROOM READ IS THE ONE PIECE OF THIS DUTY A STUB CANNOT COVER.
//
// Every case above drives the ladder against fakes, which is what makes the
// arithmetic exercisable — but it means the SQL that feeds them is never run.
// A wrong column name, a lost nullness or a misread instant would then show up
// only as one `chat_prune_rooms_unread` line an hour on a live company, with
// the horizon quietly unenforced. So this one runs against the real schema.
//
// WHAT IT PROTECTS: that the three settings the nullable column carries
// survive the read as three (inherit / for ever / a number), and that a room's
// floor is the OLDEST message in it rather than the newest, the first read or
// zero.
func TestTheRoomReadCarriesAllThreeRetentionSettingsAndTheRoomsFloor(t *testing.T) {
	t.Parallel()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })

	oldest := anchor.Add(-90 * 24 * time.Hour)
	err := estate.Tx(t.Context(), func(tx *sql.Tx) error {
		for _, room := range []struct {
			id        string
			retention any
		}{
			{"inherits", nil},
			{"forever", 0},
			{"ninety", 90},
			{"empty", nil},
		} {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO chat_channels (id, kind, retention_days, created_at, version, document)
				VALUES (?, 'public', ?, ?, 1, X'7b7d')`,
				room.id, room.retention, store.EncodeTime(anchor)); err != nil {
				return err
			}
		}
		// Written NEWEST FIRST, so a read that returned the first row it
		// saw rather than the minimum would answer with the wrong one.
		for i, at := range []time.Time{
			oldest.Add(48 * time.Hour), oldest.Add(24 * time.Hour), oldest,
		} {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO chat_messages
				  (id, channel_id, author_handle, author_kind, channel_seq,
				   created_at, version, document)
				VALUES (?, 'ninety', 'ceo', 'agent', ?, ?, 1, X'7b7d')`,
				"m"+string(rune('a'+i)), i+1, store.EncodeTime(at)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed the rooms: %v", err)
	}

	reader := chatRoomRows{db: estate.Reader()}
	rooms, err := reader.PruneRooms(t.Context(), "", chatPruneRoomsPage)
	if err != nil {
		t.Fatalf("PruneRooms: %v", err)
	}

	// A PAGE IS THE NEXT ROOMS IN ID ORDER, STRICTLY AFTER THE ONE NAMED:
	// two pages of two cover the four rooms once each, and the second
	// begins after the first's last.
	var paged []string
	for after := ""; ; {
		page, pageErr := reader.PruneRooms(t.Context(), after, 2)
		if pageErr != nil {
			t.Fatalf("PruneRooms after %q: %v", after, pageErr)
		}
		for _, room := range page {
			paged = append(paged, room.ID)
		}
		if len(page) < 2 {
			break
		}
		after = page[len(page)-1].ID
	}
	if want := []string{"empty", "forever", "inherits", "ninety"}; !slices.Equal(paged, want) {
		t.Errorf("paging two rooms at a time read %v, want %v", paged, want)
	}
	got := map[string]chatRoom{}
	for _, room := range rooms {
		got[room.ID] = room
	}
	if len(got) != 4 {
		t.Fatalf("read %d rooms, want 4: %+v", len(got), rooms)
	}
	if o := got["inherits"].RetentionDays; o != nil {
		t.Errorf("a room with a NULL retention_days read back as %d — absent is "+
			"\"take the company's\" and collapsing it into a number is how a "+
			"room gets a horizon nobody set", *o)
	}
	if o := got["forever"].RetentionDays; o == nil || *o != chatlet.RetentionForever {
		t.Errorf("a room set to keep its messages for ever read back as %v — a "+
			"present zero and an absent value are different settings and only "+
			"the pointer tells them apart", o)
	}
	if o := got["ninety"].RetentionDays; o == nil || *o != 90 {
		t.Errorf("a room's own 90-day horizon read back as %v", o)
	}
	if floor := got["ninety"].Oldest; !floor.Equal(oldest) {
		t.Errorf("the room's floor is %s, want its oldest message %s — the "+
			"ladder is anchored on this instant, so a newer one skips rows the "+
			"horizon covers and nothing ever comes back for them", floor, oldest)
	}
	if floor := got["empty"].Oldest; !floor.IsZero() {
		t.Errorf("a room holding no message answered a floor of %s; MIN over no "+
			"rows is NULL, and reading it as an instant would publish a prune "+
			"at the Unix epoch every tick for ever", floor)
	}
}

// THE ROOM READ SEEKS EACH ROOM'S OLDEST MESSAGE; IT NEVER READS THE CORPUS.
//
// The read runs every hour over every room the company ever made, and the
// answer it needs from `chat_messages` is one row per room — the leading edge
// of `chat_messages_prune_idx`. The obvious spelling, `MIN(created_at)` in a
// correlated subquery, is planned here as a scan of that whole index into an
// ephemeral table of every room's minimum: correct answers, and every message
// the company has ever kept read once an hour, a cost that grows with its
// whole history and that no result reveals. So the plan is held, not the rows.
//
// NEVER ANALYZEd, because no node is: a plan taken over statistics is a plan
// no deployment runs.
func TestTheRoomReadSeeksEachRoomsOldestMessage(t *testing.T) {
	t.Parallel()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })

	var plan []string
	err := estate.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+chatPruneRoomsSQL,
			"room-0500", chatPruneRoomsPage)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				return err
			}
			plan = append(plan, detail)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("explain the room read: %v", err)
	}
	const seek = "SEARCH m USING COVERING INDEX chat_messages_prune_idx (channel_id=?)"
	if !slices.Contains(plan, seek) {
		t.Errorf("the room read's plan is %q, which never takes %q — each "+
			"room's oldest message must be one seek of the prune index", plan, seek)
	}
	// AND THE PAGE IS A KEYSET SEEK of the rooms' own key, walked in the
	// order it is stored: a scan, or a sorter, would read every room the
	// company ever made to answer one page of them.
	const page = "SEARCH c USING INDEX sqlite_autoindex_chat_channels_1 (id>?)"
	if !slices.Contains(plan, page) {
		t.Errorf("the room read's plan is %q, which never takes %q — a page "+
			"must seek to where the last one stopped", plan, page)
	}
	for _, step := range plan {
		if strings.Contains(step, "SCAN chat_messages") || strings.Contains(step, "SCAN m ") ||
			strings.Contains(step, "ephemeral") {
			t.Errorf("the room read's plan takes %q: it reads every message the "+
				"company holds to find each room's oldest (whole plan %q)", step, plan)
		}
		if strings.Contains(step, "SCAN chat_channels") || strings.Contains(step, "SCAN c") ||
			strings.Contains(step, "TEMP B-TREE") {
			t.Errorf("the room read's plan takes %q: it reads every room the "+
				"company ever made to answer one page (whole plan %q)", step, plan)
		}
	}
}
