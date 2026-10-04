package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The object store, wired — ADR-0026.
//
// # What every node runs, and what one data node does
//
// Every node holds the [objstore.Store] over the backend its Tier A names
// ([Backends.Objects]) and reads and writes chunks through it: a stateless
// node uploads and downloads files exactly as a data node does, and the
// backend keeps the copies. The one thing left for the engine is the
// COLLECTOR — deleting what no row names, and auditing what the backend has
// lost — which needs the replicated estate, so it runs on a data node, as a
// fleet duty ([objectCollectorDuty]).

// objectStore is this node's part in the object store.
type objectStore struct {
	store *objstore.Store

	// collector is the duty's loop, nil until the native runtime starts it
	// on a data node; collectorMu serialises starting it against stopping
	// it, and collectorStopped is set, under it, once
	// [Engine.stopObjectCollector] has run — the loop never starts after
	// that.
	//
	// A LOCK AND NOT AN ATOMIC FLAG, because the check and the start have
	// to be one step: the native runtime that starts the collector may be
	// brought up by an apply on its own goroutine, as late as the teardown
	// itself, and a start that read the flag clear and then lost the
	// processor to a stop that found nothing to stop would leave a loop
	// nothing ends.
	collectorMu      sync.Mutex
	collector        *loop
	collectorStopped bool

	// duty is the collector's state between the duty's turns. ATOMIC
	// because the alarm tick reads it on another goroutine.
	duty atomic.Pointer[collectorDuty]
}

// startObjects builds this node's object store over the backend its Tier A
// named, which [OpenBackends] opened and agreed with the fleet.
func (e *Engine) startObjects() error {
	if e.backends == nil || e.backends.Objects == nil || e.backends.Fleet == nil {
		return nil
	}
	store, err := objstore.NewStore(e.backends.Objects, e.backends.Fleet, e.incarnation)
	if err != nil {
		return fmt.Errorf("engine: the object store: %w", err)
	}
	e.objects = &objectStore{store: store}
	return nil
}

// objectCollectorDuty is the fleet singleton that collects and audits.
const objectCollectorDuty = "object-collector"

// collectorPoll is how often the collector's duty is asked for.
//
// A MINUTE: the passes run hourly and daily, so the poll only decides how soon
// a node notices it has been handed the duty — a peer that held it going away —
// and how soon a pass that failed is tried again.
const collectorPoll = time.Minute

// collectorDutyTTL is how long the duty outlives a holder that stopped
// renewing it.
//
// TEN MINUTES, ten polls rather than the three every other singleton uses: the
// lease is renewed between passes, never during one, and a collection lists
// the whole store — a pass that outlived a three-poll lease would hand the
// duty to a peer mid-pass. Two collectors at once are safe, since every
// deletion is judged under its chunk's lock, and only wasteful; ten minutes is
// what a pass over a few million chunks takes, and a dead holder costs no more
// than ten minutes of a pass that is due hourly.
const collectorDutyTTL = 10 * collectorPoll

// collectorDuty is what the collector's duty remembers between turns.
type collectorDuty struct {
	collector *collect.Collector

	// mu guards the schedule, which only the loop writes and the alarm
	// tick reads.
	mu sync.Mutex
	// holding is whether this node held the duty at its last turn: only
	// the holder's audit is the fleet's, so only the holder reports it.
	holding bool
	// collected and audited are when this node — or, read from the fleet's
	// record when it took the duty, its last holder — last ran each pass.
	collected, audited time.Time
	// resumed is whether the schedule has been read back from the fleet's
	// record since this node took the duty.
	resumed bool
}

// collectionReport is what the collector's duty records in the coordination
// store after each pass, and what every node's status surface reads: who ran
// it, against which backend, and what it found.
type collectionReport struct {
	Node    string         `json:"node"`
	Backend string         `json:"backend"`
	Status  collect.Status `json:"status"`
}

