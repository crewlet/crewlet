package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executorkeep"
)

// crossingWrites is a pending store whose connection takes crossing to carry a
// run's row at its bound ([PendingStore.RowCrossing]), and which records, for
// each write of a row a case watches, the instant it was handed the write and
// its context's deadline, on the case's clock. Every other call is the store
// underneath's.
//
// An ending's decision takes decide on the case's clock and is declined, so a
// path that decides one ends there, and the next write it makes comes that much
// later — unless decides says the store decides it, as the store underneath
// does. Every call it notes then takes pace on the case's clock before the
// store underneath answers it: the time its bytes take to cross.
type crossingWrites struct {
	PendingStore
	crossing     time.Duration
	readCrossing time.Duration
	decide       time.Duration
	decides      bool
	pace         time.Duration

	mu    sync.Mutex
	given map[string][]handed
}

// handed is one write as [crossingWrites] saw it.
type handed struct {
	at, deadline time.Time
	set          bool
}

func (s *crossingWrites) RowCrossing() time.Duration { return s.crossing }

func (s *crossingWrites) RowReadCrossing() time.Duration { return s.readCrossing }

// note records the write named write, handed ctx now, and then takes the
// store's pace over it.
func (s *crossingWrites) note(ctx context.Context, write string) {
	deadline, set := ctx.Deadline()
	s.mu.Lock()
	s.given[write] = append(s.given[write], handed{at: time.Now(), deadline: deadline, set: set})
	s.mu.Unlock()
	time.Sleep(s.pace) //fixedwait:simulated a call's bytes crossing a slow link, on the case's bubble clock
}

// forget drops every call noted so far: what a case's setup made.
func (s *crossingWrites) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.given = map[string][]handed{}
}

func (s *crossingWrites) writes(write string) []handed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]handed(nil), s.given[write]...)
}

func (s *crossingWrites) MarkResumeBegun(ctx context.Context, turnID, launch string, fence Fence,
	carry ResumeCarry) (bool, error) {
	s.note(ctx, "MarkResumeBegun")
	return s.PendingStore.MarkResumeBegun(ctx, turnID, launch, fence, carry)
}

func (s *crossingWrites) StampCapture(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	s.note(ctx, "StampCapture")
	return s.PendingStore.StampCapture(ctx, turnID, capture)
}

func (s *crossingWrites) OweEnd(ctx context.Context, turnID string, ending json.RawMessage) (PendingRun, bool, error) {
	s.note(ctx, "OweEnd")
	return s.PendingStore.OweEnd(ctx, turnID, ending)
}

func (s *crossingWrites) MarkHarnessEnd(ctx context.Context, turnID, launch string, read EndRead) (bool, error) {
	s.note(ctx, "MarkHarnessEnd")
	return s.PendingStore.MarkHarnessEnd(ctx, turnID, launch, read)
}

func (s *crossingWrites) MarkEndedAtDeadline(ctx context.Context, turnID, launch string) (bool, error) {
	s.note(ctx, "MarkEndedAtDeadline")
	return s.PendingStore.MarkEndedAtDeadline(ctx, turnID, launch)
}

func (s *crossingWrites) ReleaseClaim(ctx context.Context, turnID string, release Release) (bool, error) {
	s.note(ctx, "ReleaseClaim")
	return s.PendingStore.ReleaseClaim(ctx, turnID, release)
}

func (s *crossingWrites) ReleaseBox(ctx context.Context, turnID string) error {
	s.note(ctx, "ReleaseBox")
	return s.PendingStore.ReleaseBox(ctx, turnID)
}

func (s *crossingWrites) DecideEnding(ctx context.Context, turnID string, d Decision) (PendingRun, bool, error) {
	s.note(ctx, "DecideEnding")
	if s.decides {
		return s.PendingStore.DecideEnding(ctx, turnID, d)
	}
	time.Sleep(s.decide) //fixedwait:simulated a decision the store takes a while over, on the case's bubble clock
	return PendingRun{}, false, nil
}

func (s *crossingWrites) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	s.note(ctx, "Get")
	return s.PendingStore.Get(ctx, turnID)
}

