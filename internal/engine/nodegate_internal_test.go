package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN EVICTION STOPS EVERY IDENTITY-CLAIMING LOG COUNTING THE NODE ONCE ITS
// FENCE WINDOW HAS PASSED, AND THE PAGES APPLIER DROPS WHAT THE NODE WRITES
// AFTER IT — on real streams, through the gesture the route calls.
//
// The gesture used to be the tracker writer's alone. It wrote the tracker's
// log and nothing else, so the pages log's eviction table stayed empty for the
// life of the fleet: the trim — reading the tracker's table filtered on the
// pages stream — counted an evicted node on the pages log for ever, the pages
// log's applied term stayed pinned at that node's last position, and the log
// grew toward the ceiling that refuses writes. And the pages applier's own gate
// read the same empty table, so the node's pages records applied everywhere.
func TestAnEvictionStopsEveryIdentityLogCountingTheNode(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	s := e.native.Load().log
	self := s.nodeID
	const away = "node-away"
	r := &retention{fleet: back.Fleet, state: s, db: back.Store, nodeID: self,
		cfg: config.TrackerRetention{MinAgeRaw: "1ns"}}
	identity := identityLogs(t, s)
	for _, running := range identity {
		for range 3 {
			barrierOn(t, running)
		}
	}

	// THE ABSENT NODE, as the register remembers it: it applied the first
	// record of each log and was never heard from again.
	positions := func() []coord.NodePositions {
		rows := []coord.NodePositions{{NodeID: self, At: time.Now().UTC(),
			Domains: map[string]coord.DomainPosition{}},
			{NodeID: away, At: time.Now().UTC().Add(-time.Hour),
				Domains: map[string]coord.DomainPosition{}}}
		for _, running := range identity {
			at := running.runner.Committed()
			rows[0].Domains[running.domain.Name()] = coord.DomainPosition{
				Generation: at.Generation, Seq: at.Seq, AppliedThrough: at.Seq}
			rows[1].Domains[running.domain.Name()] = coord.DomainPosition{
				Generation: at.Generation, Seq: 1, AppliedThrough: 1}
		}
		return rows
	}

	res, err := e.native.Load().gate.Evict(t.Context(), GateRequest{
		Node: away, OpID: "op-evict", By: "operator"})
	if err != nil || !res.Complete() {
		t.Fatalf("evict %s: %v (%+v)", away, err, res)
	}
	if len(res.Domains) != len(identity) {
		t.Fatalf("the gesture answered for %d log(s), want every identity-claiming "+
			"one (%d)", len(res.Domains), len(identity))
	}
	targets := map[string]uint64{}
	for _, running := range identity {
		name := running.domain.Name()
		waitApplied(t, running)
		_, last, err := running.log.Bounds(t.Context())
		if err != nil {
			t.Fatalf("read %s's end: %v", name, err)
		}
		targets[name] = last
		if group := running.domain.FeedGroup(); group != "" {
			waitUntil(t, 20*time.Second, name+"'s wake feed to scan its log", func() bool {
				floor, exists, err := running.log.GroupAckFloor(t.Context(), group)
				return err == nil && exists && floor >= last
			})
		}
	}

	tick := func(at time.Time) {
		t.Helper()
		shared := fleetInputs{at: at, readable: true, positions: positions(),
			previous: map[string]coord.TrimFloor{}}
		floors, err := back.Fleet.Floors(t.Context())
		if err != nil {
			t.Fatalf("floors: %v", err)
		}
		for _, f := range floors {
			shared.previous[f.Domain] = f
		}
		// ONE VERIFIED BACKUP COVERING EVERY LOG, as `crewlet backup`
		// records one: the newest point is what the backup term reads.
		point := coord.BackupPoint{Owner: self, At: at, Verified: true,
			Streams: map[string]coord.Position{}}
		for _, running := range identity {
			stream := running.domain.Stream().Name
			point.Streams[stream] = coord.Position{Stream: stream,
				Generation: running.runner.Committed().Generation,
				Seq:        targets[running.domain.Name()]}
		}
		shared.backups = []coord.BackupPoint{point}
		for _, running := range identity {
			if err := r.domain(t.Context(), running.domain.Name(), shared); err != nil {
				t.Fatalf("the tick on %s: %v", running.domain.Name(), err)
			}
		}
	}
	counted := func(running *runningDomain, at time.Time) bool {
		t.Helper()
		gen := running.runner.Committed().Generation
		set := statelog.CountedSet(at, reportedPositions(positions(), running.domain.Name()),
			nil, r.tombstones(t.Context(), running, gen))
		return slices.ContainsFunc(set, func(n statelog.NodePosition) bool {
			return n.NodeID == away
		})
	}
	trimTo := func(name string) uint64 {
		t.Helper()
		floors, err := back.Fleet.Floors(t.Context())
		if err != nil {
			t.Fatalf("floors: %v", err)
		}
		for _, f := range floors {
			if f.Domain == name {
				return f.TrimTo
			}
		}
		return 0
	}

	// INSIDE THE FENCE WINDOW THE NODE IS STILL COUNTED, on every log — the
	// window a live node has to read its own tombstone.
	now := time.Now().UTC()
	tick(now)
	for _, running := range identity {
		name := running.domain.Name()
		if !counted(running, now) {
			t.Fatalf("%s stopped counting %s inside the fence window", name, away)
		}
		if got := trimTo(name); got >= targets[name] {
			t.Fatalf("%s's trim reached %d with %s still counted at 1", name, got, away)
		}
	}

	// PAST IT, EVERY LOG STOPS COUNTING IT and trims past where it stood.
	later := now.Add(statelog.EvictionFenceWindow + time.Second)
	tick(later)
	for _, running := range identity {
		name := running.domain.Name()
		if counted(running, later) {
			t.Fatalf("%s still counts %s a fence window after its eviction — the "+
				"log's applied term stays pinned at its last position for ever",
				name, away)
		}
		if got := trimTo(name); got != targets[name] {
			t.Fatalf("%s's trim licensed %d past an evicted node, want %d — the "+
				"record the backup covers", name, got, targets[name])
		}
		if first, _, err := running.log.Bounds(t.Context()); err != nil ||
			first != targets[name] {
			t.Fatalf("%s's log starts at %d (err %v) after the tick, want %d",
				name, first, err, targets[name])
		}
	}

	// AND THE PAGES APPLIER DROPS WHAT THE EVICTED NODE WRITES AFTER IT —
	// beside a record from a node nobody evicted, which applies.
	wiki := s.Domain(pages.Domain{}.Name())
	appendRecord(t, wiki, pages.ContainerSubject("AWAY").String(),
		containerRecord(t, "AWAY", away))
	appendRecord(t, wiki, pages.ContainerSubject("STILL").String(),
		containerRecord(t, "STILL", "node-still-here"))
	waitApplied(t, wiki)
	requireRow(t, back, "pages_containers", "key", "AWAY", false)
	requireRow(t, back, "pages_containers", "key", "STILL", true)
}

