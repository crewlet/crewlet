package artifactrender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/artifactrender/internal/cdp"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
)

// run is one render in progress: its tab, the requests it is answering, and
// what it has found.
type run struct {
	r     *Renderer
	req   Request
	frame artifacts.Frame
	conn  *cdp.Conn
	tab   *tab
	found *collector

	attacher *cdp.Attacher
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	mu        sync.Mutex
	inflight  int
	moved     time.Time
	served    int64
	stopped   error
	load      int
	theme     Theme
	hello     chan struct{}
	helloSeen bool
	// frames are the out-of-process frames' sessions, by frame, with the
	// load each was attached in.
	frames map[cdp.FrameID]framed
	// contexts are the page session's execution contexts, by id, to the
	// frame each belongs to, so the host's own console lines are told
	// from the page's.
	contexts map[cdp.ExecutionContextID]cdp.FrameID
	// worlds are the check world made in each frame for a load.
	worlds map[worldKey]cdp.ExecutionContextID
	// css is the text of every stylesheet served to the page, by the path
	// a report names it by.
	css      map[string]string
	cssBytes int
	pngBytes int

	subscriptions int
}

type framed struct {
	session *cdp.Session
	load    int
}

type worldKey struct {
	frame cdp.FrameID
	load  int
}

// maxCSSBytes bounds the stylesheet text a render keeps for its scan: the kit
// and a page's own sheets are a fraction of it.
const maxCSSBytes = 8 << 20

// pollEvery is how often a render asks whether its frame has loaded.
const pollEvery = 100 * time.Millisecond

