package estate

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
)

// THE DATASTORE AS OPERATIONS: every read and write a tool or the API makes of
// an artifact's datastore, answered by a data node whose copy serves (this one
// where it does), so a node holding no data acts through one that does
// (ADR-0025), and a node whose own copy is out of service reads its verdict
// from one that serves.
//
// READS ARE FLOORED ON THE DATASTORE LOG. WRITES carry the caller's operation
// key on their [artifactdata.Call]: keyed, a write derives its record id from
// it and a repeat is the same operation, so an unanswered one is asked of the
// next data node under the same key; unkeyed it is never repeated. The
// authority a write is stamped with crosses whole, decided on the asking node
// where the grants and the chart are.

// ArtifactDataReader is the datastore's read side the surfaces ask of: exactly
// the domain reader's method set, so a consumer cannot tell local from routed.
type ArtifactDataReader interface {
	Docs(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection string,
		since int64, cursor string, limit int) (artifactdata.DocPage, error)
	Doc(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection, doc string) (
		artifactdata.Doc, error)
	Query(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection string,
		q query.Query) (artifactdata.DocPage, error)
	History(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, filter artifactdata.HistoryFilter,
		cursor string, limit int) (artifactdata.HistoryPage, error)
	Schema(ctx context.Context, id artifacts.ID) ([]artifacts.Collection, error)
	Store(ctx context.Context, id artifacts.ID) (artifactdata.StoreInfo, error)
	Usage(ctx context.Context) (artifactdata.Usage, error)
	State(ctx context.Context, id artifacts.ID, rd artifactdata.Reading) ([]artifactdata.StateEntry, error)
}

// ArtifactDataWriter is the datastore's write side: exactly the domain
// writer's method set a surface calls.
type ArtifactDataWriter interface {
	Write(ctx context.Context, c artifactdata.Call, id artifacts.ID, in artifacts.DataWrite) (
		artifactdata.WriteResult, error)
	Undo(ctx context.Context, c artifactdata.Call, id artifacts.ID, seq int64) (artifactdata.WriteResult, error)
	Configure(ctx context.Context, c artifactdata.Call, id artifacts.ID, collections []artifacts.Collection) (
		artifactdata.WriteResult, error)
	PurgeArtifactState(ctx context.Context, c artifactdata.Call, opID string, id artifacts.ID) error
}

// ArtifactDataHalf is the datastore a data node serves: its reader and its
// writer, which every data node runs. A nil half is one this node's core
// runtime has not built, and a nil writer beside a reader a node that
// publishes nothing, as [ArtifactsHalf]'s is.
type ArtifactDataHalf struct {
	Reader ArtifactDataReader
	Writer ArtifactDataWriter
}

func (b Backend) artifactDataReader() (ArtifactDataReader, error) {
	if b.ArtifactData.Reader == nil {
		return nil, errNoHalf
	}
	return b.ArtifactData.Reader, nil
}

func (b Backend) artifactDataWriter() (ArtifactDataWriter, error) {
	if b.ArtifactData.Writer == nil {
		return nil, errNoHalf
	}
	return b.ArtifactData.Writer, nil
}

// keyedDataCall reports a datastore write the caller made REPEATABLE.
func keyedDataCall(c artifactdata.Call) bool { return strings.TrimSpace(c.OpKey) != "" }

// ---- the reads ------------------------------------------------------------ //

type dataDocsArgs struct {
	ID         artifacts.ID
	Reading    artifactdata.Reading
	Collection string
	Since      int64
	Cursor     string
	Limit      int
}

var opDataDocs = define("artifactdata.docs", opRead, artifactDataDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a dataDocsArgs) (artifactdata.DocPage, error) {
		r, err := b.artifactDataReader()
		if err != nil {
			return artifactdata.DocPage{}, err
		}
		return r.Docs(ctx, a.ID, a.Reading, a.Collection, a.Since, a.Cursor, a.Limit)
	})

type dataDocArgs struct {
	ID         artifacts.ID
	Reading    artifactdata.Reading
	Collection string
	Doc        string
}

var opDataDoc = define("artifactdata.doc", opRead, artifactDataDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a dataDocArgs) (artifactdata.Doc, error) {
		r, err := b.artifactDataReader()
		if err != nil {
			return artifactdata.Doc{}, err
		}
		return r.Doc(ctx, a.ID, a.Reading, a.Collection, a.Doc)
	})

type dataQueryArgs struct {
	ID         artifacts.ID
	Reading    artifactdata.Reading
	Collection string
	Query      query.Query
}