// A READMISSION IS THE INVERSE COMMIT ON EVERY IDENTITY-CLAIMING LOG.
//
// The documentation said readmission puts back exactly the pin the eviction
// lifted; for the pages log no pin had ever been lifted, and no readmission
// was ever written there either. Both logs now hold both records, and each
// log's own rows say the node is back from the position its own readmission
// landed at.
func TestAReadmissionWritesTheInverseCommitToEveryLog(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	s := e.native.Load().log
	gate := e.native.Load().gate
	const away = "node-away"
	identity := identityLogs(t, s)

	if res, err := gate.Evict(t.Context(), GateRequest{
		Node: away, OpID: "op-evict", By: "operator"}); err != nil || !res.Complete() {
		t.Fatalf("evict: %v (%+v)", err, res)
	}
	// CAUGHT UP ON EVERY LOG, which is what a readmission is judged on.
	row := coord.NodePositions{NodeID: away, At: time.Now().UTC(),
		Domains: map[string]coord.DomainPosition{}}
	for _, running := range identity {
		at := running.runner.Committed()
		row.Domains[running.domain.Name()] = coord.DomainPosition{
			Generation: at.Generation, Seq: at.Seq, AppliedThrough: at.Seq}
	}
	if err := back.Fleet.PutPositions(t.Context(), row); err != nil {
		t.Fatalf("publish %s's position: %v", away, err)
	}

	res, err := gate.Readmit(t.Context(), GateRequest{
		Node: away, OpID: "op-readmit", By: "operator"})
	if err != nil || !res.Complete() {
		t.Fatalf("readmit: %v (%+v)", err, res)
	}
	for i, running := range identity {
		name := running.domain.Name()
		d := res.Domains[i]
		if d.Domain != name || d.Outcome != statelog.OutcomeApplied {
			t.Fatalf("the readmission's answer for %s is %+v, want applied", name, d)
		}
		rows, err := running.domain.(evictionLister).Evictions(t.Context(), back.Store)
		if err != nil {
			t.Fatalf("read %s's evictions: %v", name, err)
		}
		if len(rows) != 1 || !rows[0].Back ||
			rows[0].Readmitted != uint64(d.Position.Packed()) {
			t.Fatalf("%s holds %+v, want %s back at its own readmission %d — the "+
				"inverse commit on THIS log", name, rows, away, d.Position.Packed())
		}
	}
}

