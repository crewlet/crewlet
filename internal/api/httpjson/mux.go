package httpjson

import (
	"fmt"
	"net/http"
)

// Router is what a surface mounts its routes on: the two registration verbs of
// [net/http.ServeMux], and nothing else.
//
// AN INTERFACE rather than the mux itself so the API can KEEP every pattern a
// surface mounts: both listeners serve one route table behind a path
// predicate, and the listener cases walk the whole table, not a sample. A
// surface's own tests still hand it a plain *http.ServeMux.
//
// Declared here rather than by each consumer because the API's interface over
// a mountable surface names it in a method signature, and two interfaces with
// one method set are still two types there.
type Router interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Mux serves mux with the mux's OWN refusals answered as JSON.
//
// [net/http.ServeMux]'s own 404 and 405 are `text/plain`, `nosniff`, no code.
// Every client reads a codeless refusal as NOT THE ENGINE'S (a gateway
// answered; the node may still have done what was asked), so a retry looped.
//
// A WRAPPER, NOT A CATCH-ALL `/`: a pattern with no method matches every
// method, so it would take the 405 too and lose the mux's `Allow` header. The
// wrapper asks the mux which handler it WOULD run. A registered route comes
// back with its pattern and is served as before; the mux's own answers (404,
// 405, the path-cleaning redirect) come back with none; only their STATUS is
// rewritten, never the redirect's.
func Mux(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// `*` is the mux's own too — an OPTIONS * it refuses 400 and
		// closes — and it answers that before routing, so it goes to the
		// mux unchanged.
		if handler, pattern := mux.Handler(r); pattern == "" && r.RequestURI != "*" {
			handler.ServeHTTP(&muxRefusal{ResponseWriter: w, r: r}, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// muxRefusal is the writer the mux's own answer is served through: a 404 or a
// 405 becomes this package's JSON, and its text body is dropped; anything else
// — the path-cleaning redirect — is written as it was.
type muxRefusal struct {
	http.ResponseWriter
	r       *http.Request
	refused bool
}

func (m *muxRefusal) WriteHeader(status int) {
	switch status {
	case http.StatusNotFound:
		m.refused = true
		NoRoute(m.ResponseWriter, m.r)
	case http.StatusMethodNotAllowed:
		m.refused = true
		// The mux set `Allow` before it wrote the status, so it is still
		// on the header this answer is written with. It names the methods
		// the ROUTE TABLE holds for the path, which is not a promise that
		// this node serves them now: a route mounted on every API and
		// served only where the node holds what it needs ([NoRoute]) is
		// in the table either way, so the sentence names the build's
		// routes rather than claiming the node takes them.
		FailWith(m.ResponseWriter, status, CodeMethodNotAllowed, map[string]string{
			"detail": fmt.Sprintf("%s does not take %s; this build's routes for it take %s",
				m.r.URL.Path, m.r.Method, m.Header().Get("Allow")),
		})
	default:
		m.ResponseWriter.WriteHeader(status)
	}
}

// NoRoute writes the answer for a route this node does not serve: `404
// no_route`, naming the method and the path.
//
// THE MUX'S OWN ANSWER, exported for the answers that must be
// INDISTINGUISHABLE from it: a registered route with nothing to serve here
// (configuration or mode does not enable it) answers the bytes its absence
// would, and the public listener's refusal of a route on api.port is this
// answer byte for byte, so nobody on the internet learns a private admin port
// exists.
//
// THE HINT NAMES BOTH KINDS OF ABSENCE: only the first is the path's fault,
// and a node that merely does not enable the route is asked at the right path.
func NoRoute(w http.ResponseWriter, r *http.Request) {
	FailWith(w, http.StatusNotFound, CodeNoRoute, map[string]string{
		"detail": fmt.Sprintf("this node serves nothing at %s %s",
			r.Method, r.URL.Path),
		"hint": "Check the path. A route this node's build does not have is " +
			"absent rather than refused, and so is one this node does not " +
			"serve now: its configuration or the mode it started in does not " +
			"enable it.",
	})
}

func (m *muxRefusal) Write(p []byte) (int, error) {
	if m.refused {
		// The mux's own text, which the JSON above replaced.
		return len(p), nil
	}
	return m.ResponseWriter.Write(p)
}
