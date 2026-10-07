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

// EngineFiles is the file byte routes' reach into a running engine, or nil
// where it has no native tracker or no object store.
//
// HERE, beside the seam it satisfies, for [engineRuntime]'s reason: `crewlet
// run` and the end-to-end suite serve files through the same adapter, so a
// case that uploads on one node and downloads from another is exercising the
// wiring a deployment runs. NIL AS AN INTERFACE rather than an interface
// holding a nil adapter, which [App] would read as a surface and mount.
//
// THE ROWS THROUGH THIS NODE'S ESTATE ROUTER, as a seat's file tools reach
// them ([engine.OperatorWork]): a data node whose copy is out of service is
// answered by a holder whose copy serves, rather than refusing a download its
// peers could have named.
func EngineFiles(e *engine.Engine) ProjectFiles {
	if e == nil {
		return nil
	}
	reader, readable := engine.OperatorWork(e)
	as, writable := engine.OperatorWorkWriter(e)
	objects := e.Objects()
	if !readable || !writable || objects == nil {
		return nil
	}
	return engineFiles{reader: reader, as: as, objects: objects}
}

// engineFiles binds the operator on each write, for the purge's reason: the
// record names the person, and the actor is where that identity is bound.
type engineFiles struct {
	reader  estate.Work
	as      func(estate.Actor) estate.WorkWriter
	objects *objstore.Store
}

func (f engineFiles) File(ctx context.Context, project, path string,
	fresh statelog.Freshness) (tracker.FileDetail, error) {
	return f.reader.File(ctx, project, path, fresh)
}

// AcceptsFiles reads the project through the router, as the write's own
// decide will: a project the read cannot find is [tracker.ErrNoProject], and an
// archived one is refused in the writer's own sentence.
func (f engineFiles) AcceptsFiles(ctx context.Context, project string, fresh statelog.Freshness) error {
	detail, err := f.reader.Project(ctx, tracker.ProjectDetailQuery{
		Project: project, Level: fresh.Level,
		MinPosition: fresh.MinPosition, MaxLag: fresh.MaxLag, MaxLagSeq: fresh.MaxLagSeq,
	})
	if err != nil {
		return err
	}
	return tracker.AcceptFiles(detail.Key, detail.Archived)
}

func (f engineFiles) Open(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
	return f.objects.Open(ctx, o)
}

func (f engineFiles) Put(ctx context.Context, r io.Reader, limit int64,
	m objstore.PutMeta) (objstore.Object, error) {
	return f.objects.Put(ctx, r, limit, m)
}

func (f engineFiles) operator(operator string) estate.WorkWriter {
	return f.as(estate.Actor{Handle: operator, Kind: tracker.AuthorOperator,
		Provenance: tracker.Provenance{OperatorID: operator}})
}

func (f engineFiles) PutFileAs(ctx context.Context, operator, opID string,
	put tracker.FilePut) (tracker.WriteResult, error) {
	return f.operator(operator).PutFile(ctx, opID, put)
}

func (f engineFiles) RemoveFileAs(ctx context.Context, operator, opID, project, path string,
	ifMatch uint64) (tracker.WriteResult, error) {
	return f.operator(operator).RemoveFile(ctx, opID, project, path, ifMatch)
}
