package artifactrender

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/artifactserve/ticket"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("artifactrender")

// ErrNoBrowser reports a node configured with no render browser.
var ErrNoBrowser = errors.New("artifactrender: this node names no render browser; an operator " +
	"sets artifacts.render.browser_url in its Tier A")

// ErrNoOrigin reports a node whose Tier A names no artifact origin or no
// dashboard, the two origins a render's frame is served and embedded under.
var ErrNoOrigin = errors.New("artifactrender: this node names no artifact origin " +
	"(api.artifacts.external_url) or no dashboard address (api.external_url), which a " +
	"render serves its frame under, as every viewer's is")

// ErrBusy reports a node already driving [artifacts.RenderConcurrency] renders
// for [artifacts.RenderQueueWait].
var ErrBusy = errors.New("artifactrender: this node is driving as many renders as it may; try again " +
	"in a minute")

// ErrBrowser reports a render browser that could not be reached or driven.
var ErrBrowser = errors.New("artifactrender: the render browser could not be driven")

// ErrRequest reports a request outside the render's bounds, naming them.
var ErrRequest = errors.New("artifactrender: the render request is outside its bounds")

// LineagePrefix names a render's credential: a class only the render's own
// resolver knows, so the public origin answers a ticket minted through it as
// one whose credential has ended.
const LineagePrefix = "render:"

// Theme is a reader's appearance, as the dashboard resolves it.
type Theme string

// The two appearances a reader picks.
const (
	ThemeLight Theme = "light"
	ThemeDark  Theme = "dark"
)

// Themes are every theme, in the order a render takes them.
var Themes = []Theme{ThemeLight, ThemeDark}

// Valid reports whether t is a theme this build names.
func (t Theme) Valid() bool { return slices.Contains(Themes, t) }

// Scroll is where a shot is taken: the top, the bottom, a pixel offset, or an
// element scrolled to as an in-page link scrolls to its target.
type Scroll struct {
	Pixels   int
	Selector string
	Bottom   bool
}

// ParseScroll reads a scroll position as a tool argument spells it: `top`,
// `bottom`, a whole number of pixels, or a CSS selector.
func ParseScroll(s string) (Scroll, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "" || s == "top":
		return Scroll{}, nil
	case s == "bottom":
		return Scroll{Bottom: true}, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return Scroll{}, fmt.Errorf("%w: a scroll offset is 0 or more pixels", ErrRequest)
		}
		return Scroll{Pixels: n}, nil
	}
	if len(s) > maxSelectorBytes {
		return Scroll{}, fmt.Errorf("%w: a selector is at most %d bytes", ErrRequest, maxSelectorBytes)
	}
	return Scroll{Selector: s}, nil
}

// String spells s as [ParseScroll] reads it.
func (s Scroll) String() string {
	switch {
	case s.Selector != "":
		return s.Selector
	case s.Bottom:
		return "bottom"
	case s.Pixels > 0:
		return strconv.Itoa(s.Pixels)
	}
	return "top"
}

// atRest reports a position a reader lands on rather than passes through: the
// top, or an element followed to. Content under a sticky bar there is hidden
// from the reader; elsewhere it is merely scrolling past.
func (s Scroll) atRest() bool { return s.Selector != "" || (!s.Bottom && s.Pixels == 0) }

// maxSelectorBytes bounds a selector an action or a scroll names.
const maxSelectorBytes = 512

// ActionKind is what one action does.
type ActionKind string

const (
	// ActionClick presses and releases the mouse at the element's centre,
	// as a reader's click lands: on whatever is on top there.
	ActionClick ActionKind = "click"
	// ActionHover moves the mouse to the element's centre.
	ActionHover ActionKind = "hover"
	// ActionPress focuses the element, when one is named, and presses a key.
	ActionPress ActionKind = "press"
)

// ActionKinds are every action kind.
var ActionKinds = []ActionKind{ActionClick, ActionHover, ActionPress}

// Action is one gesture played before a load's shots.
type Action struct {
	Kind     ActionKind
	Selector string
	// Key is a press's key: a name (`Enter`, `Tab`, `ArrowDown`, `Escape`)
	// or one character, with modifiers before it (`Shift+Tab`).
	Key string
}

// Viewer is who the page is told is viewing: display facts only.
type Viewer struct {
	Seat        string
	DisplayName string
	// Kind is `agent` or `human`.
	Kind string
	Role artifacts.Role
}

// Person is somebody a page that declared `identity` may name.
type Person struct {
	Seat        string
	DisplayName string
	Kind        string
}

