package mcpbridge

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/browser"
)

// A JOB'S BROWSER: the screen of the browser a job's box runs, which the box's
// relay posts to and holds one poll on ([BrowserPattern]) behind the run's own
// token, routed as every route under the prefix is. The bridge bounds each
// body, holds the poll and answers what the job's screen says, and decides
// nothing about it: internal/browser/screen's package doc has the argument.

// Browser is a job's browser screen as its bridge session serves it. Declared
// here, by the consumer; internal/browser/screen's hub implements it.
type Browser interface {
	// Serving reports whether the screen serves: false while the job's
	// launch has not readied its browser at or above the floor, when every
	// verb is answered as for a session with no browser.
	Serving() bool

	// Frame, State and Checkin take one post of the relay's, its body read
	// whole within its bound by read, which the screen calls only once it
	// has charged the post: nil for one taken or dropped as stale, a
	// *browser.Invalid or *browser.TooFast for one refused, read's own error
	// for a body it could not read, and browser.ErrScreenClosed once the
	// screen has closed.
	Frame(ctx context.Context, meta string, read func() ([]byte, error)) error
	State(ctx context.Context, read func() ([]byte, error)) error
	Checkin(ctx context.Context, read func() ([]byte, error)) error

	// Poll answers the relay's poll q: the answer as it stands, and nil where
	// it is answered now, else a channel closed when it moves, which
	// [Browser.Answer] then answers; browser.ErrScreenClosed once closed.
	Poll(ctx context.Context, q browser.PollQuery) (browser.PollAnswer, <-chan struct{}, error)
	Answer() (browser.PollAnswer, error)

	// Close ends the screen with its opening: its held poll is answered and
	// every later call answers browser.ErrScreenClosed.
	Close()
}

// BrowserPattern is the browser routes' mux pattern, under the bridge's own
// prefix so every gate that covers the bridge covers it by prefix, as
// [SteerPattern] is.
const BrowserPattern = PathPrefix + "{token}/browser/{verb}"

// browserSuffix joins a job's endpoint to its browser routes.
const browserSuffix = "/browser"

// BrowserURL is the browser routes of the job whose endpoint is given: what
// the engine hands the job's relay. "" for none.
func BrowserURL(endpoint string) string {
	if endpoint == "" {
		return ""
	}
	return endpoint + browserSuffix
}

// The verbs the browser routes serve, each on its one method.
const (
	verbFrame   = "frame"
	verbState   = "state"
	verbCheckin = "checkin"
	verbPoll    = "poll"
)

// BrowserHandler serves the browser routes ([BrowserPattern]): the same
// routing as [Bridge.Handler], with the poll held as a notes poll is.
func (b *Bridge) BrowserHandler() http.Handler { return b.routed(b.serveBrowser, isBrowserPoll) }

// isBrowserPoll reports whether r is a poll of the browser routes, the one
// call of them that waits for something to happen.
func isBrowserPoll(r *http.Request) bool {
	return r.Method == http.MethodGet && r.PathValue("verb") == verbPoll
}

// serveBrowser answers one call of the browser routes on the opening that
// holds it, its token judged by the routing ([Bridge.routed]) or the relay's
// admission, as every route's is.
//
// A SESSION WITH NO BROWSER, OR ONE NOT YET SERVING, IS 404 `not_found` on
// every verb, on which the relay stops: the opening serves no browser. A CALL
// REACHING ITS OPENING AS IT ENDS IS TURNED AWAY TO ASK AGAIN, a 503 with a
// Retry-After, never that 404, as a notes call is (package doc, "A note
// inbox"). A VERB THIS BUILD DOES NOT SERVE is the mux's own 404 `no_route`,
// which the relay asks again, so a relay newer than this node keeps its job's
// screen once a node of its build serves the seat.
func (b *Bridge) serveBrowser(w http.ResponseWriter, r *http.Request, opened *binding) {
	if opened.session.Browser == nil {
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	}
	screen := opened.browserOf()
	if screen == nil {
		httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(askAgainRetry))
		return
	}
	if !screen.Serving() {
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	}
	switch verb := r.PathValue("verb"); {
	case verb == verbFrame && r.Method == http.MethodPost:
		postFrame(w, r, screen)
	case verb == verbState && r.Method == http.MethodPost:
		postReport(w, r, screen.State, browser.MaxStateBytes)
	case verb == verbCheckin && r.Method == http.MethodPost:
		postReport(w, r, screen.Checkin, browser.MaxCheckinBytes)
	case verb == verbPoll && r.Method == http.MethodGet:
		b.pollBrowser(w, r, screen)
	case verb == verbFrame || verb == verbState || verb == verbCheckin || verb == verbPoll:
		httpjson.Fail(w, http.StatusMethodNotAllowed, httpjson.CodeMethodNotAllowed)
	default:
		httpjson.NoRoute(w, r)
	}
}

