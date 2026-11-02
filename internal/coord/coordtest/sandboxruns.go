package coordtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the detached sandbox runs ----------------------------------------- //

func (h *fleetHarness) createRun(turnID, value string) bool {
	h.t.Helper()
	created, err := h.f.CreateSandboxRun(h.ctx, turnID, []byte(value))
	if err != nil {
		h.t.Fatalf("CreateSandboxRun(%s): %v", turnID, err)
	}
	return created
}

func (h *fleetHarness) run(turnID string) (coord.Record, bool) {
	h.t.Helper()
	record, found, err := h.f.SandboxRun(h.ctx, turnID)
	if err != nil {
		h.t.Fatalf("SandboxRun(%s): %v", turnID, err)
	}
	return record, found
}

// mutateRun changes a run through f's own handle, failing the case on an
// error.
func (h *fleetHarness) mutateRun(turnID string,
	decide func(coord.Record) (coord.SandboxRunChange, error)) (coord.Record, bool) {

	h.t.Helper()
	record, changed, err := h.f.MutateSandboxRun(h.ctx, turnID, decide)
	if err != nil {
		h.t.Fatalf("MutateSandboxRun(%s): %v", turnID, err)
	}
	return record, changed
}

// putRun writes value over whatever the run's record holds.
func (h *fleetHarness) putRun(turnID, value string) (coord.Record, bool) {
	h.t.Helper()
	return h.mutateRun(turnID, func(coord.Record) (coord.SandboxRunChange, error) {
		return coord.SandboxRunChange{Put: []byte(value)}, nil
	})
}

// removeRun deletes the run's record, whatever it holds.
func (h *fleetHarness) removeRun(turnID string) bool {
	h.t.Helper()
	_, removed := h.mutateRun(turnID, func(coord.Record) (coord.SandboxRunChange, error) {
		return coord.SandboxRunChange{Delete: true}, nil
	})
	return removed
}

// removeRunAt deletes a run's record only where it still stands at the version
// r carries, reporting whether it did — the removal a caller that read the
// record makes.
func (h *fleetHarness) removeRunAt(r coord.Record) bool {
	h.t.Helper()
	_, removed := h.mutateRun(r.Key, func(current coord.Record) (coord.SandboxRunChange, error) {
		return coord.SandboxRunChange{Delete: current.Version == r.Version}, nil
	})
	return removed
}

// watchRun opens a watch of one run, ended with the case.
func (h *fleetHarness) watchRun(ctx context.Context, turnID string) <-chan coord.SandboxRunUpdate {
	h.t.Helper()
	updates, err := h.f.WatchSandboxRun(ctx, turnID)
	if err != nil {
		h.t.Fatalf("WatchSandboxRun(%s): %v", turnID, err)
	}
	return updates
}

// nextRunUpdate reads one update, failing the case if none arrives.
func (h *fleetHarness) nextRunUpdate(updates <-chan coord.SandboxRunUpdate) coord.SandboxRunUpdate {
	h.t.Helper()
	select {
	case u, ok := <-updates:
		if !ok {
			h.t.Fatal("the run watch closed while the store was healthy")
		}
		return u
	case <-time.After(stallBudget):
		h.t.Fatal("the run watch delivered nothing")
	}
	return coord.SandboxRunUpdate{}
}

// awaitClosed drains a watch until its channel closes, failing the case if it
// never does.
func (h *fleetHarness) awaitClosed(updates <-chan coord.SandboxRunUpdate) {
	h.t.Helper()
	deadline := time.After(stallBudget)
	for {
		select {
		case _, ok := <-updates:
			if !ok {
				return
			}
		case <-deadline:
			h.t.Fatal("a cancelled run watch never closed its channel")
		}
	}
}

// landedFor is a write a case stages from inside a decision, as an error the
// decision returns: what went wrong, or that it landed nothing.
func landedFor(what string, ok bool, err error) error {
	switch {
	case err != nil:
		return fmt.Errorf("%s: %w", what, err)
	case !ok:
		return fmt.Errorf("%s landed nothing", what)
	}
	return nil
}

// writers is the record a mutation case appends each writer's mark to.
type writers struct {
	Marks []int `json:"marks"`
}

