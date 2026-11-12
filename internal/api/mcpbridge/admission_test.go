package mcpbridge_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/tools"
)

// When a call is counted in its run's call log, and what becomes of an update
// the ledger could not land (the package doc's "A call is on its run's record
// before it runs").

// ordinalTool moves an item to the state its call names, recording, as each
// call of it runs, the ordinal its run's call log hands the call: the repeat
// count every operation id the call derives carries. A call to the state
// "broken" could not run at all, as a call whose turn was torn down under it.
type ordinalTool struct {
	mu       sync.Mutex
	ordinals map[string]int
}

func (*ordinalTool) Name() string        { return "move_item" }
func (*ordinalTool) Description() string { return "moves an item to a state" }
func (*ordinalTool) Parameters() map[string]any {
	return map[string]any{"type": "object",
		"properties": map[string]any{"state": map[string]any{"type": "string"}}}
}

func (*ordinalTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Output: "no turn"}, nil
}

func (m *ordinalTool) CallForTurn(_ context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	state, _ := args["state"].(string)
	if state == "broken" {
		return tools.Result{}, fmt.Errorf("toolloop: %w", context.Canceled)
	}
	n := turn.CallLog().Ordinal("move_item", args)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ordinals == nil {
		m.ordinals = map[string]int{}
	}
	m.ordinals[state] = n
	return tools.Result{Output: "moved to " + state}, nil
}

// derived is the ordinal each call that ran derived, by the state it named.
func (m *ordinalTool) derived() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return maps.Clone(m.ordinals)
}

// firstBeginRefused refuses the entry of the session's first call, as a
// coordination store in an outage refuses every attempt within the ledger's
// bound, and takes every later one.
type firstBeginRefused struct {
	*ledger
	begins atomic.Int32
}

func (l *firstBeginRefused) Begin(ctx context.Context, runID string, b mcpbridge.Begun) error {
	if l.begins.Add(1) == 1 {
		return errors.New("the coordination store is electing a leader")
	}
	return l.ledger.Begin(ctx, runID, b)
}

// inFlightOf is every entry l took that no withdrawal took back and no update
// finished: a call in flight, as the run's log holds it.
func inFlightOf(l *ledger) []mcpbridge.Begun {
	withdrawn, rows := l.withdrawals(), l.rows()
	return slices.DeleteFunc(l.entries(), func(b mcpbridge.Begun) bool {
		return slices.ContainsFunc(withdrawn, func(w mcpbridge.Begun) bool {
			return w.Seq == b.Seq && w.StartedAt.Equal(b.StartedAt)
		}) || slices.ContainsFunc(rows, func(c mcpbridge.Call) bool {
			return c.Seq == b.Seq && c.StartedAt.Equal(b.StartedAt)
		})
	})
}

// seededFrom is the call log a later segment of the run seeds from what the
// run's ledger holds alone — every call it lists and every entry still in
// flight, by the identity each carries, and a listed call that carries none by
// its canonical arguments — as internal/agent/runner's seedCalls seeds a
// resumed segment's.
func seededFrom(l *ledger, surface *tools.Surface) *turnctx.CallLog {
	seeded := turnctx.NewCallLog()
	for _, row := range l.rows() {
		if row.Digest == "" {
			seeded.Seed(turnctx.SeededCall{Tool: row.Name, Args: surface.ArgsOf(row.Name, row.Args)})
			continue
		}
		seeded.Seed(turnctx.SeededCall{Tool: row.Name, Digest: row.Digest})
	}
	for _, entry := range inFlightOf(l) {
		seeded.Seed(turnctx.SeededCall{Tool: entry.Name, Digest: entry.Digest, Unknown: entry.Digest == ""})
	}
	return seeded
}

// moveFrom makes a call moving the item to a state from a box, and hands back
// its answer once the call returns.
type moveFrom func(state string) *mcp.CallToolResult