// open dials the browser, opens the render's tab and sets it up: every
// request paused, the host's script and binding in place, every frame and
// worker held until it is set up the same way.
func (r *Renderer) open(ctx context.Context, req Request, frame artifacts.Frame) (*run, error) {
	conn, err := r.dial(ctx)
	if err != nil {
		return nil, err
	}
	t, err := openTab(ctx, conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	rctx, cancel := context.WithCancel(ctx)
	u := &run{r: r, req: req, frame: frame, conn: conn, tab: t, found: &collector{},
		cancel: cancel, moved: time.Now(), frames: map[cdp.FrameID]framed{},
		contexts: map[cdp.ExecutionContextID]cdp.FrameID{}, worlds: map[worldKey]cdp.ExecutionContextID{},
		css: map[string]string{}, theme: req.Themes[0], hello: make(chan struct{})}
	if err := u.setup(rctx); err != nil {
		u.close(ctx)
		return nil, fmt.Errorf("%w: set up its tab: %w", ErrBrowser, err)
	}
	return u, nil
}

// setup prepares the page session before anything loads in it.
func (u *run) setup(ctx context.Context) error {
	p := u.tab.page
	u.watch(ctx, p, true)
	steps := []func() error{
		func() error {
			_, err := cdp.Call(ctx, p, cdp.FetchEnable, cdp.FetchEnableParams{
				Patterns: []cdp.RequestPattern{{URLPattern: "*"}}})
			return err
		},
		func() error { _, err := cdp.Call(ctx, p, cdp.RuntimeEnable, struct{}{}); return err },
		func() error { _, err := cdp.Call(ctx, p, cdp.LogEnable, struct{}{}); return err },
		func() error { _, err := cdp.Call(ctx, p, cdp.PageEnable, struct{}{}); return err },
		func() error {
			_, err := cdp.Call(ctx, p, cdp.EmulationSetFocusEmulationEnabled,
				cdp.EmulationSetFocusEmulationEnabledParams{Enabled: true})
			return err
		},
		func() error {
			_, err := cdp.Call(ctx, p, cdp.RuntimeAddBinding, cdp.RuntimeAddBindingParams{
				Name: hostBinding, ExecutionContextName: hostWorld})
			return err
		},
		func() error {
			_, err := cdp.Call(ctx, p, cdp.PageAddScriptToEvaluateOnNewDocument,
				cdp.PageAddScriptToEvaluateOnNewDocumentParams{Source: u.hostScript(), WorldName: hostWorld})
			return err
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	a, err := cdp.AutoAttach(ctx, p, cdp.AttachOptions{
		Filter: []cdp.TargetFilterEntry{{Type: "browser", Exclude: true}, {Type: "tab", Exclude: true}, {}},
		Setup:  u.setupChild,
	})
	if err != nil {
		return err
	}
	u.attacher = a
	return nil
}

// setupChild prepares a frame or a worker before it runs: its requests
// paused, its console reported, a frame's media the page's.
func (u *run) setupChild(ctx context.Context, child *cdp.Session,
	ev cdp.TargetAttachedToTargetEvent) ([]cdp.Answer, error) {

	u.watch(ctx, child, false)
	answers := []cdp.Answer{
		cdp.Send(ctx, child, cdp.FetchEnable, cdp.FetchEnableParams{
			Patterns: []cdp.RequestPattern{{URLPattern: "*"}}}),
		cdp.Send(ctx, child, cdp.RuntimeEnable, struct{}{}),
	}
	if ev.TargetInfo.Type == "iframe" {
		answers = append(answers,
			cdp.Send(ctx, child, cdp.LogEnable, struct{}{}),
			cdp.Send(ctx, child, cdp.EmulationSetEmulatedMedia, u.media()))
		u.mu.Lock()
		u.frames[cdp.FrameID(ev.TargetInfo.TargetID)] = framed{session: child, load: u.load}
		u.mu.Unlock()
	}
	return answers, nil
}

// watch reads one session's events for the render's life.
func (u *run) watch(ctx context.Context, s *cdp.Session, page bool) {
	l := s.Listen(cdp.FetchRequestPaused, cdp.RuntimeConsoleAPICalled, cdp.RuntimeExceptionThrown,
		cdp.LogEntryAdded, cdp.RuntimeExecutionContextCreated, cdp.RuntimeBindingCalled)
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		defer l.Close()
		for {
			m, err := l.Next(ctx)
			if err != nil {
				return
			}
			u.event(ctx, s, page, m)
		}
	}()
}

// event handles one event of a session.
func (u *run) event(ctx context.Context, s *cdp.Session, page bool, m cdp.Message) {
	if ev, ok, err := cdp.FetchRequestPaused.Of(m); ok && err == nil {
		u.wg.Add(1)
		go func() {
			defer u.wg.Done()
			u.answer(ctx, s, ev)
		}()
		return
	}
	if ev, ok, err := cdp.RuntimeBindingCalled.Of(m); ok && err == nil && page {
		u.wg.Add(1)
		go func() {
			defer u.wg.Done()
			u.hosted(ctx, ev)
		}()
		return
	}
	if ev, ok, err := cdp.RuntimeExecutionContextCreated.Of(m); ok && err == nil && page {
		var aux struct {
			FrameID cdp.FrameID `json:"frameId"`
		}
		_ = json.Unmarshal(ev.Context.AuxData, &aux)
		u.mu.Lock()
		u.contexts[ev.Context.ID] = aux.FrameID
		u.mu.Unlock()
		return
	}
	if ev, ok, err := cdp.RuntimeConsoleAPICalled.Of(m); ok && err == nil {
		level := consoleLevel(ev.Type)
		if level == "" || (page && u.hostContext(ev.ExecutionContextID)) {
			return
		}
		var parts []string
		for _, a := range ev.Args {
			parts = append(parts, remoteText(a))
		}
		src, line := u.where(ev.StackTrace, "", 0)
		u.found.console(ConsoleLine{Level: level, Text: clip(strings.Join(parts, " "), maxShownBytes),
			Source: src, Line: line})
		return
	}
	if ev, ok, err := cdp.RuntimeExceptionThrown.Of(m); ok && err == nil {
		d := ev.ExceptionDetails
		if page && u.hostContext(d.ExecutionContextID) {
			return
		}
		text := d.Text
		if d.Exception != nil && d.Exception.Description != "" {
			text = d.Exception.Description
		}
		text, _, _ = strings.Cut(text, "\n    at ")
		src, line := u.where(d.StackTrace, d.URL, d.LineNumber)
		u.found.console(ConsoleLine{Level: "error", Text: clip("uncaught: "+text, maxShownBytes),
			Source: src, Line: line})
		return
	}
	if ev, ok, err := cdp.LogEntryAdded.Of(m); ok && err == nil {
		e := ev.Entry
		// A REFUSED REQUEST is reported as the request, with why, by the
		// render's own answer to it. A WARNING ABOUT A RESPONSE HEADER is
		// about one the engine sets on every page, never the page's own.
		if (e.Level != "error" && e.Level != "warning") || e.Source == "network" ||
			(e.Level == "warning" && strings.Contains(strings.ToLower(e.Text), " header")) {
			return
		}
		src := ""
		if e.URL != "" {
			target, err := url.Parse(e.URL)
			if err != nil || originOf(target) == u.r.dashboard {
				return
			}
			src = u.display(target)
		}
		u.found.console(ConsoleLine{Level: e.Level, Text: clip(e.Text, maxShownBytes), Source: src,
			Line: int(e.LineNumber)})
	}
}

// hostContext reports a page session context that is the host document's.
func (u *run) hostContext(id cdp.ExecutionContextID) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	f, ok := u.contexts[id]
	return ok && f == u.mainFrame()
}

// where is the source line of a stack's innermost frame on the page, or the
// fallback where it has none.
func (u *run) where(st *cdp.StackTrace, fallback string, line int64) (string, int) {
	if st != nil && len(st.CallFrames) > 0 {
		fallback, line = st.CallFrames[0].URL, st.CallFrames[0].LineNumber
	}
	if fallback == "" {
		return "", 0
	}
	target, err := url.Parse(fallback)
	if err != nil {
		return "", 0
	}
	return u.display(target), int(line) + 1
}

// consoleLevel is the level a console method is reported at, empty for one
// that is not reported: errors, warnings and failed assertions alone.
func consoleLevel(kind string) string {
	switch kind {
	case "error", "assert":
		return "error"
	case "warning":
		return "warning"
	}
	return ""
}

// remoteText is a console argument as text.
func remoteText(o cdp.RemoteObject) string {
	if o.Type == "string" {
		var s string
		if json.Unmarshal(o.Value, &s) == nil {
			return s
		}
	}
	if o.Description != "" {
		return o.Description
	}
	if len(o.Value) > 0 {
		return string(o.Value)
	}
	return o.UnserializableValue
}

// mainFrame is the tab's own frame: the host's.
func (u *run) mainFrame() cdp.FrameID { return cdp.FrameID(u.tab.target) }

// helloed records the page's hello on this load.
func (u *run) helloed() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.helloSeen {
		u.helloSeen = true
		close(u.hello)
	}
}