func (s *crossingWrites) TurnLog(ctx context.Context, run PendingRun) (BridgeLog, error) {
	s.note(ctx, "TurnLog")
	return s.PendingStore.TurnLog(ctx, run)
}

func (s *crossingWrites) Finish(ctx context.Context, turnID, ending string) (PendingRun, bool, error) {
	s.note(ctx, "Finish")
	return s.PendingStore.Finish(ctx, turnID, ending)
}

func (s *crossingWrites) ExpirePause(ctx context.Context, turnID string) (bool, error) {
	s.note(ctx, "ExpirePause")
	return s.PendingStore.ExpirePause(ctx, turnID)
}

func (s *crossingWrites) MarkLost(ctx context.Context, turnID, launch string, loss JobLoss) (bool, error) {
	s.note(ctx, "MarkLost")
	return s.PendingStore.MarkLost(ctx, turnID, launch, loss)
}

// EVERY WRITE OF A RUN'S ROW UNDER A BUDGET OF ITS CALLER'S IS GIVEN THE ROW'S
// CROSSING BESIDE ITS REASON, and each keeps its reason.
//
// A row is written whole, up to six megabytes, at a slow link's pace; the
// store keeps a caller's deadline as it is, so every caller bounding a row
// write for a reason of its own (a carry, a stamp, a teardown) adds the time a
// row at its bound takes to cross (RowBudget). Each teardown write takes its
// own bound, and a read's stamp is still held to the read's room. Exact, in a
// bubble.
//
// Mutation: hold a write to its reason alone, share one teardown bound across
// the runs a stop ends, or give a stamp the crossing past the read's room.
func TestEveryWriteOfARunsRowIsGivenTheRowsCrossingBesideItsReason(t *testing.T) {
	t.Parallel()
	const crossing = 7 * time.Minute
	stamp := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureBegun}
	for _, c := range []struct {
		name string
		// write is the store call the path makes, and want what each of
		// those calls is given, in order.
		write string
		want  []time.Duration
		// runs is how many of the seat's runs the case launches first, one
		// where unset; path takes it on the first.
		runs int
		path func(ctx context.Context, c *Coordinator, run PendingRun)
	}{
		{
			name: "a resumed turn's carry", write: "MarkResumeBegun",
			want: []time.Duration{resumeCarryRecordBudget + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.markResumeBegun(ctx, run, ResumeCarriesOn)
			},
		},
		{
			name: "a capture's stamp off a read", write: "StampCapture",
			want: []time.Duration{sessionRecordBudget + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.stampSettled(ctx, time.Time{}, run.TurnID, stamp)
			},
		},
		{
			name: "a capture's stamp within a read's room", write: "StampCapture",
			want: []time.Duration{time.Minute},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.stampSettled(ctx, time.Now().Add(time.Minute), run.TurnID, stamp)
			},
		},
		{
			name: "a turn's owed end", write: "OweEnd",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				_, _ = c.OweEnd(ctx, run.TurnID, executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted})
			},
		},
		{
			name: "a job ended at its turn's cap", write: "MarkEndedAtDeadline",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.markEndedAtDeadline(ctx, run)
			},
		},
		{
			name: "how a collected job's harness ended", write: "MarkHarnessEnd",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.markHarnessEnd(ctx, run, EndRead{End: HarnessEndOwnRun})
			},
		},
		{
			name: "a claim handed back", write: "ReleaseClaim",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				run.Status, run.ClaimedFrom = StatusResumed, StatusRunning
				_ = c.unclaimAt(ctx, run, "", failure{}, nil, BudgetWindow{})
			},
		},
		{
			name: "a box let go", write: "ReleaseBox",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.teardown(ctx, run)
			},
		},
		{
			name: "a claim's ending", write: "DecideEnding",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				_, _ = c.endClaim(ctx, run, ending{})
			},
		},
		{
			name: "a run's ending", write: "DecideEnding",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.finish(ctx, run, ending{fence: fenceOf(run)})
			},
		},
		{
			name: "an abandoned launch's ending", write: "DecideEnding",
			want: []time.Duration{discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				c.abandon(ctx, c.mgr(), LaunchRequest{
					Turn: TurnRef{TurnID: run.TurnID, AgentHandle: run.AgentHandle}}, fenceOf(run), "")
			},
		},
		{
			name: "every run a person's stop ends", write: "DecideEnding", runs: 2,
			want: []time.Duration{discardGrace + crossing, discardGrace + crossing},
			path: func(ctx context.Context, c *Coordinator, run PendingRun) {
				_ = c.StopSeat(ctx, run.AgentHandle, RunStop{By: "ana"})
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				var store *crossingWrites
				rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
					store = &crossingWrites{PendingStore: o.Pending, crossing: crossing,
						decide: time.Minute, given: map[string][]handed{}}
					o.Pending = store
				})
				run := rig.launch("t1")
				for n := 2; n <= c.runs; n++ {
					rig.launch(fmt.Sprintf("t%d", n))
				}
				c.path(t.Context(), rig.coordinator, run)
				got := store.writes(c.write)
				if len(got) != len(c.want) {
					t.Fatalf("%s was asked %d times, want %d", c.write, len(got), len(c.want))
				}
				for i, w := range got {
					if given := w.deadline.Sub(w.at); !w.set || given != c.want[i] {
						t.Errorf("%s #%d was given %v (set %t), want %v: its reason and the crossing of "+
							"a row at its bound, %v", c.write, i+1, given, w.set, c.want[i], crossing)
					}
				}
			})
		})
	}
}

