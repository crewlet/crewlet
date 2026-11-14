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

	"github.com/crewlet/crewlet/internal/browser/cdp"
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
// Each pinned Chromium's own description of its protocol, /json/protocol,
// holds every command and event of the inventory in its domain, and every
// field of its Go parameters and result, by JSON name and recursively through
// named types, with a JSON type the Go type reads and writes; and what a
// command sends — its parameters and every object they hold — has a field for
// each property the protocol requires, never omitempty and never written as
// null at its zero value, so no declared command is one its browser refuses to
// read. On the newest arm nothing declared is deprecated, so the client
// follows the protocol as it moves.
//
// Mutations: declare a parameter the protocol does not have
// (createBrowserContext's proxyServerX), type CookiePartitionKey as the
// string it once was, drop a parameter the protocol requires
// (Fetch.continueRequest's requestId), tag a required one omitempty
// (Target.setAutoAttach's autoAttach, or a CookieParam's value inside
// Storage.setCookies), or declare a required list a plain slice, whose nil is
// written as null (DOMSnapshot.captureSnapshot's computedStyles as []string),
// and the pins disagree — red.
func TestThePinsDescribeEveryMethodTheClientDeclares(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		ep := chromiumtest.LaunchPort(t, chrome)
		req, err := http.NewRequestWithContext(bounded(t), http.MethodGet, ep.HTTP+"/json/protocol", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := httpx.Client(0).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var desc protocol
		if err := json.NewDecoder(resp.Body).Decode(&desc); err != nil {
			t.Fatalf("/json/protocol: %v", err)
		}
		if len(desc.Domains) < 20 {
			t.Fatalf("/json/protocol described %d domains: it is describing nothing", len(desc.Domains))
		}
		for _, problem := range describes(desc, newest(t)) {
			t.Error(problem)
		}
	})
}

// THE FLOOR IS READ FROM THE BROWSER: CheckVersion passes each pin, and the
// floor's arm reads major MinMajor — the build the floor's measurement was
// made on is the build the protocol cases drive.
//
// Mutation: raise MinMajor by one, and CheckVersion refuses the floor's pin —
// red.
func TestTheFloorIsReadFromTheBrowser(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		c, _ := chromiumtest.LaunchPipe(t, chrome)
		v, err := cdp.CheckVersion(bounded(t), c.Browser())
		if err != nil {
			t.Fatal(err)
		}
		if !newest(t) && v.Major != cdp.MinMajor {
			t.Fatalf("the floor's pin says it is %s, major %d, and MinMajor is %d", v.Product, v.Major, cdp.MinMajor)
		}
		if v.Major < cdp.MinMajor {
			t.Fatalf("the newest pin says it is %s, below MinMajor %d", v.Product, cdp.MinMajor)
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
		c, _ := chromiumtest.LaunchPipe(t, chrome)
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

// A CLOSED TARGET FAILS ITS CALLS IN FLIGHT: closing a page
// (Target.closeTarget) while a call waits on it fails that call with a
// *DetachedError, never a hang to its deadline.
//
// Mutation: ignore every detach (Target.detachedFromTarget and
// Inspector.detached), and the call in flight hangs to the case's bound —
// red.
func TestAClosedTargetFailsItsCallsInFlight(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		ctx := bounded(t)
		c, _ := chromiumtest.LaunchPipe(t, chrome)
		page := openPage(ctx, t, c)
		waiting := inFlight(ctx, t, page)
		if _, err := cdp.Call(ctx, c.Browser(), cdp.TargetCloseTarget, cdp.TargetCloseTargetParams{
			TargetID: page.Target().TargetID,
		}); err != nil {
			t.Fatal(err)
		}
		var detached *cdp.DetachedError
		if err := <-waiting; !errors.As(err, &detached) || detached.SessionID != page.ID() {
			t.Fatalf("the call in flight on the closed page failed with %v, want a *DetachedError naming its session", err)
		}
	})
}

