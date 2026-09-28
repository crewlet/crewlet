package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	"github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/objstore/transfer"
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The object store, wired — ADR-0019.
//
// # What every node runs, and what only a data node does
//
// Every node holds the placement map ([transfer.Cache], re-read from the
// coordination store, with the layout it places by computed once per map) and
// a client that writes and reads chunks through the broker: a stateless node
// uploads and downloads files exactly as a data node does, it just keeps none
// of the bytes. A DATA NODE additionally keeps the chunks placed on it
// ([disk.Store], under `store.objects.dir`), answers for them on its own
// subject, claims its MEMBERSHIP on a lease of its own (objectslease.go) that
// carries the store's health, and runs the passes that keep its disk in step
// with the map ([upkeep.Node]) — those start with the native runtime, because
// what they measure against is the estate's references.
//
// The map itself is maintained by ONE node at a time, a fleet duty like the
// trim: it reads the objects leases, takes the replica count and the failure
// domain from the COMPANY ([Engine.objectsCompany]), and takes a member out
// once it has been gone or failed for [membership.OutTicks] of its ticks. An
// operator's gestures on it go through [ObjectsControl].

// objectStore is this node's part in the object store.
type objectStore struct {
	// disk is this node's own chunks, nil on a node without data.
	//
	// SET ONCE, BEFORE [Engine.startObjects] PUBLISHES THE STORE, AND NEVER
	// WRITTEN AGAIN — [objectStore.release] closes it and leaves the
	// pointer. Readers run on goroutines teardown does not wait for: the
	// alarm reading is reached from an operator's retention query, which
	// the API may still be answering while the engine stops. A plain
	// pointer that teardown set to nil was a data race with every one of
	// them, and a reader that passed its nil check before the write and
	// called through the pointer after it dereferenced nil and took the
	// process down mid-shutdown. A closed store needs no nil to say so: it
	// refuses every operation with [disk.ErrClosed], and its health is
	// the last its probe found.
	disk *disk.Store

	cache  *transfer.Cache
	client *transfer.Client

	// control is the operator's gestures on the map.
	control *ObjectsControl

	// weight and labels are what this node's membership offers, fixed for
	// the process: they are Tier A, which a restart changes.
	weight int
	labels map[string]string

	// stopServe withdraws this node's chunk server, nil where it serves
	// none.
	stopServe queue.Unsubscribe

	// lease is this data node's membership, nil on a node without data.
	lease *objectsLease

	// stopCache ends the map refresh; cacheDone closes when it has.
	stopCache context.CancelFunc
	cacheDone chan struct{}

	// maintainer is the map duty's loop, nil until the node that claims it
	// exists.
	maintainer *loop

	// passes is a data node's repair, collection and scrub, started with
	// the native runtime. ATOMIC because an apply starts them on its own
	// goroutine while the alarm tick and the lease read their status on
	// others.
	passes atomic.Pointer[objectPasses]

	// passesMu serialises starting the passes against stopping them, and
	// passesStopped is set, under it, once [Engine.stopObjectPasses] has
	// run: the passes never start after that.
	//
	// ITS OWN FLAG, because nothing else can say it. The native runtime
	// that starts the passes may be brought up by an apply on its own
	// goroutine, as late as the teardown itself, and the disk is not
	// released by being set to nil any more (see disk above) — so a check
	// of the disk would start passes over a closed store, and a check of
	// the passes alone reads their absence after a stop exactly as their
	// absence before a start. A LOCK AND NOT AN ATOMIC FLAG, because the
	// check and the start have to be one step: a start that read the flag
	// clear, then lost the processor to a stop that found nothing to stop,
	// would publish passes nothing ever ends.
	passesMu      sync.Mutex
	passesStopped bool
}

// local is the chunk store as the client takes it — a NIL INTERFACE on a node
// that holds none, rather than an interface holding a nil pointer, which the
// client would read as a store and call.
func (o *objectStore) local() transfer.Chunks {
	if o.disk == nil {
		return nil
	}
	return o.disk
}

// objectMapReadBudget bounds the first read of the placement map at boot.
//
// FIVE SECONDS, the estate's admission budget: a map that has not been read by
// then is read by the refresh loop ten seconds later, and a boot held for it
// would hold every seat on this node for a map only an upload needs.
const objectMapReadBudget = 5 * time.Second

