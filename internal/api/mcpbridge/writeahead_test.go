package mcpbridge_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/tools"
)

// How a call's entry is written on its run's record before it runs (the
// package doc's "A call is on its run's record before it runs").

// entryReader is a tool that reads, as it runs, every entry its run's ledger
// holds — whether its own call's was written ahead of it.
type entryReader struct {
	ledger *ledger
	saw    chan []mcpbridge.Begun
}

func (r *entryReader) Name() string        { return "write" }
func (r *entryReader) Description() string { return "writes a thing" }
func (r *entryReader) Parameters() map[string]any {
	return map[string]any{"type": "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}}}
}

func (r *entryReader) Call(context.Context, map[string]any) (tools.Result, error) {
	r.saw <- r.ledger.entries()
	return tools.Result{Output: "written"}, nil
}

// A CALL IS ON ITS RUN'S RECORD BEFORE ITS TOOL RUNS. The bridge writes the
// call's entry — its tool, its place, the instant it took it, the digest it was
// admitted under and the bound past which nobody waits for it — on the run's
// ledger ahead of the call, and runs the call only once that write is taken:
// so a call that runs is a call a resumed segment counts, whatever becomes of
// its record afterwards. Its one update then carries the same identity, so it
// finds that entry.
//
// Mutation: run the call without writing its entry (`false &&` before the
// ledger's begin), and its tool runs with nothing on the record.
func TestACallIsOnItsRunsRecordBeforeItsToolRuns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		l := &ledger{}
		tool := &entryReader{ledger: l, saw: make(chan []mcpbridge.Begun, 1)}
		b := openBubbled(t, l, tool)
		box := b.connect(t, nil)

		res := <-callFromBox(context.WithoutCancel(t.Context()), box, "write", map[string]any{"value": "a"}, "write-1")
		if res.IsError || text(res) != "written" {
			t.Fatalf("the call was answered %v %q, want its tool's own answer", res.IsError, text(res))
		}
		saw := <-tool.saw
		if len(saw) != 1 || saw[0].Name != "write" || saw[0].Seq != 1 || saw[0].Digest == "" {
			t.Fatalf("as its tool ran, the run's ledger held %+v, want the call's own entry, its digest with it", saw)
		}
		if want := saw[0].StartedAt.Add(mcpbridge.CallFloor + mcpbridge.CallMargin); !saw[0].Until.Equal(want) {
			t.Errorf("the entry's bound is %v, want the instant the bridge took the call, its tool's bound and "+
				"the bridge's margin, %v", saw[0].Until, want)
		}
		rows := l.rows()
		if len(rows) != 1 || rows[0].Seq != saw[0].Seq || !rows[0].StartedAt.Equal(saw[0].StartedAt) ||
			rows[0].Name != saw[0].Name || rows[0].Digest != saw[0].Digest {
			t.Errorf("the call's update is %+v, want it at its entry's identity %+v", rows, saw[0])
		}
	})
}

// A CALL WHOSE ENTRY CANNOT BE WRITTEN IS NOT RUN, AND IS REFUSED. A call that
// ran with no record of it could be counted by no later call of its run, which
// could then derive its operation id again; so a call whose entry the ledger
// did not take is never run. Its box is told why, FAILED — nothing ran, so the
// call can be made again: where the store refused every attempt, that it could
// not be recorded and may be made again, in the words a call reaching a closing
// session is told; where the run's log takes no more calls of its job, that its
// run has ended. Nothing is recorded of it.
//
// Mutation: run the call whatever its entry's write answered (`false &&` before
// the begin's error), and its tool runs off the record.
func TestACallWhoseEntryCannotBeWrittenIsNotRunAndIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"a store that refused every attempt", errors.New("the coordination store is electing a leader"),
			mcpbridge.UnrecordedNote},
		{"a log that takes no more calls of the job", mcpbridge.ErrNotTaken, mcpbridge.EndedNote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				l := &ledger{beginErr: tc.err}
				tool := &stubTool{name: "write"}
				b := openBubbled(t, l, tool)
				box := b.connect(t, nil)

				res := <-callFromBox(context.WithoutCancel(t.Context()), box, "write", map[string]any{"text": "a"},
					"write-1")
				if !res.IsError || text(res) != tc.want {
					t.Errorf("the call was answered %v %q, want %q, failed", res.IsError, text(res), tc.want)
				}
				synctest.Wait()
				if seen := tool.seen(); len(seen) != 0 {
					t.Errorf("the call's tool ran %d times though its entry was not written", len(seen))
				}
				if rows := l.rows(); len(rows) != 0 {
					t.Errorf("a call that never ran is recorded: %+v", rows)
				}
			})
		})
	}
}

