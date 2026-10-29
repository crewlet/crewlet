package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
)

// The drain gate.
//
// A draining node KEEPS ITS LISTENER, because the drain is exactly when an
// orchestrator is watching it: /health has to stay 200 so the node is not
// killed in the middle of the turns the drain exists to finish, and /ready has
// to answer 503 so traffic moves elsewhere. A node that closed its listener
// before draining answered neither, and a liveness probe that cannot connect
// is a liveness probe that fails.
//
// What a drain must not do is keep taking work. It waits for the turns already
// running, and a surface still admitting deliveries would keep making more. So
// the door is this gate rather than the listener: from the first moment of a
// drain every request that would start work is refused with a 503 naming the
// drain, and everything else is served until the drain completes and the
// process closes the listener.

// DrainRetryAfter is what a refusal tells the caller to wait.
//
// THIRTY SECONDS, from what takes a draining node out of rotation. /ready
// answers 503 from the drain's first moment, but a load balancer acts on a
// probe only after its own period and failure threshold, which on Kubernetes'
// defaults is three failures ten seconds apart. A retry sooner than that
// mostly reaches this same node again; one after it reaches a peer, or on a
// single node the process that restarted.
const DrainRetryAfter = 30 * time.Second

// servedWhileDraining reports whether a request is still served once this node
// has begun to drain.
//
// The whole rule, in one function:
//
//   - The webhook edge is REFUSED, whatever the method and whatever the route.
//     A delivery is new work by definition, and one of the edge's two GET
//     routes is a landing that acts: the GitHub App return converts a creation
//     code into a sealed credential and a config revision, and an install
//     arrival asks the reconcile loop for a pass. The Slack OAuth landing only
//     renders a page, and it goes with the rest rather than being carved out,
//     because a per-route list is what the default below exists to avoid.
//   - The sandbox bridge and the telemetry edge are SERVED. They carry the tool
//     calls and the spans of coding runs that started before the drain, and a
//     detached run OUTLIVES the turn that started it: the turn suspended the
//     moment the run detached, so the drain never waits on one (see
//     docs/concepts/code-sandbox.md, where being detached is what keeps a
//     coding job out of the drain). Refusing these therefore shortens no drain
//     and does nothing but break a run mid-flight against the node still
//     holding its bridge session and its receiver.
//   - A git FETCH ALREADY BEGUN is SERVED: the upload-pack RPC, a POST, which
//     continues a fetch whose advertisement was answered before the drain —
//     here or on a peer, since git's smart HTTP keeps no state between an
//     exchange's requests — and a refusal would break it half done. A NEW
//     fetch is REFUSED at its advertisement ([auth.GitAdvertisement]), which
//     is where every fetch begins: a fetch is not "a read that starts
//     nothing" but an upload-pack process tree on the transfer lane and the
//     cache's sync from the object store before it, new work the shutdown
//     would have to wait out. And what was served is waited for: the node's
//     shutdown finishes every git request it is serving before it closes the
//     listener ([App.DrainGit]), since a clone cut part way is begun again
//     from nothing.
//   - A git PUSH is REFUSED, every request of it, the advertisement GET
//     included: refusing at the GET is what lets git print the reason, which
//     on the POST it does not — it prints only `RPC failed; HTTP 503`. Which
//     is which is [auth.GitAccess]'s answer, the one classifier the guard and
//     the code host ask too, so a drain can never take for a fetch a request
//     the host would serve as a push. A coding box's git, under
//     [auth.BoxGitPrefix], is classified as a person's and drained by these
//     same two rules.
//   - Every other read is SERVED: the probes, the dashboard, the REST reads and
//     the live socket. A read starts nothing, and it is how an operator watches
//     the drain.
//   - Every other write is REFUSED. A config write activates a revision, a
//     setup write runs a vendor's pass, a backup copies the estates the
//     teardown is about to close, and the operator surface — MCP and the act
//     transport alike — files and moves work. Refusing by default is what
//     keeps a write route added later from being admitted through a drain
//     because nobody listed it.
//
// /operator/mcp is a write by this rule whatever it is asked. It is served
// STATELESS (see internal/api/operator): every call is a POST carrying its own
// credential, there is no server-to-client stream for a GET to open and no
// session for a DELETE to end, and the route is mounted for POST alone
// (operator.MCPPattern), so either of those meets the mux's own 405 — a read of
// nothing, which starts nothing either. So every JSON-RPC call it serves, reads
// included, is refused with the writes: split by call, the gate would have to
// read the body to tell a listing from a filed task, which is the per-route
// carve-out the default exists to avoid, and an assistant asking a draining
// node is told to retry elsewhere, as the dashboard is. A listening stream
// (`subscriptions/listen`, a POST of its own) opened BEFORE the drain is no
// request this gate sees again: it is held through the drain, still told of
// every other node's move of the operator tools, and answered as the listener
// shuts down (operator.Listeners.End) — ending it at the drain's first moment
// would cut off a client that does not open another stream for the drain's
// whole length. /mcp/{token} is the one MCP surface that DOES hold sessions,
// and it is served through a drain for the reason the bridge bullet above
// gives.
//
// git is r's [auth.GitAccess], which the gate asks once and hands in.
func servedWhileDraining(r *http.Request, git auth.GitOp) bool {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, auth.WebhookPrefix):
		return false
	case strings.HasPrefix(path, mcpbridge.PathPrefix), strings.HasPrefix(path, auth.OTLPPrefix):
		return true
	case git == auth.GitFetch:
		return !auth.GitAdvertisement(r)
	case git == auth.GitPush:
		return false
	default:
		return auth.IsRead(r.Method)
	}
}

