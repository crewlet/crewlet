package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
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
// ([Backends.Objects]) and reads and writes objects through it: a stateless
// node uploads and downloads files exactly as a data node does, and the
// backend keeps the copies. The one thing left for the engine is the
// COLLECTOR — deleting what no row names, and auditing what the backend has
// lost — which needs the replicated estate, so it runs on a data node, as a
// fleet duty ([objectCollectorDuty]).
//
// # A pass keeps the duty it runs under
//
// The duty's turn — the read of the fleet's record when the duty has just been
// taken, and the pass — runs on a KEPT duty ([dutyRenewal.keep]): the lease is
// renewed four times a [collectorDutyTTL], every 45 seconds, for as long as the
// pass runs, so a pass may outlive any number of leases. It is stopped the
// moment a renewal answers that another node holds the duty, and once no
// renewal has confirmed it for two and a quarter minutes — the lease less one
// renewal, the longest no other node can have claimed it — while a renewal the
// store refuses, or holds on to while its leader moves, costs it nothing: the
// call gives up within one renewal, and the next is asked inside the window.
//
// Renewed only between passes, as it was, the lease lapsed under any pass
// longer than it: an audit asks the store about every object a live row
// names, which at a large company's two million objects is longer than any
// lease a dead holder should be allowed to keep the duty dark for. A peer took
// the duty mid-pass and ran its own beside it, and both wrote the fleet's ONE
// record of the collector (coord's ObjectStores.RecordObjectCollection), whose
// contract is one writer at a time — the older pass's report, written last,
// replacing the new holder's. So a pass that ends without the duty, or unable
// to say it still holds it, RECORDS NOTHING: the node that holds the duty
// records its own.

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
	if e.backends == nil || e.backends.Objects == nil {
		return nil
	}
	store, err := objstore.NewStore(e.backends.Objects)
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
// THREE POLLS, the ratio every other singleton here keeps: one missed claim
// must not hand the duty to a peer. It no longer has to outlive a PASS, which
// renews it as it runs ([collectorDuty.renewal]). Sized to outlive one, it was
// ten minutes, and an audit of a large store outlived that many times over: a
// peer took the duty mid-audit, ran its own beside it, and the two wrote the
// fleet's one record in turn. What it bounds now is how long the duty stays
// dark after its holder dies — three minutes, against a collection due hourly.
const collectorDutyTTL = 3 * collectorPoll

// collectorDuty is what the collector's duty remembers between turns.
type collectorDuty struct {
	collector *collect.Collector

	// renewal is how a turn keeps the duty while it runs: renewed four times
	// a [collectorDutyTTL], on a test's own clock in the tests.
	renewal dutyRenewal

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

	// decidesFrom is every domain whose rows name the objects it collects
	// and audits ([copiesNamedBy]); declines is whether this node is
	// declining the duty because its copy of one is out of service, and
	// giveBack gives the duty back where it holds it on a turn that does
	// ([Engine.workerGiveBack]) — nil holds no lease to give.
	decidesFrom []string
	declines    dutyDecline
	giveBack    func(context.Context) (bool, error)
}

// copiesNamedBy is every domain whose rows refs reads — the domains a declared
// table names an object in — once each, in refs' order: what the collector
// decides from, read off the references it was built with so no list beside
// them can drift from what it reads.
func copiesNamedBy(refs collect.References) []string {
	var out []string
	for _, src := range refs {
		if !slices.Contains(out, src.Name()) {
			out = append(out, src.Name())
		}
	}
	return out
}

// newCollectorDuty is the duty's state over c, renewed on the system's clock,
// deciding from the domains named and giving the duty back through giveBack
// when it declines — nil where there is no lease to give back.
//
// ITS RENEWALS ASK NO DECLINE: unlike the embedding duty's training, a pass
// reads the rows that name objects through the read authority, which refuses a
// copy out of service ([dutyDecline]), so a pass whose copy goes wrong under it
// stops at its next read on its own, and the next turn declines.
func newCollectorDuty(c *collect.Collector, decidesFrom []string,
	giveBack func(context.Context) (bool, error)) *collectorDuty {

	return &collectorDuty{collector: c,
		renewal:     newDutyRenewal(objectCollectorDuty, collectorDutyTTL),
		decidesFrom: decidesFrom, giveBack: giveBack}
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
	duty := newCollectorDuty(c, copiesNamedBy(refs), e.workerGiveBack(objectCollectorDuty))
	o.duty.Store(duty)
	claim := e.workerDuty(objectCollectorDuty, collectorDutyTTL)
	o.collector = startLoop(ctx, sleep, func(ctx context.Context) time.Duration {
		e.collectorTurn(ctx, claim, duty)
		return collectorPoll
	})
	return nil
}

