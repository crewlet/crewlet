// Package exec is how a Dashboard Assistant tool call becomes an ordinary
// request: the request a call builds, the answer it gets back, and the seam to
// the one component that may send it, internal/api/auth's Dispatcher
// (ADR-0050).
//
// # Every call is the person's own request, through the one handler chain
//
// A read tool and a dashboard read of the same data are the same request: the
// same route, the same guard, the same authority table, the same refusal. So a
// call is never served by calling a builtin, a writer or the question registry
// in process (each would be a fourth transport into the tools that could drift
// from the other three); it is a [Request] the Dispatcher hands to the very
// http.Handler the API port serves, carrying a grant that is useless anywhere
// else.
//
// # A leaf, so the assistant never holds a credential
//
// internal/api/auth owns every credential shape and imports the MCP bridge, so
// nothing under internal/assistant may import it (internal/assistant's import
// walk). This package is the other way round: auth imports it and implements
// [Dispatcher], and the orchestrator holds a [Grant] it can name, time and
// hand back, but never read. The grant's token never leaves auth.
//
// It builds requests and nothing else. It never sets the in-process marker
// the guard requires of an assistant grant, because only auth can: the marker
// is an unexported context key there, and a request this package built and
// sent any other way is refused as a grant presented over the wire.
package exec

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Grant is an assistant grant as the orchestrator holds it: an id it can log
// and revoke by, an expiry it can fence a turn on, and nothing it could
// present itself. internal/api/auth mints the only implementation, and its
// Dispatcher refuses any other.
type Grant interface {
	// ID is the grant's id, the `assist:<id>` every row it writes names.
	ID() uuid.UUID
	// Expires is when the grant stops being accepted, never later than the
	// session it was derived from.
	Expires() time.Time
}

// Request is one tool call as an HTTP request: a route the catalogue binds the
// tool to, its query or its JSON body, and for a write the operation key that
// makes a retry the same write.
type Request struct {
	// Method and Path are the route's, Path with its wildcards filled in.
	Method string
	Path   string
	Query  url.Values
	// Body is the request's JSON, nil for a read.
	Body []byte
	// IdempotencyKey is the operation an approved write executes under
	// (internal/api/opkey's header); empty for a read.
	IdempotencyKey string
	// IfMatch is the entity tag a conditional write is sent under (the
	// `If-Match` header, RFC 9110), quoted as the route answered it in its
	// ETag; empty for an unconditional request. NEVER a tool's or a
	// preview's to choose: internal/assistant/proposal derives it from the
	// proposal's source revision, which the proposal's digest binds, so the
	// precondition the route judges is the version the person was shown.
	IfMatch string
	// IfNoneMatch is [AnyRepresentation] for a write that creates what its
	// path names (the `If-None-Match: *` header, RFC 9110 §13.1.2: "only if
	// there is no current representation"), and empty otherwise. Derived
	// the way IfMatch is, from the proposal's source revision, and never
	// beside it: the two describe two different resources.
	IfNoneMatch string
	// ReadLimit is how much of the answer a read keeps: zero for a step's
	// own read ([MaxResponseBytes]), and [PreviewMaxResponseBytes] for the
	// read a write's preview diffs against. Never more than that, whatever
	// a caller asks.
	ReadLimit int
}

// Bound is how much of r's answer is kept: a step's bound, or the larger one
// a preview's read asks ([Request.ReadLimit]), and never more than that. The
// one rule every [Dispatcher] cuts an answer by.
func (r Request) Bound() int {
	if r.ReadLimit <= 0 {
		return MaxResponseBytes
	}
	return min(r.ReadLimit, PreviewMaxResponseBytes)
}

// AnyRepresentation is the one If-None-Match value a write here carries: `*`,
// the create-only precondition.
const AnyRepresentation = "*"

// ValidEntityTag reports whether tag is one strong entity tag as RFC 9110
// spells it: a quoted string of visible characters with no quote inside, the
// only kind of If-Match a request here carries.
func ValidEntityTag(tag string) bool {
	if len(tag) < 2 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return false
	}
	for _, c := range []byte(tag[1 : len(tag)-1]) {
		if c == '"' || c < 0x21 || c == 0x7f {
			return false
		}
	}
	return true
}

// ErrInvalidRequest reports a [Request] that is not a request to one of the
// engine's own routes: no method, or a path that is anything other than an
// absolute path with its query apart.
var ErrInvalidRequest = errors.New("exec: not a request to one of the engine's own routes")

// URL is the URL r is sent to and the one its request digest is computed over.
//
// THE ONE BUILDER: the proposal's digest (what the person approved), the exec
// grant's digest (what the guard admits) and the Dispatcher's request (what is
// sent) are all built here, so the three can never encode one request two ways.
// It refuses a method with whitespace in it and a path that carries a scheme,
// a host, user information, its own query or a fragment, so the request can
// only ever reach this engine's own handler chain.
func (r Request) URL() (*url.URL, error) {
	if r.Method == "" || strings.ContainsAny(r.Method, " \t\r\n") {
		return nil, fmt.Errorf("%w: the method %q is not one", ErrInvalidRequest, r.Method)
	}
	u, err := url.Parse(r.Path)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: the path %q does not parse: %w", ErrInvalidRequest, r.Path, err)
	case u.Scheme != "" || u.Host != "" || u.User != nil || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || !strings.HasPrefix(u.Path, "/"):
		return nil, fmt.Errorf("%w: the path %q is not an absolute path alone, with "+
			"its query apart", ErrInvalidRequest, r.Path)
	}
	u.RawQuery = r.Query.Encode()
	return u, nil
}

// Response is what the handler chain answered, read to at most
// [MaxResponseBytes].
type Response struct {
	Status int
	Header http.Header
	Body   []byte
	// Truncated is an answer longer than the bound; its body is cut there
	// and is never handed to a model or a card as though it were whole.
	Truncated bool
	// ModelDetail is the sentence a refused tool call answered a MODEL
	// with, told in process by the route that refused it
	// (internal/modelnote) and never part of Body: the route answers a
	// person a person's words, and this is the half only the model that
	// made the request reads, so it can say what to change (ADR-0060).
	// Empty where the route told none.
	ModelDetail string `json:"-"`
}

// MaxResponseBytes is the most of one answer a call reads: sixty-four
// kibibytes, sixteen of answer_knowledge's four-kibibyte sources: what one
// step reads before the model's own cut (16 KiB per result, which is what
// keeps a turn's steps inside the model's context) and the cards' (256 KiB per
// card) apply.
const MaxResponseBytes = 64 << 10

// PreviewMaxResponseBytes is the most of one answer a write's PREVIEW reads:
// one mebibyte. A preview decodes the record it diffs against (a seat, a
// unit, an agent with its fifty phases of history, the company document) and
// draws only the fields that move, so nothing of the answer reaches a model
// or a card whole, and the step's bound, which exists to keep a turn's steps
// inside the model's context, is the wrong bound for it: an agent with a
// long history was past sixty-four kibibytes, and every pause of it was
// refused before a card was drawn. A mebibyte holds the largest of those
// records many times over and still bounds what one preview holds in memory.
const PreviewMaxResponseBytes = 1 << 20

// Dispatcher sends a request through the API's own handler chain as the
// person the grant was derived from. internal/api/auth implements it; the
// orchestrator and the approval path consume it.
//
// An error is a request that could not be sent at all (a grant this
// Dispatcher did not mint, a handler not yet bound, a context already done);
// every answer the chain gave, a refusal included, is a [Response].
type Dispatcher interface {
	Do(ctx context.Context, g Grant, r Request) (Response, error)
}