// stop ends the render early, for cause.
func (u *run) stop(cause error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.stopped == nil {
		u.stopped = cause
	}
}

func (u *run) halted() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.stopped
}

// loadTheme is the theme of the load in progress.
func (u *run) loadTheme() Theme {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.theme
}

// media is the media the load in progress emulates: the reader's theme as
// the system's, so a page's own prefers-color-scheme rules agree with it.
func (u *run) media() cdp.EmulationSetEmulatedMediaParams {
	return cdp.EmulationSetEmulatedMediaParams{Features: []cdp.MediaFeature{
		{Name: "prefers-color-scheme", Value: string(u.loadTheme())}}}
}

// close ends the run: its goroutines, its tab and its connection.
func (u *run) close(ctx context.Context) {
	if u.attacher != nil {
		u.attacher.Close()
	}
	u.cancel()
	u.tab.close(ctx)
	_ = u.conn.Close()
	u.wg.Wait()
}

// render takes every load and shot the request asks for, and reports. A
// failure past the first load stops the render with what it has.
func (u *run) render(ctx context.Context) (Report, error) {
	rep := Report{Page: u.frame.Document, Mode: u.frame.Mode}
	err := u.take(ctx, &rep)
	switch {
	case err != nil && len(rep.Shots) == 0 && rep.Unresolved == nil:
		return Report{}, err
	case errors.Is(err, context.DeadlineExceeded):
		rep.Stopped = "the render passed its " + artifacts.RenderTimeout.String() + " bound"
	case err != nil:
		rep.Stopped = err.Error()
	}
	return u.finish(rep), nil
}

