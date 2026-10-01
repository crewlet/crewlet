package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// phases is every `agent_phase_completed` the coordinator published, in order.
func (r *coordRig) phases() []types.AgentPhaseCompleted {
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	var out []types.AgentPhaseCompleted
	for _, p := range r.queue.published {
		rec, ok := p.event.Data.(*types.AgentPhaseCompleted)
		if !ok {
			continue
		}
		if want := topics.Event(rec.EventType()); p.topic != want {
			r.t.Errorf("the phase record went to %q, want %q, where every reader subscribes", p.topic, want)
		}
		out = append(out, *rec)
	}
	return out
}

// A COLLECTED RUN IS A PHASE OF ITS OWN, published once, with its tokens.
//
// Until this record existed a detached coding run left no phase behind: the
// executor that launched it publishes nothing when it suspends, and its
// resumed record states the executor's own rounds. So every token a coding run
// spent reached the budget counter and nothing a person reads — the spend
// rollup and each node's daily usage are folded from these records alone.
//
// ONCE, however often the tail is retried: the first resume here fails, the
// claim is handed back and the completion comes round again, and a second copy
// of the record would be a second charge on every surface that shows spend.
func TestACollectedRunIsPublishedOnceWithItsTokens(t *testing.T) {
	rig := newCoordRig(t)
	launched := rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.now = rig.now.Add(7 * time.Minute)
	rig.runner.Finish(Result{
		Success: true, Text: "fixed the flake", DeliveredRefs: []string{"pr/42"},
		InputTokens: 9000, OutputTokens: 700, CacheReadTokens: 6000, CacheWriteTokens: 1500,
		CostUSD: 0.42,
	})
	rig.resumer.failWith(errors.New("the node lost the seat mid-resume"))

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was acked")
	}
	rig.resumer.failWith(nil)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got := len(rig.resumer.calls()); got != 1 {
		t.Fatalf("resumed %d times, want the retry to succeed once", got)
	}

	recs := rig.phases()
	if len(recs) != 1 {
		t.Fatalf("published %d phase records for one run collected twice, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Phase != types.PhaseSandbox || rec.Backend != types.BackendSandbox {
		t.Errorf("phase/backend = %q/%q, want sandbox/sandbox", rec.Phase, rec.Backend)
	}
	if rec.InputTokens != 9000 || rec.OutputTokens != 700 || rec.TotalTokens != 9700 {
		t.Errorf("tokens = %d in / %d out / %d total, want the run's 9000/700/9700",
			rec.InputTokens, rec.OutputTokens, rec.TotalTokens)
	}
	if rec.CacheReadTokens != 6000 || rec.CacheWriteTokens != 1500 {
		t.Errorf("cache = %d/%d, want the run's 6000/1500", rec.CacheReadTokens, rec.CacheWriteTokens)
	}
	if rec.CostUSD != 0.42 {
		t.Errorf("cost_usd = %v, want what the run's CLI reported", rec.CostUSD)
	}
	// THE IDENTITY: the executor iteration that launched it and the job.
	if rec.TurnID != "t1" || rec.Iteration != rigIteration || rec.LaunchID != launched.LaunchID {
		t.Errorf("identity = (%s, %d, %s), want (t1, %d, %s)",
			rec.TurnID, rec.Iteration, rec.LaunchID, rigIteration, launched.LaunchID)
	}
	// ITS CLOCK: from the launch the store dated to the collection.
	if !rec.StartedAt.Equal(launched.Launch.StartedAt) || rec.StartedAt.IsZero() {
		t.Errorf("started_at = %s, want the launch's %s", rec.StartedAt, launched.Launch.StartedAt)
	}
	if rec.DurationMS != int((7 * time.Minute).Milliseconds()) {
		t.Errorf("duration = %dms, want the 7 minutes from launch to collection", rec.DurationMS)
	}
	if rec.Model != "claude-sonnet-5" || rec.CodingAgent != "claude-code" || rec.SandboxID != launched.SandboxID {
		t.Errorf("model/agent/box = %q/%q/%q", rec.Model, rec.CodingAgent, rec.SandboxID)
	}
	if rec.WorkItem == nil || *rec.WorkItem != rigItem || rec.WorkKey != launched.UnitOfWork() {
		t.Errorf("work item/key = %+v/%q, want the launch's", rec.WorkItem, rec.WorkKey)
	}
	if rec.Response != "fixed the flake" || len(rec.DeliveredRefs) != 1 || rec.Failed {
		t.Errorf("response %q refs %v failed %v", rec.Response, rec.DeliveredRefs, rec.Failed)
	}
	if rec.ConversationKey != "chat:D1" || rec.Agent != "a-1" || rec.RoleName != "SWE" {
		t.Errorf("conversation/agent/role = %q/%q/%q", rec.ConversationKey, rec.Agent, rec.RoleName)
	}
}