// A ROW'S WRITE IN THE STORE'S OWN PROCESS IS GIVEN ITS REASON ALONE.
//
// The crossing is the store's to say, and the store in a node's own process —
// the in-memory twin a single node runs — says none, so every budget a row's
// write is held to there is the reason it always was.
//
// Mutation: have the twin say a crossing, or count one in RowBudget whatever
// the store says, and a single node's budgets grow by minutes for bytes that
// cross nothing.
func TestARowsWriteInTheStoresOwnProcessIsGivenItsReasonAlone(t *testing.T) {
	t.Parallel()
	store := NewCoordStore(memory.NewFleet())
	if got := store.RowCrossing(); got != 0 {
		t.Errorf("the store in the node's own process says a row takes %v to cross, want none", got)
	}
	if got := RowBudget(store, sessionRecordBudget); got != sessionRecordBudget {
		t.Errorf("RowBudget in process = %v, want the reason alone, %v", got, sessionRecordBudget)
	}
	// AND THE READ STANDING BY, which any node may be, is held to the
	// slowest node's record whatever this node's store says: its window
	// leaves, past the capture it stands by for and the read that sees how
	// it came out, room for the record's reason and a row's crossing at the
	// slowest pace a link is allowed.
	if room := standbyWindow - captureLife - captureWaitTick; room < sessionRecordBudget+coord.MaxValueCrossing {
		t.Errorf("a read standing by leaves %v for its record of the next place, want at least the "+
			"record's %v and the slowest node's crossing of a row, %v", room, sessionRecordBudget,
			coord.MaxValueCrossing)
	}
}

// EVERY CALL AN ENDING MAKES OF THE STORE ON ITS RUN'S ROW IS GIVEN THE
// TEARDOWN'S GRACE AND THE ROW'S CROSSING OF ITS OWN.
//
// An ending calls the store on the row several times (decision, reads, the
// delete), each carrying the row whole, which across a slow link takes the
// crossing every time. One bound for the ending gave the first call the time
// the next needed, cutting an ending whose calls would each have landed. Here
// each call takes a minute to cross, and every call is still handed the grace
// and the crossing whole. Exact, in a bubble.
//
// Mutation: bound the ending once, by [Coordinator.finish] or
// [Coordinator.endRun], and each call is handed a minute less than the last.
func TestEveryCallAnEndingMakesOfItsRowIsGivenItsOwnCrossing(t *testing.T) {
	t.Parallel()
	const crossing = 7 * time.Minute
	synctest.Test(t, func(t *testing.T) {
		var store *crossingWrites
		rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
			store = &crossingWrites{PendingStore: o.Pending, crossing: crossing, decides: true,
				pace: time.Minute, given: map[string][]handed{}}
			o.Pending = store
		})
		run := rig.launch("t1")
		store.forget()
		if _, ours := rig.coordinator.finish(t.Context(), run, ending{fence: fenceOf(run)}); !ours {
			t.Fatal("the run's ending was not this node's")
		}
		if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
			t.Fatalf("the ending left the run's record (%v), want it finished", err)
		}
		for _, call := range []string{"DecideEnding", "Get", "TurnLog", "Finish"} {
			made := store.writes(call)
			if len(made) == 0 {
				t.Errorf("the ending made no %s, want each of its calls of the row watched", call)
			}
			for i, w := range made {
				if given := w.deadline.Sub(w.at); !w.set || given != discardGrace+crossing {
					t.Errorf("the ending's %s #%d was handed %v (set %t), want the teardown's grace and "+
						"the crossing of a row at its bound, %v, of its own", call, i+1, given, w.set,
						discardGrace+crossing)
				}
			}
		}
	})
}

