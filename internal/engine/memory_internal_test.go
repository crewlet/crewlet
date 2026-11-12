package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/learning/memsync"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/words"
)

// A SEAT'S MEMORY TRAVELS ON AN EXTERNAL BROKER TOO.
//
// A seat's memory follows it between nodes over the changelog (adr/0014), and
// the topology whose seats move between machines most is the one dialling an
// external cluster. The syncer was built over the embedded broker's second
// connection, which is nil by design when the node dialled out — so on
// exactly that topology it came up nil, and every seat forgot what it had
// learned each time placement moved it, with nothing logged.
func TestSeatMemoryTravelsOnAnExternalBroker(t *testing.T) {
	t.Parallel()
	// THROUGH externaltest, whose server's account states a limit: a server
	// started here with none made the node stand this machine's free disk in
	// for its broker's limit, and a fresh node's reservations refused the boot
	// on a runner short of them.
	broker := externaltest.Start(t, queue.MaxPayloadBytes)

	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.Type = config.StreamNATS
	b.Stream.URL = broker.URL()
	b.Stream.Token = externaltest.Token
	b.Coordination.Type = config.CoordinationEmbeddedKV
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg})
	if err != nil {
		t.Fatalf("New on an external broker: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })

	if e.memory == nil {
		t.Fatal("a node on an external broker built no memory syncer, so a " +
			"seat it acquires hydrates nothing and one it releases carries " +
			"nothing — every move is a seat that forgot")
	}
}

// scriptedMemory is the memory syncer as a seat's turn calls it: each seat's
// own pass is played by a script, and so is its backlog, and it records the
// seats the running cycle or flush called, those it called on a bound already
// spent, and those whose call returned with nothing wrong, for each call, and
// every call in the order it was made.
type scriptedMemory struct {
	// play is seat's own pass, handed the bound and runOut, which spends it
	// as a seat the cycle cannot carry does before its call returns; first
	// says the cycle called no seat before it.
	play func(ctx context.Context, seat string, first bool, runOut func()) (memsync.Carried, error)

	// playBacklog is seat's backlog, handed the same; nil is a backlog that
	// carries what it holds and returns with nothing wrong.
	playBacklog func(ctx context.Context, seat string, runOut func()) (memsync.Carried, error)

	runOut context.CancelFunc

	// calls, late and completed are the own passes; backlog, backlogLate and
	// backlogDone the backlogs.
	calls, late, completed            []string
	backlog, backlogLate, backlogDone []string

	// order is every call as it was made: "own:a", "backlog:a".
	order []string
}

// PublishOwn plays the seat's script, after failing at once on a bound
// already spent, as the syncer's first read of a seat's rows does.
func (m *scriptedMemory) PublishOwn(ctx context.Context, seat string) (memsync.Carried, error) {
	first := len(m.calls) == 0
	m.calls = append(m.calls, seat)
	m.order = append(m.order, "own:"+seat)
	if err := ctx.Err(); err != nil {
		m.late = append(m.late, seat)
		return memsync.Carried{StoppedIn: "agent_diary"},
			fmt.Errorf("memsync: read agent_diary: %w", err)
	}
	carried, err := m.play(ctx, seat, first, m.runOut)
	if err == nil {
		m.completed = append(m.completed, seat)
	}
	return carried, err
}

// PublishBacklog plays the seat's backlog script, failing at once on a bound
// already spent as PublishOwn does.
func (m *scriptedMemory) PublishBacklog(ctx context.Context, seat string) (memsync.Carried, error) {
	m.backlog = append(m.backlog, seat)
	m.order = append(m.order, "backlog:"+seat)
	if err := ctx.Err(); err != nil {
		m.backlogLate = append(m.backlogLate, seat)
		return memsync.Carried{StoppedIn: "agent_diary"},
			fmt.Errorf("memsync: read agent_diary: %w", err)
	}
	var carried memsync.Carried
	var err error
	if m.playBacklog != nil {
		carried, err = m.playBacklog(ctx, seat, m.runOut)
	}
	if err == nil {
		m.backlogDone = append(m.backlogDone, seat)
	}
	return carried, err
}

// begin starts a cycle or a flush under the bound runOut spends: everything
// recorded is then that one's.
func (m *scriptedMemory) begin(runOut context.CancelFunc) {
	m.runOut, m.calls, m.late, m.completed = runOut, nil, nil, nil
	m.backlog, m.backlogLate, m.backlogDone, m.order = nil, nil, nil, nil
}

// cycle runs one cycle of loop over the held seats, as syncSeatMemory does,
// under a bound the script can spend.
func (m *scriptedMemory) cycle(t *testing.T, loop *memorySync, held ...string) syncReport {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.begin(cancel)
	return loop.cycle(ctx, seatsHeld{held: held}, m)
}

// flush runs a released seat's last turn, as releaseSeat does, under a bound
// the script can spend, and answers what it says.
func (m *scriptedMemory) flush(t *testing.T, seat string) []reportLine {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.begin(cancel)
	return flushSeatMemory(ctx, m, seat)
}

// carriedWhole is what the publish of a seat that reached the end of every
// table answers.
var carriedWhole = memsync.Carried{Rows: 6, Marked: 3}

// draining is what a publish its bound cut inside a backlog answers: the rows
// the changelog acknowledged, each of which moved a mark.
var draining = memsync.Carried{Rows: 428, Marked: 428, StoppedIn: "agent_diary"}

// runsOut is the publish of a seat whose memory the cycle cannot carry: it
// spends the cycle's bound and answers what the bound cut it at.
func runsOut(ctx context.Context, runOut func(), cut memsync.Carried) (memsync.Carried, error) {
	runOut()
	return cut, fmt.Errorf("memsync: publish a %s row: %w", cut.StoppedIn, ctx.Err())
}

// A CYCLE ITS FIRST SEAT USES UP CARRIES THE NEXT SEAT ON THE NEXT.
//
// Every seat the node holds shares a cycle's thirty seconds, one after
// another. Each cycle used to start at the first sorted seat and call every
// seat whatever was left of its bound, so a seat whose backlog outlasted a
// cycle had every cycle, and each seat after it was called on a bound already
// spent, which failed it at once: none was carried while the backlog lasted
// (crewlet/crewlet#1033). A cycle now stops at its bound, and the next starts
// after the seat that had the whole cycle.
//
// Mutation: start every cycle at the first sorted seat, start the next at the
// seat the bound cut whatever its place, keep calling seats once the bound
// has run out, or forget where the last cycle ran out, and this goes red.
func TestACycleTheFirstSeatUsesUpCarriesTheNextSeatOnTheNext(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: func(ctx context.Context, seat string, _ bool,
		runOut func(),
	) (memsync.Carried, error) {
		if seat == "a" {
			return runsOut(ctx, runOut, draining)
		}
		return carriedWhole, nil
	}}
	loop := &memorySync{}

	m.cycle(t, loop, "a", "b", "c")
	if !slices.Equal(m.calls, []string{"a"}) {
		t.Fatalf("the cycle seat a ran out called %s, want a alone: the seats after it "+
			"were called on a bound already spent, which fails each at once", words.And(m.calls))
	}
	m.cycle(t, loop, "a", "b", "c")
	if !slices.Equal(m.completed, []string{"b", "c"}) {
		t.Fatalf("the next cycle carried %s, want b and c: the seat whose backlog outlasts "+
			"a cycle had that one too", words.And(m.completed))
	}
	m.cycle(t, loop, "a", "b", "c")
	if !slices.Equal(m.calls, []string{"a"}) {
		t.Errorf("the third cycle called %s, want a alone, which ran the second out after "+
			"b and c had been carried", words.And(m.calls))
	}
}

