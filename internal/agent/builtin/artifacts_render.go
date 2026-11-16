package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifactrender"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// render_artifact: a seat sees its own page as a viewer does before review,
// rendered by the engine so no credential reaches the box. A seat's tool
// alone: a person opens the page in the dashboard.

// RenderArtifactTool is the render's tool name: its authority row's action.
const RenderArtifactTool = string(authz.ActionArtifactRender)

// ArtifactRenderer renders a version as a viewer sees it.
type ArtifactRenderer interface {
	Render(ctx context.Context, req artifactrender.Request) (artifactrender.Report, error)
}

// RenderBoxes hands a render's screenshots to the box of the executor working
// in the turn.
type RenderBoxes interface {
	// Shots is the turn's executor's box for screenshots, [ErrNoBox] where
	// the turn has none it may write to.
	Shots(ctx context.Context, turn *turnctx.Turn) (ShotBox, error)
}

// ShotBox is one executor's box as a render writes its screenshots into it.
type ShotBox interface {
	// Replace empties dir, a path under the box user's home and outside
	// the workspace, writes each file into it, and answers the absolute
	// path of each.
	Replace(ctx context.Context, dir string, files []BoxFile) ([]string, error)
}

// BoxFile is one file a render writes.
type BoxFile struct {
	Name string
	Data []byte
}

// RenderDeps are what render_artifact acts through beyond the artifacts and
// datastore tools' halves. The zero value offers no render.
type RenderDeps struct {
	// Renderer renders; it answers why where this node has no browser.
	Renderer ArtifactRenderer

	// Boxes writes the screenshots. Nil, or a turn with no box, answers
	// the findings and each shot's geometry without the images.
	Boxes RenderBoxes

	// Turns is this node's tally of each turn segment's renders.
	Turns *RenderTurns
}

// RenderTools is the one tool a seat holds beside [ArtifactTools] that the
// operator catalogue does not serve.
func RenderTools() []string { return []string{RenderArtifactTool} }

// renderDir is where a render's screenshots go in the box, below the home and
// outside the workspace, so a build or a commit never takes them in.
const renderDir = ".crewlet/renders"

// renderCandidates are the render tool a seat's registry may offer.
func renderCandidates(deps Deps) []candidate {
	a := deps.Artifacts
	a.work, a.compact = deps.Work, deps.Compact
	d := dataTools{art: a, data: deps.ArtifactData}
	r := deps.Render
	return []candidate{
		{&renderArtifact{d: d, r: r}, d.serves() && r.Renderer != nil && r.Turns != nil},
	}
}

type renderArtifact struct {
	d dataTools
	r RenderDeps
}

var _ tools.SeatCallable = (*renderArtifact)(nil)

func (t *renderArtifact) Name() string { return RenderArtifactTool }

func (t *renderArtifact) Description() string {
	return "Look at a page artifact (an html page, or a design's artboard) exactly as a " +
		"viewer sees it, in the engine's browser: the published version in its frame, with " +
		"the kit, the runtime and its data, at the widths, themes and scroll positions you " +
		"ask, after any clicks or keys you name. Use it on your own work before you ask " +
		"anybody to review it. The answer lists CSS custom properties that resolve to " +
		"nothing, sticky or fixed bars that are see-through or hide content, console errors, " +
		"requests the page made that failed, links that go nowhere, and writes the page " +
		"tried; each screenshot is a PNG in your box, which you open to look at. Nothing " +
		"is written to the artifact."
}

