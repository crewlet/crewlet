package mcpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// SIGNING THE COMPANY IN TO A REMOTE MCP SERVER WITH OAUTH, and out again.
//
//   - `POST /mcp-servers/{name}/oauth` starts a sign-in to a server whose
//     entry carries `oauth`: the engine reads the server's sign-in metadata,
//     registers where its client is a registration, and answers the
//     authorization address the person's browser is sent to, once. The
//     vendor sends the browser back to `GET /webhooks/mcp-oauth`
//     (internal/api/webhooks), which finishes it.
//   - `GET /mcp-sign-ins/{id}` is where a sign-in is, for the page that
//     polls it: pending, completing, approved or failed, and why. Never the
//     authorization address again.
//   - `DELETE /mcp-servers/{name}/oauth` gives the grant up: revoked at the
//     vendor where it offers that (RFC 7009), forgotten here, and the
//     server stopped on every node.
//
// THE TWO GESTURES ARE `secrets.write`: a step-up, a person (never the
// Dashboard Assistant, whose human-only list holds the verb), and an
// administrator, since what they make or end is the credential every agent
// reaches the server with. Each is one `mcp_server_acted`, refused ones
// included. The read is `config.read`, the configuration's reader's: it says
// how a sign-in an administrator started is going, and carries no address,
// code or token.
//
// NOTHING ANSWERED IS A TOKEN. The access token lives in each node's memory
// alone and the refresh token in the engine's own sealed row; the answers
// here name the server, the sign-in and what the vendor said.

// SignIns starts, reads and ends a server's OAuth sign-in, adapted from the
// engine where the API is wired, with this package's own refusals
// ([ErrNoSuchServer], [ErrNoOAuth], [ErrNoSignIn], [ErrNotSignedIn],
// [*SignInRefusal]).
type SignIns interface {
	StartSignIn(ctx context.Context, server string, by iam.Actor) (SignInAnswer, error)
	SignIn(ctx context.Context, id uuid.UUID) (SignInAnswer, error)
	SignOut(ctx context.Context, server string, by iam.Actor) (SignOutAnswer, error)
}

// The refusals of a sign-in, each answered with what the person can change.
var (
	// ErrNoOAuth is a server whose entry carries no oauth block.
	ErrNoOAuth = errors.New("mcpapi: the MCP server is not signed in to with OAuth")
	// ErrNoCallback is an engine with no public address for the vendor to
	// send the person back to.
	ErrNoCallback = errors.New("mcpapi: this engine has no public address for a sign-in")
	// ErrNoSignIn is a sign-in id nothing holds any more.
	ErrNoSignIn = errors.New("mcpapi: no such sign-in")
	// ErrNotSignedIn is a sign-out of a server nobody is signed in to.
	ErrNotSignedIn = errors.New("mcpapi: nobody is signed in to the MCP server")
)

// SignInRefusal is a sign-in the server's metadata or its vendor refused,
// with the status to answer it with and the engine's words for it.
type SignInRefusal struct {
	Status int
	Detail string
}

func (r *SignInRefusal) Error() string { return "mcpapi: " + r.Detail }

// SignInAnswer is a sign-in: what `POST /mcp-servers/{name}/oauth` answers
// (201) and `GET /mcp-sign-ins/{id}` reads (200).
type SignInAnswer struct {
	ID     uuid.UUID `json:"id"`
	Server string    `json:"server"`
	// Status is `pending`, `completing`, `approved` or `failed`.
	Status string `json:"status"`
	// Detail is why a sign-in failed, "" otherwise.
	Detail    string    `json:"detail"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// AuthorizeURL is where the person's browser is sent to consent, on
	// the answer to the start alone and "" on every read: it carries the
	// sign-in's state, which only the person who started it is handed.
	AuthorizeURL string `json:"authorize_url"`
}

// SignOutAnswer is what `DELETE /mcp-servers/{name}/oauth` answers.
type SignOutAnswer struct {
	Server string `json:"server"`
	// Revocation is `revoked` where the vendor revoked the grant,
	// `not_offered` where it publishes no revocation endpoint (the grant is
	// forgotten here and lapses there), and `failed` where it could not be
	// asked or refused (forgotten here all the same).
	Revocation string `json:"revocation"`
	// Said is the vendor's words where the revocation failed, bounded and
	// redacted; "" otherwise.
	Said string `json:"said"`
}

// serveStartSignIn starts a sign-in to the server its path names: 201 with the
// authorization address.
func (s *Service) serveStartSignIn(w http.ResponseWriter, r *http.Request, _ *types.MCPServerActed) {
	name := r.PathValue("name")
	started, err := s.signIns.StartSignIn(r.Context(), name, auth.AttributionOf(r.Context()))
	if err != nil {
		s.refuseSignIn(w, r, name, err, "mcp_sign_in_not_started",
			"this node could not start the sign-in; ask again")
		return
	}
	httpjson.Write(w, http.StatusCreated, started)
}

// serveSignIn reads one sign-in by id.
func (s *Service) serveSignIn(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": "this sign-in is not known any more; start it again"})
		return
	}
	answer, err := s.signIns.SignIn(r.Context(), id)
	if err != nil {
		s.refuseSignIn(w, r, "", err, "mcp_sign_in_unread", "this node could not read the sign-in; ask again")
		return
	}
	httpjson.Write(w, http.StatusOK, answer)
}

// serveSignOut gives up the server's grant.
func (s *Service) serveSignOut(w http.ResponseWriter, r *http.Request, _ *types.MCPServerActed) {
	name := r.PathValue("name")
	out, err := s.signIns.SignOut(r.Context(), name, auth.AttributionOf(r.Context()))
	if err != nil {
		s.refuseSignIn(w, r, name, err, "mcp_sign_out_failed", "this node could not sign the server out; ask again")
		return
	}
	httpjson.Write(w, http.StatusOK, out)
}

// refuseSignIn answers a sign-in gesture the engine refused, with what the
// person can change, and one it could not make as unavailable.
func (s *Service) refuseSignIn(w http.ResponseWriter, r *http.Request, name string, err error,
	event, unavailable string) {

	var refused *SignInRefusal
	switch {
	case errors.Is(err, ErrNoSuchServer):
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": "the active configuration declares no MCP server " + quoted(name)})
	case errors.Is(err, ErrNoOAuth):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict,
			httpjson.Detail{"detail": "MCP server " + quoted(name) + " is not signed in to with OAuth: " +
				"give its entry an oauth block first"})
	case errors.Is(err, ErrNoCallback):
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeConflict,
			httpjson.Detail{"detail": "this engine has no public address for the vendor to send the " +
				"person back to: set api.external_url, or api.public.external_url where the public " +
				"listener is on"})
	case errors.Is(err, ErrNoSignIn):
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": "this sign-in is not known any more; start it again"})
	case errors.Is(err, ErrNotSignedIn):
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": "nobody is signed in to MCP server " + quoted(name)})
	case errors.As(err, &refused):
		code := httpjson.CodeConflict
		if refused.Status >= http.StatusInternalServerError {
			code = httpjson.CodeVendorRefused
		}
		httpjson.FailWithFields(w, refused.Status, code, httpjson.Detail{"detail": refused.Detail})
	default:
		log.WarnContext(r.Context(), event, "server", name, "error", err.Error())
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds,
			httpjson.Detail{"detail": unavailable})
	}
}
