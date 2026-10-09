package httpjson

import (
	"fmt"
	"net/http"
)

// Router is what a surface mounts its routes on: the two registration verbs of
// [net/http.ServeMux], and nothing else.
//
// AN INTERFACE rather than the mux itself so the API can KEEP every pattern a
// surface mounts. Both of a node's listeners serve one route table behind a
// partition that is a predicate over paths, and only the table says which
// paths exist, so the table is what the listener cases walk whole rather than
// a sample somebody chose. A surface's own tests still hand it a plain
// *http.ServeMux.
//
// Declared here rather than by each consumer because the API's own interface
// over a mountable surface names it in a method signature, and two interfaces
// with one method set are still two types there.
type Router interface {
	Handle(pattern string, handler http.Handler)
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Mux serves mux with the mux's OWN refusals answered as JSON.
//
// # Why it exists
//
// [net/http.ServeMux] answers a path nothing registered with its own 404 and a
// registered path under another method with its own 405, both through
// [net/http.Error] — `text/plain` and `nosniff`, the combination this package
// exists to remove — and neither carries a code. Every client of this engine
// reads a refusal with no code as NOT THE ENGINE'S: something in front of it
// answered, and the node may still have done what was asked. So the engine's
// own "there is nothing here" read as a gateway that swallowed a write: a
// purge sent to a node with no native tracker, where the route is absent by
// design, printed "whether the purge landed is unknown" and a retry that met
// the same 404 for ever — and so did every route a newer CLI or dashboard asks
// an older node for, and every wrong method.
//
// # Why a wrapper and not a catch-all pattern
//
// A catch-all `/` pattern would take the 404, but a pattern with no method
// matches every method, so it would take the 405 as well: a wrong method would
// read as a missing path, and the `Allow` header the mux computes would be
// lost. So the wrapper asks the mux which handler it WOULD run. A registered
// route comes back with its pattern and is served exactly as before; the mux's
// own answers — the 404, the 405 and the redirect that cleans a path — come
// back with none, and only their STATUS is rewritten: the redirect passes
// through untouched, and a handler's own 404 never reaches this code at all.
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
		// on the header this answer is written with.
		FailWith(m.ResponseWriter, status, CodeMethodNotAllowed, map[string]string{
			"detail": fmt.Sprintf("%s does not take %s on this node; it takes %s",
				m.r.URL.Path, m.r.Method, m.Header().Get("Allow")),
		})
	default:
		m.ResponseWriter.WriteHeader(status)
	}
}

func (m *muxRefusal) Write(p []byte) (int, error) {
	if m.refused {
		// The mux's own text, which the JSON above replaced.
		return len(p), nil
	}
	return m.ResponseWriter.Write(p)
}

// NoRoute answers r with the mux's own 404: the path is served by nothing here.
//
// Exported for the one other answer that must be INDISTINGUISHABLE from it: the
// public listener's refusal of a route that exists on api.port. A different
// wording there would tell anybody on the internet that a private admin port
// exists and which paths it serves, so that refusal is this one, byte for byte.
func NoRoute(w http.ResponseWriter, r *http.Request) {
	FailWith(w, http.StatusNotFound, CodeNoRoute, map[string]string{
		"detail": fmt.Sprintf("this node serves nothing at %s %s", r.Method, r.URL.Path),
		"hint": "Check the path. A route this node's build does not have, " +
			"or one its configuration does not enable, is absent rather " +
			"than refused.",
	})
}
