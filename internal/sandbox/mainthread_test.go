package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
)

// countingThread is a [FakeRunner.MainThread] that answers count, its
// provider and next, and records what it was asked, in order.
type countingThread struct {
	mu       sync.Mutex
	count    *ResumedCount
	provider string
	next     NextRequest
	asked    []MainThreadAsk
}

func (c *countingThread) read(_ context.Context, _ Sandbox, ask MainThreadAsk) MainThreadReading {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, ask)
	if c.count == nil {
		return MainThreadReading{Next: c.next}
	}
	counted := *c.count
	return MainThreadReading{Count: &counted, Provider: c.provider, Next: c.next}
}

func (c *countingThread) asks() []MainThreadAsk {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]MainThreadAsk(nil), c.asked...)
}

// A COLLECTION RECORDS THE COUNT OF THE COPY ITS BOX HOLDS: a Claude Code job's
// collection reads the main thread once and writes the count, beside the least
// window the run reported, in the same write as how its harness ended, whether
// the harness ended its own run or was cut off, since the count decides the
// copy's guard too. A stop's read and a loss's ask nothing: no relaunch carries
// their round on in place.
//
// Mutation: leave the count out of the record's write (markHarnessEnd handed no
// count), or ask the thread on every read, and a case goes red.
func TestACollectionRecordsTheCountOfTheCopyItsBoxHolds(t *testing.T) {
	t.Parallel()
	bound := &ResumedCount{Tokens: 42_000, Restart: new("")}
	for name, tc := range map[string]struct {
		read   BoxRead
		result Result
		want   *ResumedCount
	}{
		"an own run's collection": {read: BoxReadCollect,
			result: Result{Success: true, Text: "done", SessionID: "s-1", ReportedWindow: 200_000,
				Captured: &SessionArchive{SessionID: "s-1"}},
			want: &ResumedCount{Tokens: 42_000, Window: 200_000, Restart: new("")}},
		"a cut-off job's collection": {read: BoxReadCollect,
			result: Result{Success: true, Text: "done", SessionID: "s-1", HarnessCutOff: true},
			want:   &ResumedCount{Tokens: 42_000, Restart: new("")}},
		"a stop's read": {read: BoxReadStop, result: Result{Success: true, Text: "done", SessionID: "s-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			thread := &countingThread{count: bound}
			rig.runner.MainThread = thread.read
			rig.launch("t1")
			run := rig.get("t1")
			rig.runner.Finish(tc.result)
			if _, _, err := rig.coordinator.readBox(t.Context(), run, tc.read); err != nil {
				t.Fatalf("readBox: %v", err)
			}
			if got := rig.get("t1").Launch.ResumedCount; !got.same(tc.want) {
				t.Errorf("the job's record holds the count %+v, want %+v", got, tc.want)
			}
			if asked := len(thread.asks()); (tc.want != nil) != (asked == 1) || asked > 1 {
				t.Errorf("the read asked the thread %d times, want %v", asked, tc.want != nil)
			}
		})
	}
}