// lostAnswer takes every entry and then answers its write as failed, as a
// coordination store does whose answer to a compare-and-swap that landed was
// lost on its way back.
type lostAnswer struct{ *ledger }

func (l lostAnswer) Begin(ctx context.Context, runID string, b mcpbridge.Begun) error {
	if err := l.ledger.Begin(ctx, runID, b); err != nil {
		return err
	}
	return errors.New("the coordination store's answer was lost")
}

// A CALL WHOSE ENTRY IS STILL BEING WRITTEN IS NEVER RUN WHERE ITS OPENING ENDS
// OR ITS SEAT'S FENCE CLOSES MEANWHILE. An opening's end answers a call it holds
// whose entry is still being written that it was not run — failed, in the words
// a call reaching the opening after its end gets, so its box makes it again
// where it goes on — and a seat's fence found closed once the entry is written
// tells the box its session was closing; either way the call never runs, and
// its entry, written all the same, is withdrawn in the same step, so no
// collection waits on it and no continuation is told of a call that never
// happened. AND AN ENTRY WHOSE WRITE LANDED AND LOST ITS ANSWER as the end came
// is withdrawn as well: the log may hold it, and only a withdrawal finds out.
//
// Mutation: answer a call the end finds not yet begun as still running (`false
// &&` before the end's test of it), and its box is told a call that never runs
// is running; withdraw an ended call's entry only where its write answered nil
// (entry-of-an-unrun-call-left-in-flight), and an entry that landed and lost its
// answer stays in the run's log, a call nobody runs.
func TestACallWhoseEntryIsStillBeingWrittenIsNeverRunWhereItsOpeningEnds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// stop ends the opening, or closes the seat's fence, while the call's
		// entry is being written.
		stop func(b *bubbled, fenced *atomic.Bool)
		want string
		// lost is an entry whose write lands and answers failed.
		lost bool
	}{
		{"the run's close", func(b *bubbled, _ *atomic.Bool) { b.bridge.Close("run-1") }, mcpbridge.EndedNote, false},
		{"the run's close, the entry's answer lost", func(b *bubbled, _ *atomic.Bool) { b.bridge.Close("run-1") },
			mcpbridge.EndedNote, true},
		{"the seat leaving the node", func(b *bubbled, _ *atomic.Bool) { b.bridge.ReleaseSeat(seat()) },
			mcpbridge.ClosingNote, false},
		{"the seat's fence closing", func(_ *bubbled, fenced *atomic.Bool) { fenced.Store(true) },
			mcpbridge.ClosingNote, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				l := &ledger{beginHold: make(chan struct{}), beginHolding: make(chan struct{}, 1)}
				tool := &stubTool{name: "write"}
				var led mcpbridge.Ledger = l
				if tc.lost {
					led = lostAnswer{l}
				}
				b := openBubbledOn(t, l, led, tool)
				var fenced atomic.Bool
				b.session.Fence = func() error {
					if fenced.Load() {
						return errors.New("the seat left this node")
					}
					return nil
				}
				box := b.connect(t, nil)

				answered := callFromBox(context.WithoutCancel(t.Context()), box, "write", map[string]any{"text": "a"},
					"write-1")
				<-l.beginHolding
				stopped := make(chan struct{})
				go func() {
					defer close(stopped)
					tc.stop(b, &fenced)
				}()
				<-stopped
				synctest.Wait()
				close(l.beginHold)
				res := <-answered
				if !res.IsError || text(res) != tc.want {
					t.Errorf("the box was answered %v %q, want %q, failed", res.IsError, text(res), tc.want)
				}
				synctest.Wait()
				if seen := tool.seen(); len(seen) != 0 {
					t.Errorf("a call whose entry was still being written ran %d times", len(seen))
				}
				if w := l.withdrawals(); len(w) != 1 || w[0].Name != "write" || w[0].Seq != 1 {
					t.Errorf("the ledger took the withdrawals %+v, want the call's own entry withdrawn", w)
				}
				if rows := l.rows(); len(rows) != 0 {
					t.Errorf("a call that never ran is recorded: %+v", rows)
				}
			})
		})
	}
}