// THE RUN'S CALL LOG COUNTS ONLY THE CALLS ITS RECORD HOLDS. A write's operation
// id carries how many earlier calls of its tool in the run asked for something
// else ([turnctx.CallLog]), and a later segment of the run — a collection, a
// continuation, a node that took the seat over — seeds its own log from the
// run's record alone. So the log of the segment that makes the calls must count
// exactly the calls that record holds, by their identity: a call it counted and
// no record holds makes the resumed segment derive, for a new call, the
// operation id a call of this segment already used, and the new call is taken
// for the old one's retry — the item moved to done and back stays done. A call
// refused at its entry, or whose opening ended while its entry was written, is
// counted nowhere, its entry withdrawn; one the log counted that its tool then
// could not run is on the record as a call not run, by the identity the log
// counted it under.
//
// Mutation: admit a call refused at its entry all the same
// (refused-call-admitted), and this segment counts a call no record holds.
func TestTheRunsCallLogCountsOnlyTheCallsItsRecordHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// calls makes the case's calls from box, on b's session — move is
		// one of them, made and answered — and checks each answer.
		calls func(t *testing.T, b *bubbled, box *mcp.ClientSession, move moveFrom)
		// led is the ledger the session writes through, over l.
		led func(l *ledger) mcpbridge.Ledger
		// held holds every entry as it is written, until a case lets it go.
		held bool
	}{
		{name: "a call the store refused at its entry",
			led: func(l *ledger) mcpbridge.Ledger { return &firstBeginRefused{ledger: l} },
			calls: func(t *testing.T, _ *bubbled, _ *mcp.ClientSession, move moveFrom) {
				if res := move("done"); !res.IsError || text(res) != mcpbridge.UnrecordedNote {
					t.Fatalf("the call whose entry was refused was answered %v %q, want it refused as one that "+
						"could not be recorded", res.IsError, text(res))
				}
				if res := move("in_progress"); res.IsError {
					t.Fatalf("the next call failed: %q", text(res))
				}
			}},
		{name: "a call whose opening ended while its entry was written", held: true,
			calls: func(t *testing.T, b *bubbled, box *mcp.ClientSession, _ moveFrom) {
				answered := callFromBox(context.WithoutCancel(t.Context()), box, "move_item",
					map[string]any{"state": "done"}, "")
				<-b.ledger.beginHolding
				closed := make(chan struct{})
				go func() {
					defer close(closed)
					b.bridge.Close("run-1")
				}()
				<-closed
				synctest.Wait()
				close(b.ledger.beginHold)
				if res := <-answered; !res.IsError || text(res) != mcpbridge.EndedNote {
					t.Fatalf("the call was answered %v %q, want it refused as not run", res.IsError, text(res))
				}
			}},
		{name: "a call its tool could not run",
			calls: func(t *testing.T, _ *bubbled, _ *mcp.ClientSession, move moveFrom) {
				if res := move("broken"); !res.IsError {
					t.Fatalf("a call that could not run answered %q", text(res))
				}
				if res := move("in_progress"); res.IsError {
					t.Fatalf("the next call failed: %q", text(res))
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				l := &ledger{}
				if tc.held {
					l.beginHold, l.beginHolding = make(chan struct{}), make(chan struct{}, 1)
				}
				var led mcpbridge.Ledger = l
				if tc.led != nil {
					led = tc.led(l)
				}
				tool := &ordinalTool{}
				b := openBubbledOn(t, l, led, tool)
				box := b.connect(t, nil)
				tc.calls(t, b, box, func(state string) *mcp.CallToolResult {
					return <-callFromBox(context.WithoutCancel(t.Context()), box, "move_item",
						map[string]any{"state": state}, "")
				})
				synctest.Wait()

				surface, ok := b.session.Surface.(*tools.Surface)
				if !ok {
					t.Fatalf("the session's surface is a %T", b.session.Surface)
				}
				live, seeded := surface.Turn().CallLog(), seededFrom(l, surface)
				for _, state := range []string{"done", "in_progress", "broken"} {
					args := surface.ArgsOf("move_item", map[string]any{"state": state}).Map()
					if got, want := live.Ordinal("move_item", args), seeded.Ordinal("move_item", args); got != want {
						t.Errorf("a move to %s counts %d earlier calls in the segment that made the calls and %d in "+
							"one seeded from the run's record (%d listed, %d in flight): an operation id derived "+
							"twice", state, got, want, len(l.rows()), len(inFlightOf(l)))
					}
				}
				t.Logf("the tool derived %v; the record lists %d call(s) and holds %d in flight",
					tool.derived(), len(l.rows()), len(inFlightOf(l)))
			})
		})
	}
}