// startObjects brings up this node's part in the object store.
func (e *Engine) startObjects(ctx context.Context, boot *config.Bootstrap) error {
	if e.backends == nil || e.backends.Queue == nil || e.backends.Fleet == nil {
		return nil
	}
	cache := transfer.NewCache(e.backends.Fleet)
	o := &objectStore{
		cache:   cache,
		control: &ObjectsControl{store: e.backends.Fleet, observer: cache, now: time.Now},
	}
	if holdsData(boot) {
		dir := boot.Store.ObjectsDirFor()
		d, err := disk.Open(dir)
		if err != nil {
			return fmt.Errorf("engine: the object store's directory (store.objects.dir): %w", err)
		}
		o.disk = d
		o.weight = boot.Store.Objects.ObjectWeight()
		if len(boot.Node.Labels) > 0 {
			o.labels = maps.Clone(boot.Node.Labels)
		}
	}
	read, cancel := context.WithTimeout(ctx, objectMapReadBudget)
	if err := o.cache.Refresh(read); err != nil {
		log.WarnContext(ctx, "object_map_unread", "error", err,
			"detail", "the refresh loop reads it again shortly; uploads wait for it")
	}
	cancel()
	if o.disk != nil {
		stop, err := transfer.Serve(ctx, e.backends.Queue, e.id, o.disk, o.cache.Layout)
		if err != nil {
			_ = o.disk.Close()
			return fmt.Errorf("engine: serve this node's chunks: %w", err)
		}
		o.stopServe = stop
		// THE MEMBERSHIP AFTER THE SERVER, so a peer that reads this node
		// as a member can already reach it; and on the ENGINE's own loop
		// rather than the seat host's, because a membership outlives the
		// host's drain (see objectslease.go). Under the process's one
		// incarnation, which the host's leases carry too: the loop is
		// separate, the identity is not. Read off the engine because this
		// runs before node.New builds the host.
		if e.backends.Coord != nil {
			o.lease = startObjectsLease(ctx, e.backends.Coord, e.id, e.incarnation,
				e.leaseTTL, o.leaseMeta)
		}
	}
	client, err := transfer.NewClient(transfer.ClientOptions{
		Queue: e.backends.Queue, Self: e.id, Local: o.local(), Layouts: o.cache.Layout,
		Refresh: o.cache.Refresh,
	})
	if err != nil {
		o.release(ctx)
		return fmt.Errorf("engine: the object client: %w", err)
	}
	o.client = client
	// DETACHED, like every long-running loop here: the refresh must outlive
	// the boot's context and stop only with the engine.
	refresh, stop := context.WithCancel(context.WithoutCancel(ctx))
	o.stopCache, o.cacheDone = stop, make(chan struct{})
	go func() {
		defer close(o.cacheDone)
		o.cache.Run(refresh)
	}()
	e.objects = o
	return nil
}

// leaseMeta is this beat's account of the store for its membership lease: the
// share it offers and the labels its failure domain is read from, its health
// as a probe finds it NOW, and what its passes last found — each absent until
// there is something to say, never a zero standing in for it.
func (o *objectStore) leaseMeta() objstore.ObjectsMeta {
	health := o.disk.Probe()
	out := objstore.ObjectsMeta{
		Weight: o.weight, Labels: o.labels,
		Health: &objstore.ObjectsHealth{
			State: string(health.State), Detail: health.Detail,
			UsedPercent: health.UsedPercent,
		},
	}
	p := o.passes.Load()
	if p == nil {
		return out
	}
	m, placed := o.cache.Current()
	fillPassesMeta(p.node.Status(), m.Epoch, placed, &out)
	return out
}

// fillPassesMeta is what the passes found, as the membership lease carries it,
// given the map epoch this node places by (placed false: none yet).
//
// STRAYS ONLY FROM A COLLECTION THAT WALKED EVERY SLOT AT THAT EPOCH, and
// absent otherwise. The count is what an operator stops a member taken out on
// — `crewlet objects status` reads zero as "empty, may be stopped for good" —
// and two other counts would read as that zero while meaning nothing of the
// kind: one from a pass that stopped short, which never looked at most of the
// disk, and one from before the map moved, which counted strays against a
// placement that no longer stands. The second is the ordinary case: a member
// taken out at epoch N last collected at N-1, where it held no strays at all,
// and its whole share only becomes strays under N. Absent says "not reported
// yet", which is what the status then tells the operator to wait for.
//
// THE SCRUB AS SOON AS IT HAS ANYTHING TO SAY: a cycle begun, or an error with
// none begun yet. The first walk of a disk that refuses it stops the scrub
// before any cycle starts, and a report keyed on the cycle alone was absent
// exactly then — every surface read "no scrub yet" off a node whose scrub had
// already failed, which is the one reading an operator must see.
func fillPassesMeta(status upkeep.Status, epoch uint64, placed bool, out *objstore.ObjectsMeta) {
	if r := status.Repair; !r.At.IsZero() {
		out.Repair = &objstore.ObjectsRepair{
			Epoch: r.Epoch, Completed: r.Completed, Placed: r.Placed, Held: r.Held,
			Pending: r.Pending, Unreachable: r.Unreachable, Missing: r.Missing, At: r.At,
		}
	}
	if s := status.Scrub; !s.CycleStarted.IsZero() || s.Error != "" {
		out.Scrub = &objstore.ObjectsScrub{
			CycleStarted: s.CycleStarted, Progress: s.Progress,
			Verified: s.Verified, Rotten: s.Rotten,
			Unreadable: s.Unreadable, Error: s.Error,
		}
	}
	if c := status.Collected; placed && !c.At.IsZero() && c.Epoch == epoch {
		strays := c.Strays
		out.Strays = &strays
	}
}