// unansweredStamps is a pending store whose connection takes crossing to carry
// a run's row at its bound, and which never answers a stamp of what came of a
// capture, as a store whose row is still crossing a slow link does not: the
// stamp waits until its context ends. A capture's reservation of its place is
// the store's underneath.
type unansweredStamps struct {
	PendingStore
	crossing time.Duration
}

func (s *unansweredStamps) RowCrossing() time.Duration { return s.crossing }

func (s *unansweredStamps) StampCapture(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	if stamp.State == CaptureBegun {
		return s.PendingStore.StampCapture(ctx, turnID, stamp)
	}
	<-ctx.Done()
	return false, ctx.Err()
}

// A STOP HOLDS THE STAMPS OF THE KEEPS IT CUTS TO THEIR REASON, NEVER TO A ROW'S
// CROSSING.
//
// A stop lets keeps under way end for a teardown's grace, cuts the rest, and
// waits for each to stamp before giving its taker lease back. A stamp given
// the row's crossing (thirteen minutes on the slowest link) held a node
// without `data` past its orchestrator's grace, which killed it before it
// closed its store. So once the coordinator stops, a stamp is held to its
// reason alone; the stamp left begun is settled by a peer. Exact, in a bubble.
//
// Mutation: give a stamp its crossing at a stop too, and the stop returns
// seven minutes later.
func TestAStopHoldsTheStampsOfTheKeepsItCutsToTheirReason(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, keep := keepingRigWith(t, func(o *CoordinatorOptions) {
			o.Pending = &unansweredStamps{PendingStore: o.Pending, crossing: 7 * time.Minute}
		})
		rig.coordinator.recordBudget = sessionRecordBudget
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		keep.gate = make(chan struct{})
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		collectLeaving(t, rig, "t1")
		synctest.Wait()
		if c := rig.get("t1").Capture; c == nil || c.State != CaptureBegun {
			t.Fatalf("the premise: the collection's capture stands %+v, want it begun with its keep "+
				"uploading", c)
		}
		start := time.Now()
		rig.coordinator.Stop()
		if took, want := time.Since(start), sessionKeepDrain+sessionRecordBudget; took != want {
			t.Errorf("the stop returned %v after it began, want the keeps' grace and the stamp's reason, %v, "+
				"whatever the row's crossing", took, want)
		}
	})
}