// A SEAT THAT RAN A CYCLE OUT BEHIND ANOTHER GOES FIRST IN THE NEXT.
//
// A seat whose publish fits in a whole cycle, but not in what a seat before it
// left of one, runs every cycle out when it comes second. Started after it,
// the next cycle would put it second again, and it would never complete.
//
// Mutation: start the next cycle after the seat the bound cut, wherever it
// was, and b never completes.
func TestASeatThatRanOutBehindAnotherGoesFirstNext(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: func(ctx context.Context, _ string, first bool,
		runOut func(),
	) (memsync.Carried, error) {
		if first {
			return carriedWhole, nil
		}
		return runsOut(ctx, runOut, draining)
	}}
	loop := &memorySync{}

	m.cycle(t, loop, "a", "b")
	if !slices.Equal(m.calls, []string{"a", "b"}) || !slices.Equal(m.completed, []string{"a"}) {
		t.Fatalf("the premise: the first cycle called %s and completed %s, want a and b "+
			"called and a alone completed", words.And(m.calls), words.And(m.completed))
	}
	m.cycle(t, loop, "a", "b")
	if !slices.Contains(m.completed, "b") {
		t.Fatalf("the cycle after b ran one out completed %s, want b: put second again, a "+
			"seat that fits a whole cycle but not a remainder never completes",
			words.And(m.completed))
	}
}

