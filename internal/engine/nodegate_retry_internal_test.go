package engine

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A GESTURE THAT REACHED ONE LOG AND NOT THE OTHER SAYS SO, AND A RETRY UNDER
// THE SAME OPERATION FINISHES IT WITHOUT WRITING ANY LOG TWICE.
//
// Two shapes, because an `unknown` answer is two different facts and the retry
// has to be right for both: the pages record never landed, or it landed and the
// answer was lost. In both, the tracker's log — which applied the first time —
// answers the retry from its own rows at the position it already holds, and
// gains no record.
//
// # And it is the rows answering, not the broker
//
// Every log here is written through a recorder that WITHHOLDS the message id,
// so the broker's duplicate window — two minutes, and an operator finishing a
// partial gesture is routinely later — cannot collapse a second append onto the
// first. What the case counts is appends that reached the broker at all. With
// the id on the wire the log's end would not move either way, and a lookup that
// never found the operation passed as one that did.
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

			// THE ENGINE'S OWN GATE over recorded logs, with the pages
			// log's write made to answer `unknown` once — the one fault a
			// real broker hands a caller that nothing on this side can
			// resolve.
			flaky, recs := recordingGate(t, e, back)
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

			req := GateRequest{Node: away, OpID: "op-partial", By: "operator"}
			first, err := flaky.Evict(t.Context(), req)
			if err != nil {
				t.Fatalf("evict: %v", err)
			}
			if first.Complete() {
				t.Fatalf("a gesture whose pages write answered unknown reported "+
					"itself complete: %+v", first)
			}
			got := byDomain(first)
			if got[trackerName].Outcome != statelog.OutcomeApplied ||
				got[pagesName].Outcome != statelog.OutcomeUnknown {
				t.Fatalf("the partial gesture answered %+v, want the tracker applied "+
					"and the pages log unknown", first.Domains)
			}
			if !got[pagesName].Retry() {
				t.Fatalf("an unknown outcome is not advised a retry: %+v", got[pagesName])
			}
			if got[trackerName].OpID != domainOpID(req.OpID, false, trackerName, away) ||
				got[pagesName].OpID != domainOpID(req.OpID, false, pagesName, away) {
				t.Fatalf("the logs' operations are %q and %q, want each derived from "+
					"%q", got[trackerName].OpID, got[pagesName].OpID, req.OpID)
			}
			for _, running := range identity {
				waitApplied(t, running)
			}
			before, appended := logEnds(t, identity), appends(recs)

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
			wantAppends := map[string]int{trackerName: 0, pagesName: 1}
			if landed {
				wantAppends[pagesName] = 0
			}
			for domain, want := range wantAppends {
				if got := appends(recs)[domain] - appended[domain]; got != want {
					t.Fatalf("the retry put %d record(s) on %s's log, want %d — a "+
						"log whose rows already hold the operation answers from "+
						"them, and only a missing one is written", got, domain, want)
				}
			}
			if after := logEnds(t, identity); after[trackerName] != before[trackerName] {
				t.Fatalf("the retry wrote the tracker's log again (%d → %d), which "+
					"already held the gesture", before[trackerName], after[trackerName])
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

// A GESTURE ID CARRIED TO ANOTHER NODE IS THAT NODE'S OWN OPERATION.
//
// Each log's operation was derived from the gesture's id, its sign and the log
// alone, so evicting node-b under the id an eviction of node-a had used was
// answered from node-a's ledger entries — complete, at node-a's positions — and
// nothing was written for node-b on any log. Inside the broker's duplicate
// window the append itself would have collapsed onto node-a's record as well.
func TestAGestureIDCarriedToAnotherNodeEvictsThatNode(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	gate := e.native.Load().gate
	identity := identityLogs(t, e.native.Load().log)
	const opID = "op-shared"

	first, err := gate.Evict(t.Context(), GateRequest{Node: "node-a", OpID: opID, By: "operator"})
	if err != nil || !first.Complete() {
		t.Fatalf("evict node-a: %v (%+v)", err, first)
	}
	second, err := gate.Evict(t.Context(), GateRequest{Node: "node-b", OpID: opID, By: "operator"})
	if err != nil || !second.Complete() {
		t.Fatalf("evict node-b under node-a's gesture id: %v (%+v)", err, second)
	}
	a, b := byDomain(first), byDomain(second)
	for _, running := range identity {
		name := running.domain.Name()
		if b[name].Position == a[name].Position {
			t.Fatalf("%s answered node-b's eviction at %s, node-a's own position — "+
				"an answer for an operation that never ran", name, b[name].Position)
		}
		waitApplied(t, running)
		rows, err := running.domain.(evictionLister).Evictions(t.Context(), back.Store)
		if err != nil {
			t.Fatalf("read %s's evictions: %v", name, err)
		}
		held := map[string]uint64{}
		for _, row := range rows {
			if !row.Back {
				held[row.NodeID] = row.From
			}
		}
		if held["node-a"] != uint64(a[name].Position.Packed()) ||
			held["node-b"] != uint64(b[name].Position.Packed()) {
			t.Fatalf("%s holds evictions %v, want node-a at %d and node-b at %d",
				name, held, a[name].Position.Packed(), b[name].Position.Packed())
		}
	}
}

// AN EVICTION RETRIED UNDER ITS ID AFTER A READMISSION TOOK THE NODE BACK IS
// REFUSED, AND WRITES NOTHING.
//
// The ledger held the eviction's operation, so the retry was answered "applied"
// at the eviction's old position on every log — while every row said the node
// was counted again. The operation did happen; it is not what the node's
// standing rests on any more, and a retry is not a new eviction.
func TestAnEvictionRetriedAfterItsReadmissionIsSuperseded(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	gate := e.native.Load().gate
	identity := identityLogs(t, e.native.Load().log)
	const away = "node-away"
	evict := GateRequest{Node: away, OpID: "op-evict", By: "operator"}

	if res, err := gate.Evict(t.Context(), evict); err != nil || !res.Complete() {
		t.Fatalf("evict: %v (%+v)", err, res)
	}
	publishCaughtUp(t, back, identity, away)
	if res, err := gate.Readmit(t.Context(), GateRequest{
		Node: away, OpID: "op-readmit", By: "operator"}); err != nil || !res.Complete() {
		t.Fatalf("readmit: %v (%+v)", err, res)
	}
	for _, running := range identity {
		waitApplied(t, running)
	}
	before := logEnds(t, identity)

	res, err := gate.Evict(t.Context(), evict)
	if err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if res.Complete() {
		t.Fatalf("an eviction retried after its readmission reported itself "+
			"complete: %+v", res)
	}
	for _, d := range res.Domains {
		var refusal *statelog.Unavailable
		if !errors.As(d.Err, &refusal) || refusal.Reason != statelog.ReasonSuperseded {
			t.Fatalf("%s answered the retry %+v, want a superseded refusal", d.Domain, d)
		}
		if d.Retry() {
			t.Fatalf("%s advises running a superseded operation again: %s",
				d.Domain, d.Remedy())
		}
	}
	if after := logEnds(t, identity); !maps.Equal(after, before) {
		t.Fatalf("a superseded retry moved the logs from %v to %v", before, after)
	}
	for _, running := range identity {
		rows, err := running.domain.(evictionLister).Evictions(t.Context(), back.Store)
		if err != nil || len(rows) != 1 || !rows[0].Back {
			t.Fatalf("%s holds %+v (%v) after the refused retry, want %s still back",
				running.domain.Name(), rows, err, away)
		}
	}

	// A NEW GESTURE IS WHAT EVICTS IT AGAIN, which the refusal says.
	if res, err := gate.Evict(t.Context(), GateRequest{
		Node: away, OpID: "op-evict-again", By: "operator"}); err != nil || !res.Complete() {
		t.Fatalf("a fresh eviction after the refusal: %v (%+v)", err, res)
	}
}

// A RETRY THROUGH A NODE WHOSE APPLIER HAS NOT REACHED THE FIRST RECORD WRITES
// NO SECOND ONE.
//
// Checked in front of the write, the retry missed the ledger on such a node
// and then decided unconditionally: its append was refused against the first
// record, the publisher waited for the applier to reach it — and the next
// round's decision, which never looked, wrote the gate again against the
// anchor it had just caught up to. Past the broker's duplicate window that is
// a second record on the log, moving the eviction's position and restarting
// its fence window. The retry here starts while the tracker's applier is
// halted and lets it resume only once the write has found itself behind, with
// message ids withheld so the broker's window plays no part.
func TestARetryThroughALaggingApplierWritesNoSecondRecord(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	s := e.native.Load().log
	gate, recs := recordingGate(t, e, back)
	trackerName := tracker.Domain{}.Name()
	req := GateRequest{Node: "node-away", OpID: "op-lagging", By: "operator"}

	// THE FIRST GESTURE LANDS ON A LOG THIS NODE IS NOT APPLYING, so the
	// tracker's answer is durable and unresolved here.
	if !s.haltApplier(trackerName) {
		t.Fatal("the tracker's applier was not running to halt")
	}
	first, err := gate.Evict(t.Context(), req)
	if err != nil || !first.Complete() {
		t.Fatalf("evict: %v (%+v)", err, first)
	}
	held := byDomain(first)[trackerName]
	if held.Outcome != statelog.OutcomePending {
		t.Fatalf("the tracker answered %+v with its applier halted, want pending", held)
	}

	// THE RETRY STARTS STILL BEHIND, and the applier comes back only once the
	// tracker's write has probed its subject — the probe is what tells a
	// write it is behind a record already on the log.
	probed := make(chan struct{})
	var once sync.Once
	recs[trackerName].setProbe(func() { once.Do(func() { close(probed) }) })
	appended := appends(recs)
	type answer struct {
		res GateResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := gate.Evict(t.Context(), req)
		done <- answer{res, err}
	}()
	select {
	case <-probed:
	case <-time.After(20 * time.Second):
		t.Fatal("the retry never probed the tracker's log")
	}
	s.resumeApplier(t.Context(), trackerName)

	got := <-done
	if got.err != nil || !got.res.Complete() {
		t.Fatalf("the retry: %v (%+v)", got.err, got.res)
	}
	if d := byDomain(got.res)[trackerName]; d.Outcome != statelog.OutcomeApplied ||
		d.Position != held.Position {
		t.Fatalf("the tracker answered the retry %+v, want applied at %s — where "+
			"the first gesture's record landed", d, held.Position)
	}
	for domain, n := range appends(recs) {
		if n != appended[domain] {
			t.Fatalf("the retry put %d record(s) on %s's log, want none — the "+
				"operation was already there", n-appended[domain], domain)
		}
	}
}

// A LOG WHOSE LEDGER CANNOT BE READ ANSWERS WITH THAT ERROR AND IS NOT WRITTEN.
//
// The lookup in front of the write read `err == nil && done`, so a ledger this
// node could not read was one that said the operation never landed, and the
// gate went out again. The pages log's ledger is taken away here; the tracker's
// log, whose ledger is intact, is written as ever.
func TestAnUnreadableLedgerIsThatLogsErrorAndNothingIsWritten(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	gate, recs := recordingGate(t, e, back)
	trackerName, pagesName := tracker.Domain{}.Name(), pages.Domain{}.Name()
	if err := back.Store.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DROP TABLE pages_ops`)
		return err
	}); err != nil {
		t.Fatalf("take the pages log's ledger away: %v", err)
	}

	res, err := gate.Evict(t.Context(), GateRequest{
		Node: "node-away", OpID: "op-unread", By: "operator"})
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	got := byDomain(res)
	if got[trackerName].Outcome != statelog.OutcomeApplied {
		t.Fatalf("the tracker's log, whose ledger reads, answered %+v", got[trackerName])
	}
	if d := got[pagesName]; d.Err == nil || !d.Retry() {
		t.Fatalf("the pages log, whose ledger cannot be read, answered %+v — want "+
			"the read's own error, which a retry can clear", d)
	}
	if n := appends(recs)[pagesName]; n != 0 {
		t.Fatalf("an unreadable ledger put %d record(s) on the pages log, want none", n)
	}
}

// A NODE ID NO NODE COULD RUN UNDER IS REFUSED BEFORE ANYTHING IS JUDGED.
//
// It becomes a subject token on every identity log, so a typo carrying a
// wildcard was an append the broker refused on every log — reported, through
// the refusal classifier's catch-all, as each log being full.
func TestAGateOnANodeIDNoNodeCouldHaveIsRefused(t *testing.T) {
	t.Parallel()
	var wrote []string
	g := fakeGate(&wrote, nil)
	for _, node := range []string{"node*", "node 4", "-node", "node>", ""} {
		_, err := g.Evict(t.Context(), GateRequest{Node: node, OpID: "op", By: "operator"})
		if !errors.Is(err, ErrInvalidGate) {
			t.Fatalf("evicting %q answered %v, want ErrInvalidGate", node, err)
		}
	}
	if len(wrote) > 0 {
		t.Fatalf("a refused request wrote %v", wrote)
	}
}

// FORCE DECIDES AN UNREADABLE LEASE LISTING AS IT DECIDES A HELD LEASE.
//
// The listing is the judgement's only input, and force is the operator saying
// it does not decide this node. Refusing a forced eviction with a 500 because
// coordination could not be listed made force work only when it was least
// needed: an absent node most wants evicting during exactly such a fault.
// Unforced, the unreadable listing is still refused, and by name.
func TestForceEvictsPastALeaseListingNobodyCouldRead(t *testing.T) {
	t.Parallel()
	var wrote []string
	g := fakeGate(&wrote, errors.New("coordination is unreachable"))
	req := GateRequest{Node: "node-away", OpID: "op-unjudged", By: "operator"}

	_, err := g.Evict(t.Context(), req)
	var unjudged *GateUnjudged
	if !errors.As(err, &unjudged) || unjudged.Node != req.Node {
		t.Fatalf("an unforced eviction past an unreadable listing answered %v, "+
			"want *GateUnjudged", err)
	}
	if len(wrote) > 0 {
		t.Fatalf("an unjudged eviction wrote %v", wrote)
	}
	req.Force = true
	res, err := g.Evict(t.Context(), req)
	if err != nil || !res.Complete() {
		t.Fatalf("a forced eviction past an unreadable listing: %v (%+v)", err, res)
	}
	if !slices.Equal(wrote, []string{"tracker", "pages"}) {
		t.Fatalf("the forced eviction wrote %v, want both logs", wrote)
	}
}

// A GESTURE WHOSE CALLER GOES AWAY AFTER THE JUDGEMENT IS FINISHED ANYWAY.
//
// Its request's context was the gesture's, so a dropped connection between the
// two logs cancelled the second write and left the node evicted on one log and
// counted on the other, with no answer for anybody to read the operation id
// out of.
func TestAGestureIsFinishedWhenItsCallerGoesAway(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var wrote []string
	g := fakeGate(&wrote, nil)
	g.logs[0].write = func(context.Context, string, string, string, bool) (statelog.Result, error) {
		wrote = append(wrote, "tracker")
		cancel()
		return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
	}
	second := g.logs[1].write
	g.logs[1].write = func(ctx context.Context, by, opID, node string,
		readmit bool) (statelog.Result, error) {
		if err := ctx.Err(); err != nil {
			return statelog.Result{}, err
		}
		return second(ctx, by, opID, node, readmit)
	}
	res, err := g.Evict(ctx, GateRequest{Node: "node-away", OpID: "op-dropped", By: "operator"})
	if err != nil || !res.Complete() {
		t.Fatalf("a gesture whose caller left after the first log: %v (%+v)", err, res)
	}
	if !slices.Equal(wrote, []string{"tracker", "pages"}) {
		t.Fatalf("the gesture wrote %v, want both logs", wrote)
	}
}

// EVERY LOG'S OPERATION IS DISTINCT PER SIGN, PER LOG AND PER NODE, AND NO TWO
// GESTURES SPELL ONE.
//
// A node id may hold dots and a gesture id may hold anything, so a plain dotted
// join spelled gesture `x` on node `y.evict.tracker.z` and gesture
// `x.evict.tracker.y` on node `z` as the same operation — one ledger row
// answering two gestures.
func TestADomainOperationIDNamesOneOperation(t *testing.T) {
	t.Parallel()
	type parts struct {
		gesture string
		readmit bool
		domain  string
		node    string
	}
	seen := map[string]parts{}
	for _, p := range []parts{
		{"op-1", false, "tracker", "node-a"},
		{"op-1", true, "tracker", "node-a"},
		{"op-1", false, "pages", "node-a"},
		{"op-1", false, "tracker", "node-b"},
		{"x", false, "tracker", "y.evict.tracker.z"},
		{"x.evict.tracker.y", false, "tracker", "z"},
		{"a:b", false, "tracker", "c"},
		{"a", false, "tracker", "b"},
	} {
		id := domainOpID(p.gesture, p.readmit, p.domain, p.node)
		if other, dup := seen[id]; dup {
			t.Fatalf("%+v and %+v both derive %q", p, other, id)
		}
		seen[id] = p
	}
}

// A LOG IS ADVISED A RETRY ONLY WHERE RUNNING THE GESTURE AGAIN CAN FINISH IT.
//
// Every incomplete answer told the operator to run the command again with
// -op-id. For an evicted node, a log rebuilt under it, a full log or an
// operation superseded since, the same command fails the same way for ever.
func TestAGateLogIsAdvisedARetryOnlyWhereOneCanFinishIt(t *testing.T) {
	t.Parallel()
	refused := func(reason statelog.Reason) DomainGate {
		return DomainGate{Stream: "CREWLET_PAGES_LOG",
			Err: &statelog.Unavailable{Reason: reason}}
	}
	for name, tc := range map[string]struct {
		gate  DomainGate
		retry bool
		hint  string
	}{
		"applied":      {gate: DomainGate{Outcome: statelog.OutcomeApplied}},
		"pending":      {gate: DomainGate{Outcome: statelog.OutcomePending}},
		"unknown":      {gate: DomainGate{Outcome: statelog.OutcomeUnknown}, retry: true, hint: "unknown"},
		"conflict":     {gate: DomainGate{Err: statelog.ErrConflict}, retry: true, hint: "changing"},
		"a failure":    {gate: DomainGate{Err: errors.New("broken pipe")}, retry: true, hint: "same operation id"},
		"behind":       {gate: refused(statelog.ReasonBehind), retry: true, hint: "catching up"},
		"below floor":  {gate: refused(statelog.ReasonBelowFloor), retry: true, hint: "snapshot"},
		"evicted":      {gate: refused(statelog.ReasonEvicted), hint: "-url"},
		"wrong stream": {gate: refused(statelog.ReasonWrongStream), hint: "reanchor"},
		"log full":     {gate: refused(statelog.ReasonLogFull), hint: "set-capacity"},
		"superseded":   {gate: refused(statelog.ReasonSuperseded), hint: "without -op-id"},
		"op reused":    {gate: refused(statelog.ReasonOpReused), hint: "without -op-id"},
		"skew":         {gate: refused(statelog.ReasonSkew), hint: "backup"},
	} {
		if got := tc.gate.Retry(); got != tc.retry {
			t.Errorf("%s: Retry() = %v, want %v", name, got, tc.retry)
		}
		hint := tc.gate.Remedy()
		if (hint == "") != (tc.hint == "") || !strings.Contains(hint, tc.hint) {
			t.Errorf("%s: Remedy() = %q, want it to name %q", name, hint, tc.hint)
		}
	}
}

// fakeGate is a gate over two logs that record which were written, judging an
// eviction against a lease listing that answers nothing held — or listErr.
func fakeGate(wrote *[]string, listErr error) *NodeGate {
	write := func(domain string) func(context.Context, string, string, string,
		bool) (statelog.Result, error) {
		return func(context.Context, string, string, string, bool) (statelog.Result, error) {
			*wrote = append(*wrote, domain)
			return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
		}
	}
	return &NodeGate{
		live:         func(context.Context) ([]statelog.Presence, error) { return nil, listErr },
		readmissible: func(context.Context, string) error { return nil },
		logs: []gateLog{
			{domain: "tracker", stream: "CREWLET_TRACKER_LOG", write: write("tracker")},
			{domain: "pages", stream: "CREWLET_PAGES_LOG", write: write("pages")},
		},
	}
}

// recorder is one domain's real log behind a counter: every append that
// reaches the broker is counted, and its MESSAGE ID IS WITHHELD, so the broker's
// duplicate window cannot collapse a second append onto a first — what a
// retry later than that window meets.
type recorder struct {
	log *jetstream.DomainLog

	mu      sync.Mutex
	appends int
	probe   func()
}

func (r *recorder) Append(ctx context.Context, subject, _ string, expect *uint64,
	body []byte) (uint64, bool, error) {
	r.mu.Lock()
	r.appends++
	r.mu.Unlock()
	return r.log.Append(ctx, subject, "", expect, body)
}

func (r *recorder) LastSeq(ctx context.Context, subject string) (uint64, bool, error) {
	r.mu.Lock()
	probe := r.probe
	r.mu.Unlock()
	if probe != nil {
		probe()
	}
	return r.log.LastSeq(ctx, subject)
}

func (r *recorder) setProbe(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probe = fn
}

// recordingGate is the engine's own gate over every identity log, each written
// through its own domain's production write authority publishing via a
// [recorder] in front of the real log.
func recordingGate(t *testing.T, e *Engine, back *Backends) (*NodeGate, map[string]*recorder) {
	t.Helper()
	s := e.native.Load().log
	g := &NodeGate{live: e.native.Load().gate.live, readmissible: e.native.Load().gate.readmissible}
	recs := map[string]*recorder{}
	for _, running := range identityLogs(t, s) {
		name := running.domain.Name()
		rec := &recorder{log: running.log}
		pub, _, err := s.publisherOver(running.domain, rec, running.log, running.runner)
		if err != nil {
			t.Fatalf("a recorded publisher for %s: %v", name, err)
		}
		gl, err := gateLogFor(running, pub, back.Store, s.nodeID, nil)
		if err != nil {
			t.Fatalf("the gate's writer for %s: %v", name, err)
		}
		g.logs = append(g.logs, gl)
		recs[name] = rec
	}
	return g, recs
}

// appends is how many appends each recorder has passed to the broker.
func appends(recs map[string]*recorder) map[string]int {
	out := make(map[string]int, len(recs))
	for name, r := range recs {
		r.mu.Lock()
		out[name] = r.appends
		r.mu.Unlock()
	}
	return out
}

// byDomain indexes one gesture's answers by log.
func byDomain(res GateResult) map[string]DomainGate {
	out := map[string]DomainGate{}
	for _, d := range res.Domains {
		out[d.Domain] = d
	}
	return out
}

// logEnds is every identity log's last sequence.
func logEnds(t *testing.T, identity []*runningDomain) map[string]uint64 {
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

// publishCaughtUp publishes node's position at every identity log's current
// end, which is what a readmission is judged on.
func publishCaughtUp(t *testing.T, back *Backends, identity []*runningDomain, node string) {
	t.Helper()
	row := coord.NodePositions{NodeID: node, At: time.Now().UTC(),
		Domains: map[string]coord.DomainPosition{}}
	for _, running := range identity {
		at := running.runner.Committed()
		row.Domains[running.domain.Name()] = coord.DomainPosition{
			Generation: at.Generation, Seq: at.Seq, AppliedThrough: at.Seq}
	}
	if err := back.Fleet.PutPositions(t.Context(), row); err != nil {
		t.Fatalf("publish %s's position: %v", node, err)
	}
}