// take takes the loads and shots into rep, and answers what stopped it early.
func (u *run) take(ctx context.Context, rep *Report) error {
	var uses []barUse
	first := true
	for _, theme := range u.req.Themes {
		for _, width := range u.req.Widths {
			if err := u.halted(); err != nil {
				return err
			}
			if err := u.loadPage(ctx, theme, width); err != nil {
				return err
			}
			if first {
				uses = u.wholePage(ctx, rep)
			}
			u.play(ctx, rep, first)
			for _, s := range u.req.Scroll {
				rep.Shots = append(rep.Shots, u.shoot(ctx, theme, width, s, uses))
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			first = false
		}
	}
	return nil
}

// finish adds the collected findings to rep.
func (u *run) finish(rep Report) Report {
	c := u.found
	c.mu.Lock()
	defer c.mu.Unlock()
	rep.Console, rep.Requests, rep.Bridge = c.lines.list(), c.requests.list(), c.bridge.list()
	rep.Covered, rep.SeeThrough = c.covered.list(), c.bars.list()
	omit := func(kind string, n int) {
		if n > 0 {
			if rep.Omitted == nil {
				rep.Omitted = map[string]int{}
			}
			rep.Omitted[kind] += n
		}
	}
	omit("console", c.lines.omitted)
	omit("requests", c.requests.omitted)
	omit("bridge", c.bridge.omitted)
	omit("covered", c.covered.omitted)
	omit("see_through", c.bars.omitted)
	u.mu.Lock()
	rep.Runtime = u.helloSeen
	u.mu.Unlock()
	return rep
}

// loadPage loads the host and its frame in theme at width, and waits for the
// page to load and its requests to settle.
func (u *run) loadPage(ctx context.Context, theme Theme, width int) error {
	u.mu.Lock()
	u.load++
	u.theme = theme
	u.hello = make(chan struct{})
	u.helloSeen = false
	sessions := []*cdp.Session{u.tab.page}
	for _, f := range u.frames {
		sessions = append(sessions, f.session)
	}
	u.mu.Unlock()
	height := artifacts.RenderDesktopHeight
	phone := width < artifacts.RenderPhoneBelow
	if phone {
		height = artifacts.RenderPhoneHeight
	}
	p := u.tab.page
	if _, err := cdp.Call(ctx, p, cdp.EmulationSetDeviceMetricsOverride,
		cdp.EmulationSetDeviceMetricsOverrideParams{Width: int64(width), Height: int64(height),
			DeviceScaleFactor: 1, Mobile: phone}); err != nil {
		return fmt.Errorf("%w: size the tab: %w", ErrBrowser, err)
	}
	for _, s := range sessions {
		if _, err := cdp.Call(ctx, s, cdp.EmulationSetEmulatedMedia, u.media()); err != nil &&
			s == p {
			return fmt.Errorf("%w: set the theme: %w", ErrBrowser, err)
		}
	}
	nav, err := cdp.Call(ctx, p, cdp.PageNavigate, cdp.PageNavigateParams{URL: u.r.dashboard + hostPath})
	switch {
	case err != nil:
		return fmt.Errorf("%w: open the host page: %w", ErrBrowser, err)
	case nav.ErrorText != "":
		return fmt.Errorf("%w: open the host page: %s", ErrBrowser, nav.ErrorText)
	}
	load, cancel := context.WithTimeout(ctx, artifacts.RenderLoadTimeout)
	defer cancel()
	if err := u.loaded(load); err != nil {
		return err
	}
	if u.req.Version.Contract != "" {
		select {
		case <-u.helloCh():
		case <-load.Done():
			// The page loaded and its runtime never spoke: reported by
			// Report.Runtime, and the shots go on.
		}
	}
	u.quiet(ctx)
	return nil
}

func (u *run) helloCh() chan struct{} {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hello
}

// loaded waits for the frame's document to load.
func (u *run) loaded(ctx context.Context) error {
	for {
		var got loaded
		if s, f, err := u.artifactFrame(ctx); err == nil {
			if err := u.eval(ctx, s, f, loadedJS, &got); err == nil && got.Loaded {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			reason := "it did not load within " + artifacts.RenderLoadTimeout.String()
			if failed := u.documentFailed(); failed != "" {
				reason = failed
			}
			return fmt.Errorf("%w: the page %s: %s", ErrRequest, u.frame.Document, reason)
		case <-time.After(pollEvery):
		}
	}
}

// documentFailed is why the page's own document was refused, where it was.
func (u *run) documentFailed() string {
	c := u.found
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, f := range c.requests.list() {
		if f.Resource == "Document" {
			return fmt.Sprintf("its document %s was answered %d (%s)", f.URL, f.Status, f.Reason)
		}
	}
	return ""
}

// quiet waits until no request has been in flight for [artifacts.RenderQuiet],
// for at most [artifacts.RenderSettleTimeout].
func (u *run) quiet(ctx context.Context) {
	deadline := time.Now().Add(artifacts.RenderSettleTimeout)
	for time.Now().Before(deadline) {
		u.mu.Lock()
		idle := u.inflight == 0 && time.Since(u.moved) >= artifacts.RenderQuiet
		u.mu.Unlock()
		if idle {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollEvery / 2):
		}
	}
}

// artifactFrame is the session and the frame the page's document is in: an
// out-of-process frame of this load, or the host's child frame.
func (u *run) artifactFrame(ctx context.Context) (*cdp.Session, cdp.FrameID, error) {
	u.mu.Lock()
	for id, f := range u.frames {
		if f.load == u.load {
			if f.session.Err() == nil {
				u.mu.Unlock()
				return f.session, id, nil
			}
		}
	}
	u.mu.Unlock()
	tree, err := cdp.Call(ctx, u.tab.page, cdp.PageGetFrameTree, struct{}{})
	if err != nil {
		return nil, "", err
	}
	for _, child := range tree.FrameTree.ChildFrames {
		target, err := url.Parse(child.Frame.URL)
		if err == nil && strings.EqualFold(originOf(target), u.frame.Origin) {
			return u.tab.page, child.Frame.ID, nil
		}
	}
	return nil, "", errors.New("the frame has not committed")
}

// eval runs a check in the frame's check world and decodes its value.
func (u *run) eval(ctx context.Context, s *cdp.Session, f cdp.FrameID, expr string, out any) error {
	key := worldKey{frame: f, load: u.loadNumber()}
	for attempt := 0; attempt < 2; attempt++ {
		u.mu.Lock()
		id, ok := u.worlds[key]
		u.mu.Unlock()
		if !ok {
			w, err := cdp.Call(ctx, s, cdp.PageCreateIsolatedWorld,
				cdp.PageCreateIsolatedWorldParams{FrameID: f, WorldName: checkWorld})
			if err != nil {
				return err
			}
			id = w.ExecutionContextID
			u.mu.Lock()
			u.worlds[key] = id
			u.mu.Unlock()
		}
		res, err := cdp.Call(ctx, s, cdp.RuntimeEvaluate, cdp.RuntimeEvaluateParams{
			Expression: expr, ContextID: id, ReturnByValue: true, AwaitPromise: true})
		if err != nil {
			// A world a navigation of the frame took is made again.
			u.mu.Lock()
			delete(u.worlds, key)
			u.mu.Unlock()
			continue
		}
		if res.ExceptionDetails != nil {
			return fmt.Errorf("the check threw: %s", res.ExceptionDetails.Text)
		}
		return json.Unmarshal(res.Result.Value, out)
	}
	return errors.New("the frame's check world could not be reached")
}

func (u *run) loadNumber() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.load
}

