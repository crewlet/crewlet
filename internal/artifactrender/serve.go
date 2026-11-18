package artifactrender

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/artifactrender/internal/cdp"
	"github.com/crewlet/crewlet/internal/artifacts"
)

// Every request of a render's tab is paused and answered here: the artifact
// origin's by the render's own handler with a browser's Fetch Metadata, the
// host's by the engine, anything else refused, so the browser needs no network.

// hostPath is the path the render's host page is served at on the dashboard's
// origin, and the kit it links below it. Never requested of the dashboard
// itself: the render answers it before it leaves the browser.
const hostPath = "/__crewlet_render__/"

// hostKit is the host page's path to the newest embedded kit.
const hostKit = hostPath + "kit/"

// errServedBound reports a render past [artifacts.RenderServedBytes].
var errServedBound = errors.New("artifactrender: the render served all the bytes it may")

// answer answers one paused request on the session that paused it.
func (u *run) answer(ctx context.Context, s *cdp.Session, ev cdp.FetchRequestPausedEvent) {
	u.begin()
	defer u.end()
	target, err := url.Parse(ev.Request.URL)
	host := ev.FrameID == u.mainFrame()
	switch {
	case err != nil:
		u.fail(ctx, s, ev, "the address cannot be read", host)
	case originOf(target) == u.r.dashboard && target.Path == hostPath:
		u.fulfill(ctx, s, ev, http.StatusOK, http.Header{
			"Content-Type":  {"text/html; charset=utf-8"},
			"Cache-Control": {"no-store"},
		}, u.hostPage())
	case originOf(target) == u.r.dashboard && strings.HasPrefix(target.Path, hostKit) && u.r.kit != "":
		// THE HOST'S OWN KIT, served as the origin serves it and fetched
		// as the host's own file, so its resource policy never stands
		// between the two origins.
		kit := *target
		kit.Scheme, kit.Host = originParts(u.frame.Origin)
		kit.Path = artifacts.KitPrefix + u.r.kit + "/" + strings.TrimPrefix(target.Path, hostKit)
		u.serve(ctx, s, ev, &kit, "style", true)
	case strings.EqualFold(originOf(target), u.frame.Origin):
		u.serve(ctx, s, ev, target, fetchDest(ev, host), host)
	default:
		u.fail(ctx, s, ev, "another origin: a viewer's browser asks that site for it, and a "+
			"render reaches no site but the engine", host)
	}
}

