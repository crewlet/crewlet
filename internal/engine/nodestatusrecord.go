package engine

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seat"
)

// This node's status record (internal/coord's nodestatusrecord.go says why it
// is not on the presence) is written on a goroutine of its own, never the
// beat, as ROWS, each a key of its own. A pass every renew period
// ([seat.Host.RenewPeriod]) writes each row that differs from what it last
// wrote there (each model's last answer aside, which moves every call) or was
// last written [coord.ReconcileInterval] ago, four writes inside the bucket's
// age ([coord.StatusFreshness]), the node's own row last; then removes each
// row the node no longer has. Every row carries the presence epoch it was
// written under: a new claim wakes the loop, a moved epoch rewrites every
// row, and a node holding no presence writes nothing. A clean stop removes
// them as a [seat.StopStep], up to the latest epoch it asked to write at,
// never a later incarnation's ([coord.NodeStatusRecords]).

// statusRecords is this node's status record writer: while it runs, and, once
// its loop is stopped, until the teardown removes what it wrote.
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

	// wake asks the loop for a pass now ([statusRecordWriter.claimed]):
	// one buffered, so a claim while a pass runs is the next pass and two
	// claims before it are one. woken counts the claims that asked, which
	// is what a case reads to see that the host's hook reaches the loop.
	wake  chan struct{}
	woken atomic.Int64

	now func() time.Time

	// mu holds a pass whole, so a pass this node's loop runs and one a case
	// drives never interleave, and guards what the last one wrote.
	mu sync.Mutex

	// written is each row this node has written, or asked to, and not
	// removed, by its key: what it last wrote there and when. A row whose
	// write fails has no fingerprint, so the next pass writes it again or,
	// where it is no longer carried, removes what may have landed; a row
	// whose removal fails stays here, so the next pass removes it again.
	written map[coord.NodeStatusRowKey]writtenRow

	// through is the latest epoch this node has ASKED to write a record at,
	// landed or not: what its removal at a stop removes up to, so a write
	// whose answer was lost is removed too, and a later incarnation's record
	// under the same node id is not ([coord.NodeStatusRecords]).
	through int64
}

// startStatusRecords starts this node's writer, kept until stopStatusRecords.
// A node with no fleet store or no seat host has no presence to join a record
// to, and writes none.
func (e *Engine) startStatusRecords(ctx context.Context) {
	if e.backends == nil || e.backends.Fleet == nil || e.node == nil || e.node.Host() == nil {
		return
	}
	host := e.node.Host()
	w := newStatusRecordWriter(e.backends.Fleet, e.nodeID(), host.PresenceEpoch, e.statusRecordRows,
		host.RenewPeriod(), e.now)
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

// statusRecordClaimed is the seat host's word that this node's presence was
// claimed at a new epoch ([seat.Config].PresenceClaimed): the writer's next
// pass is now. Called on the beat, so it never blocks; a node whose writer
// has not started, or has stopped, has nothing to wake.
func (e *Engine) statusRecordClaimed(int64) {
	e.statusRecords.mu.Lock()
	w, running := e.statusRecords.writer, e.statusRecords.cancel != nil
	e.statusRecords.mu.Unlock()
	if running {
		w.claimed()
	}
}

// stopStatusRecords ends the writer's loop and waits out its pass in flight,
// so nothing writes the record behind its removal. What it wrote stands until
// [Engine.removeStatusRecord].
func (e *Engine) stopStatusRecords() {
	e.statusRecords.mu.Lock()
	cancel, done := e.statusRecords.cancel, e.statusRecords.done
	e.statusRecords.cancel, e.statusRecords.done = nil, nil
	e.statusRecords.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// removeStatusRecord removes this node's record, once its loop has stopped
// ([Engine.stopStatusRecords]), as ONE STEP OF THE STOP'S ALLOWANCE: see "Removed
// at a clean stop" above.
func (e *Engine) removeStatusRecord(ctx context.Context) {
	e.statusRecords.mu.Lock()
	w, running := e.statusRecords.writer, e.statusRecords.cancel != nil
	if !running {
		e.statusRecords.writer = nil
	}
	e.statusRecords.mu.Unlock()
	if w == nil || running {
		return
	}
	removeCtx, done := seat.StopStep(context.WithoutCancel(ctx))
	defer done()
	w.remove(removeCtx)
}

// statusRecordRows is this node's rows as its record carries them, each read
// from memory as the heartbeat once read them: the MCP rows off the bridges'
// own record, the model rows off each entry's plan, the runner probe's last
// finding, what the node's file says it runs on, and the runs of its seats
// waiting for an operator, off the executor coordinator's memory: what every
// data node raises executor_resume_blocked from, since this node may evaluate
// no alarm table of its own ([Engine.executorReading]); and what its growth
// products met at their vendors, which every node's notices read.
func (e *Engine) statusRecordRows(ctx context.Context) coord.NodeStatusRecord {
	// AND THE WINDOWS THE MODEL ROWS CARRY, kept in this node's own file, since
	// a clean stop deletes the record.
	e.keepReadings(ctx, e.Company())
	return coord.NodeStatusRecord{
		MCP: e.mcpStatus(ctx), Models: e.modelStatus(ctx), Runners: e.runnerStatus(),
		Infra: NodeInfra(e.boot), ResumesBlocked: e.resumesBlocked(), Growth: e.growthSignalRows(),
	}
}

// writtenRow is one row as this node last wrote it: its fingerprint
// ([rowFingerprints]) and when.
type writtenRow struct {
	fingerprint []byte
	at          time.Time
}

// newStatusRecordWriter is a writer for node's record, its loop not yet run.
func newStatusRecordWriter(store coord.NodeStatusRecords, node string, epoch func() (int64, bool),
	rows func(context.Context) coord.NodeStatusRecord, period time.Duration, now func() time.Time,
) *statusRecordWriter {
	return &statusRecordWriter{store: store, node: node, epoch: epoch, rows: rows, period: period,
		now: now, wake: make(chan struct{}, 1), written: map[coord.NodeStatusRowKey]writtenRow{}}
}

// run is the writer's loop: a pass, then a wait of a renew period, or less
// where the reconcile interval since the last write comes due first or a claim
// at a new presence epoch wakes it.
func (w *statusRecordWriter) run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-w.wake:
			if !timer.Stop() {
				// The timer fired as the wake came: its tick is
				// this pass too, and the channel is drained so the
				// Reset below starts from nothing pending.
				select {
				case <-timer.C:
				default:
				}
			}
		}
		timer.Reset(w.wait(w.pass(ctx)))
	}
}