// THE COLLECTION READS AN OPENCODE COPY ITS JOB ENDED ITSELF: the read that
// collects an OpenCode job asks the export its script left as the job ended,
// once, for the count, its provider and the estimate of the next request; and a
// job whose harness did not end its own run (cut off, or one its script packed
// nothing for) is unread with no read, since its script exports the session
// after its harness stops and that export may lack what the copy holds. A
// stop's read asks nothing.
//
// Mutation: read a cut-off OpenCode job, or ask nothing of an OpenCode
// collection, and a case goes red.
func TestTheCollectionReadsAnOpenCodeCopyItsJobEndedItself(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	run := rig.get("t1")
	run.Harness = "opencode"
	rig.runner.Thread = testThreadTraits["opencode"]
	for name, tc := range map[string]struct {
		read  BoxRead
		at    capturePoint
		asked int
		want  *ResumedCount
	}{
		"its own run's collection": {read: BoxReadCollect, at: capturePoint{final: true}, asked: 1,
			want: &ResumedCount{Tokens: 42_000, Restart: new("msg_c")}},
		"a cut-off job's collection": {read: BoxReadCollect, at: capturePoint{final: true, cutOff: true},
			want: &ResumedCount{Mark: CountUnread}},
		"a stop's read": {read: BoxReadStop, at: capturePoint{final: true}},
	} {
		thread := &countingThread{count: &ResumedCount{Tokens: 42_000, Restart: new("msg_c")}, provider: "stub",
			next: NextRequest{Tokens: 42_000}}
		rig.runner.MainThread = thread.read
		got := rig.coordinator.readMainThread(t.Context(), run, nil, rig.runner, tc.read, tc.at, Result{})
		if !got.Count.same(tc.want) || len(thread.asks()) != tc.asked {
			t.Errorf("%s: the read counted %+v asking %d times, want %+v asking %d", name, got.Count,
				len(thread.asks()), tc.want, tc.asked)
		}
		if wantProvider := tc.asked == 1; (got.Provider == "stub") != wantProvider {
			t.Errorf("%s: the read names the provider %q", name, got.Provider)
		}
	}
}

// A COUNT OF A JOB NOT OVER IS UNREAD: a job its box would not end may still write
// its thread, so the read asks nothing of it and the count says the thread was
// not read whole, which reads as a copy that may compact. A runner that reads no
// thread, and one that answers nothing, are unread too.
//
// Mutation: read the thread of a job not over, or take no answer for a bound,
// and a case goes red.
func TestACountOfAJobNotOverIsUnread(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	thread := &countingThread{count: &ResumedCount{Tokens: 42_000, Restart: new("")}}
	rig.runner.MainThread = thread.read
	rig.launch("t1")
	run := rig.get("t1")
	got := rig.coordinator.readMainThread(t.Context(), run, nil, rig.runner, BoxReadCollect,
		capturePoint{final: false}, Result{})
	if got.Count == nil || got.Count.Mark != CountUnread || len(thread.asks()) != 0 {
		t.Errorf("a job not over counted %+v, asking the thread %d times; want unread, asking nothing",
			got.Count, len(thread.asks()))
	}
	thread.count = nil
	got = rig.coordinator.readMainThread(t.Context(), run, nil, rig.runner, BoxReadCollect,
		capturePoint{final: true}, Result{})
	if got.Count == nil || got.Count.Mark != CountUnread {
		t.Errorf("a read that answered nothing counted %+v, want unread", got.Count)
	}
}

// AN UNBOUNDED COUNT IS LOGGED WITH WHY, BY ITS MARK ([unboundedBecause]): the
// log says the thread was read whole only for a mark its whole bytes cannot
// bound, and otherwise that it was not read whole, or not read at all.
//
// Mutation: log every mark as a thread read whole, and a case goes red.
func TestAnUnboundedCountIsLoggedWithWhyByItsMark(t *testing.T) {
	logs := captureLogs(t)
	rig := newCoordRig(t)
	rig.launch("t1")
	run := rig.get("t1")
	says := map[CountMark]string{CountUnread: "the thread was not read whole",
		CountRelease: "the thread was not read", CountPrecompact: "the thread was not read",
		CountMedia: "the thread was read whole"}
	for mark := range says {
		rig.runner.MainThread = (&countingThread{count: &ResumedCount{Mark: mark}}).read
		rig.coordinator.readMainThread(t.Context(), run, nil, rig.runner, BoxReadCollect, capturePoint{final: true},
			Result{})
	}
	logged := map[CountMark]string{}
	for line := range strings.Lines(logs.String()) {
		if !strings.Contains(line, "executor_session_count_unbounded") {
			continue
		}
		for mark := range says {
			if strings.Contains(line, "mark="+string(mark)+" ") {
				logged[mark] = line
			}
		}
	}
	for mark, want := range says {
		line, ok := logged[mark]
		switch {
		case !ok:
			t.Errorf("a count marked %s is not logged:\n%s", mark, logs.String())
		case !strings.Contains(line, want):
			t.Errorf("a count marked %s is logged without %q: %s", mark, want, line)
		case mark != CountMedia && strings.Contains(line, "the thread was read whole"):
			t.Errorf("a count marked %s is logged as of a thread read whole: %s", mark, line)
		}
	}
}