// stopObjects ends this node's passes, withdraws its chunk server and releases
// its directory. Nil-safe.
//
// NOT THE MAP DUTY'S LOOP, which [Engine.stopObjectMap] ends with the other
// duty loops: this runs after the duties are given back, and a loop that
// claims one has to be stopped before that.
func (e *Engine) stopObjects(ctx context.Context) {
	if e.objects == nil {
		return
	}
	e.stopObjectPasses()
	// THE POINTER STAYS: a request the API is still answering may hold it,
	// and a client over a withdrawn server fails that request honestly
	// where a nil would crash it.
	e.objects.release(ctx)
}

// stopObjectMap ends the map duty's loop, waiting out a turn in flight.
// Nil-safe.
//
// BEFORE [Engine.releaseDuties], with every other loop that claims a duty. The
// loop claims the duty afresh on every turn, and a claim of a released lease
// simply succeeds — so a loop still running when the duty was given back
// took it again under this incarnation, and was then stopped holding it. The
// node exited owning the placement map for the lease's whole TTL, and for
// that long nobody maintained it: no absence counted, no failed or new
// member handled, and on a new fleet no first map, so every upload was
// refused. A peer could also take the duty in the moment after the release
// while this node's loop still ticked, which is two maintainers at once.
func (e *Engine) stopObjectMap() {
	if e.objects == nil || e.objects.maintainer == nil {
		return
	}
	e.objects.maintainer.stop()
	e.objects.maintainer = nil
}

// release undoes whatever of [Engine.startObjects] ran.
//
// THE SERVER, THEN THE MEMBERSHIP. A member is what writers send copies to and
// readers ask: one whose lease outlived its server would be asked for chunks
// it no longer answers for, and one whose lease went first would be counted
// absent while it still serves — the drain-long reshuffle the lease exists to
// prevent, in miniature.
//
// THE DISK IS CLOSED AND KEPT (see objectStore.disk): its readers are not all
// ones this teardown waits for. Closing is idempotent, so a second release — a
// Stop after a Stop — finds nothing left to close.
func (o *objectStore) release(ctx context.Context) {
	if o.stopServe != nil {
		if err := o.stopServe(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "objects_not_withdrawn", "error", err.Error())
		}
		o.stopServe = nil
	}
	if o.lease != nil {
		o.lease.stop(ctx)
		o.lease = nil
	}
	if o.stopCache != nil {
		o.stopCache()
		<-o.cacheDone
		o.stopCache = nil
	}
	if o.disk != nil {
		if err := o.disk.Close(); err != nil {
			log.WarnContext(ctx, "objects_dir_not_released", "error", err.Error())
		}
	}
}

// objectMapDuty is the fleet singleton that maintains the placement map.
const objectMapDuty = "object-map"

// objectMapInterval is how often the map's holder brings it up to date: the
// maintainer's own tick, which every absence is counted in — so the duty runs
// it at exactly the cadence its grace was derived from ([membership.OutTicks]).
const objectMapInterval = membership.TickInterval

// objectMapDutyTTL is three ticks, the ratio every singleton here uses: one
// missed tick must not hand the map to a peer mid-change.
const objectMapDutyTTL = 3 * objectMapInterval

// objectMapUnplacedPoll is how soon the map duty ticks again after a tick that
// found no stored map.
//
// ONE SECOND, and only until a map exists. Without one no node can store a
// file — every upload is refused as unavailable — and the first tick of a
// booting fleet routinely finds nothing to place on: it runs as the engine is
// built, racing this node's own objects lease, whose first claim is on a
// goroutine of its own. At the reconcile interval that cost every fresh fleet
// fifteen seconds with no object store (measured in internal/e2e, a seat's
// first upload failing on exactly that). A tick with no map is one lease
// listing and one key read, so a second is cheap, and the first map written
// ends it.
//
// It counts no absence, and that is guaranteed by WHAT IT IS KEYED ON: the
// tick's own report that it found no map in the store ([upkeep.TickResult]),
// never this node's cache of the map. The cache is a second reading on its own
// refresh — it lags a map the maintainer just read, and refuses one it cannot
// decode — and a poll keyed on it ticked every second against a map that
// existed, counting each absence fifteen times too fast.
const objectMapUnplacedPoll = time.Second