// A STOP HOLDS A RETRY PART-WAY THROUGH A LOST RUN'S TAKE TO ONE GRACE, ALL OF
// IT.
//
// A retry finishing a lost run's kept ending takes the run's session before it
// reclaims the box, on contexts the stop's cancellation does not reach (the
// place's reservation, the refused stamp, the reclaim's kill). Each held to its
// own grace, against a store and box that never answer, they ran one after
// another: ninety seconds where the stop gives thirty. So all the stopped work
// is held to one grace from the stop ([detach]). Exact, in a bubble.
//
// Mutation: detach a teardown's or stamp's context with context.WithoutCancel
// again, or start the grace at each call's own start.
func TestAStopHoldsARetryPartWayThroughALostRunsTakeToOneGrace(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		stamps := &silentStamps{}
		rig, _, _ := keepingRigWith(t, func(o *CoordinatorOptions) {
			stamps.PendingStore = o.Pending
			o.Pending = stamps
		})
		rig.coordinator.recordBudget = sessionRecordBudget
		rig.launching("t1")
		rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
		if err := rig.recoverSeat(t.Context(), "node-b:1", 9); err != nil {
			t.Fatalf("RecoverSeat: %v", err)
		}
		// THE BOX NEVER ANSWERS ITS RECLAIM, as the store never answers a
		// stamp.
		rig.provider.KillFunc = func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		}
		retried := make(chan int, 1)
		go func() { retried <- rig.fireRetries() }()
		synctest.Wait()
		if n := stamps.asked.Load(); n != 1 {
			t.Fatalf("the premise: the retry's take asked the store for its capture's place %d times, "+
				"want once, unanswered", n)
		}
		start := time.Now()
		rig.coordinator.Stop()
		if took := time.Since(start); took != discardGrace {
			t.Errorf("the stop returned %v after it began, want one teardown's grace, %v, for all the "+
				"retry made after it", took, discardGrace)
		}
		if n := <-retried; n != 1 {
			t.Errorf("fired %d retries, want the lost run's ending's one", n)
		}
	})
}

// A WAITER'S WRITE OF A RUN'S ROW IS GIVEN THE ROW'S CROSSING PAST ITS TASK'S
// BOUND.
//
// The reaper's flip of a parked run past its pause, and the fleet's verdicts
// on a run whose launch node is gone, each write the run's row whole. A task
// is bounded by a box's poll or the duty's window, neither a row's: held to
// that alone, a row of megabytes was cut mid-crossing and resent from its
// first byte by the next pass, for ever. Each is a compare-and-set, so one
// past the window beside a peer's changes nothing twice. Exact, in a bubble.
//
// Mutation: hold the flip, or either verdict, to its task's bound alone, and
// its row reads the crossing short.
func TestAWaitersWriteOfARunsRowIsGivenTheRowsCrossingPastItsTask(t *testing.T) {
	t.Parallel()
	const crossing = 7 * time.Minute
	for _, c := range []struct {
		name, write string
		// park says the run asks a question and parks, its box paused,
		// before the case's task.
		park bool
		task func(ctx context.Context, w *Waiter, rig *waiterRig, run PendingRun)
	}{
		{
			name: "the reaper's flip of a pause past its TTL", write: "ExpirePause", park: true,
			task: func(ctx context.Context, w *Waiter, rig *waiterRig, run PendingRun) {
				w.reapOne(ctx, rig.manager, withPaused(run, rig.now.Add(-DefaultPauseTTL-time.Second)))
			},
		},
		{
			name: "a gone node's parked run, its box released", write: "ExpirePause", park: true,
			task: func(ctx context.Context, w *Waiter, rig *waiterRig, run PendingRun) {
				w.endGone(ctx, rig.getIn(ctx, run.TurnID), rig.now.Add(-time.Hour), rig.now)
			},
		},
		{
			name: "a gone node's running job, lost", write: "MarkLost",
			task: func(ctx context.Context, w *Waiter, rig *waiterRig, run PendingRun) {
				w.endGone(ctx, run, rig.now.Add(-time.Hour), rig.now)
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				rig := newWaiterRig(t)
				run := rig.launch("t1")
				if c.park {
					rig.park("t1")
				}
				store := &crossingWrites{PendingStore: rig.pending, crossing: crossing, given: map[string][]handed{}}
				w, err := NewWaiter(WaiterOptions{Queue: rig.queue, Pending: store, Manager: rig.managers,
					Drives: DrivesRemote, Now: func() time.Time { return rig.now }})
				if err != nil {
					t.Fatalf("NewWaiter: %v", err)
				}
				task, stop := context.WithTimeout(t.Context(), pollBound)
				defer stop()
				bound, _ := task.Deadline()
				c.task(task, w, rig, run)
				made := store.writes(c.write)
				if len(made) != 1 {
					t.Fatalf("%s was asked %d times, want once", c.write, len(made))
				}
				if !made[0].set || !made[0].deadline.Equal(bound.Add(crossing)) {
					t.Errorf("%s was handed until %v past its task's start (set %t), want its task's bound and the "+
						"crossing of a row at its bound, %v", c.write, made[0].deadline.Sub(made[0].at), made[0].set,
						pollBound+crossing)
				}
			})
		})
	}
}