// collectorTurn is one turn of the duty: claim it, keep it while whichever
// pass is due runs, and record what the pass found if the duty is still this
// node's when it ends.
//
// DECLINED FIRST, before the claim, while this node's copy of a domain whose
// rows name objects is out of service ([dutyDecline]) — given back if this
// node holds it, and otherwise declined as an unanswered claim is: the node
// runs no pass, keeps what it believed, and forgets the resume, since another
// node may take the duty and record a newer pass meanwhile ([collectorDuty.settle]).
func (e *Engine) collectorTurn(ctx context.Context, claim func(context.Context) (bool, error),
	duty *collectorDuty) {

	if _, out := duty.declines.check(ctx, collectorDecline, e.copyOutOfService,
		duty.decidesFrom); out {
		duty.declines.giveBack(ctx, collectorDecline, duty.giveBack)
		duty.mu.Lock()
		duty.resumed = false
		duty.mu.Unlock()
		return
	}
	asked := duty.renewal.clock.Now() // BEFORE the claim: what it confirms runs from here
	held, err := true, error(nil)     // a node with nobody to claim from runs it alone
	if claim != nil {
		held, err = claim(ctx)
	}
	mine, resume := duty.settle(held, err)
	if err != nil && ctx.Err() == nil {
		log.WarnContext(ctx, "object_collector_duty_unclaimed", "error", err)
	}
	if !mine {
		return
	}
	// FROM HERE THE TURN KEEPS THE DUTY AS IT GOES — the resume's read and
	// the pass alike — so a pass may outlive any number of leases
	// ([dutyRenewal.keep]), and is stopped once the duty is not this node's.
	work, kept := duty.renewal.keep(ctx, claim, asked)
	ran := e.collectorPass(work, duty, resume)
	held, err = kept.Stop()
	// A PASS THAT ENDED WITHOUT THE DUTY RECORDS NOTHING: the fleet's record
	// has one writer at a time, and a node that lost the duty — or cannot say
	// it still holds it — writing its report would replace the holder's.
	// Settled by the same rule as the claim, so the two answers cannot be
	// read differently.
	mine, _ = duty.settle(held, err)
	if !mine || !ran || ctx.Err() != nil {
		return
	}
	e.recordCollection(ctx, duty)
}

// settle applies an answer to "does this node hold the duty?" — the claim at
// the top of a turn, or what a pass's renewals concluded as it ended — to what
// the duty remembers, and reports whether this node may act as its holder, and
// whether it has to read the fleet's record first. THREE-VALUED (ADR-0005):
// held holds; not held lets the duty and its alarm go; unknown keeps both.
func (d *collectorDuty) settle(held bool, err error) (mine, resume bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		// UNKNOWN IS NEITHER: an answer the store could not give says
		// nothing about who holds the duty, so this node acts as no
		// holder — it cannot say the duty is its own — and keeps whatever
		// it believed before, the alarm its last audit raised included.
		// Read as "not mine", a two-second store blip would clear an
		// alarm about lost files on the one node reporting it.
		//
		// BUT IT FORGETS THE RESUME. While nobody can say who holds the
		// duty, another node may take it and record a newer pass — an
		// audit that found files lost — so the next claim that answers
		// held reads the fleet's record again rather than run from this
		// node's own, older status and write those findings out of the
		// record. Resuming twice costs a read: Restore keeps the newer of
		// each half.
		d.resumed = false
		return false, false
	}
	d.holding = held
	if !held {
		d.resumed = false
		return false, false
	}
	return true, !d.resumed
}

// collectorPass reads the fleet's record first if the turn has to, runs
// whichever pass is due, and reports whether one ran.
func (e *Engine) collectorPass(ctx context.Context, duty *collectorDuty, resume bool) bool {
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
		return false
	}
	return true
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

// resumeCollector picks the schedule and the findings up from the fleet's
// record when this node takes the duty, so a duty that moved does not run every
// pass again at once — the record says when its last holder ran each — and
// does not forget what the last audit found.
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
	// THE FINDINGS AS WELL AS THE SCHEDULE: the next pass this node records
	// carries its other half from here, and seeded with nothing it would
	// write the last audit's findings — and the alarm they raise — out of
	// the fleet's record an hour after the duty moved.
	duty.collector.Restore(last.Status)
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

// objectsReading fills the object store's half of an alarm reading: the files
// the collector's last audit to run to its end found missing, damaged or
// unanswered — on
// the node holding the collector's duty, and nothing on any other, since the
// store is one the whole fleet shares and one node's count of it is the
// fleet's. FROM THE FINDINGS, never the last attempt: an audit that failed
// part of the way through counted a floor, and reading it would clear the
// alarm whenever the store stopped answering.
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
	if !holding {
		return
	}
	if found := duty.collector.Status().Audit.Found; found != nil {
		out.ObjectsMissing = found.Lost()
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

// errNoObjectStore is a read of an object on an engine built with no object
// store — a test's.
var errNoObjectStore = errors.New("engine: this node runs no object store")

// OpenObject streams one object back, checked against the row that names it
// ([objstore.Store.Open]) — the backup's read, which copies every object a
// required table of its copy of the estate names.
func (e *Engine) OpenObject(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
	if e.objects == nil {
		return nil, errNoObjectStore
	}
	return e.objects.store.Open(ctx, o)
}

// StatObject asks the store what it holds under k — the backup's check of an
// object a stream snapshot carries.
func (e *Engine) StatObject(ctx context.Context, k objstore.Key) (objstore.Info, error) {
	if e.objects == nil {
		return objstore.Info{}, errNoObjectStore
	}
	return e.objects.store.Stat(ctx, k)
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

// ObjectsStream is the broker stream the company's objects live in, for the
// backup — empty where they live outside the broker (an S3 bucket), which the
// backup then copies object by object.
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
