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
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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

	// usage is the bucket's last-read usage against its reservation,
	// zero until the holder has read one and on a backend with no
	// reservation (an S3 bucket).
	usage objstore.Usage

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
		// A NODE THAT GAVE THE DUTY BACK IS NO LONGER ITS HOLDER, and
		// lets go of what only the holder reports — the bucket's gauges,
		// its near-ceiling alarm and the missing count — exactly as a
		// claim that answered "not held" does ([Engine.settleCollector]):
		// kept, they went on describing the bucket from a node that had
		// stopped reading it, beside the node that took the duty over.
		// A give-back the store did not answer settles nothing, as an
		// unknown claim does; it only forgets the resume, since the next
		// holder may record a newer pass meanwhile.
		if duty.declines.giveBack(ctx, collectorDecline, duty.giveBack) {
			e.settleCollector(duty, false, nil)
			return
		}
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
	mine, resume := e.settleCollector(duty, held, err)
	if err != nil && ctx.Err() == nil {
		log.WarnContext(ctx, "object_collector_duty_unclaimed", "error", err)
	}
	if !mine {
		return
	}
	// THE BUCKET'S USAGE ON EVERY TURN OF THE HOLDER, before whichever pass
	// is due, so a pass that runs for hours does not hold the gauges and the
	// near-ceiling alarm at the reading before it.
	e.sampleObjectsUsage(ctx, duty)
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
	mine, _ = e.settleCollector(duty, held, err)
	if !mine || !ran || ctx.Err() != nil {
		return
	}
	e.recordCollection(ctx, duty)
}

// settleCollector settles an answer about the duty ([collectorDuty.settle])
// and, where the answer is that this node does NOT hold it, withdraws the
// bucket's gauges from this node ([Engine.unsetObjectsUsage]), so two nodes
// never report the one bucket. An unknown answer keeps them, as it keeps the
// alarm: it says nothing about who holds the duty.
func (e *Engine) settleCollector(duty *collectorDuty, held bool, err error) (mine, resume bool) {
	mine, resume = duty.settle(held, err)
	if !mine && err == nil {
		e.unsetObjectsUsage()
	}
	return mine, resume
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
		d.usage = objstore.Usage{}
		return false, false
	}
	return true, !d.resumed
}

