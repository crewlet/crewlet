package kv

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// The protocol gate's view of the seat lease bucket.
//
// The protocol gate asks a question about EVERY live lease record of the
// classes it counts — presence and seats, [coord.ProtocolGateCounts], which
// says why a duty is not one: whether any is held at a lower protocol than the
// claim (coord.ProtocolVersion, ADR-0016). It asked it by LISTING the lease
// buckets — a certified walk of every lease in the fleet — on every gated
// claim, twice (the check and the re-check), and in every FleetProtocolFloor.
// So a claim cost a function of the leases already held: measured on a
// three-member cluster with about 9,300 seats held, a gated seat claim took
// 296–506 ms (p99 1.4–3.4 s), ten claimers managed 27 claims a second
// between them, and a hundred nodes claiming ten thousand seats from cold
// drove the process past 10 GB and the members' raft groups out of quorum —
// the placement sweep claims seats one after another, each claim a walk of
// everything the claims before it had written.
//
// # What the gate actually needs, and what it cannot be told
//
// The question is about VALUES — a record's protocol is in its body, not its
// key — and the builds it exists for are the LOWER-protocol ones, whose lease
// records are all the gate can count on them writing. So nothing such a build
// writes can be narrowed at the broker by protocol, and no record a higher
// build could add would be written by the builds the gate is for. Three
// cheaper shapes were weighed and each breaks the gate:
//
//   - GATING ON PRESENCE ALONE, a listing of the `node` class — O(nodes) —
//     misses a lower-protocol node that holds seats without presence, and
//     that is not a corner: a drain gives presence up at its FIRST step (it
//     is what moves the node's share to its peers) and goes on serving its
//     seats until each is handed over. Every higher node would claim beside
//     it for the whole drain, which is the mixed-protocol fleet the gate
//     exists to prevent.
//   - A FLOOR REFRESHED ON A CADENCE, by this node or published by a
//     singleton, is as old as its cadence: a lower-protocol node that joined
//     inside it is missed by the check AND by the re-check, so the window the
//     re-check bounds to one claim widens to the cadence.
//   - A PER-OWNER PROTOCOL RECORD, which a class listing could read in
//     O(nodes), is written only by the builds that publish it — and the gate
//     must stop for any lower build that does not.
//
// # So the view is incremental, and exact by a sequence barrier
//
// The view is a WATCH of the seat lease bucket — the one holding `seat:` and
// `node:` leases and nothing else — keeping the newest record of every key of
// a counted class as it arrives, indexed by the protocol the gate judges. The
// duty bucket is not watched at all: no record in it is counted, and its
// writes are the fleet's duty re-claims and the tracker's walk-claim
// heartbeats, which the view would only take in to throw away. The view still
// keeps only what [coord.ProtocolGateCounts] counts, rather than trusting
// where a record was written: which bucket a class lives in is this store's
// layout, and which classes the gate judges is the contract's. The cost of a
// gate is then the lower-protocol records — none outside a rolling
// upgrade — plus one BARRIER, and the barrier is what makes it exact rather
// than merely recent. Before a gate is judged, the bucket's last sequence is
// read from the stream LEADER (a stream info refused when it names no leader,
// as a listing's index is), and the gate waits until the view has consumed
// through it. Then the view holds the newest record of every key written
// before that read — every key live from before the read until after it is in
// the view, which is the certified listing's own guarantee (walk.go) — so the
// check and the re-check see what a walk would have seen. The same read
// carries the store's clock, so judging a record's deadline costs nothing
// more. A view that cannot reach the leader, or does not catch up within the
// read budget, answers UNKNOWN; its watch dying does too, and the next gate
// starts it again.
//
// The watch reads whichever member the broker places it on, which may be
// behind; the barrier waits that out, which is why no copy's lag can hide a
// record from a gate.
//
// # What it costs, and why it stops
//
// Per gate: one stream info, where the listing it replaces was a watch
// consumer created and deleted per bucket (two metadata proposals each) and
// every record in the fleet read, twice per claim. Measured on one embedded
// member (BenchmarkAGatedClaim): a gated claim took 5.8 ms with ten leases
// held and 231 ms with ten thousand; it takes about 1.0 ms at both, and the
// view's load of ten thousand records, paid once per run, about 130 ms. What
// the view costs INSTEAD is every write to the seat lease bucket delivered to
// it while it runs — a fleet's heartbeats, one per held seat or presence
// lease per renew — so it runs only while this node is judging gates and
// stops [gateViewIdle] after the last one. A node at its share of seats
// claims nothing and pays nothing, and neither does one with room whose
// every candidate a peer holds: a claim on a held resource is refused
// [coord.RefusedHeld] before any gate, and the seat host reads the fleet's
// floor only after a claim the gate refused. One converging from cold pays
// one load of the bucket and then the writes of the minutes it claims
// through. A duty claim is ungated, so holding a duty starts no view.
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
	// records is the newest record of every key of a counted class in the
	// seat lease bucket, as far as applied reaches. A key whose newest
	// message is a delete or purge marker, or a tombstone (no owner), or
	// whose class the gate does not count, holds nothing a gate judges and
	// is not kept.
	records map[string]entry
	// byProtocol indexes records by the protocol the gate judges, so the
	// gate never iterates the current build's leases: it reads the
	// protocols BELOW the claim's.
	byProtocol map[int]map[string]struct{}
	// applied is the highest stream sequence of the bucket this run has
	// taken in, whatever class its message was.
	applied uint64
	// advanced is closed and replaced whenever applied moves or the run
	// fails, so a barrier waits on it rather than polling.
	advanced chan struct{}
	// failed is why the watch stopped, when it stopped on its own.
	failed error

	stop context.CancelFunc
	// exited is closed when every goroutine of the run has returned.
	exited chan struct{}
}

