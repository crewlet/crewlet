package auth

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
)

// THE ONE WAY A HANDLER ASKS WHO IS CALLING, and why it is a helper rather
// than nineteen copies of a switch.
//
// [iam.From] answers three-valued, and the value a handler is most likely to
// drop is the one that matters: nineteen sites used to write `operator, _ :=`
// because the prefix guard had already answered the question, and the
// discarded half is now the UNKNOWN arm. Discarded, "this node could not tell"
// reads as "you are nobody" — and goes on reading that way for as long as the
// identity store is unreachable, which teaches everybody in the company to go
// and check their password while nothing is wrong with it.
//
// So the switch lives here, once, and a handler gets a principal or nothing.
// What makes that stick is that this returns no resolution for a caller to
// ignore: there is no second value to drop.

// Caller is the principal behind a request, or writes the refusal and reports
// false.
//
//   - RESOLVED answers the principal.
//   - ANONYMOUS is 401 `invalid_token`: this node checked, and nobody
//     presented a credential it accepts. A refusal about the CALLER.
//   - UNKNOWN is 503 `identity_unavailable` with a Retry-After, and it is
//     deliberately not a 401: a refusal about this NODE, which the dashboard
//     retries and which must never send somebody to reset a working
//     credential. The reason [iam.Reason] carries is logged rather than
//     answered, because "the store timed out" is an operator's sentence and
//     not a caller's.
func Caller(w http.ResponseWriter, r *http.Request) (iam.Principal, bool) {
	principal, how := iam.From(r.Context())
	switch how {
	case iam.Resolved:
		return principal, true
	case iam.Anonymous:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return iam.Principal{}, false
	default:
		// A WIRING BUG AND AN OUTAGE LOOK THE SAME FROM HERE and are
		// two different sentences to whoever reads the log:
		// [iam.ErrUnresolved] is a handler reached by a path nobody
		// wired through the guard, and anything else is a resolver
		// that ran and could not answer.
		logging.Get("api.auth").WarnContext(r.Context(), "api_identity_unavailable",
			"route", r.URL.Path, "reason", iam.Reason(r.Context()),
			"remote", remoteHost(r))
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, RetryIdentitySeconds)
		return iam.Principal{}, false
	}
}