// A GESTURE THAT REACHED ONE LOG AND NOT THE OTHER SAYS SO, AND A RETRY UNDER
// THE SAME OPERATION FINISHES IT WITHOUT WRITING ANY LOG TWICE.
//
// Two shapes, because an `unknown` answer is two different facts and the retry
// has to be right for both: the pages record never landed, or it landed and the
// answer was lost. In both, the tracker's log — which applied the first time —
// answers the retry from its own ledger at the position it already holds, and
// gains no record.
func TestAPartialGateIsReportedAndARetryFinishesIt(t *testing.T) {
	t.Parallel()
	for name, landed := range map[string]bool{
		"the pages record never landed":       false,
		"the pages record landed, unanswered": true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, back, _ := trimmedTracker(t)
			s := e.native.Load().log
			const away = "node-away"
			identity := identityLogs(t, s)

			// THE ENGINE'S OWN GATE, with the pages log's write made to
			// answer `unknown` once — the one fault a real broker hands a
			// caller that nothing on this side can resolve.
			flaky := *e.native.Load().gate
			flaky.logs = slices.Clone(e.native.Load().gate.logs)
			faulted := false
			trackerName, pagesName := tracker.Domain{}.Name(), pages.Domain{}.Name()
			for i := range flaky.logs {
				if flaky.logs[i].domain != pagesName {
					continue
				}
				write := flaky.logs[i].write
				flaky.logs[i].write = func(ctx context.Context, by, opID, node string,
					readmit bool) (statelog.Result, error) {

					if faulted {
						return write(ctx, by, opID, node, readmit)
					}
					faulted = true
					if landed {
						if _, err := write(ctx, by, opID, node, readmit); err != nil {
							return statelog.Result{}, err
						}
					}
					return statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: opID}, nil
				}
			}
			ends := func() map[string]uint64 {
				t.Helper()
				out := map[string]uint64{}
				for _, running := range identity {
					_, last, err := running.log.Bounds(t.Context())
					if err != nil {
						t.Fatalf("read %s's end: %v", running.domain.Name(), err)
					}
					out[running.domain.Name()] = last
				}
				return out
			}

			req := GateRequest{Node: away, OpID: "op-partial", By: "operator"}
			first, err := flaky.Evict(t.Context(), req)
			if err != nil {
				t.Fatalf("evict: %v", err)
			}
			if first.Complete() {
				t.Fatalf("a gesture whose pages write answered unknown reported "+
					"itself complete: %+v", first)
			}
			byDomain := func(res GateResult) map[string]DomainGate {
				out := map[string]DomainGate{}
				for _, d := range res.Domains {
					out[d.Domain] = d
				}
				return out
			}
			got := byDomain(first)
			if got[trackerName].Outcome != statelog.OutcomeApplied ||
				got[pagesName].Outcome != statelog.OutcomeUnknown {
				t.Fatalf("the partial gesture answered %+v, want the tracker applied "+
					"and the pages log unknown", first.Domains)
			}
			if got[trackerName].OpID != domainOpID(req.OpID, false, trackerName) ||
				got[pagesName].OpID != domainOpID(req.OpID, false, pagesName) {
				t.Fatalf("the logs' operations are %q and %q, want each derived from "+
					"%q", got[trackerName].OpID, got[pagesName].OpID, req.OpID)
			}
			for _, running := range identity {
				waitApplied(t, running)
			}
			before := ends()

			second, err := flaky.Evict(t.Context(), req)
			if err != nil || !second.Complete() {
				t.Fatalf("the retry: %v (%+v)", err, second)
			}
			again := byDomain(second)
			if again[trackerName].Position != got[trackerName].Position {
				t.Fatalf("the tracker answered the retry at %s, want the %s it "+
					"already held — a retry is the same operation",
					again[trackerName].Position, got[trackerName].Position)
			}
			after := ends()
			if after[trackerName] != before[trackerName] {
				t.Fatalf("the retry wrote the tracker's log again (%d → %d), which "+
					"already held the gesture", before[trackerName], after[trackerName])
			}
			wantPages := before[pagesName] + 1
			if landed {
				wantPages = before[pagesName]
			}
			if after[pagesName] != wantPages {
				t.Fatalf("the pages log went from %d to %d on the retry, want %d — "+
					"written once, whether or not the first attempt landed",
					before[pagesName], after[pagesName], wantPages)
			}
			for _, running := range identity {
				waitApplied(t, running)
				rows, err := running.domain.(evictionLister).Evictions(t.Context(), back.Store)
				if err != nil {
					t.Fatalf("read %s's evictions: %v", running.domain.Name(), err)
				}
				if len(rows) != 1 || rows[0].NodeID != away || rows[0].Back {
					t.Fatalf("%s holds %+v after the retry, want %s evicted once",
						running.domain.Name(), rows, away)
				}
			}
		})
	}
}