// drainGate refuses new work once this node has begun to drain.
//
// ONE GATE FOR BOTH LISTENERS: a node with a public listener serves it the
// same chain api.port is served (see [App.Public]), so the webhook edge there
// is refused and the sandbox edges served by exactly this rule.
//
// Inside the auth guard, so a credential is still the first question a guarded
// route asks, and inside the browser posture, so a cross-origin dashboard can
// read the refusal rather than seeing an opaque network error.
func (a *App) drainGate(next http.Handler) http.Handler {
	retryAfter := strconv.Itoa(int(DrainRetryAfter / time.Second))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		git := auth.GitAccess(r)
		if a.admit(r, git) {
			if git != auth.GitNone {
				defer a.git.end()
			}
			next.ServeHTTP(w, r)
			return
		}
		// INFO, not a warning: this is the drain doing its job. The path
		// only, never the query, which is where a Confluence Cloud
		// delivery carries its shared token.
		log.Info("api_refused_while_draining", "method", r.Method, "route", r.URL.Path)
		w.Header().Set("Retry-After", retryAfter)
		if git != auth.GitNone {
			// In git's form: git prints a text/plain refusal as
			// `remote: …` and the API's JSON as nothing at all.
			auth.GitText(w, http.StatusServiceUnavailable, "this node is draining for a shutdown "+
				"and begins no new fetch or push: retry in "+retryAfter+" s, against another "+
				"node or once it has restarted")
			return
		}
		httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeDraining,
			map[string]string{
				"detail": "this node is draining for a shutdown: the turns already " +
					"running finish, and nothing new is started here",
				"hint": "retry against another node, or once this one has restarted; " +
					"/ready answers 503 for as long as the drain lasts",
			})
	})
}

// admit reports whether the drain gate lets r through, and COUNTS a git request
// it lets through for [App.DrainGit] — counted until [gitServing.end].
//
// A GIT REQUEST IS COUNTED BEFORE IT IS JUDGED, and taken back if it is
// refused, because the other order has a gap the wait can fall into: a push
// told the node is not draining, and only then counted, is invisible to a wait
// that looks between the two — the drain having begun, the turns finished and
// the wait found nothing running in that instant — so the listener closes
// under a push the gate let through. Counted first, the count and the wait's
// look take one lock, so one of them comes first: if the count does, the wait
// sees the request; if the look does, the drain began before it (the shutdown
// waits for git only once the drain has begun), so the judgement made after
// the count sees the drain and refuses everything it refuses during one.
//
// Never the other way round, deciding under that lock: the judgement asks the
// engine, and no call out of this package is made holding a lock the drain's
// wait takes.
func (a *App) admit(r *http.Request, git auth.GitOp) bool {
	if git == auth.GitNone {
		return servedWhileDraining(r, git) || !a.draining()
	}
	a.git.begin()
	if servedWhileDraining(r, git) || !a.draining() {
		return true
	}
	a.git.abandon()
	return false
}

// draining reports whether this node has begun to drain. A process with no
// engine has nothing to drain, so it never is.
func (a *App) draining() bool { return a.runtime != nil && a.runtime.ShuttingDown() }

