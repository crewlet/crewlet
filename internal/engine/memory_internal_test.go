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
	"github.com/crewlet/crewlet/internal/org"
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

// scriptedMemory is the memory syncer as a cycle and a flush call it: each
// seat's own pass is played by a script, and so is its backlog, and it records
// the seats the running cycle or flush called, those it called on a bound
// already spent, and those whose call returned with nothing wrong, for each
// call, and every call in the order it was made.
type scriptedMemory struct {
	// play is seat's own pass, handed the bound and runOut, which spends it
	// as a seat the cycle cannot carry does before its call returns; first
	// says the cycle called nothing before it, own pass or backlog, so the
	// call has the whole cycle.
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
	first := len(m.order) == 0
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
	return m.cycleOver(t, loop, seatsHeld{held: held})
}

// cycleOver runs one cycle of loop over what held answers, under a bound the
// script can spend.
func (m *scriptedMemory) cycleOver(t *testing.T, loop *memorySync, held heldHandles) syncReport {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m.begin(cancel)
	return loop.cycle(ctx, held, m)
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

// nothingAcknowledged is what a call the bound cut before the changelog
// acknowledged any of its rows answers.
var nothingAcknowledged = memsync.Carried{StoppedIn: "agent_diary"}

// movedMarks is what a backlog that carried all it held answers: the rows the
// changelog acknowledged, each of which moved a mark.
var movedMarks = memsync.Carried{Rows: 3, Marked: 3}

// runsOut is the publish of a seat whose memory the cycle cannot carry: it
// spends the cycle's bound and answers what the bound cut it at.
func runsOut(ctx context.Context, runOut func(), cut memsync.Carried) (memsync.Carried, error) {
	runOut()
	return cut, fmt.Errorf("memsync: publish a %s row: %w", cut.StoppedIn, ctx.Err())
}

// completes is the own pass of a seat whose memory the cycle carries whole.
func completes(context.Context, string, bool, func()) (memsync.Carried, error) {
	return carriedWhole, nil
}

// heldReads is a seat host whose answer changes from one read to the next:
// each read answers the next of reads, and the last once they run out.
type heldReads struct {
	reads [][]string
	n     int
}

func (h *heldReads) Held() []string {
	read := h.reads[min(h.n, len(h.reads)-1)]
	h.n++
	return slices.Clone(read)
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
// The next cycle, or the next backlogs' turn, starts after the turn that had
// the whole cycle, wrapping round, and a lone turn is its own next: after the
// last of an order comes the first. A lone seat's own pass shares every cycle
// with the backlogs' turn, so one that runs every cycle out is called in
// every cycle, after the backlogs' turn in every other, and what that case
// holds is the lap. Within the backlogs' turn a lone seat's backlog is the
// first and the only call of every turn, so one that runs every turn out
// holds the wrap, without which the turn indexes past the end of its order
// to find the seat after it.
//
// Mutation: take the turn after it without wrapping round, and the backlog's
// case panics.
func TestALoneSeatThatRunsEveryCycleOutIsCarriedEveryCycle(t *testing.T) {
	t.Parallel()
	runsEveryCycleOut := func(ctx context.Context, runOut func()) (memsync.Carried, error) {
		return runsOut(ctx, runOut, draining)
	}
	for _, tc := range []struct {
		name string
		// own and backlog are the lone seat's calls, nil where it carries
		// all it holds.
		own, backlog func(ctx context.Context, runOut func()) (memsync.Carried, error)
		// called is what of a cycle's calls the case holds to want: the
		// seat's own passes, or every call in its order.
		called func(m *scriptedMemory) []string
		want   []string
	}{{
		name: "its own pass runs every cycle out", own: runsEveryCycleOut,
		called: func(m *scriptedMemory) []string { return m.calls },
		want:   []string{"a"},
	}, {
		name: "its backlog runs every cycle out", backlog: runsEveryCycleOut,
		called: func(m *scriptedMemory) []string { return m.order },
		want:   []string{"own:a", "backlog:a"},
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
				return movedMarks, nil
			}}
			loop := &memorySync{}
			for cycle := 1; cycle <= 3; cycle++ {
				m.cycle(t, loop, "a")
				if got := tc.called(m); !slices.Equal(got, tc.want) {
					t.Fatalf("cycle %d called %s, want %s", cycle, words.And(got),
						words.And(tc.want))
				}
			}
		})
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

// NO SEAT WAITS MORE THAN TWO CYCLES FOR EACH TURN A CYCLE RUNS OUT IN.
//
// The bound the rule keeps ([memorySync.resume]), in the cases that show what
// it counts. Its k is every seat whose own pass a cycle runs out in, a seat
// that fits a whole cycle but was reached with less of one left included: of
// three seats that each need two thirds of a cycle, none needs more than one,
// and yet the cycles run out at all three, each when a cycle reaches it
// second. Its 2k is the cycles between two that reach a seat, which a seat
// sorted after a backlog can wait in full: of a, b and c, where b needs a
// little over two cycles once, a completes and the first cycle runs out at b,
// b has the whole second, and c waits both, 2k for k = 1, before the third
// reaches it. And the backlogs' turn is one more turn a cycle can run out in,
// which holds the turns after it back two more cycles: two for each turn the
// cycles run out in, the backlogs' among them, is 2k + 2 for k seats. Of a, b,
// c and d, in cycles from b's turn, where c's own pass runs out twice and the
// backlogs' turn moves nothing in two, a waits four, 2k + 2 for k = 1.
//
// A seat is reached when a cycle calls its own pass with time left: one
// called on a bound already spent carries nothing.
//
// Mutation: start every cycle at the first sorted seat, and the seat after
// the one each cycle runs out at is never reached; start the cycle after a
// backlogs' turn that moved nothing at the first seat whatever its place,
// and a waits fewer cycles than the rule lets it.
func TestNoSeatWaitsMoreThanTwoCyclesForEachTurnACycleRunsOutIn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		seats []string
		// from is the turn the first cycle starts at.
		from string
		// outlasts says the seat's own pass runs its cycle out, as the
		// seat's n-th call, first saying the cycle called nothing before it;
		// backlogsOutlast says the backlogs' turn does, moving nothing, as
		// that turn's n-th, nil for one that carries all it holds.
		outlasts        func(seat string, n int, first bool) bool
		backlogsOutlast func(n int) bool
		// ranOutAt is the turns the case's cycles run out in, and waited the
		// most cycles in a row a seat goes unreached.
		ranOutAt []string
		waited   int
	}{{
		name:     "three seats that each fit a whole cycle but not what the one before leaves",
		seats:    []string{"a", "b", "c"},
		outlasts: func(_ string, _ int, first bool) bool { return !first },
		ranOutAt: []string{"a", "b", "c"},
		waited:   1,
	}, {
		name:     "a backlog that holds the seat sorted after it back for two cycles",
		seats:    []string{"a", "b", "c"},
		outlasts: func(seat string, n int, _ bool) bool { return seat == "b" && n <= 2 },
		ranOutAt: []string{"b"},
		waited:   2,
	}, {
		name:            "a seat and the backlogs' turn that each hold the seats after them back",
		seats:           []string{"a", "b", "c", "d"},
		from:            "b",
		outlasts:        func(seat string, n int, _ bool) bool { return seat == "c" && n <= 2 },
		backlogsOutlast: func(n int) bool { return n <= 2 },
		ranOutAt:        []string{backlogsTurn, "c"},
		waited:          4,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := map[string]int{}
			var ranOutAt []string
			ranOut := func(turn string) {
				if !slices.Contains(ranOutAt, turn) {
					ranOutAt = append(ranOutAt, turn)
				}
			}
			var m *scriptedMemory
			reached := 0
			m = &scriptedMemory{play: func(ctx context.Context, seat string, first bool,
				runOut func(),
			) (memsync.Carried, error) {
				calls[seat]++
				if !tc.outlasts(seat, calls[seat], first) {
					return carriedWhole, nil
				}
				ranOut(seat)
				return runsOut(ctx, runOut, draining)
			}, playBacklog: func(ctx context.Context, _ string, runOut func()) (memsync.Carried, error) {
				if len(m.backlog) == 1 {
					reached++
				}
				if tc.backlogsOutlast == nil || !tc.backlogsOutlast(reached) {
					return movedMarks, nil
				}
				ranOut(backlogsTurn)
				return runsOut(ctx, runOut, nothingAcknowledged)
			}}
			loop := &memorySync{resume: tc.from}
			unreached, longest := map[string]int{}, map[string]int{}
			for range 9 {
				m.cycle(t, loop, tc.seats...)
				for _, seat := range tc.seats {
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
			for _, seat := range tc.seats {
				if longest[seat] > 2*k {
					t.Errorf("seat %s went %d cycles in a row unreached, past 2k = %d for the %d "+
						"turns the cycles ran out in, %s", seat, longest[seat], 2*k, k,
						words.And(ranOutAt))
				}
				waited = max(waited, longest[seat])
			}
			if !slices.Equal(ranOutAt, tc.ranOutAt) || waited != tc.waited {
				t.Errorf("the cycles ran out in %s and a seat went at most %d in a row "+
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
// decides ([syncReport.lines]), naming the pass of the call it ran out in,
// where the next cycle starts and the turns it did not reach, the backlogs'
// turn among them, and, when it ran out in the backlogs' turn, where that
// turn starts next and the seats whose backlog it did not reach. A call whose
// rows moved a mark before the cut is a backlog draining, which every seat's
// first cycles after an acquisition are, or a burst of the seat's own, and is
// said at INFO. A call that moved no mark warns, and so does a seat that had
// the whole cycle and was cut inside a whole-each-cycle table, whatever it
// marked; a seat that had only part of a cycle is given a whole one next. A
// cycle that ran out between two calls, the one before it whole, is said at
// INFO. A call refused with time left warns of its own, a seat's own pass and
// its backlog alike, and the turns a cycle did not reach say nothing: its one
// line names them. The next cycle starts at the first seat after a backlogs'
// turn that moved a mark, wherever the call it was cut in stood.
//
// Mutation: say every run-out at INFO, or every one as a warning, or every
// backlog's cut as a warning, or warn for any cut inside a whole-each-cycle
// table whatever the seat's place, or leave the pass, the backlogs' turn or
// where it starts next out of a line, or judge a cut backlogs' turn's marks
// by its last call alone, or call a backlog on a bound already spent, and
// this goes red.
func TestACycleThatRanOutWithNothingMarkedIsAWarning(t *testing.T) {
	t.Parallel()
	type script = func(ctx context.Context, runOut func()) (memsync.Carried, error)
	cutAt := func(cut memsync.Carried) script {
		return func(ctx context.Context, runOut func()) (memsync.Carried, error) {
			return runsOut(ctx, runOut, cut)
		}
	}
	returnsAsItRunsOut := func(carried memsync.Carried) script {
		return func(_ context.Context, runOut func()) (memsync.Carried, error) {
			runOut()
			return carried, nil
		}
	}
	movedAll := func(context.Context, func()) (memsync.Carried, error) { return movedMarks, nil }
	whole := memsync.Carried{Rows: 40, Marked: 12, StoppedIn: "synthesized_skills", Whole: true}
	for _, tc := range []struct {
		name string
		// own and backlog script a seat's own pass and its backlog by its
		// handle; every other call of a, b and c carries all it holds.
		own, backlog map[string]script
		want         []string
	}{{
		name: "the first seat cut with nothing acknowledged",
		own:  map[string]script{"a": cutAt(nothingAcknowledged)},
		want: []string{"WARN seat_memory_not_carried seat=a pass=own next=b not_reached=b,c,(backlogs)"},
	}, {
		name: "a later seat cut with only its whole tables acknowledged",
		own: map[string]script{"b": cutAt(memsync.Carried{Rows: 4,
			StoppedIn: "counterparty_profiles", Whole: true})},
		want: []string{"WARN seat_memory_not_carried seat=b pass=own next=b not_reached=c,(backlogs)"},
	}, {
		name: "the first seat cut after moving marks",
		own:  map[string]script{"a": cutAt(draining)},
		want: []string{"INFO memory_sync_ran_out seat=a pass=own next=b not_reached=b,c,(backlogs)"},
	}, {
		name: "a later seat cut after moving marks",
		own:  map[string]script{"b": cutAt(draining)},
		want: []string{"INFO memory_sync_ran_out seat=b pass=own next=b not_reached=c,(backlogs)"},
	}, {
		name: "the first seat cut inside a whole-each-cycle table after moving marks",
		own:  map[string]script{"a": cutAt(whole)},
		want: []string{"WARN seat_memory_not_carried seat=a pass=own next=b not_reached=b,c,(backlogs)"},
	}, {
		name: "a later seat cut inside a whole-each-cycle table after moving marks",
		own:  map[string]script{"b": cutAt(whole)},
		want: []string{"INFO memory_sync_ran_out seat=b pass=own next=b not_reached=c,(backlogs)"},
	}, {
		name: "the bound run out between two seats' own passes",
		own:  map[string]script{"a": returnsAsItRunsOut(carriedWhole)},
		want: []string{"INFO memory_sync_ran_out pass=own next=b not_reached=b,c,(backlogs)"},
	}, {
		name: "the bound run out just before the backlogs' turn",
		own:  map[string]script{"c": returnsAsItRunsOut(carriedWhole)},
		want: []string{"INFO memory_sync_ran_out pass=backlog next=(backlogs) not_reached=(backlogs)"},
	}, {
		name:    "the backlogs' first seat cut after moving marks",
		backlog: map[string]script{"a": cutAt(draining)},
		want: []string{"INFO memory_sync_ran_out seat=a pass=backlog next= not_reached= " +
			"backlogs_next=b backlogs_not_reached=b,c"},
	}, {
		name:    "a later seat's backlog cut after moving marks",
		backlog: map[string]script{"b": cutAt(draining)},
		want: []string{"INFO memory_sync_ran_out seat=b pass=backlog next= not_reached= " +
			"backlogs_next=b backlogs_not_reached=c"},
	}, {
		name:    "a seat's backlog cut with nothing acknowledged",
		backlog: map[string]script{"a": cutAt(nothingAcknowledged)},
		want: []string{"WARN seat_memory_not_carried seat=a pass=backlog next=(backlogs) " +
			"not_reached= backlogs_next=b backlogs_not_reached=b,c"},
	}, {
		name:    "a seat's backlog cut with nothing acknowledged after another's moved marks",
		backlog: map[string]script{"a": movedAll, "b": cutAt(nothingAcknowledged)},
		want: []string{"WARN seat_memory_not_carried seat=b pass=backlog next= not_reached= " +
			"backlogs_next=b backlogs_not_reached=c"},
	}, {
		name:    "the bound run out between two seats' backlogs",
		backlog: map[string]script{"a": returnsAsItRunsOut(memsync.Carried{})},
		want: []string{"INFO memory_sync_ran_out pass=backlog next=(backlogs) not_reached= " +
			"backlogs_next=b backlogs_not_reached=b,c"},
	}, {
		name:    "the bound run out between two seats' backlogs after one moved marks",
		backlog: map[string]script{"a": returnsAsItRunsOut(movedMarks)},
		want: []string{"INFO memory_sync_ran_out pass=backlog next= not_reached= " +
			"backlogs_next=b backlogs_not_reached=b,c"},
	}, {
		name: "a seat's own pass refused with time left",
		own:  map[string]script{"b": refusedRow},
		want: []string{"WARN seat_memory_not_carried seat=b pass=own"},
	}, {
		name:    "a seat's backlog refused with time left",
		backlog: map[string]script{"b": refusedRow},
		want:    []string{"WARN seat_memory_not_carried seat=b pass=backlog"},
	}, {
		name:    "a seat's own pass refused with time left and its backlog cut after moving marks",
		own:     map[string]script{"a": refusedRow},
		backlog: map[string]script{"a": cutAt(draining)},
		want: []string{"WARN seat_memory_not_carried seat=a pass=own",
			"INFO memory_sync_ran_out seat=a pass=backlog next= not_reached= " +
				"backlogs_next=b backlogs_not_reached=b,c"},
	}, {
		name: "every seat carried",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &scriptedMemory{play: func(ctx context.Context, seat string, _ bool,
				runOut func(),
			) (memsync.Carried, error) {
				if play, ok := tc.own[seat]; ok {
					return play(ctx, runOut)
				}
				return carriedWhole, nil
			}, playBacklog: func(ctx context.Context, seat string, runOut func()) (memsync.Carried, error) {
				if play, ok := tc.backlog[seat]; ok {
					return play(ctx, runOut)
				}
				return memsync.Carried{}, nil
			}}
			report := m.cycle(t, &memorySync{}, "a", "b", "c")
			if got := said(report.lines()); !slices.Equal(got, tc.want) {
				t.Errorf("the cycle said %q, want %q", strings.Join(got, "; "),
					strings.Join(tc.want, "; "))
			}
			if late := slices.Concat(m.late, m.backlogLate); len(late) > 0 {
				t.Errorf("the cycle called %s on a bound already spent, want no call there",
					words.And(late))
			}
		})
	}
}

// said is each line as its level, its message, the seats it names, the pass
// it is about, and where it says the cycles go on.
func said(lines []reportLine) []string {
	var out []string
	for _, line := range lines {
		s := line.level.String() + " " + line.msg
		for _, key := range []string{"seat", "pass", "next", "not_reached", "backlogs_next",
			"backlogs_not_reached"} {
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

// THE NEXT BACKLOGS' TURN STARTS WHERE THE LAST RAN OUT, WHATEVER THE HOST
// HOLDS NOW.
//
// The held seats are read again as every backlogs' turn begins, sorted, and
// rotated to start at the seat the last turn ran out at. A seat released
// since is no longer there, and the turn starts at the first seat after it,
// which the last turn did not reach either; a seat taken since takes its
// sorted place.
//
// Mutation: read a resume the node no longer holds as the first seat, and the
// released case goes red.
func TestTheNextBacklogsTurnStartsWhereTheLastRanOutWhateverTheHostHolds(t *testing.T) {
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
// A release flushes what the cycles have not carried, in a cycle's order:
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

// A CYCLE TAKES THE BACKLOGS' TURN AFTER THE LAST SEAT'S, WHEREVER IT STARTS.
//
// A cycle's turns are the held seats', sorted, and the backlogs' turn after
// the last of them, rotated to start where the last cycle ran out. A seat
// released since starts the cycle at the turn after its own, which the last
// cycle did not reach either, and for a seat that sorted after every one
// still held that is the backlogs' turn. No handle can name the backlogs'
// turn, so no seat is ever taken for it.
//
// Mutation: put the backlogs' turn before the seats', start the cycle after a
// released last seat at the first seat, or name the backlogs' turn as a seat
// could be named, and this goes red.
func TestACycleTakesTheBacklogsTurnAfterTheLastSeatsWhereverItStarts(t *testing.T) {
	t.Parallel()
	if org.ValidHandle(backlogsTurn) {
		t.Fatalf("the backlogs' turn is named %q, which a seat's handle can be: a seat so "+
			"named would be taken for that turn", backlogsTurn)
	}
	for _, tc := range []struct {
		name   string
		held   []string
		resume string
		want   []string
	}{
		{"no resume starts at the first seat", []string{"c", "a", "b"}, "",
			[]string{"a", "b", "c", backlogsTurn}},
		{"a held resume starts there", []string{"a", "b", "c"}, "b",
			[]string{"b", "c", backlogsTurn, "a"}},
		{"the backlogs' turn starts there", []string{"a", "b", "c"}, backlogsTurn,
			[]string{backlogsTurn, "a", "b", "c"}},
		{"a released resume starts at the turn after it", []string{"a", "c"}, "b",
			[]string{"c", backlogsTurn, "a"}},
		{"a released resume after every seat starts at the backlogs' turn",
			[]string{"a", "b", "c"}, "d", []string{backlogsTurn, "a", "b", "c"}},
		{"a resume before every seat starts at the first", []string{"b", "c"}, "a",
			[]string{"b", "c", backlogsTurn}},
		{"no seat held leaves the backlogs' turn alone", nil, "b", []string{backlogsTurn}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			held := slices.Clone(tc.held)
			if got := lapOrder(held, tc.resume); !slices.Equal(got, tc.want) {
				t.Errorf("holding %s from %q, the cycle's turns are %s, want %s",
					words.And(tc.held), tc.resume, words.And(got), words.And(tc.want))
			}
			if !slices.Equal(held, tc.held) {
				t.Errorf("the order rewrote the host's list to %s, want it read as %s",
					words.And(held), words.And(tc.held))
			}
		})
	}
}

// EVERY HELD SEAT'S OWN PASS COMES BEFORE ANY SEAT'S BACKLOG.
//
// A cycle carries what each seat it holds wrote while held here, and its
// tables sent whole, before the rows this node held for any seat when it took
// it: a turn for each seat, its own pass, in sorted order, and then the
// backlogs' turn, every seat's backlog. Each seat's own pass and its backlog
// in one turn, seat after seat, left what a seat wrote behind the backlogs of
// the seats sorted before it, and a crash before those had drained lost it
// (#1039).
//
// Mutation: take the backlogs' turn before the seats', carry a seat's backlog
// in its own turn, or carry the first seat's own pass alone, and this goes
// red.
func TestEveryHeldSeatsOwnPassComesBeforeAnySeatsBacklog(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: completes}
	m.cycle(t, &memorySync{}, "c", "a", "b")
	want := []string{"own:a", "own:b", "own:c", "backlog:a", "backlog:b", "backlog:c"}
	if !slices.Equal(m.order, want) {
		t.Errorf("the cycle called %s, want %s: every held seat's own pass, in sorted "+
			"order, before any seat's backlog", words.And(m.order), words.And(want))
	}
}

// THE BACKLOGS HAVE THEIR TURN WHILE A SEAT'S OWN PASS RUNS EVERY CYCLE OUT.
//
// A seat's own pass can outlast a cycle for as long as the seat keeps
// writing (#1041), as long as its profiles and skills, which every own pass
// sends whole, outlast one (#1040), or while it is still being taken, every
// row of it its own until its hydration completes. The backlogs' turn is a
// turn of the rotation, so the cycles that pass the seat reach it, every
// seat's backlog with it, that seat's own included. Begun only once every
// seat's own pass had returned, the backlogs would have no cycle while one
// seat's own pass ran every cycle out; and each seat's own pass and its
// backlog in one turn never reached that seat's backlog.
//
// Mutation: take the backlogs' turn only after every seat's own pass, or call
// a seat's backlog on a bound already spent, and this goes red.
func TestTheBacklogsHaveTheirTurnWhileASeatsOwnPassRunsEveryCycleOut(t *testing.T) {
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
	alone := []string{"own:a"}
	passed := []string{"own:b", "own:c", "backlog:a", "backlog:b", "backlog:c", "own:a"}
	for cycle, want := range [][]string{alone, passed, alone, passed} {
		m.cycle(t, loop, "a", "b", "c")
		if !slices.Equal(m.order, want) {
			t.Fatalf("cycle %d called %s, want %s: the cycle that passes a seat whose own "+
				"pass runs every cycle out reaches the backlogs' turn", cycle+1,
				words.And(m.order), words.And(want))
		}
		if late := slices.Concat(m.late, m.backlogLate); len(late) > 0 {
			t.Fatalf("cycle %d called %s on a bound already spent, want no call there",
				cycle+1, words.And(late))
		}
	}
}

// THE BACKLOGS TAKE TURNS IN WHAT THE OWN PASSES LEAVE.
//
// The backlogs' turn carries the held seats' backlogs by the rotation's rule,
// from where its own last turn ran out, apart from where the cycles start: a
// backlog longer than what the seats' own passes leave of a cycle drains over
// as many cycles as it needs, the backlogs after it are carried meanwhile,
// and every cycle still starts with the seats' own passes.
//
// Mutation: start every backlogs' turn at the first seat, or where the cycle
// starts, or start the cycle where the backlogs' turn does, and this goes
// red.
func TestTheBacklogsTakeTurnsInWhatTheOwnPassesLeave(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: completes, playBacklog: func(ctx context.Context, seat string,
		runOut func(),
	) (memsync.Carried, error) {
		if seat == "a" {
			return runsOut(ctx, runOut, draining)
		}
		return movedMarks, nil
	}}
	const cycles, round = 6, 2 // round is 2k, with k = 1 backlog that runs the backlogs' turn out
	loop := &memorySync{}
	started, done := make([]string, cycles), make([][]string, cycles)
	own := []string{"own:a", "own:b", "own:c"}
	for i := range cycles {
		m.cycle(t, loop, "a", "b", "c")
		if len(m.order) <= len(own) || !slices.Equal(m.order[:len(own)], own) {
			t.Fatalf("cycle %d called %s, want every seat's own pass first, %s", i+1,
				words.And(m.order), words.And(own))
		}
		started[i], done[i] = m.backlog[0], m.backlogDone
	}
	for from := 0; from+round <= cycles; from++ {
		window := fmt.Sprintf("cycles %d to %d", from+1, from+round)
		for _, seat := range []string{"b", "c"} {
			if !slices.ContainsFunc(done[from:from+round], func(seats []string) bool {
				return slices.Contains(seats, seat)
			}) {
				t.Errorf("seat %s's backlog completed in none of %s: with a's running the "+
					"backlogs' turn out, b's and c's each complete once in every two", seat,
					window)
			}
		}
		if !slices.Contains(started[from:from+round], "a") {
			t.Errorf("a's backlog started the backlogs' turn in none of %s: a backlog longer "+
				"than the turn has its whole share at least once in every 2k turns", window)
		}
	}
}

// THE BACKLOGS' TURN READS THE SEATS HELD AS IT BEGINS.
//
// A cycle's lap reads the seats held as the cycle begins, and its backlogs'
// turn reads them again as that turn begins. A seat released while the seats'
// own passes ran is not called again, its release having carried what it
// held, and a seat taken meanwhile has its backlog called: its boundary is set
// if its hydration has completed, and otherwise its backlog carries nothing.
//
// Mutation: carry the backlogs of the seats the lap began with, and this goes
// red.
func TestTheBacklogsTurnReadsTheSeatsHeldAsItBegins(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: completes}
	m.cycleOver(t, &memorySync{}, &heldReads{reads: [][]string{{"a", "b", "c"}, {"a", "c", "d"}}})
	want := []string{"own:a", "own:b", "own:c", "backlog:a", "backlog:c", "backlog:d"}
	if !slices.Equal(m.order, want) {
		t.Errorf("the cycle called %s, want %s: b released and d taken while the own passes "+
			"ran", words.And(m.order), words.And(want))
	}
}

// A SEAT THAT RAN A CYCLE OUT BEHIND ANOTHER GOES FIRST NEXT, THOUGH THE
// BACKLOGS' TURN WAS NOT REACHED.
//
// A seat whose own pass fits a whole cycle, but not what the turns before it
// leave of one, runs a cycle out when it comes second, and the next cycle
// starts at it, so that it completes. The backlogs' turn after it, which that
// cycle did not reach, waits its place in the rotation: started at the
// backlogs' turn instead, the next cycle puts the seat behind it and behind
// the seat sorted before it, second again, and the seat never completes.
//
// Mutation: start the next cycle at the backlogs' turn whenever a cycle did
// not reach it, and b never completes.
func TestASeatThatRanOutBehindAnotherGoesFirstNextThoughTheBacklogsWereNotReached(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: func(ctx context.Context, _ string, first bool,
		runOut func(),
	) (memsync.Carried, error) {
		if first {
			return carriedWhole, nil
		}
		return runsOut(ctx, runOut, draining)
	}, playBacklog: func(context.Context, string, func()) (memsync.Carried, error) {
		return movedMarks, nil
	}}
	const cycles = 6
	loop := &memorySync{}
	completed := make([][]string, cycles)
	for i := range cycles {
		m.cycle(t, loop, "a", "b")
		completed[i] = m.completed
	}
	for from := 0; from+2 <= cycles; from++ {
		if !slices.ContainsFunc(completed[from:from+2], func(done []string) bool {
			return slices.Contains(done, "b")
		}) {
			t.Errorf("b completed its own pass in neither of cycles %d and %d: a seat that "+
				"fits a whole cycle but not what a turn before it leaves goes first after "+
				"it runs one out", from+1, from+2)
		}
	}
}

// A BACKLOGS' TURN THAT HAD A WHOLE CYCLE AND MOVED NOTHING HANDS THE NEXT
// CYCLE TO THE SEATS.
//
// A backlogs' turn that a cycle reached late and cut before the changelog
// acknowledged anything of it starts the next cycle, so that it has a whole
// one: a backlog's next row can need more of a cycle than the seats' own
// passes leave. Cut so with the whole cycle, it moved nothing for want of
// something time does not give (a backlog's next row no cycle can carry, a
// broker that acknowledges nothing), and the next cycle starts at the turn
// after it, the first seat's. So the cycles alternate: one carries every
// seat's own pass and reaches the backlogs late, the next is the backlogs'
// alone. Started at the backlogs' turn again, no cycle would carry a seat's
// own pass again; never started there, the backlogs would never have a whole
// cycle.
//
// Mutation: start the next cycle at a backlogs' turn that moved nothing
// whether or not it had the whole cycle, or never there, and this goes red.
func TestABacklogsTurnThatHadAWholeCycleAndMovedNothingHandsTheNextCycleToTheSeats(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: completes, playBacklog: func(ctx context.Context, _ string,
		runOut func(),
	) (memsync.Carried, error) {
		return runsOut(ctx, runOut, nothingAcknowledged)
	}}
	loop := &memorySync{}
	for cycle, want := range [][]string{
		{"own:a", "own:b", "own:c", "backlog:a"},
		{"backlog:b"},
		{"own:a", "own:b", "own:c", "backlog:c"},
		{"backlog:a"},
	} {
		m.cycle(t, loop, "a", "b", "c")
		if !slices.Equal(m.order, want) {
			t.Fatalf("cycle %d called %s, want %s: a backlogs' turn that moved nothing goes "+
				"first in the next cycle when it came late, and hands the one after to the "+
				"seats when it had the whole cycle", cycle+1, words.And(m.order),
				words.And(want))
		}
	}
}

// A BACKLOGS' TURN CUT AFTER MOVING MARKS HANDS THE NEXT CYCLE TO THE SEATS.
//
// A backlog longer than what the seats' own passes leave of a cycle, such as
// the whole store a restart leaves a data node to carry back for each seat it
// held (#1039's second case), is cut cycle after cycle while it drains.
// Moving marks, it needs no whole cycle, so the next cycle starts at the
// first seat rather than at the backlogs' turn, and every cycle carries every
// seat's own rows before any backlog. Started at the backlogs' turn, as the
// rotation's rule alone would start it, every other cycle would carry the
// backlogs first, and what a seat wrote meanwhile would wait behind them.
//
// Mutation: drop the exception, and this goes red.
func TestABacklogsTurnCutAfterMovingMarksHandsTheNextCycleToTheSeats(t *testing.T) {
	t.Parallel()
	m := &scriptedMemory{play: completes, playBacklog: func(ctx context.Context, _ string,
		runOut func(),
	) (memsync.Carried, error) {
		return runsOut(ctx, runOut, draining)
	}}
	loop := &memorySync{}
	for cycle, seat := range []string{"a", "b", "c", "a"} {
		m.cycle(t, loop, "a", "b", "c")
		want := []string{"own:a", "own:b", "own:c", "backlog:" + seat}
		if !slices.Equal(m.order, want) {
			t.Fatalf("cycle %d called %s, want %s: every seat's own pass before the backlogs "+
				"that the cycle before cut after they moved marks", cycle+1,
				words.And(m.order), words.And(want))
		}
		if loop.resume != "" {
			t.Fatalf("after cycle %d the next starts at %q, want the first seat", cycle+1,
				loop.resume)
		}
	}
}