// frameEval runs a check in the artifact's frame as it stands.
func (u *run) frameEval(ctx context.Context, expr string, out any) error {
	s, f, err := u.artifactFrame(ctx)
	if err != nil {
		return err
	}
	return u.eval(ctx, s, f, expr, out)
}

// wholePage runs the checks that read the page once: its custom properties,
// its links and where its in-page links land. It answers the unresolved
// backgrounds a see-through bar is told by.
func (u *run) wholePage(ctx context.Context, rep *Report) []barUse {
	var st styles
	_ = u.frameEval(ctx, stylesJS, &st)
	u.mu.Lock()
	sheets := make([]sheet, 0, len(u.css)+len(st.Blocks)+len(st.Attrs))
	paths := make([]string, 0, len(u.css))
	for p := range u.css {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		sheets = append(sheets, sheet{source: p, text: u.css[p]})
	}
	u.mu.Unlock()
	for _, b := range st.Blocks {
		sheets = append(sheets, sheet{source: "<style>", text: b})
	}
	for _, a := range st.Attrs {
		sheets = append(sheets, sheet{source: `style=""`, text: a, inline: true})
	}
	scanned := scanSheets(sheets)
	var uses []barUse
	unresolved := pageOwn(scanned.unresolved())
	var selectors []string
	for _, un := range unresolved {
		for _, us := range un.uses {
			if us.selector != "" && !slices.Contains(selectors, us.selector) {
				selectors = append(selectors, us.selector)
			}
		}
	}
	var matches []match
	if len(selectors) > 0 {
		_ = u.frameEval(ctx, matchesJS(selectors), &matches)
	}
	counted := func(sel string) (int, string) {
		i := slices.Index(selectors, sel)
		if i < 0 || i >= len(matches) {
			return 0, ""
		}
		return matches[i].Count, matches[i].Example
	}
	for i, un := range unresolved {
		if i == artifacts.RenderFindings {
			if rep.Omitted == nil {
				rep.Omitted = map[string]int{}
			}
			rep.Omitted["unresolved"] = len(unresolved) - i
			break
		}
		out := Unresolved{Property: un.name, Fallback: un.fallback, Near: scanned.near(un.name)}
		for j, us := range un.uses {
			n, ex := counted(us.selector)
			if n > 0 {
				out.Elements += n
				if out.Example == "" {
					out.Example = ex
				}
			}
			if j < maxUsesShown {
				out.Uses = append(out.Uses, Use{Source: us.source, Line: us.line,
					Selector: clip(us.selector, 120), Property: us.property})
			}
			if !us.fallback && strings.HasPrefix(us.property, "background") && us.selector != "" {
				uses = append(uses, barUse{Selector: us.selector, Name: un.name})
			}
		}
		rep.Unresolved = append(rep.Unresolved, out)
	}
	u.links(ctx, rep)
	var landings []landing
	if err := u.frameEval(ctx, anchorsJS, &landings); err == nil {
		for _, l := range landings {
			u.found.coveredBy(Covered{Element: l.Element, By: l.By, Theme: u.loadTheme(),
				Width: u.req.Widths[0], Scroll: "link to " + l.Element})
		}
	}
	return uses
}