// THE MAIN THREAD IS READ BEFORE THE TAKE, WITHIN ITS WAIT: before the take,
// leaving the take's budget whole, and within [MainThreadReadWait] armed as the
// read starts, so in a synctest bubble the runner's deadline is the bubble's
// instant plus that wait and no session has been taken when it reads.
//
// Mutation: read the thread after the take, arm no wait, or build the
// coordinator with another wait, and a check goes red.
func TestTheMainThreadIsReadBeforeTheTakeWithinItsWait(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		var (
			deadline time.Time
			armed    bool
			taken    = -1
			at       time.Time
		)
		rig.runner.MainThread = func(ctx context.Context, _ Sandbox, _ MainThreadAsk) MainThreadReading {
			deadline, armed = ctx.Deadline()
			taken, at = len(rig.runner.Takes()), time.Now()
			return MainThreadReading{Count: &ResumedCount{Tokens: 1, Restart: new("")}}
		}
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
			t.Fatalf("readBox: %v", err)
		}
		rig.awaitKeeps()
		if taken != 0 {
			t.Errorf("the thread was read with %d sessions taken, want before the take", taken)
		}
		if !armed || !deadline.Equal(at.Add(MainThreadReadWait)) {
			t.Errorf("the read's deadline is %v (armed %v), want the instant it began plus its wait, %v",
				deadline, armed, at.Add(MainThreadReadWait))
		}
	})
}

