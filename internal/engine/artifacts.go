package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/org"
)

// THE ARTIFACTS DOMAIN'S ENGINE WIRING: its two sides over the running log, the
// fan-out of what each committed batch moved, the purge's dependents, and the
// trash reaper's and the object store's views of it.
//
// EVERY DATA NODE RUNS IT, with or without a company: the register runs every
// domain or none ([register]), and artifacts are no native half a company opts
// into. A node without `data` reaches them through a data node (internal/estate).

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

// ArtifactsEstate is the artifacts estate as every surface (tools, serving
// origin, API) reaches it on every node, through the estate router (ADR-0025),
// and false on an engine built with no router.
//
// THE ROUTER'S ON EVERY NODE, for [OperatorWork]'s reason: a data node whose
// copy is out of service, or a node without `data`, answers from a peer's,
// and a surface cannot tell which node answered.
func ArtifactsEstate(e *Engine) (estate.Artifacts, bool) {
	if e.router == nil {
		return estate.Artifacts{}, false
	}
	return e.router.Artifacts(), true
}

// ArtifactsReader is this node's artifacts read side, or nil on an engine with
// no core runtime (a node without `data`: see [ArtifactsEstate]).
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
// running n: the native half's and the artifacts, assistant and executor logs',
// which every data node runs. The collector starts with the native half
// ([Engine.startNativeCollector]), and every declared table must have its
// estate ([collect.Sources]), so a reader missing here makes the collector
// refuse to build rather than read that domain's objects as nobody's.
func objectEstatesFor(n *native, artifacts *artifactsdomain.Reader,
	assistant *assistantdomain.Reader, executor *executordomain.Reader) []collect.Estate {
	estates := n.objectEstates()
	if artifacts != nil {
		estates = append(estates, artifactsdomain.ObjectEstate{Reader: artifacts})
	}
	if assistant != nil {
		estates = append(estates, assistantdomain.ObjectEstate{Reader: assistant})
	}
	if executor != nil {
		estates = append(estates, executordomain.ObjectEstate{Reader: executor})
	}
	return estates
}

