package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/config"
	sshint "github.com/crewlet/crewlet/internal/ssh"
)

// THE ATLAS-COLLECT DUTY, wired: the fleet singleton that reads each connected
// integration's vendor whole for the atlas and records what changed on the
// atlas log (ADR-0129, internal/atlas/collect).

// atlasCollectDutyName is the fleet singleton the duty claims.
const atlasCollectDutyName = collect.DutyName

// atlasCollectDutyTTL is how long the duty survives without a re-claim.
//
// DERIVED FROM THE LONGEST VISIT IT ADMITS, plus two ticks to renew in, for
// the integration duty's reason: the worker re-claims before every source it
// visits, so the lease only ever has to cover ONE visit, and a visit is bounded
// by [collect.VisitDeadline]. A TTL of three ticks would lapse under the first
// read of a large account, and a peer would read the same vendor beside it.
const atlasCollectDutyTTL = collect.VisitDeadline + 2*collect.Tick

// startAtlasCollect arms the duty on a data node that publishes, which is
// the node that holds the atlas log's writer. A node without one, or without
// a coordination store to record a source's standing in, runs no loop and
// says so once.
//
// Registration is static and the document is read per tick, for the reconcile
// loop's reason: which integrations are connected moves under a running loop,
// and each source answers [integration.ErrNotConfigured] for a block that is
// gone.
func (e *Engine) startAtlasCollect(ctx context.Context) {
	if e.backends == nil || e.backends.Fleet == nil {
		log.InfoContext(ctx, "atlas_collect_idle",
			"detail", "this node has no coordination store, so there is nowhere to record a source's standing")
		return
	}
	reader, writer, ok := e.atlasLogSides()
	if !ok {
		log.InfoContext(ctx, "atlas_collect_idle",
			"detail", "this node holds no atlas log it publishes to, so a data node reads the vendors for the atlas")
		return
	}
	store, err := collect.NewCoordStore(e.backends.Fleet)
	if err != nil {
		log.ErrorContext(ctx, "atlas_collect_unavailable", "error", err)
		return
	}
	worker, err := collect.New(collect.Options{
		Sources: e.atlasSources(),
		Store:   store,
		Log:     atlasCollectLog{Reader: reader, Writer: writer},
		// THE LIVE DOCUMENT AND THIS NODE'S RESOLVER, read on every tick:
		// a source resolves its own credential from them, and an apply can
		// change both.
		Input: func(context.Context) (collect.Input, bool) {
			company := e.Company()
			if company == nil || company.Config == nil {
				return collect.Input{}, false
			}
			return collect.Input{Config: company.Config, Env: e.resolver()}, true
		},
		// AND HOW OFTEN EACH SOURCE IS READ, the company's own field, read
		// fresh so a shortened interval applies at once.
		Interval: func() time.Duration {
			company := e.Company()
			if company == nil || company.Config == nil {
				return config.Atlas{}.RefreshInterval()
			}
			return company.Config.Atlas.RefreshInterval()
		},
		ClaimDuty: e.workerDuty(atlasCollectDutyName, atlasCollectDutyTTL),
		// THE POSTURE GATE, before the claim: a node the fleet has moved past
		// would read a vendor under a credential the current document may no
		// longer name.
		Admits: e.admits,
	})
	if err != nil {
		// NOT FATAL, for the reconcile loop's reason: a company whose atlas
		// is not refreshed from its vendors still runs every seat.
		log.ErrorContext(ctx, "atlas_collect_unavailable", "error", err,
			"detail", "the atlas is not read from the company's vendors on this node")
		return
	}
	e.atlasCollect = worker
	// Detached, for the reason the reconcile loop is.
	worker.Start(context.WithoutCancel(ctx))
}

// atlasSources is what the duty reads: every registered vendor, and the
// company's servers. The SSH source is built here rather than registered,
// because each command it runs is recorded through this node's runner, on the
// access log.
func (e *Engine) atlasSources() []collect.Source {
	return append(collect.Registered(), sshint.NewAtlasSource(e.sshRun()))
}

// atlasCollectLog is the atlas log's two sides as the duty takes them: this
// node's own, since the duty runs only where the log is published.
type atlasCollectLog struct {
	*atlasdomain.Reader
	*atlasdomain.Writer
}

// AtlasCollect exposes the duty, for the tests that hold its wiring.
func (e *Engine) AtlasCollect() *collect.Worker { return e.atlasCollect }

// AtlasSourceStore is the fleet row every source's standing is recorded in,
// for the API: read from coordination rather than the worker's memory, since
// the node answering is usually not the node holding the duty.
func (e *Engine) AtlasSourceStore() (*collect.CoordStore, error) {
	return collect.NewCoordStore(e.backends.Fleet)
}

// stopAtlasCollect ends the loop, waiting for a visit in flight. Before the
// duties are released and before the native half comes down, as every loop
// that publishes is.
func (e *Engine) stopAtlasCollect() {
	if e.atlasCollect != nil {
		e.atlasCollect.Stop()
	}
}
