package api

import (
	"context"
	"io"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/transfer"
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
func EngineFiles(e *engine.Engine) ProjectFiles {
	if e == nil {
		return nil
	}
	reader, writer, objects := e.Tracker(), e.TrackerWriter(), e.Objects()
	if reader == nil || writer == nil || objects == nil {
		return nil
	}
	return engineFiles{reader: reader, writer: writer, objects: objects}
}

// engineFiles binds the operator on each write, for the purge's reason: the
// record names the person, and `As` is where that identity is bound.
type engineFiles struct {
	reader  *tracker.Reader
	writer  *tracker.Writer
	objects *transfer.Client
}

func (f engineFiles) File(ctx context.Context, project, path string,
	fresh statelog.Freshness) (tracker.FileDetail, error) {
	return f.reader.File(ctx, project, path, fresh)
}

func (f engineFiles) Open(ctx context.Context, m objstore.Manifest) (io.ReadCloser, error) {
	return f.objects.Open(ctx, m)
}

func (f engineFiles) PutChunk(ctx context.Context, h objstore.Hash, data []byte) (int, error) {
	return f.objects.Put(ctx, h, data)
}

func (f engineFiles) as(operator string) *tracker.Writer {
	return f.writer.As(operator, tracker.AuthorOperator, tracker.Provenance{OperatorID: operator})
}

func (f engineFiles) PutFileAs(ctx context.Context, operator, opID string,
	put tracker.FilePut) (tracker.WriteResult, error) {
	return f.as(operator).PutFile(ctx, opID, put)
}

func (f engineFiles) RemoveFileAs(ctx context.Context, operator, opID, project, path string,
	ifMatch uint64) (tracker.WriteResult, error) {
	return f.as(operator).RemoveFile(ctx, opID, project, path, ifMatch)
}
