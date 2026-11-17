package engine

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/codescan"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/gitlet"
	gitlethome "github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The atlas's code scan, wired: a Gitlet home loop worker recording what a
// repository's deploy and dependency files declare when its default branch
// moves (ADR-0133, internal/atlas/codescan).

// buildAtlasCodeScan builds the scan and registers it with this node's home
// loop, its git reads under the loop's own bound so they never run ahead of
// the seats. Each repository's standing lives in the coordination store, so a
// node without one registers nothing.
func (e *Engine) buildAtlasCodeScan(home *gitlethome.Loop) error {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	store, err := codescan.NewCoordStore(e.backends.Fleet)
	if err != nil {
		return err
	}
	scanner, err := codescan.New(codescan.Options{
		Code: atlasCodeReads{rows: e.router.Code(), git: home}, Log: atlasCodeLog{engine: e}, Store: store,
		// Records only where this node publishes, read per pass: the core
		// runtime is built beside Gitlet, and a node restarted to write
		// nothing must not.
		Admits: func() bool {
			_, _, ok := e.atlasLogSides()
			return ok
		},
	})
	if err != nil {
		return err
	}
	return home.Register(scanner)
}

// atlasCodeReads is Gitlet as the scan reads it: the rows through the router,
// and git through the home loop, which bounds every worker's reads together.
type atlasCodeReads struct {
	rows interface {
		Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	}
	git *gitlethome.Loop
}

// Read reads Gitlet's rows.
func (r atlasCodeReads) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	return r.rows.Read(ctx, q)
}

// GitRead is one of the home's own git reads.
func (r atlasCodeReads) GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	return r.git.GitRead(ctx, q)
}

// atlasLogSides is this node's atlas log, both sides, where it publishes.
func (e *Engine) atlasLogSides() (*atlasdomain.Reader, *atlasdomain.Writer, bool) {
	c := e.core.Load()
	if c == nil || !e.mode.Publishes() || c.atlasReader == nil || c.atlasWriter == nil {
		return nil, nil, false
	}
	return c.atlasReader, c.atlasWriter, true
}

// errNoAtlasLog is a call on a node holding no atlas log it publishes to,
// which the scan's Admits gate rules out first.
var errNoAtlasLog = errors.New("engine: this node holds no atlas log it publishes to")

// atlasCodeLog is the atlas log as the scan takes it, resolved at each call:
// the runtime that holds it is published after Gitlet is built.
type atlasCodeLog struct{ engine *Engine }

// LiveOf reads one voice's live claims from this node's atlas log.
func (l atlasCodeLog) LiveOf(ctx context.Context, src atlas.Source, fresh statelog.Freshness) (atlasdomain.Live,
	error) {

	reader, _, ok := l.engine.atlasLogSides()
	if !ok {
		return atlasdomain.Live{}, errNoAtlasLog
	}
	return reader.LiveOf(ctx, src, fresh)
}

// Read writes one read of one entry on this node's atlas log.
func (l atlasCodeLog) Read(ctx context.Context, c atlasdomain.Call, in atlasdomain.ReadInput) (statelog.Result,
	error) {

	_, writer, ok := l.engine.atlasLogSides()
	if !ok {
		return statelog.Result{}, errNoAtlasLog
	}
	return writer.Read(ctx, c, in)
}