// Bridge answers a page's data and state reads as the render's viewer, in
// the runtime contract's shapes. Every write is the render's own to refuse.
type Bridge interface {
	Read(ctx context.Context, method string, params map[string]any) (any, *Refusal)
}

// Refusal is a call refused, in the runtime contract's codes.
type Refusal struct {
	Code    string
	Message string
}

// Request is one render.
type Request struct {
	// Principal is who the page is decided as, on every document it loads.
	Principal iam.Principal

	// Admit decides Principal's read again, as its tool decided it (the
	// requester ceiling included), on every document request: Resolved to
	// serve, Anonymous to refuse, Unknown for a node that cannot tell.
	Admit func(ctx context.Context) iam.Resolution

	Viewer  Viewer
	People  []Person
	Head    artifactsdomain.Head
	Version artifactsdomain.Version

	// Page is the HTML page to open: empty for a page's entry, an
	// artboard's path for a design.
	Page string

	Widths  []int
	Themes  []Theme
	Scroll  []Scroll
	Actions []Action
	Bridge  Bridge
}

// shots is how many screenshots r asks for.
func (r Request) shots() int { return len(r.Widths) * len(r.Themes) * len(r.Scroll) }

// check holds r to the render's bounds, naming the one it is outside.
func (r Request) check() error {
	switch {
	case r.Admit == nil || r.Bridge == nil:
		return fmt.Errorf("%w: a render needs its admission and its bridge", ErrRequest)
	case len(r.Widths) == 0 || len(r.Themes) == 0 || len(r.Scroll) == 0:
		return fmt.Errorf("%w: a render takes at least one width, theme and scroll position",
			ErrRequest)
	case r.shots() > artifacts.RenderShots:
		return fmt.Errorf("%w: %d widths, %d themes and %d scroll positions are %d shots; "+
			"a render takes at most %d", ErrRequest, len(r.Widths), len(r.Themes),
			len(r.Scroll), r.shots(), artifacts.RenderShots)
	case len(r.Actions) > artifacts.RenderActions:
		return fmt.Errorf("%w: a render plays at most %d actions", ErrRequest,
			artifacts.RenderActions)
	}
	for _, w := range r.Widths {
		if w < artifacts.RenderMinWidth || w > artifacts.RenderMaxWidth {
			return fmt.Errorf("%w: width %d is outside %d..%d pixels", ErrRequest, w,
				artifacts.RenderMinWidth, artifacts.RenderMaxWidth)
		}
	}
	for _, t := range r.Themes {
		if !t.Valid() {
			return fmt.Errorf("%w: theme %q is light or dark", ErrRequest, t)
		}
	}
	for _, a := range r.Actions {
		switch {
		case !slices.Contains(ActionKinds, a.Kind):
			return fmt.Errorf("%w: an action is click, hover or press", ErrRequest)
		case a.Kind != ActionPress && strings.TrimSpace(a.Selector) == "":
			return fmt.Errorf("%w: a %s names the selector of its element", ErrRequest, a.Kind)
		case a.Kind == ActionPress && !validKey(a.Key):
			return fmt.Errorf("%w: %q is no key a press plays: a name such as Enter, Tab, "+
				"Escape or ArrowDown, or one character, after any of Shift+, Control+, Alt+ "+
				"and Meta+", ErrRequest, a.Key)
		case len(a.Selector) > maxSelectorBytes:
			return fmt.Errorf("%w: a selector is at most %d bytes", ErrRequest, maxSelectorBytes)
		}
	}
	return nil
}

// Config is what a node's renderer is built from.
type Config struct {
	// BrowserURL is Tier A's artifacts.render.browser_url.
	BrowserURL string

	// Client reaches the browser: its /json/version and its socket.
	Client *http.Client

	// Tickets and Origins are the node's own (engine.ArtifactServing): a
	// render's frame is minted and served as a viewer's is.
	Tickets *ticket.Codec
	Origins artifactserve.Origins

	// Dashboard is the API's external origin, the one an artifact's
	// frame-ancestors admit: the render's host page is served there.
	Dashboard string

	// Serve is the artifact origin's handler dependencies but its tickets,
	// origins and credentials, which the renderer sets: the same estate
	// router, object store, chart and embedded tree the origin serves from.
	Serve artifactserve.Deps

	// KitVersion is the newest kit the build embeds, whose surface colour
	// the host paints around the frame, as the dashboard does.
	KitVersion string

	// Contract is the runtime contract the host tells a page, the newest
	// the build serves, as the dashboard's host does.
	Contract string

	// Now is the clock; nil is UTC wall time.
	Now func() time.Time
}