func (t *renderArtifact) Parameters() map[string]any {
	themes := make([]any, 0, len(artifactrender.Themes))
	for _, th := range artifactrender.Themes {
		themes = append(themes, string(th))
	}
	kinds := make([]any, 0, len(artifactrender.ActionKinds))
	for _, k := range artifactrender.ActionKinds {
		kinds = append(kinds, string(k))
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"artifact": artifactParam(),
			"version": map[string]any{
				"type":        "integer",
				"description": "A retained version's number. Omit for the head.",
			},
			"page": map[string]any{
				"type": "string",
				"description": "The HTML page to open: omit for a page's entry; a design " +
					"names the artboard's file.",
			},
			"widths": map[string]any{
				"type": "array", "items": map[string]any{"type": "integer"},
				"description": fmt.Sprintf("The frame's widths in CSS pixels, %d..%d "+
					"(default 1440 and 390). Below %d the page is laid out as a phone "+
					"lays it out.", artifacts.RenderMinWidth, artifacts.RenderMaxWidth,
					artifacts.RenderPhoneBelow),
			},
			"themes": map[string]any{
				"type": "array", "items": map[string]any{"type": "string", "enum": themes},
				"description": "The reader's appearance (default both).",
			},
			"scroll": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"description": "Where each shot is taken: `top` (the default), `bottom`, " +
					"a number of pixels, or a CSS selector scrolled to as an in-page link " +
					"scrolls to its target. Every in-page link is also followed and " +
					"checked without a shot.",
			},
			"actions": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind": map[string]any{"type": "string", "enum": kinds},
						"selector": map[string]any{"type": "string",
							"description": "The element a click or a hover lands on, or a " +
								"press focuses first."},
						"key": map[string]any{"type": "string",
							"description": "A press's key: Enter, Tab, Escape, ArrowDown " +
								"and the like, or one character, after any of Shift+, " +
								"Control+, Alt+ and Meta+."},
					},
					"required": []any{"kind"},
				},
				"description": fmt.Sprintf("Up to %d clicks, hovers or key presses played "+
					"in order after each load and before its shots: a menu opened, a tab "+
					"chosen.", artifacts.RenderActions),
			},
		},
		"required": []any{"artifact"},
	}
}

func (t *renderArtifact) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *renderArtifact) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	spec, problem := renderSpecOf(args)
	if problem != "" {
		return failed(RenderArtifactTool + ": " + problem), nil
	}
	art := t.d.art
	c, refusal := art.caller(ctx, turn, t.Name(), args, false)
	if refusal != nil {
		return *refusal, nil
	}
	h, ref, refusal := art.head(ctx, t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	dec, refusal := art.decide(ctx, c, t.Name(), authz.ActionArtifactRender, h, ref)
	switch {
	case refusal != nil:
		return *refusal, nil
	case dec.Elevated:
		// AN ADMINISTRATOR'S READ OF ANOTHER'S WORK IS RECORDED, and a render
		// writes nothing, so it renders only what the caller may read in
		// their own right.
		return refusedBy(tools.RefusalForbidden, ErrRenderElevated, fmt.Sprintf("%s: only "+
			"your administrator's override admits you to %q, which is recorded as an "+
			"elevated read, and a render records nothing. Read it with %s, or ask its "+
			"owner (%s) to share it with you.", t.Name(), clip(ref), GetArtifactTool,
			h.Owner)), nil
	case h.Status == artifactsdomain.StatusTrashed:
		return artifactTrashed(t.Name(), ref, h), nil
	case !h.Kind.Framed():
		return failed(fmt.Sprintf("%s: a %s artifact is drawn by the dashboard from its "+
			"format and runs no page of its own, so there is nothing to render; read it "+
			"with %s.", t.Name(), h.Kind, GetArtifactTool)), nil
	}
	number := argInt(args, "version", h.HeadNumber)
	if number < 1 || number > h.HeadNumber {
		return failed(fmt.Sprintf("%s: artifact %q has versions 1..%d.", t.Name(), clip(ref),
			h.HeadNumber)), nil
	}
	version, err := art.Reader.Version(ctx, h.ID, number)
	if err != nil {
		if errors.Is(err, artifactsdomain.ErrNotFound) {
			return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: artifact %q no "+
				"longer keeps version %d.", t.Name(), clip(ref), number)), nil
		}
		return artifactReadFailure(ctx, t.Name(), ref, err), nil
	}
	runID := ""
	if turn != nil {
		runID = turn.RunID
	}
	refund, used, ok := t.r.Turns.spend(runID)
	if !ok {
		return refusedBy(tools.RefusalForbidden, ErrRenderTurnQuota, fmt.Sprintf("%s: this "+
			"turn has rendered %d times, the most one turn may. Fix what the last render "+
			"found, then finish the turn.", t.Name(), used)), nil
	}
	reads := &renderReads{d: t.d, turn: turn, args: args, id: h.ID}
	org := art.orgFor(turn)
	req := artifactrender.Request{
		Principal: c.principal,
		Admit:     t.admit(c, h.ID),
		Viewer:    renderViewer(c, dec.Standing.Role, org),
		People:    renderPeople(org),
		Head:      h, Version: version, Page: spec.page,
		Widths: spec.widths, Themes: spec.themes, Scroll: spec.scroll, Actions: spec.actions,
		Bridge: reads,
	}
	rep, err := t.r.Renderer.Render(ctx, req)
	if err != nil {
		refund()
		return renderFailure(err), nil
	}
	return t.answer(ctx, turn, c, h, version, rep)
}

