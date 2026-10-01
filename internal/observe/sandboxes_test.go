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
	pairs := map[livestate.SandboxStatus]string{
		livestate.SandboxLaunching: sandbox.StatusLaunching,
		livestate.SandboxRunning:   sandbox.StatusRunning,
		livestate.SandboxAwaiting:  sandbox.StatusAwaiting,
		livestate.SandboxReseed:    sandbox.StatusReseed,
	}
	for live, record := range pairs {
		if string(live) != record {
			t.Errorf("the panel says %q where the record says %q", live, record)
		}
	}
	for _, status := range sandbox.Active {
		valid := livestate.SandboxStatus(status).Valid()
		if status == sandbox.StatusResumed && valid {
			t.Errorf("%q is a run that is over, and the panel would show it", status)
		}
		if status != sandbox.StatusResumed && !valid {
			t.Errorf("%q is a run in flight the panel cannot carry", status)
		}
	}
	if len(livestate.SandboxStatuses) != len(sandbox.Active)-1 {
		t.Errorf("the panel carries %v, want every active status but resumed (%v)",
			livestate.SandboxStatuses, sandbox.Active)
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
			TurnID: "tn-parked", Role: "Coder", AgentHandle: "coder", CodingAgent: "claude-code",
			SandboxID: "sb-1", Status: sandbox.StatusAwaiting, Owner: "core-2",
			TaskDescription: "Add retry to the webhook client\nand the rest of the brief",
			Question:        "which ceiling?", Audience: "founder", WorkItem: item,
			LaunchID: "l-1", Launch: sandbox.LaunchRecord{ID: "l-1", StartedAt: launched},
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
	if e.Status != livestate.SandboxAwaiting || e.Owner != "core-2" || e.WorkItem == nil ||
		e.WorkItem.Key != "ENG-1" || e.Question != "which ceiling?" || e.Audience != "founder" {
		t.Errorf("entry = %+v, want the record's facts", e)
	}
	if e.Task != "Add retry to the webhook client" {
		t.Errorf("task = %q, want the first line, as the announcement cuts it", e.Task)
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
	records []livestate.SandboxRecord
	asOf    time.Time
}

type recordingSink struct {
	mu    sync.Mutex
	calls []reconcileCall
}

func (s *recordingSink) ReconcileSandboxes(records []livestate.SandboxRecord, asOf time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, reconcileCall{records, asOf})
}