// artifactDependents is every domain keeping state per artifact (today the
// datastore), which a purge removes first, in order ([artifactsdomain.Dependent]).
// Read at each purge from the core runtime the artifacts writer belongs to, so
// its datastore writer is up too ([core.openArtifactData]).
func (e *Engine) artifactDependents() []artifactsdomain.Dependent {
	c := e.core.Load()
	if c == nil || c.artifactDataWriter == nil {
		return nil
	}
	return []artifactsdomain.Dependent{c.artifactDataWriter}
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

// moveFanout is every listener one domain's [Engine.OnArtifactsMoved] or
// [Engine.OnArtifactDataMoved] registered.
//
// SEVERAL LISTENERS, EACH ITS OWN STOP, unlike single-slot setters: the serving
// origin's authorization cache and the socket hub are built by different halves
// of the process and stop at different times. One type serves both logs.
type moveFanout[M any] struct {
	mu        sync.Mutex
	next      uint64
	listeners map[uint64]func([]M)
}

// add registers fn and answers the function that unregisters it.
func (f *moveFanout[M]) add(fn func([]M)) (stop func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listeners == nil {
		f.listeners = map[uint64]func([]M){}
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

// tell hands every listener, in registration order, its own copy of the batch,
// so a listener that keeps or reorders it cannot change what the next is told.
func (f *moveFanout[M]) tell(moved []M) {
	if len(moved) == 0 {
		return
	}
	f.mu.Lock()
	ids := make([]uint64, 0, len(f.listeners))
	for id := range f.listeners {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	fns := make([]func([]M), 0, len(ids))
	for _, id := range ids {
		fns = append(fns, f.listeners[id])
	}
	f.mu.Unlock()
	for _, fn := range fns {
		fn(slices.Clone(moved))
	}
}

// listening reports whether anything hears this domain's batches, asked by the
// live relay's follower before it follows a peer ([Engine.liveListening]).
func (f *moveFanout[M]) listening() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.listeners) > 0
}

// OnArtifactsMoved registers what this node does when a committed artifacts
// batch moved artifacts (a new version, a narrowing, a trash, a purge), and
// answers the function that unregisters it.
//
// fn RUNS ON THE APPLY LOOP'S GOROUTINE, or the live relay follower's, with the
// next batch waiting, so it must not block. Every serving node is a data node,
// hearing its own applier while its copy serves and a sound peer's, relayed,
// while not (liverelay.go). A [artifactsdomain.MoveReset] names no artifact:
// anything decided from artifact rows may have moved, as at every switch of the
// relay's source ([Engine.liveGap]).
func (e *Engine) OnArtifactsMoved(fn func([]artifactsdomain.Moved)) (stop func()) {
	return e.artifactsListeners.add(fn)
}

// artifactsMoved is what the artifacts applier calls after a committed batch,
// announced to listeners and relay followers ([Engine.announce]).
func (e *Engine) artifactsMoved(moved []artifactsdomain.Moved) {
	e.announce(liveBatch{Artifacts: moved})
}

// OnArtifactDataMoved registers what this node does when a committed datastore
// batch moved documents (the socket hub, shaping each delta per reader from the
// read rule and the row's creator the movement carries), and answers the
// function that unregisters it.
//
// fn must not block, as for [Engine.OnArtifactsMoved], and hears one source at
// a time. A movement with [artifactdata.Moved.Reset] names no store: anything
// decided from datastore rows may have moved.
func (e *Engine) OnArtifactDataMoved(fn func([]artifactdata.Moved)) (stop func()) {
	return e.artifactDataListeners.add(fn)
}

// artifactDataMoved is what the datastore applier calls after a committed
// batch, announced like [Engine.artifactsMoved]. It also samples the footprint
// from this node's own rows, whichever source its listeners hear; its gauge
// says so.
func (e *Engine) artifactDataMoved(moved []artifactdata.Moved) {
	e.announce(liveBatch{ArtifactData: moved})
	e.sampleDataFootprint()
}

// sampleDataFootprint signals the footprint sampler, which reads the rows off
// the apply loop's goroutine; a signal already pending covers this batch too.
func (e *Engine) sampleDataFootprint() {
	select {
	case e.dataFootprint <- struct{}{}:
	default:
	}
}

// openArtifactData builds the datastore's reader and writer over its running
// domain. A node whose datastore did not come up is a boot failure naming it.
func (c *core) openArtifactData(e *Engine, sl *stateLog) error {
	running := sl.Domain(artifactdata.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("engine: this node runs no artifact datastore domain: the domain " +
			"is in the register and its stream failed to come up")
	}
	reader, err := artifactdata.NewReader(artifactdata.ReaderOptions{
		DB: sl.estate().Reader(), Log: running.reader,
	})
	if err != nil {
		return fmt.Errorf("engine: artifact datastore reader: %w", err)
	}
	deps := artifactdata.WriterDeps{
		Publisher: running.publisher, DB: sl.estate().Reader(),
		SeatExists: e.artifactSeatExists,
	}
	// A NIL RECORDER STAYS A NIL INTERFACE, never a typed nil the writer calls.
	if e.metrics != nil {
		deps.Metrics = e.metrics
	}
	deps.Objectives = e.objectives
	writer, err := artifactdata.NewWriter(deps)
	if err != nil {
		return fmt.Errorf("engine: artifact datastore writer: %w", err)
	}
	c.artifactDataReader, c.artifactDataWriter = reader, writer
	return nil
}

// runDataFootprint samples the datastore footprint each time a committed batch
// signals, until ctx ends: ONE GOROUTINE, owned by the core's run context, so
// no sample runs on the apply loop and never two at once.
func (e *Engine) runDataFootprint(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.dataFootprint:
		}
		w := e.ArtifactDataWriter()
		if w == nil {
			continue
		}
		if err := w.FootprintGauge(ctx); err != nil && ctx.Err() == nil {
			log.Warn("artifact_data_footprint_unread", "error", err)
		}
	}
}

// ArtifactDataEstate is the datastore as the tools and the API reach it on
// every node, through the estate router (ADR-0025), and false on an engine
// built with no router.
func ArtifactDataEstate(e *Engine) (estate.ArtifactData, bool) {
	if e.router == nil {
		return estate.ArtifactData{}, false
	}
	return e.router.ArtifactData(), true
}

// ArtifactDataReader is this node's datastore read side, or nil on an engine
// with no core runtime.
func (e *Engine) ArtifactDataReader() *artifactdata.Reader {
	if c := e.core.Load(); c != nil {
		return c.artifactDataReader
	}
	return nil
}

// ArtifactDataWriter is this node's datastore write side, or nil on an engine
// with no core runtime.
func (e *Engine) ArtifactDataWriter() *artifactdata.Writer {
	if c := e.core.Load(); c != nil {
		return c.artifactDataWriter
	}
	return nil
}

// errGateWritesNoDocument is the node gate's datastore writer asked to resolve
// a person value, which it never writes.
var errGateWritesNoDocument = errors.New("engine: the node gate's datastore writer writes no document")

// artifactSeatExists reports whether a seat handle is a seat of the running
// organisation: what a datastore person value is held to.
func (e *Engine) artifactSeatExists(ctx context.Context, handle string) (bool, error) {
	_, found, err := SeatViewOf(e).Seat(ctx, handle)
	return found, err
}