// serve answers a request through the render's artifact handler.
func (u *run) serve(ctx context.Context, s *cdp.Session, ev cdp.FetchRequestPausedEvent,
	target *url.URL, dest string, host bool) {

	method := ev.Request.Method
	req, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		u.fail(ctx, s, ev, "the address cannot be read", host)
		return
	}
	req.Host = target.Host
	for k, v := range ev.Request.Headers {
		// PLAIN BODIES: a fulfilled body is handed to the page as it is.
		if !strings.EqualFold(k, "Accept-Encoding") {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Sec-Fetch-Dest", dest)
	w := &recorder{header: http.Header{}, limit: u.budget()}
	u.r.handler.ServeHTTP(w, req)
	if w.over {
		u.fail(ctx, s, ev, "the render served all the bytes it may", host)
		u.stop(errServedBound)
		return
	}
	u.spend(int64(w.body.Len()))
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	if !host && status == http.StatusOK && method == http.MethodGet &&
		strings.HasPrefix(w.header.Get("Content-Type"), "text/css") {
		u.keepCSS(u.display(target), w.body.String())
	}
	header := w.header
	if host {
		header.Del("Cross-Origin-Resource-Policy")
	} else if status >= http.StatusBadRequest {
		u.found.requestFailed(RequestFailure{Method: method, URL: u.display(target),
			Resource: ev.ResourceType, Status: status, Reason: refusalReason(status)})
	}
	u.fulfill(ctx, s, ev, status, header, w.body.Bytes())
}

// fulfill answers a paused request with a response the engine made.
func (u *run) fulfill(ctx context.Context, s *cdp.Session, ev cdp.FetchRequestPausedEvent,
	status int, header http.Header, body []byte) {

	entries := make([]cdp.HeaderEntry, 0, len(header))
	for k, vs := range header {
		for _, v := range vs {
			entries = append(entries, cdp.HeaderEntry{Name: k, Value: v})
		}
	}
	if _, err := cdp.Call(ctx, s, cdp.FetchFulfillRequest, cdp.FetchFulfillRequestParams{
		RequestID: ev.RequestID, ResponseCode: int64(status), ResponseHeaders: entries, Body: body,
	}); err != nil && ctx.Err() == nil {
		log.Debug("artifact_render_unfulfilled", "error", err.Error())
	}
}

// fail refuses a paused request, reporting it unless the host asked.
func (u *run) fail(ctx context.Context, s *cdp.Session, ev cdp.FetchRequestPausedEvent,
	reason string, host bool) {

	if !host {
		target, _ := url.Parse(ev.Request.URL)
		shown := ev.Request.URL
		if target != nil {
			shown = u.display(target)
		}
		u.found.requestFailed(RequestFailure{Method: ev.Request.Method, URL: shown,
			Resource: ev.ResourceType, Reason: reason})
	}
	if _, err := cdp.Call(ctx, s, cdp.FetchFailRequest, cdp.FetchFailRequestParams{
		RequestID: ev.RequestID, ErrorReason: "BlockedByClient",
	}); err != nil && ctx.Err() == nil {
		log.Debug("artifact_render_unfailed", "error", err.Error())
	}
}

// display is an address as a report names it: a path of the version under
// the ticket, an origin path of the runtime or the kit, or another origin's
// address; never the ticket, which is a credential.
func (u *run) display(target *url.URL) string {
	if !strings.EqualFold(originOf(target), u.frame.Origin) {
		shown := target.Scheme + "://" + target.Host + target.EscapedPath()
		return clip(shown, maxShownBytes)
	}
	p := target.EscapedPath()
	if rest, ok := strings.CutPrefix(p, "/t/"); ok {
		if _, file, ok := strings.Cut(rest, "/"); ok {
			return clip(file, maxShownBytes)
		}
	}
	return clip(p, maxShownBytes)
}

// maxShownBytes bounds an address or a text a report quotes.
const maxShownBytes = 300

// fetchDest is the Sec-Fetch-Dest a browser sends for a request of this
// resource type: a document is the frame's, since the host's is never served
// by the artifact origin.
func fetchDest(ev cdp.FetchRequestPausedEvent, host bool) string {
	switch ev.ResourceType {
	case "Document":
		if host {
			return "document"
		}
		return "iframe"
	case "Stylesheet":
		return "style"
	case "Script":
		return "script"
	case "Image":
		return "image"
	case "Font":
		return "font"
	case "Media":
		return "video"
	case "TextTrack":
		return "track"
	case "Manifest":
		return "manifest"
	case "CSPViolationReport":
		return "report"
	}
	return "empty"
}

// refusalReason says what an artifact origin status means for a page.
func refusalReason(status int) string {
	switch status {
	case http.StatusNotFound:
		return "the version holds no such file, or the page may not open it"
	case http.StatusMethodNotAllowed:
		return "the artifact origin answers GET and HEAD alone"
	case http.StatusServiceUnavailable:
		return "the engine could not vouch for the artifact's rows; a viewer would be asked to retry"
	case http.StatusUnauthorized:
		return "the frame's ticket ended"
	}
	return "refused with " + strconv.Itoa(status)
}

// originOf is u's origin, as a browser spells it.
func originOf(u *url.URL) string { return strings.ToLower(u.Scheme + "://" + u.Host) }

// originParts splits an origin into its scheme and host.
func originParts(origin string) (string, string) {
	scheme, host, _ := strings.Cut(origin, "://")
	return scheme, host
}

// clip is s cut to at most n bytes on a character boundary, marked where cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// recorder is a response held in memory for a fulfil, bounded by what the
// render may still serve.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	limit  int64
	over   bool
}

func (w *recorder) Header() http.Header { return w.header }

func (w *recorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *recorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if int64(w.body.Len()+len(b)) > w.limit {
		w.over = true
		return 0, errServedBound
	}
	return w.body.Write(b)
}

// begin and end count a request in flight, which a shot waits to see none of
// for [artifacts.RenderQuiet].
func (u *run) begin() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.inflight++
	u.moved = time.Now()
}

func (u *run) end() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.inflight--
	u.moved = time.Now()
}

// budget is what the render may still serve, and spend charges n of it.
func (u *run) budget() int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return artifacts.RenderServedBytes - u.served
}

func (u *run) spend(n int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.served += n
}

// keepCSS keeps a stylesheet the page loaded for the custom property scan,
// within [maxCSSBytes].
func (u *run) keepCSS(path, text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.css[path]; ok || u.cssBytes+len(text) > maxCSSBytes {
		return
	}
	u.css[path] = text
	u.cssBytes += len(text)
}
