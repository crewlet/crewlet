package kv

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// The two gates' view of the lease buckets.
//
// Both gates in this package ask a question about EVERY live lease record:
// the protocol gate whether any is held at a lower protocol than the claim
// (coord.ProtocolVersion, ADR-0016), and the duty gate whether any in the
// seat lease bucket was written by a build that predates the duty bucket
// (layoutDutyLane). They asked it by LISTING both buckets — a certified walk
// of every lease in the fleet — on every gated claim, twice (the check and
// the re-check), on every duty claim and in every FleetProtocolFloor. So a
// claim cost a function of the leases already held: measured on a
// three-member cluster with about 9,300 seats held, a gated seat claim took
// 296–506 ms (p99 1.4–3.4 s), ten claimers managed 27 claims a second
// between them, and a hundred nodes claiming ten thousand seats from cold
// drove the process past 10 GB and the members' raft groups out of quorum —
// the placement sweep claims seats one after another, each claim a walk of
// everything the claims before it had written.
//
// # What the gates actually need, and what they cannot be told
//
// The question is about VALUES — a record's protocol and layout are in its
// body, not its key — and the builds it exists for are the OLDER ones, which
// write nothing but their lease records. So nothing an older build writes can
// be narrowed at the broker, and no record this build could add would be
// written by the builds the gate is for. Three cheaper shapes were weighed and
// each breaks the gate:
//
//   - GATING ON PRESENCE, a listing of the `node` class — O(nodes) — misses
//     an older node that holds seats without presence, and that is not a
//     corner: a drain gives presence up at its FIRST step (it is what moves
//     the node's share to its peers) and goes on serving its seats until
//     each is handed over. Every newer node would claim beside it for the
//     whole drain, which is the mixed-protocol fleet the gate exists to
//     prevent.
//   - A FLOOR REFRESHED ON A CADENCE, by this node or published by a
//     singleton, is as old as its cadence: an older node that joined inside
//     it is missed by the check AND by the re-check, so the window the
//     re-check bounds to one claim widens to the cadence.
//   - A PER-OWNER PROTOCOL RECORD, which a class listing could read in
//     O(nodes), is written by this build and later ones — never by the older
//     builds the gate is about.
//
// # So the view is incremental, and exact by a sequence barrier
//
// The view is a WATCH of both lease buckets: the newest record of every key,
// kept as it arrives, indexed by the two facts the gates judge. The cost of a
// gate is then the older-protocol records — none outside a rolling upgrade —
// plus one BARRIER, and the barrier is what makes it exact rather than merely
// recent. Before a gate is judged, each bucket's last sequence is read from
// the stream LEADER (a stream info refused when it names no leader, as a
// listing's index is), and the gate waits until the view has consumed through
// it. Then the view holds the newest record of every key written before that
// read — every key live from before the read until after it is in the view,
// which is the certified listing's own guarantee (walk.go) — so the check
// and the re-check see what a walk would have seen. The same read carries
// the store's clock, so judging a record's deadline costs nothing more. A
// view that cannot reach the leader, or does not catch up within the read
// budget, answers UNKNOWN; its watch dying does too, and the next gate starts
// it again.
//
// The watch reads whichever member the broker places it on, which may be
// behind; the barrier waits that out, which is why no copy's lag can hide a
// record from a gate.
//
// # What it costs, and why it stops
//
// Per gate: two stream infos, where the listing it replaces was a watch
// consumer created and deleted per bucket (two metadata proposals each) and
// every record in the fleet read, twice per claim. Measured on one embedded
// member (BenchmarkAGatedClaim): a gated claim took 5.8 ms with ten leases
// held and 231 ms with ten thousand; it takes about 1.0 ms at both, and the
// view's load of ten thousand records, paid once per run, about 130 ms. What the view costs
// INSTEAD is every lease write delivered to it while it runs — a fleet's
// heartbeats, one per held lease per renew — so it runs only while this node
// is judging gates and stops [gateViewIdle] after the last one. A node at its
// share of seats claims nothing and pays nothing, and neither does one with
// room whose every candidate a peer holds: a claim on a held resource is
// refused [coord.RefusedHeld] before any gate, and the seat host reads the
// fleet's floor only after a claim the gate refused. One converging from
// cold pays one load of the buckets and then the writes of the minutes it
// claims through; a node holding a fleet duty re-claims it every tick and
// keeps its view for as long as it holds one.
type gateView struct {
	s *Store

	mu sync.Mutex
	// run is the running watch and everything it has taken in, nil while
	// the view is stopped. Each start is a new run, so a goroutine of a
	// run that was stopped cannot write into the one that replaced it.
	run *gateRun
	// lastUse is the last time a gate was judged, which is what the idle
	// stop measures from.
	lastUse time.Time
	idle    *time.Timer
}