// THE BOX READ'S BUDGET HOLDS THE MAIN THREAD'S READ: a Claude Code or an
// OpenCode collection, capped or not, waits the read's wait more at the host's
// share, as does a Claude Code stop's or loss's whose copy holds a hook's
// serve, and such a read the verdict's reason and a row's crossing too. A
// capped host may record the cap ending a job, so it is held with both, each
// against a run naming no harness. The docs' figures are held: 36 s more than
// a read of no thread, about 5,416 s uncapped and 6,690 s capped, composed.
//
// Mutation: add the wait to every read, none, or a Claude Code collection
// alone, ask a run naming no harness, drop the wait or the cap's record, change
// the wait, or drop the verdict's crossing or count it with no serve or for an
// OpenCode job, and a case goes red.
func TestTheBoxReadBudgetHoldsTheMainThreadRead(t *testing.T) {
	t.Parallel()
	share := time.Duration(float64(MainThreadReadWait) / BoxReadAnswerShare)
	// ANY INSTANT: the budget reads only whether the turn has a cap, since the
	// host decides on its own clock whether it has passed.
	capAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	claude, open := PendingRun{Harness: "claude-code"}, PendingRun{Harness: "opencode"}
	capped := PendingRun{Harness: "claude-code", Deadline: capAt}
	// EACH HARNESS AS ITS RUNNER STATES IT: hooks whose serves its thread shows,
	// and a plugin that confirms its own, each reading its thread.
	budget := func(run PendingRun, read BoxRead) time.Duration {
		return BoxReadBudget(run, read, testThreadTraits[run.Harness])
	}
	for name, tc := range map[string]struct {
		run  PendingRun
		read BoxRead
		more time.Duration
	}{
		"a Claude Code collection":                  {run: claude, read: BoxReadCollect, more: share},
		"an OpenCode collection":                    {run: open, read: BoxReadCollect, more: share},
		"an OpenCode job's stop":                    {run: open, read: BoxReadStop},
		"a Claude Code job's stop":                  {run: claude, read: BoxReadStop},
		"a Claude Code job's loss":                  {run: claude, read: BoxReadLoss},
		"a Claude Code collection of a capped turn": {run: capped, read: BoxReadCollect, more: share},
	} {
		named := tc.run
		named.Harness = ""
		if got, want := budget(tc.run, tc.read), budget(named, tc.read)+tc.more; got != want {
			t.Errorf("%s: the read's budget is %v, want %v", name, got, want)
		}
	}
	for _, run := range []PendingRun{claude, open} {
		if got := budget(run, BoxReadCollect) - budget(PendingRun{}, BoxReadCollect); got != 36*time.Second {
			t.Errorf("an uncapped %s collection waits %v more than one that reads no thread, want 36s",
				run.Harness, got)
		}
	}
	held := SessionTakeBudget + 2*sessionRecordBudget + coord.MaxValueReadCrossing + 3*discardGrace +
		boxReadChanges*coord.MaxValueCrossing + MainThreadReadWait
	for name, tc := range map[string]struct {
		run  PendingRun
		held time.Duration
	}{
		"an uncapped": {run: claude, held: held},
		// THE JOB'S END ON TOP, and the record that the cap ended a job its
		// host saw working: a teardown's grace and one more change of the row
		// at the slowest node's crossing.
		"a capped": {run: capped, held: held + JobEndBudget + discardGrace + coord.MaxValueCrossing},
	} {
		if got, want := budget(tc.run, BoxReadCollect),
			pollBound+time.Duration(float64(tc.held)/BoxReadAnswerShare); got != want {
			t.Errorf("%s turn's Claude Code collection's budget is %v, want %v", name, got, want)
		}
	}
	served := func(run PendingRun) PendingRun {
		run.LaunchID = "job-1"
		run.Steer.Notes = []RunNote{{ID: "n1", Serves: []HookServed{{Take: "take-n1", Launch: "job-1",
			Boundary: string(types.SteerBoundaryToolBatch)}}}}
		return run
	}
	verdictHeld := hookVerdictWait + coord.MaxValueCrossing
	verdict := time.Duration(float64(verdictHeld) / BoxReadAnswerShare)
	for name, tc := range map[string]struct {
		run  PendingRun
		read BoxRead
		more time.Duration
	}{
		"a Claude Code collection":                  {run: claude, read: BoxReadCollect, more: verdict},
		"a Claude Code collection of a capped turn": {run: capped, read: BoxReadCollect, more: verdict},
		"a Claude Code job's stop":                  {run: claude, read: BoxReadStop, more: share + verdict},
		"a Claude Code job's loss":                  {run: claude, read: BoxReadLoss, more: share + verdict},
		"an OpenCode collection":                    {run: open, read: BoxReadCollect},
	} {
		got, want := budget(served(tc.run), tc.read), budget(tc.run, tc.read)+tc.more
		if off := got - want; off < -time.Microsecond || off > time.Microsecond {
			t.Errorf("%s whose copy holds a serve: the read's budget is %v, want %v", name, got, want)
		}
	}
}

// A CLAIM IS TOLD THE COUNT THE READ MADE: the claim was read before the read
// wrote the job's record, and the resume on it judges a round carried on in the
// box by the claim's count, so the collection merges the read's count onto the
// claim by the record's rule.
//
// Mutation: leave the claim's count as it was read, and the resume reads none.
func TestAClaimIsToldTheCountTheReadMade(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.runner.MainThread = (&countingThread{count: &ResumedCount{Tokens: 42_000, Restart: new("")}}).read
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1"})
	var (
		mu      sync.Mutex
		claimed []*ResumedCount
	)
	rig.resumer.failWith(errors.New("held for the case to read the claim"))
	rig.resumer.during = func(_ context.Context, run PendingRun) {
		mu.Lock()
		defer mu.Unlock()
		claimed = append(claimed, run.Launch.ResumedCount)
	}
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Logf("OnCompleted: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := (&ResumedCount{Tokens: 42_000, Restart: new("")}); len(claimed) != 1 || !claimed[0].same(want) {
		t.Errorf("the resume on the claim read the counts %+v, want once %+v", claimed, want)
	}
}

