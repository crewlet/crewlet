package coordtest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the parts beside a sandbox run's record ---------------------------- //

// runLog reads the log beside a run, failing the case on an error.
func (h *fleetHarness) runLog(turnID string) (coord.Record, bool) {
	h.t.Helper()
	return h.runPart(turnID, coord.RunLog)
}

// runPart reads one part beside a run, failing the case on an error.
func (h *fleetHarness) runPart(turnID string, part coord.RunPart) (coord.Record, bool) {
	h.t.Helper()
	record, found, err := h.f.SandboxRunPart(h.ctx, turnID, part)
	if err != nil {
		h.t.Fatalf("SandboxRunPart(%s, %s): %v", turnID, part, err)
	}
	return record, found
}

// putRunLog writes value as the run's log whatever it holds, through f's own
// handle.
func (h *fleetHarness) putRunLog(turnID, value string) (coord.Record, bool) {
	h.t.Helper()
	return h.putRunPart(turnID, coord.RunLog, value)
}

// putRunPart writes value as one part beside the run whatever it holds,
// through f's own handle.
func (h *fleetHarness) putRunPart(turnID string, part coord.RunPart, value string) (coord.Record, bool) {
	h.t.Helper()
	record, changed, err := h.f.MutateSandboxRunPart(h.ctx, turnID, part, func(coord.Record, bool) ([]byte, error) {
		return []byte(value), nil
	})
	if err != nil {
		h.t.Fatalf("MutateSandboxRunPart(%s, %s): %v", turnID, part, err)
	}
	return record, changed
}

// appendMark is a log decision that appends mark to the log's writers.
func appendMark(mark int) func(coord.Record, bool) ([]byte, error) {
	return func(current coord.Record, found bool) ([]byte, error) {
		var w writers
		if found {
			if err := json.Unmarshal(current.Value, &w); err != nil {
				return nil, fmt.Errorf("decode the log: %w", err)
			}
		}
		w.Marks = append(w.Marks, mark)
		return json.Marshal(w)
	}
}

// watchRunPart opens a watch of one part beside a run, ended with the case.
func (h *fleetHarness) watchRunPart(ctx context.Context, turnID string,
	part coord.RunPart) <-chan coord.SandboxRunUpdate {

	h.t.Helper()
	updates, err := h.f.WatchSandboxRunPart(ctx, turnID, part)
	if err != nil {
		h.t.Fatalf("WatchSandboxRunPart(%s, %s): %v", turnID, part, err)
	}
	return updates
}

