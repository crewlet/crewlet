package auth

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
)

// The BROWSER-ORIGIN posture, which is a different question from the
// credential one the rest of this package answers.
//
// A token says WHO is asking. An origin says WHOSE PAGE their browser is
// running, and the browser is the only thing that enforces the answer — which
// is why an unimplemented allow-list is invisible: the operator writes one,
// the engine ignores it, and every cross-origin fetch fails in a console the
// engine never sees. `api.auth.allowed_origins` was in exactly that state.

// CORSMaxAge is how long a browser may cache one preflight.
//
// TEN MINUTES, chosen against how long a posture change may take to be
// obeyed rather than against the cost of a preflight: a browser's own cap is
// hours, and an origin removed from the allow-list would keep working for
// that long on every tab that had already asked. Ten minutes is short enough
// that a rotation is effective within one coffee break and long enough that a
// dashboard doing a fetch a second pays one preflight per six hundred.
const CORSMaxAge = 10 * time.Minute

// corsMethods and corsHeaders are what a permitted origin may send.
//
// A CLOSED SET rather than an echo of what the browser asked for. Reflecting
// `Access-Control-Request-Headers` is the common shortcut and it turns the
// allow-list into the only control left — every header a page can invent is
// then permitted, including ones a future route might read.
var (
	corsMethods = []string{
		http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPut, http.MethodPatch, http.MethodDelete,
	}
	corsHeaders = []string{"Authorization", "Content-Type"}
	// corsExposed is every header a cross-origin client reads that a browser
	// hides by default (only a short safelist is readable cross-origin): the
	// engine's mark, without which every answer reads as something in front
	// of the engine, the entity tag a config write sends back as If-Match,
	// and the wait a 429 or a 503 says to keep.
	corsExposed = []string{httpjson.EngineHeader, "ETag", "Retry-After"}
)

// CORS is the set of browser origins this API answers cross-origin.
//
// EMPTY IS SAME-ORIGIN ONLY, which is the whole default: the dashboard is
// served by this process, so it needs no entry, and a company that never
// configured one gets no cross-origin reads at all.
type CORS struct{ allowed []string }

// NewCORS reads the posture out of Tier A. A nil bootstrap is same-origin
// only, which is the same answer a config that names no origin gives.
//
// Each entry is kept in the browser's spelling ([Origin]), and a request's
// `Origin` is compared in it too, so an entry written `https://Ops.Example.com`
// permits the `https://ops.example.com` every browser sends.
func NewCORS(b *config.Bootstrap) *CORS {
	if b == nil {
		return &CORS{}
	}
	allowed := make([]string, 0, len(b.API.Auth.AllowedOrigins))
	for _, entry := range b.API.Auth.AllowedOrigins {
		if origin := Origin(entry); origin != "" && !slices.Contains(allowed, origin) {
			allowed = append(allowed, origin)
		}
	}
	return &CORS{allowed: allowed}
}

// Origins reports how many origins are permitted, for the startup line that
// states the posture beside the anonymous-read one.
func (c *CORS) Origins() int { return len(c.allowed) }

// Middleware answers preflights and stamps the permission onto a response.
//
// # It wraps OUTSIDE the guard, and that is not a style choice
//
// A preflight is an `OPTIONS` the browser sends ITSELF, and it carries no
// Authorization header — the browser will not attach one until it has been
// told the origin is permitted. Inside the guard, every preflight to a
// guarded route answers 401, the browser reports a CORS failure, and the real
// request is never sent. So the preflight is answered here and never reaches
// the mux.
//
// # A request with no Origin is left alone
//
// Every non-browser caller — a CLI, a script, a peer — sends none, and adding
// headers to their responses would be noise on the overwhelming majority of
// this API's traffic.
//
// # An origin that is not permitted is not REFUSED
//
// It is answered with no CORS headers, and the browser blocks the response
// on its own. Refusing with a status would break the same-origin case: a
// same-origin POST carries an `Origin` too, and a company with an empty
// allow-list — the default — would be refusing its own dashboard.
func (c *CORS) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		preflight := r.Method == http.MethodOptions &&
			r.Header.Get("Access-Control-Request-Method") != ""
		if origin == "" {
			if preflight {
				// A preflight with no origin is not a preflight any
				// browser sent. It is still an OPTIONS the mux has no
				// route for, so it ends here rather than as a 404
				// somebody has to explain.
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		// VARY ON ORIGIN WHENEVER ONE WAS SENT, permitted or not. A
		// shared cache that stored the permitted answer under a key
		// that did not include the origin would serve one site's
		// permission to every other.
		w.Header().Add("Vary", "Origin")
		// The header echoes the Origin as the browser sent it, which is
		// the one value it matches the response against.
		if canonical := Origin(origin); canonical != "" && slices.Contains(c.allowed, canonical) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Expose-Headers", strings.Join(corsExposed, ", "))
			if preflight {
				w.Header().Set("Access-Control-Allow-Methods",
					strings.Join(corsMethods, ", "))
				w.Header().Set("Access-Control-Allow-Headers",
					strings.Join(corsHeaders, ", "))
				w.Header().Set("Access-Control-Max-Age",
					strconv.Itoa(int(CORSMaxAge.Seconds())))
			}
		}
		if preflight {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