// THE CAPTURE STAMPS ITS COUNT. A take that counts the session it keeps stamps the
// count on the capture beside its context, with the least window the job's run
// reported: what a launch that continues the capture judges its first request by.
//
// Mutation: stamp no count, or no window, and this goes red.
func TestTheCaptureStampsItsCount(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		session.Count = &ResumedCount{Tokens: 42_000, Restart: new("u-1")}
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive,
			ReportedWindow: 200_000})
		if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
			t.Fatalf("readBox: %v", err)
		}
		rig.awaitKeeps()
		run := rig.get("t1")
		want := &ResumedCount{Tokens: 42_000, Window: 200_000, Restart: new("u-1")}
		if run.Capture == nil || !run.Capture.ResumedCount.same(want) {
			t.Fatalf("the capture is stamped %+v, want the count %+v", run.Capture, want)
		}
	})
}

// AN OLDER BUILD'S WRITE CARRIES THE COUNT. A fleet mid-upgrade rewrites the job's
// record and the capture's stamp on a build that does not know the field, and a
// relaunch that reads them may run on this build: so the count, the provider
// beside it and the restart basis ride each record under their own keys, which
// a build that does not declare them keeps verbatim and writes back. The
// provider is a key of the record, never a field of the count, which is closed:
// inside it, an older build that knows the count would drop it on every write.
//
// Mutation: leave a field off the wire, or drop the carried keys from the
// encode, and a case goes red.
func TestAnOlderBuildsWriteCarriesTheCount(t *testing.T) {
	t.Parallel()
	count := &ResumedCount{Tokens: 42_000, Mark: CountMedia, Window: 200_000, Restart: new("u-1")}
	carried := carriedByAnOlderBuild
	carried(t, reflect.TypeFor[LaunchRecord](), []string{"resumed_count", "basis_restart", "count_provider",
		"count_release"}, LaunchRecord{ResumedCount: count, BasisRestart: new("u-0"), CountProvider: "stub",
		CountRelease: "2.3.2"}, func(raw []byte) any {
		var back LaunchRecord
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		return []any{back.ResumedCount, back.BasisRestart, back.CountProvider, back.CountRelease}
	})
	carried(t, reflect.TypeFor[SessionCapture](), []string{"resumed_count", "count_provider"},
		SessionCapture{ResumedCount: count, CountProvider: "stub"}, func(raw []byte) any {
			var back SessionCapture
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			return []any{back.ResumedCount, back.CountProvider}
		})
}

// carriedByAnOlderBuild holds that value, of type typ, reads as read reads it
// after a build that declares none of keys decodes and writes it back: what
// that build keeps of them verbatim ([unmarshalCarrying], [marshalCarrying]).
func carriedByAnOlderBuild(t *testing.T, typ reflect.Type, keys []string, value any, read func([]byte) any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	declared := wireKeys(typ)
	older := maps.Clone(declared)
	for _, key := range keys {
		if !declared[key] {
			t.Fatalf("%s declares no key %s: %v", typ, key, declared)
		}
		delete(older, key)
	}
	var plain map[string]json.RawMessage
	extra, err := unmarshalCarrying(raw, &plain, older)
	if err != nil {
		t.Fatal(err)
	}
	written, err := marshalCarrying(struct{}{}, older, extra)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(written); !reflect.DeepEqual(got, read(raw)) {
		t.Errorf("after the older build's write %s reads %+v, want %+v: %s", typ, got, read(raw), written)
	}
}

