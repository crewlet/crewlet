package kv

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// gateView is the protocol gate's view of the seat lease bucket: a WATCH that
// keeps the newest record of every key of a class [coord.ProtocolGateCounts]
// counts, indexed by protocol, made exact by a BARRIER. Before a gate is
// judged, [Store.streamPoint] reads the bucket's last sequence and clock from
// the stream leader and the gate waits until the view has consumed through it,
// so it sees what a certified listing would, whatever member the watch reads.
// A listing per claim costs a function of the leases held, and the cheaper
// shapes break the gate (package doc, "The protocol gate's view").
// It runs only while this node judges gates, stopping [gateViewIdle] after the
// last; a view that cannot reach the leader, does not catch up within the read
// budget, or whose watch dies answers UNKNOWN, and the next gate restarts it.
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
// judged: twelve of the seat sweep's five-second passes, so a converging node
// keeps one view across its passes rather than reloading the bucket each one,
// while a node that has stopped claiming stops receiving the fleet's lease
// writes, the view's whole standing cost.
const gateViewIdle = time.Minute

func newGateView(s *Store) *gateView {
	return &gateView{s: s}
}

// blocked reports whether any live presence or seat lease is held at a
// protocol below protocol — the protocol gate's predicate, judged exactly
// (see [gateView]).
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
// By the DEADLINE even on the seat lease bucket, where [Store.held] reads a
// record taken at the bucket's full age as live while readable: the bucket
// reaps it at that deadline, and a view, which learns of writes but never of
// expiries, would keep it for ever. A claiming record (epoch 0) is held.
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
// The watch OUTLIVES the gate that starts it, ended by the idle stop or Close,
// so it takes the caller's context without its cancellation: a claim that
// gives up must not stop the view every later claim reads.
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
// LEADER, in one request: the barrier's target, and the store's own clock
// every deadline is judged against ([Store.storeNow]). A stream info a member
// answers with NO leader is its own copy, refused as [keysUnder] refuses it:
// a barrier short of the quorum's last write would let a gate pass a record
// the quorum holds. An empty bucket has nothing to wait for.
func (s *Store) streamPoint(ctx context.Context, l *lane) (uint64, time.Time, error) {
	info, err := leaderInfo(ctx, s.js, l.stream)
	if err != nil {
		return 0, time.Time{}, unavailable("read "+l.kv.Bucket()+"'s last write", err)
	}
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