// gateViewIdle is how long the view keeps running after the last gate it
// judged.
//
// A MINUTE: twelve of the seat sweep's five-second passes, so a node that is
// converging — claiming a share of seats a pass at a time — keeps one view
// through all of it instead of loading the bucket again every pass. Past it a
// node that has stopped claiming stops receiving the fleet's lease writes,
// which is the whole of the view's standing cost.
const gateViewIdle = time.Minute

func newGateView(s *Store) *gateView {
	return &gateView{s: s}
}

// blocked reports whether any live presence or seat lease is held at a
// protocol below protocol — the protocol gate's predicate, judged exactly
// (see the file doc).
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

// floor is the lowest protocol among the live leases the gate counts, and
// whether there were any — the same records [gateView.blocked] judges, so a
// floor read after a refusal names the lease that refused it.
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

// anyLive reports whether any of keys is a record the gate counts as held,
// dropping the ones whose deadline has passed: a record past its deadline is
// never held again — a renew is a new message — so the sets the gate
// iterates stay the size of what is live. Held under the view's mutex.
func (r *gateRun) anyLive(keys map[string]struct{}, clk gateClock) bool {
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

// gateClock is the bucket's clock, read by the barrier with the sequence.
type gateClock time.Time

// live is the gate's "held": a record naming an owner whose deadline, on its
// own bucket's clock, has not passed.
//
// Judged by the DEADLINE even on the seat lease bucket, where a record taken
// at the bucket's full age is otherwise judged live for as long as it can be
// read ([Store.held]): the bucket reaps such a record at that same deadline,
// and a view — which learns of a write but never of an expiry — would keep a
// reaped record for ever. A claiming record (epoch 0) is held, as the gate
// has always counted it.
func (c gateClock) live(e entry) bool {
	return e.value.Owner != "" && e.created.Add(e.value.ttl()).After(time.Time(c))
}

// settle is the barrier: it reads the seat lease bucket's last sequence and
// clock from its stream leader, waits until the view has taken in every
// message through that sequence, and hands back the run it waited on and the
// clock. The view is started if it is not running.
func (v *gateView) settle(ctx context.Context) (*gateRun, gateClock, error) {
	run, err := v.ensure(ctx)
	if err != nil {
		return nil, gateClock{}, err
	}
	target, at, err := v.s.streamPoint(ctx, v.s.leases)
	if err != nil {
		return nil, gateClock{}, err
	}
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, v.s.js.Options().DefaultTimeout)
		defer cancel()
	}
	for {
		v.mu.Lock()
		failed, advanced, caught := run.failed, run.advanced, run.applied >= target
		v.mu.Unlock()
		switch {
		case failed != nil:
			return nil, gateClock{}, unavailable("judge the protocol gate", failed)
		case caught:
			return run, gateClock(at), nil
		}
		select {
		case <-advanced:
		case <-ctx.Done():
			return nil, gateClock{}, unavailable("judge the protocol gate", fmt.Errorf(
				"the view did not catch up with %s's last write: %w",
				v.s.leases.kv.Bucket(), ctx.Err()))
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
	l := v.s.leases
	w, err := l.kv.WatchAll(ctx)
	if err != nil {
		cancel()
		return nil, unavailable("watch "+l.kv.Bucket()+" for the protocol gate", err)
	}
	run := &gateRun{
		records:    map[string]entry{},
		byProtocol: map[int]map[string]struct{}{},
		advanced:   make(chan struct{}),
		stop:       cancel,
		exited:     make(chan struct{}),
	}
	v.run = run
	go func() {
		defer close(run.exited)
		v.consume(ctx, run, l, w)
	}()
	if v.idle == nil {
		v.idle = time.AfterFunc(gateViewIdle, v.stopIfIdle)
	} else {
		v.idle.Reset(gateViewIdle)
	}
	return run, nil
}

// consume applies the bucket's watch to its run until the watch ends.
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

// apply takes one message of the bucket into a run. Every message advances
// the run, whatever its class, because the barrier waits on the bucket's
// sequence; only a live-owner record of a class the gate counts is kept.
func (v *gateView) apply(ctx context.Context, run *gateRun, l *lane, kve jetstream.KeyValueEntry) {
	k := kve.Key()
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
		keep = ok && e.value.Owner != "" && coord.ProtocolGateCounts(e.resource)
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
		p := e.value.Protocol
		if run.byProtocol[p] == nil {
			run.byProtocol[p] = map[string]struct{}{}
		}
		run.byProtocol[p][k] = struct{}{}
	}
	if kve.Revision() > run.applied {
		run.applied = kve.Revision()
		close(run.advanced)
		run.advanced = make(chan struct{})
	}
}

// forget removes one key from a run and its indexes. Held under the view's
// mutex.
func (r *gateRun) forget(k string) {
	e, ok := r.records[k]
	if !ok {
		return
	}
	delete(r.records, k)
	p := e.value.Protocol
	if keys := r.byProtocol[p]; keys != nil {
		delete(keys, k)
		if len(keys) == 0 {
			delete(r.byProtocol, p)
		}
	}
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

// Close stops what the store runs in the background — the gate's view of the
// seat lease bucket — and waits for it. The store answers nothing after it that
// needs the view without starting it again.
func (s *Store) Close() {
	s.gate.close()
}