// Renderer renders artifact versions on one node.
type Renderer struct {
	browser   string
	client    *http.Client
	frames    *artifactserve.Frames
	origins   artifactserve.Origins
	handler   *artifactserve.Handler
	assets    artifactserve.Artifacts
	lineages  *lineages
	dashboard string
	kit       string
	contract  string
	now       func() time.Time
	slots     chan struct{}
}

// New builds a node's renderer. A config with no browser builds one whose
// every render answers [ErrNoBrowser]: the tool names the key to set.
func New(c Config) (*Renderer, error) {
	now := c.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	switch {
	case c.Client == nil:
		return nil, errors.New("artifactrender: the renderer needs an HTTP client to reach its browser")
	case strings.TrimSpace(c.Dashboard) == "":
		return nil, errors.New("artifactrender: the renderer needs api.external_url, the origin " +
			"an artifact's frame is embedded from")
	}
	frames, err := artifactserve.NewFrames(c.Tickets, c.Origins, now)
	if err != nil {
		return nil, fmt.Errorf("artifactrender: %w", err)
	}
	l := newLineages()
	deps := c.Serve
	deps.Tickets, deps.Origins, deps.Credentials, deps.Now = c.Tickets, c.Origins, l, now
	// A RENDER IS NO VIEW: it is counted in neither the origin's requests
	// nor the view objective, which measure what readers wait for.
	deps.Metrics, deps.Objectives = nil, nil
	h, err := artifactserve.New(deps)
	if err != nil {
		return nil, fmt.Errorf("artifactrender: %w", err)
	}
	return &Renderer{
		browser: strings.TrimSpace(c.BrowserURL), client: c.Client, frames: frames,
		origins: c.Origins, handler: h, assets: c.Serve.Artifacts, lineages: l,
		dashboard: strings.TrimRight(strings.TrimSpace(c.Dashboard), "/"),
		kit:       c.KitVersion, contract: c.Contract, now: now,
		slots: make(chan struct{}, artifacts.RenderConcurrency),
	}, nil
}

// ArtifactsMoved forgets the render handler's verdicts about every artifact
// this node's applier reports moved, as the origin's handler does.
func (r *Renderer) ArtifactsMoved(moved []artifactsdomain.Moved) { r.handler.ArtifactsMoved(moved) }

// Configured reports whether this node names a render browser.
func (r *Renderer) Configured() bool { return r.browser != "" }

// Render renders one version of a framed artifact as req.Viewer sees it and
// reports what it found. Nothing is written; the page's writes are refused.
func (r *Renderer) Render(ctx context.Context, req Request) (Report, error) {
	if r.browser == "" {
		return Report{}, ErrNoBrowser
	}
	if err := req.check(); err != nil {
		return Report{}, err
	}
	if !req.Head.Kind.Framed() {
		return Report{}, fmt.Errorf("%w: a %s artifact is drawn by the dashboard from its "+
			"format, not served as a page", ErrRequest, req.Head.Kind)
	}
	release, err := r.acquire(ctx)
	if err != nil {
		return Report{}, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, artifacts.RenderTimeout)
	defer cancel()

	lineage := LineagePrefix + uuid.NewString()
	r.lineages.add(lineage, req.Principal, req.Admit)
	defer r.lineages.remove(lineage)
	frame, err := r.frames.For(artifactserve.FrameRequest{
		Principal: req.Principal, Lineage: lineage,
		// THE TICKET ENDS WITH THE RENDER, whose lineage is gone by then.
		Until: r.now().Add(artifacts.RenderTimeout), Head: req.Head, Version: req.Version,
		Document: req.Page, Granted: req.Version.Capabilities,
	})
	if err != nil {
		return Report{}, fmt.Errorf("%w: %w", ErrRequest, err)
	}
	run, err := r.open(ctx, req, frame)
	if err != nil {
		return Report{}, err
	}
	defer run.close(ctx)
	return run.render(ctx)
}

// acquire takes one of the node's render slots, waiting at most
// [artifacts.RenderQueueWait].
func (r *Renderer) acquire(ctx context.Context) (func(), error) {
	wait, cancel := context.WithTimeout(ctx, artifacts.RenderQueueWait)
	defer cancel()
	select {
	case r.slots <- struct{}{}:
		return func() { <-r.slots }, nil
	case <-wait.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrBusy
	}
}
