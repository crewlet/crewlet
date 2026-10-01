package observe

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// RunLister is the durable record of the fleet's coding runs. Satisfied by
// *sandbox.CoordStore.
type RunLister interface {
	ListActive(ctx context.Context) ([]sandbox.PendingRun, error)
}

// SandboxSink is where a reconcile lands. Satisfied by api/stream.Service,
// which pushes the sandbox set when the reconcile moved it.
type SandboxSink interface {
	ReconcileSandboxes(records []livestate.SandboxRecord, asOf time.Time)
}

// SandboxReconciler reads the durable run record back over the live projection's
// running-runs set: once at boot, and every [livestate.ReconcileInterval] after.
//
// The lifecycle events keep that set current to the moment, but the stream is
// lossy both ways and the set is in memory: a process that came up mid-run
// never saw its runs start, and a completion that never arrived left a run on
// the panel for good. The record in the coordination store is the truth about
// which runs exist — every node opens it, so every node's panel is the fleet's
// — and this is what puts the panel back in step with it. See
// [livestate.LiveState.ReconcileSandboxes] for which of the two wins when.
type SandboxReconciler struct {
	runs     RunLister
	sink     SandboxSink
	interval time.Duration
	now      func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewSandboxReconciler builds a reconciler, or nil for a node with no run record
// or no projection — which a nil reconciler's methods treat as nothing to do.
func NewSandboxReconciler(runs RunLister, sink SandboxSink) *SandboxReconciler {
	if runs == nil || sink == nil {
		return nil
	}
	return &SandboxReconciler{
		runs: runs, sink: sink, interval: livestate.ReconcileInterval,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// Reconcile reads the record once and lands it.
//
// THE READ'S INSTANT IS TAKEN BEFORE THE READ, and on the clock the projection
// stamps what it learns with: a run whose start arrived while the listing was
// in flight is newer than the read, and the projection keeps it.
//
// A record that could not be read changes nothing and is returned: an empty
// listing would read as "no run is in flight" and clear the panel over a
// store blip.
func (r *SandboxReconciler) Reconcile(ctx context.Context) error {
	if r == nil {
		return nil
	}
	asOf := r.now()
	runs, err := r.runs.ListActive(ctx)
	if err != nil {
		return err
	}
	r.sink.ReconcileSandboxes(SandboxRecords(runs), asOf)
	return nil
}

// Start runs the reconcile every interval until Stop, the first one interval
// from now — the boot's own reconcile is the caller's, made before the listener
// binds so the first snapshot a socket gets already holds the fleet's runs.
func (r *SandboxReconciler) Start(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cancel != nil {
		return
	}
	loop, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// The goroutine closes ITS OWN channel, never the field, which Stop
	// clears while the goroutine is still unwinding.
	done := make(chan struct{})
	r.cancel, r.done = cancel, done
	go func() {
		defer close(done)
		tick := time.NewTicker(r.interval)
		defer tick.Stop()
		for {
			select {
			case <-loop.Done():
				return
			case <-tick.C:
			}
			if err := r.Reconcile(loop); err != nil && loop.Err() == nil {
				// WARN, and try again next interval: what a failed read
				// costs is a panel as stale as its events left it, and the
				// next read repairs it.
				log.WarnContext(loop, "sandbox_panel_not_reconciled", "error", err,
					"hint", "the running-runs panel shows what the events said; "+
						"the durable run record is read again next interval")
			}
		}
	}()
}

// Stop ends the loop and waits for it.
func (r *SandboxReconciler) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.cancel, r.done = nil, nil
	r.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// SandboxRecords renders the durable runs as the projection's records.
//
// A run whose status is not one a live entry carries is left out: `resumed` is a
// run whose turn has already taken its result back, so it is over rather than
// in flight.
func SandboxRecords(runs []sandbox.PendingRun) []livestate.SandboxRecord {
	out := make([]livestate.SandboxRecord, 0, len(runs))
	for _, run := range runs {
		status := livestate.SandboxStatus(run.Status)
		if !status.Valid() {
			continue
		}
		started := run.CreatedAt
		if launch := run.LaunchFacts(); !launch.StartedAt.IsZero() {
			started = launch.StartedAt
		}
		entry := livestate.SandboxEntry{
			TurnID: run.TurnID, Role: run.Role, AgentHandle: run.AgentHandle,
			AgentID: run.AgentID, CodingAgent: run.CodingAgent, SandboxID: run.SandboxID,
			// The label the announcement would have carried, cut the
			// same way. The record keeps the task rather than the brief,
			// so an entry this process never saw announced is labelled
			// by what the run was asked to do.
			Task:      sandbox.Summarise(run.TaskDescription),
			Status:    status,
			StartedAt: isoOrEmpty(started),
			Question:  run.Question, Audience: run.Audience,
			WorkItem: run.WorkItem,
			Owner:    run.Owner,
		}
		// HELD, NOT STAMPED — the reading the pause reaper acts on and the
		// `sandbox_runs` board draws, so a parked run whose pause instant
		// never reached its row still reads as held.
		if held, ok := run.HeldSince(); ok {
			entry.PausedAt = isoOrEmpty(held)
		}
		out = append(out, livestate.SandboxRecord{
			Entry: entry, LaunchID: run.LaunchID, WrittenAt: run.UpdatedAt,
		})
	}
	return out
}

func isoOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