var runPartCases = []fleetCase{{
	// WHAT A NODE WAITING ON A RUN'S CALLS IN FLIGHT FOLLOWS. A part's watch
	// says where the part stands when it begins — none, for a run nobody
	// logged a call of yet — then delivers each state it reaches, and its
	// removal with the run's record: a watch that missed the removal would
	// wait on calls of a run that is gone. A write of the record, or of the
	// other part, is none of its: a node waiting on a log must not be woken by
	// every lease stamped on the run.
	name: "a part's watch hears its state, each change and its removal, and no other key",
	fn: func(h *fleetHarness) {
		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		h.createRun("turn-1", `{"status":"running"}`)
		updates := h.watchRunPart(ctx, "turn-1", coord.RunLog)
		if u := h.nextRunUpdate(updates); u.Found {
			h.t.Fatalf("the first update of a log nobody wrote = %+v, want none", u)
		}
		written, _ := h.putRunLog("turn-1", `{"calls":["first"]}`)
		if u := h.nextRunUpdate(updates); !u.Found || string(u.Record.Value) != `{"calls":["first"]}` ||
			u.Record.Version != written.Version || u.Record.Key != "turn-1" {
			h.t.Fatalf("the update after the log's first write = %+v, want the log as written (%+v)", u, written)
		}
		h.putRunPart("turn-1", coord.RunHanded, `{"sealed":"x"}`)
		h.putRun("turn-1", `{"status":"resumed"}`)
		next, _ := h.putRunLog("turn-1", `{"calls":["first","second"]}`)
		if u := h.nextRunUpdate(updates); !u.Found || u.Record.Version != next.Version {
			h.t.Fatalf("the next update = %+v, want the log's own next write (%+v) and nothing of the "+
				"record's or the other part's", u, next)
		}
		// A part watched while it exists starts from it.
		second := h.watchRunPart(ctx, "turn-1", coord.RunLog)
		if u := h.nextRunUpdate(second); !u.Found || u.Record.Version != next.Version {
			h.t.Fatalf("the first update of a watch of a written log = %+v, want it (%+v)", u, next)
		}
		if !h.removeRun("turn-1") {
			h.t.Fatal("the run could not be removed")
		}
		for _, w := range []<-chan coord.SandboxRunUpdate{updates, second} {
			if u := h.nextRunUpdate(w); u.Found {
				h.t.Fatalf("the update after the run's removal = %+v, want its log gone with it", u)
			}
		}
		if _, err := h.f.WatchSandboxRunPart(h.ctx, "turn-1", "notes"); err == nil {
			h.t.Error("WatchSandboxRunPart watched a part no run has")
		}
		cancel()
		h.awaitClosed(updates)
		h.awaitClosed(second)
	},
}, {
	// What the log is kept apart FOR. A box's git request reads its run's
	// record and a floor raise writes it, and every listing reads every
	// record: a log that rode any of them — or moved the record's version,
	// on which every flip conditions — put two hundred calls' arguments and
	// output back on all of them.
	name: "a run's log is kept beside its record and never moves it",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running"}`)
		before, _ := h.run("turn-1")
		if _, found := h.runLog("turn-1"); found {
			h.t.Fatal("a run nobody logged anything for has a log")
		}
		written, changed := h.putRunLog("turn-1", `{"calls":["first"]}`)
		if !changed || written.Key != "turn-1" || written.Version == 0 ||
			string(written.Value) != `{"calls":["first"]}` {
			h.t.Fatalf("the first log write answered (%+v, %v), want the log written under the turn", written, changed)
		}
		log, found := h.runLog("turn-1")
		if !found || string(log.Value) != `{"calls":["first"]}` || log.Version != written.Version {
			h.t.Errorf("the log reads (%+v, %v), want what was written (%+v)", log, found, written)
		}
		after, _ := h.run("turn-1")
		if string(after.Value) != `{"status":"running"}` || after.Version != before.Version {
			h.t.Errorf("writing the log moved the record: %+v, was %+v", after, before)
		}
		// THE HANDED SET IS A PART OF ITS OWN, kept apart from the record
		// for the log's reason and from the log, which it never moves.
		if _, found := h.runPart("turn-1", coord.RunHanded); found {
			h.t.Fatal("a run nobody handed anything has a handed set")
		}
		h.putRunPart("turn-1", coord.RunHanded, `{"sealed":"s"}`)
		if handed, found := h.runPart("turn-1", coord.RunHanded); !found || string(handed.Value) != `{"sealed":"s"}` {
			h.t.Errorf("the handed set reads (%s, %v), want what was written", handed.Value, found)
		}
		if again, _ := h.runLog("turn-1"); again.Version != written.Version {
			h.t.Errorf("writing the handed set moved the log: %+v, was %+v", again, written)
		}
		if after, _ := h.run("turn-1"); after.Version != before.Version {
			h.t.Errorf("writing the handed set moved the record: %+v, was %+v", after, before)
		}
		runs, err := h.f.SandboxRuns(h.ctx)
		if err != nil {
			h.t.Fatalf("SandboxRuns: %v", err)
		}
		if len(runs) != 1 || runs[0].Key != "turn-1" || string(runs[0].Value) != `{"status":"running"}` {
			h.t.Errorf("the listing of records = %+v, want the one record and never a part of it", runs)
		}
	},
}, {
	// The log's own read-decide-write, held to the record's: a decision
	// another writer beat is decided again on the log as it now stands —
	// never sent as it was, which would drop the call the other writer
	// appended.
	name: "a log write another writer beat is decided again on the log as it stands",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running"}`)
		h.putRunLog("turn-1", `{"marks":[1]}`)
		calls := 0
		written, changed, err := h.f.MutateSandboxRunPart(h.ctx, "turn-1", coord.RunLog, func(current coord.Record, found bool) ([]byte, error) {
			calls++
			if calls == 1 {
				_, ok, werr := h.f.MutateSandboxRunPart(h.ctx, "turn-1", coord.RunLog, appendMark(2))
				if err := landedFor("the staged log write", ok, werr); err != nil {
					return nil, err
				}
			}
			return appendMark(3)(current, found)
		})
		if err != nil || !changed {
			h.t.Fatalf("MutateSandboxRunPart = (%v, %v), want it landed", changed, err)
		}
		if calls != 2 {
			h.t.Errorf("the decision was asked %d times, want twice: once beaten, once on the newer log", calls)
		}
		var w writers
		if err := json.Unmarshal(written.Value, &w); err != nil || !slices.Equal(w.Marks, []int{1, 2, 3}) {
			h.t.Errorf("the log holds %s, want every write's mark: [1 2 3]", written.Value)
		}
	},
}, {
	// Every tool call a box makes over the bridge is an append, from as many
	// as twenty subagents at once on the node serving the bridge — and a
	// peer's recovery may write it too. Each lands, or a call that ran is
	// missing from the log a resumed turn judges the run by.
	name: "concurrent log writes from several nodes all land",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running"}`)
		nodes := h.nodes(3)
		const each = 8
		var wg sync.WaitGroup
		errs := make(chan error, len(nodes)*each)
		for n, node := range nodes {
			for i := range each {
				wg.Go(func() {
					_, ok, werr := node.MutateSandboxRunPart(h.ctx, "turn-1", coord.RunLog, appendMark(n*each+i))
					if err := landedFor("an append", ok, werr); err != nil {
						errs <- err
					}
				})
			}
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			h.t.Error(err)
		}
		log, _ := h.runLog("turn-1")
		var w writers
		if err := json.Unmarshal(log.Value, &w); err != nil {
			h.t.Fatalf("decode the log: %v", err)
		}
		slices.Sort(w.Marks)
		want := make([]int, len(nodes)*each)
		for i := range want {
			want[i] = i
		}
		if !slices.Equal(w.Marks, want) {
			h.t.Errorf("the log holds %v, want every one of %d appends exactly once", w.Marks, len(nodes)*each)
		}
	},
}, {
	// A part lives and dies with its record: the run's end removes every
	// one, and a record created under the turn again starts with none rather
	// than the ended run's calls, which its resume would replay as its own,
	// or the secrets its box was handed, which a sealed copy of nothing
	// still needs.
	name: "removing a run removes every part of it",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"done"}`)
		for _, part := range coord.RunParts() {
			h.putRunPart("turn-1", part, `{"calls":["submit_work"]}`)
		}
		if !h.removeRun("turn-1") {
			h.t.Fatal("the removal did not land")
		}
		for _, part := range coord.RunParts() {
			if held, found := h.runPart("turn-1", part); found {
				h.t.Errorf("the ended run's %s survived it: %s", part, held.Value)
			}
		}
		h.createRun("turn-1", `{"status":"launching"}`)
		for _, part := range coord.RunParts() {
			if held, found := h.runPart("turn-1", part); found {
				h.t.Errorf("a run created again under the turn reads the ended run's %s: %s", part, held.Value)
			}
		}
	},
}, {
	// THE RACE THE STORE CLOSES, not its caller: a log write decided while
	// the run stood and landing after its end — the late tool call of a box
	// that is shutting down, the job a launch opened as a stop ended the
	// run. Staged inside the decision: the run is removed under it, with
	// its log, before the write it decides lands. A log left there would be
	// held by no record, read by nobody, and never removed by the run's end,
	// which has already happened.
	name: "a log write racing its run's removal leaves no log",
	fn: func(h *fleetHarness) {
		for _, logged := range []bool{false, true} {
			turn := fmt.Sprintf("turn-logged-%v", logged)
			h.createRun(turn, `{"status":"running"}`)
			if logged {
				h.putRunLog(turn, `{"marks":[0]}`)
			}
			removed := false
			written, changed, err := h.f.MutateSandboxRunPart(h.ctx, turn, coord.RunLog, func(current coord.Record, found bool) ([]byte, error) {
				if !removed {
					removed = true
					_, ok, werr := h.f.MutateSandboxRun(h.ctx, turn, func(coord.Record) (coord.SandboxRunChange, error) {
						return coord.SandboxRunChange{Delete: true}, nil
					})
					if err := landedFor("the staged removal", ok, werr); err != nil {
						return nil, err
					}
				}
				return appendMark(1)(current, found)
			})
			if err != nil || changed {
				h.t.Errorf("a log write after its run's end (logged before: %v) answered (%+v, %v, %v), "+
					"want nothing landed", logged, written, changed, err)
			}
			if log, found := h.runLog(turn); found {
				h.t.Errorf("a log write racing the run's end (logged before: %v) left a log behind: %s",
					logged, log.Value)
			}
		}
	},
}, {
	name: "a log of a run with no record is never written",
	fn: func(h *fleetHarness) {
		written, changed, err := h.f.MutateSandboxRunPart(h.ctx, "turn-none", coord.RunLog, appendMark(1))
		if err != nil || changed {
			h.t.Errorf("a log write for a run with no record answered (%+v, %v, %v), want nothing landed",
				written, changed, err)
		}
		if log, found := h.runLog("turn-none"); found {
			h.t.Errorf("a run with no record has a log: %s", log.Value)
		}
		if _, _, err := h.f.SandboxRunPart(h.ctx, "", coord.RunLog); err == nil {
			h.t.Error("SandboxRunPart read a log with no turn id")
		}
		if _, _, err := h.f.MutateSandboxRunPart(h.ctx, "", coord.RunLog, appendMark(1)); err == nil {
			h.t.Error("MutateSandboxRunPart wrote a log with no turn id")
		}
		// A PART NOBODY NAMED is refused rather than written under a key
		// that neither the record's removal nor the sweep would ever take.
		h.createRun("turn-1", `{"status":"running"}`)
		if _, _, err := h.f.MutateSandboxRunPart(h.ctx, "turn-1", "notes", appendMark(1)); err == nil {
			h.t.Error("MutateSandboxRunPart wrote a part that is not one of coord.RunParts")
		}
		if _, _, err := h.f.SandboxRunPart(h.ctx, "turn-1", "notes"); err == nil {
			h.t.Error("SandboxRunPart read a part that is not one of coord.RunParts")
		}
	},
}}