// postFrame hands the screen one frame: its body declared a JPEG and read
// within [browser.MaxFrameBytes] once the screen has charged it, and its
// metadata as its header carries it, which the screen reads.
func postFrame(w http.ResponseWriter, r *http.Request, screen Browser) {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil ||
		media != browser.FrameType {
		httpjson.FailWithFields(w, http.StatusUnsupportedMediaType, httpjson.CodeUnsupportedMediaType,
			httpjson.Detail{"detail": "a frame is declared Content-Type: " + browser.FrameType})
		return
	}
	body := &postBody{w: w, r: r, bound: browser.MaxFrameBytes}
	answerPost(w, r, screen.Frame(r.Context(), r.Header.Get(browser.FrameHeader), body.read), body)
}

// postReport hands the screen one state or check-in, its body read within
// bound once the screen has charged it.
func postReport(w http.ResponseWriter, r *http.Request,
	take func(context.Context, func() ([]byte, error)) error, bound int64) {
	body := &postBody{w: w, r: r, bound: bound}
	answerPost(w, r, take(r.Context(), body.read), body)
}

// postBody is a post's body as the screen reads it: within bound, only once
// the screen has charged the post, so a post past its rate is refused with
// its body unread (internal/browser/screen's package doc, "The box can be
// anything"), and why it could not be read, kept for the answer.
type postBody struct {
	w      http.ResponseWriter
	r      *http.Request
	bound  int64
	unread error
}

func (p *postBody) read() ([]byte, error) {
	body, err := httpjson.ReadBody(p.w, p.r, p.bound)
	p.unread = err
	return body, err
}

// answerPost answers a post the screen took or refused, or, where its body
// could not be read, as a body past its bound or unreadable is answered.
func answerPost(w http.ResponseWriter, r *http.Request, err error, body *postBody) {
	if body.unread != nil {
		httpjson.Refuse(w, body.unread)
		return
	}
	answerScreen(w, r, err, httpjson.CodeInvalidBody)
}

// answerScreen answers what the screen said of a call: 204 for a post taken
// or dropped as stale; 400 with invalid, saying why, for one it refused whole;
// 429 `too_fast` with the refusing limit's own wait as its Retry-After; and,
// once it has closed, 503 to ask again, its opening having ended under it.
func answerScreen(w http.ResponseWriter, r *http.Request, err error, invalid httpjson.Code) {
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if refused, ok := errors.AsType[*browser.Invalid](err); ok {
		httpjson.FailWithFields(w, http.StatusBadRequest, invalid, httpjson.Detail{"why": string(refused.Why)})
		return
	}
	if fast, ok := errors.AsType[*browser.TooFast](err); ok {
		httpjson.ThrottledWith(w, httpjson.CodeTooFast, fast.RetryAfter, nil)
		return
	}
	if !errors.Is(err, browser.ErrScreenClosed) {
		log.WarnContext(r.Context(), "mcp_bridge_browser_failed", "error", err.Error(),
			"detail", "the job's browser screen answered what the bridge does not know; the relay asks again")
	}
	httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(askAgainRetry))
}

// pollBrowser answers the relay's poll, holding it for up to [pollHold] until
// its answer moves: a newer answer, a newer poll in its place, or the
// screen's close. As a notes poll is ([Bridge.pollNotes]), A POLL THIS NODE
// CANNOT HOLD IS REFUSED, never answered as one that waited: once the bridge
// shuts down, a 503 `draining`, and once the screen closed, a 503 to ask again.
func (b *Bridge) pollBrowser(w http.ResponseWriter, r *http.Request, screen Browser) {
	q, err := browser.ParsePollQuery(r.URL.Query())
	if err != nil {
		answerScreen(w, r, err, httpjson.CodeInvalidQuery)
		return
	}
	if b.shuttingDown() {
		httpjson.Unavailable(w, httpjson.CodeDraining, httpjson.RetrySeconds(askAgainRetry))
		return
	}
	answer, moved, err := screen.Poll(r.Context(), q)
	if err == nil && moved != nil {
		hold := time.NewTimer(b.hold)
		defer hold.Stop()
		select {
		case <-moved:
		case <-hold.C:
		case <-b.shutdown:
			httpjson.Unavailable(w, httpjson.CodeDraining, httpjson.RetrySeconds(askAgainRetry))
			return
		case <-r.Context().Done():
			return
		}
		answer, err = screen.Answer()
	}
	if err != nil {
		answerScreen(w, r, err, httpjson.CodeInvalidQuery)
		return
	}
	httpjson.Write(w, http.StatusOK, answer)
}
