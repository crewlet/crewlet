package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/objstore/s3obj/s3fake"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY DECLARED TABLE IS READ BY THE COLLECTOR THIS ENGINE STARTS.
//
// The declarations are one list (internal/objstore/references) and the
// estates the collector reads them through are this engine's. A consumer
// declaring a table in a domain the engine hands no estate for would have the
// collector refuse to start at boot, logged and nothing more, and the fleet
// would stop collecting and auditing — so the mismatch fails here, at build
// time, instead.
func TestTheCollectorReadsEveryDeclaredTable(t *testing.T) {
	t.Parallel()
	if _, err := collect.Sources(references.All, collectorEstates()...); err != nil {
		t.Fatalf("the collector cannot be built from the declared tables: %v", err)
	}
}

// objectsRuntime is a runtime as [Engine.startNative] leaves every one that
// runs a state log: each domain whose rows name objects readable — the
// tracker's whatever its tools are, and the code log's, which has no backend
// to choose. Its readers are zero values, because only the estates' names are
// read here.
func objectsRuntime() *native {
	return &native{trackerObjects: &worklet.Reader{}, codeReader: &code.Reader{}}
}

// collectorEstates is every estate the collector reads on a data node, as
// [Engine.startCollectorOver] builds them: [objectsRuntime]'s, and the
// artifacts and assistant logs', which every data node runs
// ([objectEstatesFor]).
func collectorEstates() []collect.Estate {
	return objectEstatesFor(objectsRuntime(), &artifactsdomain.Reader{}, &assistantdomain.Reader{})
}

// EVERY REQUIRED TABLE NAMES ITS OWN DOMAIN'S LOG (ADR-0075). A backup reads
// the log a declaration names past its store copy, and the deferred records of
// the table it names, to carry the objects a restore's replay names — so a
// declaration naming another domain's stream or table would have the backup
// read records that never write it and miss the ones that do, with nothing
// failing. The registered domain is the authority on both names; the
// declaration restates them only because the backup cannot import every
// domain, and this is what holds the two together.
func TestEveryRequiredTableNamesItsDomainsLog(t *testing.T) {
	t.Parallel()
	byName := map[string]statelog.Domain{}
	for _, d := range registeredDomains() {
		byName[d.Name()] = d
	}
	for _, ref := range references.All {
		if ref.Standing != objstore.Required {
			continue
		}
		d, ok := byName[ref.Domain]
		switch {
		case !ok:
			t.Errorf("%s is written by the %s domain, which this engine registers no log for",
				ref.Table, ref.Domain)
		case ref.Log != d.Stream().Name:
			t.Errorf("%s names %s as its log; the %s domain's log is %s", ref.Table, ref.Log,
				ref.Domain, d.Stream().Name)
		case ref.Deferred != d.DeferredTable():
			t.Errorf("%s names %s as its log's deferred records; the %s domain keeps them in %s",
				ref.Table, ref.Deferred, ref.Domain, d.DeferredTable())
		}
	}
}

// A RUNTIME THAT RUNS NO STATE LOG OFFERS NO ESTATE — a zero runtime, a
// test's, holds no domain's rows — and the collector over it is REFUSED, by
// name, and the refusal reaches the caller that logs it: never a collector
// over nothing, which would read every object as unreferenced, and never a
// collector skipped without a word, which is how every company whose tracker
// was not native went without one and nothing said so.
func TestARuntimeThatRunsNoStateLogOffersNoEstate(t *testing.T) {
	t.Parallel()
	n := &native{}
	if got := n.objectEstates(); len(got) != 0 {
		t.Fatalf("a runtime that runs no state log offered %d estates", len(got))
	}
	store, err := objstore.NewStore(memobj.New())
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{objects: &objectStore{store: store}}
	err = e.startCollectorOver(t.Context(), n)
	if err == nil {
		t.Fatal("a collector over a runtime that offers no estate was not refused: " +
			"a node holding no domain's rows would go without a collector and say nothing")
	}
	named := false
	for _, ref := range references.All {
		named = named || strings.Contains(err.Error(), ref.Table)
	}
	if !named {
		t.Errorf("the refusal names no declared table, so the line it is logged as "+
			"does not say which files go uncollected: %v", err)
	}
	if e.objects.duty.Load() != nil {
		t.Error("a collector was started over a runtime that offers no estate")
	}
}

// noTrackerDoc is a company that keeps its knowledge base here and its work
// nowhere: it runs the state log, and with it the tracker's log, for its pages
// alone.
const noTrackerDoc = `
name: Nimbus
tracker:
  backend: none
knowledge:
  backend: doclet
roles:
  - name: SWE
    handle: swe
`

// EVERY DOMAIN A DECLARATION NAMES HAS AN ESTATE THE COLLECTOR READS, WHATEVER
// BACKENDS THE COMPANY CHOSE — and the collector runs over them.
//
// The register runs every domain's log or none, so a domain's rows exist
// because its log runs, never because its tool surface does: a company that
// keeps its pages here and its work in Jira, or nowhere, still applies the
// tracker's log and still holds every `tracker_files` row it ever wrote — a
// company that moved its tracker off native holds all of its project files'.
// [collect.Sources] refuses a declared table whose domain has no estate,
// because its objects would read as unreferenced. So an estate built only
// where the tracker's TOOLS are left such a node with no collector at all —
// nothing audited, nothing collected — and with NOTHING SAYING SO: the empty
// list never reached Sources, because the collector's start returned before
// building them, so no warning was written and no status line showed it.
//
// EACH ARM NAMES THE BRANCH IT COVERS and holds its premise: the native
// tracker's arm the estate read through the tools' own reader, the others the
// estate built for the object store alone, where no tool reader exists. Those
// cover that branch only while the company each boots is served no tracker
// tools — so that is asserted, not assumed. And the code host's estate is
// offered on every arm, whatever `code.native` says, because its log runs in
// every register: the arm whose state log runs on the code host alone, and the
// others, which host no code, alike.
func TestEveryDeclaredDomainHasAnObjectEstate(t *testing.T) {
	t.Parallel()
	declared := map[string]bool{}
	for _, ref := range references.All {
		declared[ref.Domain] = true
	}
	for name, arm := range map[string]struct {
		doc string
		// toolReader is whether this node serves the company's tracker
		// TOOLS, which decides the branch its estate is built on.
		toolReader bool
	}{
		"Worklet":               {doc: companyWithoutSandboxDoc, toolReader: true},
		"no tracker, on Doclet": {doc: noTrackerDoc, toolReader: false},
		// THE STATE LOG ON THE CODE HOST ALONE: no Worklet and no
		// Doclet, so nothing but code.native
		// starts the register — and every declaring domain's estate is
		// still offered, the tracker's built for the object store alone.
		"the code host alone": {doc: codeHostAlone, toolReader: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newSandboxNode(t, parseCompany(t, arm.doc))
			n := e.native.Load()
			if n == nil || e.core.Load() == nil {
				t.Fatal("the premise: the company runs a state log, and this node runs none")
			}
			if (n.trackerReader != nil) != arm.toolReader {
				t.Fatalf("the premise: this arm covers the estate built where the "+
					"tracker's tools are served = %v, and this node serves them = %v",
					arm.toolReader, n.trackerReader != nil)
			}
			// THE ESTATES THE COLLECTOR IS BUILT OVER, as [Engine.startCollectorOver]
			// builds them: the native half's and the artifacts log's, which
			// every data node runs ([objectEstatesFor]).
			estates := objectEstatesFor(n, e.ArtifactsReader(), e.AssistantReader())
			offered := map[string]bool{}
			for _, estate := range estates {
				if offered[estate.Name()] {
					t.Errorf("the %s domain is offered twice", estate.Name())
				}
				offered[estate.Name()] = true
			}
			if !maps.Equal(offered, declared) {
				t.Errorf("the runtime offers the estates of %v; the declared tables "+
					"name the domains %v", slices.Sorted(maps.Keys(offered)),
					slices.Sorted(maps.Keys(declared)))
			}
			if _, err := collect.Sources(references.All, estates...); err != nil {
				t.Fatalf("the collector cannot be built from this runtime's estates: %v", err)
			}
			// AND IT RUNS: the duty's first pass is taken as the node boots,
			// so a collection is recorded where every node reads it — over
			// the estates above, pinned through each one's own log.
			if e.objects == nil || e.objects.duty.Load() == nil {
				t.Fatal("the node started no object collector")
			}
			var report CollectionReport
			waitUntil(t, 20*time.Second, "the collector's first pass to be recorded", func() bool {
				r, found, err := e.backends.ObjectCollection(t.Context())
				report = r
				return err == nil && found
			})
			if !report.Status.Collect.Completed {
				t.Errorf("the collector's first pass did not complete: %+v", report.Status.Collect)
			}
		})
	}
}