// AN EVICTION IS JUDGED ONCE, BEFORE ANY LOG IS WRITTEN: a node still holding
// its presence lease is refused on every log, and only force overrides it.
//
// The judgement existed and nothing called it, so an eviction of a running
// machine — a mistyped node id — went through, moved its seats and dropped its
// records everywhere.
func TestAnEvictionOfALiveNodeIsRefusedBeforeAnyLogIsWritten(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	s := e.native.Load().log
	gate := e.native.Load().gate
	identity := identityLogs(t, s)
	const live = "node-live"
	if _, err := back.Coord.TryAcquire(t.Context(), coord.NodeResource(live),
		coord.AcquireOptions{Owner: live, TTL: time.Minute}); err != nil {
		t.Fatalf("hold %s's presence lease: %v", live, err)
	}
	ends := func() []uint64 {
		t.Helper()
		var out []uint64
		for _, running := range identity {
			_, last, err := running.log.Bounds(t.Context())
			if err != nil {
				t.Fatalf("read %s's end: %v", running.domain.Name(), err)
			}
			out = append(out, last)
		}
		return out
	}
	before := ends()
	_, err := gate.Evict(t.Context(), GateRequest{Node: live, OpID: "op-live", By: "operator"})
	var refusal *statelog.EvictionRefusal
	if !errors.As(err, &refusal) || refusal.NodeID != live {
		t.Fatalf("evicting a node holding a live lease answered %v, want its refusal", err)
	}
	if after := ends(); !slices.Equal(after, before) {
		t.Fatalf("a refused eviction moved the logs from %v to %v", before, after)
	}

	res, err := gate.Evict(t.Context(), GateRequest{
		Node: live, OpID: "op-live", By: "operator", Force: true})
	if err != nil || !res.Complete() {
		t.Fatalf("a forced eviction: %v (%+v)", err, res)
	}
}

// identityLogs is every identity-claiming domain this node runs, in the
// register's own order.
func identityLogs(t *testing.T, s *stateLog) []*runningDomain {
	t.Helper()
	var out []*runningDomain
	for _, name := range s.order {
		if running := s.domains[name]; running.domain.ClaimsIdentity() {
			out = append(out, running)
		}
	}
	if len(out) < 2 {
		t.Fatalf("this node runs %d identity-claiming log(s), and the case is about "+
			"a gesture that has to reach more than one", len(out))
	}
	return out
}

// containerRecord is a container's settings as writer's page store would
// publish them.
func containerRecord(t *testing.T, key, writer string) []byte {
	t.Helper()
	body, err := json.Marshal(pages.ContainerPayload{
		V: pages.DocumentVersion, Key: key, Name: "The " + key + " space"})
	if err != nil {
		t.Fatalf("encode the container: %v", err)
	}
	payload, err := pages.Encode(pages.MutationRecord{
		RecordEnvelope: pages.RecordEnvelope{
			V: pages.RecordVersion, OpID: "op-container-" + key,
			Subject: pages.ContainerSubject(key), Op: pages.OpPatch,
			Writer: writer, Scope: pages.ScopeSet{Subject: true},
		},
		Mutation: body, Actor: writer, ActorKind: pages.AuthorOperator,
	})
	if err != nil {
		t.Fatalf("encode the container record: %v", err)
	}
	return payload
}

