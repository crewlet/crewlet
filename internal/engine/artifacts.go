package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/org"
)

// THE ARTIFACTS DOMAIN'S ENGINE WIRING: its two sides over the running log, the
// fan-out of what each committed batch moved, the purge's dependents, and the
// trash reaper's and the object store's views of it.
//
// EVERY DATA NODE RUNS IT, whatever its company keeps and whether or not it has
// one: the register runs every domain or none ([register]), and an artifact is
// not a native half a company opts into. A node without `data` holds no estate
// and reaches artifacts through a data node (internal/estate).

// openArtifacts builds the artifacts estate's reader and writer over its
// running domain. A node whose artifacts domain did not come up is a boot
// failure naming the domain rather than a nil every surface branches on.
func (c *core) openArtifacts(e *Engine, sl *stateLog) error {
	running := sl.Domain(artifactsdomain.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("engine: this node runs no artifacts domain: the domain " +
			"is in the register and its stream failed to come up")
	}
	reader, err := artifactsdomain.NewReader(artifactsdomain.ReaderOptions{
		DB: sl.estate().Reader(), Log: running.reader,
	})
	if err != nil {
		return fmt.Errorf("engine: artifacts reader: %w", err)
	}
	writer, err := artifactsdomain.NewWriter(artifactsdomain.WriterDeps{
		Publisher: running.publisher, DB: sl.estate().Reader(),
		Dependents: e.artifactDependents, SeatKind: e.artifactSeatKind,
	})
	if err != nil {
		return fmt.Errorf("engine: artifacts writer: %w", err)
	}
	c.artifactsReader, c.artifactsWriter = reader, writer
	return nil
}

// ArtifactsEstate is the artifacts estate as every surface reaches it: the
// tools, the serving origin and the API, on every node, through the estate
// router (ADR-0025), and false on an engine built with no router.
//
// THE ROUTER'S ON EVERY NODE, for [OperatorWork]'s reason: a data node whose
// copy is out of service answers from a peer's copy, and a node without `data`
// reaches a data node's. Its method set is the domain reader's and writer's,
// so a surface cannot tell which node answered.
func ArtifactsEstate(e *Engine) (estate.Artifacts, bool) {
	if e.router == nil {
		return estate.Artifacts{}, false
	}
	return e.router.Artifacts(), true
}

// ArtifactsReader is this node's artifacts read side, or nil on an engine with
// no core runtime: a node without `data`, which reads artifacts through a data
// node ([ArtifactsEstate]).
func (e *Engine) ArtifactsReader() *artifactsdomain.Reader {
	if c := e.core.Load(); c != nil {
		return c.artifactsReader
	}
	return nil
}

// ArtifactsWriter is this node's artifacts write side, or nil on an engine with
// no core runtime.
func (e *Engine) ArtifactsWriter() *artifactsdomain.Writer {
	if c := e.core.Load(); c != nil {
		return c.artifactsWriter
	}
	return nil
}

// objectEstatesFor is every estate the object store's passes read on a node
// running n: the native half's, and the artifacts log's over artifacts, which
// every data node runs. EMPTY WHERE THE NATIVE HALF OFFERS NONE: the collector
// is started with the native half ([Engine.startNativeCollector]), and every
// declared table must have its estate ([collect.Sources]), the tracker's
// included.
func objectEstatesFor(n *native, artifacts *artifactsdomain.Reader) []collect.Estate {
	estates := n.objectEstates()
	if len(estates) == 0 || artifacts == nil {
		return estates
	}
	return append(estates, artifactsdomain.ObjectEstate{Reader: artifacts})
}

// artifactDependents is every domain that keeps state per artifact, which a
// purge removes before the artifact's own record ([artifactsdomain.Dependent]),
// in the order listed. EMPTY until such a domain is wired here: the artifacts
// log itself holds everything else an artifact has.
func (e *Engine) artifactDependents() []artifactsdomain.Dependent {
	return nil
}

// errGateTransfers is the node gate's writer asked to resolve a transfer's new
// owner, which it never records.
var errGateTransfers = errors.New("engine: the node gate's writer records evictions and transfers no artifact")

// artifactSeatKind is the actor kind of the seat a transfer moves an artifact
// to, from the running organisation: a person's seat or an agent's.
func (e *Engine) artifactSeatKind(ctx context.Context, handle string) (iam.ActorKind, error) {
	seat, found, err := SeatViewOf(e).Seat(ctx, handle)
	switch {
	case err != nil:
		return "", err
	case !found:
		return "", fmt.Errorf("%w: the organisation holds no seat %q", artifactsdomain.ErrInvalid, handle)
	case org.RoleKind(seat.Kind) == org.KindHuman:
		return iam.ActorHuman, nil
	}
	return iam.ActorAgent, nil
}

// artifactsFanout is every listener [Engine.OnArtifactsMoved] registered.
//
// SEVERAL LISTENERS AND EACH ITS OWN STOP, unlike the single-slot setters
// beside it: the serving origin's authorization cache and the socket hub both
// hear every batch, are built by different halves of the process, and stop at
// different times.
type artifactsFanout struct {
	mu        sync.Mutex
	next      uint64
	listeners map[uint64]func([]artifactsdomain.Moved)
}

// OnArtifactsMoved registers what this node does when a committed artifacts
// batch moved artifacts (a new version, a narrowing, a trash, a purge), and
// answers the function that unregisters it.
//
// fn IS CALLED ON THE APPLY LOOP'S OWN GOROUTINE, with the next batch waiting
// behind it, so it must not block: signal and do the work elsewhere. EVERY DATA
// NODE APPLIES EVERY RECORD, and every node that serves the artifact origin or
// a socket is a data node, so each hears every movement from its own applier
// and nothing is forwarded between nodes. A movement of kind
// [artifactsdomain.MoveReset] names no artifact: everything this node decided
// from its artifact rows may have moved.
func (e *Engine) OnArtifactsMoved(fn func([]artifactsdomain.Moved)) (stop func()) {
	f := &e.artifactsListeners
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listeners == nil {
		f.listeners = map[uint64]func([]artifactsdomain.Moved){}
	}
	id := f.next
	f.next++
	f.listeners[id] = fn
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			delete(f.listeners, id)
		})
	}
}

// artifactsMoved is what the artifacts applier calls after a committed batch:
// every listener, in the order they registered, each handed the batch.
func (e *Engine) artifactsMoved(moved []artifactsdomain.Moved) {
	if len(moved) == 0 {
		return
	}
	f := &e.artifactsListeners
	f.mu.Lock()
	ids := make([]uint64, 0, len(f.listeners))
	for id := range f.listeners {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	fns := make([]func([]artifactsdomain.Moved), 0, len(ids))
	for _, id := range ids {
		fns = append(fns, f.listeners[id])
	}
	f.mu.Unlock()
	// A COPY EACH, so one listener that keeps or reorders the batch cannot
	// change what the next one is told.
	for _, fn := range fns {
		fn(slices.Clone(moved))
	}
}