// THE TRANSCRIPT RIDES THE RECORD, REDACTED.
//
// The runner reconstructs an activity transcript from the coding agent's own
// output, and it was collected, redacted and then thrown away: nothing the
// resume carried included it, so the doc's promise that it is published was
// untrue. It is the whole account of what an agent with no telemetry did —
// and it came out of a box whose environment holds the seat's credentials, so
// the publish is a boundary that redacts again rather than trusting the runner.
func TestTheTranscriptRidesThePhaseRecordRedacted(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	secret := "ghp_" + strings.Repeat("c", 36)
	rig.runner.Finish(Result{
		Success: true, Text: "done",
		Transcript: "[tool] bash: git clone https://" + secret + "@example.com/acme/api\n[tool] bash: go test ./...",
	})

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	recs := rig.phases()
	if len(recs) != 1 {
		t.Fatalf("published %d phase records, want 1", len(recs))
	}
	if !strings.Contains(recs[0].ActivityTranscript, "go test ./...") {
		t.Errorf("the transcript was not published: %q", recs[0].ActivityTranscript)
	}
	if strings.Contains(recs[0].ActivityTranscript, secret) {
		t.Errorf("a credential reached the event store: %q", recs[0].ActivityTranscript)
	}
}

// TWO LAUNCHES IN ONE TURN ARE TWO SPANS.
//
// A resumed executor may call run_sandbox again in the same iteration, which
// is a second job with its own start, its own spend and its own record. The
// record of the first must not tell the second it was already published, and
// the second's clock starts at its own launch rather than the row's creation.
func TestTwoLaunchesInOneTurnAreTwoSpans(t *testing.T) {
	rig := newCoordRig(t)
	first := rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.resumer.during = func(ctx context.Context, r PendingRun) {
		rig.resumer.mu.Lock()
		rig.resumer.during = nil
		rig.resumer.mu.Unlock()
		rig.now = rig.now.Add(time.Minute)
		req := launchReq(r.TurnID)
		req.ReuseBox = r.SandboxID
		req.LLM = &AgentLLM{Model: "claude-opus-5"}
		if _, err := Launch(ctx, rig.manager, rig.pending, rig.queue, req); err != nil {
			t.Errorf("relaunch: %v", err)
		}
	}
	rig.runner.Finish(Result{Success: true, Text: "first pass", InputTokens: 500})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the first run's completion: %v", err)
	}

	rig.suspend("t1")
	second := rig.get("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.now = rig.now.Add(3 * time.Minute)
	rig.runner.Finish(Result{Success: true, Text: "second pass", InputTokens: 800})
	payload, ev = rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the second run's completion: %v", err)
	}

	recs := rig.phases()
	if len(recs) != 2 {
		t.Fatalf("published %d phase records for two runs, want 2", len(recs))
	}
	if recs[0].LaunchID != first.LaunchID || recs[1].LaunchID != second.LaunchID ||
		recs[0].LaunchID == recs[1].LaunchID {
		t.Errorf("launch ids = %q, %q; want the two jobs', distinct", recs[0].LaunchID, recs[1].LaunchID)
	}
	if recs[0].InputTokens != 500 || recs[1].InputTokens != 800 {
		t.Errorf("tokens = %d, %d; want each job's own", recs[0].InputTokens, recs[1].InputTokens)
	}
	if !recs[1].StartedAt.Equal(second.Launch.StartedAt) || !recs[1].StartedAt.After(recs[0].StartedAt) {
		t.Errorf("the second span starts at %s, want its own launch at %s", recs[1].StartedAt, second.Launch.StartedAt)
	}
	if recs[1].DurationMS != int((3 * time.Minute).Milliseconds()) {
		t.Errorf("the second span is %dms, want its own 3 minutes", recs[1].DurationMS)
	}
	if recs[0].Model != "claude-sonnet-5" || recs[1].Model != "claude-opus-5" {
		t.Errorf("models = %q, %q; want each launch's own", recs[0].Model, recs[1].Model)
	}
}

