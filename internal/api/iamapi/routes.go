package iamapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Prefix is the surface this package serves.
//
// NOT ON internal/api/auth's EXEMPTION LIST, and nothing under it ever will
// be: every route here is guarded, reads included, for the reason /secrets
// guards its listing.
const Prefix = "/iam/"

// IdempotencyHeader carries an operation id back on a retry.
//
// THE SAME SPELLING /chart USES, because a caller retrying an identity write
// and a caller retrying a chart write are the same script: a fresh id would
// defeat the ledger that makes the retry safe, so the answer to an `unknown`
// carries the id and the route accepts it back.
const IdempotencyHeader = "Idempotency-Key"

// Routes registers the sixteen on a mux.
//
// EVERY ROUTE CARRIES ITS OWN POLICY, stated where it is mounted, through
// [authz.Router] — which is the only reader of the matched pattern, because a
// middleware runs BEFORE the mux matches and anything it decided from would
// be a second router.
//
// THE OBJECT A ROUTE NAMES IS A PERSON ID. That is particular to this surface
// and it is forced: the identity estate keys on an id precisely because a
// person changes their login, so a self check against a mutable name would
// open somebody else's row the day they swapped.
func (s *Service) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, guard)
	var failures []error
	mount := func(pattern string, p authz.Policy, h http.HandlerFunc) {
		if err := router.Handle(pattern, p, h); err != nil {
			failures = append(failures, err)
		}
	}
	// at is a policy naming no object: a listing is not about anybody in
	// particular, so it falls through to the capabilities.
	at := func(a authz.Action) authz.Policy { return authz.Policy{Action: a} }
	// about names the person in the path, which is what makes the self
	// arm reachable.
	about := func(a authz.Action) authz.Policy {
		return authz.Policy{
			Action: a,
			Object: func(r *http.Request) authz.Object {
				return authz.Object{
					Kind: authz.KindPerson, Owner: r.PathValue("id"),
				}
			},
		}
	}
	// ofSubject names the person a query parameter picks, or the caller
	// themselves — which is what lets `GET /iam/credentials` with no
	// parameter be a person reading their own.
	ofSubject := func(a authz.Action) authz.Policy {
		return authz.Policy{
			Action: a,
			Object: func(r *http.Request) authz.Object {
				return authz.Object{
					Kind: authz.KindPerson, Owner: s.subjectOf(r),
				}
			},
		}
	}

	mount("GET /iam/people", at(authz.ActionDirectoryRead), s.GetPeople)
	mount("POST /iam/people", at(authz.ActionDirectoryWrite), s.PostPeople)
	mount("GET /iam/people/{id}", about(authz.ActionDirectoryRead), s.GetPerson)
	mount("PATCH /iam/people/{id}", at(authz.ActionDirectoryWrite), s.PatchPerson)
	mount("DELETE /iam/people/{id}", at(authz.ActionDirectoryWrite), s.DeletePerson)
	// AN INVITATION IS ADDRESSED TO AN ADDRESS, not to a person, so it is
	// its own collection rather than a verb on somebody's row. The design
	// put it at `/iam/people/{id}/invite`, which assumes the person
	// already exists and the link only sets their password; the estate
	// this engine actually has does the opposite — the invitation holds
	// the address, arbitrates on it, and the REDEMPTION is what enrols
	// somebody. A route named after a person who does not exist yet would
	// have had to invent an id for them.
	mount("POST /iam/invitations", at(authz.ActionDirectoryWrite), s.PostInvite)
	mount("GET /iam/people/{id}/sessions",
		about(authz.ActionDirectoryRead), s.GetSessions)
	mount("DELETE /iam/people/{id}/sessions",
		about(authz.ActionSessionEnd), s.DeleteSessions)
	mount("POST /iam/people/{id}/mfa/reset",
		at(authz.ActionDirectoryWrite), s.PostMFAReset)
	mount("GET /iam/credentials",
		ofSubject(authz.ActionDirectoryRead), s.GetCredentials)
	mount("POST /iam/credentials",
		ofSubject(authz.ActionCredentialWrite), s.PostCredentials)
	mount("DELETE /iam/credentials/{id}",
		ofSubject(authz.ActionCredentialWrite), s.DeleteCredential)
	// THE FLEET'S OWN GRANT, not the directory's. sessions.go argues it:
	// a restore is run by whoever runs the deployment, and requiring
	// people:manage as well would hand every SRE the grant that can grant.
	mount("POST /iam/invalidate-all",
		at(authz.ActionFleetOperate), s.PostInvalidateAll)
	mount("GET /iam/check", at(authz.ActionDirectoryRead), s.GetCheck)
	mount("POST /iam/bootstrap-code",
		at(authz.ActionDirectoryWrite), s.PostBootstrapCode)
	// THE AUDIT VERB IT ALREADY HAS. `/iam/audit` and `/events` are the
	// same question read from two tables — what did this company do, and
	// who asked it to — so a grant of its own here would be a second
	// answer to one question.
	mount("GET /iam/audit", at(authz.ActionAuditRead), s.GetAudit)
	return errors.Join(failures...)
}

