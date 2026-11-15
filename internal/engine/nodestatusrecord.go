package engine

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THIS NODE'S STATUS RECORD: its MCP, model and runner rows and its infra
// summary, written beside its presence rather than on it (internal/coord's
// nodestatusrecord.go says why they left the presence).
//
// # A loop of its own
//
// The rows are read from memory and written on a goroutine of their own, never
// on the beat: a node whose data path is slow writes its record late, and its
// presence, which is what keeps it counted and its seats held, is renewed on
// time whatever the record's write costs.
//
// # Written when it moves, and at least every reconcile interval
//
// A pass every renew period ([seat.Host.RenewPeriod]) builds the record and
// writes it when it differs from the one this node last wrote — comparing
// everything but each model's last answer, which moves on every call and would
// otherwise write the record on every pass of a busy node — so a sit-out, a
// failure or a server going down reaches every reader within a renew period,
// as it did on the presence. And whatever moved, it is written at least every
// [coord.ReconcileInterval]: four writes inside the bucket's age
// ([coord.StatusFreshness]), so a live node's record never ages out, and a
// model's last answer is never staler than that.
//
// # Under the presence epoch
//
// Each record carries the epoch of the presence this node held when it wrote
// it, and readers count it only while that presence holds at that epoch. A pass
// that finds the epoch moved writes at once, whatever it last wrote, since every
// reader stopped counting the record the old epoch named. A node that holds no
// presence writes nothing: its record would be one no reader counts, under an
// epoch that is about to move.
//
// # Removed at a clean stop
//
// A node that stops says so: its record goes with its presence rather than
// standing until the age passes. A removal that fails costs nothing, since its
// readers stopped counting it when its presence went.

// statusRecordRemoveBound is how long a stopping node gives the removal of its
// record: the reconcile interval, as the removal of its MCP listings has
// ([Engine.stopMCPListings]), past which the age removes it anyway.
const statusRecordRemoveBound = coord.ReconcileInterval

// statusRecords is this node's status record writer, while it runs.
type statusRecords struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	writer *statusRecordWriter
}

// statusRecordWriter writes one node's status record. Its inputs are
// functions, so a case runs it on a bubble's clock over a store of its own.
type statusRecordWriter struct {
	store coord.NodeStatusRecords
	node  string

	// epoch is the presence epoch this node holds now, and false while it
	// holds none ([seat.Host.PresenceEpoch]).
	epoch func() (int64, bool)

	// rows builds the record's rows from memory: its MCP, model and runner
	// rows and its infra summary. Node, Epoch and At are the writer's.
	rows func(ctx context.Context) coord.NodeStatusRecord

	// period is how often a pass runs: the host's renew period.
	period time.Duration

	now func() time.Time

	// mu holds a pass whole, so a pass this node's loop runs and one a case
	// drives never interleave, and guards what the last one wrote.
	mu sync.Mutex

	// What this node last wrote: its epoch, its fingerprint (everything but
	// each model's last answer and the instant), and when. written is false
	// until the first write lands, and after a write fails nothing here
	// moves, so the next pass writes again.
	written     bool
	writtenAt   time.Time
	epochOf     int64
	fingerprint []byte
}

// startStatusRecords starts this node's writer, kept until stopStatusRecords.
// A node with no fleet store or no seat host has no presence to join a record
// to, and writes none.
func (e *Engine) startStatusRecords(ctx context.Context) {
	if e.backends == nil || e.backends.Fleet == nil || e.node == nil || e.node.Host() == nil {
		return
	}
	host := e.node.Host()
	w := &statusRecordWriter{
		store:  e.backends.Fleet,
		node:   e.nodeID(),
		epoch:  host.PresenceEpoch,
		rows:   e.statusRecordRows,
		period: host.RenewPeriod(),
		now:    e.now,
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	e.statusRecords.mu.Lock()
	e.statusRecords.cancel, e.statusRecords.done, e.statusRecords.writer = cancel, done, w
	e.statusRecords.mu.Unlock()
	go func() {
		defer close(done)
		w.run(loopCtx)
	}()
}

// stopStatusRecords ends the writer and begins removing this node's record,
// and returns the wait for the removal.
//
// BESIDE THE MCP LISTINGS' REMOVAL, never after it: the teardown calls this
// before [Engine.stopMCPListings] and waits after, so a store that does not
// answer costs the stop one [statusRecordRemoveBound], not one per record
// family. Neither removal is a give-back of the stop's allowance
// ([seat.StopBudget]): what falls back when it fails is the bucket's age, and
// every reader stopped counting the record when the presence went.
func (e *Engine) stopStatusRecords(ctx context.Context) (wait func()) {
	e.statusRecords.mu.Lock()
	cancel, done, w := e.statusRecords.cancel, e.statusRecords.done, e.statusRecords.writer
	e.statusRecords.cancel, e.statusRecords.done, e.statusRecords.writer = nil, nil, nil
	e.statusRecords.mu.Unlock()
	if cancel == nil {
		return func() {}
	}
	cancel()
	removed := make(chan struct{})
	go func() {
		defer close(removed)
		<-done
		w.remove(ctx)
	}()
	return func() { <-removed }
}

// statusRecordRows is this node's rows as its record carries them, each read
// from memory as the heartbeat once read them: the MCP rows off the bridges'
// own record, the model rows off each entry's plan, the runner probe's last
// finding, and what the node's file says it runs on.
func (e *Engine) statusRecordRows(ctx context.Context) coord.NodeStatusRecord {
	return coord.NodeStatusRecord{
		MCP: e.mcpStatus(ctx), Models: e.modelStatus(ctx), Runners: e.runnerStatus(),
		Infra: NodeInfra(e.boot),
	}
}

// run is the writer's loop: a pass, then a wait of a renew period, or less
// where the reconcile interval since the last write comes due first.
func (w *statusRecordWriter) run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		timer.Reset(w.wait(w.pass(ctx)))
	}
}

