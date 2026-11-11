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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// AtlasToolDeps is what the atlas tools read on this node: the settings it
// runs, read per call, their revision and the deployment's addresses. Gitlet
// comes through the code tools' own half, as the caller (ADR-0127).
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
	return src
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

// turnReadsAtlas reports whether a turn of the seat handle may read the atlas
// within whom it works for (ADR-0109): the atlas tools' decision, made with
// the ceiling the turn's every tool call is held to, composed by the one
// composer, before its prompt is built. Undecidable is false, logged: a
// prompt that cannot tell shows no entry.
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

// atlasSystems is the atlas a turn's prompt names entries from: settings and
// records as the seat reads them, a private address only within its own
// reach. Gitlet's repositories are read by the atlas tools, as the seat and
// with its ceiling, which a prompt built before the turn's first call has no
// caller for.
type atlasSystems struct{ engine *Engine }

func (s atlasSystems) Systems(ctx context.Context, r prefetch.Request) (atlas.View, bool, error) {
	deps := AtlasToolDeps(s.engine)
	settings, running := AtlasSettingsFor(ctx, deps)
	if !running {
		return atlas.View{}, false, nil
	}
	answer, _ := deps.AtlasRead(ctx, nil, code.Principal{},
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