// claimed asks the loop for a pass now, on a claim of the presence at a new
// epoch. Never blocks: a pass already asked for is the one this asks for.
func (w *statusRecordWriter) claimed() {
	w.woken.Add(1)
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// wait is how long after a pass the next one runs: a renew period, or until
// the reconcile interval since the oldest row's write comes due, whichever is
// first.
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
	if !answered || len(w.written) == 0 {
		return w.period
	}
	var oldest time.Time
	for _, row := range w.written {
		if oldest.IsZero() || row.at.Before(oldest) {
			oldest = row.at
		}
	}
	until := oldest.Add(coord.ReconcileInterval).Sub(w.now())
	if until <= 0 {
		return w.period
	}
	return min(w.period, until)
}

// pass builds this node's record and writes each of its rows that moved, its
// epoch moved, or whose reconcile interval since its last write has passed,
// the node's own row last; then removes each row it wrote that the record no
// longer carries (a server or an entry no longer configured, growth signals
// gone). It reports whether it answered: did all of that, or found nothing
// due.
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
	rows, err := r.Rows()
	if err != nil {
		log.WarnContext(ctx, "node_status_record_unwritten", "node", w.node, "epoch", epoch, "error", err,
			"detail", "its rows stand as last written; the next pass builds them again")
		return false
	}
	moved := rowFingerprints(r)
	carried := make(map[coord.NodeStatusRowKey]bool, len(rows))
	for _, row := range rows {
		carried[row.Key] = true
		was, ok := w.written[row.Key]
		if ok && bytes.Equal(was.fingerprint, moved[row.Key]) && r.At.Sub(was.at) < coord.ReconcileInterval {
			continue
		}
		w.through = max(w.through, epoch)
		// NO DEADLINE OF THE PASS'S: the write waits as long as the
		// coordination store's rule gives its bytes (internal/coord/kv's
		// settled writes), and only this loop waits on it.
		if err := w.store.PutNodeStatusRow(ctx, row); err != nil {
			// IT MAY HAVE LANDED: kept with no fingerprint, so the next
			// pass writes it again, or removes it once it is not carried.
			if !ok {
				was.at = r.At
			}
			w.written[row.Key] = writtenRow{at: was.at}
			if ctx.Err() == nil {
				log.WarnContext(ctx, "node_status_record_unwritten", "node", w.node, "epoch", epoch,
					"row", row.Key.Key(), "error", err, "detail", "its rows stand as last written, and read "+
						"as this node not saying once its presence moves or they age out; the next pass "+
						"writes them again")
			}
			return false
		}
		w.written[row.Key] = writtenRow{fingerprint: moved[row.Key], at: r.At}
	}
	for key := range w.written {
		if carried[key] {
			continue
		}
		if err := w.store.RemoveNodeStatusRow(ctx, key, w.through); err != nil {
			if ctx.Err() == nil {
				log.WarnContext(ctx, "node_status_row_not_removed", "node", w.node, "row", key.Key(),
					"error", err, "detail", "the row stands until the next pass removes it or it ages out")
			}
			return false
		}
		delete(w.written, key)
	}
	return true
}

// remove removes every row of this node's record, up to the latest epoch it
// asked to write one at, on whatever bound ctx carries. A writer that never
// asked has nothing of its own to remove.
func (w *statusRecordWriter) remove(ctx context.Context) {
	w.mu.Lock()
	through := w.through
	w.mu.Unlock()
	if through == 0 {
		return
	}
	if err := w.store.DeleteNodeStatus(ctx, w.node, through); err != nil {
		log.WarnContext(ctx, "node_status_record_not_removed", "node", w.node, "error", err,
			"detail", "readers stopped counting it with this node's presence; the bucket's age removes it")
	}
}

// rowFingerprints is what decides whether each row moved, by its key: its
// wire form with each model's last answer and the record's own instant left
// out, since the first moves on every call a model answers and the second on
// every pass. Its epoch is in it, so a pass that finds the presence epoch
// moved writes every row.
func rowFingerprints(r coord.NodeStatusRecord) map[coord.NodeStatusRowKey][]byte {
	r.At = time.Time{}
	if len(r.Models) > 0 {
		models := make([]coord.ModelStatus, len(r.Models))
		copy(models, r.Models)
		for i := range models {
			models[i].LastAnswer = time.Time{}
		}
		r.Models = models
	}
	out := map[coord.NodeStatusRowKey][]byte{}
	// A record that does not encode is one [coord.NodeStatusRecord.Rows]
	// refused already, so the pass never reads these.
	rows, _ := r.Rows()
	for _, row := range rows {
		out[row.Key] = row.Value
	}
	return out
}