var opDataQuery = define("artifactdata.query", opRead, artifactDataDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a dataQueryArgs) (artifactdata.DocPage, error) {
		r, err := b.artifactDataReader()
		if err != nil {
			return artifactdata.DocPage{}, err
		}
		return r.Query(ctx, a.ID, a.Reading, a.Collection, a.Query)
	})

type dataHistoryArgs struct {
	ID      artifacts.ID
	Reading artifactdata.Reading
	Filter  artifactdata.HistoryFilter
	Cursor  string
	Limit   int
}

var opDataHistory = define("artifactdata.history", opRead, artifactDataDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a dataHistoryArgs) (artifactdata.HistoryPage, error) {
		r, err := b.artifactDataReader()
		if err != nil {
			return artifactdata.HistoryPage{}, err
		}
		return r.History(ctx, a.ID, a.Reading, a.Filter, a.Cursor, a.Limit)
	})

type dataStoreArgs struct {
	ID artifacts.ID
}

var opDataSchema = define("artifactdata.schema", opRead, artifactDataDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a dataStoreArgs) ([]artifacts.Collection, error) {
		r, err := b.artifactDataReader()
		if err != nil {
			return nil, err
		}
		return r.Schema(ctx, a.ID)
	})

var opDataStore = define("artifactdata.store", opRead, artifactDataDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a dataStoreArgs) (artifactdata.StoreInfo, error) {
		r, err := b.artifactDataReader()
		if err != nil {
			return artifactdata.StoreInfo{}, err
		}
		return r.Store(ctx, a.ID)
	})

// dataUsageArgs carries nothing: the counters are the company's.
type dataUsageArgs struct{}

var opDataUsage = define("artifactdata.usage", opRead, artifactDataDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, _ dataUsageArgs) (artifactdata.Usage, error) {
		r, err := b.artifactDataReader()
		if err != nil {
			return artifactdata.Usage{}, err
		}
		return r.Usage(ctx)
	})

type dataStateArgs struct {
	ID      artifacts.ID
	Reading artifactdata.Reading
}

var opDataState = define("artifactdata.state", opRead, artifactDataDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a dataStateArgs) ([]artifactdata.StateEntry, error) {
		r, err := b.artifactDataReader()
		if err != nil {
			return nil, err
		}
		return r.State(ctx, a.ID, a.Reading)
	})

// ---- the writes ----------------------------------------------------------- //

type dataWriteArgs struct {
	Call  artifactdata.Call
	ID    artifacts.ID
	Write artifacts.DataWrite
}

var opDataWrite = define("artifactdata.write", opOnceWrite, artifactDataDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a dataWriteArgs) (artifactdata.WriteResult, error) {
		w, err := b.artifactDataWriter()
		if err != nil {
			return artifactdata.WriteResult{}, err
		}
		return w.Write(ctx, a.Call, a.ID, a.Write)
	}).repeatableWhen(func(a dataWriteArgs) bool { return keyedDataCall(a.Call) })

type dataUndoArgs struct {
	Call artifactdata.Call
	ID   artifacts.ID
	Seq  int64
}

var opDataUndo = define("artifactdata.undo", opOnceWrite, artifactDataDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a dataUndoArgs) (artifactdata.WriteResult, error) {
		w, err := b.artifactDataWriter()
		if err != nil {
			return artifactdata.WriteResult{}, err
		}
		return w.Undo(ctx, a.Call, a.ID, a.Seq)
	}).repeatableWhen(func(a dataUndoArgs) bool { return keyedDataCall(a.Call) })

type dataConfigureArgs struct {
	Call        artifactdata.Call
	ID          artifacts.ID
	Collections []artifacts.Collection
}

var opDataConfigure = define("artifactdata.configure", opOnceWrite, artifactDataDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a dataConfigureArgs) (artifactdata.WriteResult, error) {
		w, err := b.artifactDataWriter()
		if err != nil {
			return artifactdata.WriteResult{}, err
		}
		return w.Configure(ctx, a.Call, a.ID, a.Collections)
	}).repeatableWhen(func(a dataConfigureArgs) bool { return keyedDataCall(a.Call) })

type dataPurgeArgs struct {
	Call artifactdata.Call
	OpID string
	ID   artifacts.ID
}

// dataPurged is a purge step's answer: nothing, since a step that landed is
// told by its absence of an error.
type dataPurged struct{}

// opDataPurge IS IDEMPOTENT: the step names its own operation id, so a repeat
// on another data node is the same record, which the ledger collapses.
var opDataPurge = define("artifactdata.purge", opIdempotentWrite, artifactDataDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a dataPurgeArgs) (dataPurged, error) {
		w, err := b.artifactDataWriter()
		if err != nil {
			return dataPurged{}, err
		}
		return dataPurged{}, w.PurgeArtifactState(ctx, a.Call, a.OpID, a.ID)
	})

