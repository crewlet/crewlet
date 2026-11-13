package estate

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ATLAS LOG AS OPERATIONS: what people record about what the company runs
// on (ADR-0126), read and written from whichever node asks. A seat's turn reads
// the atlas where its seat is held, a seats-only leaf included, and every read
// is answered at the level its caller passes, on this node's floor on the log.
//
// A PERSON'S WRITES AND AN AGENT'S ARE ROUTED TOO, on the terms of every surface write: the
// operator's catalogue, which the dashboard and the assistant write through,
// is served wherever the API is, and a node holding no data reaches a data
// node's writer through the router as it reaches its reader. Each write carries
// the operation id its caller derived, so one no data node answered is asked of
// the next under the same id. A value's purge crosses as its digest, never as
// the value it erases ([atlasdomain.Writer.PurgeDigest]).

// AtlasReader is the atlas log's read side: the domain reader's methods a node
// off the estate needs.
type AtlasReader interface {
	Live(ctx context.Context, fresh statelog.Freshness) (atlasdomain.Live, error)
	Entry(ctx context.Context, id string, fresh statelog.Freshness) (atlasdomain.EntryRow, bool, error)
	Log(ctx context.Context, q atlasdomain.LogQuery) (atlasdomain.LogPage, error)
	Activity(ctx context.Context, since time.Time, fresh statelog.Freshness) (atlasdomain.Activity, error)
	Purges(ctx context.Context, fresh statelog.Freshness) ([]atlasdomain.PurgeRow, error)
}

// AtlasWriter is the atlas log's write side as a surface writes it: a person's
// claim, an agent's with the evidence of its turn, and an administrator's
// purge of an entry or of a value by its digest.
type AtlasWriter interface {
	Record(ctx context.Context, c atlasdomain.Call, claim atlas.Claim, ifMatch uint64) (statelog.Result, error)
	Observe(ctx context.Context, c atlasdomain.Call, claim atlas.Claim,
		seen atlasdomain.Observation) (statelog.Result, error)
	PurgeEntry(ctx context.Context, c atlasdomain.Call, ref atlas.Ref) (statelog.Result, error)
	PurgeDigest(ctx context.Context, c atlasdomain.Call, digest string) (statelog.Result, error)
}

// The domain's own sides are what a data node serves the log with.
var (
	_ AtlasReader = (*atlasdomain.Reader)(nil)
	_ AtlasWriter = (*atlasdomain.Writer)(nil)
)

// AtlasHalf is the atlas log a data node serves; a nil writer beside a reader
// is a node that publishes nothing, whose writes move on to one that does.
type AtlasHalf struct {
	Reader AtlasReader
	Writer AtlasWriter
}

// atlasReader is the half's reader, or [errNoHalf].
func (b Backend) atlasReader() (AtlasReader, error) {
	if b.Atlas.Reader == nil {
		return nil, errNoHalf
	}
	return b.Atlas.Reader, nil
}

// atlasWriter is the half's writer, or [errNoHalf].
func (b Backend) atlasWriter() (AtlasWriter, error) {
	if b.Atlas.Writer == nil {
		return nil, errNoHalf
	}
	return b.Atlas.Writer, nil
}

// ---- the reads ------------------------------------------------------------ //

type atlasLiveArgs struct {
	Fresh statelog.Freshness
}

// EVERY LIVE CLAIM, in one transaction on the node that answers: what the atlas
// folds beside the claims it computes when it is read.
var opAtlasLive = define("atlas.live", opRead, atlasDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a atlasLiveArgs) (atlasdomain.Live, error) {
		r, err := b.atlasReader()
		if err != nil {
			return atlasdomain.Live{}, err
		}
		return r.Live(ctx, a.Fresh)
	})

type atlasEntryArgs struct {
	ID    string
	Fresh statelog.Freshness
}