// A BOX READ GIVES EACH CHANGE OF THE RUN'S ROW ITS HOST MAKES ITS CROSSING,
// AND ITS READ OF THE ROW A READ'S: the host on its own connection, the asker
// at the slowest node's.
//
// A read of a job's box reserves its capture's place, stamps the take's outcome
// and how the harness ended, and records the pause, each a change crossing the
// row in minutes on the slowest link: each gets a reason or a grace and a
// crossing, the asker coord.MaxValueCrossing for each; the row's read before
// the take gets a reason and its reply's crossing alone. Exact, bubbled.
//
// Mutation: drop the crossing from any room, count fewer changes or reasons in
// the asker's budget, or count the row's read as a change.
func TestABoxReadGivesEachChangeOfTheRowItsHostMakesItsCrossing(t *testing.T) {
	t.Parallel()
	const crossing = 7 * time.Minute
	run := PendingRun{TurnID: "t1"}
	held := SessionTakeBudget + 2*sessionRecordBudget + coord.MaxValueReadCrossing + 3*discardGrace +
		4*coord.MaxValueCrossing
	if got, want := BoxReadBudget(run, BoxReadCollect, ThreadTraits{}),
		pollBound+time.Duration(float64(held)/BoxReadAnswerShare); got != want {
		t.Errorf("a collection of a turn with no cap waits %v, want the poll's bound and, at the host's "+
			"share, the take, the reasons of its read of the row and its reservation, that read's reply "+
			"and four changes of the row at the slowest node's crossing, and three graces, %v", got, want)
	}
	inBubble(t, func(t *testing.T) {
		rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
			o.Pending = &crossingWrites{PendingStore: o.Pending, crossing: crossing,
				given: map[string][]handed{}}
		})
		// THE HOST'S SHARE OF WHAT THE ASKER WAITS: the take is given its own
		// budget and its reservation's room beside it, whole.
		share := time.Duration(float64(BoxReadBudget(run, BoxReadCollect, ThreadTraits{})) * BoxReadAnswerShare)
		read, cancel := context.WithDeadline(t.Context(), time.Now().Add(share))
		defer cancel()
		take, cancelTake, _ := rig.coordinator.takeContext(read)
		defer cancelTake()
		deadline, _ := take.Deadline()
		if got, want := time.Until(deadline), SessionTakeBudget+sessionRecordBudget+crossing; got != want {
			t.Errorf("a take on a host whose store takes %v to cross is given %v, want its own budget, "+
				"its reservation's reason and that crossing beside it, %v", crossing, got, want)
		}
		// A READ TOO SHORT FOR ALL OF IT: the take ends where its records
		// keep their room, and the records where the pause and its record
		// keep theirs.
		short, cancelShort := context.WithDeadline(t.Context(), time.Now().Add(SessionTakeBudget))
		defer cancelShort()
		take, cancelTake, by := rig.coordinator.takeContext(short)
		defer cancelTake()
		deadline, _ = take.Deadline()
		end, _ := short.Deadline()
		if got, want := by.Sub(deadline), 2*discardGrace+2*crossing; got != want {
			t.Errorf("a take's records are left %v after it, want a grace and a crossing for each of "+
				"its two changes of the row, %v", got, want)
		}
		if got, want := end.Sub(by), discardGrace+crossing; got != want {
			t.Errorf("a read's pause and its record are left %v, want a grace and a crossing, %v", got, want)
		}
	})
}

// unansweredReads is a pending store whose connection takes crossing to carry
// a change of a run's row at its bound and readCrossing a read's reply, and
// whose first fails reads of a run's row are not answered, as a store in an
// outage's moment does not: each is refused at once. It notes the context's
// deadline of every read asked, on the case's clock.
type unansweredReads struct {
	PendingStore
	crossing, readCrossing time.Duration
	fails                  int

	mu    sync.Mutex
	asked []handed
}

