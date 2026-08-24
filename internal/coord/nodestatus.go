package coord

import "time"

// What a node reports about ITSELF, fleet-wide.
//
// # Why this rides on the presence lease
//
// Only the node running a seat knows how many turns are in flight on it,
// whether it has begun draining, and what it concluded about its own config
// lag. `/health` answers those about the node that served the request, so
// behind a load balancer a refresh tells a different story each time.
//
// The alternative was request/reply: the lease table locates the owner, and
// a caller asks it directly. That was refused — see
// rewrite/decisions/501-node-runtime.md. Every answer would become partial
// (some nodes reply, some time out), it opens a new trust edge (a request
// carrying an operator's authority across a node boundary), and it
// duplicates a mechanism that already works.
//
// The presence lease is renewed on a timer and its Meta is re-sent on EVERY
// heartbeat — that is not an addition, it is how a node already advertises
// its roles and labels. Live status goes beside them.
//
// Freshness is the heartbeat interval, which is exactly the freshness every
// other column of the fleet view already has, and the view prints how long
// each lease has left — so a stale row reads as stale rather than as
// current.

// StatusKey is where node status sits in a presence lease's Meta.
//
// Its own key rather than fields at the top level: placement reads `roles`
// and `labels` off the same map, and two concerns sharing a namespace is how
// one of them eventually shadows the other.
const StatusKey = "status"

// NodeStatus is the live half of what a node is.
//
// It carries no struct tags: the wire shape is [NodeStatus.Meta] and
// [StatusFromMeta], which encode by hand because the lease's Meta is a
// `map[string]any` a peer merges into. A second encoding declared in tags
// would be one nothing calls and nothing keeps honest.
type NodeStatus struct {
	// InFlight is how many turns are running.
	InFlight int

	// Draining is true from the moment a drain begins.
	//
	// Reported even though a draining node DROPS its presence lease: the
	// drop is what takes it out of the placement count, and this is what
	// the last heartbeat before it said. A row that is both draining and
	// expiring is a node shutting down cleanly; one that vanished without
	// it is a node that died.
	Draining bool

	// Posture is what this node concluded about its own config lag —
	// serve, wait, shed, isolated or stuck. The only place an operator can
	// see WHY a node left rotation: /ready reports a bare 503 either way,
	// and "draining" and "cannot apply epoch 41" call for opposite
	// responses.
	Posture string

	// StartedAt is when the ENGINE started, which on a split deployment is
	// a different process on a different clock from the API's own start.
	StartedAt time.Time
}

// Meta renders the status for a lease's Meta map.
func (s NodeStatus) Meta() map[string]any {
	out := map[string]any{
		"in_flight": s.InFlight,
		"draining":  s.Draining,
	}
	if s.Posture != "" {
		out["posture"] = s.Posture
	}
	if !s.StartedAt.IsZero() {
		out["started_at"] = s.StartedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// StatusFromMeta reads a peer's status off a presence lease's Meta, and
// reports whether the node published one at all.
//
// # Absent is not zero
//
// A node that publishes no status — an older build, or one whose engine is
// not co-located — is not a node with no work in flight. Reporting it as 0
// would draw an idle row for a process that is simply not saying, which is
// the confident-zero mistake the whole surface is written to avoid.
func StatusFromMeta(meta map[string]any) (NodeStatus, bool) {
	raw, ok := meta[StatusKey].(map[string]any)
	if !ok {
		return NodeStatus{}, false
	}
	status := NodeStatus{
		InFlight: intFromMeta(raw["in_flight"]),
		Posture:  stringFromMeta(raw["posture"]),
	}
	status.Draining, _ = raw["draining"].(bool)
	if at, err := time.Parse(time.RFC3339, stringFromMeta(raw["started_at"])); err == nil {
		status.StartedAt = at
	}
	return status, true
}

// intFromMeta accepts the int this build writes and the float64 a JSON round
// trip through the lease store returns.
//
// Both, because the same map is read locally (where it is an int) and after
// a store round trip (where it is not), and a reader that knew only one
// would report every peer's in-flight count as zero.
func intFromMeta(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func stringFromMeta(v any) string {
	s, _ := v.(string)
	return s
}