// A RUN THAT STOPPED TO ASK IS STOPPED, NOT FAILED — and is still published.
//
// It spent what it spent, and its resume is a person's answer that collects
// nothing, so this is the only record that run will ever have: its cost
// especially, which used to ride the resume and so was reported nowhere for a
// run that parked. A run that genuinely did not succeed is marked failed with
// its reason.
func TestEveryCollectedRunIsPublishedWithItsOutcome(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("asks")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{
		NeedsInput: true, Question: "which branch?", AskTo: "requester",
		InputTokens: 300, CostUSD: 0.05,
	})
	payload, ev := rig.completion("asks")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	recs := rig.phases()
	if len(recs) != 1 {
		t.Fatalf("a parked run published %d phase records, want 1", len(recs))
	}
	if recs[0].Failed || !strings.Contains(recs[0].Notes, "which branch?") {
		t.Errorf("a parked run reads failed=%v notes=%q; want stopped with its question", recs[0].Failed, recs[0].Notes)
	}
	if recs[0].InputTokens != 300 || recs[0].CostUSD != 0.05 {
		t.Errorf("a parked run's spend = %d tokens, $%v; want its own", recs[0].InputTokens, recs[0].CostUSD)
	}

	rig.launch("breaks")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Error: "the coding agent exited with status 1: no such file"})
	payload, ev = rig.completion("breaks")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	recs = rig.phases()
	if len(recs) != 2 {
		t.Fatalf("published %d phase records, want 2", len(recs))
	}
	if !recs[1].Failed || !strings.Contains(recs[1].Error, "no such file") {
		t.Errorf("a run that did not succeed reads failed=%v error=%q", recs[1].Failed, recs[1].Error)
	}
}

// A PUBLISH THE QUEUE REFUSED IS NOT RECORDED, and never costs the run.
//
// Telemetry never fails the tail — the turn resumes exactly as it would have —
// and a record that did not go out is not marked as having gone, so a retry of
// the tail, if one comes, offers it again.
func TestARefusedPhasePublishIsNotRecorded(t *testing.T) {
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true, Text: "done", InputTokens: 100})
	rig.failPublishes(errors.New("broker unavailable"))
	rig.resumer.failWith(errors.New("the node lost the seat mid-resume"))

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err == nil {
		t.Fatal("a failed resume was acked")
	}
	if got := rig.get("t1").LaunchFacts(); got.Published {
		t.Fatal("a publish the queue refused was recorded as published, so no retry would offer it again")
	}

	rig.failPublishes(nil)
	rig.resumer.failWith(nil)
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if got := len(rig.phases()); got != 1 {
		t.Fatalf("published %d phase records, want the retry's one", got)
	}
}

// AN OLDER ROW NAMES NO START, and the record states none.
//
// A row a build that predates the launch record wrote has no start and no
// iteration; the record then says so by omission rather than measuring a
// duration from the year 1.
func TestARunLaunchedByAnOlderBuildStatesNoClock(t *testing.T) {
	run := PendingRun{TurnID: "t1", LaunchID: "job-2", Launch: LaunchRecord{
		ID: "job-1", StartedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), Iteration: 4, Published: true,
	}}
	facts := run.LaunchFacts()
	if facts != (LaunchRecord{}) {
		t.Fatalf("a record kept for job-1 answered for job-2: %+v", facts)
	}
	rec := runPhase(run, facts, Result{Success: true}, time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC))
	if !rec.StartedAt.IsZero() || rec.DurationMS != 0 || rec.Iteration != 0 {
		t.Errorf("a run with no launch record states start %s, %dms, iteration %d",
			rec.StartedAt, rec.DurationMS, rec.Iteration)
	}
}