// startObjectCollector starts the collector's duty against the estate's
// references. A no-op on a node with no object store, and once the collector
// has been stopped.
func (e *Engine) startObjectCollector(ctx context.Context, refs collect.References) error {
	o := e.objects
	if o == nil {
		return nil
	}
	o.collectorMu.Lock()
	defer o.collectorMu.Unlock()
	if o.collectorStopped || o.collector != nil {
		return nil
	}
	c, err := collect.New(collect.Options{Store: o.store, References: refs})
	if err != nil {
		return fmt.Errorf("engine: the object collector: %w", err)
	}
	duty := &collectorDuty{collector: c}
	o.duty.Store(duty)
	claim := e.workerDuty(objectCollectorDuty, collectorDutyTTL)
	o.collector = startLoop(ctx, sleep, func(ctx context.Context) time.Duration {
		e.collectorTurn(ctx, claim, duty)
		return collectorPoll
	})
	return nil
}

// collectorTurn is one turn of the duty: claim it, and run whichever pass is
// due.
func (e *Engine) collectorTurn(ctx context.Context, claim func(context.Context) (bool, error),
	duty *collectorDuty) {

	mine := claim == nil // a node with nobody to claim from runs it alone
	if claim != nil {
		held, err := claim(ctx)
		if err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "object_collector_duty_unclaimed", "error", err)
		}
		mine = err == nil && held
	}
	duty.mu.Lock()
	duty.holding = mine
	if !mine {
		duty.resumed = false
	}
	resume := mine && !duty.resumed
	duty.mu.Unlock()
	if !mine {
		return
	}
	if resume {
		e.resumeCollector(ctx, duty)
	}

	now := time.Now()
	duty.mu.Lock()
	collectDue := now.Sub(duty.collected) >= collect.CollectInterval
	auditDue := now.Sub(duty.audited) >= collect.AuditInterval
	duty.mu.Unlock()
	switch {
	case collectDue:
		_, err := duty.collector.Collect(ctx)
		duty.ran(&duty.collected, collect.CollectInterval, err)
	case auditDue:
		_, err := duty.collector.Audit(ctx)
		duty.ran(&duty.audited, collect.AuditInterval, err)
	default:
		return
	}
	if ctx.Err() == nil {
		e.recordCollection(ctx, duty)
	}
}

// collectorRetry is how soon a pass that failed is tried again.
//
// TEN MINUTES: the failures that stop a pass — a store that did not answer, a
// barrier on a log at its ceiling — are usually over in minutes, and a pass
// asked of a failing store every poll would be a full listing a minute against
// the dependency least able to take it.
const collectorRetry = 10 * time.Minute

// ran records a pass that ended at now: due again an interval later, or — if
// it failed — [collectorRetry] later.
func (d *collectorDuty) ran(last *time.Time, interval time.Duration, err error) {
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		*last = now.Add(collectorRetry - interval)
		return
	}
	*last = now
}

// resumeCollector picks the schedule up from the fleet's record when this
// node takes the duty, so a duty that moved does not run every pass again at
// once: the record says when its last holder ran each.
func (e *Engine) resumeCollector(ctx context.Context, duty *collectorDuty) {
	raw, found, err := e.backends.Fleet.ObjectCollection(ctx)
	var last collectionReport
	if err == nil && found {
		err = json.Unmarshal(raw, &last)
	}
	if err != nil {
		// RUN THE PASSES rather than wait on a record that cannot be read:
		// the cost is a pass run early, never one skipped.
		log.WarnContext(ctx, "object_collector_schedule_unread", "error", err)
	}
	duty.mu.Lock()
	defer duty.mu.Unlock()
	duty.resumed = true
	if last.Status.Collect.At.After(duty.collected) {
		duty.collected = last.Status.Collect.At
	}
	if last.Status.Audit.At.After(duty.audited) {
		duty.audited = last.Status.Audit.At
	}
}

// recordCollection writes the collector's status where every node reads it.
func (e *Engine) recordCollection(ctx context.Context, duty *collectorDuty) {
	raw, err := json.Marshal(collectionReport{
		Node: e.id, Backend: e.backends.objectsIdentity, Status: duty.collector.Status(),
	})
	if err == nil {
		err = e.backends.Fleet.RecordObjectCollection(ctx, raw)
	}
	if err != nil {
		log.WarnContext(ctx, "object_collection_unrecorded", "error", err,
			"detail", "the status surfaces show the previous pass until the next one records")
	}
}

