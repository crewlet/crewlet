package cdp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifactrender/internal/cdp"
	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/chromiumtest"
	"github.com/crewlet/crewlet/internal/httpx"
)

// These cases drive the two pinned Chromiums (chromiumtest), one at a time:
// none of them is parallel, since each Chromium takes a few hundred megabytes
// and a core beside the rest of a shard.

// bounded is the case's context under a liveness bound: everything a case
// waits on arrives in seconds when its claim holds, so the bound only turns a
// hang into a failure that says what never came.
func bounded(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// newest reports whether t runs on the newest pin's arm of chromiumtest.Each.
func newest(t testing.TB) bool { return strings.HasSuffix(t.Name(), "/newest") }

// THE PINS DESCRIBE EVERY METHOD THE CLIENT DECLARES — the floor's proof.
// Each pinned Chromium's own /json/protocol holds every command and event of
// the inventory in its domain, and every field of its Go parameters and result,
// by JSON name and recursively through named types, with a JSON type the Go
// type reads and writes; and what a command sends has a field for each
// property the protocol requires, never omitempty and never null at its zero
// value. On the newest arm nothing declared is deprecated.
//
// Mutations, each red: declare createBrowserContext's disposeOnDetachX; type
// RemoteObject's description as a number; drop Fetch.failRequest's requestId;
// tag Target.setAutoAttach's autoAttach or a MediaFeature's value omitempty;
// type Page.navigate's url as a *string, whose nil is written as null.
func TestThePinsDescribeEveryMethodTheClientDeclares(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		var desc protocol
		if err := json.Unmarshal(chromiumtest.Protocol(t, chrome).Raw, &desc); err != nil {
			t.Fatalf("/json/protocol: %v", err)
		}
		for _, problem := range describes(desc, newest(t)) {
			t.Error(problem)
		}
	})
}

// THE FLOOR IS READ FROM THE BROWSER: CheckVersion passes each pin, and the
// floor's arm reads major browser.MinChromiumMajor — the build the floor names
// is the build the floor's cases drive.
//
// Mutation: raise MinChromiumMajor by one, and CheckVersion refuses the
// floor's pin — red.
func TestTheFloorIsReadFromTheBrowser(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		c := launch(t, chrome)
		v, err := cdp.CheckVersion(bounded(t), c.Browser())
		if err != nil {
			t.Fatal(err)
		}
		floor := browser.MinChromiumMajor
		if !newest(t) && v.Major != floor {
			t.Fatalf("the floor's pin says it is %s, major %d, and MinChromiumMajor is %d", v.Product, v.Major, floor)
		}
		if v.Major < floor {
			t.Fatalf("the newest pin says it is %s, below MinChromiumMajor %d", v.Product, floor)
		}
	})
}

