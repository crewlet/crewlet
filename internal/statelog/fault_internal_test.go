package statelog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// A RUN OF FAILURES IS REPORTED ONCE, NOT ONCE PER RETRY.
//
// `statelog_apply_faulted` was written on every retry past the budget, and a
// fault that outlives the budget is the kind that lasts — so a full disk wrote
// twelve ERROR lines a minute per domain for as long as it was full, and
// anything counting the event read one incident as hundreds. A run is written
// as its transitions instead: it starts, it crosses the budget, it recovers.
// And a second run is a second incident, which is written again.
//
// Internal, because the clock the budget is measured on is the runner's own
// and the retry pause is real time: driven through Run, ten minutes of a
// fault would take ten minutes.
func TestAFaultPastTheBudgetIsReportedOncePerRun(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	runner, err := NewRunner(RunnerDeps{
		Domain: loggerProbe{}, Spec: loggerProbeSpec(), Verifier: loggerProbeVerifier(t),
		Applier: struct{ Applier }{},
		Fetch:   struct{ Fetcher }{}, Log: struct{ CheckpointLog }{},
		Node: nodeProbe{}, DB: struct{ Estate }{},
		Logger: slog.New(slog.NewJSONHandler(&buf, nil)),
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	full := errors.New("the disk is full")

	// faultFor retries one failure at the loop's slowest pace for as long
	// as it is told to, which is the most lines the old shape could write.
	faultFor := func(d time.Duration) {
		t.Helper()
		start := now
		for now.Sub(start) <= d {
			if stopped := runner.faulted(t.Context(), sideApply, full); stopped != nil {
				t.Fatalf("a transient failure ended the loop: %v", stopped)
			}
			now = now.Add(ApplyRetryCeiling)
		}
	}
	counts := func() map[string]int {
		t.Helper()
		out := map[string]int{}
		for _, record := range jsonRecords(t, &buf) {
			msg, _ := record["msg"].(string)
			out[msg]++
		}
		return out
	}

	// ONE RUN, TEN MINUTES LONG: a hundred and twenty retries.
	faultFor(10 * time.Minute)
	got := counts()
	if got["statelog_apply_retrying"] != 1 || got["statelog_apply_faulted"] != 1 {
		t.Fatalf("a ten-minute fault wrote %d statelog_apply_retrying and %d "+
			"statelog_apply_faulted lines, want one of each — a line per retry "+
			"reads one incident as a hundred of them",
			got["statelog_apply_retrying"], got["statelog_apply_faulted"])
	}
	// AND THE ONE IT WROTE IS THE MOMENT Fault STARTED ANSWERING, which is
	// when the reads began refusing and the seats began moving.
	for _, record := range jsonRecords(t, &buf) {
		if record["msg"] != "statelog_apply_faulted" {
			continue
		}
		if record["level"] != "ERROR" || record["error"] != full.Error() {
			t.Errorf("statelog_apply_faulted is %v naming %v, want ERROR naming %q",
				record["level"], record["error"], full)
		}
	}
	if _, faulted := runner.Fault(now); !faulted {
		t.Fatal("a ten-minute fault is not reported by Fault, so nothing else " +
			"carries the level the log no longer repeats")
	}

	// THE RECOVERY CLOSES THE RUN.
	runner.recovered(t.Context())
	if got := counts()["statelog_apply_recovered"]; got != 1 {
		t.Fatalf("%d statelog_apply_recovered lines, want one closing the run", got)
	}

	// A SECOND RUN IS A SECOND INCIDENT, and it is written again.
	faultFor(2 * ApplyRetryBudget)
	got = counts()
	if got["statelog_apply_retrying"] != 2 || got["statelog_apply_faulted"] != 2 {
		t.Fatalf("a second run wrote %d statelog_apply_retrying and %d "+
			"statelog_apply_faulted lines in total, want two of each — the latch "+
			"outlived the recovery that ended the first run",
			got["statelog_apply_retrying"], got["statelog_apply_faulted"])
	}
	runner.recovered(t.Context())

	// AND A BLIP INSIDE THE BUDGET IS NEVER AN ERROR.
	faultFor(ApplyRetryBudget - 2*ApplyRetryCeiling)
	runner.recovered(t.Context())
	if got := counts()["statelog_apply_faulted"]; got != 2 {
		t.Fatalf("a blip inside the budget wrote a statelog_apply_faulted line "+
			"(%d in total, want 2)", got)
	}

	// A RUN THE LOOP WAS RESTARTED OUT OF ends with it. An adoption ends
	// the loop mid-fault and runs it again, and Run forgets the fault as a
	// verdict about rows the node no longer has — so a failure after it is
	// a new run, and it is reported when it crosses the budget in its turn.
	faultFor(2 * ApplyRetryBudget)
	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runner.Run(stopped); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run under a cancelled context returned %v", err)
	}
	faultFor(2 * ApplyRetryBudget)
	if got := counts()["statelog_apply_faulted"]; got != 4 {
		t.Fatalf("%d statelog_apply_faulted lines in total, want 4 — a run that "+
			"began after the loop was restarted was never reported", got)
	}
}

// A RUN ENDS ONLY ONCE EVERY PART THAT FAILED IN IT HAS SUCCEEDED AGAIN.
//
// A commit of the checkpoint transaction beside the loop is the retry of that
// transaction's failure and of nothing else's. Here a fetch fails and that
// transaction then commits what the loop handed it before: the run stands — no
// `statelog_apply_recovered`, its clock running on, Fault reporting it past the
// budget, a second failed fetch the same run rather than a second one — until a
// fetch is answered. Before, any success ended any run, so a commit beside the
// loop logged a recovery from a fetch nobody had retried and restarted the
// fault's clock and its back-off.
func TestARunEndsOnlyOnceEveryPartThatFailedHasSucceeded(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	runner, err := NewRunner(RunnerDeps{
		Domain: loggerProbe{}, Spec: loggerProbeSpec(), Verifier: loggerProbeVerifier(t),
		Applier: struct{ Applier }{},
		Fetch:   struct{ Fetcher }{}, Log: struct{ CheckpointLog }{},
		Node: nodeProbe{}, DB: struct{ Estate }{},
		Logger: slog.New(slog.NewJSONHandler(&buf, nil)),
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	counts := func() map[string]int {
		t.Helper()
		out := map[string]int{}
		for _, record := range jsonRecords(t, &buf) {
			msg, _ := record["msg"].(string)
			out[msg]++
		}
		return out
	}
	unanswered := errors.New("the broker did not answer")
	fail := func(side faultSide, err error) {
		t.Helper()
		if stopped := runner.faulted(t.Context(), side, err); stopped != nil {
			t.Fatalf("a transient failure ended the loop: %v", stopped)
		}
	}

	fail(sideFetch, unanswered)
	now = now.Add(ApplyRetryBudget)
	if runner.recoveredOn(t.Context(), sideBeside) {
		t.Fatal("a commit beside the loop ended a run of failed fetches, so the " +
			"back-off went back to the beat")
	}
	if got := counts()["statelog_apply_recovered"]; got != 0 {
		t.Fatalf("%d statelog_apply_recovered line(s) after a commit beside the loop, "+
			"want none — no fetch had been answered", got)
	}
	if _, faulted := runner.Fault(now); !faulted {
		t.Fatal("a fetch failing for the whole budget is not reported by Fault — its " +
			"clock was restarted by a commit beside the loop")
	}
	fail(sideFetch, unanswered)
	if got := counts()["statelog_apply_retrying"]; got != 1 {
		t.Fatalf("%d statelog_apply_retrying lines, want one — the second failed fetch "+
			"belongs to the run the first began", got)
	}
	if !runner.recoveredOn(t.Context(), sideFetch) {
		t.Fatal("a fetch answered did not end a run of nothing but failed fetches")
	}
	if got := counts()["statelog_apply_recovered"]; got != 1 {
		t.Fatalf("%d statelog_apply_recovered lines once a fetch was answered, want one", got)
	}

	// THE TRANSACTION'S OWN FAILURE is ended by its commit.
	fail(sideBeside, errors.New("the commit failed"))
	if !runner.recoveredOn(t.Context(), sideBeside) {
		t.Fatal("a commit beside the loop did not end the run its own failure began")
	}

	// A RUN BOTH FAILED IN needs both to succeed again — or a transaction of
	// the loop's own, which carries whatever either failed on.
	fail(sideFetch, unanswered)
	fail(sideBeside, errors.New("the commit failed"))
	if runner.recoveredOn(t.Context(), sideFetch) {
		t.Fatal("an answered fetch ended a run in which the commit beside the loop " +
			"is still failing")
	}
	if !runner.recoveredOn(t.Context(), sideBeside) {
		t.Fatal("the commit beside the loop, the last part failing, did not end the run")
	}
	fail(sideVerify, errors.New("the log's ends could not be read"))
	fail(sideBeside, errors.New("the commit failed"))
	runner.recovered(t.Context())
	if _, faulted := runner.Fault(now.Add(time.Hour)); faulted {
		t.Fatal("a transaction of the loop's own did not end the run")
	}
	if got, want := counts()["statelog_apply_recovered"], 4; got != want {
		t.Fatalf("%d statelog_apply_recovered lines in all, want %d — one a run", got, want)
	}
}

// ONLY A VERIFICATION THAT SETTLED ENDS A VERIFICATION'S FAILURE.
//
// The look at the checkpoint's record failed — the log's ends could not be
// read — and the loop then let through a run of nothing past the checkpoint,
// which needs no look: the verification is still owed, and its failure stands,
// with no `statelog_apply_recovered`, until a look settles it. Before, any run
// the verification let through ended the run of failures.
func TestOnlyAVerificationThatSettledEndsItsFailure(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	runner, err := NewRunner(RunnerDeps{
		Domain: loggerProbe{}, Spec: loggerProbeSpec(), Verifier: loggerProbeVerifier(t),
		Applier: struct{ Applier }{},
		Fetch:   struct{ Fetcher }{}, Log: struct{ CheckpointLog }{},
		Node: nodeProbe{}, DB: struct{ Estate }{},
		Logger: slog.New(slog.NewJSONHandler(&buf, nil)),
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	recoveries := func() int {
		t.Helper()
		n := 0
		for _, record := range jsonRecords(t, &buf) {
			if record["msg"] == "statelog_apply_recovered" {
				n++
			}
		}
		return n
	}

	runner.mu.Lock()
	runner.verify = true
	runner.mu.Unlock()
	if stopped := runner.faulted(t.Context(), sideVerify,
		errors.New("the log's ends could not be read")); stopped != nil {
		t.Fatalf("a transient failure ended the loop: %v", stopped)
	}
	if runner.verifiedOn(t.Context()) {
		t.Fatal("a run that needed no look ended a failed look's run of failures")
	}
	if got := recoveries(); got != 0 {
		t.Fatalf("%d statelog_apply_recovered line(s) while the verification is still "+
			"owed, want none", got)
	}
	if _, faulted := runner.Fault(now.Add(ApplyRetryBudget)); !faulted {
		t.Fatal("a look failing for the whole budget is not reported by Fault")
	}

	runner.mu.Lock()
	runner.verify = false
	runner.mu.Unlock()
	if !runner.verifiedOn(t.Context()) {
		t.Fatal("a verification that settled did not end its failure")
	}
	if got := recoveries(); got != 1 {
		t.Fatalf("%d statelog_apply_recovered lines once the look settled, want one", got)
	}
}

// jsonRecords decodes every line a JSON handler wrote.
func jsonRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line is not JSON: %q: %v", line, err)
		}
		out = append(out, record)
	}
	return out
}