// namedNothing is a source of references that names no object, counting the
// barriers each pass takes.
type namedNothing struct{ barriers int }

func (*namedNothing) Name() string { return "tracker" }

func (s *namedNothing) Barrier(context.Context) (statelog.Position, error) {
	s.barriers++
	return statelog.Position{}, nil
}

func (*namedNothing) Referenced(context.Context, []objstore.Key,
	statelog.Position) (map[objstore.Key]struct{}, bool, error) {
	return map[objstore.Key]struct{}{}, true, nil
}

func (*namedNothing) Required(statelog.Position) []objstore.Walk { return nil }

func (*namedNothing) Moved(statelog.Position) []objstore.Walk { return nil }

// A NODE NAMING ANOTHER OBJECT STORE IS REFUSED, NAMING BOTH.
//
// The fleet records the store its first node opened, and a node whose
// store.objects names another would split the company's files between the two
// with nothing failing. The refusal is TYPED, carrying both identities, because
// the setting fails in other ways that name it too — the record unreadable,
// the bucket unreachable — and a caller asking whether a boot was refused for
// this reason (a restore that put the old store's record back) must not be
// answered by one of those. And it comes BEFORE the backend is opened: the
// store named here listens nowhere, and reaching for it would answer the
// open's failure instead.
func TestANodeNamingAnotherObjectStoreIsRefusedNamingBoth(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	const recorded = "s3:http://old.example.com/code/nimbus/"
	if _, err := fleet.AgreeObjectBackend(t.Context(), recorded); err != nil {
		t.Fatal(err)
	}
	boot := config.DefaultBootstrap()
	boot.Store.Objects = config.StoreObjects{Backend: config.ObjectBackendS3, S3: config.ObjectsS3{
		Endpoint: "http://127.0.0.1:1", Region: "us-east-1", Bucket: "code", Prefix: "nimbus/",
		PathStyle: true,
	}}
	out := &Backends{Fleet: fleet}
	err := openObjects(t.Context(), &boot, out, nil, nil)
	var mismatch *ObjectStoreMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("a node naming another object store than the fleet's was answered %v, "+
			"not the agreement's refusal", err)
	}
	if mismatch.Configured != boot.Store.Objects.Identity() || mismatch.Fleet != recorded {
		t.Errorf("the refusal names %q as this node's store and %q as the fleet's, want %q and %q",
			mismatch.Configured, mismatch.Fleet, boot.Store.Objects.Identity(), recorded)
	}
	for _, named := range []string{"store.objects", recorded, boot.Store.Objects.Identity()} {
		if !strings.Contains(err.Error(), named) {
			t.Errorf("the refusal %q does not name %q", err, named)
		}
	}
	if out.Objects != nil || out.objectsIdentity != "" {
		t.Errorf("a refused node opened its object store anyway (%T, %q)", out.Objects,
			out.objectsIdentity)
	}
}

// THE OBJECT STORE'S BUCKET COUNTS INTO THE NODE'S RECORDER, from its first
// request: the probe that opening it makes is attempts and requests like any
// other, so an `s3` node handed its recorder holds them once the store is
// open. The probe stores one object under the prefix (PutObject), deletes it
// (DeleteObject), makes three presigned puts of its own through the plain
// transport — no SDK operation, so nothing the counter labels — and deletes
// the object once more as it ends, however it ended (a second DeleteObject).
// A recorder that stopped at the engine — never handed to the client's
// constructor — leaves every S3 series the catalogue publishes empty, which
// reads as a bucket that never throttled anybody.
func TestTheObjectStoresBucketCountsIntoTheNodesRecorder(t *testing.T) {
	t.Parallel()
	srv := s3fake.Start(t, "files")
	boot := config.DefaultBootstrap()
	// STATIC KEYS: with none the SDK's own chain looks for credentials on
	// the machine — the instance's metadata endpoint among them — before
	// the first request.
	boot.Store.Objects = config.StoreObjects{Backend: config.ObjectBackendS3, S3: config.ObjectsS3{
		Endpoint: srv.URL, Region: "us-east-1", Bucket: srv.Bucket(), PathStyle: true,
		AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "example-secret",
	}}
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	out := &Backends{Fleet: coordmemory.NewFleet()}
	if err := openObjects(t.Context(), &boot, out, nil, rec); err != nil {
		t.Fatalf("open the object store: %v", err)
	}
	attempts, requests := map[string]uint64{}, map[string]uint64{}
	for _, s := range rec.Read() {
		switch {
		case s.Name == metrics.ObjectsS3Attempts && s.Attrs["outcome"] == "answered":
			attempts[s.Attrs["operation"]] += s.Total
		case s.Name == metrics.ObjectsS3Attempts:
			t.Errorf("the probe's %s counted an attempt %s; the fake answers every request",
				s.Attrs["operation"], s.Attrs["outcome"])
		case s.Name == metrics.ObjectsS3RequestAttempts:
			requests[s.Attrs["operation"]] += s.Count
		}
	}
	want := map[string]uint64{"PutObject": 1, "DeleteObject": 2}
	if !maps.Equal(attempts, want) {
		t.Errorf("the node's recorder counted the answered attempts %v after the object store "+
			"opened, want the probe's %v", attempts, want)
	}
	if !maps.Equal(requests, want) {
		t.Errorf("the node's recorder counted the requests %v after the object store opened, "+
			"want the probe's %v, one attempt each", requests, want)
	}
}

// collectorNode is an engine holding just enough to run the collector's duty
// by hand: an in-memory store and fleet, and a collector over source.
func collectorNode(t *testing.T, source collect.Source) (*Engine, *collectorDuty, *coordmemory.Fleet) {
	t.Helper()
	return collectorNodeOn(t, source, memobj.New())
}

// collectorNodeHolding is [collectorNode] over a store that already holds one
// object uploaded two days ago under a key minted then: past the grace by both
// instants, so a collection judges it, and pins its estate to do so.
func collectorNodeHolding(t *testing.T, source collect.Source) (*Engine, *collectorDuty, *coordmemory.Fleet) {
	t.Helper()
	backend, upload := agedUploads(t)
	upload()
	return collectorNodeOn(t, source, backend)
}

