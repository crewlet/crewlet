package observe_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE LIVE PANEL SPEAKS THE RUN RECORD'S WORDS.
//
// It spoke two of its own, and one of them — `awaiting_input` — is not a word
// the record can write, so the dashboard, which asks "is a person needed?" in
// the record's vocabulary, never once saw a live run that was waiting on
// somebody. This package is the one that sees both vocabularies, so it holds
// them together: every status a run that still owns a box can be in is one a
// live entry carries, spelled identically, and `resumed` — a run whose turn has
// taken its result back — is not.
func TestTheLiveStatusesAreTheRunRecordsWords(t *testing.T) {
	t.Parallel()
	pairs := map[livestate.ExecutorStatus]string{
		livestate.ExecutorLaunching: sandbox.StatusLaunching,
		livestate.ExecutorRunning:   sandbox.StatusRunning,
		livestate.ExecutorAwaiting:  sandbox.StatusAwaiting,
		livestate.ExecutorReseed:    sandbox.StatusReseed,
		livestate.ExecutorAnswered:  sandbox.StatusAnswered,
	}
	for live, record := range pairs {
		if string(live) != record {
			t.Errorf("the panel says %q where the record says %q", live, record)
		}
	}
	for _, status := range sandbox.Active {
		valid := livestate.ExecutorStatus(status).Valid()
		if status == sandbox.StatusResumed && valid {
			t.Errorf("%q is a run that is over, and the panel would show it", status)
		}
		if status != sandbox.StatusResumed && !valid {
			t.Errorf("%q is a run in flight the panel cannot carry", status)
		}
	}
	if len(livestate.ExecutorStatuses) != len(sandbox.Active)-1 {
		t.Errorf("the panel carries %v, want every active status but resumed (%v)",
			livestate.ExecutorStatuses, sandbox.Active)
	}
}

func TestARecordRendersAsTheEntryItsRunWouldHaveAnnounced(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 6, 14, 9, 0, 0, 0, time.UTC)
	launched := created.Add(time.Minute)
	written := created.Add(time.Hour)
	item := &types.WorkItem{Backend: "native", ID: "t-1", Key: "ENG-1"}
	runs := []sandbox.PendingRun{
		{
			TurnID: "tn-parked", Role: "Coder", AgentHandle: "coder", Harness: "claude-code",
			SandboxID: "sb-1", Status: sandbox.StatusAwaiting, Owner: "core-2",
			TaskDescription: "Add retry to the webhook client\nand the rest of the brief",
			Label:           "Add retry to the webhook client",
			Question:        "which ceiling?", Audience: "founder", WorkItem: item,
			AudienceHandles: []string{"founder", "cto"},
			LaunchID:        "l-1", Launch: sandbox.LaunchRecord{StartedAt: launched},
			CreatedAt: created, UpdatedAt: written,
		},
		{TurnID: "tn-over", Role: "Coder", Status: sandbox.StatusResumed},
	}
	records := observe.SandboxRecords(runs)
	if len(records) != 1 {
		t.Fatalf("records = %+v, want only the run still in flight", records)
	}
	rec := records[0]
	if rec.LaunchID != "l-1" || !rec.WrittenAt.Equal(written) {
		t.Errorf("record names launch %q written %v", rec.LaunchID, rec.WrittenAt)
	}
	e := rec.Entry
	if e.Status != livestate.ExecutorAwaiting || e.Owner != "core-2" || e.WorkItem == nil ||
		e.WorkItem.Key != "ENG-1" || e.Question != "which ceiling?" || e.Audience != "founder" {
		t.Errorf("entry = %+v, want the record's facts", e)
	}
	// WHO THE QUESTION IS PUT TO rides the entry, because the record is the
	// only place it is kept and the panel shows the question to exactly
	// those seats whatever their reach to the seat that asked — and it is
	// the entry's own copy, so nothing the projection does to it reaches
	// back into the run it was read from.
	if !slices.Equal(e.AudienceHandles, []string{"founder", "cto"}) {
		t.Errorf("the entry's question is put to %v, want the record's [founder cto]",
			e.AudienceHandles)
	} else {
		e.AudienceHandles[0] = "rewritten"
		if runs[0].AudienceHandles[0] != "founder" {
			t.Error("the entry shares its audience with the run record it was read from")
		}
	}
	if e.Task != "Add retry to the webhook client" {
		t.Errorf("task = %q, want the label its launch announced", e.Task)
	}
	if e.StartedAt != launched.Format(time.RFC3339Nano) {
		t.Errorf("started at %q, want the launch's own instant", e.StartedAt)
	}
	// Parked with a box and no pause stamp: the box is held all the same,
	// and dated by the park itself — the reaper's reading.
	if e.PausedAt != written.Format(time.RFC3339Nano) {
		t.Errorf("paused at %q, want the held reading %s", e.PausedAt, written)
	}
}