// gateRun is one start of the view: its watch and what the watch has taken
// in. Every field but stop and exited is guarded by the view's mutex.
type gateRun struct {
	// records is the newest record of every key in both lease buckets,
	// as far as applied reaches. A key whose newest message is a delete
	// or purge marker, or a tombstone (no owner), holds nothing a gate
	// judges and is not kept.
	records map[gateKey]entry
	// byProtocol and oldLayout index records by the two facts the gates
	// judge, so neither ever iterates the current build's leases: the
	// protocol gate reads the protocols BELOW the claim's, and the duty
	// gate the seat lease bucket's records by an older layout.
	byProtocol map[int]map[gateKey]struct{}
	oldLayout  map[gateKey]struct{}
	// applied is the highest stream sequence of each bucket this run has
	// taken in.
	applied map[*lane]uint64
	// advanced is closed and replaced whenever applied moves or the run
	// fails, so a barrier waits on it rather than polling.
	advanced chan struct{}
	// failed is why the watch stopped, when it stopped on its own.
	failed error

	stop context.CancelFunc
	// exited is closed when every goroutine of the run has returned.
	exited chan struct{}
}

// gateKey is one record's place: a bucket and a key in it.
type gateKey struct {
	lane *lane
	key  string
}

// gateViewIdle is how long the view keeps running after the last gate it
// judged.
//
// A MINUTE: twelve of the seat sweep's five-second passes, so a node that is
// converging — claiming a share of seats a pass at a time — keeps one view
// through all of it instead of loading the buckets again every pass; and
// longer than the ticks of the fleet duties that re-claim often, so their
// holder is not reloading either. Past it a node that has stopped claiming
// stops receiving the fleet's lease writes, which is the whole of the view's
// standing cost.
const gateViewIdle = time.Minute

func newGateView(s *Store) *gateView {
	return &gateView{s: s}
}