// A CYCLE THAT RAN OUT BETWEEN SEATS STARTS AT THE FIRST IT DID NOT REACH.
//
// A seat's publish can return with nothing wrong just as the bound runs out.
// The seats after it were not reached, so the next cycle starts at the first of
// them: not at the first sorted seat, and not at the seat that completed, which
// would have the cycle again ahead of the seats it held back.
//
// Mutation: start the next cycle at the first sorted seat, or at the seat
// whose publish returned, and this goes red.
func TestACycleThatRanOutBetweenSeatsStartsAtTheFirstNotReached(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: func(_ context.Context, seat string, _ bool,
		runOut func(),
	) (memsync.Carried, error) {
		if seat == "a" {
			runOut()
		}
		return carriedWhole, nil
	}}
	loop := &memorySync{}

	m.cycle(t, loop, "a", "b", "c")
	if !slices.Equal(m.calls, []string{"a"}) {
		t.Fatalf("the cycle whose bound ran out as a's publish returned called %s, want a "+
			"alone", words.And(m.calls))
	}
	m.cycle(t, loop, "a", "b", "c")
	if len(m.calls) == 0 || m.calls[0] != "b" {
		t.Errorf("the next cycle called %s, want b first: the first seat the cycle before "+
			"it did not reach", words.And(m.calls))
	}
}

// A LONE SEAT THAT RUNS EVERY CYCLE OUT IS CARRIED EVERY CYCLE.
//
// The next cycle starts after the seat that had the whole cycle, wrapping
// round, and a lone seat is its own next: after the last seat of the cycle's
// order comes the first. A lone seat is the first of every cycle wherever the
// cycle starts, so what this holds is the wrap, without which the cycle
// indexes past the end of its order to find the seat after it.
//
// Mutation: take the seat after it without wrapping round, and the cycle
// panics.
func TestALoneSeatThatRunsEveryCycleOutIsCarriedEveryCycle(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: func(ctx context.Context, _ string, _ bool,
		runOut func(),
	) (memsync.Carried, error) {
		return runsOut(ctx, runOut, draining)
	}}
	loop := &memorySync{}
	for cycle := 1; cycle <= 3; cycle++ {
		m.cycle(t, loop, "a")
		if !slices.Equal(m.calls, []string{"a"}) {
			t.Fatalf("cycle %d called %s, want a once", cycle, words.And(m.calls))
		}
	}
}

// EVERY SEAT IS CARRIED WITHIN TWO CYCLES FOR EACH SEAT THAT RUNS ONE OUT.
//
// The bound the rule keeps ([memorySync.resume]), in the case that reaches its
// whole-cycle half: of four seats, a and c run every cycle out and b and d
// fit. With k = 2 seats the cycles run out at, a and c each have a whole cycle
// in every 2k = 4 consecutive cycles, and b and d, which no cycle cuts here,
// each complete in every four as well.
//
// Mutation: when the cycle's first seat ran out, start the next cycle at the
// seat after the one after it, and b and d never complete.
func TestEverySeatIsCarriedWithinTwoCyclesForEachSeatThatRunsOut(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: func(ctx context.Context, seat string, _ bool,
		runOut func(),
	) (memsync.Carried, error) {
		if seat == "a" || seat == "c" {
			return runsOut(ctx, runOut, draining)
		}
		return carriedWhole, nil
	}}
	const cycles, round = 8, 4 // round is 2k, with k = 2 seats that run cycles out
	loop := &memorySync{}
	started := make([]string, cycles)
	completed := make([][]string, cycles)
	for i := range cycles {
		m.cycle(t, loop, "a", "b", "c", "d")
		started[i], completed[i] = m.calls[0], m.completed
	}
	for from := 0; from+round <= cycles; from++ {
		window := fmt.Sprintf("cycles %d to %d", from+1, from+round)
		for _, seat := range []string{"b", "d"} {
			if !slices.ContainsFunc(completed[from:from+round], func(done []string) bool {
				return slices.Contains(done, seat)
			}) {
				t.Errorf("seat %s completed in none of %s: with a and c running every "+
					"cycle out, b and d each complete once in every four", seat, window)
			}
		}
		for _, seat := range []string{"a", "c"} {
			if !slices.Contains(started[from:from+round], seat) {
				t.Errorf("seat %s started none of %s: a seat with a backlog has a whole "+
					"cycle at least once in every 2k cycles", seat, window)
			}
		}
	}
}