// pageOwn is the unresolved names the page's own styles use: the kit's and
// the brand's sheets count as definitions, but what they reference (the
// brand's tokens, values the kit's scripts set) is theirs.
func pageOwn(all []unresolvedUse) []unresolvedUse {
	out := all[:0]
	for _, un := range all {
		own := un.uses[:0:0]
		for _, us := range un.uses {
			if !strings.HasPrefix(us.source, "/_crewlet/") {
				own = append(own, us)
			}
		}
		if len(own) == 0 {
			continue
		}
		un.uses = own
		un.fallback = !slices.ContainsFunc(own, func(us use) bool { return !us.fallback })
		out = append(out, un)
	}
	return out
}

// maxUsesShown bounds the uses one unresolved property lists.
const maxUsesShown = 5

// links reports the page's links that go nowhere a viewer can follow.
func (u *run) links(ctx context.Context, rep *Report) {
	var all []link
	if err := u.frameEval(ctx, linksJS, &all); err != nil {
		return
	}
	seen := map[string]int{}
	for _, l := range all {
		problem := u.linkProblem(l)
		if problem == "" {
			continue
		}
		key := l.Href + "\x00" + problem
		if i, ok := seen[key]; ok {
			rep.Links[i].Count++
			continue
		}
		if len(rep.Links) == artifacts.RenderFindings {
			if rep.Omitted == nil {
				rep.Omitted = map[string]int{}
			}
			rep.Omitted["links"]++
			continue
		}
		seen[key] = len(rep.Links)
		rep.Links = append(rep.Links, Link{Href: clip(l.Href, 200), Text: l.Text, Problem: problem,
			Count: 1})
	}
}