// ErrRenderElevated is a render only an administrator's override admitted.
var ErrRenderElevated = errors.New("builtin: a render admitted only by the admin path")

// ErrRenderTurnQuota is a turn past [artifacts.TurnRenders].
var ErrRenderTurnQuota = errors.New("builtin: the turn has rendered as often as it may")

// admit decides the render again for every document the page loads, as the
// tool decided it: the caller and whoever the turn acts for, over a fresh
// read of the artifact.
func (t *renderArtifact) admit(c artifactCaller, id artifacts.ID) func(context.Context) iam.Resolution {
	art := t.d.art
	return func(ctx context.Context) iam.Resolution {
		h, _, err := art.Reader.Artifact(ctx, id, seatRead)
		switch {
		case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
			return iam.Anonymous
		case err != nil:
			return iam.Unknown
		}
		d := authz.DecideWithin(ctx, c.principal, c.ceiling, authz.ActionArtifactRender,
			art.objectOf(h), art.Chart, art.now())
		switch {
		case d.Unknown():
			return iam.Unknown
		case !d.Allowed || d.Elevated:
			return iam.Anonymous
		}
		return iam.Resolved
	}
}

// renderFailure is a render that answered nothing.
func renderFailure(err error) tools.Result {
	msg := strings.TrimPrefix(err.Error(), "artifactrender: ")
	switch {
	case errors.Is(err, artifactrender.ErrNoBrowser):
		return refusedBy(tools.RefusalUnavailable, err, RenderArtifactTool+": this deployment "+
			"has no browser to render in: whoever runs it sets artifacts.render.browser_url "+
			"in the Tier A of every node that runs seats. Tell them; until then, check the "+
			"page's source with "+GetArtifactTool+".")
	case errors.Is(err, artifactrender.ErrNoOrigin):
		return refusedBy(tools.RefusalUnavailable, err, RenderArtifactTool+": this node names "+
			"no artifact origin or no dashboard address to serve a frame under: whoever runs "+
			"this deployment sets api.artifacts.external_url and api.external_url on every "+
			"node that runs seats, as on the ones serving the dashboard. Tell them.")
	case errors.Is(err, artifactrender.ErrBusy):
		return refusedBy(tools.RefusalUnavailable, err, RenderArtifactTool+": the engine is "+
			"rendering as many pages as it may right now. Try again in a minute.")
	case errors.Is(err, artifactrender.ErrBrowser):
		return refusedBy(tools.RefusalUnavailable, err, RenderArtifactTool+": the render "+
			"browser could not be driven ("+msg+"). Try again shortly; if it goes on, tell "+
			"whoever runs this deployment.")
	case errors.Is(err, artifactrender.ErrRequest):
		return failed(RenderArtifactTool + ": " + msg)
	case errors.Is(err, context.DeadlineExceeded):
		return refusedBy(tools.RefusalUnavailable, err, RenderArtifactTool+": the render did "+
			"not finish in time. Ask for fewer shots, or try again shortly.")
	}
	return refusedBy(tools.RefusalUnavailable, err, RenderArtifactTool+" could not render the "+
		"page right now ("+msg+"). Try again shortly.")
}

// renderViewer is who the page is told is viewing: the seat, as the chart
// names it, at its role on the artifact.
func renderViewer(c artifactCaller, role artifacts.Role, o *org.Organization) artifactrender.Viewer {
	seat := c.principal.Seat
	v := artifactrender.Viewer{Seat: seat, DisplayName: seat, Kind: "agent", Role: role}
	if o == nil {
		return v
	}
	if r := o.SeatByHandle(seat); r != nil {
		if r.Name != "" {
			v.DisplayName = r.Name
		}
		if r.IsHuman() {
			v.Kind = "human"
		}
	}
	return v
}

// maxRenderPeople bounds the people a page that declared `identity` is told
// of: a company's chart, past what a person picker lists.
const maxRenderPeople = 500

// renderPeople is every seat of the chart, in display facts alone, as a
// viewer's dashboard answers a page.
func renderPeople(o *org.Organization) []artifactrender.Person {
	if o == nil {
		return nil
	}
	var out []artifactrender.Person
	for r := range o.AllRoles() {
		if len(out) == maxRenderPeople {
			break
		}
		kind := "agent"
		if r.IsHuman() {
			kind = "human"
		}
		name := r.Name
		if name == "" {
			name = r.Handle()
		}
		out = append(out, artifactrender.Person{Seat: r.Handle(), DisplayName: name, Kind: kind})
	}
	return out
}
