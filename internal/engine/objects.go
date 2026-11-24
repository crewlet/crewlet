package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The object store, wired (ADR-0026). Every node reads and writes through the
// [objstore.Store] over its Tier A's backend ([Backends.Objects]); only the
// COLLECTOR — deleting what no row names, auditing what the backend lost —
// needs the replicated estate, so it runs on a data node as a fleet duty
// ([objectCollectorDuty]).
//
// A turn (the resume's read and the pass) runs on a KEPT duty
// ([dutyRenewal.keep]), renewed four times a [collectorDutyTTL], so an audit
// of millions of objects may outlive any lease. The fleet's ONE record
// (coord's ObjectStores.RecordObjectCollection) takes one writer at a time,
// so a pass that ends without the duty, or unable to say it still holds it,
// RECORDS NOTHING: the holder records its own.

// objectStore is this node's part in the object store.
type objectStore struct {
	store *objstore.Store

	// raise is the bucket raise this node's boot did not make, nil where
	// there was none; the alarm tick reads it ([raiseObjects]).
	raise atomic.Pointer[objectsRaise]

	// collector is the duty's loop, nil until the native runtime starts it
	// on a data node; collectorStopped is set under collectorMu once
	// [Engine.stopObjectCollector] has run, and no loop starts after it.
	//
	// A LOCK, NOT AN ATOMIC FLAG: the check and the start must be one step.
	// An apply may start the collector on its own goroutine as late as the
	// teardown, and a start that read the flag clear, then lost to a stop
	// that found nothing to stop, would leave a loop nothing ends.
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

// raiseObjectsAtBoot raises the nats bucket to this data node's explicit
// store.objects.nats_max_bytes, keeping what it did not raise for the alarm.
// owner is this process's incarnation, which holds the raise's lease.
func (e *Engine) raiseObjectsAtBoot(ctx context.Context, boot *config.Bootstrap, owner string) {
	if e.objects == nil || e.backends == nil {
		return
	}
	if r := raiseObjects(ctx, boot, e.backends, owner); r != nil {
		e.objects.raise.Store(r)
	}
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
// must not hand the duty to a peer. It need not outlive a pass, which renews
// it as it runs ([collectorDuty.renewal]); it bounds how long the duty stays
// dark after its holder dies, against a collection due hourly.
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
// it, against which backend, and what it found ([encodeCollection]).
//
// THE BACKEND BY ITS IDENTITY'S SHA-256: an identity is up to 13,289 bytes
// (internal/config's S3 bounds), which beside the findings would pass the
// small value the record is, and its one reader compares it with its own.
type collectionReport struct {
	Node    string         `json:"node"`
	Backend string         `json:"backend_sha256"`
	Status  collect.Status `json:"status"`
}

// maxCollectionWordsBytes is the most each sentence a collection report keeps
// takes as stored (a pass's error, its sweep's, why it stopped judging, an
// audit's error): a store's or a listing's error whole, and the four a
// quarter of the record. maxMissingNameBytes is a missing file's name's, a
// project and a path.
const (
	maxCollectionWordsBytes = 1 << 10
	maxMissingNameBytes     = 512
)

// encodeCollection is the record of status, run by node against the store
// identity names: a small value ([coord.SmallValueBytes]), its sentences and
// each file's name cut to what they take as stored ([coord.FitText]), and
// its unreadable files the longest opening of their list that fits. EVERY
// COUNT IS WHOLE: a reader says how many files the list leaves out by the
// findings' counts, as it does past [collect.MissingShown].
func encodeCollection(node, identity string, status collect.Status) ([]byte, error) {
	sum := sha256.Sum256([]byte(identity))
	r := collectionReport{Node: node, Backend: hex.EncodeToString(sum[:]), Status: status}
	c, a := &r.Status.Collect, &r.Status.Audit
	for _, words := range []*string{&c.Skipped, &c.SweepError, &c.Error, &a.Error} {
		*words = coord.FitText(*words, maxCollectionWordsBytes)
	}
	if a.Found == nil {
		return json.Marshal(r)
	}
	found := *a.Found
	found.MissingFiles = make([]collect.MissingFile, len(a.Found.MissingFiles))
	for i, m := range a.Found.MissingFiles {
		m.NamedBy = coord.FitText(m.NamedBy, maxMissingNameBytes)
		found.MissingFiles[i] = m
	}
	a.Found = &found
	all := found.MissingFiles
	keeping := func(n int) ([]byte, error) {
		found.MissingFiles = all[:n]
		return json.Marshal(r)
	}
	// The longest opening that fits, by bisection: a longer one never
	// encodes shorter.
	fits, over := 0, len(all)+1
	for over-fits > 1 {
		mid := fits + (over-fits)/2
		raw, err := keeping(mid)
		if err != nil {
			return nil, err
		}
		if len(raw) <= coord.SmallValueBytes {
			fits = mid
		} else {
			over = mid
		}
	}
	return keeping(fits)
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
	c, err := e.newCollector(o.store, refs)
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

// newCollector is the collector this engine runs over refs in store: the one
// constructor the duty and its tests share, so a case holds the duty to what
// it runs.
//
// WHAT THIS BUILD CANNOT SEE is asked before every pass judges anything
// ([Engine.unseen]): a newer build's log, or tables a newer build migrated
// this node's copy to, may name objects this build's declarations do not.
func (e *Engine) newCollector(store *objstore.Store, refs collect.References) (*collect.Collector, error) {
	return collect.New(collect.Options{Store: store, References: refs, Unseen: e.unseen})
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
		// A NODE THAT GAVE THE DUTY BACK IS NO LONGER ITS HOLDER, and lets
		// go of what only the holder reports (the bucket's gauges, its
		// near-ceiling alarm, the missing count) as a "not held" claim does
		// ([Engine.settleCollector]). A give-back the store did not answer
		// settles nothing, as an unknown claim; it only forgets the resume,
		// since the next holder may record a newer pass meanwhile.
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
	// A PASS THAT ENDED WITHOUT THE DUTY, or unsure of it, RECORDS NOTHING:
	// its report would replace the holder's. Settled by the claim's own rule,
	// so the two answers cannot be read differently.
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
		// UNKNOWN IS NEITHER: this node acts as no holder yet keeps what it
		// believed, its last audit's alarm included; read as "not mine", a
		// store blip would clear an alarm about lost files on the one node
		// reporting it.
		//
		// BUT IT FORGETS THE RESUME: another node may take the duty meanwhile
		// and record a newer audit, so the next held claim rereads the
		// fleet's record rather than write those findings out of it from this
		// node's older status. Resuming twice costs a read (Restore keeps the
		// newer of each half).
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
// record when this node takes the duty, so a moved duty neither reruns every
// pass at once nor forgets what the last audit found. It reports whether it
// read the record; when it did not, the turn runs no pass.
//
// ONLY A RECORD OF THIS STORE counts ([Backends.ObjectCollection]): another
// store's leaves both passes due at once. An UNANSWERED read is unknown
// (ADR-0005): the turn runs no pass and the resume stays owed, as in
// [collectorDuty.settle], since a pass run from nothing would write the last
// holder's findings out of the record and clear objects_missing for a day. A
// record that DOES NOT DECODE is an answer and the passes run from nothing,
// since waiting on it would skip every pass.
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
	raw, err := encodeCollection(e.id, e.backends.objectsIdentity, duty.collector.Status())
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
	// THIS NODE'S OWN BOOT, so read on every node rather than the holder's.
	if r := o.raise.Load(); r != nil {
		out.ObjectsBucketWanted, out.ObjectsBucketKept, out.ObjectsBucketWhy = r.Wanted, r.Kept, r.Why
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
// identity a node agreed with the fleet at boot ([openObjects]); false when no
// pass on it has recorded one. It is the record's ONE reader, for the duty's
// resume ([Engine.resumeCollector]) and the status surfaces alike.
//
// A REPORT OF ANOTHER STORE IS NONE: the report outlives a move of the files
// or a restore into a new bucket, and taken as the new store's it would put
// off its first audit, raise the old store's lost files as its own, and show
// the OLD store on /fleet and `crewlet objects status`.
//
// READ WHOLE OR NOT AT ALL: a partly decoded record could put a pass off on a
// timestamp beside a field nobody could read.
func ObjectCollection(ctx context.Context, records CollectionRecords, store string) (CollectionReport, bool, error) {
	raw, found, err := records.ObjectCollection(ctx)
	if err != nil || !found {
		return CollectionReport{}, false, err
	}
	var r collectionReport
	if err := json.Unmarshal(raw, &r); err != nil {
		return CollectionReport{}, false, fmt.Errorf("%w: %w", errCollectionUnreadable, err)
	}
	if sum := sha256.Sum256([]byte(store)); r.Backend != hex.EncodeToString(sum[:]) {
		return CollectionReport{}, false, nil
	}
	return CollectionReport{Node: r.Node, Backend: store, Status: r.Status}, true, nil
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

// CollectionReport is the collector's last report ([collectionReport]), its
// Backend the reader's own store, whose digest the record names.
type CollectionReport struct {
	Node    string
	Backend string
	Status  collect.Status
}