// A RUN IS NAMED FROM ITS RECORD AS IT WAS ANNOUNCED: by the label its launch
// stored, which for an executor run is its turn's task rather than the
// heading its brief opens with.
func TestARecordIsNamedByTheLabelItsLaunchAnnounced(t *testing.T) {
	t.Parallel()
	run := sandbox.PendingRun{
		TurnID: "tn-1", Status: sandbox.StatusRunning,
		Label: "Ship the login fix", TaskDescription: "## Task\n\nthe turn's task\nand more",
	}
	records := observe.SandboxRecords([]sandbox.PendingRun{run})
	if len(records) != 1 {
		t.Fatalf("records = %+v, want the run in flight", records)
	}
	if got := records[0].Entry.Task; got != run.Label {
		t.Errorf("the run is named %q, want %q", got, run.Label)
	}
}

// A RECORD NAMES THE MEMBER ITS RUN WAS LAUNCHED ON, as the announcement did,
// so a run this process never saw announced is drawn with the entry and model
// it runs on.
func TestARecordNamesTheMemberItsRunWasLaunchedOn(t *testing.T) {
	t.Parallel()
	run := sandbox.PendingRun{
		TurnID: "tn-1", Status: sandbox.StatusRunning,
		Launch: sandbox.LaunchRecord{ProviderKey: "spare", Model: "claude-haiku"},
	}
	records := observe.SandboxRecords([]sandbox.PendingRun{run})
	if len(records) != 1 {
		t.Fatalf("records = %+v, want the run in flight", records)
	}
	if e := records[0].Entry; e.ProviderKey != "spare" || e.Model != "claude-haiku" {
		t.Errorf("the record names %q/%q, want the member the run was launched on",
			e.ProviderKey, e.Model)
	}
}

func TestAReconcileLandsTheRecordAndAFailedReadLandsNothing(t *testing.T) {
	t.Parallel()
	sink := &recordingSink{}
	runs := &fakeRuns{runs: []sandbox.PendingRun{
		{TurnID: "tn-1", Role: "Coder", Status: sandbox.StatusRunning},
	}}
	r := observe.NewSandboxReconciler(runs, sink)
	before := time.Now().UTC()
	if err := r.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(sink.calls) != 1 || len(sink.calls[0].records) != 1 {
		t.Fatalf("sink got %+v, want the one run", sink.calls)
	}
	// THE READ'S INSTANT IS TAKEN BEFORE THE READ, so a run announced while
	// the listing was in flight is newer than it.
	if sink.calls[0].asOf.Before(before) || sink.calls[0].asOf.After(runs.readAt) {
		t.Errorf("as of %v, want an instant before the read at %v", sink.calls[0].asOf, runs.readAt)
	}

	// An empty answer would clear the panel over a store blip.
	runs.err = errors.New("the coordination store could not be reached")
	if err := r.Reconcile(t.Context()); err == nil {
		t.Error("a failed read was not reported")
	}
	if len(sink.calls) != 1 {
		t.Errorf("a failed read landed %d reconciles, want none", len(sink.calls)-1)
	}
	if observe.NewSandboxReconciler(nil, sink) != nil || observe.NewSandboxReconciler(runs, nil) != nil {
		t.Error("a reconciler was built with nothing to read or nowhere to land it")
	}
}

type fakeRuns struct {
	runs   []sandbox.PendingRun
	err    error
	readAt time.Time
}

func (f *fakeRuns) ListActive(context.Context) ([]sandbox.PendingRun, error) {
	f.readAt = time.Now().UTC()
	return slices.Clone(f.runs), f.err
}

type reconcileCall struct {
	records []livestate.ExecutorRecord
	asOf    time.Time
}

type recordingSink struct {
	mu    sync.Mutex
	calls []reconcileCall
}

func (s *recordingSink) ReconcileExecutors(records []livestate.ExecutorRecord, asOf time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, reconcileCall{records, asOf})
}
