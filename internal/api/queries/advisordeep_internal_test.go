package queries

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/modelpass"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DEEP RUN ASKS AS THE ADVISOR: the engine's own principal holding an
// administrator's reads, never a person's grant. Mutation: ask as a person,
// and a check fails.
func TestTheDeepRunAsksAsTheAdvisor(t *testing.T) {
	t.Parallel()
	var asked iam.Principal
	var how iam.Resolution
	reads := deepReads{ask: func(ctx context.Context, _ string, _ Params) (any, error) {
		asked, how = iam.From(ctx)
		return map[string]any{}, nil
	}}
	if _, err := reads.Answer(t.Context(), "advisor_snapshot", nil); err != nil {
		t.Fatal(err)
	}
	if how != iam.Resolved || asked.Kind != iam.KindEngine || !asked.Can(iam.GrantAdmin) || asked.Seat != "" {
		t.Errorf("asked as %+v, want the engine's Advisor holding an administrator's reads", asked)
	}
}

// A DEEP RUN'S SUMMARY SAYS WHAT IT DID: its kind, its progress lines, what it
// spent of its bounds and the bound it stopped at. Mutation: read its record as
// a daily pass's, and the progress is lost.
func TestADeepRunsSummarySaysWhatItDid(t *testing.T) {
	t.Parallel()
	var run modelpass.DeepRun
	run.Status, run.InputTokens, run.OutputTokens, run.StoppedBy, run.Minutes = "done", 900_000, 100_000, "done", 12.4
	run.Bounds.Tokens, run.Bounds.Minutes = 2_000_000, 20
	run.Progress = []modelpass.Progress{{Text: "reading capacity"}}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	got, err := runSummary(advisordomain.RunRow{ID: "r", Status: advisordomain.RunDone, Kind: advisordomain.RunKindDeep,
		Classes: json.RawMessage(`[]`), Model: raw})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "deep" || len(got.Progress) != 1 || got.Budget == nil || got.Budget.Tokens != 1_000_000 ||
		got.Budget.MinutesCeiling != 20 || got.Budget.Minutes != 13 || got.StoppedBy == nil || *got.StoppedBy != "done" {
		t.Errorf("summary = %+v, budget %+v", got, got.Budget)
	}
}

// A PREMISE IS ASKED AGAIN AS THE ADVISOR: its read answered through the
// registry, the number at its path compared as the finding said, a number no
// longer there not holding, and a read off the allowlist refused. Mutation:
// read a missing number as holding, and a finding about an item gone stays.
func TestAPremiseIsAskedAgainAsTheAdvisor(t *testing.T) {
	t.Parallel()
	hits := 4.0
	o := advisorObserver{ask: func(context.Context, string, Params) (any, error) {
		if hits < 0 {
			return map[string]any{"entries": []any{}}, nil
		}
		return map[string]any{"entries": []any{map[string]any{"hits": hits}}}, nil
	}}
	p := advisor.Premise{Read: "work_workload", Path: "entries[0].hits", Op: advisor.PremiseGe, Value: 3}
	if held, err := o.Holds(t.Context(), p); err != nil || !held {
		t.Errorf("4 hits held %v (%v), want held", held, err)
	}
	hits = 2
	if held, _ := o.Holds(t.Context(), p); held {
		t.Error("2 hits held a premise of 3 or more")
	}
	hits = -1
	if held, err := o.Holds(t.Context(), p); err != nil || held {
		t.Errorf("a number no longer there held %v (%v)", held, err)
	}
	if _, err := o.Holds(t.Context(), advisor.Premise{Read: "iam_sessions", Path: "n", Op: advisor.PremiseGe}); err == nil {
		t.Error("a premise off the allowlist was asked")
	}
}

// liveLog is an advisor log with a deep run in flight, asked for or not.
type liveLog struct {
	noticeLog
	pending []advisordomain.RunRow
}

func (liveLog) LiveProgress(context.Context, statelog.Freshness) (string, []advisordomain.ProgressLine, bool, error) {
	return "run-1", []advisordomain.ProgressLine{{At: time.Unix(100, 0).UTC(), Text: "reading capacity"}}, true, nil
}

func (l liveLog) PendingRequests(context.Context, statelog.Freshness) ([]advisordomain.RunRow, error) {
	return l.pending, nil
}

// A DEEP RUN IN FLIGHT IS THE LATEST RUN, WITH ITS PROGRESS: a person's press
// shows its lines while it runs, and a run at its slot that nobody pressed
// shows as running from its first line. Mutation: answer the last run that
// ended, and the page says nothing until the investigation is done.
func TestADeepRunInFlightIsTheLatestRunWithItsProgress(t *testing.T) {
	t.Parallel()
	got, err := Sources{Advisor: liveLog{}}.latestRun(t.Context())
	if err != nil || got == nil || got.RunID != "run-1" || got.Status != RunRunning || got.Kind != "deep" ||
		len(got.Progress) != 1 || got.Trigger != string(advisordomain.TriggerSchedule) {
		t.Fatalf("latest = %+v (%v), want the slot's deep run running with its line", got, err)
	}
	pressed := liveLog{pending: []advisordomain.RunRow{{ID: "run-1", By: "ada", Kind: advisordomain.RunKindDeep}}}
	got, err = Sources{Advisor: pressed}.latestRun(t.Context())
	if err != nil || got.By != "ada" || got.Kind != "deep" || len(got.Progress) != 1 {
		t.Errorf("latest = %+v (%v), want ada's deep run with its line", got, err)
	}
}
