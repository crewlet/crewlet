package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/httprelay"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
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
	Keyring statelog.Keyring

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

// Join makes this bridge one of its fleet's: a box's call that reaches its
// route is answered by the node holding the run's seat, wherever that is, and
// a call for a session this node holds is answered here wherever it landed. It
// serves until the returned Unsubscribe runs.
//
// # Why every node joins, and the route is wherever the API is
//
// A session is live state in the process holding its seat (see
// [Bridge.verdict]), and the address a box dials is fixed when the box starts.
// Between the two sit a load balancer in front of several nodes, which sends a
// call wherever it likes, and a seat that moves to a peer mid-run, which
// leaves the box dialling a node that no longer holds it. So the node a call
// lands on reads the seat's lease — the seat is in the token — and relays the
// whole exchange to the holder over the broker (internal/httprelay), event
// stream included. Every node is reached that way, so [BaseURLVar] is ONE
// address for the fleet: any node that serves the route, and a node that
// serves none — one without the ingress role — is still reached through one
// that does.
func (b *Bridge) Join(ctx context.Context, opts FleetOptions) (queue.Unsubscribe, error) {
	if b == nil {
		return nil, errors.New("mcpbridge: no bridge to join to its fleet")
	}
	if opts.Leases == nil || opts.HolderWait <= 0 {
		return nil, errors.New("mcpbridge: joining a fleet needs its seat leases and a holder wait")
	}
	// THE RELAY'S HANDLER IS THE MCP HANDLER ITSELF, behind the route's
	// own pattern so the token reaches it as a path value — never the
	// route's handler, which would relay a relayed call again. And the
	// notes route's, served on the opening the token names here, for the
	// same reason never through its own routing.
	mux := http.NewServeMux()
	mux.Handle(PathPrefix+"{token}", b.streamable)
	mux.Handle(SteerPattern, b.heldOnly(b.serveSteer))
	relay, err := httprelay.New(httprelay.Options{
		Fabric: opts.Fabric, Name: RelayName, Node: opts.Node, Owner: opts.Owner,
		Keyring: opts.Keyring, Admit: b.admitRelayed, Handler: mux,
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

// admitRelayed is what this node can do for a call relayed to it: the same
// verdict it gives a call that reached its own route.
//
// BUT NEVER A LOOPBACK SESSION's ([Session.Loopback]): its token is worth
// something from this host's loopback alone, and a relayed call came from
// wherever the box that sent it is. The client address the entry carries
// cannot stand in for that, because it is the entry's own loopback that it
// would name.
func (b *Bridge) admitRelayed(r *http.Request) httprelay.Verdict {
	// THE TOKEN IS THE FIRST SEGMENT after the prefix, whatever route under
	// it the call is for: the MCP endpoint, or the notes route beneath it.
	token, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, PathPrefix), "/")
	sub, ok := b.parse(token)
	if !ok {
		return httprelay.Refuse
	}
	if opened := b.held(sub); opened != nil && opened.session.Loopback {
		return httprelay.Refuse
	}
	return b.verdict(sub)
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
// holder answers it, here or relayed, or the call waits for one that can. A
// seat's holder is the one its lease names, and a pinned session's the
// incarnation its token names ([route]).
func (b *Bridge) forward(w http.ResponseWriter, r *http.Request, sub subject, f *fleet) {
	if sub.route.pinned() && sub.route.owner == "" {
		b.refuse(w, r, sub.run, "the run's session was opened by a node that had joined "+
			"no fleet, so only that node's own route reaches it")
		return
	}
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
	holds := "which holds the run's seat"
	if sub.route.pinned() {
		holds = "which opened the run's session"
	}
	var why string
	for attempt := 0; ; attempt++ {
		owner, err := sub.route.owner, error(nil)
		if !sub.route.pinned() {
			owner, err = holderOf(ctx, f.leases, sub.route.seat)
		}
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
			case httprelay.Rerouted:
				if sub.route.pinned() {
					// THE PROCESS THAT OPENED IT IS GONE, and its session
					// with it: the node answers as a later incarnation,
					// and nothing takes a pinned session over.
					b.refuse(w, r, sub.run, "the run has ended: "+node+" no longer runs "+
						"the process that opened its session")
					return
				}
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
		}
	}
}

// holderOf is the incarnation a seat's live lease names, or empty while no
// node holds it.
func holderOf(ctx context.Context, leases Leases, seat uuid.UUID) (string, error) {
	lease, err := leases.Get(ctx, coord.SeatResource(seat))
	if err != nil {
		return "", err
	}
	if lease == nil || !lease.Live(time.Now()) {
		return "", nil
	}
	return lease.Owner, nil
}
