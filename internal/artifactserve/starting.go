package artifactserve

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// Starting is what the artifact listener (api.artifacts) answers while its node
// starts: bound before the boot, beside api.port, and handed the origin's own
// [Handler] once the node serves (internal/api's boot surface says why a
// listener is bound that early).
//
// EVERY ANSWER IS THE ORIGIN'S OWN REFUSAL, empty and inert under the same
// headers, for the reason [Handler.refuse] gives: the origin serves bytes and
// nothing else, so a starting node's JSON would be a document an artifact page
// could read. A request for one of [Routes] is told to come back, `503` with
// the Retry-After retry answers at the moment it is asked (a starting node's,
// or a drain's once a stop has begun). A method or a path the origin never
// serves meets the `405` or the `404` it meets once the node serves, so the
// socket says no more while the node starts than it does afterwards.
//
// Nothing here reads a credential, a cookie or a ticket: nothing is served
// that one could unlock.
func Starting(origins Origins, retry func() time.Duration) (http.Handler, error) {
	if retry == nil {
		return nil, errors.New("artifactserve: a starting origin needs the Retry-After it answers")
	}
	policy := origins.refusalPolicy()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch p := r.URL.Path; {
		case !servedMethod(r):
			w.Header().Set("Allow", "GET, HEAD")
			refuseUnder(w, policy, http.StatusMethodNotAllowed, 0)
		case strings.HasPrefix(p, runtimePrefix), strings.HasPrefix(p, kitPrefix), ticketed(p):
			refuseUnder(w, policy, http.StatusServiceUnavailable, retry())
		default:
			refuseUnder(w, policy, http.StatusNotFound, 0)
		}
	}), nil
}

// ticketed reports whether p is a ticketed path the origin routes ([ticketOf]).
func ticketed(p string) bool {
	_, _, ok := ticketOf(p)
	return ok
}
