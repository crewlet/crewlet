package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifactrender"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/static"
)

// render_artifact's engine wiring: the renderer over this node's own artifact
// origin and the browser Tier A names, the run's box and the turn tally.

// RenderToolDeps are render_artifact's dependencies on this node, or the zero
// value, which offers no render, where the node takes no artifact uploads.
func RenderToolDeps(e *Engine) builtin.RenderDeps {
	if e == nil || e.router == nil || e.objects == nil {
		return builtin.RenderDeps{}
	}
	deps := builtin.RenderDeps{Renderer: e.artifactRenderer(), Turns: e.renderTurns}
	if e.sandbox.Load() != nil {
		deps.Boxes = renderBoxes{engine: e}
	}
	return deps
}

// artifactRenderer is this node's renderer, built at its first use and for
// the life of the process, or one that answers why this node renders nothing.
func (e *Engine) artifactRenderer() builtin.ArtifactRenderer {
	e.renderOnce.Do(func() {
		r, err := e.buildRenderer()
		if err != nil {
			e.render = unrendered{err: err}
			return
		}
		e.render = r
	})
	return e.render
}

// buildRenderer builds the renderer from Tier A, or answers why it cannot.
func (e *Engine) buildRenderer() (*artifactrender.Renderer, error) {
	boot := e.boot
	switch {
	case boot == nil || boot.Artifacts.Render.BrowserURL == "":
		return nil, artifactrender.ErrNoBrowser
	case boot.API.Artifacts.ExternalBase() == "" || boot.API.ExternalBase() == "":
		return nil, artifactrender.ErrNoOrigin
	}
	tickets, origins, err := ArtifactServing(boot)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", artifactrender.ErrNoOrigin, err)
	}
	reads, _ := ArtifactsEstate(e)
	tree, err := fs.Sub(static.FS(), "artifacts")
	if err != nil {
		return nil, fmt.Errorf("engine: the embedded artifact tree: %w", err)
	}
	runtimes, err := embeddedReleases(runtimeTree)
	if err != nil {
		return nil, err
	}
	r, err := artifactrender.New(artifactrender.Config{
		BrowserURL: boot.Artifacts.Render.BrowserURL, Client: httpx.Client(0),
		Tickets: tickets, Origins: origins, Dashboard: boot.API.ExternalBase(),
		Serve: artifactserve.Deps{Artifacts: reads, Objects: e.Objects(),
			Chart: ChartAuthorityOf(e), Local: e, Embedded: tree},
		KitVersion: kitVersion(), Contract: runtimes.Latest(),
	})
	if err != nil {
		return nil, err
	}
	// FOR THE LIFE OF THE PROCESS, as the renderer is: every movement this
	// node hears forgets the render handler's verdicts, as the origin's.
	e.OnArtifactsMoved(r.ArtifactsMoved)
	return r, nil
}

// unrendered is the renderer of a node that renders nothing, answering why.
type unrendered struct{ err error }

func (u unrendered) Render(context.Context, artifactrender.Request) (artifactrender.Report, error) {
	return artifactrender.Report{}, u.err
}

// renderBoxes writes a render's screenshots into the box of the executor
// working in the turn: the same box, held to the same rules, a publish reads
// from ([artifactBoxes]).
type renderBoxes struct{ engine *Engine }

func (b renderBoxes) Shots(ctx context.Context, t *turnctx.Turn) (builtin.ShotBox, error) {
	box, err := artifactBoxes(b).attach(ctx, t)
	if err != nil {
		return nil, err
	}
	return shotBox{box: box}, nil
}

// shotBox is one attached box, written under its home.
type shotBox struct{ box sandbox.Sandbox }

// shotRemoveTimeout bounds the removal of an artifact's earlier shots.
const shotRemoveTimeout = 30

func (s shotBox) Replace(ctx context.Context, dir string, files []builtin.BoxFile) ([]string, error) {
	abs := path.Join(s.box.Home(), dir)
	res, err := s.box.Exec(ctx, "rm -rf -- "+sandbox.ShellQuote(abs),
		sandbox.ExecOptions{TimeoutSec: shotRemoveTimeout})
	switch {
	case err != nil:
		return nil, err
	case res.ExitCode != 0:
		return nil, errors.New("the box could not clear " + dir)
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		rel := path.Join(dir, f.Name)
		if err := s.box.WriteFile(ctx, rel, f.Data); err != nil {
			return out, err
		}
		out = append(out, path.Join(s.box.Home(), rel))
	}
	return out, nil
}

// renderState is the engine's render fields, kept beside each other.
type renderState struct {
	renderOnce  sync.Once
	render      builtin.ArtifactRenderer
	renderTurns *builtin.RenderTurns
}