// agedUploads is a memory store, and a put of one object into it uploaded two
// days ago under a key minted then — past the grace by both instants, so the
// next collection judges it and pins its estate to do so. A pass that finds
// nothing past the grace decides nothing and takes no barrier
// (internal/objstore/collect), so a case about what a pass does while it runs
// gives it something to judge.
func agedUploads(t *testing.T) (objstore.Backend, func()) {
	t.Helper()
	then := time.Now().Add(-2 * collect.PendingGrace)
	var aged atomic.Bool
	backend := memobj.NewAt(func() time.Time {
		if aged.Load() {
			return then
		}
		return time.Now()
	})
	return backend, func() {
		t.Helper()
		aged.Store(true)
		defer aged.Store(false)
		if err := backend.Put(t.Context(), objstore.KeyAt(then).Name(),
			strings.NewReader("an old upload"), objstore.PutMeta{}); err != nil {
			t.Fatal(err)
		}
	}
}

// collectorNodeOn is [collectorNode] over the backend given.
func collectorNodeOn(t *testing.T, source collect.Source,
	backend objstore.Backend) (*Engine, *collectorDuty, *coordmemory.Fleet) {
	t.Helper()
	fleet := coordmemory.NewFleet()
	store, err := objstore.NewStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	refs := collect.References{source}
	c, err := collect.New(collect.Options{Store: store, References: refs})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{id: "data-a",
		backends: &Backends{Fleet: fleet, objectsIdentity: "nats"},
		objects:  &objectStore{store: store}}
	duty := newCollectorDuty(c, copiesNamedBy(refs), nil)
	e.objects.duty.Store(duty)
	return e, duty, fleet
}

// collectionRecorded is the collector's report as the fleet holds it, of
// whichever store it names — decoded here rather than through the reader the
// node itself resumes from, which is under test.
func collectionRecorded(t *testing.T, fleet *coordmemory.Fleet) (CollectionReport, bool) {
	t.Helper()
	raw, found, err := fleet.ObjectCollection(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return CollectionReport{}, false
	}
	var r collectionReport
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return CollectionReport(r), true
}

// EACH TURN RUNS AT MOST ONE PASS, the one that is due, and RECORDS what it
// found where every node reads it: a collection first (a node that never
// collected owes one), the audit on the next turn, and then nothing until one
// of them is due again. One pass a turn because a pass lists or walks the
// whole store, and a turn that ran both would hold the duty's lease through
// two of them.
func TestTheCollectorRunsThePassThatIsDueAndRecordsIt(t *testing.T) {
	t.Parallel()
	source := &namedNothing{}
	backend, upload := agedUploads(t)
	e, duty, fleet := collectorNodeOn(t, source, backend)

	if _, found := collectionRecorded(t, fleet); found {
		t.Fatal("the premise: a report was recorded before any pass ran")
	}
	e.collectorTurn(t.Context(), nil, duty)
	r, found := collectionRecorded(t, fleet)
	if !found || r.Status.Collect.At.IsZero() || !r.Status.Audit.At.IsZero() {
		t.Fatalf("the first turn recorded %+v (found %v), want a collection and no audit", r, found)
	}
	if r.Node != "data-a" || r.Backend != "nats" || !r.Status.Collect.Completed {
		t.Errorf("the report names node %q and backend %q, completed %v",
			r.Node, r.Backend, r.Status.Collect.Completed)
	}

	e.collectorTurn(t.Context(), nil, duty)
	r, _ = collectionRecorded(t, fleet)
	if r.Status.Audit.At.IsZero() || !r.Status.Audit.Completed {
		t.Fatalf("the second turn recorded audit %+v, want a completed audit", r.Status.Audit)
	}

	// SOMETHING PAST THE GRACE, so a pass that ran where none is due would
	// judge it and take a barrier: over a store holding nothing to judge, a
	// pass takes none, and the count below could not move either way.
	upload()
	barriers := source.barriers
	for range 3 {
		e.collectorTurn(t.Context(), nil, duty)
	}
	if source.barriers != barriers {
		t.Errorf("turns with no pass due pinned the estate %d more times", source.barriers-barriers)
	}
}