// subjectOf is the person a request is about: the one it names, or the caller.
//
// THE CALLER IS THE DEFAULT, which is what makes `GET /iam/credentials` with
// no parameter a person reading their own — and it is read from the RESOLVED
// principal rather than from a body, so naming somebody else is a parameter
// the authority table then decides on rather than a claim the handler
// believes.
func (s *Service) subjectOf(r *http.Request) string {
	if named := strings.TrimSpace(r.URL.Query().Get("person")); named != "" {
		return named
	}
	if principal, how := iam.From(r.Context()); how == iam.Resolved {
		return principal.ID.String()
	}
	return ""
}

// guard is what [authz.Router] asks before a handler runs: the shared
// [authz.ContextGuard], whose one clause worth getting right — a caller this
// node could not resolve is UNKNOWN, never the zero principal — is written
// once there rather than once per surface.
//
// NO CHART SEAM. Not one rule this surface mounts asks the chart — the two
// directory classes decide from the person's own id and two capabilities — so
// handing one would be wiring a dependency nothing reads, and [authz.NoChart]
// says exactly that where a nil would have looked like an omission.
var guard = authz.ContextGuard(authz.NoChart{})

// retryIdentity is the `Retry-After` on an identity 503, in seconds.
//
// TWO, which is an apply loop's own scale: what a caller is waiting for is
// this node's applier to commit one more batch.
const retryIdentity = 2

// readBody decodes one request body at this surface's bound.
func readBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var out T
	raw, err := httpjson.ReadBody(w, r, MaxBodyBytes)
	if err != nil {
		// ANSWERED, not merely abandoned: a handler that returned here
		// wrote no status, so a body over the cap came back as an empty
		// 200 — which a client reads as the write having landed.
		httpjson.Refuse(w, err)
		return out, false
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": err.Error()})
		return out, false
	}
	return out, true
}

// opIDFor is the operation id one gesture is published under.
//
// THE CALLER'S OWN WHERE THEY SENT ONE, which is what makes a retry after an
// `unknown` land once: the ledger resolves it, and a fresh id per attempt
// would defeat the mechanism that exists for exactly this case.
func (s *Service) opIDFor(r *http.Request, derived string) string {
	if given := strings.TrimSpace(r.Header.Get(IdempotencyHeader)); given != "" {
		return given
	}
	return derived
}

// statelog0 is the zero position, for the arms that refuse before publishing.
func statelog0() statelog.Position { return statelog.Position{} }

// unavailable answers a read this node could not perform.
//
// 503 AND NEVER AN EMPTY LIST. An identity estate that could not be read and
// a company with nobody in it render identically as `[]`, and the second is
// an answer somebody acts on — so a failed read says so.
func (s *Service) unavailable(w http.ResponseWriter, r *http.Request,
	what string, err error) {

	log.WarnContext(r.Context(), "api_iam_read_failed",
		"what", what, "error", err)
	httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, retryIdentity)
}

// answerWrite renders one identity write's outcome.
//
// THE SAME SIX ANSWERS /chart GIVES, and for the same reasons — see
// [chartapi.Service.answerWrite], which states them at length. What is
// particular here is [iamdomain.ErrRefused] and [iamdomain.ErrClaimed]: the
// first is authority (403, and it will never land however often it is
// retried) and the second is a lost race on an address, a login or a seat
// (409, naming who holds it).
func (s *Service) answerWrite(w http.ResponseWriter, r *http.Request,
	at statelog.Position, err error, extra map[string]any) {

	body := map[string]any{"position": at.String()}
	for k, v := range extra {
		body[k] = v
	}
	var claimed *iamdomain.ErrClaimed
	switch {
	case errors.Is(err, iamdomain.ErrRefused):
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeUnauthorized,
			map[string]string{"detail": err.Error()})
	case errors.As(err, &claimed):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeBadParams,
			map[string]string{"detail": err.Error()})
	case errors.Is(err, iamdomain.ErrNotFindable),
		errors.Is(err, iamdomain.ErrNotFound),
		// A LOGIN OUTSIDE ITS KIND'S GRAMMAR and a kind the directory
		// does not enrol are values the caller typed. Before these were
		// classified they fell through to the 500 below, which told an
		// administrator who wrote `jane` the engine was broken.
		errors.Is(err, iamdomain.ErrInvalidLogin),
		errors.Is(err, iamdomain.ErrNotEnrollable),
		// AND A VALUE OUTSIDE A BOUND — a reason past the cap, a colleague
		// level this build cannot name — for the same reason.
		errors.Is(err, iamdomain.ErrInvalid):
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": err.Error()})
	case errors.Is(err, statelog.ErrConflict):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeBadParams,
			map[string]string{"detail": err.Error()})
	case errors.Is(err, statelog.ErrUnavailable):
		httpjson.FailWith(w, http.StatusServiceUnavailable,
			httpjson.CodeUnavailable, map[string]string{"detail": err.Error()})
	case err != nil:
		httpjson.FailWith(w, http.StatusInternalServerError,
			httpjson.CodeInternalError, map[string]string{"detail": err.Error()})
	default:
		httpjson.Write(w, http.StatusOK, body)
	}
}