// A LAUNCH RECORDS THE RESTART OF THE SESSION IT HANDS ITS RUN, the newest
// place its thread restarts at, so a relaunch knows whether the job restarted
// it after its brief. Continuing the copy in its box: the previous job's
// collection read (by the reset that replaced its record), marked or a bound,
// or nothing known where it did not see the thread whole. Continuing a kept
// capture: the capture's own read (marked here). A fresh session: a thread
// that restarts nowhere. A mark of what whole bytes cannot bound says nothing
// of where the thread restarts, so such a read is a known basis.
//
// Mutation: record nothing, the copy's count where it did not read the thread
// whole, a marked count's as not known at either place, or a fresh session's as
// not known, and a case goes red.
func TestALaunchRecordsTheRestartOfTheSessionItHandsItsRun(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		read *ResumedCount
		want *string
	}{
		"the copy in its box, read whole":     {read: &ResumedCount{Tokens: 1, Restart: new("u-9")}, want: new("u-9")},
		"the copy in its box, not read whole": {read: &ResumedCount{Mark: CountUnread}},
		"the copy in its box, read whole and marked": {read: &ResumedCount{Mark: CountMedia, Restart: new("u-9")},
			want: new("u-9")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			askAndAnswer(t, rig, false, func(ctx context.Context, r PendingRun, asked *AskedRun) {
				if _, err := rig.pending.MarkHarnessEnd(ctx, "t1", r.LaunchID, EndRead{End: HarnessEndOwnRun,
					Count: tc.read}); err != nil {
					t.Errorf("MarkHarnessEnd: %v", err)
				}
				if _, err := rig.launchVia(ctx, rig.manager, continuing(asked)); err != nil {
					t.Errorf("relaunch: %v", err)
				}
			})
			if got := rig.get("t1").Launch.BasisRestart; deref(got) != deref(tc.want) {
				t.Errorf("the job continuing the copy in its box records the restart %s, want %s", deref(got),
					deref(tc.want))
			}
		})
	}
	t.Run("a kept capture", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, restore, kept := keptTurn(t)
			if _, _, err := rig.pending.mutate(t.Context(), "t1", func(run *PendingRun) bool {
				run.Kept.ResumedCount = &ResumedCount{Mark: CountMedia, Restart: new("u-7")}
				return true
			}); err != nil {
				t.Fatalf("counting the kept capture: %v", err)
			}
			if _, err := launchRestoring(t, rig, restore, continuingKept(kept)); err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			if got := rig.get("t1").Launch.BasisRestart; deref(got) != "u-7" {
				t.Errorf("the job continuing the kept capture records the restart %s, want the capture's u-7",
					deref(got))
			}
		})
	})
	t.Run("a session of its own", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		if _, err := rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t1")); err != nil {
			t.Fatalf("launch: %v", err)
		}
		if got := rig.get("t1").Launch.BasisRestart; got == nil || *got != "" {
			t.Errorf("a job starting a session of its own records the restart %s, want one that restarts nowhere",
				deref(got))
		}
	})
}

// deref is what a restart reads, "<nil>" for none known.
func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// A CAPTURE'S COUNT IS HELD TO WHAT A ROW CARRIES: a stamp naming a mark past its
// bound, or a restart past a uuid's thirty-six bytes, is refused before it reaches
// the row, as every text a box describes its session by is; and the take's own
// stamp writes such a mark as unread rather than lose the capture to the refusal.
//
// Mutation: let the stamp carry a mark or a restart past its bound, or stamp the
// take's count unbounded, and a case goes red.
func TestACapturesCountIsHeldToWhatARowCarries(t *testing.T) {
	t.Parallel()
	long := CountMark(strings.Repeat("m", maxCountMarkBytes+1))
	for name, count := range map[string]*ResumedCount{
		"a mark past its bound":    {Mark: long},
		"a restart past its bound": {Tokens: 1, Restart: new(strings.Repeat("u", RestartIDMaxBytes+1))},
	} {
		if err := (SessionCapture{ResumedCount: count}).Bounded(); !errors.Is(err, ErrCaptureUnbounded) {
			t.Errorf("%s: the stamp is refused with %v, want %v", name, err, ErrCaptureUnbounded)
		}
	}
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		session.Count = &ResumedCount{Mark: long}
		rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
		if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
			t.Fatalf("readBox: %v", err)
		}
		rig.awaitKeeps()
		run := rig.get("t1")
		if run.Capture == nil || run.Capture.ResumedCount == nil || run.Capture.ResumedCount.Mark != CountUnread {
			t.Errorf("the take's count past its bound is stamped %+v, want the capture kept and its count unread",
				run.Capture)
		}
	})
}