// NO SEAT WAITS MORE THAN TWO CYCLES FOR EACH SEAT A CYCLE RUNS OUT AT.
//
// The bound the rule keeps ([memorySync.resume]), in the cases that show what
// it counts. Its k is every seat a cycle runs out at, a seat that fits a whole
// cycle but was reached with less of one left included: of three seats that
// each need two thirds of a cycle, none needs more than one, and yet the
// cycles run out at all three, each when a cycle reaches it second. And its
// 2k is the cycles between two that reach a seat, which a seat sorted after a
// backlog can wait in full: of a, b and c, where b needs a little over two
// cycles once, a completes and the first cycle runs out at b, b has the whole
// second, and c waits both, 2k for k = 1, before the third reaches it.
//
// A seat is reached when a cycle calls its publish with time left: one called
// on a bound already spent carries nothing.
//
// Mutation: start every cycle at the first sorted seat, and the seat after
// the one each cycle runs out at is never reached.
func TestNoSeatWaitsMoreThanTwoCyclesForEachSeatACycleRunsOutAt(t *testing.T) {
	t.Parallel()
	seats := []string{"a", "b", "c"}
	for _, tc := range []struct {
		name string
		// outlasts says the seat's publish runs its cycle out, as the
		// seat's n-th call, first saying the cycle called no seat before it.
		outlasts func(seat string, n int, first bool) bool
		// ranOutAt is the seats the case's cycles run out at, and waited the
		// most cycles in a row a seat goes unreached.
		ranOutAt []string
		waited   int
	}{{
		name:     "three seats that each fit a whole cycle but not what the one before leaves",
		outlasts: func(_ string, _ int, first bool) bool { return !first },
		ranOutAt: []string{"a", "b", "c"},
		waited:   1,
	}, {
		name:     "a backlog that holds the seat sorted after it back for two cycles",
		outlasts: func(seat string, n int, _ bool) bool { return seat == "b" && n <= 2 },
		ranOutAt: []string{"b"},
		waited:   2,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := map[string]int{}
			var ranOutAt []string
			m := &scriptedMemory{play: func(ctx context.Context, seat string, first bool,
				runOut func(),
			) (memsync.Carried, error) {
				calls[seat]++
				if !tc.outlasts(seat, calls[seat], first) {
					return carriedWhole, nil
				}
				if !slices.Contains(ranOutAt, seat) {
					ranOutAt = append(ranOutAt, seat)
				}
				return runsOut(ctx, runOut, draining)
			}}
			loop := &memorySync{}
			unreached, longest := map[string]int{}, map[string]int{}
			for range 9 {
				m.cycle(t, loop, seats...)
				for _, seat := range seats {
					if slices.Contains(m.calls, seat) && !slices.Contains(m.late, seat) {
						unreached[seat] = 0
						continue
					}
					unreached[seat]++
					longest[seat] = max(longest[seat], unreached[seat])
				}
			}
			slices.Sort(ranOutAt)
			k, waited := len(ranOutAt), 0
			for _, seat := range seats {
				if longest[seat] > 2*k {
					t.Errorf("seat %s went %d cycles in a row unreached, past 2k = %d for the %d "+
						"seats the cycles ran out at, %s", seat, longest[seat], 2*k, k,
						words.And(ranOutAt))
				}
				waited = max(waited, longest[seat])
			}
			if !slices.Equal(ranOutAt, tc.ranOutAt) || waited != tc.waited {
				t.Errorf("the cycles ran out at %s and a seat went at most %d in a row "+
					"unreached, want %s and %d", words.And(ranOutAt), waited,
					words.And(tc.ranOutAt), tc.waited)
			}
		})
	}
}