// stopObjectCollector ends the collector's duty for good, waiting out a pass
// in flight. Nil-safe.
//
// BEFORE [Engine.releaseDuties], with every other loop that claims a duty: a
// loop still running past the release takes the duty straight back.
func (e *Engine) stopObjectCollector() {
	o := e.objects
	if o == nil {
		return
	}
	o.collectorMu.Lock()
	o.collectorStopped = true
	l := o.collector
	o.collector = nil
	o.collectorMu.Unlock()
	if l != nil {
		l.stop()
	}
}

// objectsReading fills the object store's half of an alarm reading: what the
// collector's last audit found missing — on the node holding the collector's
// duty, and nothing on any other, since the store is one the whole fleet
// shares and one node's count of it is the fleet's.
func (e *Engine) objectsReading(_ time.Time, out *statelog.Reading) {
	o := e.objects
	if o == nil {
		return
	}
	duty := o.duty.Load()
	if duty == nil {
		return
	}
	duty.mu.Lock()
	holding := duty.holding
	duty.mu.Unlock()
	if holding {
		out.ObjectsMissing = duty.collector.Status().Audit.Missing
	}
}

// backgroundBarriers is the barrier records a day the object store's collector
// puts on the domain's log, whatever the company's seats do:
// [collect.PinsPerDay] on the log of a domain a declared table names
// ([references.All]) and on no other. ONE collector runs in the fleet, so it is
// the count, never a count per node.
func (e *Engine) backgroundBarriers(domain string) int {
	if e.objects == nil || !pinnedDomains()[domain] {
		return 0
	}
	return collect.PinsPerDay
}

// pinnedDomains is every domain a declared table names — the domains whose
// logs the collector pins.
var pinnedDomains = sync.OnceValue(func() map[string]bool {
	out := map[string]bool{}
	for _, t := range references.All {
		out[t.Domain] = true
	}
	return out
})

// errNoObjectStore is a read of a chunk on an engine built with no object
// store — a test's.
var errNoObjectStore = errors.New("engine: this node runs no object store")

// GetChunk reads one chunk — the backup's read, which carries every chunk its
// copy names.
func (e *Engine) GetChunk(ctx context.Context, h objstore.Hash) ([]byte, error) {
	if e.objects == nil {
		return nil, errNoObjectStore
	}
	return e.objects.store.Get(ctx, h)
}

// ObjectStore is this node's object store as the tools take it — a NIL
// INTERFACE on a node running none, so the file tools that need the bytes are
// omitted rather than registered and broken.
func (e *Engine) ObjectStore() builtin.ObjectStore {
	if e.objects == nil {
		return nil
	}
	return e.objects.store
}

// Objects is this node's object store, nil on a node running none — what a
// surface streaming a file's bytes reads and writes through.
func (e *Engine) Objects() *objstore.Store {
	if e.objects == nil {
		return nil
	}
	return e.objects.store
}

// ObjectsStream is the broker stream the company's chunks live in, for the
// backup — empty where they live outside the broker (an S3 bucket), which the
// backup then copies chunk by chunk.
func (e *Engine) ObjectsStream() string {
	if e.backends == nil || e.backends.objectsStream == "" {
		return ""
	}
	return e.backends.objectsStream
}

// ObjectCollection is the collector's last report as the fleet recorded it,
// for the status surfaces — false when no pass has recorded one.
func ObjectCollection(ctx context.Context, fleet CollectionRecords) (CollectionReport, bool, error) {
	raw, found, err := fleet.ObjectCollection(ctx)
	if err != nil || !found {
		return CollectionReport{}, false, err
	}
	var r collectionReport
	if err := json.Unmarshal(raw, &r); err != nil {
		return CollectionReport{}, false, fmt.Errorf("engine: the object collection record is unreadable: %w", err)
	}
	return CollectionReport(r), true, nil
}

// CollectionRecords is where the collector's reports are recorded — the
// coordination store's object family, narrowed to the one read.
type CollectionRecords interface {
	ObjectCollection(ctx context.Context) ([]byte, bool, error)
}

// CollectionReport is the collector's last report — see [collectionReport].
type CollectionReport struct {
	Node    string
	Backend string
	Status  collect.Status
}
