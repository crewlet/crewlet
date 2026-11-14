package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/codescan"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/integration"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/statelog"
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
	duty := atlasCollectLog{Reader: reader, Writer: writer}
	forges, err := e.atlasForges(duty)
	if err != nil {
		log.ErrorContext(ctx, "atlas_collect_unavailable", "error", err)
		return
	}
	worker, err := collect.New(collect.Options{
		Sources: e.atlasSources(),
		Store:   store,
		Log:     duty,
		// AND EACH FORGE'S REPOSITORIES' FILES, where Gitlet mirrors none
		// of them (ADR-0136): a visit of the same duty, after the forge's.
		Code: forges,
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

// atlasForges is the reader of the files of the repositories a forge hosts
// and Gitlet does not mirror: its standings beside the scan's own in the
// fleet's coordination store, and the atlas log as the duty holds it.
func (e *Engine) atlasForges(duty collect.Log) (*codescan.Forges, error) {
	store, err := codescan.NewForgeStore(e.backends.Fleet)
	if err != nil {
		return nil, err
	}
	return codescan.NewForges(codescan.ForgeOptions{Log: duty, Store: store, Mirrored: e.atlasMirrored})
}

// atlasMirrored is the atlas key of every repository a Gitlet repository
// pulls from, read from Gitlet's own listing of its mirrors: the engine's
// read, which no principal makes. A company that keeps no code in Gitlet
// mirrors nothing. Only the address is read; the credential beside it is a
// reference, and is not.
func (e *Engine) atlasMirrored(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	company := e.Company()
	if company == nil || company.Config == nil || company.Config.CodeBackendFor() != config.CodeGitlet {
		return out, nil
	}
	if e.router == nil {
		return nil, errors.New("engine: this node reaches no Gitlet to list the mirrors of")
	}
	for cursor := ""; ; {
		page, err := e.router.Code().Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadMirrorsDue, Cursor: cursor,
			Fresh: statelog.Freshness{Level: statelog.ReadStale}})
		if err != nil {
			return nil, fmt.Errorf("engine: list Gitlet's mirrors: %w", err)
		}
		for _, m := range page.Mirrors {
			if m.From == nil {
				continue
			}
			// AN ADDRESS THAT IS NO FORGE'S KEY mirrors nothing a forge lists.
			if key, err := atlas.RepositoryKeyOf(m.From.URL); err == nil {
				out[key] = true
			}
		}
		if cursor = page.Next; cursor == "" {
			return out, nil
		}
	}
}

// AtlasSourceReport is every source this build reads that the running
// company connects, as a reader is shown it at now: its standing and the ask
// waiting on it, both off the coordination store, since the node answering
// is rarely the node holding the duty. False where this node runs no company
// or has no coordination store to read.
func (e *Engine) AtlasSourceReport(ctx context.Context, now time.Time) (collect.Report, bool, error) {
	company := e.Company()
	if company == nil || company.Config == nil || e.backends == nil || e.backends.Fleet == nil {
		return collect.Report{}, false, nil
	}
	store, err := e.AtlasSourceStore()
	if err != nil {
		return collect.Report{}, false, err
	}
	standings, err := store.Load(ctx)
	if err != nil {
		return collect.Report{}, false, err
	}
	asked, err := store.Requests(ctx)
	if err != nil {
		return collect.Report{}, false, err
	}
	var kinds []integration.Kind
	for _, src := range e.atlasSources() {
		if collect.Connected(src, company.Config) {
			kinds = append(kinds, src.Kind())
		}
	}
	return collect.Describe(kinds, standings, asked, company.Config.Atlas.RefreshInterval(), now), true, nil
}

// AskAtlasRefresh records a person's ask that a source be read now, for the
// node holding the duty to honour.
func (e *Engine) AskAtlasRefresh(ctx context.Context, kind integration.Kind, req collect.Request) error {
	store, err := e.AtlasSourceStore()
	if err != nil {
		return err
	}
	return store.Ask(ctx, kind, req)
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
	if e.backends == nil {
		return nil, errors.New("engine: this node has no coordination store to read a source's standing from")
	}
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