// heldBegin holds the entry of the call at place seq until released, saying, as
// every entry reaches it, whose it is.
type heldBegin struct {
	*ledger
	seq     int
	entered chan int
	release chan struct{}
}

func (l *heldBegin) Begin(ctx context.Context, runID string, b mcpbridge.Begun) error {
	l.entered <- b.Seq
	if b.Seq == l.seq {
		<-l.release
	}
	return l.ledger.Begin(ctx, runID, b)
}

// SIDE-BY-SIDE CALLS ARE ADMITTED IN THE ORDER THEY ARRIVED, whichever entry's
// write comes back first. A harness makes a response's calls at once, and a
// call's count reads every call admitted before it, so admitted in the order
// their entries landed, two calls' counts would depend on which write the
// store answered first, and a re-run making the same calls would not reproduce
// them. Each call is admitted only once the call that arrived before it has
// been: the second call, its entry written, waits for the first's, and runs
// after it, counting it.
//
// Mutation: admit a call as soon as its own entry is written
// (admission-out-of-arrival-order), and the second call runs first, counting
// nothing before it.
func TestSideBySideCallsAreAdmittedInTheOrderTheyArrived(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		l := &ledger{}
		led := &heldBegin{ledger: l, seq: 1, entered: make(chan int, 2), release: make(chan struct{})}
		move := &ordinalTool{}
		b := openBubbledOn(t, l, led, move)
		box := b.connect(t, nil)
		ctx := context.WithoutCancel(t.Context())

		first := callFromBox(ctx, box, "move_item", map[string]any{"state": "done"}, "")
		if seq := <-led.entered; seq != 1 {
			t.Fatalf("the first call reached the ledger at place %d", seq)
		}
		second := callFromBox(ctx, box, "move_item", map[string]any{"state": "in_progress"}, "")
		if seq := <-led.entered; seq != 2 {
			t.Fatalf("the second call reached the ledger at place %d", seq)
		}
		synctest.Wait()
		if got := move.derived(); len(got) != 0 {
			t.Fatalf("with the first call's entry still being written, %v ran: a call was admitted before the "+
				"call that arrived before it", got)
		}
		close(led.release)
		for _, answered := range []<-chan *mcp.CallToolResult{first, second} {
			if res := <-answered; res.IsError {
				t.Fatalf("a call failed: %q", text(res))
			}
		}
		if got := move.derived(); got["done"] != 0 || got["in_progress"] != 1 {
			t.Errorf("the calls derived %v, want the first to count nothing before it and the second to count "+
				"the first", got)
		}
	})
}

// owingLedger answers the update of every call as one the ledger could not land
// within its own attempts ([mcpbridge.ErrUnrecorded]) until it has refused
// refuse of them — every one, where refuse is negative — and takes every update
// after on its ledger; it keeps when each was asked for.
type owingLedger struct {
	*ledger
	refuse int
	mu     sync.Mutex
	asked  []time.Time
}

func (l *owingLedger) Record(ctx context.Context, runID string, c mcpbridge.Call) error {
	l.mu.Lock()
	l.asked = append(l.asked, time.Now())
	n := len(l.asked)
	l.mu.Unlock()
	if l.refuse < 0 || n <= l.refuse {
		return fmt.Errorf("%w: the coordination store is electing a leader", mcpbridge.ErrUnrecorded)
	}
	return l.ledger.Record(ctx, runID, c)
}

