package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	"github.com/crewlet/crewlet/internal/objstore/transfer"
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// The object store, wired — ADR-0019.
//
// # What every node runs, and what only a data node does
//
// Every node holds the placement map ([transfer.Cache], re-read from the
// coordination store) and a client that writes and reads chunks through the
// broker: a stateless node uploads and downloads files exactly as a data node
// does, it just keeps none of the bytes. A DATA NODE additionally keeps the
// chunks placed on it ([disk.Store], under `store.objects.dir`), answers for
// them on its own subject, and runs the two passes that keep its disk in step
// with the map ([upkeep.Node]) — those start with the native runtime, because
// what they measure against is the estate's references.
//
// The map itself is maintained by ONE node at a time, a fleet duty like the
// trim: it reads who is live, adds and weighs the data nodes, and takes one
// out after it has been gone for [upkeep.OutGrace].

// objectStore is this node's part in the object store.
type objectStore struct {
	// disk is this node's own chunks, nil on a node without data.
	disk *disk.Store

	cache  *transfer.Cache
	client *transfer.Client

	// stopServe withdraws this node's chunk server, nil where it serves
	// none.
	stopServe queue.Unsubscribe

	// stopCache ends the map refresh; cacheDone closes when it has.
	stopCache context.CancelFunc
	cacheDone chan struct{}

	// maintainer is the map duty's loop, nil until the node that claims it
	// exists.
	maintainer *loop

	// passes is a data node's repair and collection, started with the
	// native runtime. ATOMIC because an apply starts them on its own
	// goroutine while the alarm tick reads their status on another.
	passes atomic.Pointer[objectPasses]
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
	o := &objectStore{cache: transfer.NewCache(e.backends.Fleet)}
	if holdsData(boot) {
		dir := boot.Store.ObjectsDirFor()
		d, err := disk.Open(dir)
		if err != nil {
			return fmt.Errorf("engine: the object store's directory (store.objects.dir): %w", err)
		}
		o.disk = d
	}
	read, cancel := context.WithTimeout(ctx, objectMapReadBudget)
	if err := o.cache.Refresh(read); err != nil {
		log.WarnContext(ctx, "object_map_unread", "error", err,
			"detail", "the refresh loop reads it again shortly; uploads wait for it")
	}
	cancel()
	if o.disk != nil {
		stop, err := transfer.Serve(ctx, e.backends.Queue, e.id, o.disk, o.cache.Current)
		if err != nil {
			_ = o.disk.Close()
			return fmt.Errorf("engine: serve this node's chunks: %w", err)
		}
		o.stopServe = stop
	}
	client, err := transfer.NewClient(transfer.ClientOptions{
		Queue: e.backends.Queue, Self: e.id, Local: o.local(), Maps: o.cache.Current,
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

// stopObjects withdraws this node's chunk server, ends its loops and releases
// its directory. Nil-safe.
func (e *Engine) stopObjects(ctx context.Context) {
	if e.objects == nil {
		return
	}
	e.stopObjectPasses()
	if e.objects.maintainer != nil {
		e.objects.maintainer.stop()
		e.objects.maintainer = nil
	}
	// THE POINTER STAYS: a request the API is still answering may hold it,
	// and a client over a withdrawn server fails that request honestly
	// where a nil would crash it.
	e.objects.release(ctx)
}

// release undoes whatever of [Engine.startObjects] ran.
func (o *objectStore) release(ctx context.Context) {
	if o.stopServe != nil {
		if err := o.stopServe(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "objects_not_withdrawn", "error", err.Error())
		}
		o.stopServe = nil
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
		o.disk = nil
	}
}

// objectMapDuty is the fleet singleton that maintains the placement map.
const objectMapDuty = "object-map"

// objectMapInterval is how often the map's holder brings it up to date.
//
// THE RECONCILE INTERVAL, the heartbeat every presence lease is renewed on: a
// node that joins is placed on within one of its own heartbeats, and an
// absence is timed against a grace forty ticks long, so a coarser tick buys
// nothing but a slower join.
const objectMapInterval = coord.ReconcileInterval

// objectMapDutyTTL is three ticks, the ratio every singleton here uses: one
// missed tick must not hand the map to a peer mid-change.
const objectMapDutyTTL = 3 * objectMapInterval

// objectMapUnplacedPoll is how often the map duty ticks while this node holds
// no map at all.
//
// ONE SECOND, and only until a map exists. Without one no node can store a
// file — every upload is refused as unavailable — and the first tick of a
// booting fleet routinely finds nothing to place on: it runs as the engine is
// built, before the seat host has claimed this node's own presence lease. At
// the reconcile interval that cost every fresh fleet fifteen seconds with no
// object store (measured in internal/e2e, a seat's first upload failing on
// exactly that). A tick with no map is one presence listing and one key read,
// so a second is cheap, and the first map written ends it.
const objectMapUnplacedPoll = time.Second

// startObjectMap arms the map duty. After the node exists, which the duty's
// lease is claimed through.
func (e *Engine) startObjectMap(ctx context.Context, boot *config.Bootstrap) error {
	if e.objects == nil || e.backends == nil || e.backends.Coord == nil {
		return nil
	}
	m, err := upkeep.NewMaintainer(upkeep.MaintainerOptions{
		Store:    e.backends.Fleet,
		Live:     func(ctx context.Context) ([]upkeep.Presence, error) { return objectHolders(ctx, e.backends.Coord) },
		Replicas: boot.Stream.Replicas,
		Observer: e.objects.cache,
	})
	if err != nil {
		return fmt.Errorf("engine: the object map's maintainer: %w", err)
	}
	claim := e.workerDuty(objectMapDuty, objectMapDutyTTL)
	cache := e.objects.cache
	pace := func() time.Duration {
		if _, placed := cache.Current(); !placed {
			return objectMapUnplacedPoll
		}
		return objectMapInterval
	}
	e.objects.maintainer = startLoop(ctx, pace, func(ctx context.Context) {
		if claim != nil {
			mine, err := claim(ctx)
			if err != nil || !mine {
				return
			}
		}
		if err := m.Tick(ctx); err != nil {
			log.WarnContext(ctx, "object_map_not_maintained", "error", err)
		}
	})
	return nil
}

// objectHolders is every live node offering a share of the object store.
func objectHolders(ctx context.Context, leases liveLeases) ([]upkeep.Presence, error) {
	held, err := leases.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, err
	}
	out := make([]upkeep.Presence, 0, len(held))
	for _, lease := range held {
		if profile, ok := placement.FromLease(lease); ok && profile.ObjectWeight > 0 {
			out = append(out, upkeep.Presence{Node: profile.ID, Weight: profile.ObjectWeight})
		}
	}
	return out, nil
}

// loop is a function run on a detached context until stopped, waiting for a
// run in flight.
type loop struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startLoop runs run at once and then again each time the wait next answers
// has passed — asked after every run, so a loop can pace itself on what the
// run found.
func startLoop(ctx context.Context, next func() time.Duration, run func(context.Context)) *loop {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l := &loop{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		for {
			run(ctx)
			wait := time.NewTimer(next())
			select {
			case <-ctx.Done():
				wait.Stop()
				return
			case <-wait.C:
			}
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

// objectPasses is a data node's repair and collection, each on its own
// interval, and a repair started at once whenever the map's epoch moves.
type objectPasses struct {
	node   *upkeep.Node
	cancel context.CancelFunc
	done   chan struct{}
}

// objectEpochPoll is how often the passes look for a new map epoch.
//
// THE MAP CACHE'S OWN INTERVAL: a new epoch cannot be seen sooner than the
// cache reads it, and a repair started within one refresh of a change is the
// whole of what "at once" can mean here.
const objectEpochPoll = transfer.CacheInterval

// startObjectPasses runs this data node's repair and collection against the
// estate's references. Nil-safe on a node that holds no chunks.
func (e *Engine) startObjectPasses(ctx context.Context, refs upkeep.References) error {
	o := e.objects
	if o == nil || o.disk == nil || o.passes.Load() != nil {
		return nil
	}
	n, err := upkeep.NewNode(upkeep.NodeOptions{
		Self: e.id, Local: o.disk, Peers: o.client, Maps: o.cache.Current, References: refs,
	})
	if err != nil {
		return fmt.Errorf("engine: the object store's passes: %w", err)
	}
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &objectPasses{node: n, cancel: cancel, done: make(chan struct{})}
	o.passes.Store(p)
	go p.run(loopCtx, o.cache)
	return nil
}

// stopObjectPasses ends the passes, waiting for one in flight. Nil-safe.
func (e *Engine) stopObjectPasses() {
	if e.objects == nil {
		return
	}
	if p := e.objects.passes.Swap(nil); p != nil {
		p.cancel()
		<-p.done
	}
}

// run repairs every [upkeep.RepairInterval] and whenever the epoch moves, and
// collects every [upkeep.CollectInterval].
//
// ONE GOROUTINE FOR BOTH, so a collection never runs beside a repair on the
// same disk: a repair copying a group in and a collection judging that group
// in the same instant would each be reasoning about a directory the other is
// changing.
func (p *objectPasses) run(ctx context.Context, cache *transfer.Cache) {
	defer close(p.done)
	poll := time.NewTicker(objectEpochPoll)
	defer poll.Stop()
	var epoch uint64
	var repaired, collected time.Time
	for {
		now := time.Now()
		m, placed := cache.Current()
		switch {
		case !placed:
		case m.Epoch != epoch || now.Sub(repaired) >= upkeep.RepairInterval:
			if err := p.node.Repair(ctx); err != nil && ctx.Err() == nil {
				log.WarnContext(ctx, "object_repair_incomplete", "error", err)
			}
			epoch, repaired = m.Epoch, time.Now()
		case now.Sub(collected) >= upkeep.CollectInterval:
			if err := p.node.Collect(ctx); err != nil && ctx.Err() == nil {
				log.WarnContext(ctx, "object_collect_skipped", "error", err)
			}
			collected = time.Now()
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
	}
}

// objectsMissing is the alarm's input: the chunks this node's last repair
// found on no member. Zero on a node that runs no passes.
func (e *Engine) objectsMissing() int {
	if e.objects == nil {
		return 0
	}
	if p := e.objects.passes.Load(); p != nil {
		return p.node.Status().Missing
	}
	return 0
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