// THE NEXT REQUEST RIDES WHERE THE COUNT DOES. The read that collects a Claude
// Code job asks its main thread for the harness's estimate of the copy's next
// request beside the count, and writes it on the job's own record in the same
// write, carries it back to the claim it collects for, which a relaunch carrying
// the round on in the job's box reads; and the take's estimate of the archive
// it keeps is stamped on the capture, which a relaunch continuing that capture
// reads, beside the context its last request carried.
//
// Mutation: leave the estimate out of the record's write, out of the claim, or
// off the capture's stamp, and a case goes red.
func TestTheNextRequestRidesWhereTheCountDoes(t *testing.T) {
	t.Parallel()
	estimate := NextRequest{Tokens: 42_000, OlderTokens: 31_500}
	t.Run("the job's record", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		thread := &countingThread{count: &ResumedCount{Tokens: 40_000, Restart: new("")}, next: estimate}
		rig.runner.MainThread = thread.read
		rig.launch("t1")
		run := rig.get("t1")
		rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1"})
		if _, _, err := rig.coordinator.readBox(t.Context(), run, BoxReadCollect); err != nil {
			t.Fatalf("readBox: %v", err)
		}
		if got := rig.get("t1").Launch.NextRequest(); got != estimate {
			t.Errorf("the job's record holds the next request %+v, want %+v", got, estimate)
		}
		if asks := thread.asks(); len(asks) != 1 || !asks[0].Next || !asks[0].Count {
			t.Errorf("the read asked the thread %+v, want once for the count and the next request", asks)
		}
	})
	t.Run("the claim", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		rig.runner.MainThread = (&countingThread{count: &ResumedCount{Tokens: 40_000, Restart: new("")},
			next: estimate}).read
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1"})
		var (
			mu      sync.Mutex
			claimed []NextRequest
		)
		rig.resumer.failWith(errors.New("held for the case to read the claim"))
		rig.resumer.during = func(_ context.Context, run PendingRun) {
			mu.Lock()
			defer mu.Unlock()
			claimed = append(claimed, run.Launch.NextRequest())
		}
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Logf("OnCompleted: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(claimed) != 1 || claimed[0] != estimate {
			t.Errorf("the resume on the claim read the next requests %+v, want once %+v", claimed, estimate)
		}
	})
	t.Run("the capture", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			rig.launchKept(t, "t1")
			session := rig.runner.Session
			session.ContextTokens, session.NextRequest = 30_000, estimate
			rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
			if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
				t.Fatalf("readBox: %v", err)
			}
			rig.awaitKeeps()
			run := rig.get("t1")
			if run.Capture == nil || run.Capture.NextRequest() != estimate || run.Capture.ContextTokens != 30_000 {
				t.Fatalf("the capture is stamped %+v, want the next request %+v beside the context 30,000",
					run.Capture, estimate)
			}
			if got := copyNextRequest(run); got != estimate {
				t.Errorf("a relaunch following the job reads the next request %+v, want %+v", got, estimate)
			}
		})
	})
}

