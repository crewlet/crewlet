package main

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// asEngine serves h as the engine serves its API (`api.servedOn`): every
// answer carries the engine's mark ([httpjson.EngineHeader]). A fake that
// plays a node wraps its handler in it; one that plays a proxy, a gateway or
// a vendor's service does not, which is the whole of the difference a client
// reads ([ownBody]).
func asEngine(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(httpjson.EngineHeader, httpjson.EngineHeaderValue)
		h.ServeHTTP(w, r)
	})
}
