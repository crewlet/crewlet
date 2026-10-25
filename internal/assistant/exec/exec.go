// Package exec is how a Dashboard Assistant tool call becomes an ordinary
// request: the request a call builds, the answer it gets back, and the seam to
// the one component that may send it, internal/api/auth's Dispatcher
// (ADR-0046).
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
	"net/http"
	"net/url"
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
}

// MaxResponseBytes is the most of one answer a call reads: sixty-four
// kibibytes, sixteen of answer_knowledge's four-kibibyte sources, which keeps
// eight steps' results inside an interactive model's context before the
// model's own cut (16 KiB per result) and the cards' (256 KiB per card) apply.
const MaxResponseBytes = 64 << 10

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
