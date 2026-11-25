package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/httprelay"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/recordseal"
)

// RelayName scopes the bridge's relay on the broker: its subjects are
// `crewlet.mcpbridge.…` and its signed channels `mcpbridge.open` and
// `mcpbridge.frame` (internal/httprelay). Exported for the engine's gate that
// holds every signed channel's key domains apart.
const RelayName = "mcpbridge"

// Leases reads which incarnation holds a seat: the half of the coordination
// store routing a call needs.
type Leases interface {
	Get(ctx context.Context, resource string) (*coord.Lease, error)
}

// FleetOptions join a bridge to its fleet ([Bridge.Join]).
type FleetOptions struct {
	// Fabric is the broker every node of the fleet reaches.
	Fabric httprelay.Fabric

	// Leases is the fleet's coordination store, which names the
	// incarnation holding each seat.
	Leases Leases

	// Node is this node's id, and Owner this process's incarnation — what a
	// seat's lease names while this process holds it.
	Node  string
	Owner string

	// Keyring is the fleet's: every relayed request and frame is signed
	// under it.
	Keyring recordseal.Keyring

	// Answers is the node's answer budget the relay's OPENs and frames
	// hold their weight of (internal/httprelay).
	Answers *queue.AnswerBudget

	// HolderWait bounds how long a box's call waits for a node that can
	// answer it: through a takeover, the old holder's lease running out
	// and the new one recovering the seat. Required.
	HolderWait time.Duration
}

// fleet is a bridge's membership of its fleet.
type fleet struct {
	relay      *httprelay.Relay
	leases     Leases
	holderWait time.Duration
}

// Join makes this bridge one of its fleet's: a box's call is answered by the
// node holding the run's seat, wherever it landed. It serves until the
// returned Unsubscribe runs.
//
// # Why every node joins
// A session is live state in the process holding its seat ([Bridge.verdict]),
// but a box's address is fixed at start, a load balancer sends a call
// anywhere and a seat can move mid-run. So the node a call lands on reads the
// seat's lease (the seat is in the token) and relays the whole exchange,
// event stream included, to the holder (internal/httprelay). Hence
// [BaseURLVar] is ONE address for the fleet, reaching even a node without
// the ingress role.
func (b *Bridge) Join(ctx context.Context, opts FleetOptions) (queue.Unsubscribe, error) {
	if b == nil {
		return nil, errors.New("mcpbridge: no bridge to join to its fleet")
	}
	if opts.Leases == nil || opts.HolderWait <= 0 {
		return nil, errors.New("mcpbridge: joining a fleet needs its seat leases and a holder wait")
	}
	relay, err := httprelay.New(httprelay.Options{
		Fabric: opts.Fabric, Name: RelayName, Node: opts.Node, Owner: opts.Owner,
		Keyring: opts.Keyring, Admit: b.admitRelayed, Handler: b.relayed(), Answers: opts.Answers,
	})
	if err != nil {
		return nil, fmt.Errorf("mcpbridge: %w", err)
	}
	stop, err := relay.Serve(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcpbridge: %w", err)
	}
	b.fleet.Store(&fleet{relay: relay, leases: opts.Leases, holderWait: opts.HolderWait})
	return func(ctx context.Context) error {
		b.fleet.Store(nil)
		return stop(ctx)
	}, nil
}

// relayed is the handler a call relayed to this node is served by, once the
// relay has admitted it ([Bridge.admitRelayed]).
//
// THE RELAY'S HANDLER IS THE MCP HANDLER ITSELF, behind the route's own
// pattern so the token reaches it as a path value, never the route's
// handler, which would relay a relayed call again: at the job's endpoint, and
// at each of the seat's MCP servers beneath it ([ServerPattern]); and the
// notes, skill and browser routes' own, never their routing. EVERY ROUTE IS
// SERVED ON THE OPENING HELD HERE ([Bridge.heldOnly]), behind the step that
// holds it to the session the box started ([Bridge.serveBegun]), and through
// [Bridge.serveMCP] as this node's own route is: a relayed request is as much
// the run's first, and the MCP handler alone would serve it before that.
func (b *Bridge) relayed() http.Handler {
	mux := http.NewServeMux()
	streamable := b.heldOnly(b.serveMCP)
	mux.Handle(PathPrefix+"{token}", streamable)
	mux.Handle(ServerPattern, streamable)
	mux.Handle(SteerPattern, b.heldOnly(b.serveSteer))
	mux.Handle(SkillPattern, b.heldOnly(b.serveSkill))
	mux.Handle(BrowserPattern, b.heldOnly(b.serveBrowser))
	return mux
}