// linkProblem is what is wrong with following l in the frame, or empty.
func (u *run) linkProblem(l link) string {
	href := strings.TrimSpace(l.Href)
	lower := strings.ToLower(href)
	switch {
	case href == "" || href == "#":
		return ""
	case strings.HasPrefix(href, "#"):
		if l.Target != nil && !*l.Target {
			return "no element has the id " + clip(href[1:], 80)
		}
		return ""
	case strings.HasPrefix(lower, "javascript:"):
		return "runs nothing: the page's policy refuses a javascript: link"
	case strings.HasPrefix(lower, "mailto:"), strings.HasPrefix(lower, "tel:"):
		return ""
	}
	target, err := url.Parse(l.Resolved)
	if err != nil {
		return "the address cannot be read"
	}
	if !strings.EqualFold(originOf(target), u.frame.Origin) {
		if l.Blank {
			return "opens nothing: the frame may not open a window; call crewlet.openLink(url)"
		}
		return "replaces the page inside its frame with another site; call crewlet.openLink(url)"
	}
	rest, ok := strings.CutPrefix(target.EscapedPath(), "/t/")
	if !ok {
		return ""
	}
	_, file, _ := strings.Cut(rest, "/")
	file, err = url.PathUnescape(file)
	if err != nil || file == "" || strings.HasPrefix(file, manifest.ReservedAssetsPrefix) {
		return ""
	}
	if _, ok := manifest.EntryAt(u.req.Version.Manifest, file); !ok {
		return "the version holds no " + clip(file, 120)
	}
	return ""
}

// play plays the request's actions on the load in progress, reporting them
// from the first load alone.
func (u *run) play(ctx context.Context, rep *Report, report bool) {
	for _, a := range u.req.Actions {
		res := u.act(ctx, a)
		if report {
			rep.Actions = append(rep.Actions, res)
		}
		u.quiet(ctx)
	}
}

