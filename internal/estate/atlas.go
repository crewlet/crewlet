package estate

import (
	"context"

	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ATLAS LOG'S READS AS OPERATIONS: what people record about what the
// company runs on (ADR-0126), read from whichever node asks. A seat's turn reads
// the atlas where its seat is held, a seats-only leaf included, and every read
// is answered at the level its caller passes, on this node's floor on the log.
//
// ITS WRITES ARE NOT ROUTED: a person's write reaches the API, which runs only
// on a data node, and a data node writes through its own writer, so nothing
// writes the log from a node that holds no copy of it.

// AtlasReader is the atlas log's read side: the domain reader's methods a node
// off the estate needs.
type AtlasReader interface {
	Live(ctx context.Context, fresh statelog.Freshness) (atlasdomain.Live, error)
	Entry(ctx context.Context, id string, fresh statelog.Freshness) (atlasdomain.EntryRow, bool, error)
	History(ctx context.Context, id, cursor string, limit int,
		fresh statelog.Freshness) (atlasdomain.HistoryPage, error)
	Purges(ctx context.Context, fresh statelog.Freshness) ([]atlasdomain.PurgeRow, error)
}

// The domain's own sides are what a data node serves the log with.
var _ AtlasReader = (*atlasdomain.Reader)(nil)

// AtlasHalf is the atlas log a data node serves.
type AtlasHalf struct {
	Reader AtlasReader
}

// atlasReader is the half's reader, or [errNoHalf].
func (b Backend) atlasReader() (AtlasReader, error) {
	if b.Atlas.Reader == nil {
		return nil, errNoHalf
	}
	return b.Atlas.Reader, nil
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

type atlasHistoryArgs struct {
	ID     string
	Cursor string
	Limit  int
	Fresh  statelog.Freshness
}

var opAtlasHistory = define("atlas.history", opRead, atlasDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a atlasHistoryArgs) (atlasdomain.HistoryPage, error) {
		r, err := b.atlasReader()
		if err != nil {
			return atlasdomain.HistoryPage{}, err
		}
		return r.History(ctx, a.ID, a.Cursor, a.Limit, a.Fresh)
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

// History answers one page of an entry's claims, newest first.
func (x AtlasReads) History(ctx context.Context, id, cursor string, limit int,
	fresh statelog.Freshness) (atlasdomain.HistoryPage, error) {
	return call(ctx, x.r, opAtlasHistory, nil,
		atlasHistoryArgs{ID: id, Cursor: cursor, Limit: limit, Fresh: fresh})
}

// Purges answers every purge, newest first.
func (x AtlasReads) Purges(ctx context.Context, fresh statelog.Freshness) ([]atlasdomain.PurgeRow, error) {
	return call(ctx, x.r, opAtlasPurges, nil, atlasPurgesArgs{Fresh: fresh})
}