// atlasEntryAnswer is one entry and whether the log holds a claim on it.
type atlasEntryAnswer struct {
	Entry atlasdomain.EntryRow
	Found bool
}

var opAtlasEntry = define("atlas.entry", opRead, atlasDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a atlasEntryArgs) (atlasEntryAnswer, error) {
		r, err := b.atlasReader()
		if err != nil {
			return atlasEntryAnswer{}, err
		}
		row, found, err := r.Entry(ctx, a.ID, a.Fresh)
		return atlasEntryAnswer{Entry: row, Found: found}, err
	})

// ONE PAGE OF THE LOG, newest first: the whole company's, or named entries'.
var opAtlasLog = define("atlas.log", opRead, atlasDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q atlasdomain.LogQuery) (atlasdomain.LogPage, error) {
		r, err := b.atlasReader()
		if err != nil {
			return atlasdomain.LogPage{}, err
		}
		return r.Log(ctx, q)
	})

type atlasActivityArgs struct {
	Since time.Time
	Fresh statelog.Freshness
}

// THE LOG COUNTED, a day and an entry at a time since an instant, with each
// entry's newest record: one transaction on the node that answers.
var opAtlasActivity = define("atlas.activity", opRead, atlasDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a atlasActivityArgs) (atlasdomain.Activity, error) {
		r, err := b.atlasReader()
		if err != nil {
			return atlasdomain.Activity{}, err
		}
		return r.Activity(ctx, a.Since, a.Fresh)
	})

type atlasPurgesArgs struct {
	Fresh statelog.Freshness
}

var opAtlasPurges = define("atlas.purges", opRead, atlasDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a atlasPurgesArgs) ([]atlasdomain.PurgeRow, error) {
		r, err := b.atlasReader()
		if err != nil {
			return nil, err
		}
		return r.Purges(ctx, a.Fresh)
	})

// ---- the writes: a surface's, each under the operation it carries ------- //

type atlasRecordArgs struct {
	Call    atlasdomain.Call
	Claim   atlas.Claim
	IfMatch uint64
}

// A PERSON'S CLAIM, judged again by the data node's writer: the asking node's
// judgement is the caller's, and the writer's is the one the log keeps.
var opAtlasRecord = define("atlas.record", opIdempotentWrite, atlasDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a atlasRecordArgs) (statelog.Result, error) {
		w, err := b.atlasWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.Record(ctx, a.Call, a.Claim, a.IfMatch)
	})

type atlasObserveArgs struct {
	Call  atlasdomain.Call
	Claim atlas.Claim
	Seen  atlasdomain.Observation
}

// AN AGENT'S CLAIM, from whichever node holds its seat (ADR-0132): the node
// that ran the turn checked the cited call's answer, which only it holds, and
// the data node's writer holds the claim to everything a record can be held to.
var opAtlasObserve = define("atlas.observe", opIdempotentWrite, atlasDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a atlasObserveArgs) (statelog.Result, error) {
		w, err := b.atlasWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.Observe(ctx, a.Call, a.Claim, a.Seen)
	})

type atlasPurgeEntryArgs struct {
	Call  atlasdomain.Call
	Entry atlas.Ref
}

var opAtlasPurgeEntry = define("atlas.purge_entry", opIdempotentWrite, atlasDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a atlasPurgeEntryArgs) (statelog.Result, error) {
		w, err := b.atlasWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.PurgeEntry(ctx, a.Call, a.Entry)
	})

type atlasPurgeValueArgs struct {
	Call   atlasdomain.Call
	Digest string
}

// A VALUE'S PURGE CROSSES AS ITS DIGEST: the value it erases is what somebody
// must not keep, so no request between nodes carries it.
var opAtlasPurgeValue = define("atlas.purge_value", opIdempotentWrite, atlasDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a atlasPurgeValueArgs) (statelog.Result, error) {
		w, err := b.atlasWriter()
		if err != nil {
			return statelog.Result{}, err
		}
		return w.PurgeDigest(ctx, a.Call, a.Digest)
	})