var runCases = []fleetCase{{
	// The reason this moved off the node's own database. A detached run
	// outlives its turn, its process and sometimes its node — and the node
	// that owns the seat AFTERWARDS is the one whose recovery pass has to
	// see it. Per-node, that pass found nothing: the suspended Execute
	// conversation became unreachable and a billed box was neither resumed
	// nor reaped.
	name: "a run one node launched is readable by the seat's next owner",
	fn: func(h *fleetHarness) {
		if !h.createRun("turn-1", `{"status":"running"}`) {
			h.t.Fatal("the first create lost")
		}
		record, found := h.run("turn-1")
		if !found {
			h.t.Fatal("the run a peer launched is invisible — its box leaks")
		}
		if string(record.Value) != `{"status":"running"}` {
			h.t.Errorf("value = %q", record.Value)
		}
		if record.Key != "turn-1" {
			h.t.Errorf("key = %q, want the turn id", record.Key)
		}
		if record.Version == 0 {
			h.t.Error("version = 0 — a caller cannot condition a write on it")
		}
	},
}, {
	// The kick-off turn's id IS the key, so a second create is a retried
	// launch and not a second run. Overwriting would replace the box
	// reference of a job that is already executing.
	name: "a duplicate launch is a retry, not a second run",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running","box":"sbx-1"}`)
		if h.createRun("turn-1", `{"status":"running","box":"sbx-2"}`) {
			h.t.Error("a second create reported itself as new")
		}
		record, _ := h.run("turn-1")
		if string(record.Value) != `{"status":"running","box":"sbx-1"}` {
			h.t.Errorf("value = %q — the retry overwrote a live run", record.Value)
		}
	},
}, {
	// A DECISION IS TAKEN ON WHAT THE LAST WRITE LEFT. Every mutation on a
	// run is a conditional flip whose condition is one of the run's OWN
	// fields — the at-most-once tail claim above all — so the record a
	// decision is handed has to be the one the write before it stored, at
	// a version that moved with it.
	name: "each mutation decides on the record the last one left",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running"}`)
		first, _ := h.run("turn-1")

		written, changed := h.mutateRun("turn-1", func(current coord.Record) (coord.SandboxRunChange, error) {
			if string(current.Value) != `{"status":"running"}` || current.Version != first.Version ||
				current.Key != "turn-1" {
				return coord.SandboxRunChange{}, fmt.Errorf("decided on %+v, want the record as created", current)
			}
			return coord.SandboxRunChange{Put: []byte(`{"status":"resumed"}`)}, nil
		})
		if !changed {
			h.t.Fatal("a mutation that chose to write reported it wrote nothing")
		}
		if string(written.Value) != `{"status":"resumed"}` || written.Key != "turn-1" {
			h.t.Errorf("the mutation answered %+v, want the record it wrote", written)
		}
		if written.Version == first.Version || written.Version == 0 {
			h.t.Errorf("the version is %d after a write from %d, so nothing can tell the two apart",
				written.Version, first.Version)
		}
		stored, _ := h.run("turn-1")
		if string(stored.Value) != `{"status":"resumed"}` || stored.Version != written.Version {
			h.t.Errorf("the store holds %+v, want what the mutation answered (%+v)", stored, written)
		}
		h.mutateRun("turn-1", func(current coord.Record) (coord.SandboxRunChange, error) {
			if string(current.Value) != `{"status":"resumed"}` || current.Version != written.Version {
				h.t.Errorf("the next decision read %+v, want the last write's record", current)
			}
			return coord.SandboxRunChange{}, nil
		})
	},
}, {
	// THE BUSIEST RECORD A RUN HAS IS ITS OWN, and its writers are many: every
	// tool call a box makes over the bridge appends to it — from as many as
	// twenty subagents at once — beside the seat's own turns. Every one of
	// them must land, none overwriting another, and none may be answered
	// "the record kept changing": a lost race is another writer landing, which
	// proves the store healthy. MORE WRITERS THAN SIXTEEN, ACROSS FOUR NODES,
	// so a backend that bounds its retries by a count it guessed, or that
	// arbitrates only one handle's writes, is found out.
	name: "every one of many concurrent mutations lands",
	fn: func(h *fleetHarness) {
		const callers, each = 40, 3
		h.createRun("turn-1", `{"marks":[]}`)
		nodes := h.nodes(4)
		var wg sync.WaitGroup
		errs := make(chan error, callers*each)
		// Released together, so the writers overlap rather than each
		// finishing before the next is scheduled.
		start := make(chan struct{})
		for i := range callers {
			wg.Go(func() {
				<-start
				for n := range each {
					mark := i*each + n
					_, changed, err := nodes[i%len(nodes)].MutateSandboxRun(h.ctx, "turn-1",
						func(current coord.Record) (coord.SandboxRunChange, error) {
							var w writers
							if err := json.Unmarshal(current.Value, &w); err != nil {
								return coord.SandboxRunChange{}, err
							}
							w.Marks = append(w.Marks, mark)
							next, err := json.Marshal(w)
							return coord.SandboxRunChange{Put: next}, err
						})
					switch {
					case err != nil:
						errs <- fmt.Errorf("mark %d: %w", mark, err)
					case !changed:
						errs <- fmt.Errorf("mark %d: the mutation reported it wrote nothing", mark)
					}
				}
			})
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			h.t.Errorf("a mutation under contention failed: %v", err)
		}
		record, _ := h.run("turn-1")
		var w writers
		if err := json.Unmarshal(record.Value, &w); err != nil {
			h.t.Fatalf("the record no longer decodes: %v (%q)", err, record.Value)
		}
		slices.Sort(w.Marks)
		want := make([]int, callers*each)
		for i := range want {
			want[i] = i
		}
		if !slices.Equal(w.Marks, want) {
			h.t.Fatalf("the record holds %d marks %v after %d concurrent mutations, want every one "+
				"exactly once: a write that landed over another lost it", len(w.Marks), w.Marks, len(want))
		}
	},
}, {
	// THE ZERO CHANGE IS AN ANSWER: the condition the caller evaluated did not
	// hold, and the record stays exactly as it was read — its bytes and its
	// version. And a decision that scribbles over the bytes it was handed
	// has not written anything either.
	name: "a mutation that decides nothing writes nothing",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running"}`)
		before, _ := h.run("turn-1")
		read, changed := h.mutateRun("turn-1", func(current coord.Record) (coord.SandboxRunChange, error) {
			for i := range current.Value {
				current.Value[i] = 'x'
			}
			return coord.SandboxRunChange{}, nil
		})
		if changed {
			h.t.Error("a mutation that decided nothing reported a write")
		}
		if string(read.Value) != `{"status":"running"}` || read.Version != before.Version {
			h.t.Errorf("it answered %+v, want the record as read (%+v)", read, before)
		}
		after, _ := h.run("turn-1")
		if string(after.Value) != `{"status":"running"}` || after.Version != before.Version {
			h.t.Errorf("the store holds %+v after a mutation that decided nothing, want %+v", after, before)
		}
	},
}, {
	// A DELETE IS DECIDED ON A VERSION LIKE ANY OTHER WRITE. A terminal delete
	// raced by a write that reopened the run must not take the reopened row
	// with it: the decision is asked again on the reopened record, and what
	// it decides there is what happens.
	name: "a mutation deletes the record at the version it decided on",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"done"}`)
		peer := h.nodes(2)[1]
		var seen []string
		_, removed := h.mutateRun("turn-1", func(current coord.Record) (coord.SandboxRunChange, error) {
			seen = append(seen, string(current.Value))
			if len(seen) == 1 {
				// Another node reopens the run between this
				// decision's read and its write.
				_, ok, err := peer.MutateSandboxRun(h.ctx, "turn-1",
					func(coord.Record) (coord.SandboxRunChange, error) {
						return coord.SandboxRunChange{Put: []byte(`{"status":"reopened"}`)}, nil
					})
				if staged := landedFor("the peer's reopen", ok, err); staged != nil {
					return coord.SandboxRunChange{}, staged
				}
			}
			if string(current.Value) != `{"status":"done"}` {
				return coord.SandboxRunChange{}, nil
			}
			return coord.SandboxRunChange{Delete: true}, nil
		})
		if removed {
			h.t.Error("a delete decided on a record somebody reopened reported it removed the run")
		}
		if want := []string{`{"status":"done"}`, `{"status":"reopened"}`}; !slices.Equal(seen, want) {
			h.t.Errorf("the decision was asked on %q, want the record it read and then the reopened one", seen)
		}
		if record, found := h.run("turn-1"); !found || string(record.Value) != `{"status":"reopened"}` {
			h.t.Fatalf("the run reads (%q, %v) — a delete decided on its old record took the reopened one",
				record.Value, found)
		}

		before, _ := h.run("turn-1")
		gone, removed := h.mutateRun("turn-1", func(coord.Record) (coord.SandboxRunChange, error) {
			return coord.SandboxRunChange{Delete: true}, nil
		})
		if !removed {
			h.t.Fatal("a delete decided on the record as it stands was refused")
		}
		if string(gone.Value) != `{"status":"reopened"}` || gone.Version != before.Version {
			h.t.Errorf("the delete answered %+v, want the record it removed (%+v)", gone, before)
		}
		if _, found := h.run("turn-1"); found {
			h.t.Error("the run survived its delete")
		}
	},
}, {
	// A version belongs to one incarnation of a record. A decision taken on
	// a run that was then removed and created again under the same turn id
	// must not land on the successor, as it could not against a KV revision,
	// which never repeats: it is asked again, on the record that is there.
	name: "a mutation decided on a removed run never lands on its successor",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"done"}`)
		peer := h.nodes(2)[1]
		var seen []string
		h.mutateRun("turn-1", func(current coord.Record) (coord.SandboxRunChange, error) {
			seen = append(seen, string(current.Value))
			if len(seen) > 1 {
				return coord.SandboxRunChange{}, nil
			}
			_, ok, err := peer.MutateSandboxRun(h.ctx, "turn-1",
				func(coord.Record) (coord.SandboxRunChange, error) {
					return coord.SandboxRunChange{Delete: true}, nil
				})
			if staged := landedFor("the peer's delete", ok, err); staged != nil {
				return coord.SandboxRunChange{}, staged
			}
			ok, err = peer.CreateSandboxRun(h.ctx, "turn-1", []byte(`{"status":"running"}`))
			if staged := landedFor("the peer's create", ok, err); staged != nil {
				return coord.SandboxRunChange{}, staged
			}
			return coord.SandboxRunChange{Put: []byte(`{"status":"stale"}`)}, nil
		})
		if want := []string{`{"status":"done"}`, `{"status":"running"}`}; !slices.Equal(seen, want) {
			h.t.Errorf("the decision was asked on %q, want the removed run and then its successor", seen)
		}
		if record, _ := h.run("turn-1"); string(record.Value) != `{"status":"running"}` {
			h.t.Errorf("the successor holds %q — a decision taken on the removed run overwrote it",
				record.Value)
		}
	},
}, {
	// "There is no run" is an answer, never an error, and never a run: a
	// mutation of a record that is gone — the run settled under it — has
	// nothing to decide on, and a decision asked about nothing could only
	// invent one.
	name: "a mutation of a missing run decides nothing",
	fn: func(h *fleetHarness) {
		asked := false
		record, changed, err := h.f.MutateSandboxRun(h.ctx, "missing",
			func(coord.Record) (coord.SandboxRunChange, error) {
				asked = true
				return coord.SandboxRunChange{Put: []byte(`{"status":"running"}`)}, nil
			})
		if err != nil || changed || record.Key != "" || record.Version != 0 || len(record.Value) != 0 {
			h.t.Errorf("MutateSandboxRun(missing) = (%+v, %v, %v), want (zero, false, nil)", record, changed, err)
		}
		if asked {
			h.t.Error("the decision was asked about a run that has no record")
		}
		if _, found := h.run("missing"); found {
			h.t.Error("a mutation of a missing run created one")
		}
	},
}, {
	// A CALLER'S OWN REFUSAL REACHES IT IN ITS OWN VOCABULARY, with nothing
	// written: the run's package refuses in sentinels it compares with
	// errors.Is, and a store that wrapped them away — or wrote half a
	// decision — would turn a refusal into an outage. A change that both
	// writes and removes the record is refused the same way.
	name: "a refused decision writes nothing and is returned as it is",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running"}`)
		before, _ := h.run("turn-1")
		refusal := errors.New("the run is ending")
		_, changed, err := h.f.MutateSandboxRun(h.ctx, "turn-1",
			func(coord.Record) (coord.SandboxRunChange, error) {
				return coord.SandboxRunChange{Put: []byte(`{"status":"half"}`)}, refusal
			})
		if !errors.Is(err, refusal) || changed {
			h.t.Errorf("a refused decision answered (%v, %v), want the refusal and no write", changed, err)
		}
		_, changed, err = h.f.MutateSandboxRun(h.ctx, "turn-1",
			func(coord.Record) (coord.SandboxRunChange, error) {
				return coord.SandboxRunChange{Put: []byte(`{"status":"both"}`), Delete: true}, nil
			})
		if err == nil || changed {
			h.t.Errorf("a change that writes and removes the run answered (%v, %v), want a refusal", changed, err)
		}
		after, found := h.run("turn-1")
		if !found || string(after.Value) != `{"status":"running"}` || after.Version != before.Version {
			h.t.Errorf("the store holds (%+v, %v), want the record untouched (%+v)", after, found, before)
		}
	},
}, {
	// What the node serving a run's bridge holds its view of the run from. The
	// watch must say where the record stands when it begins — "no record" is
	// otherwise indistinguishable from "nothing has arrived yet" — and then
	// deliver each state it reaches, a delete included: a watch that missed
	// the delete would serve a run that has finished.
	name: "a watch hears the record, each change and its delete",
	fn: func(h *fleetHarness) {
		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		updates := h.watchRun(ctx, "turn-1")
		if u := h.nextRunUpdate(updates); u.Found {
			h.t.Fatalf("the first update of a run nobody created = %+v, want no record", u)
		}

		h.createRun("turn-1", `{"status":"launching"}`)
		created, _ := h.run("turn-1")
		if u := h.nextRunUpdate(updates); !u.Found || string(u.Record.Value) != `{"status":"launching"}` ||
			u.Record.Version != created.Version || u.Record.Key != "turn-1" {
			h.t.Fatalf("update after the create = %+v, want the record as stored (%+v)", u, created)
		}

		written, _ := h.putRun("turn-1", `{"status":"running"}`)
		if u := h.nextRunUpdate(updates); !u.Found || string(u.Record.Value) != `{"status":"running"}` ||
			u.Record.Version != written.Version {
			h.t.Fatalf("update after a mutation = %+v, want the record it wrote (%+v)", u, written)
		}

		// A run watched while it exists starts from it.
		second := h.watchRun(ctx, "turn-1")
		if u := h.nextRunUpdate(second); !u.Found || u.Record.Version != written.Version {
			h.t.Fatalf("the first update of a watch of a live run = %+v, want its record (%+v)", u, written)
		}

		if !h.removeRun("turn-1") {
			h.t.Fatal("the run could not be removed")
		}
		for _, w := range []<-chan coord.SandboxRunUpdate{updates, second} {
			if u := h.nextRunUpdate(w); u.Found {
				h.t.Fatalf("update after the delete = %+v, want the record gone", u)
			}
		}

		h.createRun("turn-1", `{"status":"launching","again":true}`)
		if u := h.nextRunUpdate(updates); !u.Found || string(u.Record.Value) != `{"status":"launching","again":true}` {
			h.t.Fatalf("update after the run was created again = %+v, want its new record", u)
		}

		// Ending the watch closes the channel: a consumer can tell a
		// watch that stopped from one that is merely quiet.
		cancel()
		h.awaitClosed(updates)
		h.awaitClosed(second)
	},
}, {
	// A WATCH IS OF ONE RUN. A busy fleet writes every run's record on every
	// bridged call, and a watch that heard all of them would be one node
	// reading every run to serve one.
	name: "a watch hears its own run and no other",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running"}`)
		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		updates := h.watchRun(ctx, "turn-1")
		h.nextRunUpdate(updates)

		h.createRun("turn-2", `{"status":"running"}`)
		h.putRun("turn-2", `{"status":"resumed"}`)
		h.removeRun("turn-2")
		h.putRun("turn-1", `{"status":"resumed"}`)
		if u := h.nextRunUpdate(updates); u.Record.Key != "turn-1" || string(u.Record.Value) != `{"status":"resumed"}` {
			h.t.Fatalf("the next update = %+v, want turn-1's own change and nothing of turn-2's", u)
		}
	},
}, {
	// A CLOSED CHANNEL IS NOT A DELETE, and not a resume either: the watcher
	// stopped hearing, and a change made while nobody listened has to reach
	// the watch that replaces it — as its first update, the state then.
	name: "a watch that stopped hearing is watched again without losing a change",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running"}`)
		ctx, cancel := context.WithCancel(h.ctx)
		updates := h.watchRun(ctx, "turn-1")
		if u := h.nextRunUpdate(updates); !u.Found {
			h.t.Fatalf("first update = %+v, want the run's record", u)
		}
		cancel()
		h.awaitClosed(updates)

		written, _ := h.putRun("turn-1", `{"status":"awaiting"}`)

		again := h.watchRun(h.ctx, "turn-1")
		if u := h.nextRunUpdate(again); !u.Found || string(u.Record.Value) != `{"status":"awaiting"}` ||
			u.Record.Version != written.Version {
			h.t.Fatalf("the first update of the watch after the gap = %+v, want the change made "+
				"while nobody listened (%+v)", u, written)
		}
	},
}, {
	// Every listing this serves filters on fields coordination cannot see —
	// the seat, the status, the conversation key, the pause instant — so
	// there is one read and the caller decodes. Ordering by turn id is part
	// of the contract so two backends answer a recovery pass the same way.
	name: "every run is listed, by turn id",
	fn: func(h *fleetHarness) {
		h.createRun("turn-b", `{"seat":"cto"}`)
		h.createRun("turn-a", `{"seat":"ceo"}`)
		h.createRun("turn-c", `{"seat":"cto"}`)

		records, err := h.f.SandboxRuns(h.ctx)
		if err != nil {
			h.t.Fatalf("SandboxRuns: %v", err)
		}
		var keys []string
		for _, r := range records {
			keys = append(keys, r.Key)
		}
		if !slices.Equal(keys, []string{"turn-a", "turn-b", "turn-c"}) {
			h.t.Errorf("runs = %v, want every one of them in turn-id order", keys)
		}
		for _, r := range records {
			if r.Version == 0 || len(r.Value) == 0 {
				h.t.Errorf("listed run %s came back without its value or version: %+v", r.Key, r)
			}
		}
	},
}, {
	name: "a caller mutating a listed value cannot reach the store",
	fn: func(h *fleetHarness) {
		h.createRun("turn-1", `{"status":"running"}`)
		record, _ := h.run("turn-1")
		for i := range record.Value {
			record.Value[i] = 'x'
		}
		again, _ := h.run("turn-1")
		if string(again.Value) != `{"status":"running"}` {
			h.t.Errorf("the store took a caller's mutation: %q", again.Value)
		}
	},
}, {
	name: "an unnamed run is an error, not a lost race",
	fn: func(h *fleetHarness) {
		created, err := h.f.CreateSandboxRun(h.ctx, "", []byte(`{}`))
		if err == nil {
			h.t.Error("CreateSandboxRun accepted a run with no turn id")
		}
		if created {
			h.t.Error("CreateSandboxRun created a run with no turn id")
		}
		_, changed, err := h.f.MutateSandboxRun(h.ctx, "", func(coord.Record) (coord.SandboxRunChange, error) {
			h.t.Error("a decision was asked about a run with no turn id")
			return coord.SandboxRunChange{}, nil
		})
		if err == nil || changed {
			h.t.Errorf("MutateSandboxRun with no turn id = (%v, %v), want an error", changed, err)
		}
		if _, err := h.f.WatchSandboxRun(h.ctx, ""); err == nil {
			h.t.Error("WatchSandboxRun watched a run with no turn id")
		}
	},
}}