// startObjectMap arms the map duty. After the node exists, which the duty's
// lease is claimed through.
func (e *Engine) startObjectMap(ctx context.Context) error {
	if e.objects == nil || e.backends == nil || e.backends.Coord == nil {
		return nil
	}
	m, err := upkeep.NewMaintainer(upkeep.MaintainerOptions{
		Store:    e.backends.Fleet,
		Live:     func(ctx context.Context) ([]upkeep.Presence, error) { return objectHolders(ctx, e.backends.Coord) },
		Company:  e.objectsCompany,
		Observer: e.objects.cache,
	})
	if err != nil {
		return fmt.Errorf("engine: the object map's maintainer: %w", err)
	}
	duty := mapDuty{claim: e.workerDuty(objectMapDuty, objectMapDutyTTL), tick: m.Tick}
	e.objects.maintainer = startLoop(ctx, sleep, duty.turn)
	return nil
}

// mapDuty is this node's part in the map duty: ask for it, and tick while it
// holds it.
type mapDuty struct {
	// claim answers whether this node holds the duty, nil where it has no
	// node to claim it through and ticks alone.
	claim func(context.Context) (bool, error)

	tick func(context.Context) (upkeep.TickResult, error)
}

// turn is one turn of the duty, answering how long to wait before the next.
//
// [objectMapInterval] after a tick that found a map, WHATEVER ELSE HAPPENED:
// that tick counted every open absence, and the grace is
// [membership.OutTicks] of them only if they are that far apart. The poll only after a tick that
// read the store cleanly and found none. And the interval after everything
// else:
//
//   - A turn that does not hold the duty counts nothing, and asks again at the
//     tick's cadence — the ratio every singleton's lease is sized to — since
//     the node that holds it is the one bringing a first map in. Asking every
//     second would be a lease write per second from every other node.
//   - A tick that failed before it could say is retried at the tick's cadence
//     too: the poll is sized for a read that answers, and a store that is
//     failing is not asked every second by a loop that cannot help it.
func (d mapDuty) turn(ctx context.Context) time.Duration {
	if d.claim != nil {
		mine, err := d.claim(ctx)
		if err != nil && ctx.Err() == nil {
			// SAID, as every other duty says it: "a peer holds it" and
			// "the store could not be asked" are the same silence here and
			// very different situations to an operator reading why the
			// map stopped moving.
			log.WarnContext(ctx, "object_map_duty_unclaimed", "error", err)
		}
		if err != nil || !mine {
			return objectMapInterval
		}
	}
	res, err := d.tick(ctx)
	if err != nil && ctx.Err() == nil {
		log.WarnContext(ctx, "object_map_not_maintained", "error", err)
	}
	if res.Mapped || err != nil {
		return objectMapInterval
	}
	return objectMapUnplacedPoll
}

// objectsCompany is what the company currently in force says about the object
// store, read afresh on every tick of the map duty — so a revision changing
// the replica count or the failure domain reaches the map on the next tick,
// with no restart and no rebuild of the duty.
//
// STAMPED WITH THE ACTIVATION'S INSTANT ([configplane.ActivationStamp]), the
// stamp the chart and the knowledge containers take, and for their reason:
// the duty moves between nodes, and a holder a revision behind must not set
// the map back to what an older activation said — the maintainer applies a
// company only at least as recent as the one the map was set by. A company no
// activation has named — a Tier B file a node booted with, before its
// reconciler published it — has no instant, and the map takes nothing from
// it, exactly as [Engine.applyChart] writes nothing.
func (e *Engine) objectsCompany() (membership.Company, bool) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return membership.Company{}, false
	}
	// ONE TEST FOR BOTH: the zero instant — no activation — stamps before
	// the Unix epoch, as would any instant an unsigned epoch cannot carry
	// without wrapping into one that outranks every later activation.
	stamp := configplane.ActivationStamp(c.ActivatedAt)
	if stamp <= 0 {
		return membership.Company{}, false
	}
	return membership.Company{
		Epoch:         uint64(stamp),
		Replicas:      c.Config.Objects.ReplicaCount(),
		FailureDomain: c.Config.Objects.FailureDomain,
		Block:         "objects",
	}, true
}

// objectHolders is every data node holding its object-store membership lease,
// as the maintainer weighs it.
//
// THE OBJECTS LEASES AND NOTHING ELSE — never presence, which a drain gives up
// while the node still serves (objectslease.go). A lease whose weight cannot
// be read offers no share and is skipped ([objstore.ObjectsFromMeta]); one
// whose store reports itself FAILED is unhealthy, which the map counts exactly
// as absent. Any other state, an unknown one included, is healthy: a full
// store still serves what it holds, and a state this build does not know is
// not one that said it failed.
func objectHolders(ctx context.Context, leases liveLeases) ([]upkeep.Presence, error) {
	held, err := leases.ListLive(ctx, coord.ClassObjects)
	if err != nil {
		return nil, err
	}
	out := make([]upkeep.Presence, 0, len(held))
	for _, lease := range held {
		node, ok := coord.ObjectsNode(lease.Resource)
		if !ok {
			continue
		}
		meta, ok := objstore.ObjectsFromMeta(lease.Meta)
		if !ok {
			continue
		}
		p := upkeep.Presence{Node: node, Weight: meta.Weight, Labels: meta.Labels}
		if h := meta.Health; h != nil && disk.HealthState(h.State) == disk.HealthFailed {
			p.Unhealthy, p.Detail = true, h.Detail
		}
		if r := meta.Repair; r != nil {
			// THE PASS'S OWN RULE for what it may claim: one that did not
			// reach every group reports nothing, so a node never counts
			// clean for a split on groups it did not look at.
			p.Repair = upkeep.RepairStatus{
				Epoch: r.Epoch, Completed: r.Completed, Pending: r.Pending,
			}.Report()
		}
		out = append(out, p)
	}
	return out, nil
}

