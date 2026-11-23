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

// The boot surface: what a node's listeners answer from `crewlet run` binding
// them until it hands them the node's own surface. The listener is bound BEFORE
// the boot, and /health is the line inside it. A boot first recovers what the
// node holds itself (store, estate, a join's snapshot), which only the
// deployment can bound, then brings up what its COMPANY configures (tool servers
// with no ceiling on their startup timeout, sandboxes, the first apply), which
// only the company can. /health is 503 while recovering ([PhaseRecovering]),
// bounded by a probe sized by the deployment, and 200 once recovered
// ([PhasePreparing]); a 200 from the first instant would leave a boot stuck on a
// dead disk nothing to restart it. A hung company part leaves the node
// `preparing` and nothing restarts it: the first apply has no deadline (cut
// short it would publish a company missing parts); a restart meets the same wait.

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

// Boot answers a node's listeners while the node starts: /ready answers 503
// [ReasonStarting] throughout, and every route the node will serve answers 503
// [httpjson.CodeStarting] with a Retry-After, so a caller can tell a node about
// to serve from one that is not there. Nothing asks for a credential, since
// nothing is served. The artifact origin ([ArtifactsListener]) answers as the
// origin does, an empty refusal under its own headers ([artifactserve.Starting]).
// On an ingress node a route waits inside the CORS layer the full surface uses
// (why [App.drainGate] sits inside it too), so an allowed dashboard origin reads
// `starting`, not an opaque network error. Once a stop has begun, /health says
// shutting_down with its phase's status code (503 while recovering, 200 after),
// /ready says draining, and every route the node would serve is `503 draining`.
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
//
// ONLY SUCH A ROUTE IS TOLD TO COME BACK; every other path is answered as the
// serving node answers it, or with the mux's own 404 where that answer is a
// refusal: on the public listener as [partition] does; on api.port of an ingress
// node with api.public set, the 404 naming that listener; on a node without the
// ingress role, everything but its probes, except the two routes its own boxes
// dial on a node that runs seats, served to this host's loopback alone
// ([Probes]), so a box whose run outlived the last process calls back while it
// restarts. No caller waits for a route that will never be there.
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
// A starting node refuses them — an executor calling back while its node
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
// NEVER A NODE WITHOUT THE ROLE, its seats included: a box off the node's
// loopback reaches the fleet's bridge on any API node, which relays to the
// seat's holder over the broker (internal/api/mcpbridge), and a box sharing it
// dials api.port on loopback ([Probes]), so no route such a box needs lies here.
//
// ONE PREDICATE for `crewlet run` binding the listener before there is an
// engine and serving it once there is one, so a node never binds a public
// listener it then serves nothing on.
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
