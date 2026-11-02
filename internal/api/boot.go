package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/artifactserve"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/version"
)

// The boot surface: what a node's listeners answer from the moment `crewlet
// run` binds them to the moment it hands them the node's own surface.
//
// # Bound before the boot, so nothing the company configures holds a probe
//
// A node's boot is two different things back to back. First it recovers what
// it holds ITSELF — its store and its migrations, the embedded broker's file
// store, the replicated estate, the logs it provisions, a join's snapshot —
// which takes as long as the store is large, and which only the deployment can
// bound, because only the deployment knows how large a store it gave the node.
// Then it brings up what its COMPANY configures: the shared tool servers (each
// allowed twice its `startup_timeout_seconds`, with no ceiling on the setting),
// the sandboxes, the native halves, the reconciler's first apply, the
// dashboard's projection. A listener bound only at the end of both made a
// startup probe sized for the first also bound the second, so a company whose
// tool server was slow to answer had its node killed and restarted into the
// same wait for ever. (Applying the logs is neither: it catches up in the
// background from the end of the recovery, and what waits for it is a seat's
// acquisition, never the listener.)
//
// So the listener is bound before either, and /health is the line between them:
// 503 while the node recovers ([PhaseRecovering]), which a startup probe sized
// by the deployment bounds exactly as it did, and 200 from the moment the
// engine says it has recovered what it holds ([PhasePreparing]) — the process
// is alive and past the part only a dead disk could hold for ever, and the
// company's part is the company's to bound, never the probe's.
//
// WHICH LEAVES ONE THING NO PROBE CATCHES, and it is the price of the line: a
// part of the company's half that never returns. Each part that waits on
// something else carries a bound of its own, but the reconciler's first apply
// has no deadline beyond its parts' — cut short, it would publish a company
// with whatever had not come up yet missing from it — so a part that hangs
// leaves the node `preparing`, with /ready refusing it `starting`, for as long
// as it hangs, and nothing restarts it. A restart would not have cured it
// either: the next boot meets the same company and waits on the same part.
// What shows it is the readiness that never comes, which a deployment watches
// a rollout by and alerts on.
//
// # Answering 200 during the recovery would have been the obvious move, and wrong
//
// A 200 from the first instant could say only that the process exists, and a
// boot that never finished — on a disk that stopped answering — would then have
// nothing to restart it. That is the one thing a startup probe is for.
//
// # Everything else waits, and says so
//
// /ready answers 503 [ReasonStarting] throughout: the node is not doing its
// work and takes no traffic. Every route the node will serve answers 503
// [httpjson.CodeStarting] with a Retry-After, rather than the refused
// connection it used to meet, so a caller can tell a node about to serve from
// one that is not there.
//
// ONLY A ROUTE THE NODE WILL SERVE IS TOLD TO COME BACK. Every other path is
// answered as the node answers it once it serves, or with the mux's own 404
// where that answer is a refusal a 404 says no more than: on the public
// listener (api.public), whose routes are the ones outside parties call,
// exactly as [partition] answers there; on api.port of an ingress node with
// api.public set, the 404 naming that listener for a route outside parties
// call; and on a node without the ingress role, everything but its probes,
// which its mux refuses with a 404 once it serves — but for the two routes
// its own boxes dial on a node that runs seats, which it serves to this
// host's loopback alone ([Probes]) and so tells a caller there to come back,
// as a box on the host whose run outlived the node's last process calls back
// while it restarts. So a socket says nothing more while it starts than it
// does afterwards, and no caller is sent back to wait for a route that will
// never be there. And nothing here asks for a credential, since nothing is
// served: there is nothing a caller could be refused that it would otherwise
// read.
//
// THE ARTIFACT ORIGIN (api.artifacts) IS BOUND WITH THEM on a node that serves
// one ([ArtifactsListener]), and answers as the origin answers: an empty
// refusal under its own inert headers, `503` with the Retry-After for a route
// it will serve and the `405` or `404` it gives anything else, never this
// surface's JSON ([artifactserve.Starting]).
//
// IN THE SAME BROWSER POSTURE where the node will serve one: on an ingress
// node a route waits inside the CORS layer the full surface answers it in, so
// a dashboard on an allowed origin reads `starting` rather than an opaque
// network error — the reason [App.drainGate] sits inside that layer too.
//
// Once a stop has begun — a signal during the boot — the answers are a
// drain's, as far as a starting node can give them: /health says
// shutting_down, with the status code of the phase it is in (503 while the
// node still recovers, since a probe sized for the recovery is still what
// bounds it, and 200 after), /ready says draining, and every route the node
// would have served `503 draining`.