// openPage opens a tab on about:blank and attaches to it, flat.
func openPage(ctx context.Context, t testing.TB, c *cdp.Conn) *cdp.Session {
	t.Helper()
	created, err := cdp.Call(ctx, c.Browser(), cdp.TargetCreateTarget, cdp.TargetCreateTargetParams{URL: "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := cdp.Call(ctx, c.Browser(), cdp.TargetAttachToTarget, cdp.TargetAttachToTargetParams{
		TargetID: created.TargetID, Flatten: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c.Session(attached.SessionID)
}

// evaluate is the value of expression in s's page, by value.
func evaluate(ctx context.Context, s *cdp.Session, expression string) (json.RawMessage, error) {
	r, err := cdp.Call(ctx, s, cdp.RuntimeEvaluate, cdp.RuntimeEvaluateParams{Expression: expression, ReturnByValue: true})
	if err != nil {
		return nil, err
	}
	if r.ExceptionDetails != nil {
		return nil, fmt.Errorf("%s threw: %s", expression, r.ExceptionDetails.Text)
	}
	return r.Result.Value, nil
}

// inFlight starts a call on s that Chromium never answers — a promise that
// never settles, awaited — and returns once the page has started it, which the
// page says by a flag the call sets first.
func inFlight(ctx context.Context, t testing.TB, s *cdp.Session) <-chan error {
	t.Helper()
	ended := make(chan error, 1)
	go func() {
		_, err := cdp.Call(ctx, s, cdp.RuntimeEvaluate, cdp.RuntimeEvaluateParams{
			Expression:   "(async () => { window.cdpInFlight = true; await new Promise(() => {}); })()",
			AwaitPromise: true,
		})
		ended <- err
	}()
	// Each poll is a round trip to the page, so this waits for nothing but
	// the page itself.
	for {
		got, err := evaluate(ctx, s, "window.cdpInFlight === true")
		if err != nil {
			t.Fatalf("polling for the call in flight: %v", err)
		}
		if string(got) == "true" {
			return ended
		}
		select {
		case err := <-ended:
			t.Fatalf("the call meant to stay in flight ended: %v", err)
		default:
		}
	}
}

// A CRASHED PAGE FAILS ITS CALLS IN FLIGHT AND NO OTHER PAGE'S CALLS: a call
// waiting on a page whose renderer crashes (Page.crash) fails with a
// *CrashedError naming that page's target — Chromium itself would never
// answer it — later calls on it fail at once, and another page answers.
//
// Mutation: ignore Inspector.targetCrashed, and the call in flight hangs to
// the case's bound — red.
func TestACrashedPageFailsItsCallsInFlightAndNoOtherPagesCalls(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		ctx := bounded(t)
		c := launch(t, chrome)
		crashing, other := openPage(ctx, t, c), openPage(ctx, t, c)
		target := crashing.Target().TargetID
		if target == "" {
			t.Fatal("the attached page's target is unknown")
		}
		waiting := inFlight(ctx, t, crashing)
		go func() { _, _ = cdp.Call(ctx, crashing, cdp.PageCrash, struct{}{}) }()

		var crashed *cdp.CrashedError
		if err := <-waiting; !errors.As(err, &crashed) || crashed.TargetID != target {
			t.Fatalf("the call in flight on the crashed page failed with %v, want a *CrashedError naming %s", err, target)
		}
		if _, err := evaluate(ctx, crashing, "1"); !errors.As(err, &crashed) {
			t.Fatalf("a later call on the crashed page answered %v, want the *CrashedError", err)
		}
		if got, err := evaluate(ctx, other, "1 + 1"); err != nil || string(got) != "2" {
			t.Fatalf("the other page answered %s, %v", got, err)
		}
	})
}

// A CLOSED TARGET FAILS ITS CALLS IN FLIGHT: disposing of a browser context
// (Target.disposeBrowserContext), as a render does at its end, while a call
// waits on a page in it closes the page and fails that call with a
// *DetachedError, never a hang to its deadline.
//
// Mutation: ignore every detach (Target.detachedFromTarget and
// Inspector.detached), and the call in flight waits out its deadline — red.
func TestAClosedTargetFailsItsCallsInFlight(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		ctx := bounded(t)
		c := launch(t, chrome)
		bc, err := cdp.Call(ctx, c.Browser(), cdp.TargetCreateBrowserContext, cdp.TargetCreateBrowserContextParams{})
		if err != nil {
			t.Fatal(err)
		}
		contextID := bc.BrowserContextID
		created, err := cdp.Call(ctx, c.Browser(), cdp.TargetCreateTarget, cdp.TargetCreateTargetParams{
			URL: "about:blank", BrowserContextID: contextID,
		})
		if err != nil {
			t.Fatal(err)
		}
		attached, err := cdp.Call(ctx, c.Browser(), cdp.TargetAttachToTarget, cdp.TargetAttachToTargetParams{
			TargetID: created.TargetID, Flatten: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		page := c.Session(attached.SessionID)
		waiting := inFlight(ctx, t, page)
		if _, err := cdp.Call(ctx, c.Browser(), cdp.TargetDisposeBrowserContext, cdp.TargetDisposeBrowserContextParams{
			BrowserContextID: contextID,
		}); err != nil {
			t.Fatal(err)
		}
		var detached *cdp.DetachedError
		if err := <-waiting; !errors.As(err, &detached) || detached.SessionID != page.ID() {
			t.Fatalf("the call in flight on the closed page failed with %v, want a *DetachedError naming its session", err)
		}
	})
}