// THE PROVIDER RIDES BESIDE THE COUNT, AND SO DOES ITS WRITER. The read that
// collects an OpenCode job names the provider of the reply its count is
// anchored on, and the collection writes it on the job's record beside the
// count with the release that wrote the copy, in the same write, carries both
// back to the claim, and stamps the take's provider on the capture beside its
// count; a provider no record may carry as itself is written as none, its count
// unread with it, and a release that is not one as none; and a later read that
// brings no count leaves all three as they were, since both are the count's.
//
// Mutation: leave the provider or the writer out of the record's write or out
// of the claim, the provider off the stamp, keep one no record may carry, or
// write either where the read brings no count, and a case goes red.
func TestTheProviderRidesBesideTheCount(t *testing.T) {
	t.Parallel()
	count := &ResumedCount{Tokens: 40_000, Restart: new("msg_c")}
	t.Run("the job's record and the claim", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		rig.runner.MainThread = (&countingThread{count: count, provider: "stub"}).read
		rig.launch("t1")
		rig.coordinator.countRun("swe", StatusRunning)
		rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.3.2"})
		var (
			mu      sync.Mutex
			claimed []string
		)
		rig.resumer.failWith(errors.New("held for the case to read the claim"))
		rig.resumer.during = func(_ context.Context, run PendingRun) {
			mu.Lock()
			defer mu.Unlock()
			claimed = append(claimed, run.Launch.CountProvider+" "+run.Launch.CountRelease)
		}
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Logf("OnCompleted: %v", err)
		}
		if got := rig.get("t1").Launch; got.CountProvider != "stub" || got.CountRelease != "2.3.2" {
			t.Errorf("the job's record names the provider %q written by %q, want stub by 2.3.2",
				got.CountProvider, got.CountRelease)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(claimed) != 1 || claimed[0] != "stub 2.3.2" {
			t.Errorf("the resume on the claim read the providers and writers %q, want once stub 2.3.2", claimed)
		}
	})
	t.Run("the record's own rule", func(t *testing.T) {
		t.Parallel()
		held := LaunchRecord{ResumedCount: count, CountProvider: "stub", CountRelease: "2.3.2"}
		if got, provider, release := held.counted(nil, "", ""); !got.same(count) || provider != "stub" ||
			release != "2.3.2" {
			t.Errorf("a read with no count leaves %+v, %q and %q, want the record's own", got, provider, release)
		}
		got, provider, _ := held.counted(count, `a"b`, "2.3.2")
		if provider != "" || got.Mark != CountUnread || got.Restart != nil {
			t.Errorf("a provider no record may carry is written as %q beside %+v, want none beside an unread "+
				"count", provider, got)
		}
		if _, _, release := held.counted(count, "stub", "2.3"); release != "" {
			t.Errorf("a release that is not one is written as %q, want none", release)
		}
	})
	t.Run("the capture's stamp", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := keepingRig(t)
			rig.launchKept(t, "t1")
			session := rig.runner.Session
			session.Count, session.CountProvider = count, "stub"
			rig.runner.Finish(Result{Success: true, SessionID: "s-1", Captured: &session.Archive})
			if _, _, err := rig.coordinator.readBox(t.Context(), rig.get("t1"), BoxReadCollect); err != nil {
				t.Fatalf("readBox: %v", err)
			}
			rig.awaitKeeps()
			if run := rig.get("t1"); run.Capture == nil || run.Capture.CountProvider != "stub" {
				t.Errorf("the capture is stamped %+v, want the provider stub", run.Capture)
			}
		})
	})
	if err := (SessionCapture{CountProvider: strings.Repeat("p", MaxCountProviderBytes+1)}).Bounded(); !errors.Is(
		err, ErrCaptureUnbounded) {
		t.Errorf("a stamp naming a provider past its bound is refused with %v, want %v", err, ErrCaptureUnbounded)
	}
}

// testThreadTraits is what each harness's runner states its main-thread read
// settles (internal/sandbox/executor's), for cases that name a harness.
var testThreadTraits = map[string]ThreadTraits{
	"claude-code": {Reads: true, CountsCutOff: true, Receipts: true, PlacesWithResumeRead: true},
	"opencode":    {Reads: true},
}