func (l *owingLedger) asks() []time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.asked)
}

// AN UPDATE THE LEDGER COULD NOT LAND IS ASKED FOR AGAIN ONCE ITS ANSWER IS OUT.
// The update is everything the call did — what it answered, cost and committed,
// a submission among them — and a store that refuses it for longer than the
// ledger's own attempts comes back from that outage. So the call is answered at
// once, with its own result, and its goroutine asks for the same update again
// until it lands, once: a store back before the call's bound takes it, and the
// run's record lists the call with what it answered. Past the call's bound,
// past which no collection waits for it, the bridge asks no longer, and the
// call's entry stays, its outcome unknown. On a bubble's clock, so the asks'
// spacing costs the suite nothing.
//
// Mutation: ask for an update the ledger could not land no more
// (owed-update-never-asked-again), and the call's outcome is lost though the
// store answered a moment later.
func TestAnUpdateTheLedgerCouldNotLandIsAskedAgainOnceItsAnswerIsOut(t *testing.T) {
	t.Parallel()
	t.Run("a store back before the call's bound", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			l := &ledger{}
			led := &owingLedger{ledger: l, refuse: 3}
			post := &stubTool{name: "post_message"}
			b := openBubbledOn(t, l, led, post)
			box := b.connect(t, nil)

			res := <-callFromBox(context.WithoutCancel(t.Context()), box, "post_message",
				map[string]any{"text": "shipped"}, "")
			if res.IsError || text(res) != "post_message ok" {
				t.Fatalf("the call was answered %v %q, want its own result", res.IsError, text(res))
			}
			if rows := l.rows(); len(rows) != 0 {
				t.Fatalf("the call's answer came once its update had landed (%+v): its box waited out the asks",
					rows)
			}
			time.Sleep(time.Minute)
			synctest.Wait()
			rows := l.rows()
			if len(rows) != 1 || rows[0].Name != "post_message" || rows[0].Output != "post_message ok" {
				t.Fatalf("the run's record lists %+v, want the call once, with what it answered", rows)
			}
			if asked := led.asks(); len(asked) != 4 {
				t.Errorf("the update was asked for %d times, want four: three refused and the one that landed",
					len(asked))
			}
			if left := inFlightOf(l); len(left) != 0 {
				t.Errorf("the call's entry is still in flight beside its update: %+v", left)
			}
		})
	})
	t.Run("a store out past the call's bound", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			l := &ledger{}
			led := &owingLedger{ledger: l, refuse: -1}
			post := &stubTool{name: "post_message"}
			b := openBubbledOn(t, l, led, post)
			box := b.connect(t, nil)

			if res := <-callFromBox(context.WithoutCancel(t.Context()), box, "post_message",
				map[string]any{"text": "shipped"}, ""); res.IsError {
				t.Fatalf("the call was answered %q, want its own result", text(res))
			}
			entries := l.entries()
			if len(entries) != 1 {
				t.Fatalf("the ledger took %d entries, want the call's", len(entries))
			}
			time.Sleep(mcpbridge.CallFloor + mcpbridge.CallMargin + time.Minute)
			synctest.Wait()
			asked := led.asks()
			if len(asked) < 2 {
				t.Fatalf("the update was asked for %d times, want it asked again", len(asked))
			}
			if last := asked[len(asked)-1]; !last.Before(entries[0].Until) {
				t.Errorf("the update was last asked for at %v, past the call's bound %v", last, entries[0].Until)
			}
			time.Sleep(time.Hour)
			synctest.Wait()
			if n := len(led.asks()); n != len(asked) {
				t.Errorf("the update was asked for %d more times past the call's bound", n-len(asked))
			}
			if left := inFlightOf(l); len(left) != 1 || len(l.rows()) != 0 {
				t.Errorf("past its bound the record holds %+v in flight and lists %+v, want the call's entry alone",
					left, l.rows())
			}
		})
	})
}