// BootRetryAfter is the Retry-After a starting node's refusal carries.
//
// TEN SECONDS, the period of the startup probe the deployment guide gives:
// that is the cadence at which a boot's progress is read, and a caller told to
// come back sooner mostly meets the same boot again. How long the rest of a
// boot takes is unknown to the node itself — it is waiting on things that set
// their own bounds — so no estimate of it would be more honest.
const BootRetryAfter = 10 * time.Second

// BootHealth is what /health answers on a node that has not handed its listener
// its own surface yet.
//
// EVERY FIELD IS ONE OF [Health]'s, under the same name, so a script reading
// /health reads one shape whichever of the two answered it. What it leaves out
// is every fact about a running engine — its queue, its seats, its posture,
// its applied epoch — because a starting node has none of them yet, and a zero
// standing in for one would break [Health]'s rule that a zero there is a real
// zero.
type BootHealth struct {
	Status  string `json:"status"`
	Node    string `json:"node"`
	Version string `json:"version"`
	Phase   string `json:"phase"`
}

// Boot answers a node's listeners while the node starts. The notes at the top
// of this file say what it answers and why.
type Boot struct {
	node      string
	recovered atomic.Bool
	stopping  atomic.Bool
	api       http.Handler
	public    http.Handler
	artifacts http.Handler
}