// DrainGit waits until this node has finished the git requests it is serving:
// until none is running and none has ended for [gitExchangeGap]. It is the git
// half of a node's drain, which crewlet's shutdown runs once the engine has
// drained and before it closes the listener, and it answers ctx's error if ctx
// ends first.
//
// WHY THE SHUTDOWN WAITS FOR THEM. A git transfer cut part way is not resumed:
// the client begins it again from nothing, on a peer, so the fleet pays its
// pack, its transfer slot and its sync from the object store twice — at every
// rolling restart, for every clone and push that happened to be running. The
// listener's own grace ([apiShutdownGrace] in crewlet) is sized for a REST
// read, and would cut every transfer longer than five seconds.
//
// WHAT IT WAITS FOR. Every git request the drain gate let through before the
// wait's last look — a push or a fetch begun before the drain, and a fetch's
// upload RPC served during it — and then the quiet an exchange's next request
// would break: git's smart HTTP keeps no state between an exchange's requests,
// so "nothing running" alone would close the listener between two rounds of
// one fetch ([gitExchangeGap]). Nothing else: a request the gate refused
// started nothing, and every other request the listener is serving — a
// dashboard's socket, a coding run's bridge session — is the listener's grace
// to end, as is an upload RPC let through in the instant after the wait ended
// and before the listener closed.
//
// WHEN IT ENDS. From the drain's first moment the gate begins no fetch or push
// here — a fetch's advertisement and every request of a push are refused — but
// it goes on serving every upload RPC that reaches it, since each continues a
// fetch already begun, here or on a peer. So the count can GROW while this
// waits, each such request a whole pack and a fresh gap: they reach a draining
// node for as long as a load balancer still sends it requests — until the
// balancer has acted on /ready's 503 ([DrainRetryAfter]), or, under
// Kubernetes, until the pod is out of its Service's endpoints, which the
// deployment guide's preStop sleep gives time for — and a client holding a
// token can send one with no advertisement before it at all. The wait
// therefore ends once that traffic has moved elsewhere and what was running
// is done, and what bounds it is the supervisor (below), never the gate. It
// must also run only once the drain has begun: before it, the gate begins
// every new exchange, and the wait need never end.
//
// NO DEADLINE OF ITS OWN, for the reason the turns' wait has none: what bounds
// a shutdown is whatever supervises the process — a container runtime's kill
// grace, the operator's second interrupt — which already has a bound and can
// see things this process cannot, so a second one here could only disagree
// with it. Nor can a client that stopped taking part hold it open: the code
// host holds every git connection to the transfer pace (api/paced), which
// ends a request whose client stops reading its answer or sending its body.
func (a *App) DrainGit(ctx context.Context) error {
	started := time.Now()
	reported := false
	for {
		running, ended, moved := a.git.state()
		quiet := a.gitGap - time.Since(ended)
		if running == 0 && quiet <= 0 {
			if reported {
				log.InfoContext(ctx, "git_drain_complete", "waited", time.Since(started).Round(time.Millisecond))
			}
			return nil
		}
		if !reported {
			// ONCE, when there is something to wait for: a node that
			// served no git says nothing, and this line is what an
			// operator reads to learn why the listener is still open
			// after drain_complete.
			reported = true
			log.InfoContext(ctx, "git_draining", "running", running,
				"detail", "the git requests this node is serving finish before its listener closes",
				"hint", "a second interrupt exits at once, and a clone or push it cuts is begun again by its client")
		}
		var gap <-chan time.Time
		var timer *time.Timer
		if running == 0 {
			timer = time.NewTimer(quiet)
			gap = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			log.WarnContext(ctx, "git_drain_abandoned", "running", running, "error", ctx.Err())
			return ctx.Err()
		case <-moved:
		case <-gap:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// gitExchangeGap is how long a draining node goes on listening, once the last
// git request it served has ended, for the next request of an exchange still
// under way.
//
// GIT'S SMART HTTP IS STATELESS: a fetch is one request after another — the
// advertisement, then v2's ls-refs and its fetch, or v0's negotiation a round
// a POST — each sent only once the client has read the answer before it.
// Between two of them this node is serving nothing of that fetch, so a drain
// that ended the instant nothing was running would close the listener under a
// fetch half done, and its next request would meet a closed port: exactly the
// cut [App.DrainGit] exists to spare it.
//
// FIVE SECONDS, git's own uploadpack.keepAlive default: the longest a git
// server lets a healthy exchange go silent before it speaks up, so that nothing
// on the path takes the exchange for dead. A client's pause between two
// requests is a round trip and its own work on the last answer — milliseconds
// inside one network, a few hundred across the world — so this holds it many
// times over, and costs a shutdown that served git in its last five seconds at
// most five seconds more.
const gitExchangeGap = 5 * time.Second

// gitServing counts the git requests the drain gate let through and has not
// seen answered, and when the last of them was answered. Its zero value is
// ready: nothing running, nothing ever answered.
type gitServing struct {
	mu sync.Mutex
	// running counts the requests begun and not yet ended or abandoned —
	// for an instant, a request the gate is about to refuse ([App.admit]).
	running int
	// ended is when the last request was answered, on the monotonic clock —
	// never the app's own, which a test fixes, since a gap measured on a
	// fixed clock would never pass.
	ended time.Time
	// moved is closed at the next begin or end and then forgotten, so a
	// waiter holding it wakes on the change rather than polling; nil until
	// somebody waits.
	moved chan struct{}
}

// begin counts one git request, before the gate judges it.
func (g *gitServing) begin() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running++
	g.wake()
}

// end counts one git request answered, which is when an exchange's next request
// is waited for from ([gitExchangeGap]).
func (g *gitServing) end() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running--
	g.ended = time.Now()
	g.wake()
}

// abandon takes back a git request the gate counted and then refused. It wakes
// a wait that saw it counted, and leaves when the last request was answered
// where it was: a refused request started nothing, so no exchange's next
// request follows it and no gap is owed for it.
func (g *gitServing) abandon() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running--
	g.wake()
}

// wake tells every waiter the count moved. Under mu.
func (g *gitServing) wake() {
	if g.moved != nil {
		close(g.moved)
		g.moved = nil
	}
}

// state is the count, when the last request was answered, and a channel
// closed at the next change.
func (g *gitServing) state() (running int, ended time.Time, moved <-chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.moved == nil {
		g.moved = make(chan struct{})
	}
	return g.running, g.ended, g.moved
}