// A LOG WHOSE OWN LEDGER ALREADY HOLDS THE GESTURE IS ANSWERED FROM IT AND NOT
// WRITTEN AGAIN.
//
// The broker's duplicate window collapses a retry for two minutes, and it is an
// optimisation rather than a mechanism: an operator finishes a partial eviction
// when they read the answer, which is routinely later than that. Past the
// window a second record under the same operation id lands, every applier
// applies it — moving the eviction's position and restarting its fence window —
// and only the answer is right. The log's own ledger is what makes the retry
// write nothing, and a ledger that cannot be read is not one that said no: the
// write is attempted, and its own snapshot of the same estate says why not.
func TestALogThatAlreadyHoldsTheGestureIsAnsweredFromItsLedger(t *testing.T) {
	t.Parallel()
	held := statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 2, Seq: 41}
	var written []string
	write := func(domain string, seq uint64) func(context.Context, string, string,
		string, bool) (statelog.Result, error) {
		return func(_ context.Context, _, opID, _ string, _ bool) (statelog.Result, error) {
			written = append(written, domain)
			return statelog.Result{Outcome: statelog.OutcomeApplied, OpID: opID,
				Position: statelog.Position{Stream: domain, Seq: seq}}, nil
		}
	}
	g := &NodeGate{
		live: func(context.Context) ([]statelog.Presence, error) { return nil, nil },
		logs: []gateLog{{
			domain: "tracker", stream: "CREWLET_TRACKER_LOG",
			applied: func(_ context.Context, opID string) (statelog.Position, bool, error) {
				return held, opID == "op-1.evict.tracker", nil
			},
			write: write("tracker", 99),
		}, {
			domain: "pages", stream: "CREWLET_PAGES_LOG",
			applied: func(context.Context, string) (statelog.Position, bool, error) {
				return statelog.Position{}, false, errors.New("the ledger is unreadable")
			},
			write: write("pages", 7),
		}},
	}
	res, err := g.Evict(t.Context(), GateRequest{Node: "node-away", OpID: "op-1", By: "operator"})
	if err != nil || !res.Complete() {
		t.Fatalf("evict: %v (%+v)", err, res)
	}
	if !slices.Equal(written, []string{"pages"}) {
		t.Fatalf("the gesture wrote %v, want only the pages log — the tracker's "+
			"ledger already holds this operation", written)
	}
	if got := res.Domains[0]; got.Outcome != statelog.OutcomeApplied || got.Position != held {
		t.Fatalf("the tracker answered %+v, want applied at the %s its ledger holds",
			got, held)
	}
	if got := res.Domains[1]; got.Outcome != statelog.OutcomeApplied || got.Position.Seq != 7 {
		t.Fatalf("the pages log answered %+v, want the write's own answer", got)
	}

	// THE SAME ID CARRIED TO THE OPPOSITE GESTURE IS A DIFFERENT OPERATION
	// on every log. Answered from the eviction's own entry, a readmission
	// would report the node back on the tracker's log while its applier
	// still dropped every record the node wrote.
	written = nil
	g.readmissible = func(context.Context, string) error { return nil }
	back, err := g.Readmit(t.Context(), GateRequest{Node: "node-away", OpID: "op-1", By: "operator"})
	if err != nil || !back.Complete() {
		t.Fatalf("readmit: %v (%+v)", err, back)
	}
	if !slices.Equal(written, []string{"tracker", "pages"}) {
		t.Fatalf("a readmission under the eviction's id wrote %v, want both logs — "+
			"the tracker's ledger holds the eviction, not this", written)
	}
	if got := back.Domains[0]; got.Position == held || got.OpID != "op-1.readmit.tracker" {
		t.Fatalf("the tracker answered the readmission %+v, want its own write "+
			"under op-1.readmit.tracker rather than the eviction's entry", got)
	}
}

// THE NODE BLOCK SHOWS A NODE EVICTED ONLY WHERE EVERY LOG HOLDS ITS TOMBSTONE,
// AS OF THE LATEST OF THEM.
//
// The trim counts nodes per log, so "evicted" beside a node is a claim about
// every log at once. Built by appending every log's tombstones — the last one
// read winning — a node evicted on the tracker's log and not yet on the pages
// log read as evicted and uncounted while the pages log's applied term waited
// on it; and a window measured from the earliest tombstone calls the eviction
// effective while one log still counts the node.
func TestTheReportShowsANodeEvictedOnlyOnceEveryLogHoldsIt(t *testing.T) {
	t.Parallel()
	at := time.Date(2031, 4, 2, 3, 0, 0, 0, time.UTC)
	tomb := func(node string, offset time.Duration) statelog.Tombstone {
		return statelog.Tombstone{NodeID: node, At: at.Add(offset), By: "operator"}
	}
	got := fleetTombstones([][]statelog.Tombstone{
		{tomb("both", 0), tomb("tracker-only", 0), tomb("both", 0)},
		{tomb("both", 30*time.Second), tomb("pages-only", 0)},
	})
	if len(got) != 1 || got[0].NodeID != "both" {
		t.Fatalf("tombstones = %+v, want only the node every log holds", got)
	}
	if !got[0].At.Equal(at.Add(30 * time.Second)) {
		t.Fatalf("the tombstone is dated %s, want the latest of the logs' %s",
			got[0].At, at.Add(30*time.Second))
	}
	if got := fleetTombstones(nil); got != nil {
		t.Fatalf("no log at all produced tombstones %+v", got)
	}
}