// wait is how long after a pass the next one runs: a renew period, or until
// the reconcile interval since the last write comes due, whichever is first.
// After a pass that did not answer — the store refused the write, or this node
// held no presence — a renew period, so a failing store is asked again on the
// beat's own schedule rather than in a loop.
//
// NEVER NOTHING: a pass that answered has written whatever was due, so the
// reconcile is in the future; were it not, the next pass would run at once
// and find the same, and the loop would spin. A renew period then, which is
// the longest a write can be late by anyway.
func (w *statusRecordWriter) wait(answered bool) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !answered || !w.written {
		return w.period
	}
	until := w.writtenAt.Add(coord.ReconcileInterval).Sub(w.now())
	if until <= 0 {
		return w.period
	}
	return min(w.period, until)
}

// due reports whether the reconcile interval since the last write has passed.
// Called under mu.
func (w *statusRecordWriter) due(now time.Time) bool {
	return !w.written || now.Sub(w.writtenAt) >= coord.ReconcileInterval
}

// pass builds this node's record and writes it where it moved, its epoch
// moved, or the reconcile interval since the last write has passed. It reports
// whether it answered: wrote the record, or found it unmoved and not yet due.
func (w *statusRecordWriter) pass(ctx context.Context) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	epoch, held := w.epoch()
	if !held {
		return false
	}
	// THE ROWS ARE READ WITHIN A PASS: every input is in memory, but an MCP
	// row's judgement may read what the integration minting its credential
	// last recorded, which on a wedged store would otherwise hold the loop.
	// A read the bound cut short keeps that row's last judgement.
	buildCtx, cancel := context.WithTimeout(ctx, w.period)
	r := w.rows(buildCtx)
	cancel()
	r.Node, r.Epoch, r.At = w.node, epoch, w.now()
	moved := recordFingerprint(r)
	if w.written && epoch == w.epochOf && bytes.Equal(moved, w.fingerprint) && !w.due(r.At) {
		return true
	}
	// NO DEADLINE OF THE PASS'S: the write waits as long as the coordination
	// store's rule gives its bytes (internal/coord/kv's settled writes),
	// which across a link at the floor is the record's crossing, and only
	// this loop waits on it.
	if err := w.store.PutNodeStatus(ctx, r); err != nil {
		if ctx.Err() == nil {
			log.WarnContext(ctx, "node_status_record_unwritten", "node", w.node, "epoch", epoch,
				"error", err, "detail", "its rows stand as last written, and read as this node "+
					"not saying once its presence moves or the record ages out; the next pass "+
					"writes it again")
		}
		return false
	}
	w.written, w.writtenAt, w.epochOf, w.fingerprint = true, r.At, epoch, moved
	return true
}

// remove removes this node's record, bounded by [statusRecordRemoveBound].
func (w *statusRecordWriter) remove(ctx context.Context) {
	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusRecordRemoveBound)
	defer cancel()
	if err := w.store.DeleteNodeStatus(removeCtx, w.node); err != nil {
		log.WarnContext(removeCtx, "node_status_record_not_removed", "node", w.node, "error", err,
			"detail", "readers stopped counting it with this node's presence; the bucket's age removes it")
	}
}

// recordFingerprint is what decides whether a record moved: its wire form with each
// model's last answer and the record's own instant left out, since the first
// moves on every call a model answers and the second on every pass.
func recordFingerprint(r coord.NodeStatusRecord) []byte {
	r.At = time.Time{}
	if len(r.Models) > 0 {
		models := make([]coord.ModelStatus, len(r.Models))
		copy(models, r.Models)
		for i := range models {
			models[i].LastAnswer = time.Time{}
		}
		r.Models = models
	}
	raw, err := r.Encode()
	if err != nil {
		// A record that does not encode is one the write refuses too, so
		// the pass tries the write and reports what refused it: a written
		// record's fingerprint is never nil.
		return nil
	}
	return raw
}
