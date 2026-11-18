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
// A draining node KEEPS ITS LISTENER: /health has to stay 200 so the
// orchestrator does not kill the node mid-turn, and /ready answers 503 so
// traffic moves elsewhere; a closed listener fails the liveness probe.
//
// A drain must not keep taking work, so the door is this gate rather than the
// listener: from the drain's first moment every request that would start work
// is refused with a 503 naming the drain, and everything else is served until
// the process closes the listener.

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
// New work is refused, everything else served. The default is by method
// ([auth.IsRead]), so a write route added later is refused without being
// listed. /operator/mcp is a write whatever it is asked: stateless, POST-only
// (operator.MCPPattern), and splitting its calls would mean reading the body.
// A listening stream opened BEFORE the drain is held through it and answered at
// shutdown (operator.Listeners.End), since ending it early would cut off a
// client that opens no other stream.
//
// git is r's [auth.GitAccess], which the gate asks once and hands in.
func servedWhileDraining(r *http.Request, git auth.GitOp) bool {
	path := r.URL.Path
	switch {
	// Every delivery is new work, and a GET landing acts too (the GitHub App
	// return seals a credential and writes a revision); no per-route list.
	case strings.HasPrefix(path, auth.WebhookPrefix):
		return false
	// A detached executor outlives its turn, so the drain never waits on one
	// (docs/concepts/code-sandbox.md); refusing its bridge and spans shortens
	// no drain and only breaks a run mid-flight. /mcp/{token} likewise.
	case strings.HasPrefix(path, mcpbridge.PathPrefix), strings.HasPrefix(path, auth.OTLPPrefix):
		return true
	// A fetch begun before the drain continues (git's smart HTTP keeps no
	// state between requests, so it may be on a peer); a NEW one is refused at
	// its advertisement, being an upload-pack tree plus a cache sync. What was
	// served is waited for in [App.DrainGit].
	case git == auth.GitFetch:
		return !auth.GitAdvertisement(r)
	// A push is refused from its advertisement GET, which is where git prints
	// the reason (on the POST it prints only `RPC failed; HTTP 503`);
	// [auth.GitAccess] is the one classifier (a box's git, under
	// [auth.BoxGitPrefix], counts as a person's). Downloads are new transfer work.
	case git == auth.GitPush, git == auth.GitDownload:
		return false
	// Every other read starts nothing and is how an operator watches the drain.
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
			what := "fetch or push"
			if git == auth.GitDownload {
				what = "download"
			}
			auth.GitText(w, http.StatusServiceUnavailable, "this node is draining for a shutdown "+
				"and begins no new "+what+": retry in "+retryAfter+" s, against another "+
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
// A GIT REQUEST IS COUNTED BEFORE IT IS JUDGED, and taken back if refused: a
// push judged first and counted after is invisible to a wait that looks between
// the two, so the listener closes under a push the gate let through. Counted
// first, the count and the wait's look share one lock: if the count wins, the
// wait sees the request; if the look wins, the drain had already begun, so the
// judgement made after the count refuses it.
//
// Never decide under that lock: the judgement asks the engine, and no call out
// of this package is made holding a lock the drain's wait takes.
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
// until none is running and none has ended for [gitExchangeGap], or ctx ends
// (its error). Shutdown runs it once the engine has drained, before closing the
// listener: a git transfer cut part way restarts from nothing on a peer, and
// the listener's own grace ([apiShutdownGrace]) would cut any over five seconds.
// It must run only once the drain has begun, or the gate keeps admitting new
// exchanges and the wait never ends. The count can GROW meanwhile, since upload
// RPCs of fetches begun elsewhere are served until the balancer acts on /ready.
// No deadline of its own: the supervisor's kill grace already bounds shutdown,
// and api/paced ends a request whose client stops reading or sending.
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

// gitExchangeGap is how long a draining node keeps listening after its last git
// request ended, for the next request of an exchange under way. Git's smart
// HTTP is stateless, so ending the instant nothing ran would close the listener
// under a fetch half done ([App.DrainGit]).
//
// FIVE SECONDS, git's own uploadpack.keepAlive default, the longest a healthy
// exchange goes silent. A client's pause is a round trip plus its own work,
// milliseconds to a few hundred, so this holds it many times over and costs a
// shutdown at most five seconds more.
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