// admitRelayed is what this node can do for a call relayed to it: the same
// verdict it gives a call that reached its own route.
func (b *Bridge) admitRelayed(r *http.Request) httprelay.Verdict {
	// THE TOKEN IS THE FIRST SEGMENT after the prefix, whatever route under
	// it the call is for: the MCP endpoint, one of the seat's MCP servers
	// beneath it, or the notes, skill or browser routes.
	token, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, PathPrefix), "/")
	sub, ok := b.parse(token)
	if !ok {
		return httprelay.Refuse
	}
	_, verdict := b.verdict(sub)
	return verdict
}

// The pace a waiting call reads the seat's lease again at. A takeover is
// measured in a lease's lifetime, so the ceiling only has to keep a call from
// waking far more often than the lease can change; the base answers the
// common case — a holder a moment from finishing taking the seat — within a
// round trip or two.
const (
	holderRetryBase    = 100 * time.Millisecond
	holderRetryCeiling = 2 * time.Second
)

// forward answers a call this node holds no open session for: the session's
// holder answers it, here or relayed, or the call waits for one that can. The
// holder is the one its seat's lease names ([route]).
//
// A STOPPING NODE WAITS FOR NO HOLDER ([Bridge.Shutdown]). A waiting call has
// been taken by no node, and one that kept waiting would hold the listener
// busy through the graceful stop (up to [FleetOptions.HolderWait], two
// seat-lease lifetimes) and then be cut, which a box's client reads as a
// broken connection, not a retry. So the wait ends at shutdown with an answer
// that this node is draining and when to retry, landing on a node that stays.
// A call already handed to its holder finishes, as [Bridge.forwardPoll] has it.
func (b *Bridge) forward(w http.ResponseWriter, r *http.Request, sub subject, f *fleet) {
	req, err := httprelay.Read(r)
	switch {
	case errors.Is(err, httprelay.ErrTooLarge):
		httpjson.Fail(w, http.StatusRequestEntityTooLarge, httpjson.CodeBodyTooLarge)
		return
	case err != nil:
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeUnreadableBody)
		return
	}
	ctx := r.Context()
	deadline := time.Now().Add(f.holderWait)
	const holds = "which holds the run's seat"
	var why string
	for attempt := 0; ; attempt++ {
		owner, err := coord.Holder(ctx, f.leases, coord.SeatResource(sub.route.seat))
		switch {
		case err != nil:
			why = "which node holds the run's seat could not be read: " + err.Error()
		case owner == "":
			why = "no node holds the run's seat"
		default:
			// THE HOLDER IS ASKED THE SAME WAY WHEREVER IT IS, this node
			// included: one path decides every call, and a node's own
			// relay answers it as a peer's would.
			outcome, reason := f.relay.Forward(ctx, w, req, owner)
			node := config.IncarnationNode(owner)
			switch outcome {
			case httprelay.Served:
				return
			case httprelay.Refused:
				b.refuse(w, r, sub.run, "the run has ended: "+node+", "+holds+", "+
					"holds no session for its job")
				return
			case httprelay.Lost:
				// THE CALL MAY HAVE RUN, so it is not offered to anyone
				// else: the box's client decides whether to repeat it, as
				// it would after any connection that broke mid-answer.
				log.WarnContext(ctx, "mcp_bridge_holder_lost", "run_id", sub.run, "node", node,
					"reason", reason)
				httpjson.Fail(w, http.StatusBadGateway, httpjson.CodeUnavailable)
				return
			}
			why = node + ", " + holds + ", did not take the call: " + reason
		}
		if ctx.Err() != nil {
			return
		}
		pause := backoff.Jitter(backoff.Doubling(attempt, holderRetryBase, holderRetryCeiling), 0.2)
		if time.Now().Add(pause).After(deadline) {
			log.WarnContext(ctx, "mcp_bridge_no_holder", "run_id", sub.run, "waited", f.holderWait.String(),
				"reason", why)
			httpjson.Unavailable(w, httpjson.CodeUnavailable, httpjson.RetrySeconds(holderRetryCeiling))
			return
		}
		timer := time.NewTimer(pause)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		case <-b.shutdown:
			timer.Stop()
			httpjson.Unavailable(w, httpjson.CodeDraining, httpjson.RetrySeconds(holderRetryCeiling))
			return
		}
	}
}