func (s *unansweredReads) RowCrossing() time.Duration { return s.crossing }

func (s *unansweredReads) RowReadCrossing() time.Duration { return s.readCrossing }

func (s *unansweredReads) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	deadline, set := ctx.Deadline()
	s.mu.Lock()
	s.asked = append(s.asked, handed{at: time.Now(), deadline: deadline, set: set})
	unanswered := len(s.asked) <= s.fails
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return PendingRun{}, false, err
	}
	if unanswered {
		return PendingRun{}, false, errors.New("the coordination store did not answer")
	}
	return s.PendingStore.Get(ctx, turnID)
}

func (s *unansweredReads) reads() []handed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]handed(nil), s.asked...)
}

// A TAKE'S READ OF ITS ROW IS ASKED AGAIN IN A ROOM OF ITS OWN, ENDING WHERE THE
// TAKE MUST ([Coordinator.placingRow]): a READ'S ROOM, a record's reason and
// the reply's crossing alone ([RowReadBudget]), never a change's, from its
// first ask, which its take's budget begins after; never past the instant the
// take ends by ([Coordinator.takeBy]), so the read's records keep their room;
// and never past the read it is part of, which nothing it reads outlives.
// Exact, bubbled.
//
// Mutation: ask the row once, read it in a context of its own
// ([Coordinator.settleContext]) or a change's room ([Coordinator.rowContext]),
// or bound it by the records' end rather than the take's.
func TestATakesReadOfItsRowIsAskedAgainWithinItsOwnRoom(t *testing.T) {
	t.Parallel()
	const crossing, readCrossing = 7 * time.Minute, 3 * time.Minute
	room := sessionRecordBudget + readCrossing
	for _, c := range []struct {
		name  string
		fails int
		// left is how long the read the take is part of has, none for a read
		// with no deadline; cut, when the read is cut short.
		left, cut time.Duration
		// answered is whether the row's read answers; ends, when it returns,
		// and every ask's deadline, from its first.
		answered        bool
		ends, deadlines time.Duration
	}{
		{name: "a store that answers on its third ask", fails: 2, answered: true, ends: 3 * time.Second,
			deadlines: room},
		{name: "a store that never answers", fails: 1 << 20, ends: room, deadlines: room},
		{name: "a read too short for the row's room", fails: 1 << 20,
			left: discardGrace + crossing + 2*discardGrace + 2*crossing + 2*time.Minute,
			ends: 2 * time.Minute, deadlines: 2 * time.Minute},
		{name: "a read cut short", fails: 1 << 20, cut: time.Minute, ends: time.Minute, deadlines: room},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				var store *unansweredReads
				rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
					store = &unansweredReads{PendingStore: o.Pending, crossing: crossing,
						readCrossing: readCrossing, fails: c.fails}
					o.Pending = store
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if c.left > 0 {
					ctx, cancel = context.WithTimeout(ctx, c.left)
					defer cancel()
				}
				if c.cut > 0 {
					time.AfterFunc(c.cut, cancel)
				}
				start := time.Now()
				_, _, err := rig.coordinator.placingRow(ctx, rig.coordinator.recordsBy(ctx), "t1")
				if answered := err == nil; answered != c.answered {
					t.Errorf("the row's read answered %t (%v), want %t", answered, err, c.answered)
				}
				if took := time.Since(start); took != c.ends {
					t.Errorf("the row's read returned %v after it began, want %v", took, c.ends)
				}
				asked := store.reads()
				if len(asked) < 2 {
					t.Fatalf("the row was read %d times, want it asked again while the store did not answer",
						len(asked))
				}
				for i, a := range asked {
					if !a.set || !a.deadline.Equal(start.Add(c.deadlines)) {
						t.Errorf("ask %d of %d of the row was handed until %v past the read's start (set %t), "+
							"want %v", i+1, len(asked), a.deadline.Sub(start), a.set, c.deadlines)
						break
					}
				}
			})
		})
	}
}