// ---- the asking half ---------------------------------------------------- //

// ArtifactData is the datastore, answered by a data node whose copy serves.
type ArtifactData struct{ r *Router }

// ArtifactData answers the datastore.
func (r *Router) ArtifactData() ArtifactData { return ArtifactData{r: r} }

// Docs answers a page of a collection.
func (a ArtifactData) Docs(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection string,
	since int64, cursor string, limit int) (artifactdata.DocPage, error) {
	return call(ctx, a.r, opDataDocs, nil, dataDocsArgs{ID: id, Reading: rd, Collection: collection,
		Since: since, Cursor: cursor, Limit: limit})
}

// Doc answers one document.
func (a ArtifactData) Doc(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection,
	doc string) (artifactdata.Doc, error) {
	return call(ctx, a.r, opDataDoc, nil, dataDocArgs{ID: id, Reading: rd, Collection: collection, Doc: doc})
}

// Query answers a page of a query.
func (a ArtifactData) Query(ctx context.Context, id artifacts.ID, rd artifactdata.Reading, collection string,
	q query.Query) (artifactdata.DocPage, error) {
	return call(ctx, a.r, opDataQuery, nil, dataQueryArgs{ID: id, Reading: rd, Collection: collection, Query: q})
}

// History answers a page of a store's history.
func (a ArtifactData) History(ctx context.Context, id artifacts.ID, rd artifactdata.Reading,
	filter artifactdata.HistoryFilter, cursor string, limit int) (artifactdata.HistoryPage, error) {
	return call(ctx, a.r, opDataHistory, nil, dataHistoryArgs{ID: id, Reading: rd, Filter: filter,
		Cursor: cursor, Limit: limit})
}

// Schema answers a store's collections.
func (a ArtifactData) Schema(ctx context.Context, id artifacts.ID) ([]artifacts.Collection, error) {
	out, err := call(ctx, a.r, opDataSchema, nil, dataStoreArgs{ID: id})
	if out == nil && err == nil {
		out = []artifacts.Collection{}
	}
	return out, err
}

// Store answers a store's catalogue row.
func (a ArtifactData) Store(ctx context.Context, id artifacts.ID) (artifactdata.StoreInfo, error) {
	return call(ctx, a.r, opDataStore, nil, dataStoreArgs{ID: id})
}

// Usage answers the company's datastore counters.
func (a ArtifactData) Usage(ctx context.Context) (artifactdata.Usage, error) {
	return call(ctx, a.r, opDataUsage, nil, dataUsageArgs{})
}

// State answers a reader's per-viewer state keys.
func (a ArtifactData) State(ctx context.Context, id artifacts.ID, rd artifactdata.Reading) (
	[]artifactdata.StateEntry, error) {
	out, err := call(ctx, a.r, opDataState, nil, dataStateArgs{ID: id, Reading: rd})
	if out == nil && err == nil {
		out = []artifactdata.StateEntry{}
	}
	return out, err
}

// Write publishes one batch of ops. Never repeated when unanswered unless its
// call carries the caller's operation key. So for every write below.
func (a ArtifactData) Write(ctx context.Context, c artifactdata.Call, id artifacts.ID,
	in artifacts.DataWrite) (artifactdata.WriteResult, error) {
	return a.written(call(ctx, a.r, opDataWrite, nil, dataWriteArgs{Call: c, ID: id, Write: in}))
}

// Undo publishes the inverse of one write.
func (a ArtifactData) Undo(ctx context.Context, c artifactdata.Call, id artifacts.ID, seq int64) (
	artifactdata.WriteResult, error) {
	return a.written(call(ctx, a.r, opDataUndo, nil, dataUndoArgs{Call: c, ID: id, Seq: seq}))
}

// Configure publishes a schema change.
func (a ArtifactData) Configure(ctx context.Context, c artifactdata.Call, id artifacts.ID,
	collections []artifacts.Collection) (artifactdata.WriteResult, error) {
	return a.written(call(ctx, a.r, opDataConfigure, nil,
		dataConfigureArgs{Call: c, ID: id, Collections: collections}))
}

// PurgeArtifactState removes an artifact's datastore, as its purge's first step.
func (a ArtifactData) PurgeArtifactState(ctx context.Context, c artifactdata.Call, opID string,
	id artifacts.ID) error {
	_, err := call(ctx, a.r, opDataPurge, nil, dataPurgeArgs{Call: c, OpID: opID, ID: id})
	return err
}

// written observes a write's position for the next read's floor and answers it.
func (a ArtifactData) written(out artifactdata.WriteResult, err error) (artifactdata.WriteResult, error) {
	a.r.Observe(out.Result.Position)
	return out, err
}
