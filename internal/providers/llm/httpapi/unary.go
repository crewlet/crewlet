package httpapi

import (
	"bytes"
	"io"
	"mime"
	"net/http"
)

// UnaryAnswer keeps the one answer to a STREAMING request that an SDK's
// event-stream decoder cannot read: a whole response body, from an endpoint
// that took `stream: true` and answered as though it had not been asked.
//
// "Anthropic-compatible" and "OpenAI-compatible" are de-facto standards with
// real variance, and a local shim or a gateway may implement the unary route
// only. Both SDKs hand any 2xx response to their event-stream decoder whatever
// its content type, and a JSON body has no events in it, so the stream ends
// empty and the answer — usage included — is gone. The adapters used to ask
// again, unary, which worked and paid twice: the first request was processed
// and billed by the endpoint, and its answer reached no completion, so no
// counter and no rollup ever heard of it, once per provider per process on
// every node.
//
// So a 2xx response that is not an event stream is read here, before the
// decoder sees it, and offered to accept, which knows the unary shape of the
// adapter's own SDK. A body it takes is kept for the adapter and the decoder is
// handed an empty one, so the stream ends with no events and nothing else is
// asked. A body it does not take is handed back to the decoder unchanged, so a
// stream an endpoint mislabelled is still decoded as one — only no longer as
// it arrives, which is the price of reading it to find out.
//
// Read WHOLE, as the SDKs' own unary path reads a 2xx body: the answer is
// bounded by the request's own max_tokens, and the read by its silence — a
// backend installs its [IdleWatchdog] INSIDE this middleware, so the body read
// here is the watched one. One per call: a UnaryAnswer is not safe to share
// between requests.
type UnaryAnswer struct {
	accept func(body []byte) bool
	taken  bool
}

// NewUnaryAnswer is a UnaryAnswer whose accept reports whether a body is the
// endpoint's unary answer, keeping it when it is.
func NewUnaryAnswer(accept func(body []byte) bool) *UnaryAnswer {
	return &UnaryAnswer{accept: accept}
}

// Middleware is the request middleware both SDKs take — each declares its own
// option.Middleware as an alias of exactly this signature.
func (u *UnaryAnswer) Middleware(req *http.Request,
	next func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	res, err := next(req)
	if err != nil || res == nil || res.Body == nil ||
		res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices ||
		isEventStream(res.Header.Get("Content-Type")) {
		return res, err
	}
	body, readErr := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if u.accept(body) {
		u.taken = true
		res.Body = http.NoBody
		return res, nil
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	return res, nil
}

// Taken reports whether the request was answered unary and the answer kept.
func (u *UnaryAnswer) Taken() bool { return u.taken }

// isEventStream reports whether a Content-Type names an event stream. A
// missing or malformed one does not, and its body is then offered to accept,
// which is what keeps a unary answer with no type from being lost.
func isEventStream(contentType string) bool {
	media, _, err := mime.ParseMediaType(contentType)
	return err == nil && media == "text/event-stream"
}