// A KILLED BROWSER FAILS EVERY CALL IN FLIGHT: killing Chromium's whole
// process group fails a call waiting on one of its pages with ErrClosed, and
// ends the connection.
//
// Mutation: leave the calls in flight waiting when the reader ends, and they
// hang to the case's bound — red.
func TestAKilledBrowserFailsEveryCallInFlight(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		ctx := bounded(t)
		ep := chromiumtest.LaunchPort(t, chrome)
		c := dial(t, ep)
		waiting := inFlight(ctx, t, openPage(ctx, t, c))
		if err := ep.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		if err := <-waiting; !errors.Is(err, cdp.ErrClosed) {
			t.Fatalf("the call in flight failed with %v, want ErrClosed", err)
		}
		select {
		case <-c.Done():
		case <-ctx.Done():
			t.Fatal("the connection did not end with its browser")
		}
	})
}

// dial dials ep's browser websocket over the engine's own transport, as the
// render dials the Chromium an operator runs, and closes it when t ends.
func dial(t testing.TB, ep chromiumtest.Endpoint) *cdp.Conn {
	t.Helper()
	c, err := cdp.Dial(t.Context(), ep.Browser, cdp.DialOptions{HTTPClient: httpx.Client(0)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// launch is a connection to a Chromium of chrome's, launched with args.
func launch(t testing.TB, chrome string, args ...string) *cdp.Conn {
	t.Helper()
	return dial(t, chromiumtest.LaunchPort(t, chrome, args...))
}

// A SCREENSHOT-SIZED MESSAGE CROSSES THE SOCKET: a 9 MiB answer, the largest
// screenshot the render asks for once base64 has carried it, arrives, and one
// a mebibyte past ReadLimit fails its own call with ErrMessageTooBig and
// nothing else: the same page answers the next call, and the connection is
// open. On the newest arm alone: the floor adds nothing to a claim about this
// client's transport.
//
// Mutations: ReadLimit = 8 << 20, and the 9 MiB answer fails its call — red;
// end the connection at a message past ReadLimit, and the page's next call
// fails — red.
func TestAScreenshotSizedMessageCrossesTheSocket(t *testing.T) {
	ctx := bounded(t)
	c := launch(t, chromiumtest.Newest(t))
	page := openPage(ctx, t, c)
	got, err := evaluate(ctx, page, "'x'.repeat(9 << 20)")
	if err != nil {
		t.Fatalf("a 9 MiB answer: %v", err)
	}
	if len(got) != 9<<20+2 {
		t.Fatalf("a 9 MiB answer arrived as %d bytes", len(got))
	}
	past := fmt.Sprintf("'x'.repeat(%d)", cdp.ReadLimit+1<<20)
	if _, err := evaluate(ctx, page, past); !errors.Is(err, cdp.ErrMessageTooBig) {
		t.Fatalf("an answer a mebibyte past ReadLimit: %v, want ErrMessageTooBig", err)
	}
	if got, err := evaluate(ctx, page, "1 + 1"); err != nil || string(got) != "2" {
		t.Fatalf("the page whose answer passed ReadLimit then answered %s, %v", got, err)
	}
	if err := c.Err(); err != nil {
		t.Fatalf("the connection ended with %v", err)
	}
}

// A PAGE ENDS AT MOST ITS OWN SESSION WITH A MESSAGE PAST THE READ LIMIT. Under
// Runtime.enable, which a binding needs, Chromium sends a page's console lines
// and calls of its bindings unasked, as large as the page makes them. A console
// line past ReadLimit, which no listener asked for, is dropped: a small call of
// the binding routed after it is delivered. A call of the binding past it,
// which a listener asked for, ends that session alone with ErrMessageTooBig,
// while the other tab answers and a new one opens. On the newest arm alone:
// the floor adds nothing to a claim about this client's reader.
//
// Mutations, each red: end the connection at a message past ReadLimit; end the
// session of an event past it nobody asked for; drop an event past it a
// listener asked for.
func TestAPageEndsAtMostItsOwnSessionWithAMessagePastTheReadLimit(t *testing.T) {
	ctx := bounded(t)
	c := launch(t, chromiumtest.Newest(t))
	page, other := openPage(ctx, t, c), openPage(ctx, t, c)
	calls := page.Listen(cdp.RuntimeBindingCalled)
	defer calls.Close()
	if _, err := cdp.Call(ctx, page, cdp.RuntimeEnable, struct{}{}); err != nil {
		t.Fatal(err)
	}
	if _, err := cdp.Call(ctx, page, cdp.RuntimeAddBinding, cdp.RuntimeAddBindingParams{Name: binding}); err != nil {
		t.Fatal(err)
	}
	past := fmt.Sprintf("'x'.repeat(%d)", cdp.ReadLimit+1<<20)

	if _, err := evaluate(ctx, page, "console.log("+past+"); "+binding+"('after'); 1"); err != nil {
		t.Fatalf("the page that logged a line past ReadLimit: %v", err)
	}
	m, err := calls.Next(ctx)
	if err != nil {
		t.Fatalf("after a console line past ReadLimit the page's listener read %v", err)
	}
	if e, _, err := cdp.RuntimeBindingCalled.Of(m); err != nil || e.Payload != "after" {
		t.Fatalf("the call after the console line read as %+v, %v", e, err)
	}

	// Its answer may come before its event or after it: the end is read off
	// the listener, which reports it either way.
	_, _ = evaluate(ctx, page, binding+"("+past+"); 1")
	if _, err := calls.Next(ctx); !errors.Is(err, cdp.ErrMessageTooBig) {
		t.Fatalf("after a call of the binding past ReadLimit the listener read %v, want ErrMessageTooBig", err)
	}
	if err := page.Err(); !errors.Is(err, cdp.ErrMessageTooBig) {
		t.Fatalf("the page's session ended with %v, want ErrMessageTooBig", err)
	}
	if got, err := evaluate(ctx, other, "1 + 1"); err != nil || string(got) != "2" {
		t.Fatalf("the other tab answered %s, %v", got, err)
	}
	if got, err := evaluate(ctx, openPage(ctx, t, c), "1 + 1"); err != nil || string(got) != "2" {
		t.Fatalf("a tab opened after answered %s, %v", got, err)
	}
	if err := c.Err(); err != nil {
		t.Fatalf("the connection ended with %v", err)
	}
}

// binding is the function each held target is given before it runs, which its
// first statement calls.
const binding = "cdpTestReport"

// report is the first statement of every page, frame and worker the case
// serves: the binding, if the target was set up before it ran, or a request
// for /missed if it was not.
const report = `function report(name) {
  if (typeof self.` + binding + ` === 'function') self.` + binding + `(name);
  else fetch('/missed?' + name);
}
`

// site serves the case's pages, frames and workers on loopback, and hands
// each /missed it is asked for to missed.
func site(t testing.TB, pages map[string]string, missed chan<- string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missed" {
			missed <- "missed:" + r.URL.RawQuery
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		kind := "text/html"
		if strings.HasSuffix(r.URL.Path, ".js") {
			kind = "text/javascript"
		}
		w.Header().Set("Content-Type", kind)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// AUTO-ATTACH HOLDS EVERY NEW TARGET UNTIL IT IS SET UP, however long its
// setup takes: every kind the package doc says Chromium holds — a page opened
// from a running tab, its out-of-process frame, its workers, a worker's own,
// eight popups opened four ways from two sites, and a tab the case opens on
// about:blank — calls the binding its Setup gave it, never fetching /missed,
// and was announced held. On each pin twice, the second time with every Setup
// but the opener's taking two seconds, as on a loaded machine.
//
// Mutations, each red on both pins: send waitForDebuggerOnStart false; resume
// a child whether Chromium holds it or not; wait for a Setup's answers before
// the resume; resume a held child before its Setup; turn a child's own
// auto-attach on without the hold.
func TestAutoAttachHoldsEveryNewTargetUntilItIsSetUp(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		// A quick Setup can pass with a target released early, as long as its
		// commands still arrive first; a slow one cannot, so the second run
		// holds the claim whatever the Setup's speed.
		for _, run := range []struct {
			name       string
			setupTakes time.Duration
		}{
			{"a quick setup", 0},
			{"a slow setup", 2 * time.Second},
		} {
			t.Run(run.name, func(t *testing.T) { holdsEveryNewTarget(t, chrome, run.setupTakes) })
		}
	})
}

// arrival is what a Setup was told of one target.
type arrival struct {
	target cdp.TargetID
	kind   string
	url    string
	opener cdp.TargetID
	held   bool
}

// blank is a page announced on about:blank with no opener, as the case's own
// tab is, with what its Setup sent.
type blank struct {
	target          cdp.TargetID
	session         *cdp.Session
	enable, binding *cdp.Pending[struct{}]
}

// holdsEveryNewTarget is TestAutoAttachHoldsEveryNewTargetUntilItIsSetUp's
// run on chrome, every Setup but the opener's taking setupTakes before it
// sends a command. Resuming a child before Chromium has answered its own
// auto-attach stays green here, two runs a pin, so
// TestAutoAttachResumesATargetOnlyAfterItsSetup holds that order.
func holdsEveryNewTarget(t *testing.T, chrome string, setupTakes time.Duration) {
	ctx := bounded(t)
	reports := make(chan string, 64)
	named := report + "report(new URL(location).searchParams.get('name'));"
	popup := "<!doctype html><script>" + named + "</script>"
	other := site(t, map[string]string{
		"/frame.html": "<!doctype html><script>" + report +
			"report('frame'); new Worker('/worker.js?name=frame-dedicated');</script>",
		"/popup.html": popup,
		"/worker.js":  named,
	}, reports)
	// The same loopback server by another name: another site.
	otherSite := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	// A shared worker starts no worker: neither pin defines Worker in one.
	page := site(t, map[string]string{
		// The frame first, so that its hold runs beside the popups': each
		// window.open that keeps its opener holds the page's script until
		// its popup is released.
		"/page.html": `<!doctype html><iframe src="` + otherSite + `/frame.html"></iframe><script>` + report +
			`report('page');
new Worker('/nesting.js?name=dedicated');
new SharedWorker('/worker.js?name=shared');
navigator.serviceWorker.register('/service.js');
for (const [origin, prefix] of [['` + otherSite + `', ''], ['', 'same-site-']]) {
  window.open(origin + '/popup.html?name=' + prefix + 'popup');
  window.open(origin + '/popup.html?name=' + prefix + 'popup-without-opener', '_blank', 'noopener');
  for (const [name, rel] of [['link-popup', ''], ['link-popup-with-opener', 'opener']]) {
    const a = document.createElement('a');
    a.href = origin + '/popup.html?name=' + prefix + name;
    a.target = '_blank';
    a.rel = rel;
    document.body.appendChild(a);
    a.click();
  }
}
</script>`,
		"/popup.html": popup,
		"/worker.js":  named,
		"/nesting.js": named + "\nnew Worker('/worker.js?name=nested-dedicated');\n",
		"/service.js": report + "report('service');\n",
	}, reports)

	c := launch(t, chrome, "--site-per-process", "--disable-popup-blocking")
	opened, err := cdp.Call(ctx, c.Browser(), cdp.TargetCreateTarget, cdp.TargetCreateTargetParams{URL: "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	openers := make(chan *cdp.Session, 1)
	blanks := make(chan blank, 8)
	var mu sync.Mutex
	var arrivals []arrival
	// The browser attaches pages and the service and shared workers, each
	// child its frames and workers, so every target is held by one session.
	// The opener runs before the attacher reaches it, so the attacher never
	// resumes it, which would release the page it is opening, and the case
	// drives it from its Setup on. It is on about:blank, as a caller's tab
	// never is when it opens a window, which holds here only because Chromium
	// run so gives each tab a renderer of its own (the package doc). A popup
	// with no opener answers its Setup's commands only once resumed, so every
	// Setup sends them, waiting for none.
	a, err := cdp.AutoAttach(ctx, c.Browser(), cdp.AttachOptions{
		Filter: []cdp.TargetFilterEntry{
			{Type: "page"}, {Type: "shared_worker"}, {Type: "service_worker"}, {Exclude: true},
		},
		ChildFilter: []cdp.TargetFilterEntry{{Type: "iframe"}, {Type: "worker"}, {Exclude: true}},
		Setup: func(ctx context.Context, child *cdp.Session, attached cdp.TargetAttachedToTargetEvent) ([]cdp.Answer, error) {
			info := attached.TargetInfo
			mu.Lock()
			arrivals = append(arrivals, arrival{info.TargetID, info.Type, info.URL, info.OpenerID, attached.WaitingForDebugger})
			mu.Unlock()
			if info.TargetID == opened.TargetID {
				openers <- child
				return nil, nil
			}
			if setupTakes > 0 {
				select {
				case <-time.After(setupTakes): //fixedwait:simulated a Setup a loaded machine slows, so that a target released before it runs first
				case <-ctx.Done():
					return nil, context.Cause(ctx)
				}
			}
			l := child.Listen(cdp.RuntimeBindingCalled)
			go func() {
				defer l.Close()
				for {
					m, err := l.Next(ctx)
					if err != nil {
						return
					}
					if e, _, err := cdp.RuntimeBindingCalled.Of(m); err == nil && e.Name == binding {
						reports <- "binding:" + e.Payload
					}
				}
			}()
			// Enabled first, so the binding follows a document into the
			// process it commits in (RuntimeAddBinding's doc).
			enable := cdp.Send(ctx, child, cdp.RuntimeEnable, struct{}{})
			bind := cdp.Send(ctx, child, cdp.RuntimeAddBinding, cdp.RuntimeAddBindingParams{Name: binding})
			if info.Type == "page" && info.URL == "about:blank" && info.OpenerID == "" {
				blanks <- blank{info.TargetID, child, enable, bind}
			}
			return []cdp.Answer{enable, bind}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	// saw is what the Setups were told so far, read under the lock and
	// reported after it: a failure that held it would stop a Setup still
	// recording, and the attacher's Close in cleanup would wait on it for
	// ever.
	saw := func() []arrival {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(arrivals)
	}

	var opener *cdp.Session
	select {
	case opener = <-openers:
	case <-ctx.Done():
		t.Fatalf("the opener was never attached; the Setups were told of %+v (Err %v)", saw(), a.Err())
	}
	// window.open returns to the opener only once the page it opens is
	// released, so it is called beside the wait for the reports.
	opening := make(chan error, 1)
	go func() {
		_, err := evaluate(ctx, opener, "window.open('"+page.URL+"/page.html'), true")
		opening <- err
	}()
	// The case's own tab, on about:blank, navigated once its Setup's answers
	// are in, as a caller opens its tabs (the package doc).
	created, err := cdp.Call(ctx, c.Browser(), cdp.TargetCreateTarget, cdp.TargetCreateTargetParams{URL: "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	navigating := make(chan error, 1)
	go func() {
		for {
			select {
			case b := <-blanks:
				if b.target != created.TargetID {
					continue // the browser's own first tab
				}
				if _, err := b.enable.Wait(ctx); err != nil {
					navigating <- err
					return
				}
				if _, err := b.binding.Wait(ctx); err != nil {
					navigating <- err
					return
				}
				_, err := cdp.Call(ctx, b.session, cdp.PageNavigate, cdp.PageNavigateParams{URL: otherSite + "/popup.html?name=created-tab"})
				navigating <- err
				return
			case <-ctx.Done():
				navigating <- fmt.Errorf("the case's own tab was never set up: %w", context.Cause(ctx))
				return
			}
		}
	}()

	want := []string{"binding:created-tab", "binding:dedicated", "binding:frame", "binding:frame-dedicated",
		"binding:link-popup", "binding:link-popup-with-opener", "binding:nested-dedicated", "binding:page",
		"binding:popup", "binding:popup-without-opener", "binding:same-site-link-popup",
		"binding:same-site-link-popup-with-opener", "binding:same-site-popup",
		"binding:same-site-popup-without-opener", "binding:service", "binding:shared"}
	var got []string
	for len(got) < len(want) {
		select {
		case r := <-reports:
			got = append(got, r)
		case <-ctx.Done():
			t.Fatalf("%d of %d targets reported (%q) before the bound; the Setups were told of %+v (Err %v)",
				len(got), len(want), got, saw(), a.Err())
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("the targets reported %q, want %q: one that fetched /missed ran before its setup", got, want)
	}
	if err := <-opening; err != nil {
		t.Fatalf("the opener's window.open: %v", err)
	}
	if err := <-navigating; err != nil {
		t.Fatalf("navigating the case's own tab: %v", err)
	}
	// Every target the case's pages started — one of the two sites', or a
	// popup, which names its opener — and the case's own tab.
	for _, got := range saw() {
		own := strings.HasPrefix(got.url, page.URL) || strings.HasPrefix(got.url, otherSite) ||
			got.opener != "" || got.target == created.TargetID
		if own && !got.held {
			t.Errorf("%+v, which the case started, was announced unheld", got)
		}
	}
	if err := a.Err(); err != nil {
		t.Fatalf("the attacher failed: %v", err)
	}
}

// THE ZERO ATTACH OPTIONS HOLD EACH WORKER ONCE. Chromium announces a
// service worker at the browser's level and again at every page it serves, and
// one held by two sessions is set up by each and starts only once both have
// resumed it. AutoAttach on the browser with no filter — Chromium's own default
// for the browser's session, and for each child that default without a
// service worker — holds the service worker a page registers, and the shared
// worker it starts, by the browser's session alone: each is announced once,
// its Setup's commands are answered, and once resumed its first statement
// calls the binding its Setup gave it.
//
// Mutation: send a nil ChildFilter as it is, Chromium's own default, and the
// page's session holds the service worker too: it is announced twice — red.
func TestTheZeroAttachOptionsHoldEachWorkerOnce(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		ctx := bounded(t)
		reports := make(chan string, 16)
		srv := site(t, map[string]string{
			"/page.html": "<!doctype html><script>new SharedWorker('/shared.js');\n" +
				"navigator.serviceWorker.register('/service.js');</script>",
			"/shared.js":  report + "report('shared');\n",
			"/service.js": report + "report('service');\n",
		}, reports)
		c := launch(t, chrome)
		var mu sync.Mutex
		announced := map[string]int{} // each target of the site's, by type and address
		firstTab := make(chan *cdp.Session, 1)
		a, err := cdp.AutoAttach(ctx, c.Browser(), cdp.AttachOptions{
			Setup: func(ctx context.Context, child *cdp.Session, attached cdp.TargetAttachedToTargetEvent) ([]cdp.Answer, error) {
				info := attached.TargetInfo
				if info.Type == "page" && info.URL == "about:blank" {
					select {
					case firstTab <- child:
					default:
					}
					return nil, nil
				}
				// The browser's own targets — its interface, its
				// extensions' workers — are resumed as they come.
				if !strings.HasPrefix(info.URL, srv.URL) {
					return nil, nil
				}
				mu.Lock()
				announced[info.Type+" "+info.URL]++
				mu.Unlock()
				l := child.Listen(cdp.RuntimeBindingCalled)
				go func() {
					defer l.Close()
					for {
						m, err := l.Next(ctx)
						if err != nil {
							return
						}
						if e, _, err := cdp.RuntimeBindingCalled.Of(m); err == nil && e.Name == binding {
							reports <- "binding:" + e.Payload
						}
					}
				}()
				return []cdp.Answer{
					cdp.Send(ctx, child, cdp.RuntimeEnable, struct{}{}),
					cdp.Send(ctx, child, cdp.RuntimeAddBinding, cdp.RuntimeAddBindingParams{Name: binding}),
				}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		var tab *cdp.Session
		select {
		case tab = <-firstTab:
		case <-ctx.Done():
			t.Fatalf("the first tab was never attached (the attacher's Err is %v)", a.Err())
		}
		if _, err := cdp.Call(ctx, tab, cdp.PageNavigate, cdp.PageNavigateParams{URL: srv.URL + "/page.html"}); err != nil {
			t.Fatal(err)
		}
		var got []string
		for len(got) < 2 {
			select {
			case r := <-reports:
				got = append(got, r)
			case <-ctx.Done():
				// Read under the lock and reported after it, as the hold
				// case does.
				mu.Lock()
				saw := maps.Clone(announced)
				mu.Unlock()
				t.Fatalf("%d of the two workers reported (%q) before the bound; the site's targets were announced %v "+
					"(the attacher's Err is %v)", len(got), got, saw, a.Err())
			}
		}
		slices.Sort(got)
		if want := []string{"binding:service", "binding:shared"}; !slices.Equal(got, want) {
			t.Fatalf("the workers reported %q, want %q: one that fetched /missed ran before its setup", got, want)
		}
		mu.Lock()
		defer mu.Unlock()
		for target, n := range announced {
			if n != 1 {
				t.Errorf("%s was announced %d times, want once", target, n)
			}
		}
		if len(announced) != 2 {
			t.Errorf("the site's targets announced were %v, want its two workers", announced)
		}
		if err := a.Err(); err != nil {
			t.Fatalf("the attacher failed: %v", err)
		}
	})
}

// A TARGET ALREADY RUNNING WHEN AUTO-ATTACH REACHES IT ARRIVES UNHELD, and
// its Setup is told so. Two tabs opened before AutoAttach are announced with
// WaitingForDebugger false, and their Setups, which may wait on a target
// already running, read their documents' state at once. A tab opened once the
// attacher is on is announced held. (The browser's own first tab is either,
// by whether the attacher reached it before or after it started, and is not
// judged.)
//
// Mutation: hand Setup an announcement saying every target is held, and the
// running tabs read as held — red.
func TestATargetAlreadyRunningWhenAutoAttachReachesItArrivesUnheld(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		ctx := bounded(t)
		c := launch(t, chrome)
		running := map[cdp.TargetID]bool{}
		for range 2 {
			before, err := cdp.Call(ctx, c.Browser(), cdp.TargetCreateTarget, cdp.TargetCreateTargetParams{URL: "about:blank"})
			if err != nil {
				t.Fatal(err)
			}
			running[before.TargetID] = true
		}
		type arrival struct {
			target cdp.TargetID
			held   bool
			state  string // document.readyState, as an unheld target's Setup read it
		}
		arrivals := make(chan arrival, 8)
		a, err := cdp.AutoAttach(ctx, c.Browser(), cdp.AttachOptions{
			Filter: []cdp.TargetFilterEntry{{Type: "page"}, {Exclude: true}},
			Setup: func(ctx context.Context, child *cdp.Session, attached cdp.TargetAttachedToTargetEvent) ([]cdp.Answer, error) {
				got := arrival{target: attached.TargetInfo.TargetID, held: attached.WaitingForDebugger}
				// A running target answers at once, so its Setup may wait on
				// it (AttachOptions.Setup).
				if !got.held {
					state, err := evaluate(ctx, child, "document.readyState")
					if err != nil {
						return nil, err
					}
					got.state = string(state)
				}
				arrivals <- got
				return nil, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		next := func() arrival {
			t.Helper()
			select {
			case got := <-arrivals:
				return got
			case <-ctx.Done():
				t.Fatalf("a tab was never set up (the attacher's Err is %v)", a.Err())
			}
			return arrival{}
		}

		for judged := 0; judged < len(running); {
			got := next()
			if !running[got.target] {
				continue // the browser's own first tab
			}
			judged++
			if got.held || got.state == "" {
				t.Errorf("tab %s, running before the attacher, arrived as %+v: want unheld, its document's state read in its Setup",
					got.target, got)
			}
		}

		after, err := cdp.Call(ctx, c.Browser(), cdp.TargetCreateTarget, cdp.TargetCreateTargetParams{URL: "about:blank"})
		if err != nil {
			t.Fatal(err)
		}
		for {
			got := next()
			if got.target != after.TargetID {
				continue // the browser's own first tab, if it came late
			}
			if !got.held {
				t.Errorf("the tab opened once the attacher was on arrived as %+v, want held", got)
			}
			break
		}
		if err := a.Err(); err != nil {
			t.Fatalf("the attacher failed: %v", err)
		}
	})
}