// A SEAT WHOSE PUBLISH FAILS WITH TIME LEFT HAS HAD ITS PUBLISH, AND THE ORDER
// HOLDS.
//
// A refusal or a broker error while the cycle still has time is that seat's
// failure, not the cycle's: the seats after it are carried in the same cycle,
// and the next cycle starts where it would have, at the first seat. Read as
// the bound running out, it left the seats after it for the next cycle, and
// started that at the seat that failed.
//
// Mutation: read any error as the bound running out, and c is not carried.
func TestASeatWhosePublishFailsWithTimeLeftKeepsTheOrder(t *testing.T) {
	t.Parallel()
	refused := errors.New("memsync: publish a agent_diary row for b: the broker refused the row")
	m := &scriptedMemory{play: func(_ context.Context, seat string, _ bool,
		_ func(),
	) (memsync.Carried, error) {
		if seat == "b" {
			return memsync.Carried{StoppedIn: "agent_diary"}, refused
		}
		return carriedWhole, nil
	}}
	loop := &memorySync{}

	report := m.cycle(t, loop, "a", "b", "c")
	if !slices.Equal(m.completed, []string{"a", "c"}) {
		t.Fatalf("the cycle in which b was refused carried %s, want a and c: the seats "+
			"after a refusal are carried in the same cycle", words.And(m.completed))
	}
	if loop.resume != "" {
		t.Errorf("the next cycle starts at %q, want the first seat: a refusal with time "+
			"left does not move where the cycles start", loop.resume)
	}
	if len(report.failed) != 1 || report.failed[0].handle != "b" ||
		!errors.Is(report.failed[0].err, refused) {
		t.Errorf("the cycle reported %d failed publishes, want b's refusal alone",
			len(report.failed))
	}
}

// A CYCLE THAT RAN OUT WITH NOTHING MARKED IS A WARNING.
//
// A cycle whose bound runs out says so in one line, at a level progress
// decides ([syncReport.lines]), naming the pass of the call it ran out in. A
// seat whose rows moved a mark before the cut is a backlog draining, which
// every seat's first cycles after an acquisition are, or a burst of its own,
// and is said at INFO. A seat that moved no mark warns, and so does a seat
// that had the whole cycle and was cut inside a whole-each-cycle table,
// whatever it marked; a seat that had only part of a cycle is given a whole
// one next. A seat whose own pass returned just as the bound ran out, before
// its backlog, is said at INFO, as a cycle that ran out between two seats'
// turns is. A seat refused with time left warns of its own, and the seats a
// cycle did not reach say nothing: its one line names them.
//
// Mutation: say every run-out at INFO, or every one as a warning, or warn for
// any cut inside a whole-each-cycle table whatever the seat's place, or leave
// the pass out of a line, and this goes red.
func TestACycleThatRanOutWithNothingMarkedIsAWarning(t *testing.T) {
	t.Parallel()
	refused := errors.New("the broker refused the row")
	cutAt := func(cut memsync.Carried) func(context.Context, func()) (memsync.Carried, error) {
		return func(ctx context.Context, runOut func()) (memsync.Carried, error) {
			return runsOut(ctx, runOut, cut)
		}
	}
	whole := memsync.Carried{Rows: 40, Marked: 12, StoppedIn: "synthesized_skills", Whole: true}
	for _, tc := range []struct {
		name string
		// seat is the seat the case scripts, play its own pass and backlog
		// its backlog, where it scripts one; every other call of a, b and c
		// reaches the end of its tables.
		seat          string
		play, backlog func(ctx context.Context, runOut func()) (memsync.Carried, error)
		want          []string
	}{{
		name: "the first seat cut with nothing acknowledged",
		seat: "a", play: cutAt(memsync.Carried{StoppedIn: "agent_diary"}),
		want: []string{"WARN seat_memory_not_carried seat=a pass=own next=b not_reached=b,c"},
	}, {
		name: "a later seat cut with only its whole tables acknowledged",
		seat: "b",
		play: cutAt(memsync.Carried{Rows: 4, StoppedIn: "counterparty_profiles", Whole: true}),
		want: []string{"WARN seat_memory_not_carried seat=b pass=own next=b not_reached=c"},
	}, {
		name: "the first seat cut after moving marks",
		seat: "a", play: cutAt(draining),
		want: []string{"INFO memory_sync_ran_out seat=a pass=own next=b not_reached=b,c"},
	}, {
		name: "a later seat cut after moving marks",
		seat: "b", play: cutAt(draining),
		want: []string{"INFO memory_sync_ran_out seat=b pass=own next=b not_reached=c"},
	}, {
		name: "the first seat cut inside a whole-each-cycle table after moving marks",
		seat: "a", play: cutAt(whole),
		want: []string{"WARN seat_memory_not_carried seat=a pass=own next=b not_reached=b,c"},
	}, {
		name: "a later seat cut inside a whole-each-cycle table after moving marks",
		seat: "b", play: cutAt(whole),
		want: []string{"INFO memory_sync_ran_out seat=b pass=own next=b not_reached=c"},
	}, {
		name: "the bound run out between a seat's own pass and its backlog",
		seat: "a",
		play: func(_ context.Context, runOut func()) (memsync.Carried, error) {
			runOut()
			return carriedWhole, nil
		},
		want: []string{"INFO memory_sync_ran_out seat=a pass=backlog next=b not_reached=b,c"},
	}, {
		name: "the bound run out between two seats' turns",
		seat: "a",
		backlog: func(_ context.Context, runOut func()) (memsync.Carried, error) {
			runOut()
			return memsync.Carried{}, nil
		},
		want: []string{"INFO memory_sync_ran_out pass=own next=b not_reached=b,c"},
	}, {
		name: "a seat refused with time left",
		seat: "b",
		play: func(context.Context, func()) (memsync.Carried, error) {
			return memsync.Carried{StoppedIn: "agent_diary"}, refused
		},
		want: []string{"WARN seat_memory_not_carried seat=b pass=own"},
	}, {
		name: "every seat carried",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &scriptedMemory{play: func(ctx context.Context, seat string, _ bool,
				runOut func(),
			) (memsync.Carried, error) {
				if seat == tc.seat && tc.play != nil {
					return tc.play(ctx, runOut)
				}
				return carriedWhole, nil
			}, playBacklog: func(ctx context.Context, seat string, runOut func()) (memsync.Carried, error) {
				if seat == tc.seat && tc.backlog != nil {
					return tc.backlog(ctx, runOut)
				}
				return memsync.Carried{}, nil
			}}
			report := m.cycle(t, &memorySync{}, "a", "b", "c")
			if got := said(report.lines()); !slices.Equal(got, tc.want) {
				t.Errorf("the cycle said %q, want %q", strings.Join(got, "; "),
					strings.Join(tc.want, "; "))
			}
		})
	}
}