// blocked reports whether any live lease is held at a protocol below
// protocol — the protocol gate's predicate, judged exactly (see the file
// doc).
func (v *gateView) blocked(ctx context.Context, protocol int) (bool, error) {
	run, clk, err := v.settle(ctx)
	if err != nil {
		return false, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for p, keys := range run.byProtocol {
		if p >= protocol {
			continue
		}
		if run.anyLive(keys, clk) {
			return true, nil
		}
	}
	return false, nil
}

// olderLayout reports whether any live record in the seat lease bucket was
// written by a build that predates the duty bucket — the duty gate's
// predicate.
func (v *gateView) olderLayout(ctx context.Context) (bool, error) {
	run, clk, err := v.settle(ctx)
	if err != nil {
		return false, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return run.anyLive(run.oldLayout, clk), nil
}

// floor is the lowest protocol among live leases, and whether there were any.
func (v *gateView) floor(ctx context.Context) (int, bool, error) {
	run, clk, err := v.settle(ctx)
	if err != nil {
		return 0, false, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	floor, found := 0, false
	for p, keys := range run.byProtocol {
		if found && p >= floor {
			continue
		}
		if run.anyLive(keys, clk) {
			floor, found = p, true
		}
	}
	return floor, found, nil
}

// anyLive reports whether any of keys is a record the gates count as held,
// dropping the ones whose deadline has passed: a record past its deadline is
// never held again — a renew is a new message — so the sets the gates
// iterate stay the size of what is live. Held under the view's mutex.
func (r *gateRun) anyLive(keys map[gateKey]struct{}, clk gateClock) bool {
	for k := range keys {
		e, ok := r.records[k]
		if !ok {
			delete(keys, k)
			continue
		}
		if clk.live(e) {
			return true
		}
		r.forget(k)
	}
	return false
}

// gateClock is each bucket's clock, read by the barrier with the sequence.
type gateClock map[*lane]time.Time

// live is the gates' "held": a record naming an owner whose deadline, on its
// own bucket's clock, has not passed.
//
// Judged by the DEADLINE even on the seat lease bucket, where a record taken
// at the bucket's full age is otherwise judged live for as long as it can be
// read ([Store.held]): the bucket reaps such a record at that same deadline,
// and a view — which learns of a write but never of an expiry — would keep a
// reaped record for ever. A claiming record (epoch 0) is held, as the gates
// have always counted it.
func (c gateClock) live(e entry) bool {
	return e.value.Owner != "" && e.created.Add(e.value.ttl()).After(c[e.lane])
}

// settle is the barrier: it reads each lease bucket's last sequence and
// clock from its stream leader, waits until the view has taken in every
// message through that sequence, and hands back the run it waited on and the
// clocks. The view is started if it is not running.
func (v *gateView) settle(ctx context.Context) (*gateRun, gateClock, error) {
	run, err := v.ensure(ctx)
	if err != nil {
		return nil, nil, err
	}
	lanes := []*lane{v.s.leases, v.s.duties}
	targets := make(map[*lane]uint64, len(lanes))
	clk := make(gateClock, len(lanes))
	for _, l := range lanes {
		last, at, err := v.s.streamPoint(ctx, l)
		if err != nil {
			return nil, nil, err
		}
		targets[l], clk[l] = last, at
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, v.s.js.Options().DefaultTimeout)
		defer cancel()
	}
	for {
		v.mu.Lock()
		failed, advanced := run.failed, run.advanced
		caught := true
		for l, target := range targets {
			if run.applied[l] < target {
				caught = false
			}
		}
		v.mu.Unlock()
		switch {
		case failed != nil:
			return nil, nil, unavailable("judge the lease gates", failed)
		case caught:
			return run, clk, nil
		}
		select {
		case <-advanced:
		case <-ctx.Done():
			return nil, nil, unavailable("judge the lease gates", fmt.Errorf(
				"the view did not catch up with the lease buckets' last writes: %w",
				ctx.Err()))
		}
	}
}

// ensure starts the view's watch if it is not running — or if the run that
// was running failed — and marks the use the idle stop measures from.
//
// The watch OUTLIVES the gate that starts it: its lifetime is the view's, ended
// by the idle stop or by Close. So it takes the caller's context without its
// cancellation — a claim that gives up must not stop the view every later
// claim reads.
func (v *gateView) ensure(ctx context.Context) (*gateRun, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.lastUse = time.Now()
	if v.run != nil && v.run.failed == nil {
		return v.run, nil
	}
	v.halt()
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	lanes := []*lane{v.s.leases, v.s.duties}
	watchers := make([]jetstream.KeyWatcher, 0, len(lanes))
	for _, l := range lanes {
		w, err := l.kv.WatchAll(ctx)
		if err != nil {
			cancel()
			return nil, unavailable("watch "+l.kv.Bucket()+" for the lease gates", err)
		}
		watchers = append(watchers, w)
	}
	run := &gateRun{
		records:    map[gateKey]entry{},
		byProtocol: map[int]map[gateKey]struct{}{},
		oldLayout:  map[gateKey]struct{}{},
		applied:    map[*lane]uint64{},
		advanced:   make(chan struct{}),
		stop:       cancel,
		exited:     make(chan struct{}),
	}
	v.run = run
	var wg sync.WaitGroup
	for i, l := range lanes {
		wg.Go(func() { v.consume(ctx, run, l, watchers[i]) })
	}
	go func() {
		wg.Wait()
		close(run.exited)
	}()
	if v.idle == nil {
		v.idle = time.AfterFunc(gateViewIdle, v.stopIfIdle)
	} else {
		v.idle.Reset(gateViewIdle)
	}
	return run, nil
}

// consume applies one bucket's watch to its run until the watch ends.
func (v *gateView) consume(ctx context.Context, run *gateRun, l *lane, w jetstream.KeyWatcher) {
	for kve := range w.Updates() {
		if kve == nil {
			// The end of the initial values — the client's guess, which
			// the barrier does not rely on.
			continue
		}
		v.apply(ctx, run, l, kve)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if ctx.Err() == nil && run.failed == nil {
		// Ended on its own: the connection closed, or the consumer was
		// lost for good. Every gate waiting on this run answers unknown,
		// and the next gate starts a new one.
		run.failed = fmt.Errorf("the watch of %s ended", l.kv.Bucket())
		run.stop()
		close(run.advanced)
		run.advanced = make(chan struct{})
	}
}

// apply takes one message of one bucket into a run.
func (v *gateView) apply(ctx context.Context, run *gateRun, l *lane, kve jetstream.KeyValueEntry) {
	k := gateKey{lane: l, key: kve.Key()}
	var (
		e    entry
		keep bool
	)
	if kve.Operation() == jetstream.KeyValuePut {
		var ok bool
		if e, ok = decodeEntry(kve, l); !ok {
			// Skipped as every listing skips one, and loudly: it names
			// nothing a gate can judge.
			log.WarnContext(ctx, "coord_kv_undecodable_record", "bucket", l.kv.Bucket(), "key", kve.Key())
		}
		keep = ok && e.value.Owner != ""
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if ctx.Err() != nil {
		// The run was stopped; what it held is gone.
		return
	}
	run.forget(k)
	if keep {
		run.records[k] = e
		p := coord.StoredProtocol(e.value.Protocol)
		if run.byProtocol[p] == nil {
			run.byProtocol[p] = map[gateKey]struct{}{}
		}
		run.byProtocol[p][k] = struct{}{}
		if l == v.s.leases && e.value.Layout < layoutDutyLane {
			run.oldLayout[k] = struct{}{}
		}
	}
	if kve.Revision() > run.applied[l] {
		run.applied[l] = kve.Revision()
		close(run.advanced)
		run.advanced = make(chan struct{})
	}
}

// forget removes one key from a run and its indexes. Held under the view's
// mutex.
func (r *gateRun) forget(k gateKey) {
	e, ok := r.records[k]
	if !ok {
		return
	}
	delete(r.records, k)
	p := coord.StoredProtocol(e.value.Protocol)
	if keys := r.byProtocol[p]; keys != nil {
		delete(keys, k)
		if len(keys) == 0 {
			delete(r.byProtocol, p)
		}
	}
	delete(r.oldLayout, k)
}

// stopIfIdle stops the watch when no gate has been judged for gateViewIdle,
// and otherwise re-arms for the rest of the interval.
func (v *gateView) stopIfIdle() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.run == nil {
		return
	}
	if rest := gateViewIdle - time.Since(v.lastUse); rest > 0 {
		v.idle.Reset(rest)
		return
	}
	v.halt()
}

// close stops the view and waits for its goroutines.
func (v *gateView) close() {
	v.mu.Lock()
	if v.idle != nil {
		v.idle.Stop()
	}
	var exited chan struct{}
	if v.run != nil {
		exited = v.run.exited
	}
	v.halt()
	v.mu.Unlock()
	if exited != nil {
		<-exited
	}
}

// halt stops the running watch, if any. The run is dropped with it: a view
// that is not running learns of no write, so nothing it held could be
// trusted when it starts again. Held under the view's mutex.
func (v *gateView) halt() {
	if v.run == nil {
		return
	}
	v.run.stop()
	v.run = nil
}

// streamPoint reads a lease bucket's last sequence and clock from its stream
// LEADER, in one request.
//
// The sequence is the barrier's target; the clock is the store's own, the one
// every deadline is judged against ([Store.storeNow]). A stream info a member
// answers while its group has NO leader is that member's own copy — the hole
// [keysUnder] refuses for the same reason — so it is refused here too: a
// barrier short of the quorum's last write would let a gate pass a record the
// quorum holds. An empty bucket has nothing to wait for.
func (s *Store) streamPoint(ctx context.Context, l *lane) (uint64, time.Time, error) {
	stream, err := s.js.Stream(ctx, l.stream)
	if err != nil {
		return 0, time.Time{}, unavailable("read "+l.kv.Bucket()+"'s last write", err)
	}
	info := stream.CachedInfo()
	switch {
	case info == nil || info.TimeStamp.IsZero():
		return 0, time.Time{}, fmt.Errorf("%w: the broker did not report %s's stream state",
			coord.ErrUnavailable, l.kv.Bucket())
	case info.Cluster != nil && info.Cluster.Leader == "":
		return 0, time.Time{}, unavailable("read "+l.kv.Bucket()+"'s last write", errIndexLeaderless)
	}
	last := info.State.LastSeq
	if info.State.Msgs == 0 {
		last = 0
	}
	return last, info.TimeStamp.UTC(), nil
}

// Close stops what the store runs in the background — the gates' view of the
// lease buckets — and waits for it. The store answers nothing after it that
// needs the view without starting it again.
func (s *Store) Close() {
	s.gate.close()
}
