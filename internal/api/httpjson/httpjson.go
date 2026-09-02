// Package httpjson is how every JSON surface in this engine answers.
//
// It exists because there were four byte-identical copies of the same response
// writer and three of the same body reader, and they had already drifted where
// it shows: one 413 said `body_too_large`, another `value_too_large`, and a
// third answered `payload too large` as plain text. A client cannot branch on
// a vocabulary that depends on which route it hit.
//
// The other half is subtler and was wrong at five call sites. [net/http.Error]
// sets `Content-Type: text/plain` and `X-Content-Type-Options: nosniff`, so
// handing it a JSON literal produces the one combination guaranteed to stop a
// strict client parsing it — the body says it is JSON, the headers swear it is
// not, and the sniffing that would otherwise paper over it is explicitly
// disabled. Every error here goes out as real JSON with the right type.
package httpjson

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"encoding/json"
)

// Code is the machine-readable `error` value a failed request carries.
//
// A NAMED TYPE over a closed set, so a route cannot invent a fifth spelling of
// "too large" the way three of them already had. Clients branch on these; the
// human-readable half belongs in logs, where an operator reads it.
type Code string

// The codes every JSON surface answers with. snake_case throughout, which is
// what the dashboard and the websocket protocol already read.
const (
	// CodeEncodeFailed is the one this package can produce on its own: the
	// handler's own body would not marshal.
	CodeEncodeFailed Code = "encode_failed"

	// CodeBodyTooLarge is the single spelling of a 413, whether what
	// overflowed was a config document, a secret value or a webhook
	// delivery.
	CodeBodyTooLarge Code = "body_too_large"

	// CodeUnreadableBody is a body that could not be read to the end —
	// a client that hung up mid-request, not one that sent too much.
	CodeUnreadableBody Code = "unreadable_body"

	// CodeInvalidBody is a body that was read but is not what the route
	// accepts.
	CodeInvalidBody Code = "invalid_body"

	// CodeInternalError is the deliberately opaque answer to a failure the
	// caller can do nothing about. The detail goes to the log.
	CodeInternalError Code = "internal_error"
)

// Valid reports whether c is one this package defines.
func (c Code) Valid() bool {
	switch c {
	case CodeEncodeFailed, CodeBodyTooLarge, CodeUnreadableBody,
		CodeInvalidBody, CodeInternalError:
		return true
	default:
		return false
	}
}

// ErrTooLarge is what a body over a route's cap surfaces as.
//
// A sentinel rather than a *http.MaxBytesError so callers do not each have to
// know that detail of net/http, and so [Refuse] can tell the two failures
// apart without a second type assertion.
var ErrTooLarge = errors.New("httpjson: body over the limit")

// Write answers with status and body as JSON.
//
// The status is written BEFORE the body because it has to be: once any byte of
// the body is written the header is gone, and a WriteHeader after it is a
// silent no-op that leaves the route answering 200 for a failure.
func Write(w http.ResponseWriter, status int, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		// The handler's body is unmarshalable, so the only thing left to
		// send is this package's own. Written by hand rather than through
		// Marshal, which is what just failed.
		slog.Error("http_encode_failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"` + string(CodeEncodeFailed) + `"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// Fail answers with status and a `{"error": code}` body.
func Fail(w http.ResponseWriter, status int, code Code) {
	Write(w, status, map[string]string{"error": string(code)})
}

// FailWith is [Fail] plus extra fields, for a route whose refusal carries
// something the caller can act on — which field was wrong, how long to wait.
//
// `error` is always present and always wins: a caller that branches on it must
// never find it missing because a route's extras happened to omit it.
func FailWith(w http.ResponseWriter, status int, code Code, extra map[string]string) {
	body := make(map[string]string, len(extra)+1)
	for k, v := range extra {
		body[k] = v
	}
	body["error"] = string(code)
	Write(w, status, body)
}

// BodyReadTimeout bounds how long a client may take to deliver its body.
//
// A size cap is not a time bound, and the two failures are different: the size
// cap stops a client sending 25 MiB, and this stops one sending 25 bytes a
// minute apart. Without it a request that dribbles holds a handler goroutine
// and a connection slot for as long as the client cares to keep dribbling —
// the cheapest denial there is against a listener, and the listener is the one
// surface an unauthenticated caller can reach.
//
// It is NOT the server's ReadTimeout, which is why that field is still unset:
// ReadTimeout covers the whole exchange from the first header byte, so any
// value large enough for a 25 MiB webhook on a slow link is also large enough
// to be no bound at all on a small one. A deadline taken HERE starts when the
// handler asks for the body, so it bounds the body alone.
//
// THIRTY SECONDS, from the largest thing this reads: webhooks.MaxBodyBytes is
// 25 MiB, which needs roughly 7 Mbit/s sustained to arrive inside the bound —
// far below what any CI runner, forge or operator workstation delivers, and
// far above the trickle this exists to cut off. The server's own
// ReadHeaderTimeout (10 s) and IdleTimeout (60 s) bound the other two phases;
// this is the third.
const BodyReadTimeout = 30 * time.Second

// ReadBody reads at most max bytes of a request body, within
// [BodyReadTimeout].
//
// It reads the WHOLE body even when the request will be refused. An HTTP
// server that answers without draining leaves unread bytes in the socket and
// the client sees a connection reset instead of the status it was sent — which
// for a 401 means "retry forever" rather than "your signature is wrong".
//
// Over the cap it returns [ErrTooLarge]; anything else is the read's own
// error — a blown deadline included, since a body that never arrived and one
// that was cut off are the same thing to a caller.
func ReadBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	// http.ErrNotSupported is IGNORED rather than reported: a
	// ResponseWriter that cannot carry a deadline is a recorder or a
	// wrapper, never a real connection, so there is nothing to bound and
	// nothing a caller could do about it. Failing the read there would
	// break every handler under httptest for a property the test has no
	// way to violate.
	if err := http.NewResponseController(w).
		SetReadDeadline(time.Now().Add(BodyReadTimeout)); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		return nil, err
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err == nil {
		return raw, nil
	}
	var overflow *http.MaxBytesError
	if errors.As(err, &overflow) {
		return nil, ErrTooLarge
	}
	return nil, err
}

// Refuse answers a [ReadBody] failure with the status it deserves: 413 for a
// body over the cap, 400 for one that could not be read.
func Refuse(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrTooLarge) {
		Fail(w, http.StatusRequestEntityTooLarge, CodeBodyTooLarge)
		return
	}
	Fail(w, http.StatusBadRequest, CodeUnreadableBody)
}
