package api

import (
	"context"
	"io"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EngineNative is [NativeRoutes] over a running engine — a project's file
// bytes, the purge and the eviction gate, each resolved on EVERY CALL — or nil
// for a nil engine, which [New] refuses by name.
//
// HERE, beside the seam it satisfies, for [engineRuntime]'s reason: `crewlet
// run` and the end-to-end suite serve these routes through the same adapter,
// so a case that uploads on one node and downloads from another is exercising
// the wiring a deployment runs.
//
// PER CALL, never once, for [NativeRoutes]' reason: the native runtime is
// built at boot or at the first apply whose company runs a state log, which on
// a node that booted with no company is after the API was assembled. Each call
// costs what the engine's accessors cost — atomic loads, no lock and no I/O.
func EngineNative(e *engine.Engine) NativeRoutes {
	if e == nil {
		return nil
	}
	return engineNative{e: e}
}

// engineNative is [EngineNative]'s adapter.
type engineNative struct{ e *engine.Engine }

// FileReads is a download's reach where this node runs the native tracker and
// holds an object store — both, or false. NOT GATED ON THE WRITER: a download
// is a read, served in every mode the listing beside it is.
//
// THE ROWS THROUGH THIS NODE'S ESTATE ROUTER, as a seat's file tools reach
// them ([engine.OperatorWork]): a data node whose copy of the tracker — the
// domain a file's row is — is out of service is answered by a holder whose
// copy of it serves (ADR-0031), rather than refusing a download its peers
// could have named.
func (n engineNative) FileReads() (FileReads, bool) {
	files, ok := n.files()
	return files, ok
}

// FileWrites is an upload's and a removal's reach where this node also hands
// out the tracker's writer — never in a maintenance mode.
//
// The reader and the writer are two readings of the runtime, which is safe
// for one reason: the runtime is published once and never withdrawn
// ([engine.Engine]'s `native`), so two readings that disagree can only be a
// first that found none and a later that found it — and that answers false,
// the route absent for the one request that raced the first apply.
func (n engineNative) FileWrites() (FileWrites, bool) {
	files, readable := n.files()
	as, writable := engine.OperatorWorkWriter(n.e)
	if !readable || !writable {
		return nil, false
	}
	return engineFileWrites{engineFiles: files, as: as}, true
}

// files is the read half both answers are built on: the router's reads and the
// object store, both or false.
func (n engineNative) files() (engineFiles, bool) {
	reader, readable := engine.OperatorWork(n.e)
	objects := n.e.Objects()
	if !readable || objects == nil {
		return engineFiles{}, false
	}
	return engineFiles{reader: reader, objects: objects}, true
}

// Purger is the purge's writer, acting as the operator on each request, or
// false where this node hands out no tracker writer.
func (n engineNative) Purger() (TaskPurger, bool) {
	as, ok := engine.OperatorWorkWriter(n.e)
	if !ok {
		return nil, false
	}
	return purgeAdapter{as: as}, true
}

// Nodes is the eviction gate over every identity-claiming log, or false where
// this node runs no state log.
//
// NOT THE TRACKER'S WRITER, which is what this once was: that wrote the
// tracker's log alone, so an eviction lifted one log's pin and left the pages
// log counting the node for ever — and a company whose tracker is external,
// which still runs both logs, could not evict anybody at all.
//
// The nil check converts: a nil *engine.NodeGate is answered false rather than
// handed over inside the interface, where it would pass the route's check and
// panic on the first press.
func (n engineNative) Nodes() (NodeGate, bool) {
	gate := n.e.NodeGate()
	if gate == nil {
		return nil, false
	}
	return gate, true
}

// purgeAdapter is the purge's seam over the router's writer.
//
// AN ADAPTER RATHER THAN THE WRITER ITSELF, because the API's seam takes the
// operator as an argument: every other write on that surface is the fleet's
// and the process's own writer is the right author, while a purge destroys a
// company's data and the record has to carry the person who asked for it.
// `As` is where that identity is bound, and it is a tracker concept this
// package deliberately does not import a concrete type for.
type purgeAdapter struct {
	as func(estate.Actor) estate.WorkWriter
}

func (p purgeAdapter) PurgeAs(ctx context.Context, operator, opID, id, project,
	reason string) (tracker.WriteResult, error) {

	return p.as(operatorActor(operator)).PurgeTask(ctx, opID, id, project, reason)
}

// operatorActor is the party a native route's write is attributed to: the
// operator on the request, by the token's own name — the one spelling the
// purge and the file writes share, so the two cannot record one person two
// ways.
func operatorActor(operator string) estate.Actor {
	return estate.Actor{Handle: operator, Kind: tracker.AuthorOperator,
		Provenance: tracker.Provenance{OperatorID: operator}}
}

// engineFiles is [FileReads] over the router's reads and this node's object
// store.
type engineFiles struct {
	reader  estate.Work
	objects *objstore.Store
}

func (f engineFiles) File(ctx context.Context, project, path string,
	fresh statelog.Freshness) (tracker.FileDetail, error) {
	return f.reader.File(ctx, project, path, fresh)
}

func (f engineFiles) Open(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
	return f.objects.Open(ctx, o)
}

// engineFileWrites is [FileWrites]: the read half, and the router's writer,
// bound to the operator on each write for the purge's reason — the record
// names the person, and the actor is where that identity is bound.
type engineFileWrites struct {
	engineFiles
	as func(estate.Actor) estate.WorkWriter
}

// AcceptsFiles reads the project through the router, as the write's own
// decide will: a project the read cannot find is [tracker.ErrNoProject], and an
// archived one is refused in the writer's own sentence.
func (f engineFileWrites) AcceptsFiles(ctx context.Context, project string, fresh statelog.Freshness) error {
	detail, err := f.reader.Project(ctx, tracker.ProjectDetailQuery{
		Project: project, Level: fresh.Level,
		MinPosition: fresh.MinPosition, MaxLag: fresh.MaxLag, MaxLagSeq: fresh.MaxLagSeq,
	})
	if err != nil {
		return err
	}
	return tracker.AcceptFiles(detail.Key, detail.Archived)
}

func (f engineFileWrites) Put(ctx context.Context, r io.Reader, limit int64,
	m objstore.PutMeta) (objstore.Object, error) {
	return f.objects.Put(ctx, r, limit, m)
}

func (f engineFileWrites) operator(operator string) estate.WorkWriter {
	return f.as(operatorActor(operator))
}

func (f engineFileWrites) PutFileAs(ctx context.Context, operator, opID string,
	put tracker.FilePut) (tracker.WriteResult, error) {
	return f.operator(operator).PutFile(ctx, opID, put)
}

func (f engineFileWrites) RemoveFileAs(ctx context.Context, operator, opID, project, path string,
	ifMatch uint64) (tracker.WriteResult, error) {
	return f.operator(operator).RemoveFile(ctx, opID, project, path, ifMatch)
}