// loop is a function run on a detached context until stopped, waiting for a
// run in flight.
type loop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startLoop runs run at once, and again each time the wait it answered has
// passed — the RUN answers it, so a loop paces itself on what that run found
// rather than on a second reading taken beside it. wait sleeps for a duration
// or until the context ends ([sleep]), a parameter for the tests.
func startLoop(ctx context.Context, wait func(context.Context, time.Duration),
	run func(context.Context) time.Duration) *loop {

	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l := &loop{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		for ctx.Err() == nil {
			wait(ctx, run(ctx))
		}
	}()
	return l
}

func (l *loop) stop() {
	l.cancel()
	<-l.done
}

// errNoObjectStore is a gesture that needs the object store on a node that
// runs none — an engine built without a broker, which is a test's.
var errNoObjectStore = errors.New("engine: this node runs no object store")

// objectPasses is a data node's repair, collection and scrub.
type objectPasses struct {
	node   *upkeep.Node
	cancel context.CancelFunc
	// done closes when the pass loop and the scrub have both ended.
	done chan struct{}

	mu sync.Mutex
	// epoch is the map epoch this node last saw placed, and epochSince
	// when it first saw it — what the degraded alarm measures a repair's
	// lateness from.
	epoch      uint64
	epochSince time.Time
}

// objectEpochPoll is how often the passes look for a new map epoch.
//
// THE MAP CACHE'S OWN INTERVAL: a new epoch cannot be seen sooner than the
// cache reads it, and a repair started within one refresh of a change is the
// whole of what "at once" can mean here.
const objectEpochPoll = transfer.CacheInterval

// objectPassRetryFloor is how soon a pass that left work is tried again,
// doubling on every further failure up to the interval that pass runs on
// anyway ([nextPassRetry]).
//
// THIRTY SECONDS: two heartbeats, long enough for a peer that was restarting
// to be back, and three map refreshes. A repair stopped by one member that did
// not answer used to wait out the whole ten-minute interval with this node
// short of copies it could have fetched a minute later; retrying at the poll
// instead would ask a member that is gone for good once every ten seconds.
// Doubling keeps the first retry quick and a long outage cheap. The same
// schedule serves the collection an epoch owes once the fleet has settled,
// for the same reasons: a barrier or a peer that failed it is usually back in
// a minute, and one that is not should not be asked every ten seconds.
const objectPassRetryFloor = 30 * time.Second

// nextPassRetry is the wait after a failed pass, given the last one: the floor
// first, then doubling, never past ceiling — the interval the pass runs on
// regardless, so a retry never comes later than the pass would have.
func nextPassRetry(last, ceiling time.Duration) time.Duration {
	if last <= 0 {
		return min(objectPassRetryFloor, ceiling)
	}
	return min(2*last, ceiling)
}

