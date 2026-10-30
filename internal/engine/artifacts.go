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
// running n: the native half's, the artifacts log's over artifacts and the
// assistant log's over assistant, both of which every data node runs. EMPTY
// WHERE THE NATIVE HALF OFFERS NONE: the collector is started with the native
// half ([Engine.startNativeCollector]), and every declared table must have its
// estate ([collect.Sources]), the tracker's included, so a reader missing here
// is a collector [collect.Sources] refuses to build rather than one that reads
// that domain's objects as nobody's.
func objectEstatesFor(n *native, artifacts *artifactsdomain.Reader,
	assistant *assistantdomain.Reader) []collect.Estate {
	estates := n.objectEstates()
	if len(estates) == 0 {
		return estates
	}
	if artifacts != nil {
		estates = append(estates, artifactsdomain.ObjectEstate{Reader: artifacts})
	}
	if assistant != nil {
		estates = append(estates, assistantdomain.ObjectEstate{Reader: assistant})
	}
	return estates
}

// artifactDependents is every domain that keeps state per artifact, which a
// purge removes before the artifact's own record ([artifactsdomain.Dependent]),
// in the order listed: the datastore, the one domain beside the artifacts log
// that keeps state per artifact. Read at each purge, from the core runtime the
// artifacts writer itself belongs to, so a node whose artifacts writer is up
// has its datastore writer up too ([core.openArtifactData]).
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
// SEVERAL LISTENERS AND EACH ITS OWN STOP, unlike the single-slot setters
// beside it: the serving origin's authorization cache and the socket hub both
// hear every batch, are built by different halves of the process, and stop at
// different times. ONE TYPE FOR BOTH LOGS, so the registration, the stop and
// the copy each listener is handed are written once.
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

// tell hands every listener, in the order they registered, its own copy of
// the batch, so one listener that keeps or reorders it cannot change what the
// next one is told.
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

// listening reports whether anything is registered to hear this domain's
// batches: what the live relay's follower asks before it follows a peer
// ([Engine.liveListening]).
func (f *moveFanout[M]) listening() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.listeners) > 0
}

// OnArtifactsMoved registers what this node does when a committed artifacts
// batch moved artifacts (a new version, a narrowing, a trash, a purge), and
// answers the function that unregisters it.
//
// fn IS CALLED ON THE APPLY LOOP'S OWN GOROUTINE, or the live relay
// follower's, with the next batch waiting behind it, so it must not block:
// signal and do the work elsewhere. EVERY DATA NODE APPLIES EVERY RECORD, and
// every node that serves the artifact origin or a socket is a data node, so
// each hears every movement from its own applier while its copy serves, and a
// sound peer's, relayed, while it does not (liverelay.go). A movement of kind
// [artifactsdomain.MoveReset] names no artifact: everything this node decided
// from its artifact rows may have moved, which is also what every switch of
// the relay's source is ([Engine.liveGap]).
func (e *Engine) OnArtifactsMoved(fn func([]artifactsdomain.Moved)) (stop func()) {
	return e.artifactsListeners.add(fn)
}

// artifactsMoved is what the artifacts applier calls after a committed batch:
// announced to this node's listeners and kept for its relay followers, one
// source at a time ([Engine.announce]).
func (e *Engine) artifactsMoved(moved []artifactsdomain.Moved) {
	e.announce(liveBatch{Artifacts: moved})
}

// OnArtifactDataMoved registers what this node does when a committed datastore
// batch moved documents, and answers the function that unregisters it: the
// socket hub, which shapes each delta per reader by the collection's read rule
// and the row's creator the movement carries.
//
// fn IS CALLED ON THE APPLY LOOP'S OWN GOROUTINE, or the live relay
// follower's, and must not block, for [Engine.OnArtifactsMoved]'s reason, and
// it hears the same one source at a time. A movement with
// [artifactdata.Moved.Reset] names no store: everything this node decided from
// datastore rows may have moved.
func (e *Engine) OnArtifactDataMoved(fn func([]artifactdata.Moved)) (stop func()) {
	return e.artifactDataListeners.add(fn)
}

// artifactDataMoved is what the datastore applier calls after a committed
// batch: announced like [Engine.artifactsMoved]. It also samples the company's
// datastore footprint onto this node's recorder, from the rows the batch just
// committed, whichever source its listeners hear: the footprint is this node's
// own rows, and its gauge says so.
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
	// A NIL RECORDER STAYS A NIL INTERFACE: one wrapped in it would be a
	// recorder the writer calls into.
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

// runDataFootprint samples the company's datastore footprint each time a
// committed batch signals, until ctx ends. ONE GOROUTINE, owned by the core's
// run context, so a sample never runs on the apply loop and never two at once.
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

// ArtifactDataEstate is the datastore as every surface reaches it: the tools
// and the API, on every node, through the estate router (ADR-0025), and false
// on an engine built with no router.
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