// A NODE THAT TAKES THE DUTY PICKS UP ITS LAST HOLDER'S SCHEDULE from the
// fleet's record, so a duty that moved — a restart, a deploy — does not run a
// full listing and a full audit again at once; and a node that did not hold
// the duty runs nothing.
func TestATakenDutyResumesTheScheduleItsLastHolderRecorded(t *testing.T) {
	t.Parallel()
	source := &namedNothing{}
	// AN AGED UPLOAD, so a pass that wrongly ran would judge it and take the
	// barrier this case counts.
	e, duty, fleet := collectorNodeHolding(t, source)
	now := time.Now()
	raw, err := json.Marshal(collectionReport{Node: "data-b", Backend: "nats", Status: collect.Status{
		Collect: collect.CollectionReport{Completed: true, At: now.Add(-10 * time.Minute)},
		Audit:   collect.AuditReport{Completed: true, At: now.Add(-time.Hour)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.RecordObjectCollection(t.Context(), raw); err != nil {
		t.Fatal(err)
	}

	notMine := func(context.Context) (bool, error) { return false, nil }
	e.collectorTurn(t.Context(), notMine, duty)
	if source.barriers != 0 || duty.resumed {
		t.Fatalf("a node that did not hold the duty pinned %d times, resumed %v",
			source.barriers, duty.resumed)
	}

	mine := func(context.Context) (bool, error) { return true, nil }
	e.collectorTurn(t.Context(), mine, duty)
	if source.barriers != 0 {
		t.Fatalf("a duty taken ten minutes after the last collection ran a pass")
	}
	if r, _ := collectionRecorded(t, fleet); r.Node != "data-b" {
		t.Errorf("a turn that ran nothing rewrote the record as %q's", r.Node)
	}

	// LOSING THE DUTY FORGETS THE RESUME, so taking it back reads the
	// record again: the holder in between may have run a pass.
	e.collectorTurn(t.Context(), notMine, duty)
	if duty.resumed || duty.holding {
		t.Errorf("a node that lost the duty still reads as holding %v, resumed %v",
			duty.holding, duty.resumed)
	}
}

// A FAILED PASS IS DUE AGAIN AFTER THE RETRY, NOT THE INTERVAL: the failures
// that stop one are usually over in minutes, and a collection that failed
// waiting the full hour leaves a whole hour of garbage behind it — an audit,
// a whole day of not knowing what was lost.
func TestAFailedPassIsDueAgainAfterTheRetry(t *testing.T) {
	t.Parallel()
	for _, interval := range []time.Duration{collect.CollectInterval, collect.AuditInterval} {
		d := &collectorDuty{}
		var last time.Time
		d.ran(&last, interval, errors.New("the store did not answer"))
		dueIn := time.Until(last.Add(interval))
		if dueIn > collectorRetry || dueIn < collectorRetry-time.Minute {
			t.Errorf("a failed pass of a %v interval is due again in %v, want %v",
				interval, dueIn, collectorRetry)
		}
		d.ran(&last, interval, nil)
		if dueIn := time.Until(last.Add(interval)); dueIn < interval-time.Minute {
			t.Errorf("a pass that ran is due again in %v, want its interval %v", dueIn, interval)
		}
	}
}

// failingSource is a source whose barrier never answers.
type failingSource struct{ namedNothing }

func (*failingSource) Barrier(context.Context) (statelog.Position, error) {
	return statelog.Position{}, errors.New("the log did not answer")
}

// A PASS THAT FAILED IS RECORDED AS FAILED, its error where an operator reads
// it, rather than leaving the last success standing as the store's status. The
// store holds an object past the grace, so the pass has one to judge and asks
// the failing estate for its barrier.
func TestAFailedPassIsRecordedWithItsError(t *testing.T) {
	t.Parallel()
	e, duty, fleet := collectorNodeHolding(t, &failingSource{})
	e.collectorTurn(t.Context(), nil, duty)
	r, found := collectionRecorded(t, fleet)
	if !found || r.Status.Collect.Completed || r.Status.Collect.Error == "" {
		t.Fatalf("a failed collection recorded %+v (found %v)", r.Status.Collect, found)
	}
}

// THE MISSING COUNT IS THE DUTY HOLDER'S ALONE. The store is one the whole
// fleet shares, so one node's audit of it is the fleet's — and a node that
// does not hold the duty, whose audit is whatever it found when it last did,
// would raise the alarm a second time, or keep raising it after the holder
// found the objects restored.
func TestTheMissingCountIsReportedOnlyByTheDutyHolder(t *testing.T) {
	t.Parallel()
	source := &namedObject{k: objstore.KeyAt(time.Now())}
	e, duty, _ := collectorNode(t, source)
	e.collectorTurn(t.Context(), nil, duty) // collect
	e.collectorTurn(t.Context(), nil, duty) // audit

	var r statelog.Reading
	e.objectsReading(time.Now(), &r)
	if r.ObjectsMissing != 1 {
		t.Fatalf("the holder reads %d missing, want the one object the store lost", r.ObjectsMissing)
	}

	e.collectorTurn(t.Context(), func(context.Context) (bool, error) { return false, nil }, duty)
	r = statelog.Reading{}
	e.objectsReading(time.Now(), &r)
	if r.ObjectsMissing != 0 {
		t.Errorf("a node that gave the duty up still reports %d missing", r.ObjectsMissing)
	}
}

// namedObject is a source naming one object, which the store never held.
type namedObject struct {
	namedNothing
	k objstore.Key
}

func (s *namedObject) Referenced(_ context.Context, among []objstore.Key,
	_ statelog.Position) (map[objstore.Key]struct{}, bool, error) {
	out := map[objstore.Key]struct{}{}
	for _, k := range among {
		if k == s.k {
			out[k] = struct{}{}
		}
	}
	return out, true, nil
}

func (s *namedObject) Required(statelog.Position) []objstore.Walk {
	return []objstore.Walk{&onePage{page: []objstore.Reference{{
		Object:  objstore.Object{Key: s.k, Hash: objstore.HashOf(nil), Size: 0},
		NamedBy: "ENG/lost.md",
	}}}}
}

// onePage is a walk of one page, and then none.
type onePage struct {
	page []objstore.Reference
	read bool
}

func (w *onePage) Next(context.Context) ([]objstore.Reference, bool, error) {
	if w.read {
		return nil, true, nil
	}
	w.read = true
	return w.page, true, nil
}

// THE COLLECTOR NEVER STARTS ONCE IT HAS BEEN STOPPED. The native runtime
// that starts it may be brought up by an apply on its own goroutine, as late
// as the teardown, and only the stop's own flag keeps a late start from
// running a duty nothing would ever end.
func TestTheCollectorNeverStartsOnceStopped(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, companyWithoutSandboxDoc))
	e.Stop(context.Background())

	// A REFUSED ESTATE FOR EVERY DOMAIN A DECLARATION NAMES, so the
	// references build whatever domains declare tables.
	var estates []collect.Estate
	seen := map[string]bool{}
	for _, ref := range references.All {
		if !seen[ref.Domain] {
			seen[ref.Domain] = true
			estates = append(estates, refusedEstate{name: ref.Domain})
		}
	}
	refs, err := collect.Sources(references.All, estates...)
	if err != nil {
		t.Fatalf("the references: %v", err)
	}
	if err := e.startObjectCollector(t.Context(), refs); err != nil {
		t.Fatalf("startObjectCollector on a stopped node: %v", err)
	}
	e.objects.collectorMu.Lock()
	started := e.objects.collector != nil
	e.objects.collectorMu.Unlock()
	if started {
		e.stopObjectCollector()
		t.Fatal("the collector started on a node that had stopped it")
	}
}

// refusedEstate is a domain whose barrier never answers, so a pass started
// over it fails at its pin rather than reading anything.
type refusedEstate struct{ name string }

func (r refusedEstate) Name() string { return r.name }

func (refusedEstate) Barrier(context.Context) (statelog.Position, error) {
	return statelog.Position{}, errors.New("the estate is not answering")
}

func (refusedEstate) Read(context.Context, statelog.Position, func(*sql.Tx) error) (bool, error) {
	return false, errors.New("the estate is not answering")
}

// A NODE THAT TAKES THE DUTY PICKS UP WHAT ITS LAST HOLDER FOUND, not only
// when: the files the last audit found missing raise the alarm on the new
// holder at once, and the next pass it records — a collection, say — carries
// them on in the fleet's record rather than writing them out of it.
func TestATakenDutyKeepsTheFindingsItsLastHolderRecorded(t *testing.T) {
	t.Parallel()
	e, duty, fleet := collectorNode(t, &namedNothing{})
	now := time.Now()
	lost := collect.MissingFile{Object: objstore.KeyAt(now), NamedBy: "ENG/lost.md"}
	raw, err := json.Marshal(collectionReport{Node: "data-b", Backend: "nats", Status: collect.Status{
		Collect: collect.CollectionReport{Completed: true, At: now.Add(-2 * collect.CollectInterval)},
		Audit: collect.AuditReport{Completed: true, Referenced: 7, Missing: 1, At: now.Add(-time.Hour),
			Found: &collect.AuditFindings{At: now.Add(-time.Hour), Completed: true, Referenced: 7,
				Missing: 1, MissingFiles: []collect.MissingFile{lost}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.RecordObjectCollection(t.Context(), raw); err != nil {
		t.Fatal(err)
	}

	mine := func(context.Context) (bool, error) { return true, nil }
	e.collectorTurn(t.Context(), mine, duty) // takes the duty, and the collection is due
	var reading statelog.Reading
	e.objectsReading(time.Now(), &reading)
	if reading.ObjectsMissing != 1 {
		t.Fatalf("the new holder reads %d missing, want the one its last holder found", reading.ObjectsMissing)
	}
	r, _ := collectionRecorded(t, fleet)
	if r.Node != "data-a" || r.Status.Collect.At.Before(now) {
		t.Fatalf("the turn recorded %+v, want this node's collection", r)
	}
	if f := r.Status.Audit.Found; f == nil || f.Missing != 1 || len(f.MissingFiles) != 1 ||
		f.MissingFiles[0] != lost || r.Status.Audit.Referenced != 7 {
		t.Fatalf("the record after the new holder's collection carries audit %+v; "+
			"want its last holder's findings kept", r.Status.Audit)
	}
}

// ANOTHER STORE'S REPORT IS NOTHING TO RESUME. A fleet whose files moved — the
// documented move, which deletes the backend record and leaves the report
// beside it, or a restore into a new bucket that brought the bucket back —
// holds a report about the OLD store. Resumed, the new holder waited out the
// old store's schedule before auditing the new one, which is the one check
// that the copy is whole; raised the old store's lost files as the new
// store's; and at its next pass wrote both into the fleet's record under the
// new store's name, an audit of one store reported as an audit of another.
func TestATakenDutyResumesNothingFromAnotherStoresReport(t *testing.T) {
	t.Parallel()
	source := &namedNothing{}
	e, duty, fleet := collectorNodeHolding(t, source)
	now := time.Now()
	lost := collect.MissingFile{Object: objstore.KeyAt(now), NamedBy: "ENG/lost.md"}
	raw, err := json.Marshal(collectionReport{Node: "data-b",
		Backend: "s3:https://old.example.com/acme-files/crewlet/", Status: collect.Status{
			Collect: collect.CollectionReport{Completed: true, At: now.Add(-10 * time.Minute)},
			Audit: collect.AuditReport{Completed: true, Referenced: 7, Missing: 1, At: now.Add(-time.Hour),
				Found: &collect.AuditFindings{At: now.Add(-time.Hour), Completed: true, Referenced: 7,
					Missing: 1, MissingFiles: []collect.MissingFile{lost}}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.RecordObjectCollection(t.Context(), raw); err != nil {
		t.Fatal(err)
	}

	mine := func(context.Context) (bool, error) { return true, nil }
	e.collectorTurn(t.Context(), mine, duty)
	if source.barriers == 0 {
		t.Fatal("the new store was not collected at once: its holder resumed the old store's schedule")
	}
	var reading statelog.Reading
	e.objectsReading(time.Now(), &reading)
	if reading.ObjectsMissing != 0 {
		t.Errorf("the new store's holder reads %d missing, the old store's findings", reading.ObjectsMissing)
	}
	r, _ := collectionRecorded(t, fleet)
	if r.Node != "data-a" || r.Backend != "nats" || r.Status.Collect.At.Before(now) {
		t.Fatalf("the turn recorded node %q, backend %q, collection %+v; want this node's "+
			"collection of its own store", r.Node, r.Backend, r.Status.Collect)
	}
	if !r.Status.Audit.At.IsZero() || r.Status.Audit.Found != nil {
		t.Fatalf("the record names %q with audit %+v, which ran on the old store", r.Backend, r.Status.Audit)
	}

	e.collectorTurn(t.Context(), mine, duty)
	if r, _ = collectionRecorded(t, fleet); r.Status.Audit.At.Before(now) || !r.Status.Audit.Completed {
		t.Fatalf("the second turn recorded audit %+v, want the new store audited at once", r.Status.Audit)
	}
}

// A RECORD THAT CANNOT BE READ RESUMES NOTHING OF IT, not the half that
// decoded: JSON fills every field it can before it reports one it could not,
// and a schedule taken from a record nobody could read waits out a pass on
// the strength of a timestamp beside a field that would not decode.
//
// AND IT IS AN ANSWER, unlike a record the store did not give up: the passes
// run from nothing over it, since waiting on a record that will never decode
// would skip every pass.
func TestARecordThatCannotBeReadResumesNothing(t *testing.T) {
	t.Parallel()
	source := &namedNothing{}
	// AN AGED UPLOAD, so a pass that runs judges something and takes the
	// barrier this case reads: a pass over a store holding nothing past the
	// grace decides nothing and takes none.
	e, duty, fleet := collectorNodeHolding(t, source)
	now := time.Now()
	// A WELL-FORMED DOCUMENT with a recent collection beside an audit of the
	// wrong type: the decoder fills the first and reports the second.
	raw := []byte(`{"node":"data-b","backend":"nats","status":{"collect":{"completed":true,"at":"` +
		now.Add(-10*time.Minute).UTC().Format(time.RFC3339Nano) + `"},"audit":7}}`)
	var partial collectionReport
	if err := json.Unmarshal(raw, &partial); err == nil || partial.Status.Collect.At.IsZero() {
		t.Fatalf("the premise: the record decodes in part and fails (err %v, collection %+v)",
			err, partial.Status.Collect)
	}
	if err := fleet.RecordObjectCollection(t.Context(), raw); err != nil {
		t.Fatal(err)
	}

	// THE COLLECTION IS DUE AT ONCE, as it is with no record at all; taken
	// from the half that decoded, it waited out the hour and the turn ran the
	// audit in its place.
	e.collectorTurn(t.Context(), func(context.Context) (bool, error) { return true, nil }, duty)
	if source.barriers == 0 {
		t.Fatal("the turn ran no pass over a record that does not decode, as though the " +
			"store had not answered: no pass would ever run")
	}
	if r, _ := collectionRecorded(t, fleet); r.Status.Collect.At.Before(now) {
		t.Fatalf("the turn recorded collection %+v and audit at %v: the holder took its schedule "+
			"from a record it could not read", r.Status.Collect, r.Status.Audit.At)
	}
}

// unansweredRecord is the fleet, but its store does not give the collector's
// record up the next fails times it is asked.
type unansweredRecord struct {
	*memFleet
	fails atomic.Int32
}

func (f *unansweredRecord) ObjectCollection(ctx context.Context) ([]byte, bool, error) {
	if f.fails.Add(-1) >= 0 {
		return nil, false, errors.New("the coordination store did not answer")
	}
	return f.memFleet.ObjectCollection(ctx)
}

// A RECORD THE STORE DID NOT GIVE UP IS UNKNOWN, NOT EMPTY (ADR-0005). A node
// taking the duty whose read of the fleet's record goes unanswered — the
// objects bucket's leader moving, a store blip — runs NO pass and records
// nothing, and reads the record again at its next turn. Run from nothing, as a
// record that does not decode is, it collected at once and recorded a status
// with no audit half: the last holder's findings written out of the fleet's
// record, and objects_missing cleared, until a whole audit of the store found
// them again — a day later.
func TestARecordTheStoreDidNotGiveUpRunsNoPass(t *testing.T) {
	t.Parallel()
	// recordLost has data-b, which held the duty in between, record an audit
	// that found lost, with a collection due again.
	recordLost := func(t *testing.T, fleet *coordmemory.Fleet, lost []collect.MissingFile) {
		t.Helper()
		now := time.Now()
		found := &collect.AuditFindings{At: now, Completed: true, Referenced: 2, MissingFiles: lost}
		for _, m := range lost {
			switch m.Finding {
			case collect.FindingUnanswered:
				found.Unanswered++
			default:
				found.Missing++
			}
		}
		audit := collect.AuditReport{Completed: true, Referenced: 2, Missing: found.Missing,
			Unanswered: found.Unanswered, At: now, Found: found}
		raw, err := json.Marshal(collectionReport{Node: "data-b", Backend: "nats", Status: collect.Status{
			Collect: collect.CollectionReport{Completed: true, At: now.Add(-2 * collect.CollectInterval)},
			Audit:   audit,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := fleet.RecordObjectCollection(t.Context(), raw); err != nil {
			t.Fatal(err)
		}
	}
	// unanswered is a node over source whose fleet withholds the record the
	// next time it is asked, over a store holding an aged upload, and the put
	// of another: a pass judges an object past the grace and takes a barrier,
	// so a turn that ran one where none may run moves the count [unchanged]
	// reads. Over a store holding nothing to judge, no pass takes a barrier
	// and the count could not move either way.
	unanswered := func(t *testing.T, source collect.Source) (*Engine, *collectorDuty,
		*coordmemory.Fleet, *unansweredRecord, func()) {
		t.Helper()
		backend, upload := agedUploads(t)
		upload()
		e, duty, fleet := collectorNodeOn(t, source, backend)
		record := &unansweredRecord{memFleet: fleet}
		e.backends.Fleet = record
		return e, duty, fleet, record, upload
	}
	mine := func(context.Context) (bool, error) { return true, nil }
	missing := func(e *Engine) int {
		var r statelog.Reading
		e.objectsReading(time.Now(), &r)
		return r.ObjectsMissing
	}
	// unchanged fails unless the fleet's record is still data-b's.
	unchanged := func(t *testing.T, fleet *coordmemory.Fleet, source *namedNothing,
		barriers int, duty *collectorDuty) {
		t.Helper()
		if source.barriers != barriers {
			t.Errorf("a turn whose record went unanswered ran a pass (%d barriers)",
				source.barriers-barriers)
		}
		if r, _ := collectionRecorded(t, fleet); r.Node != "data-b" {
			t.Errorf("a turn whose record went unanswered recorded %q's report over data-b's", r.Node)
		}
		if duty.resumed {
			t.Error("a turn whose record went unanswered counts the record as read")
		}
	}
	// carried fails unless the turn after an answered read recorded this
	// node's pass with lost carried on.
	carried := func(t *testing.T, fleet *coordmemory.Fleet, lost int) {
		t.Helper()
		r, _ := collectionRecorded(t, fleet)
		if r.Node != "data-a" {
			t.Fatalf("the turn after the record answered recorded %q's report, want this node's", r.Node)
		}
		if f := r.Status.Audit.Found; f == nil || len(f.MissingFiles) != lost {
			t.Fatalf("this node's first pass recorded findings %+v; want the %d files "+
				"the holder in between found lost", f, lost)
		}
	}

	t.Run("a node taking the duty", func(t *testing.T) {
		t.Parallel()
		source := &namedNothing{}
		e, duty, fleet, record, _ := unanswered(t, source)
		recordLost(t, fleet, []collect.MissingFile{{Object: objstore.KeyAt(time.Now()),
			NamedBy: "ENG/lost.md", Finding: collect.FindingMissing}})

		record.fails.Store(1)
		e.collectorTurn(t.Context(), mine, duty)
		unchanged(t, fleet, source, 0, duty)

		// A MINUTE LATER the record answers: the findings raise the alarm
		// here, and the collection that is due carries them on.
		e.collectorTurn(t.Context(), mine, duty)
		if got := missing(e); got != 1 {
			t.Errorf("the holder reads %d missing once the record answered, want 1", got)
		}
		carried(t, fleet, 1)
	})

	t.Run("a node taking it back after an unknown claim", func(t *testing.T) {
		t.Parallel()
		source := &namedObject{k: objstore.KeyAt(time.Now())}
		e, duty, fleet, record, upload := unanswered(t, source)
		e.collectorTurn(t.Context(), nil, duty) // collect
		e.collectorTurn(t.Context(), nil, duty) // audit: the one object is lost
		e.collectorTurn(t.Context(), func(context.Context) (bool, error) {
			return false, errors.New("the coordination store did not answer")
		}, duty)
		recordLost(t, fleet, []collect.MissingFile{
			{Object: source.k, NamedBy: "ENG/lost.md", Finding: collect.FindingMissing},
			{Object: objstore.KeyAt(time.Now()), NamedBy: "ENG/also-lost.md",
				Finding: collect.FindingUnanswered},
		})
		duty.mu.Lock()
		duty.collected = time.Time{} // a collection is due: a pass would run if one could
		duty.mu.Unlock()
		upload() // and one past the grace for it to judge, whatever the collection did with the first
		barriers := source.barriers

		record.fails.Store(1)
		e.collectorTurn(t.Context(), mine, duty)
		unchanged(t, fleet, &source.namedNothing, barriers, duty)
		// WHAT IT BELIEVED STANDS: its own audit's one lost file, never
		// cleared by a read the store did not answer.
		if got := missing(e); got != 1 {
			t.Errorf("the alarm reads %d missing after an unanswered read, want the 1 "+
				"this node's own audit found", got)
		}

		e.collectorTurn(t.Context(), mine, duty)
		if got := missing(e); got != 2 {
			t.Errorf("the alarm reads %d missing once the record answered, want the 2 "+
				"the holder in between found", got)
		}
		carried(t, fleet, 2)
	})
}

// A CLAIM THAT CANNOT BE ANSWERED IS NEITHER HELD NOR LOST: the node runs no
// pass, and keeps the alarm its last audit raised, rather than clearing an
// alarm about lost files over a store blip.
//
// AND IT RESUMES AGAIN when a claim next answers held: while nobody could say
// who held the duty, another node may have taken it and recorded an audit
// that found more files lost, and a node running on from its own older status
// would write those findings — and the alarm — out of the fleet's record with
// its next pass.
func TestAnUnknownClaimKeepsTheAlarm(t *testing.T) {
	t.Parallel()
	source := &namedObject{k: objstore.KeyAt(time.Now())}
	e, duty, fleet := collectorNode(t, source)
	e.collectorTurn(t.Context(), nil, duty) // collect
	e.collectorTurn(t.Context(), nil, duty) // audit
	barriers := source.barriers

	unknown := func(context.Context) (bool, error) {
		return false, errors.New("the coordination store did not answer")
	}
	e.collectorTurn(t.Context(), unknown, duty)
	var r statelog.Reading
	e.objectsReading(time.Now(), &r)
	if r.ObjectsMissing != 1 {
		t.Fatalf("an unanswered claim left the alarm at %d, want the 1 the audit found", r.ObjectsMissing)
	}
	if source.barriers != barriers || !duty.holding {
		t.Fatalf("an unanswered claim ran a pass (%d barriers) or gave the duty up (holding %v)",
			source.barriers-barriers, duty.holding)
	}

	// Another node held the duty meanwhile, and its audit found two files
	// lost: one the store does not hold, and one it would not answer for —
	// which the alarm counts as surely.
	now := time.Now()
	lost := []collect.MissingFile{
		{Object: source.k, NamedBy: "ENG/lost.md", Finding: collect.FindingMissing},
		{Object: objstore.KeyAt(now), NamedBy: "ENG/also-lost.md", Finding: collect.FindingUnanswered},
	}
	raw, err := json.Marshal(collectionReport{Node: "data-b", Backend: "nats", Status: collect.Status{
		Collect: collect.CollectionReport{Completed: true, At: now.Add(-2 * collect.CollectInterval)},
		Audit: collect.AuditReport{Completed: true, Referenced: 2, Missing: 1, Unanswered: 1, At: now,
			Found: &collect.AuditFindings{At: now, Completed: true, Referenced: 2,
				Missing: 1, Unanswered: 1, MissingFiles: lost}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fleet.RecordObjectCollection(t.Context(), raw); err != nil {
		t.Fatal(err)
	}
	mine := func(context.Context) (bool, error) { return true, nil }
	e.collectorTurn(t.Context(), mine, duty)
	r = statelog.Reading{}
	e.objectsReading(time.Now(), &r)
	if r.ObjectsMissing != 2 {
		t.Fatalf("the alarm reads %d missing once the claim answered, want the 2 the holder in between found",
			r.ObjectsMissing)
	}

	// The next pass this node records carries those findings on.
	duty.mu.Lock()
	duty.collected = time.Time{}
	duty.mu.Unlock()
	e.collectorTurn(t.Context(), mine, duty)
	rec, _ := collectionRecorded(t, fleet)
	if rec.Node != "data-a" {
		t.Fatalf("the due collection recorded %q's report, want this node's", rec.Node)
	}
	if f := rec.Status.Audit.Found; f == nil || f.Missing != 1 || f.Unanswered != 1 || len(f.MissingFiles) != 2 {
		t.Fatalf("the pass after an unknown claim recorded findings %+v; "+
			"want the two files the holder in between found lost", f)
	}
}

// heldPass is a source naming one object the store never held, whose pin — the
// first thing every pass does — waits while hold is set, until release is
// closed or the pass is stopped: a pass exactly as long as a test wants.
type heldPass struct {
	namedObject
	hold    atomic.Bool
	release chan struct{}
}

func newHeldPass() *heldPass {
	return &heldPass{namedObject: namedObject{k: objstore.KeyAt(time.Now())}, release: make(chan struct{})}
}

func (s *heldPass) Barrier(ctx context.Context) (statelog.Position, error) {
	if !s.hold.Load() {
		return statelog.Position{}, nil
	}
	select {
	case <-s.release:
		return statelog.Position{}, nil
	case <-ctx.Done():
		return statelog.Position{}, context.Cause(ctx)
	}
}

// turnAside runs one turn of the duty on a goroutine of its own, as the loop
// does: done is closed when it returns, and wait waits for that.
func turnAside(t *testing.T, e *Engine, claim func(context.Context) (bool, error),
	duty *collectorDuty) (done <-chan struct{}, wait func()) {
	t.Helper()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		e.collectorTurn(t.Context(), claim, duty)
	}()
	return returned, func() {
		t.Helper()
		select {
		case <-returned:
		case <-time.After(10 * time.Second):
			t.Fatal("the turn never returned")
		}
	}
}

// heldDutyNode is collectorNode over a source that holds every pass until it
// is released, whose duty is renewed on a test's clock — and which has run a
// collection and an audit already, so the fleet holds a record, the alarm
// reads the one object the store lost, and the next pass is a collection the
// test forces due.
func heldDutyNode(t *testing.T) (*Engine, *collectorDuty, *coordmemory.Fleet, *fakeDutyClock) {
	t.Helper()
	source := newHeldPass()
	backend, upload := agedUploads(t)
	e, duty, fleet := collectorNodeOn(t, source, backend)
	e.collectorTurn(t.Context(), nil, duty) // collect
	e.collectorTurn(t.Context(), nil, duty) // audit
	// SOMETHING FOR THE HELD PASS TO JUDGE, so it takes the barrier the
	// source holds it at.
	upload()
	var r statelog.Reading
	e.objectsReading(time.Now(), &r)
	if r.ObjectsMissing != 1 {
		t.Fatalf("the premise: the audit left the alarm at %d, want 1", r.ObjectsMissing)
	}
	clk := newFakeDutyClock()
	duty.renewal.clock = clk
	duty.mu.Lock()
	duty.collected = time.Time{}
	duty.mu.Unlock()
	source.hold.Store(true)
	return e, duty, fleet, clk
}

// A COLLECTOR'S PASS KEEPS ITS DUTY FOR AS LONG AS IT RUNS: a pass five leases
// long is renewed throughout, no peer can claim the duty at any point of it,
// and what it found is recorded as this node's when it ends. Renewed only
// between passes, the lease lapsed under a long audit, a peer ran its own
// beside it, and the two wrote the fleet's one record in turn.
func TestACollectorPassLongerThanItsLeaseKeepsTheDuty(t *testing.T) {
	t.Parallel()
	source := newHeldPass()
	source.hold.Store(true)
	e, duty, fleet := collectorNodeHolding(t, source)
	clk := newFakeDutyClock()
	duty.renewal.clock = clk
	store := &coordmemory.Backend{Clock: clk.Now}
	mine := schedule.ClaimNamedDuty(store, objectCollectorDuty, "a:1", "data-a", collectorDutyTTL)
	peer := schedule.ClaimNamedDuty(store, objectCollectorDuty, "b:1", "data-b", collectorDutyTTL)
	claim := newScriptedClaim()
	claim.next = mine

	_, wait := turnAside(t, e, claim.claim, duty)
	if a := claim.await(t); !a.held || a.err != nil {
		t.Fatalf("the premise: the turn's claim answered %v, %v", a.held, a.err)
	}
	for i := range 20 { // fifteen minutes: five leases
		clk.tick(t, duty.renewal.every())
		if a := claim.await(t); !a.held || a.err != nil {
			t.Fatalf("renewal %d answered %v, %v", i+1, a.held, a.err)
		}
		if held, err := peer(t.Context()); held || err != nil {
			t.Fatalf("a peer claimed the duty %v into the pass (%v, %v)",
				time.Duration(i+1)*duty.renewal.every(), held, err)
		}
	}
	close(source.release)
	wait()

	r, found := collectionRecorded(t, fleet)
	if !found || r.Node != "data-a" || !r.Status.Collect.Completed {
		t.Fatalf("the pass recorded %+v (found %v), want this node's completed collection", r, found)
	}
	duty.mu.Lock()
	defer duty.mu.Unlock()
	if !duty.holding {
		t.Error("the node that kept the duty through its pass no longer reads as holding it")
	}
}

// A PASS IS STOPPED THE MOMENT A RENEWAL ANSWERS THAT ANOTHER NODE HOLDS THE
// DUTY, and records nothing: that node records its own, and the fleet's record
// has one writer at a time. The duty, its resume and its alarm go with it, as
// they do for a claim answering the same.
func TestACollectorPassStopsWhenAnotherNodeHoldsTheDuty(t *testing.T) {
	t.Parallel()
	e, duty, fleet, clk := heldDutyNode(t)
	before, _ := collectionRecorded(t, fleet)
	claim := newScriptedClaim(answer{held: true}, answer{held: false})

	_, wait := turnAside(t, e, claim.claim, duty)
	claim.await(t)
	clk.tick(t, duty.renewal.every())
	claim.await(t)
	wait() // never released: only the lost duty ends the pass

	if after, _ := collectionRecorded(t, fleet); !after.Status.Collect.At.Equal(before.Status.Collect.At) {
		t.Fatalf("a pass stopped because another node held the duty recorded a collection at %v",
			after.Status.Collect.At)
	}
	duty.mu.Lock()
	holding, resumed := duty.holding, duty.resumed
	duty.mu.Unlock()
	if holding || resumed {
		t.Fatalf("a node that lost the duty mid-pass reads as holding %v, resumed %v", holding, resumed)
	}
	var r statelog.Reading
	e.objectsReading(time.Now(), &r)
	if r.ObjectsMissing != 0 {
		t.Errorf("a node that lost the duty mid-pass still reports %d missing", r.ObjectsMissing)
	}
	if got := duty.collector.Status().Collect.Error; !strings.Contains(got, errDutyLost.Error()) {
		t.Errorf("the stopped pass reports %q, want why it was stopped: %q", got, errDutyLost)
	}
}

// A PASS IS STOPPED ONCE NO RENEWAL HAS CONFIRMED ITS DUTY FOR AS LONG AS THE
// LEASE IS SURE TO LAST, and records nothing — another node may hold the duty
// by then. But unknown is not lost: the node keeps the alarm its last audit
// raised and only forgets its resume, as for a claim nobody answered.
func TestACollectorPassStopsWhenItsDutyCannotBeConfirmed(t *testing.T) {
	t.Parallel()
	e, duty, fleet, clk := heldDutyNode(t)
	before, _ := collectionRecorded(t, fleet)
	claim := newScriptedClaim(answer{held: true}, unanswered, unanswered)

	returned, wait := turnAside(t, e, claim.claim, duty)
	claim.await(t)
	clk.tick(t, duty.renewal.every())
	claim.await(t) // unanswered, with two renewals' intervals still confirmed
	clk.tick(t, duty.renewal.every())
	claim.await(t) // unanswered, with one still confirmed
	clk.tickToTheEnd(t, duty.renewal.every(), returned)
	wait() // never released: only the unconfirmed duty ends the pass

	if n := claim.count(); n != 3 {
		t.Fatalf("the duty was asked for %d times; want the turn's claim and two renewals, "+
			"and none once nothing confirmed it", n)
	}
	if after, _ := collectionRecorded(t, fleet); !after.Status.Collect.At.Equal(before.Status.Collect.At) {
		t.Fatalf("a pass whose duty could not be confirmed recorded a collection at %v",
			after.Status.Collect.At)
	}
	duty.mu.Lock()
	holding, resumed := duty.holding, duty.resumed
	duty.mu.Unlock()
	if !holding || resumed {
		t.Fatalf("an unconfirmed duty reads as holding %v, resumed %v; want the duty kept "+
			"and the resume forgotten", holding, resumed)
	}
	var r statelog.Reading
	e.objectsReading(time.Now(), &r)
	if r.ObjectsMissing != 1 {
		t.Errorf("an unconfirmed duty left the alarm at %d, want the 1 the audit found", r.ObjectsMissing)
	}
	if got := duty.collector.Status().Collect.Error; !strings.Contains(got, errDutyUnconfirmed.Error()) {
		t.Errorf("the stopped pass reports %q, want why it was stopped: %q", got, errDutyUnconfirmed)
	}
}

// A RENEWAL THE STORE DID NOT ANSWER COSTS A PASS NOTHING — refused at once,
// or held on to until its call gives up, which is what a request to a
// coordination stream whose leader is moving meets: the next is asked inside
// the window the last confirmation bought, answers held, and the pass runs to
// its end and is recorded — a coordination store's leader election does not
// throw away half an hour of audit.
func TestACollectorPassSurvivesARenewalTheStoreDidNotAnswer(t *testing.T) {
	t.Parallel()
	for _, heldOn := range []bool{false, true} {
		name := "refused"
		if heldOn {
			name = "held on to"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			source := newHeldPass()
			source.hold.Store(true)
			// AN AGED UPLOAD, so the pass judges something and takes the
			// barrier the source holds it at.
			e, duty, fleet := collectorNodeHolding(t, source)
			clk := newFakeDutyClock()
			duty.renewal.clock = clk
			claim := newScriptedClaim(answer{held: true}, unanswered)
			store := newHoldOn(0)
			if heldOn {
				claim = newScriptedClaim(answer{held: true})
				claim.next = store.claim
			}

			_, wait := turnAside(t, e, claim.claim, duty)
			claim.await(t)
			clk.tick(t, duty.renewal.every())
			if heldOn {
				store.held(t)
				clk.advance(duty.renewal.every()) // the call's own deadline
				claim.await(t)
				clk.tick(t, 0) // the next renewal, already due
			} else {
				claim.await(t)
				clk.tick(t, duty.renewal.every())
			}
			claim.await(t)
			clk.tick(t, duty.renewal.every()) // the first confirmation's window ends
			claim.await(t)
			close(source.release)
			wait()

			r, found := collectionRecorded(t, fleet)
			if !found || r.Node != "data-a" || !r.Status.Collect.Completed {
				t.Fatalf("the pass recorded %+v (found %v), want this node's completed collection", r, found)
			}
		})
	}
}

// reservedBackend is a store that holds a byte reservation, as the broker's
// own bucket does.
type reservedBackend struct {
	*memobj.Backend
	usage objstore.Usage
}

func (b *reservedBackend) Usage(context.Context) (objstore.Usage, error) { return b.usage, nil }

// THE BUCKET'S USAGE IS THE DUTY HOLDER'S TO REPORT, like the missing count:
// the holder's reading of the one bucket is the fleet's, and a node that gave
// the duty up reports nothing that could raise the alarm a second time.
func TestTheBucketUsageIsReportedOnlyByTheDutyHolder(t *testing.T) {
	t.Parallel()
	e, duty, _ := collectorNode(t, &namedNothing{})
	backend := &reservedBackend{Backend: memobj.New(), usage: objstore.Usage{Bytes: 9 << 30, MaxBytes: 10 << 30}}
	store, err := objstore.NewStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	e.objects.store = store
	e.collectorTurn(t.Context(), nil, duty)
	var r statelog.Reading
	e.objectsReading(time.Now(), &r)
	if r.ObjectsBucketBytes != 9<<30 || r.ObjectsBucketMaxBytes != 10<<30 {
		t.Fatalf("the holder reads %d of %d, want the bucket's 9 GiB of 10 GiB",
			r.ObjectsBucketBytes, r.ObjectsBucketMaxBytes)
	}
	if !slices.ContainsFunc(statelog.Evaluate(r), func(a statelog.Alarm) bool {
		return a.Kind == statelog.KindObjectsBucketNearCeiling
	}) {
		t.Fatal("a bucket at nine tenths of its reservation raised no alarm")
	}
	e.collectorTurn(t.Context(), func(context.Context) (bool, error) { return false, nil }, duty)
	r = statelog.Reading{}
	e.objectsReading(time.Now(), &r)
	if r.ObjectsBucketMaxBytes != 0 {
		t.Errorf("a node that gave the duty up still reports a reservation of %d", r.ObjectsBucketMaxBytes)
	}
}

// A HOLDER THAT DECLINES THE DUTY AND GIVES IT BACK LETS THE BUCKET'S GAUGES
// GO, as a holder told by its claim that another node holds the duty does: it
// stopped reading the bucket, and a gauge holds its last write, so kept they
// went on exporting this node's last reading — and raising the near-ceiling
// alarm from it — beside the reading of the node that took the duty over. A
// give-back the store did not answer is no answer about who holds the duty,
// and keeps them, as an unknown claim does.
func TestADecliningHolderLetsTheBucketsGaugesGo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		giveBack func(context.Context) (bool, error)
		kept     bool
	}{
		{"given_back", func(context.Context) (bool, error) { return true, nil }, false},
		{"held_none", func(context.Context) (bool, error) { return false, nil }, false},
		{"unanswered", func(context.Context) (bool, error) {
			return false, errors.New("the coordination store did not answer")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, duty, _ := collectorNode(t, &namedNothing{})
			backend := &reservedBackend{Backend: memobj.New(),
				usage: objstore.Usage{Bytes: 9 << 30, MaxBytes: 10 << 30}}
			store, err := objstore.NewStore(backend)
			if err != nil {
				t.Fatal(err)
			}
			e.objects.store = store
			rec, err := metrics.New()
			if err != nil {
				t.Fatal(err)
			}
			e.metrics = rec
			duty.giveBack = tc.giveBack
			gauges := func() (reading statelog.Reading, exported int) {
				e.objectsReading(time.Now(), &reading)
				for _, s := range rec.Read() {
					if s.Name == metrics.ObjectsBucketBytes || s.Name == metrics.ObjectsBucketMaxBytes {
						exported++
					}
				}
				return reading, exported
			}
			e.collectorTurn(t.Context(), nil, duty)
			if r, exported := gauges(); r.ObjectsBucketMaxBytes != 10<<30 || exported != 2 {
				t.Fatalf("the holder reads a reservation of %d and exports %d of the bucket's "+
					"two gauges, want 10 GiB and both", r.ObjectsBucketMaxBytes, exported)
			}

			// ITS COPY OF THE TRACKER GOES OUT OF SERVICE, so it declines.
			withCore(e)
			e.local = localWith(e, time.Now, func(context.Context, *core) copyVerdict {
				return wrongOn(trackerName)
			})
			e.collectorTurn(t.Context(), func(context.Context) (bool, error) {
				t.Error("a node whose tracker copy is out of service claimed the collector")
				return false, nil
			}, duty)
			r, exported := gauges()
			switch {
			case tc.kept && (r.ObjectsBucketMaxBytes != 10<<30 || exported != 2):
				t.Errorf("a give-back the store did not answer left a reservation of %d and "+
					"%d gauges; an unknown answer keeps what the node reported",
					r.ObjectsBucketMaxBytes, exported)
			case !tc.kept && (r.ObjectsBucketMaxBytes != 0 || exported != 0):
				t.Errorf("a node that gave the duty back still reads a reservation of %d and "+
					"exports %d of the bucket's gauges", r.ObjectsBucketMaxBytes, exported)
			}
		})
	}
}
