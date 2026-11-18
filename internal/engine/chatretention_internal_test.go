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

// The chat prune duty's cases. They hold that a company keeping its
// conversation for ever keeps it, that a room's own setting decides, that a
// tick is bounded and resumes where the last stopped, that two nodes never
// publish one ladder, and that the rungs are the broker's instants rather than
// this node's clock.
//
// Every case drives [chatPrune.tick] directly against stub seams; the loop
// around it is only a ticker and a claim.

// fakeClock is the duty's clock, movable by the case. It has no mutex: the
// cases call tick synchronously, on the test's goroutine.
type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

// prunePublished is one record the duty published.
type prunePublished struct {
	channel string
	cutoff  time.Time
	actor   chatlet.Actor
	opID    string
}

// fakePruner records what was published and models the applier: a prune
// deletes strictly below its cutoff, so the room's floor becomes the cutoff
// itself — the densest room, the case the per-record bound exists for.
type fakePruner struct {
	rooms   *fakeRooms
	calls   []prunePublished
	outcome statelog.Outcome

	// refuse is the rooms this build cannot write to, by id: per room, as in
	// the write path, where an unclassifiable kind refuses its room only.
	refuse map[string]error

	// after, when set, is called with the count published so far after each
	// record; a case lands the tick's deadline there.
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
// in room-id order, strictly after the room named, as the real statement seeks.
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

// override is an addressable retention day count: the column is nullable, so
// every carrier of it is a pointer.
func override(n int) *int { return &n }

// pruneHarness wires a duty over stubs that already holds the duty, so every
// case is about the sweep rather than the lease.
//
// The fake copies the rooms: an applied prune moves a floor in place
// ([fakeRooms.advance]), and parallel subtests spreading one slice (`all...`)
// would share its backing array and race on each other's floors.
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

// anchor is a fixed instant, so a case's arithmetic is readable.
var anchor = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

// A COMPANY THAT KEEPS EVERYTHING FOR EVER PUBLISHES NOTHING.
//
// `message_retention_days: 0` means for ever, the opposite of a duration's
// zero, which is why the accessor answers it with a second return. A duty that
// ignored it would publish a cutoff of now, deleting the whole conversation on
// its first tick, with no inverse.
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
// A company keeping everything may still set thirty days on one room, so an
// early return on "no company horizon" would break the second half;
// [chatlet.HorizonFor] alone decides which setting wins.
func TestARoomsOwnHorizonBeatsTheCompanys(t *testing.T) {
	t.Parallel()
	// Below both the company's year and the room's month, so the horizon
	// applied decides whether anything is published.
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
// A prune that deletes nothing still costs a record on the company's busiest
// log, every tick, for every room, replicated to every node.
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
// The bound keeps one delete of a whole backlog from holding the only writer
// on every node; the rows themselves are the bookmark, so nothing has to
// remember how far a capped sweep got.
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

	// The rungs are contiguous windows ending exactly on the horizon: no gap
	// (rows left below a cutoff nobody republishes), no overlap (empty records).
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

	// Every rung is its own operation carrying its mint instant: one without is
	// read as older than every loss of the chat ledger, so a node whose ledger
	// lost a row would answer every prune `unknown` and retention would stop
	// silently.
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
// A second publisher is not corruption (a range delete below an instant is
// idempotent) but doubles every prune record on the hottest log. The claim is
// checked before the rooms are read, so a fleet reads them once a tick.
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
// A message's `created_at` is the broker's stored instant, so a rung anchored
// on a room's oldest message is identical on every node and unmoved by a late
// tick or skewed clocks; anchored on `now`, every tick would publish new
// cutoffs for the same rows.
//
// The complement is asserted too, or a duty publishing nothing would pass: the
// final rung, which brings a room inside its horizon, moves with the clock.
func TestThePruneCutoffIsTheBrokersInstantAndNotTheDutysClock(t *testing.T) {
	t.Parallel()
	horizon := anchor.Add(-config.MinMessageRetentionDays * 24 * time.Hour)
	// Several windows deep, so the first rung is a step, not the horizon.
	backlogged := horizon.Add(-5 * chatPruneWindow)
	// Within one window, so its only rung is the horizon.
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
// A room of a kind this build cannot classify refuses every write; giving up
// there would hold the company's retention hostage to one row a newer peer
// wrote. The order is [chatlet.Eligible]'s, so every node publishes the same
// sweep in the same sequence.
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
	// The middle room in id order refuses, so a sweep that gave up on a
	// refusal never reaches the last.
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
// Nothing bounds that set (an archive frees its [chatlet.MaxChannels] slot, a
// direct conversation is never refused by it, no room is deleted), so it is
// read a page at a time, each after the last room of the one before, and every
// room on every page is swept.
func TestThePruneSweepPagesThroughEveryRoom(t *testing.T) {
	t.Parallel()
	floor := anchor.Add(-2 * 365 * 24 * time.Hour)
	// Two whole pages and a short one, so a sweep that read once or stopped
	// at the first full page leaves rooms unpruned.
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
// A tick may not outlive its interval, so with enough backlog a lap ends
// early. Restarted at the first room id, every tick would spend itself on the
// low ids and the tail would keep messages past its horizon. So the next tick
// begins after the last room finished, comes round and stops where it began;
// a room whose ladder was cut is taken again, not skipped.
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

	// The deadline lands inside charlie's ladder: two whole rooms and part
	// of a third.
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
// A deadline, a stop or a failed read usually lands while the next page is
// read; restarting at the first room there is the starvation the resume point
// exists to end. Each resumes after the last room of the finished page.
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
		// cut ends the second read, by cancelling the tick inside it or
		// by failing it.
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
// `chat_prune_rooms_unread` says rooms went past their horizon because the
// estate did not answer. A deadline inside the read is the tick ending on
// time; warning about it would log, hourly on every backlogged company, what
// reads as a failing store.
//
// Not parallel: it swaps the process-wide logger, and a parallel case's lines
// would be counted.
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
// A claim that took the tick's budget, or a stop with it, swept nothing, so
// the cut lap is still the one to continue; forgetting it would restart at
// the first room.
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
// `pending` and `unknown` are not failures, but the rungs above were computed
// from a floor this node has not seen move; the rest waits for a tick against
// a floor that changed.
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
// A prune is machinery: a seat's or an operator's name would sit on a deletion
// nobody asked for, and [chatlet.Store.Prune] refuses every other author kind,
// so a duty signing as an agent would publish nothing.
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
// The rows left after a company leaves Chatlet are the only copy of an archive
// nobody writes to; deleting them on a policy the company no longer expresses
// is irreversible. The rooms are not even read.
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
// Read as an empty company, a failed read would publish nothing too, but with
// no line saying the horizon went unenforced.
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
// Read through [config.ChatletConfig.MessageRetention], so the 365-day
// default lives in one place. Which document is the fleet pointer's, read per
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
// The gate is the store, decided once at boot, not the policy: armed without
// it, the loop would read absent tables hourly for the life of the process.
//
// And it is the write side's store ([Engine.writeSide]), never the runtime's:
// a node in `maintenance` or `seal` publishes nothing, and a prune published
// into a seal's capacity window would void its acknowledgement as evidence.
// Every other gate on that path is a caller's, so this case holds the duty's.
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
	// A data node running no workers never claims the duty, so arms none:
	// armed, it would warn every tick about a horizon it could never enforce.
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
			// The teardown is a no-op, not a nil dereference: the boot
			// path's failure arm reaches it unarmed.
			e.stopChatRetention()
		})
	}

	// The control: the same node, publishing, arms it, or every case above
	// would pass on a duty never armed anywhere.
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
// The cases above never run the SQL feeding the ladder; a wrong column, a lost
// nullness or a misread instant would show only as an hourly
// `chat_prune_rooms_unread` line, so this runs against the real schema. It
// holds that the nullable column's three settings (inherit, for ever, a
// number) survive as three, and that a room's floor is its oldest message.
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
		// Newest first, so a read returning the first row rather than the
		// minimum answers wrong.
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

	// A page is the next rooms in id order, strictly after the one named:
	// two pages of two cover the four rooms once each.
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
// The read runs hourly over every room and needs one row per room from
// `chat_messages`, the leading edge of `chat_messages_prune_idx`. Spelled
// `MIN(created_at)` in a correlated subquery, the planner unnests it into a
// grouped read of the whole index: right answers, at a cost growing with the
// company's history that no result reveals. So the plan is held, not the
// rows, the rewrite recognised by [storetest.Decorrelated], the predicate
// every plan gate shares. Never ANALYZEd, because no node is.
//
// Mutation: spell the floor `MIN(m.created_at)` again and this fails.
func TestTheRoomReadSeeksEachRoomsOldestMessage(t *testing.T) {
	t.Parallel()
	db, estate := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = db.Close() })

	plan := storetest.Plan(t, estate, chatPruneRoomsSQL, []any{"room-0500", chatPruneRoomsPage})
	const seek = "SEARCH m USING COVERING INDEX chat_messages_prune_idx (channel_id=?)"
	if !slices.Contains(plan, seek) {
		t.Errorf("the room read's plan is %q, which never takes %q — each "+
			"room's oldest message must be one seek of the prune index", plan, seek)
	}
	// The page is a keyset seek of the rooms' key in stored order: a scan or
	// a sorter would read every room to answer one page.
	const page = "SEARCH c USING INDEX sqlite_autoindex_chat_channels_1 (id>?)"
	if !slices.Contains(plan, page) {
		t.Errorf("the room read's plan is %q, which never takes %q — a page "+
			"must seek to where the last one stopped", plan, page)
	}
	if step, unnested := storetest.Decorrelated(plan); unnested {
		t.Errorf("the room read's plan takes %q: its floor is a per-room "+
			"aggregate the planner answers from every message the company holds "+
			"(whole plan %q)", step, plan)
	}
	for _, step := range plan {
		if strings.Contains(step, "SCAN chat_messages") || strings.Contains(step, "SCAN m ") {
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