// act plays one action.
func (u *run) act(ctx context.Context, a Action) ActionResult {
	res := ActionResult{Action: a}
	p := u.tab.page
	switch a.Kind {
	case ActionClick, ActionHover:
		var pt point
		if err := u.frameEval(ctx, pointJS(a.Selector), &pt); err != nil {
			res.Detail = "the page could not be read: " + err.Error()
			return res
		}
		switch {
		case pt.Invalid:
			res.Detail = "the selector is not one the browser reads"
			return res
		case !pt.Found:
			res.Detail = "no element matches it"
			return res
		case pt.Hidden:
			res.Detail = pt.Element + " has no size, so nothing can reach it"
			return res
		}
		if !pt.On {
			res.Hit = pt.Hit
		}
		events := []string{"mouseMoved"}
		if a.Kind == ActionClick {
			events = append(events, "mousePressed", "mouseReleased")
		}
		for _, kind := range events {
			e := cdp.InputDispatchMouseEventParams{Type: kind, X: pt.X, Y: pt.Y}
			if kind != "mouseMoved" {
				e.Button, e.Buttons, e.ClickCount = "left", 1, 1
			}
			if _, err := cdp.Call(ctx, p, cdp.InputDispatchMouseEvent, e); err != nil {
				res.Detail = "the browser refused the pointer: " + err.Error()
				return res
			}
		}
		res.Done = true
		if res.Hit != "" {
			res.Detail = "it landed on " + res.Hit + ", which is on top of " + pt.Element
		}
	case ActionPress:
		if a.Selector != "" {
			var focused bool
			if err := u.frameEval(ctx, focusJS(a.Selector), &focused); err != nil || !focused {
				res.Detail = "the element could not take the focus"
				return res
			}
		}
		for _, e := range keyEvents(a.Key) {
			if _, err := cdp.Call(ctx, p, cdp.InputDispatchKeyEvent, e); err != nil {
				res.Detail = "the browser refused the key: " + err.Error()
				return res
			}
		}
		res.Done = true
	}
	return res
}

// shoot takes one screenshot at scroll s, with what the page shows there.
func (u *run) shoot(ctx context.Context, theme Theme, width int, s Scroll, uses []barUse) Shot {
	shot := Shot{Width: width, Height: artifacts.RenderDesktopHeight, Theme: theme, Scroll: s.String()}
	if width < artifacts.RenderPhoneBelow {
		shot.Height = artifacts.RenderPhoneHeight
	}
	var moved scrolled
	if err := u.frameEval(ctx, scrollJS(s), &moved); err != nil {
		shot.Failed = "the page could not be scrolled: " + err.Error()
		return shot
	}
	if moved.Missing {
		shot.Failed = "no element matches " + clip(s.Selector, 120)
		return shot
	}
	u.quiet(ctx)
	var settled loaded
	_ = u.frameEval(ctx, loadedJS, &settled)
	var at where
	if err := u.frameEval(ctx, shotJS(s.atRest(), uses), &at); err == nil {
		shot.ScrollY, shot.ScrollHeight = at.ScrollY, at.ScrollHeight
		for _, b := range at.SeeThrough {
			u.found.seeThrough(Bar{Element: b.Element, Position: b.Position, Theme: theme,
				Width: width, Unresolved: b.Unresolved})
		}
		for _, c := range at.Covered {
			u.found.coveredBy(Covered{Element: c.Element, By: c.By, Theme: theme, Width: width,
				Scroll: s.String()})
		}
	}
	img, err := cdp.Call(ctx, u.tab.page, cdp.PageCaptureScreenshot, cdp.PageCaptureScreenshotParams{
		Format: "png"})
	switch {
	case err != nil:
		shot.Failed = "the browser took no screenshot: " + err.Error()
	case len(img.Data) > int(artifacts.RenderShotBytes):
		shot.TooLarge = true
	default:
		u.mu.Lock()
		over := u.pngBytes+len(img.Data) > int(artifacts.RenderBoxBytes)
		if !over {
			u.pngBytes += len(img.Data)
		}
		u.mu.Unlock()
		if over {
			shot.Failed = "the render's screenshots passed their " +
				strconv.FormatInt(artifacts.RenderBoxBytes>>20, 10) + " MiB bound"
		} else {
			shot.PNG = img.Data
		}
	}
	return shot
}