// NewBoot is the boot surface of the node bootstrap describes, answering as
// nodeID — the name its engine will run under, resolved the way the engine
// resolves it.
func NewBoot(bootstrap *config.Bootstrap, nodeID string) (*Boot, error) {
	switch {
	case bootstrap == nil:
		return nil, errors.New("api: the boot surface needs the node's Tier A")
	case strings.TrimSpace(nodeID) == "":
		return nil, errors.New("api: the boot surface needs the node id its answers name")
	}
	b := &Boot{node: nodeID}
	profile := bootstrap.Profile(nodeID)
	ingress, public := profile.RunsIngress(), bootstrap.API.Public.Enabled()
	// THE BROWSER POSTURE the node will answer in: the full surface's CORS
	// layer on an ingress node, and none on the probe surface, which has none.
	posture := func(h http.Handler) http.Handler { return h }
	if ingress {
		posture = auth.NewCORS(bootstrap).Middleware
	}
	// WHAT api.port WILL SERVE once the node does. An ingress node serves
	// every route there — but the ones outside parties call, when api.public
	// is set ([partition]). A node without the role serves its probes, and,
	// where it runs seats, the two routes its own boxes dial to this host's
	// loopback alone ([Probes]); every other path is the mux's own 404 or
	// 405, which says no more than the mux's 404 does once it serves.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", b.serveHealth)
	mux.HandleFunc("GET /ready", b.serveReady)
	switch {
	case ingress:
		mux.HandleFunc("/", b.refuse)
	case profile.RunsSeats():
		boxes := thisHostAlone(http.HandlerFunc(b.refuse))
		mux.Handle(mcpbridge.PathPrefix, boxes)
		mux.Handle(auth.OTLPPrefix, boxes)
	}
	served := posture(httpjson.Mux(mux))
	b.api = servedOn(bootstrap, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ingress && public && auth.Public(r.URL.Path) {
			// A route outside parties call, on an ingress node whose public
			// listener serves it: the 404 naming that listener, outside the
			// browser posture as [partition] answers it.
			servedOnThePublicListener(w, r)
			return
		}
		served.ServeHTTP(w, r)
	}))
	// AND WHAT THE PUBLIC LISTENER WILL SERVE, which only an ingress node
	// binds ([PublicListener]): every route outside parties call. Anything
	// else is the mux's own 404, what [partition] answers there, byte for
	// byte.
	refused := posture(http.HandlerFunc(b.refuse))
	b.public = servedOn(bootstrap, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ingress || !auth.Public(r.URL.Path) {
			httpjson.NoRoute(w, r)
			return
		}
		refused.ServeHTTP(w, r)
	}))
	// AND WHAT THE ARTIFACT ORIGIN WILL SERVE, on a node that binds it: the
	// origin's own refusal, told to come back on its routes.
	if ArtifactsListener(bootstrap, nodeID) {
		origins, err := artifactserve.NewOrigins(artifactserve.OriginsConfigOf(bootstrap))
		if err != nil {
			return nil, fmt.Errorf("api: the boot surface's artifact origin: %w", err)
		}
		if b.artifacts, err = artifactserve.Starting(origins, b.retryAfter); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Handler is what api.port answers while the node starts.
func (b *Boot) Handler() http.Handler { return b.api }

// Public is what the public listener (api.public) answers while the node
// starts.
func (b *Boot) Public() http.Handler { return b.public }

// Artifacts is what the artifact origin (api.artifacts) answers while the node
// starts, or nil on a node that binds no listener for it ([ArtifactsListener]).
func (b *Boot) Artifacts() http.Handler { return b.artifacts }

// retryAfter is the Retry-After a refusal carries now: a starting node's, or a
// drain's once a stop has begun.
func (b *Boot) retryAfter() time.Duration {
	if b.stopping.Load() {
		return DrainRetryAfter
	}
	return BootRetryAfter
}

// Recovered records that the engine has recovered what this node holds itself,
// which turns /health from 503 to 200. See [PhasePreparing].
func (b *Boot) Recovered() { b.recovered.Store(true) }

// Stopping records that the node has begun to stop before it served, which
// turns every answer into a drain's.
func (b *Boot) Stopping() { b.stopping.Store(true) }

// Health is what /health answers now, and the status it answers it with: 503
// while the node recovers, 200 once it has.
func (b *Boot) Health() (BootHealth, int) {
	body := BootHealth{Status: StatusStarting, Node: b.node, Version: version.String(),
		Phase: PhaseRecovering}
	status := http.StatusServiceUnavailable
	if b.recovered.Load() {
		body.Phase, status = PhasePreparing, http.StatusOK
	}
	// SHUTTING DOWN OUTRANKS STARTING, as it outranks every status: a node
	// that was told to stop is stopping, whatever else is true of it.
	if b.stopping.Load() {
		body.Status = StatusShuttingDown
	}
	return body, status
}

func (b *Boot) serveHealth(w http.ResponseWriter, _ *http.Request) {
	body, status := b.Health()
	writeJSON(w, status, body)
}

// serveReady answers /ready: never ready, because a node that has not handed
// its listener its own surface does no work and takes no traffic.
//
// `configured` is false because it is true: no epoch is active in this process
// yet, whatever the store holds. `posture` is empty rather than any of the
// reconciler's words, since no reconciler exists to have concluded one.
func (b *Boot) serveReady(w http.ResponseWriter, _ *http.Request) {
	body := Readiness{Node: b.node, Reason: ReasonStarting}
	if b.stopping.Load() {
		body.Draining, body.Reason = true, ReasonDraining
	}
	writeJSON(w, http.StatusServiceUnavailable, body)
}

// refuse answers a route the node will serve once it has started.
func (b *Boot) refuse(w http.ResponseWriter, r *http.Request) {
	// DEBUG, not info: a vendor holding a delivery retries it, and a line per
	// retry for the whole of a long boot is noise. The path only, never the
	// query, which is where a Confluence Cloud delivery carries its token —
	// and not all of the path where a credential rides in it ([loggedRoute]).
	log.Debug("api_refused_while_starting", "method", r.Method, "route", loggedRoute(r.URL.Path))
	if b.stopping.Load() {
		httpjson.UnavailableWith(w, httpjson.CodeDraining, httpjson.RetrySeconds(DrainRetryAfter),
			httpjson.Detail{
				"detail": "this node was told to stop before it finished starting, " +
					"and serves nothing",
				"hint": "retry against another node, or once this one has restarted",
			})
		return
	}
	detail := "this node is recovering what it holds (its store, its broker and " +
		"its copy of the estate) and serves nothing until it has started"
	if b.recovered.Load() {
		detail = "this node has recovered what it holds and is bringing up what " +
			"its company configures; it serves nothing until it has started"
	}
	httpjson.UnavailableWith(w, httpjson.CodeStarting, httpjson.RetrySeconds(BootRetryAfter),
		httpjson.Detail{
			"detail": detail,
			"hint": "retry after the Retry-After: once the node serves, /health stops " +
				"answering status \"starting\" and /ready stops naming reason \"starting\"",
		})
}

// loggedRoute is the path a refusal's log line names: the request's own, with
// the signed run token that the tool bridge (`/mcp/{token}`) and the OTLP
// receiver (`/otlp/{token}/v1/{signal}`) carry IN THE PATH replaced by a
// placeholder.
//
// THE SERVING NODE NEVER NEEDED THIS, which is why the drain gate's line names
// the raw path: it serves both prefixes through a drain, so it never logs one.
// A starting node refuses them — a coding run calling back while its node
// restarts — and a line naming the raw path would write a live credential
// into the log.
func loggedRoute(path string) string {
	for _, prefix := range []string{mcpbridge.PathPrefix, auth.OTLPPrefix} {
		if rest, ok := strings.CutPrefix(path, prefix); ok {
			if _, tail, found := strings.Cut(rest, "/"); found {
				return prefix + "{token}/" + tail
			}
			return prefix + "{token}"
		}
	}
	return path
}

// PublicListener reports whether a node binds a listener on api.public: one
// with the ingress role, for the routes outside parties call. api.port 0 is the
// hard off switch for it, as everywhere.
//
// NEVER A NODE WITHOUT THE ROLE, its seats included: a box off the node's host
// reaches the fleet's bridge on any node that serves the API, which relays
// its call to the seat's holder over the broker (internal/api/mcpbridge), and
// a box on the host dials the node's api.port on loopback, which the node
// serves its own host alone ([Probes]) — so a node that serves no API has no
// route a caller off its host needs it to serve.
//
// ONE PREDICATE for the two moments that need it — `crewlet run` binding the
// listener before there is an engine, and serving it once there is one — so a
// node never binds a public listener it then serves nothing on.
func PublicListener(bootstrap *config.Bootstrap, nodeID string) bool {
	return bootstrap.API.Port != 0 && bootstrap.API.Public.Enabled() &&
		bootstrap.Profile(nodeID).RunsIngress()
}

// ArtifactsListener reports whether a node binds a listener on api.artifacts:
// one with the ingress role, whose dashboard frames what it serves, where the
// file sets a port for it. api.port 0 is the hard off switch for it too.
//
// ONE PREDICATE, for [PublicListener]'s reason: `crewlet run` binds the
// listener before there is an engine, and serves the origin on it once there
// is one, so a node never binds an origin it then serves nothing on, nor
// builds one for a socket it never bound.
func ArtifactsListener(bootstrap *config.Bootstrap, nodeID string) bool {
	return bootstrap.API.Port != 0 && bootstrap.API.Artifacts.Enabled() &&
		bootstrap.Profile(nodeID).RunsIngress()
}
