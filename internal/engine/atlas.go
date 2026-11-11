package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/store"
)

// AtlasToolDeps is what the atlas tools read on this node: the settings it
// runs, read per call, their revision and the deployment's addresses, and
// every record source it reaches, what people recorded in the atlas's log
// included. Gitlet comes through the code tools' own half, as the caller
// (ADR-0127).
func AtlasToolDeps(e *Engine) builtin.AtlasDeps {
	if e == nil {
		return builtin.AtlasDeps{}
	}
	return builtin.AtlasDeps{Settings: e.atlasSettings, Records: e.atlasRecords}
}

// atlasRecords is every record source of the engine's the atlas reads, as
// this node reaches it: the replicated reads through the estate router, so a
// node without the data role answers them too, and the coordination store's
// directly. A source this node reaches none of is left nil, and the read
// reports it as unread rather than empty.
func (e *Engine) atlasRecords(ctx context.Context) gather.Sources {
	var src gather.Sources
	if e.backends != nil {
		if e.backends.Fleet != nil {
			src.ToolServers, src.ServerTests = e.backends.Fleet, e.backends.Fleet
		}
		if e.backends.Coord != nil {
			src.Nodes = e.backends.Coord
		}
	}
	if store, err := e.IntegrationStore(); err == nil {
		src.Integrations = store
	} else {
		log.WarnContext(ctx, "atlas_integrations_unread", "error", err)
	}
	if ledger, err := e.SeatAccessLedger(); err == nil {
		src.SeatAccess = ledger
	} else {
		log.WarnContext(ctx, "atlas_seat_access_unread", "error", err)
	}
	if reads, ok := e.accessReads(); ok {
		src.AccessLog = reads
	}
	if e.router != nil {
		src.Usage = e.UsageReads()
	}
	src.Alarms = e
	// AND WHAT PEOPLE RECORDED, through the router, so every read folds it.
	if reads, ok := e.AtlasReads(); ok {
		src.Log = reads
	}
	return src
}

// AtlasWriteToolDeps is what the atlas's write tools change it through on this
// node: the log, read and written through the router, so a person's write from
// a node without data is a data node's. Who writes and the chart an owner is
// named from are the surface's, set per call. The zero value where this node
// reaches no copy of the log.
func AtlasWriteToolDeps(e *Engine) builtin.AtlasWriteDeps {
	if e == nil {
		return builtin.AtlasWriteDeps{}
	}
	reads, readable := e.AtlasReads()
	writes, writable := e.AtlasWrites()
	if !readable || !writable {
		return builtin.AtlasWriteDeps{}
	}
	return builtin.AtlasWriteDeps{Log: atlasLog{AtlasReader: reads, AtlasWriter: writes}}
}

// atlasLog is the log's two sides as one, as the write tools take it.
type atlasLog struct {
	estate.AtlasReader
	estate.AtlasWriter
}

// atlasSettings is the settings this node runs as the atlas reads them, and
// false where it runs none yet.
func (e *Engine) atlasSettings(ctx context.Context) (builtin.AtlasSettings, bool) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return builtin.AtlasSettings{}, false
	}
	public := ""
	if e.webhookBase != e.externalBase {
		public = e.webhookBase
	}
	return builtin.AtlasSettings{
		Company:    c.Config,
		Revision:   e.runningRevision(ctx, c),
		Deployment: derive.Deployment{APIAddress: e.externalBase, PublicAddress: public},
	}, true
}

// runningRevision is the revision the epoch c was applied from, or "" where
// this node cannot tell: the fleet's pointer names it only while it names the
// activation c was applied from, never one this node has not applied yet.
func (e *Engine) runningRevision(ctx context.Context, c *Company) string {
	if e.backends == nil || e.backends.Fleet == nil {
		return ""
	}
	target, found, err := e.backends.Fleet.Target(ctx)
	if err != nil {
		log.WarnContext(ctx, "atlas_revision_unread", "error", err,
			"detail", "the fleet's activation could not be read, so this atlas read names no revision "+
				"for the settings it read")
		return ""
	}
	return revisionRunning(target, found, c)
}

// revisionRunning is target's revision where target is the activation c was
// applied from, compared as the engine compares activation instants
// ([store.EncodeTime]), and "" otherwise.
func revisionRunning(target coord.Activation, found bool, c *Company) string {
	if !found || c == nil || c.ActivatedAt.IsZero() ||
		store.EncodeTime(target.At) != store.EncodeTime(c.ActivatedAt) {
		return ""
	}
	return target.RevisionID
}
