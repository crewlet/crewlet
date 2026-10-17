package auth

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

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
// A CLOSED SET rather than an echo of what the browser asked for. Reflecting
// `Access-Control-Request-Headers` is the common shortcut and it turns the
// allow-list into the only control left — every header a page can invent is
// then permitted, including ones a future route might read.
//
// AND EXACTLY THE HEADERS THE ROUTES READ AND SET, held so by a gate
// (TestTheCORSListsAreTheHeadersTheRoutesReadAndSet) that walks this API's
// source for every request header a handler reads and every response header
// one sets, and refuses one that is on neither these lists nor its own
// reasoned list of what a page is not offered. A header left out is not
// stripped: the browser refuses the WHOLE request at the preflight, or hides
// the header from the page's script, so a route that reads one a permitted
// origin cannot send is a route that origin can never use, and nothing on
// the engine's side ever sees it fail.
//
// THE OPERATION KEY ([opkey.Header]), because a permitted origin is permitted
// to write: `POST /operator/act/{tool}` refuses a request without one, and the
// human write surface (`/worklet`, `/doclet/pages`) and `/iam` read it to make a retry
// the same write rather than a second one. THE PRECONDITIONS, `If-Match` and
// `If-None-Match`: `/config` and its entities, a work item, a project file and
// `/setup`'s disconnect are written against the version they were read at,
// and `GET /config` revalidates on its tag. `X-Summary`, the revision summary
// a `/config` write must carry. And THE TWO LINK SECRETS
// (`X-Crewlet-Invite-Secret`, `X-Crewlet-Reset-Secret`, declared in
// internal/api/authapi, which imports this package): a link's view takes its
// secret in a header so that it is never in a URL, and a page on a permitted
// origin may already redeem one — the cross-site rule admits the sign-in
// writes — so withholding the view's header would only stop it reading what
// it is redeeming.
//
// THE READER IS NOT ([ReaderHeader]), and stays same-origin only: only the
// dashboard sends it, and the dashboard is served by this process,
// same-origin, where no preflight is asked. A page on another origin has no
// tab of the dashboard's to name the reader of, and offering it would hand
// that page the one header whose whole job is to say which person a tab of
// the dashboard was opened for.
//
// THE EXPOSED HEADERS are the ones a client reads to form its next request:
// the `ETag` it writes back as `If-Match`, the `Retry-After` that says when to
// ask again, the `Accept-Patch` a refused patch names the format it takes in,
// the `Allow` an `OPTIONS` or a `405` lists the methods in, and the
// `Content-Disposition` that names a downloaded project file. A
// CORS-safelisted header (`Content-Type`, `Content-Length`, `Cache-Control`)
// is readable without being listed.
//
// THE METHODS ARE EXACTLY THE ONES THE ROUTES SERVE, held so by
// TestTheCORSMethodsAreTheMethodsTheRoutesServe over every route pattern the
// API's source registers — `OPTIONS` among them, because `OPTIONS /config`
// answers the `Allow` and `Accept-Patch` that corsExposed offers, and only
// GET, HEAD and POST are sent without asking: a script's `OPTIONS` is
// preflighted like a `PATCH`, and a preflight that did not name it refused the
// request outright.
var (
	corsMethods = []string{
		http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions,
	}
	corsHeaders = []string{
		"Authorization", "Content-Type", opkey.Header,
		"If-Match", "If-None-Match", "X-Summary",
		"X-Crewlet-Invite-Secret", "X-Crewlet-Reset-Secret",
	}
	corsExposed = []string{
		"ETag", "Retry-After", "Accept-Patch", "Allow", "Content-Disposition",
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