// said is each line as its level, its message, the seats it names and the
// pass it is about.
func said(lines []reportLine) []string {
	var out []string
	for _, line := range lines {
		s := line.level.String() + " " + line.msg
		for _, key := range []string{"seat", "pass", "next", "not_reached"} {
			for i := 0; i+1 < len(line.args); i += 2 {
				if line.args[i] == key {
					s += fmt.Sprintf(" %s=%v", key, line.args[i+1])
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// THE NEXT CYCLE STARTS WHERE THE LAST RAN OUT, WHATEVER THE HOST HOLDS NOW.
//
// The held seats are read again every cycle, sorted, and rotated to start at
// the seat the last cycle ran out at. A seat released since is no longer
// there, and the cycle starts at the first seat after it, which the last cycle
// did not reach either; a seat taken since takes its sorted place.
//
// Mutation: read a resume the node no longer holds as the first seat, and the
// released case goes red.
func TestTheNextCycleStartsWhereTheLastRanOutWhateverTheHostHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		held   []string
		resume string
		want   []string
	}{
		{"no resume starts at the first seat", []string{"c", "a", "b"}, "", []string{"a", "b", "c"}},
		{"a held resume starts there", []string{"a", "b", "c"}, "b", []string{"b", "c", "a"}},
		{"the last seat wraps round", []string{"a", "b", "c"}, "c", []string{"c", "a", "b"}},
		{"a released resume starts at the seat after it", []string{"a", "c"}, "b",
			[]string{"c", "a"}},
		{"a resume after every seat starts at the first", []string{"a", "b", "c"}, "d",
			[]string{"a", "b", "c"}},
		{"a resume before every seat starts at the first", []string{"b", "c"}, "a",
			[]string{"b", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			held := slices.Clone(tc.held)
			if got := cycleOrder(held, tc.resume); !slices.Equal(got, tc.want) {
				t.Errorf("holding %s from %q, the cycle's order is %s, want %s",
					words.And(tc.held), tc.resume, words.And(got), words.And(tc.want))
			}
			if !slices.Equal(held, tc.held) {
				t.Errorf("the order rewrote the host's list to %s, want it read as %s",
					words.And(held), words.And(tc.held))
			}
		})
	}
}

// refusedRow is a call the broker refused while the bound still had time.
func refusedRow(context.Context, func()) (memsync.Carried, error) {
	return memsync.Carried{StoppedIn: "agent_diary"},
		errors.New("memsync: publish a agent_diary row for a: the broker refused the row")
}

// endsAsItReturns is a call that carried all it had just as the bound ran out.
func endsAsItReturns(_ context.Context, runOut func()) (memsync.Carried, error) {
	runOut()
	return carriedWhole, nil
}

// cutAfterMarks is a call the bound cut after its rows moved marks.
func cutAfterMarks(ctx context.Context, runOut func()) (memsync.Carried, error) {
	return runsOut(ctx, runOut, draining)
}

// A RELEASE'S FLUSH CARRIES THE SEAT'S OWN PASS BEFORE ITS BACKLOG.
//
// A release flushes what the cycles have not carried, as a cycle's turn does:
// what the seat wrote while held here and its tables sent whole, then, while
// the bound has time, the rows this node held for it when it took it. The
// flush used to be one publish, the backlog and the seat's own rows in one
// stamp order, so a release soon after an acquisition or a restart flushed old
// rows and lost the new (#1039). Each call is said on its own, at the level of
// what it lost: an own pass that returned an error warns, and so does a
// backlog that failed with time left; a backlog the bound cut, or that the
// bound left no time to begin, is said at INFO, the seat's own rows having
// gone.
//
// Mutation: carry the backlog first, skip it after an own pass refused with
// time left, call it on a bound already spent, warn for every backlog the
// bound cut, say an own pass the bound cut at INFO, or say one line for the
// whole flush, and this goes red.
func TestAReleaseFlushCarriesTheSeatsOwnPassBeforeItsBacklog(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		own, backlog  func(ctx context.Context, runOut func()) (memsync.Carried, error)
		order, saying []string
	}{{
		name:  "both calls complete",
		order: []string{"own:a", "backlog:a"},
	}, {
		name: "the own pass cut by the bound", own: cutAfterMarks,
		order:  []string{"own:a"},
		saying: []string{"WARN seat_memory_not_flushed seat=a pass=own"},
	}, {
		name: "the own pass complete as the bound runs out", own: endsAsItReturns,
		order:  []string{"own:a"},
		saying: []string{"INFO memory_flush_ran_out seat=a pass=backlog"},
	}, {
		name: "the own pass refused with time left, the backlog complete", own: refusedRow,
		order:  []string{"own:a", "backlog:a"},
		saying: []string{"WARN seat_memory_not_flushed seat=a pass=own"},
	}, {
		name: "the own pass refused with time left, the backlog cut by the bound",
		own:  refusedRow, backlog: cutAfterMarks,
		order: []string{"own:a", "backlog:a"},
		saying: []string{"WARN seat_memory_not_flushed seat=a pass=own",
			"INFO memory_flush_ran_out seat=a pass=backlog"},
	}, {
		name: "both calls refused with time left", own: refusedRow, backlog: refusedRow,
		order: []string{"own:a", "backlog:a"},
		saying: []string{"WARN seat_memory_not_flushed seat=a pass=own",
			"WARN seat_memory_not_flushed seat=a pass=backlog"},
	}, {
		name: "the backlog cut after moving marks", backlog: cutAfterMarks,
		order:  []string{"own:a", "backlog:a"},
		saying: []string{"INFO memory_flush_ran_out seat=a pass=backlog"},
	}, {
		name: "the backlog refused with time left", backlog: refusedRow,
		order:  []string{"own:a", "backlog:a"},
		saying: []string{"WARN seat_memory_not_flushed seat=a pass=backlog"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &scriptedMemory{play: func(ctx context.Context, _ string, _ bool,
				runOut func(),
			) (memsync.Carried, error) {
				if tc.own != nil {
					return tc.own(ctx, runOut)
				}
				return carriedWhole, nil
			}, playBacklog: func(ctx context.Context, _ string, runOut func()) (memsync.Carried, error) {
				if tc.backlog != nil {
					return tc.backlog(ctx, runOut)
				}
				return memsync.Carried{}, nil
			}}
			lines := m.flush(t, "a")
			if !slices.Equal(m.order, tc.order) {
				t.Errorf("the flush called %s, want %s: the seat's own pass, then its backlog "+
					"while the bound has time", words.And(m.order), words.And(tc.order))
			}
			if late := slices.Concat(m.late, m.backlogLate); len(late) > 0 {
				t.Errorf("the flush called %s on a bound already spent, want no call there",
					words.And(late))
			}
			if got := said(lines); !slices.Equal(got, tc.saying) {
				t.Errorf("the flush said %q, want %q", strings.Join(got, "; "),
					strings.Join(tc.saying, "; "))
			}
		})
	}
}

// A SEAT'S TURN REPORTS EACH OF ITS CALLS.
//
// A cycle gives each held seat a turn: its own pass, then, while the bound has
// time, its backlog. Each call is classified on its own, by the bound as it
// returned: one that failed with time left warns, whatever the other did, and
// the run-out is said of the call the bound ran out in, on what that call
// carried, at the level its progress decides. Merged into one, a turn whose
// own pass was refused with time left and whose backlog the bound then cut
// read as a backlog draining, at INFO, and said nothing of the refusal, for as
// long as the backlog lasted. An own pass that returns whole just as the bound
// runs out calls no backlog, and says so at INFO.
//
// Mutation: carry the backlog first, merge the turn's two calls into one
// report, drop an own pass's failure when its backlog fails too, call the
// backlog on a bound already spent, or warn of a turn cut between its calls,
// and this goes red.
func TestASeatsTurnReportsEachOfItsCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// own and backlog are seat a's calls; seat b's complete.
		own, backlog  func(ctx context.Context, runOut func()) (memsync.Carried, error)
		order, saying []string
		next          string
	}{{
		name:  "both of each seat's calls complete",
		order: []string{"own:a", "backlog:a", "own:b", "backlog:b"},
	}, {
		name: "a's own pass refused with time left, its backlog complete", own: refusedRow,
		order:  []string{"own:a", "backlog:a", "own:b", "backlog:b"},
		saying: []string{"WARN seat_memory_not_carried seat=a pass=own"},
	}, {
		name: "a's own pass refused with time left, its backlog cut after moving marks",
		own:  refusedRow, backlog: cutAfterMarks,
		order: []string{"own:a", "backlog:a"},
		saying: []string{"WARN seat_memory_not_carried seat=a pass=own",
			"INFO memory_sync_ran_out seat=a pass=backlog next=b not_reached=b"},
		next: "b",
	}, {
		name: "a's own pass complete, its backlog refused with time left", backlog: refusedRow,
		order:  []string{"own:a", "backlog:a", "own:b", "backlog:b"},
		saying: []string{"WARN seat_memory_not_carried seat=a pass=backlog"},
	}, {
		name: "both of a's calls refused with time left", own: refusedRow, backlog: refusedRow,
		order: []string{"own:a", "backlog:a", "own:b", "backlog:b"},
		saying: []string{"WARN seat_memory_not_carried seat=a pass=own",
			"WARN seat_memory_not_carried seat=a pass=backlog"},
	}, {
		name: "a's own pass complete as the bound runs out", own: endsAsItReturns,
		order:  []string{"own:a"},
		saying: []string{"INFO memory_sync_ran_out seat=a pass=backlog next=b not_reached=b"},
		next:   "b",
	}, {
		name: "a's own pass cut", own: cutAfterMarks,
		order:  []string{"own:a"},
		saying: []string{"INFO memory_sync_ran_out seat=a pass=own next=b not_reached=b"},
		next:   "b",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &scriptedMemory{play: func(ctx context.Context, seat string, _ bool,
				runOut func(),
			) (memsync.Carried, error) {
				if seat == "a" && tc.own != nil {
					return tc.own(ctx, runOut)
				}
				return carriedWhole, nil
			}, playBacklog: func(ctx context.Context, seat string, runOut func()) (memsync.Carried, error) {
				if seat == "a" && tc.backlog != nil {
					return tc.backlog(ctx, runOut)
				}
				return memsync.Carried{}, nil
			}}
			loop := &memorySync{}
			report := m.cycle(t, loop, "a", "b")
			if !slices.Equal(m.order, tc.order) {
				t.Errorf("the cycle called %s, want %s: each seat's own pass, then its backlog "+
					"while the bound has time", words.And(m.order), words.And(tc.order))
			}
			if late := slices.Concat(m.late, m.backlogLate); len(late) > 0 {
				t.Errorf("the cycle called %s on a bound already spent, want no call there",
					words.And(late))
			}
			if got := said(report.lines()); !slices.Equal(got, tc.saying) {
				t.Errorf("the cycle said %q, want %q", strings.Join(got, "; "),
					strings.Join(tc.saying, "; "))
			}
			if loop.resume != tc.next {
				t.Errorf("the next cycle starts at %q, want %q", loop.resume, tc.next)
			}
		})
	}
}