// startObjectPasses runs this data node's repair, collection and scrub against
// the estate's references. Nil-safe on a node that holds no chunks, and a
// no-op once the passes have been stopped ([objectStore.passesStopped]).
func (e *Engine) startObjectPasses(ctx context.Context, refs upkeep.References) error {
	o := e.objects
	if o == nil || o.disk == nil {
		return nil
	}
	o.passesMu.Lock()
	defer o.passesMu.Unlock()
	if o.passesStopped || o.passes.Load() != nil {
		return nil
	}
	n, err := upkeep.NewNode(upkeep.NodeOptions{
		Self: e.id, Local: o.disk, Peers: o.client, Layouts: o.cache.Layout, References: refs,
	})
	if err != nil {
		return fmt.Errorf("engine: the object store's passes: %w", err)
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &objectPasses{node: n, cancel: cancel, done: make(chan struct{})}
	o.passes.Store(p)
	schedule := &passSchedule{self: e.id, node: n, settled: e.objectsSettled(), now: time.Now}
	var both sync.WaitGroup
	both.Add(2)
	go func() {
		defer both.Done()
		p.run(loopCtx, o.cache, schedule)
	}()
	// THE SCRUB ON ITS OWN GOROUTINE, at its own paced rate. It reads every
	// chunk this node holds once a week, so on the passes' goroutine it
	// would either hold repair up for the week or be cut short by every
	// pass; and it decides nothing about which chunks belong here — it only
	// checks bytes against their names — so running beside the passes is
	// safe where a collection beside a repair is not.
	go func() {
		defer both.Done()
		if err := n.Scrub(loopCtx); err != nil && loopCtx.Err() == nil {
			log.WarnContext(loopCtx, "object_scrub_not_started", "error", err)
		}
	}()
	go func() {
		both.Wait()
		close(p.done)
	}()
	return nil
}

// objectsSettled answers whether the fleet has finished repairing at a map's
// epoch ([upkeep.Settled]), as the members' objects leases report it — nil on
// an engine with no coordination store to read them from, which then collects
// on the hour alone.
//
// THE LEASES, the maintainer's own reading ([objectHolders]), so "settled"
// here is the very answer the map's split gate acts on.
func (e *Engine) objectsSettled() func(context.Context, placement.Map) (bool, error) {
	if e.backends == nil || e.backends.Coord == nil {
		return nil
	}
	leases := e.backends.Coord
	return func(ctx context.Context, m placement.Map) (bool, error) {
		live, err := objectHolders(ctx, leases)
		if err != nil {
			return false, err
		}
		return upkeep.Settled(m, live), nil
	}
}

// stopObjectPasses ends the passes and the scrub for good, waiting for work in
// flight. Nil-safe.
//
// THE WAIT OUTSIDE THE LOCK: a start that arrives meanwhile finds the flag set
// and returns at once, so nothing needs the lock held while a pass winds down.
func (e *Engine) stopObjectPasses() {
	if e.objects == nil {
		return
	}
	o := e.objects
	o.passesMu.Lock()
	o.passesStopped = true
	p := o.passes.Swap(nil)
	o.passesMu.Unlock()
	if p != nil {
		p.cancel()
		<-p.done
	}
}

// run polls the map every [objectEpochPoll] and runs whichever pass is due
// ([passSchedule.poll]).
func (p *objectPasses) run(ctx context.Context, cache *transfer.Cache, s *passSchedule) {
	poll := time.NewTicker(objectEpochPoll)
	defer poll.Stop()
	for {
		m, placed := cache.Current()
		if placed {
			p.saw(m.Epoch, time.Now())
		}
		s.poll(ctx, m, placed)
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
	}
}

// passRunner is what the pass schedule drives: a data node's repair and
// collection, and what they last found.
type passRunner interface {
	Repair(ctx context.Context) error
	Collect(ctx context.Context) error
	Status() upkeep.Status
}

// passSchedule decides which of a data node's passes is due, and remembers
// between polls what ran when.
//
// A REPAIR whenever the map's epoch moves, every [upkeep.RepairInterval], and
// sooner after a pass that left work a retry may do. A COLLECTION every
// [upkeep.CollectInterval] — and once more for every epoch, as soon as the
// fleet has SETTLED at it: this node's own repair completed there, and every
// member the map places on reports a completed one with nothing pending. That
// is the moment the copies the epoch moved away from this node can go, since
// each is now held by every member the map places it on — and a member taken
// out holds its whole share of them, so a decommission that waited for the
// hourly pass sat on a copied node for up to an hour after the fleet had
// finished with it. It runs on EVERY node, the settled fleet's members
// included, because which nodes hold strays is what the walk finds out.
//
// ONE GOROUTINE FOR BOTH, and one pass per poll, so a collection never runs
// beside a repair on the same disk: a repair copying a group in and a
// collection judging that group in the same instant would each be reasoning
// about a directory the other is changing. A repair that is due goes first.
type passSchedule struct {
	// self is this node's id, and node its passes.
	self string
	node passRunner

	// settled answers whether the fleet has finished repairing at a map's
	// epoch ([Engine.objectsSettled]); nil collects on the hour alone.
	settled func(context.Context, placement.Map) (bool, error)

	// now is the clock the intervals are measured on, injected for tests.
	now func() time.Time

	// ranAt is the epoch the last repair ran at — zero, which no map has,
	// until one has run — and repaired when it ended; retry is the last
	// wait after a failed repair, and retryAt when the next is due.
	ranAt             uint64
	repaired, retryAt time.Time
	retry             time.Duration

	// collected is when the last collection ended.
	collected time.Time

	// swept is the epoch whose settled collection has run — zero until one
	// has — and sweepRetry and sweepAt the same backoff as the repair's,
	// after a settled collection that failed.
	swept      uint64
	sweepAt    time.Time
	sweepRetry time.Duration
}

// poll runs the pass that is due against the map this node places by, if one
// is: at most one, and none while there is no map.
func (s *passSchedule) poll(ctx context.Context, m placement.Map, placed bool) {
	if !placed {
		return
	}
	now := s.now()
	retryDue := !s.retryAt.IsZero() && !now.Before(s.retryAt)
	switch {
	case m.Epoch != s.ranAt || now.Sub(s.repaired) >= upkeep.RepairInterval || retryDue:
		s.repair(ctx, m.Epoch)
	case s.settledDue(ctx, m, now):
		s.collectSettled(ctx, m)
	case now.Sub(s.collected) >= upkeep.CollectInterval:
		if err := s.collect(ctx); err != nil && ctx.Err() == nil {
			log.WarnContext(ctx, "object_collect_skipped", "error", err)
		}
	}
}

// repair runs a repair pass at epoch and schedules its retry.
func (s *passSchedule) repair(ctx context.Context, epoch uint64) {
	if epoch != s.ranAt {
		// A NEW MAP starts every retry from the floor: the failures being
		// backed off from were against the old placement.
		s.retry = 0
		s.sweepRetry, s.sweepAt = 0, time.Time{}
	}
	err := s.node.Repair(ctx)
	s.ranAt, s.repaired = epoch, s.now()
	switch {
	case ctx.Err() != nil:
	case err != nil:
		s.retry = nextPassRetry(s.retry, upkeep.RepairInterval)
		s.retryAt = s.repaired.Add(s.retry)
		log.WarnContext(ctx, "object_repair_incomplete", "error", err,
			"retry_in", s.retry.String())
	default:
		s.retry, s.retryAt = 0, time.Time{}
	}
}

// settledDue reports whether the epoch's settled collection is due now: not
// yet run at it, not backing off from a failed one, this node's own repair
// completed at it, and the fleet settled there.
//
// THIS NODE'S REPAIR IS ASKED FIRST, from its own status: it is part of what
// "settled" means, and asking it first keeps the lease listing off every poll
// while this node is still copying its own share in.
func (s *passSchedule) settledDue(ctx context.Context, m placement.Map, now time.Time) bool {
	if s.settled == nil || s.swept == m.Epoch || now.Before(s.sweepAt) {
		return false
	}
	if s.node.Status().Repaired.Epoch != m.Epoch {
		return false
	}
	settled, err := s.settled(ctx, m)
	if err != nil {
		// DEBUG, not a warning: the leases are the coordination store,
		// whose outage every loop on this node is already reporting, and
		// this is asked every poll. The hourly collection still runs.
		log.DebugContext(ctx, "object_settle_unread", "epoch", m.Epoch, "error", err)
		return false
	}
	return settled
}

// collectSettled runs the collection the fleet settling at m's epoch owes.
//
// OWED UNTIL IT LEAVES NO STRAY. A pass that failed, and one that walked every
// slot but kept strays — a member that did not answer the confirmation at that
// moment, or answered by a map a refresh behind — is tried again on the
// repair's backoff, capped at the hour the collection runs on anyway: the
// copies it kept are exactly the ones an operator is waiting on, and the
// member that did not answer is usually back in a minute. A node that holds no
// stray at the epoch is done after one pass — and so is one that keeps every
// copy it holds BY RULE ([upkeep.KeepsEveryCopy], which the collection itself
// asks): one the map does not hold at all, until it is a member again, and a
// member on probation, until the map trusts it. A retry there could only count
// the same strays, every thirty seconds doubling, for as long as the rule
// holds.
func (s *passSchedule) collectSettled(ctx context.Context, m placement.Map) {
	epoch := m.Epoch
	log.InfoContext(ctx, "object_collect_settled", "epoch", epoch,
		"detail", "every member has finished repairing at this epoch; dropping the copies it moved away")
	err := s.collect(ctx)
	if ctx.Err() != nil {
		return
	}
	done := s.node.Status().Collected
	keeps, _ := upkeep.KeepsEveryCopy(m, s.self)
	if err == nil && done.Epoch == epoch && (done.Strays == 0 || keeps) {
		s.swept, s.sweepRetry, s.sweepAt = epoch, 0, time.Time{}
		return
	}
	s.sweepRetry = nextPassRetry(s.sweepRetry, upkeep.CollectInterval)
	s.sweepAt = s.collected.Add(s.sweepRetry)
	if err != nil {
		log.WarnContext(ctx, "object_collect_skipped", "error", err,
			"retry_in", s.sweepRetry.String())
		return
	}
	log.InfoContext(ctx, "object_strays_kept", "epoch", epoch, "strays", done.Strays,
		"retry_in", s.sweepRetry.String(),
		"detail", "a member the map places them on has not confirmed an intact copy yet")
}

// collect runs a collection pass, and restarts the hourly clock from its end.
func (s *passSchedule) collect(ctx context.Context) error {
	err := s.node.Collect(ctx)
	s.collected = s.now()
	return err
}

// saw records the epoch the cache places by, and when this node first saw it.
func (p *objectPasses) saw(epoch uint64, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.epochSince.IsZero() || epoch != p.epoch {
		p.epoch, p.epochSince = epoch, now
	}
}

// seen is the epoch this node last saw placed and when it first saw it, zero
// before it has seen one.
func (p *objectPasses) seen() (uint64, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.epoch, p.epochSince
}

// objectsReading fills the object store's half of an alarm reading: the
// store's health as its last probe found it, and what the passes say about
// the copies the map places here ([fillPassesReading]). Nothing on a node
// holding no chunks, which reads as nothing to report.
//
// Safe beside the teardown, which does not wait for this reader — an
// operator's retention query reaches it from the API: the disk is never
// written after the store is published, and the passes are one atomic load.
func (e *Engine) objectsReading(now time.Time, out *statelog.Reading) {
	o := e.objects
	if o == nil || o.disk == nil {
		return
	}
	fillHealthReading(o.disk.Health(), out)
	p := o.passes.Load()
	if p == nil {
		return
	}
	epoch, since := p.seen()
	fillPassesReading(p.node.Status(), epoch, since, now, out)
}

// fillHealthReading is the store's health as the alarms read it.
func fillHealthReading(health disk.Health, out *statelog.Reading) {
	out.ObjectsHealth = health.State
	out.ObjectsHealthDetail = health.Detail
	out.ObjectsUsedPercent = health.UsedPercent
}

// fillPassesReading is what the passes found, as the alarms read it, given the
// map epoch this node places by and when it first saw it (zero: none yet).
//
// A PASS THAT STOPPED SHORT IS NOT EVIDENCE OF ABSENCE. Every repair pins the
// estate first, so one fails before it looks at a single group whenever a
// log is at its ceiling, has no quorum, or this node's applier trails it —
// and it reports zeroes for everything it never looked at. So:
//
//   - MISSING is the last COMPLETED pass's count, raised by a later pass that
//     stopped short only if that pass counted more. A missing chunk is one
//     every member answered it does not hold, so a short pass's positive
//     count is real — just a floor — while its zero is only the groups it
//     never reached. Taken from the last pass alone, a known loss cleared
//     itself the moment an unrelated barrier failure ended a pass early. At
//     any epoch: a chunk nobody holds is lost under every map.
//   - PENDING is the last completed pass AT THE CURRENT EPOCH, never an older
//     epoch's, which is a claim about a placement the map no longer makes.
//   - UNREPAIRED-FOR runs from the later of the epoch being seen and the last
//     pass that completed at it, so it measures how long this node's repair
//     has gone without completing whether or not the map moved. Measured
//     from the epoch alone, a repair loop that stopped completing at an
//     unchanged epoch reported the last completed pass for ever: objects
//     degraded stayed quiet while the chunks the scrub removed as rotten, or
//     that a pass had found pending, stayed one copy short. The alarm table
//     fires at twice the interval a pass runs on, for the reason it gives.
func fillPassesReading(status upkeep.Status, epoch uint64, since, now time.Time,
	out *statelog.Reading) {

	out.ObjectsMissing = max(status.Repaired.Missing, status.Repair.Missing)
	if since.IsZero() {
		return
	}
	out.ObjectsRepairInterval = upkeep.RepairInterval
	from := since
	if done := status.Repaired; !done.At.IsZero() && done.Epoch == epoch {
		out.ObjectsPending = done.Pending
		out.ObjectsUnreachable = done.Unreachable
		if done.At.After(from) {
			from = done.At
		}
	}
	out.ObjectsUnrepairedFor = now.Sub(from)
}

// GetChunk reads one chunk from wherever the fleet holds it — the backup's
// read, which has to carry every chunk its copy names rather than this node's
// share of them.
func (e *Engine) GetChunk(ctx context.Context, h objstore.Hash) ([]byte, error) {
	if e.objects == nil || e.objects.client == nil {
		return nil, errNoObjectStore
	}
	return e.objects.client.Get(ctx, h)
}

// ObjectStore is this node's object client as the tools take it — a NIL
// INTERFACE on a node running none, so the file tools that need the bytes are
// omitted rather than registered and broken.
func (e *Engine) ObjectStore() builtin.ObjectStore {
	if e.objects == nil || e.objects.client == nil {
		return nil
	}
	return e.objects.client
}

// Objects is this node's object client, nil on a node running none — what a
// surface streaming a file's bytes reads and writes through.
func (e *Engine) Objects() *transfer.Client {
	if e.objects == nil {
		return nil
	}
	return e.objects.client
}

// ObjectsControl is the operator's gestures on the placement map, nil on a
// node that runs no object store. Any node that runs one may make them: the
// map is one record in the coordination store, and a gesture is a
// compare-and-set on it ([ObjectsControl]).
func (e *Engine) ObjectsControl() *ObjectsControl {
	if e.objects == nil {
		return nil
	}
	return e.objects.control
}
