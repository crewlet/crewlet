package queries

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
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
// spent of its bounds, the bound it stopped at and how many questions it answered. Mutation: read its record as
// a daily pass's, and the progress is lost.
func TestADeepRunsSummarySaysWhatItDid(t *testing.T) {
	t.Parallel()
	var run modelpass.DeepRun
	run.Status, run.InputTokens, run.OutputTokens, run.StoppedBy, run.Minutes = "done", 900_000, 100_000, "done", 12.4
	run.Bounds.Tokens, run.Bounds.Minutes = 2_000_000, 20
	run.Progress = []modelpass.Progress{{Text: "reading capacity"}}
	run.Tally = &modelpass.AgendaTally{Items: 4, Confirmed: 2, Refuted: 1, Open: 1}
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
	if got.Agenda == nil || *got.Agenda != (AdvisorAgenda{Items: 4, Answered: 3}) {
		t.Errorf("agenda = %+v, want 3 of 4 answered", got.Agenda)
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
func (liveLog) RunProgress(context.Context, string, statelog.Freshness) ([]advisordomain.ProgressLine, error) {
	return nil, nil
}

func (l liveLog) PendingRequests(context.Context, statelog.Freshness) ([]advisordomain.RunRow, error) {
	return l.pending, nil
}

// Checkpoint keeps three questions, one answered.
func (liveLog) Checkpoint(_ context.Context, run string, _ statelog.Freshness) (json.RawMessage, bool, error) {
	if run != "run-1" {
		return nil, false, nil
	}
	return json.RawMessage(`{"agenda":[{"id":"A1","verdict":"confirmed"},{"id":"A2","verdict":"open"},{"id":"A3"}]}`),
		true, nil
}

// A DEEP RUN IN FLIGHT IS THE LATEST RUN, WITH ITS PROGRESS: a person's press
// shows its lines and how many of its questions it has answered while it
// runs, and a run at its slot that nobody pressed shows as running from its
// first line. Mutation: answer the last run that ended, or leave the
// checkpoint unread, and the page cannot say how far it is.
func TestADeepRunInFlightIsTheLatestRunWithItsProgress(t *testing.T) {
	t.Parallel()
	now := func() time.Time { return time.Unix(160, 0) }
	got, err := Sources{Advisor: liveLog{}, Now: now}.latestRun(t.Context())
	if err != nil || got == nil || got.RunID != "run-1" || got.Status != RunRunning || got.Kind != "deep" ||
		len(got.Progress) != 1 || got.Trigger != string(advisordomain.TriggerSchedule) {
		t.Fatalf("latest = %+v (%v), want the slot's deep run running with its line", got, err)
	}
	pressed := liveLog{pending: []advisordomain.RunRow{{ID: "run-1", By: "ada", Kind: advisordomain.RunKindDeep}}}
	got, err = Sources{Advisor: pressed, Now: now}.latestRun(t.Context())
	if err != nil || got.By != "ada" || got.Kind != "deep" || len(got.Progress) != 1 {
		t.Errorf("latest = %+v (%v), want ada's deep run with its line", got, err)
	}
	if got.Agenda == nil || *got.Agenda != (AdvisorAgenda{Items: 3, Answered: 1}) {
		t.Errorf("agenda = %+v, want 1 of 3 answered", got.Agenda)
	}
}

// staleLog is an advisor log with a deep run that wrote its last line at heard
// and no record, beside a recorded run that ended at ended.
type staleLog struct {
	noticeLog
	heard, ended time.Time
}

func (l staleLog) LiveProgress(context.Context, statelog.Freshness) (string, []advisordomain.ProgressLine, bool, error) {
	return "orphan", []advisordomain.ProgressLine{{At: l.heard.Add(-time.Minute), Text: "reading capacity"},
		{At: l.heard, Text: "testing a lead"}}, true, nil
}
func (l staleLog) LatestRun(context.Context, statelog.Freshness) (advisordomain.RunRow, bool, error) {
	return advisordomain.RunRow{ID: "recorded", Status: advisordomain.RunDone, Trigger: advisordomain.TriggerSchedule,
		StartedAt: l.ended.Add(-time.Minute), EndedAt: l.ended}, true, nil
}

// THE LATEST RUN PREFERS NEWER TRUTH OVER A STALE LIVE RUN: a run with lines
// and no record reads as running only while nothing was recorded after its
// last line and it was heard from within the longest a deep run lasts; past
// either its holder stopped, and the latest recorded run stands. Mutation:
// answer the live run first, and a run whose holder restarted reads as
// running for hours.
func TestTheLatestRunPrefersNewerTruthOverAStaleLiveRun(t *testing.T) {
	t.Parallel()
	heard := time.Date(2030, 1, 9, 3, 10, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		ended, now time.Time
		want       string
	}{
		{"recorded after its last line", heard.Add(time.Hour), heard.Add(2 * time.Hour), "recorded"},
		{"unheard past a deep run's longest", heard.Add(-time.Hour), heard.Add(duty.DeepLongest), "recorded"},
		{"heard since the last record", heard.Add(-time.Hour), heard.Add(time.Minute), "orphan"},
	} {
		got, err := Sources{Advisor: staleLog{heard: heard, ended: tc.ended},
			Now: func() time.Time { return tc.now }}.latestRun(t.Context())
		if err != nil || got == nil || got.RunID != tc.want || (tc.want == "orphan") != (got.Status == RunRunning) {
			t.Errorf("%s: latest = %+v (%v), want %s", tc.name, got, err, tc.want)
		}
	}
}

// AN INTERRUPTED RUN READS AS INTERRUPTED, SAYING WHY: its status as recorded,
// its reason, and no budget, since it kept no measure of what it spent.
// Mutation: answer its budget, and the page says it spent nothing of none.
func TestAnInterruptedRunReadsAsInterruptedSayingWhy(t *testing.T) {
	t.Parallel()
	model, err := json.Marshal(map[string]any{"status": advisor.PassFailed, "reason": duty.InterruptedSaid})
	if err != nil {
		t.Fatal(err)
	}
	got, err := runSummary(advisordomain.RunRow{ID: "orphan", Status: advisordomain.RunInterrupted,
		Kind: advisordomain.RunKindDeep, Trigger: advisordomain.TriggerSchedule, Model: model})
	if err != nil || got.Status != string(advisordomain.RunInterrupted) || got.Model == nil ||
		got.Model.Reason != duty.InterruptedSaid || got.Budget != nil {
		t.Errorf("summary = %+v (%v), want interrupted, saying why, with no budget", got, err)
	}
}

// A RUN READS AS ITS STEPS: a failed read folds into the read it ended, one
// failed step with its reason; a verdict carries its question; the newest
// read of a run in flight is running and of a finished run done; and a line
// from before steps is a note. Mutation: list the failure as a second line,
// and the page shows a read twice, once still going.
func TestARunReadsAsItsSteps(t *testing.T) {
	t.Parallel()
	at := time.Date(2030, 1, 9, 3, 0, 0, 0, time.UTC)
	lines := []advisordomain.ProgressLine{
		{At: at, Text: "Picked up where it stopped"},
		{At: at.Add(time.Second), Step: advisordomain.ProgressRead, Text: "Reading token spend", Detail: "tokens days=7"},
		{At: at.Add(2 * time.Second), Step: advisordomain.ProgressRead, Failed: true, Text: "Could not read token spend",
			Detail: "tokens days=7", Why: "the ledger is down"},
		{At: at.Add(3 * time.Second), Step: advisordomain.ProgressVerdict, Text: "Confirmed: releases wait",
			Detail: "A2 Do releases pile up behind one seat?"},
		{At: at.Add(4 * time.Second), Step: advisordomain.ProgressRead, Text: "Reading work items", Detail: "work_items"},
	}
	want := []AdvisorProgress{
		{At: at, Text: "Picked up where it stopped", Step: "note", State: StepDone},
		{At: at.Add(2 * time.Second), Text: "Could not read token spend", Step: "read", State: StepFailed,
			Detail: "tokens days=7", Why: "the ledger is down"},
		{At: at.Add(3 * time.Second), Text: "Confirmed: releases wait", Step: "verdict", State: StepDone,
			Detail: "A2 Do releases pile up behind one seat?"},
		{At: at.Add(4 * time.Second), Text: "Reading work items", Step: "read", State: StepRunning, Detail: "work_items"},
	}
	if got := stepsOf(lines, true); !slices.Equal(got, want) {
		t.Errorf("in flight:\n%+v\nwant\n%+v", got, want)
	}
	want[3].State = StepDone
	if got := stepsOf(lines, false); !slices.Equal(got, want) {
		t.Errorf("finished:\n%+v\nwant\n%+v", got, want)
	}
	// A FINISHED RUN'S RECORD keeps its last lines with their steps.
	run := modelpass.DeepRun{Pass: advisor.Pass{Status: advisor.PassDone}, StoppedBy: modelpass.StoppedDone}
	for _, l := range lines {
		run.Progress = append(run.Progress, modelpass.Progress{At: l.At, Text: l.Text, Step: l.Step, Failed: l.Failed,
			Detail: l.Detail, Why: l.Why})
	}
	record, err := json.Marshal(run.Kept())
	if err != nil {
		t.Fatal(err)
	}
	got, err := runSummary(advisordomain.RunRow{ID: "run-1", Status: advisordomain.RunDone,
		Kind: advisordomain.RunKindDeep, Model: record})
	if err != nil || len(got.Progress) != 3 || got.Progress[0].Why != "the ledger is down" ||
		got.Progress[1].Detail != "A2 Do releases pile up behind one seat?" {
		t.Errorf("a record's steps = %+v (%v), want its last lines with their steps", got.Progress, err)
	}
}

// finishedLog is an advisor log whose latest run is a deep run that ended,
// its record keeping its last lines and its progress record its whole trail.
type finishedLog struct {
	noticeLog
	record json.RawMessage
	trail  []advisordomain.ProgressLine
}

func (finishedLog) LiveProgress(context.Context, statelog.Freshness) (string, []advisordomain.ProgressLine, bool, error) {
	return "", nil, false, nil
}
func (finishedLog) PendingRequests(context.Context, statelog.Freshness) ([]advisordomain.RunRow, error) {
	return nil, nil
}
func (l finishedLog) LatestRun(context.Context, statelog.Freshness) (advisordomain.RunRow, bool, error) {
	return advisordomain.RunRow{ID: "run-1", Status: advisordomain.RunDone, Kind: advisordomain.RunKindDeep,
		Trigger: advisordomain.TriggerPerson, By: "ada", Model: l.record}, true, nil
}
func (l finishedLog) RunProgress(_ context.Context, run string, _ statelog.Freshness) ([]advisordomain.ProgressLine, error) {
	if run != "run-1" {
		return nil, nil
	}
	return l.trail, nil
}

// A FINISHED RUN KEEPS ITS WHOLE TRAIL READABLE: its record keeps only its
// last lines, and the latest run answers every line it wrote, from its
// progress record (each read and each verdict), oldest first; a run whose
// lines are gone answers the ones its record kept. Mutation: answer the
// record's lines, and the page loses the investigation once it ends.
func TestAFinishedRunKeepsItsWholeTrailReadable(t *testing.T) {
	t.Parallel()
	run := modelpass.DeepRun{Pass: advisor.Pass{Status: advisor.PassDone}, StoppedBy: modelpass.StoppedDone}
	var trail []advisordomain.ProgressLine
	for i := range 30 {
		line := advisordomain.ProgressLine{At: time.Unix(int64(100+i), 0).UTC(), Text: fmt.Sprintf("A%d confirmed: line %d", i+1, i)}
		trail = append(trail, line)
		run.Progress = append(run.Progress, modelpass.Progress{At: line.At, Text: line.Text})
	}
	record, err := json.Marshal(run.Kept())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Sources{Advisor: finishedLog{record: record, trail: trail}}.latestRun(t.Context())
	if err != nil || got == nil || got.Kind != "deep" || len(got.Progress) != 30 ||
		got.Progress[0].Text != "A1 confirmed: line 0" || got.Progress[29].Text != "A30 confirmed: line 29" {
		t.Fatalf("latest = %+v (%v), want the run's thirty lines, oldest first", got, err)
	}
	gone, err := Sources{Advisor: finishedLog{record: record}}.latestRun(t.Context())
	if err != nil || len(gone.Progress) != 3 || gone.Progress[2].Text != "A30 confirmed: line 29" {
		t.Errorf("latest with its lines gone = %+v (%v), want the record's last three", gone, err)
	}
}

// A PRESS'S RUN SAYS WHY ITS DEEP PART DID NOT RUN, and what it mended: a deep
// part kept from an investigation that concluded lately says so and from
// when, with no budget it never had; one that could not run says why; and a
// run's model summary counts the findings listed after a repair beside those
// dropped. Mutation: answer a kept part as a deep run that spent nothing, and
// the page says it investigated.
func TestAPressesRunSaysWhyItsDeepPartDidNotRun(t *testing.T) {
	t.Parallel()
	from := time.Unix(1000, 0).UTC()
	kept, err := json.Marshal(map[string]any{"status": "done", "skipped": "kept", "kept_from": from,
		"reason": "The deep investigation from 2 h ago is kept.", "listed": 2,
		"repaired": map[string]int{"fix_not_built": 1, "reworded": 1}, "dropped": map[string]int{"wrong_number": 1}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Sources{Advisor: finishedLog{record: kept}}.latestRun(t.Context())
	if err != nil || got.DeepSkipped == nil || *got.DeepSkipped != "kept" || got.DeepKeptFrom == nil ||
		!got.DeepKeptFrom.Equal(from) || got.Budget != nil {
		t.Fatalf("latest = %+v (%v), want the deep part kept from its time, no budget", got, err)
	}
	if got.Model.Repaired[advisor.RepairNoFix] != 1 || got.Model.Repaired[advisor.RepairReworded] != 1 ||
		got.Model.Dropped[advisor.DropWrongFact] != 1 {
		t.Errorf("model = %+v, want what was mended beside what was dropped", got.Model)
	}
	ran, err := json.Marshal(modelpass.DeepRun{Pass: advisor.Pass{Status: advisor.PassDone}, StoppedBy: "done"}.Kept())
	if err != nil {
		t.Fatal(err)
	}
	full, err := Sources{Advisor: finishedLog{record: ran}}.latestRun(t.Context())
	if err != nil || full.DeepSkipped != nil || full.Budget == nil || full.Model.Repaired == nil {
		t.Errorf("a run that investigated = %+v (%v), want its budget and no skip", full, err)
	}
}
