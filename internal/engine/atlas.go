package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/atlas/gather"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// AtlasToolDeps is what the atlas tools read on this node: its settings, read
// per call, and every record source it reaches, the atlas's log included.
// Gitlet comes through the code tools' own half, as the caller (ADR-0127).
func AtlasToolDeps(e *Engine) builtin.AtlasDeps {
	if e == nil {
		return builtin.AtlasDeps{}
	}
	return builtin.AtlasDeps{Settings: e.atlasSettings, Records: e.atlasRecords, Log: e.atlasLog, Sources: e}
}

// AtlasRefreshToolDeps is what a person asks a read of one atlas source
// through: the coordination store, where the collect duty's holder reads the
// ask. Who asks is set per call. The zero value where this node has no
// coordination store.
func AtlasRefreshToolDeps(e *Engine) builtin.AtlasRefreshDeps {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return builtin.AtlasRefreshDeps{}
	}
	return builtin.AtlasRefreshDeps{Sources: e}
}

// atlasLog is the atlas's log as this node reads it: through the router, so
// a node without the data role reads its lines and its counts too.
func (e *Engine) atlasLog(context.Context) (builtin.AtlasLogReads, bool) {
	reads, ok := e.AtlasReads()
	if !ok {
		return nil, false
	}
	return reads, true
}

// atlasRecords is every record source the atlas reads, as this node reaches
// it: replicated reads through the router, so a node without the data role
// answers too, the coordination store's directly. A source it cannot reach is
// left nil, which the read reports as unread rather than empty.
func (e *Engine) atlasRecords(ctx context.Context) gather.Sources {
	var src gather.Sources
	if e.backends != nil {
		if e.backends.Fleet != nil {
			src.ToolServers, src.ServerTests = e.backends.Fleet, e.backends.Fleet
			src.NodeStatuses = e.backends.Fleet
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
	// What people recorded, so every read folds it.
	if reads, ok := e.AtlasReads(); ok {
		src.Log = reads
	}
	return src
}

// AtlasWriteToolDeps is what the atlas's write tools change it through: the
// log through the router, so a write from a node without data lands on a
// data node. Who writes is set per call. The zero value where this node
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

// atlasRecordBucket is the fleet counter one seat's atlas records are counted
// in, by the seat's derived agent id, as its token counter is keyed.
func atlasRecordBucket(agentID string) string { return "atlas-records:" + agentID }

// AtlasRecordToolDeps is what an agent's atlas records are made through
// (ADR-0132): the log's writer through the router, and the fleet's shared
// counter, a UTC day wide, so a seat's allowance holds whichever node runs its
// turns. The zero value where this node reaches no writer or no counter.
func AtlasRecordToolDeps(e *Engine) builtin.AtlasRecordDeps {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return builtin.AtlasRecordDeps{}
	}
	writes, writable := e.AtlasWrites()
	if !writable {
		return builtin.AtlasRecordDeps{}
	}
	counter := e.backends.Fleet
	return builtin.AtlasRecordDeps{
		Log: writes,
		Allow: func(ctx context.Context, agentID string, limit int, now time.Time) (bool, error) {
			return counter.Allow(ctx, atlasRecordBucket(agentID), limit, coord.MaxCounterWindow, now)
		},
	}
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
// this node cannot tell: the fleet's pointer counts only while it names c's
// own activation, never one this node has not applied yet.
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

// turnReadsAtlas reports whether a turn of the seat handle may read the atlas
// within whom it works for (ADR-0109): the atlas tools' decision under the
// turn's ceiling, made before its prompt is built. Undecidable is false,
// logged: a prompt that cannot tell shows no entry.
func (e *Engine) turnReadsAtlas(ctx context.Context, company *Company, handle string, tel turnTelemetry) bool {
	seat := company.Org.AgentSeatByHandle(handle)
	if seat == nil {
		return false
	}
	turn := &turnctx.Turn{Seat: seat, Org: company.Org, Requester: tel.requester, WorkingFor: tel.workingFor,
		RequesterLogin: tel.requesterLogin, RequesterApps: tel.requesterApps, Senders: tel.senders}
	ceiling, err := builtin.CeilingOfTurn(ctx, turn, turnRequesters{engine: e})
	if err != nil {
		log.WarnContext(ctx, "atlas_turn_ceiling_unread", "seat", handle, "error", err)
		return false
	}
	d := authz.DecideWithinApps(ctx, turnctx.SeatPrincipal(company.Org, seat), ceiling, authz.ActionAtlasGet,
		authz.Object{Kind: authz.KindAtlasEntry}, orgchart.Of(company.Org), time.Now())
	if d.Unknown() {
		log.WarnContext(ctx, "atlas_turn_undecided", "seat", handle)
		return false
	}
	return d.Allowed
}

// atlasSystems is the atlas a turn's prompt names entries from, as the seat
// reads it, a private address only within its own reach. Gitlet's
// repositories are left to the atlas tools: they read as the seat under its
// ceiling, which a prompt built before the first call has no caller for.
type atlasSystems struct{ engine *Engine }

func (s atlasSystems) Systems(ctx context.Context, r prefetch.Request) (atlas.View, bool, error) {
	deps := AtlasToolDeps(s.engine)
	settings, running := AtlasSettingsFor(ctx, deps)
	if !running {
		return atlas.View{}, false, nil
	}
	answer, _ := deps.AtlasRead(ctx, nil, "", gitlet.Principal{},
		statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)}, time.Now().UTC())
	view, _ := builtin.AtlasForSeat(answer.View, settings.Company, r.Seat)
	return view, true, nil
}

// AtlasSettingsFor is the settings deps reads, and false where it reads none.
func AtlasSettingsFor(ctx context.Context, deps builtin.AtlasDeps) (builtin.AtlasSettings, bool) {
	if deps.Settings == nil {
		return builtin.AtlasSettings{}, false
	}
	return deps.Settings(ctx)
}
