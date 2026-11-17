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

// The atlas-collect duty, wired: the fleet singleton that reads each connected
// integration's vendor whole and records what changed on the atlas log
// (ADR-0129, internal/atlas/collect).

// atlasCollectDutyName is the fleet singleton the duty claims.
const atlasCollectDutyName = collect.DutyName

// atlasCollectDutyTTL is how long the duty survives without a re-claim: the
// longest visit plus two ticks to renew in. The worker re-claims before every
// source, so the lease covers one visit, bounded by [collect.VisitDeadline];
// a TTL of a few ticks would lapse under a large account's read and let a
// peer read the same vendor beside it.
const atlasCollectDutyTTL = collect.VisitDeadline + 2*collect.Tick

// startAtlasCollect arms the duty on a data node that publishes, the holder
// of the atlas log's writer; a node without one, or without a coordination
// store, runs no loop and says so once. Registration is static and the
// document read per tick: connected integrations move under a running loop,
// and a source whose block is gone answers [integration.ErrNotConfigured].
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
		// Each forge's repositories Gitlet does not mirror (ADR-0136),
		// visited after the forge.
		Code: forges,
		// Read every tick: a source resolves its credential from them, and
		// an apply can change both.
		Input: func(context.Context) (collect.Input, bool) {
			company := e.Company()
			if company == nil || company.Config == nil {
				return collect.Input{}, false
			}
			return collect.Input{Config: company.Config, Env: e.resolver()}, true
		},
		// Read fresh, so a shortened interval applies at once.
		Interval: func() time.Duration {
			company := e.Company()
			if company == nil || company.Config == nil {
				return config.Atlas{}.RefreshInterval()
			}
			return company.Config.Atlas.RefreshInterval()
		},
		ClaimDuty: e.workerDuty(atlasCollectDutyName, atlasCollectDutyTTL),
		// Before the claim: a node the fleet has moved past would read a
		// vendor under a credential the current document may no longer name.
		Admits: e.admits,
	})
	if err != nil {
		// Not fatal: a company whose atlas is not refreshed still runs
		// every seat.
		log.ErrorContext(ctx, "atlas_collect_unavailable", "error", err,
			"detail", "the atlas is not read from the company's vendors on this node")
		return
	}
	e.atlasCollect = worker
	// Detached, as the reconcile loop is.
	worker.Start(context.WithoutCancel(ctx))
}

// atlasSources is every registered vendor and the company's servers. The SSH
// source is built here, not registered, so each command it runs goes through
// this node's runner onto the access log.
func (e *Engine) atlasSources() []collect.Source {
	return append(collect.Registered(), sshint.NewAtlasSource(e.sshRun()))
}

// atlasForges reads the files of the repositories a forge hosts and Gitlet
// does not mirror, its standings in the coordination store beside the scan's.
func (e *Engine) atlasForges(duty collect.Log) (*codescan.Forges, error) {
	store, err := codescan.NewForgeStore(e.backends.Fleet)
	if err != nil {
		return nil, err
	}
	return codescan.NewForges(codescan.ForgeOptions{Log: duty, Store: store, Mirrored: e.atlasMirrored})
}

// atlasMirrored is the atlas key of every repository a Gitlet repository
// pulls from: the engine's own read of Gitlet's mirrors, which no principal
// makes. Only the address is read, never the credential reference beside it.
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
			// An address that is no forge's key mirrors nothing a forge lists.
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
// company connects, with its standing and pending ask at now, read off the
// coordination store since the node answering is rarely the duty's holder.
// False where this node runs no company or has no coordination store.
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
// for the API: coordination, not the worker's memory, since the node
// answering is usually not the duty's holder.
func (e *Engine) AtlasSourceStore() (*collect.CoordStore, error) {
	if e.backends == nil {
		return nil, errors.New("engine: this node has no coordination store to read a source's standing from")
	}
	return collect.NewCoordStore(e.backends.Fleet)
}

// stopAtlasCollect ends the loop, waiting for a visit in flight; called
// before the duties are released and the native half comes down, as every
// publishing loop is.
func (e *Engine) stopAtlasCollect() {
	if e.atlasCollect != nil {
		e.atlasCollect.Stop()
	}
}