// A KILLED BROWSER FAILS EVERY CALL IN FLIGHT, on both transports: killing
// Chromium's whole process group fails a call waiting on one of its pages with
// ErrClosed, and ends the connection.
//
// Mutation: leave the calls in flight waiting when the reader ends, and they
// hang to the case's bound — red.
func TestAKilledBrowserFailsEveryCallInFlight(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		for _, transport := range []string{"pipe", "socket"} {
			t.Run(transport, func(t *testing.T) {
				ctx := bounded(t)
				var c *cdp.Conn
				var p *chromiumtest.Process
				if transport == "pipe" {
					c, p = chromiumtest.LaunchPipe(t, chrome)
				} else {
					ep := chromiumtest.LaunchPort(t, chrome)
					c, p = chromiumtest.Dial(t, ep), ep.Process
				}
				waiting := inFlight(ctx, t, openPage(ctx, t, c))
				if err := p.Kill(); err != nil {
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
	})
}

// launch is a Chromium of chrome's over transport, "pipe" or "socket".
func launch(t testing.TB, chrome, transport string) *cdp.Conn {
	t.Helper()
	if transport == "pipe" {
		c, _ := chromiumtest.LaunchPipe(t, chrome)
		return c
	}
	return chromiumtest.Dial(t, chromiumtest.LaunchPort(t, chrome))
}

// A SCREENSHOT-SIZED MESSAGE CROSSES BOTH TRANSPORTS: a 21 MiB answer, the
// size of the largest screenshot the browser asks for, arrives over the pipe
// and over the socket, and a 33 MiB one, past ReadLimit, fails its own call
// with ErrMessageTooBig and nothing else: the same page answers the next call,
// and the connection is open. On the newest arm alone: the floor adds nothing
// to a claim about this client's transports.
//
// Mutations: ReadLimit = 16 << 20, and the 21 MiB answer fails its call — red;
// end the connection at a message past ReadLimit, and the page's next call
// fails — red.
func TestAScreenshotSizedMessageCrossesBothTransports(t *testing.T) {
	chrome := chromiumtest.Newest(t)
	for _, transport := range []string{"pipe", "socket"} {
		t.Run(transport, func(t *testing.T) {
			ctx := bounded(t)
			c := launch(t, chrome, transport)
			page := openPage(ctx, t, c)
			got, err := evaluate(ctx, page, "'x'.repeat(21 << 20)")
			if err != nil {
				t.Fatalf("a 21 MiB answer: %v", err)
			}
			if len(got) != 21<<20+2 {
				t.Fatalf("a 21 MiB answer arrived as %d bytes", len(got))
			}
			if _, err := evaluate(ctx, page, "'x'.repeat(33 << 20)"); !errors.Is(err, cdp.ErrMessageTooBig) {
				t.Fatalf("a 33 MiB answer: %v, want ErrMessageTooBig", err)
			}
			if got, err := evaluate(ctx, page, "1 + 1"); err != nil || string(got) != "2" {
				t.Fatalf("the page whose answer passed ReadLimit then answered %s, %v", got, err)
			}
			if err := c.Err(); err != nil {
				t.Fatalf("the connection ended with %v", err)
			}
		})
	}
}

// A PAGE ENDS AT MOST ITS OWN SESSION WITH A MESSAGE PAST THE READ LIMIT, on
// both transports. Under Runtime.enable, which a binding needs, Chromium sends
// a page's console lines and calls of the session's bindings unasked, as large
// as the page makes them. A 40 MiB console line, which no listener can ask
// for, is dropped: the page's session goes on — a small call of the binding
// after it, routed after it, is delivered. A 40 MiB call of the binding, whose
// event a listener of that session asked for, ends that session alone with
// ErrMessageTooBig: its listener reports it, while the other tab answers and
// a new one opens on a connection still open. On the newest arm alone: the
// floor adds nothing to a claim about this client's reader.
//
// Mutations: end the connection at a message past ReadLimit, and the console
// line ends it — red; end the session of an event past ReadLimit nobody asked
// for, and the console line ends the page's — red; drop an event past
// ReadLimit a listener asked for, and the page's session goes on — red.
func TestAPageEndsAtMostItsOwnSessionWithAMessagePastTheReadLimit(t *testing.T) {
	chrome := chromiumtest.Newest(t)
	for _, transport := range []string{"pipe", "socket"} {
		t.Run(transport, func(t *testing.T) {
			ctx := bounded(t)
			c := launch(t, chrome, transport)
			page, other := openPage(ctx, t, c), openPage(ctx, t, c)
			calls := page.Listen(cdp.RuntimeBindingCalled)
			defer calls.Close()
			if _, err := cdp.Call(ctx, page, cdp.RuntimeEnable, struct{}{}); err != nil {
				t.Fatal(err)
			}
			if _, err := cdp.Call(ctx, page, cdp.RuntimeAddBinding, cdp.RuntimeAddBindingParams{Name: binding}); err != nil {
				t.Fatal(err)
			}

			if _, err := evaluate(ctx, page, "console.log('x'.repeat(40 << 20)); "+binding+"('after'); 1"); err != nil {
				t.Fatalf("the page that logged a 40 MiB line: %v", err)
			}
			m, err := calls.Next(ctx)
			if err != nil {
				t.Fatalf("after a 40 MiB console line the page's listener read %v", err)
			}
			if e, _, err := cdp.RuntimeBindingCalled.Of(m); err != nil || e.Payload != "after" {
				t.Fatalf("the call after the console line read as %+v, %v", e, err)
			}

			// Its answer may come before its event or after it: the end is
			// read off the listener, which reports it either way.
			_, _ = evaluate(ctx, page, binding+"('x'.repeat(40 << 20)); 1")
			if _, err := calls.Next(ctx); !errors.Is(err, cdp.ErrMessageTooBig) {
				t.Fatalf("after a 40 MiB call of the binding the listener read %v, want ErrMessageTooBig", err)
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
		})
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
// setup takes. A tab opened before the attacher, as a pool's is, opens a page
// served on one site, which holds a frame of another (an out-of-process frame,
// with a dedicated worker of its own), starts a dedicated worker, which starts
// one of its own, a shared worker and a service worker, and opens eight
// popups, four of the other site and four of its own: with window.open,
// keeping its opener and with noopener, and with a link's target=_blank, with
// no opener (a link's default) and with rel=opener. (A shared worker can start
// no worker: neither pin defines Worker in one.) Once the attacher is on, the
// case also opens a tab of its own on about:blank, and navigates it once its
// Setup's answers are in. AutoAttach on the browser gives every target a
// binding in its Setup, and every target's first statement calls the binding
// or, had it run before its setup, fetches /missed. The case waits for exactly
// sixteen reports, one a target, and every one is the binding's; and every
// target the case's pages and the case started was announced held. These are
// the kinds AutoAttach's doc says Chromium holds; the one new target it says
// Chromium does not, a tab created with an address, the case never opens.
//
// It runs twice on each pin: with a quick Setup, and with a slow one, every
// Setup but the opener's taking two seconds before it sends a command, as a
// Setup on a loaded machine may. A quick Setup can pass with a target
// released early, as long as its commands still arrive first; a slow one
// cannot, so the second run is the one that holds the claim whatever the
// Setup's speed.
//
// The filters are the ones a browser's attacher runs: the browser attaches
// pages and the service and shared workers, each child its frames and
// workers, so every target is held by one session. The opener is already
// running when the attacher reaches it, so the attacher never resumes it: a
// resume of the opener would release the page it is opening (AutoAttach's
// doc), and the case drives it from its Setup on. It is on about:blank, as a
// caller's tab never is when it opens a window, since the resume of a held tab
// sharing its renderer could release the page it opens (AutoAttach's doc):
// Chromium run as here gives each tab a renderer of its own. A popup with no
// opener has no renderer while it is held, and answers its Setup's commands
// only once resumed: every Setup sends them, waiting for none.
//
// Mutations: send waitForDebuggerOnStart false, and Chromium announces every
// target unheld, so the attacher resumes none, and the page the opener opens
// never runs — red at the case's bound; resume a child whether Chromium holds
// it or not, and in the slow run the opener's resume releases the page it is
// opening before its setup, and the page, its frame, the frame's worker, its
// dedicated worker, that worker's own and the page's first popup run first —
// red on both pins every time (TestAutoAttachNeverResumesATargetAlreadyRunning
// holds the attacher to it in a bubble); wait for a Setup's answers before the
// resume, and the four popups with no opener are never resumed — red at the
// case's bound; resume a held child before its Setup, and in either run
// targets run first — red on both pins; turn each child's own auto-attach on
// without the hold, and the page's dedicated worker, that worker's own and the
// frame's run before their setups — red on both pins. Resuming a child before
// Chromium has answered its own auto-attach stayed green here, two runs a pin,
// so TestAutoAttachResumesATargetOnlyAfterItsSetup holds that order.
func TestAutoAttachHoldsEveryNewTargetUntilItIsSetUp(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
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
// sends a command.
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

	c, _ := chromiumtest.LaunchPipe(t, chrome, "--site-per-process", "--disable-popup-blocking")
	opened, err := cdp.Call(ctx, c.Browser(), cdp.TargetCreateTarget, cdp.TargetCreateTargetParams{URL: "about:blank"})
	if err != nil {
		t.Fatal(err)
	}
	openers := make(chan *cdp.Session, 1)
	blanks := make(chan blank, 8)
	var mu sync.Mutex
	var arrivals []arrival
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
	// are in, as a caller opens its tabs (AutoAttach's doc).
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
		c, _ := chromiumtest.LaunchPipe(t, chrome)
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
// its Setup is told so. Two tabs opened before AutoAttach, as a pool's open
// tabs are, are announced with WaitingForDebugger false, and their Setups,
// which may wait on a target already running, read their documents' state at
// once. A tab opened once the attacher is on is announced held. (The
// browser's own first tab is either, by whether the attacher reached it
// before or after it started, and is not judged.)
//
// Mutation: hand Setup an announcement saying every target is held, and the
// running tabs read as held — red.
func TestATargetAlreadyRunningWhenAutoAttachReachesItArrivesUnheld(t *testing.T) {
	chromiumtest.Each(t, func(t *testing.T, chrome string) {
		ctx := bounded(t)
		c, _ := chromiumtest.LaunchPipe(t, chrome)
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