// collectorPass reads the fleet's record first if the turn has to, runs
// whichever pass is due, and reports whether one ran — none on a turn whose
// read of the record the store did not answer ([Engine.resumeCollector]).
func (e *Engine) collectorPass(ctx context.Context, duty *collectorDuty, resume bool) bool {
	if resume && !e.resumeCollector(ctx, duty) {
		return false
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

// sampleObjectsUsage reads how much of the bucket's reservation its objects
// take, on every turn of the duty's holder: the gauges and the
// `objects_bucket_near_ceiling` alarm read it.
//
// ONE NODE, ONCE A POLL. The bucket is one the whole fleet shares, so the
// holder's reading is the fleet's, and a read a minute is one stream-info
// request to the stream's leader, against the hour a bucket at four fifths
// takes to fill at any upload rate a fleet sustains. A read that fails keeps
// the last one, since unknown is not empty, and says so.
func (e *Engine) sampleObjectsUsage(ctx context.Context, duty *collectorDuty) {
	usage, held, err := e.objects.store.Usage(ctx)
	switch {
	case !held:
		return
	case err != nil:
		if ctx.Err() == nil {
			log.WarnContext(ctx, "objects_usage_unread", "error", err,
				"detail", "the bucket's gauges and its near-ceiling alarm keep the last reading")
		}
		return
	}
	duty.mu.Lock()
	duty.usage = usage
	duty.mu.Unlock()
	if e.metrics != nil {
		e.metrics.Set(metrics.ObjectsBucketBytes, float64(usage.Bytes), nil)
		e.metrics.Set(metrics.ObjectsBucketMaxBytes, float64(usage.MaxBytes), nil)
	}
}

// unsetObjectsUsage withdraws the bucket's gauges from a node that no longer
// holds the duty, so two nodes never report the one bucket.
func (e *Engine) unsetObjectsUsage() {
	if e.metrics != nil {
		e.metrics.Unset(metrics.ObjectsBucketBytes, nil)
		e.metrics.Unset(metrics.ObjectsBucketMaxBytes, nil)
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

// resumeCollector picks the schedule and the findings up from the fleet's
// record when this node takes the duty, so a duty that moved does not run every
// pass again at once — the record says when its last holder ran each — and
// does not forget what the last audit found. It reports whether it read the
// record: false when the store did not give it up, and the turn then runs no
// pass.
//
// ONLY A RECORD OF THIS STORE, read whole ([Backends.ObjectCollection]): a
// report of another store — left behind by a move of the fleet's files — is
// none, and both passes are due at once, as on a fleet that never recorded
// one.
//
// AND NO PASS UNTIL THE STORE HAS ANSWERED. A record the store did not give
// up — the objects bucket's leader moving, a blip — is UNKNOWN (ADR-0005),
// and says nothing about what the last holder found: run from nothing, the
// turn collected at once and recorded a status with no audit half, writing
// the last holder's findings out of the fleet's record and clearing
// objects_missing until a whole audit of the store found them again, a day
// later. So the turn runs no pass and records nothing, the resume stays owed,
// and the next turn — a minute later — reads again, as an unknown claim is
// settled ([collectorDuty.settle]). A record that DOES NOT DECODE is an
// answer, and the passes run from nothing over it: waiting on a record that
// will never decode would skip every pass, where running from nothing costs a
// pass run early.
func (e *Engine) resumeCollector(ctx context.Context, duty *collectorDuty) bool {
	last, found, err := e.backends.ObjectCollection(ctx)
	switch {
	case errors.Is(err, errCollectionUnreadable):
		log.WarnContext(ctx, "object_collector_schedule_unread", "error", err,
			"detail", "the collector's record does not decode, so this store is "+
				"collected and audited now and the record replaced")
	case err != nil:
		if ctx.Err() == nil {
			log.WarnContext(ctx, "object_collector_schedule_unanswered", "error", err,
				"detail", "no pass runs until the fleet's record of the collector is "+
					"read; the duty's next turn asks again")
		}
		return false
	case !found:
		log.InfoContext(ctx, "object_collector_nothing_to_resume",
			"store", e.backends.objectsIdentity,
			"detail", "the fleet holds no report of this store's passes — a new fleet, "+
				"or one whose files moved here — so it is collected and audited now")
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
	return true
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
// BEFORE THE ADMISSION IS WITHDRAWN ([Engine.withdraw]), with every other
// loop that appends to a log: every pass pins the estate with a barrier, and
// one still running once the admission is gone appends where a capacity
// coordinator may already be sealing. And so before [Engine.releaseDuties],
// which follows the withdrawal, as every loop that claims a duty is: one
// still running past the release takes the duty straight back.
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

// objectsReading fills the object store's half of an alarm reading: the
// bucket's usage against its reservation, and the files the collector's last
// audit to run to its end found missing, damaged or unanswered — on
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
	holding, usage := duty.holding, duty.usage
	duty.mu.Unlock()
	if !holding {
		return
	}
	out.ObjectsBucketBytes, out.ObjectsBucketMaxBytes = usage.Bytes, usage.MaxBytes
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

// DirectUploadStatus is what health reports as `objects.direct_upload`:
// "available" where the object store can hand a client a presigned put,
// "unavailable (<reason>)" where it cannot, and "" on a node running no
// object store. Fixed at boot: the s3 backend's probe decides it once
// (internal/objstore/s3obj), and the broker's own bucket never offers one.
func (e *Engine) DirectUploadStatus() string {
	if e.objects == nil {
		return ""
	}
	if ok, why := e.objects.store.DirectUpload(); !ok {
		return "unavailable (" + why + ")"
	}
	return "available"
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

// errCollectionUnreadable is a record of the collector that the store gave up
// and that does not decode — an ANSWER, unlike a store that did not give the
// record up at all, and the duty treats the two apart
// ([Engine.resumeCollector]).
var errCollectionUnreadable = errors.New("engine: the object collection record is unreadable")

// ObjectCollection is the collector's last report on store, the object store
// identity a node agreed with the fleet at boot ([openObjects]), as the fleet
// recorded it — false when no pass on that store has recorded one. It is the
// record's ONE reader: a node taking the collector's duty resumes from it
// ([Engine.resumeCollector]) and the status surfaces render it, both through
// [Backends.ObjectCollection], so the rules below cannot hold for one and not
// the other.
//
// A REPORT OF ANOTHER STORE IS NONE. The record names the store its passes
// ran on, and it outlives a change of store: a move deletes the backend record
// beside it and leaves the report, and a restore into a new bucket may bring
// the bucket back. Resumed, another store's report put the new store's first
// audit — the one check that its copy is whole — up to a day away, raised the
// old store's lost files as the new one's, and was written back at the next
// pass under the new store's name; rendered, it showed every node's /fleet and
// `crewlet objects status` the OLD store as the one the files are in, with the
// old store's audit and lost files, until the new store's first pass recorded
// — and for as long as no data node took the duty. So it answers as a fleet
// whose collector never reported.
//
// READ WHOLE OR NOT AT ALL: JSON fills every field it can before it reports
// one it could not, so a record that fails to decode answers nothing of it —
// a partly decoded one would put a pass off on the strength of a timestamp
// beside a field nobody could read.
func ObjectCollection(ctx context.Context, records CollectionRecords, store string) (CollectionReport, bool, error) {
	raw, found, err := records.ObjectCollection(ctx)
	if err != nil || !found {
		return CollectionReport{}, false, err
	}
	var r collectionReport
	if err := json.Unmarshal(raw, &r); err != nil {
		return CollectionReport{}, false, fmt.Errorf("%w: %w", errCollectionUnreadable, err)
	}
	if r.Backend != store {
		return CollectionReport{}, false, nil
	}
	return CollectionReport(r), true, nil
}

// ObjectCollection is the collector's last report on the object store this
// node agreed with the fleet at boot — [ObjectCollection] over this node's
// fleet store and its identity, which only [OpenBackends] knows.
func (b *Backends) ObjectCollection(ctx context.Context) (CollectionReport, bool, error) {
	return ObjectCollection(ctx, b.Fleet, b.objectsIdentity)
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