// ---- the asking half ---------------------------------------------------- //

// AtlasReads is the atlas log's reads, answered by a data node whose copy
// serves (this one where it does), so a consumer cannot tell local from routed.
type AtlasReads struct{ r *Router }

// AtlasReads answers the atlas log's reads.
func (r *Router) AtlasReads() AtlasReads { return AtlasReads{r: r} }

var _ AtlasReader = AtlasReads{}

// Live answers every live claim.
func (x AtlasReads) Live(ctx context.Context, fresh statelog.Freshness) (atlasdomain.Live, error) {
	return call(ctx, x.r, opAtlasLive, nil, atlasLiveArgs{Fresh: fresh})
}

// Entry answers one entry by its id, and whether the log holds a claim on it.
func (x AtlasReads) Entry(ctx context.Context, id string,
	fresh statelog.Freshness) (atlasdomain.EntryRow, bool, error) {
	out, err := call(ctx, x.r, opAtlasEntry, nil, atlasEntryArgs{ID: id, Fresh: fresh})
	return out.Entry, out.Found, err
}

// Log answers one page of the log, newest first.
func (x AtlasReads) Log(ctx context.Context, q atlasdomain.LogQuery) (atlasdomain.LogPage, error) {
	return call(ctx, x.r, opAtlasLog, nil, q)
}

// Activity answers the log counted since an instant, and each entry's newest
// record.
func (x AtlasReads) Activity(ctx context.Context, since time.Time,
	fresh statelog.Freshness) (atlasdomain.Activity, error) {
	return call(ctx, x.r, opAtlasActivity, nil, atlasActivityArgs{Since: since, Fresh: fresh})
}

// Purges answers every purge, newest first.
func (x AtlasReads) Purges(ctx context.Context, fresh statelog.Freshness) ([]atlasdomain.PurgeRow, error) {
	return call(ctx, x.r, opAtlasPurges, nil, atlasPurgesArgs{Fresh: fresh})
}

// AtlasWrites is the atlas log's writes, made by a data node whose copy serves
// and which publishes (this one where it does), so a person's write is the same
// call from a node holding no data.
type AtlasWrites struct{ r *Router }

// AtlasWrites makes the atlas log's writes.
func (r *Router) AtlasWrites() AtlasWrites { return AtlasWrites{r: r} }

var _ AtlasWriter = AtlasWrites{}

// Record writes one person's claim, conditioned on the entry's version.
func (x AtlasWrites) Record(ctx context.Context, c atlasdomain.Call, claim atlas.Claim,
	ifMatch uint64) (statelog.Result, error) {
	return call(ctx, x.r, opAtlasRecord, nil, atlasRecordArgs{Call: c, Claim: claim, IfMatch: ifMatch})
}

// Observe writes one agent's claim, with the turn and the evidence behind it.
func (x AtlasWrites) Observe(ctx context.Context, c atlasdomain.Call, claim atlas.Claim,
	seen atlasdomain.Observation) (statelog.Result, error) {
	return call(ctx, x.r, opAtlasObserve, nil, atlasObserveArgs{Call: c, Claim: claim, Seen: seen})
}

// PurgeEntry erases an entry everywhere and marks it.
func (x AtlasWrites) PurgeEntry(ctx context.Context, c atlasdomain.Call, ref atlas.Ref) (statelog.Result, error) {
	return call(ctx, x.r, opAtlasPurgeEntry, nil, atlasPurgeEntryArgs{Call: c, Entry: ref})
}

// PurgeDigest erases the value whose digest this is everywhere and marks it.
func (x AtlasWrites) PurgeDigest(ctx context.Context, c atlasdomain.Call, digest string) (statelog.Result, error) {
	return call(ctx, x.r, opAtlasPurgeValue, nil, atlasPurgeValueArgs{Call: c, Digest: digest})
}
