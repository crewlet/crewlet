package auth

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
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

// corsMethods and corsHeaders are what a permitted origin may send, and
// corsExposed what its script may read of an answer.
//
// CLOSED SETS, never an echo of `Access-Control-Request-Headers`, which would
// permit every header a page can invent, including ones a future route reads.
// A header left out is not stripped: the browser refuses the WHOLE request at
// the preflight (or hides it from the script), and the engine never sees it
// fail. TestTheCORSListsAreTheHeadersTheRoutesReadAndSet holds the lists to
// the routes.
var (
	// The methods are exactly those the routes serve, held by
	// TestTheCORSMethodsAreTheMethodsTheRoutesServe; `OPTIONS` is among them
	// because a script's is preflighted like a `PATCH`.
	corsMethods = []string{
		http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions,
	}
	// Request headers: the operation key (a retry is the same write), the
	// preconditions, `X-Summary`, the request id and the two link secrets
	// (declared in internal/api/authapi), kept in headers so never in a URL;
	// the cross-site rule already admits redeeming one. The reader
	// ([httpjson.ReaderHeader]) stays same-origin: it names which person a
	// dashboard tab was opened for.
	corsHeaders = []string{
		"Authorization", "Content-Type", opkey.Header,
		"If-Match", "If-None-Match", "X-Summary", httpjson.RequestIDHeader,
		"X-Crewlet-Invite-Secret", "X-Crewlet-Reset-Secret",
	}
	// Exposed: what a client reads to tell whose answer it holds and form its
	// next request. The engine's mark ([httpjson.EngineHeader]) tells it a
	// write's outcome from something in front of the engine. CORS-safelisted
	// headers need no listing.
	corsExposed = []string{
		httpjson.EngineHeader, "ETag", "Retry-After", "Accept-Patch", "Allow",
		"Content-Disposition", "Location", httpjson.RequestIDHeader,
	}
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
// It wraps OUTSIDE the guard: a preflight carries no Authorization (the browser
// attaches none until told the origin is permitted), so inside the guard every
// preflight to a guarded route would answer 401 and the real request never be
// sent. It is answered here and never reaches the mux.
//
// A request with no Origin (a CLI, a script, a peer) is left alone. An origin
// not permitted is not REFUSED, only given no CORS headers: a same-origin POST
// carries an `Origin` too, so a status would refuse the dashboard of a company
// with the default empty allow-list.
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
			if preflight {
				w.Header().Set("Access-Control-Allow-Methods",
					strings.Join(corsMethods, ", "))
				w.Header().Set("Access-Control-Allow-Headers",
					strings.Join(corsHeaders, ", "))
				w.Header().Set("Access-Control-Max-Age",
					strconv.Itoa(int(CORSMaxAge.Seconds())))
			} else {
				// ON THE ANSWER ITSELF, never the preflight: the actual
				// response is the one a browser reads the exposure from.
				w.Header().Set("Access-Control-Expose-Headers",
					strings.Join(corsExposed, ", "))
			}
		}
		if preflight {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
