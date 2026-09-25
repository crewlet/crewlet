package coord

import (
	"time"

	"github.com/crewlet/crewlet/internal/textcut"
)

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
// a caller asks it directly. That was refused: every answer would become partial
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

	// StartedAt is when the node's ENGINE was built, which is when the node
	// started: its API runs in the same process and reports the same
	// instant on its own health body.
	StartedAt time.Time

	// Projections is how many of this node's document projections have
	// caught up, out of how many it runs. Zero of zero is a node running
	// no native backend, which is every company on the vendor ones.
	//
	// # Why it is here and not on /ready
	//
	// A node mid-hydration is HEALTHY. It serves its dashboard, answers
	// its probes and runs the duties it holds; what it does not do is
	// take on NEW seats, because a seat whose tools read an incomplete
	// projection answers "there is no such item" — and a seat acts on
	// that. So the fact belongs where an operator asking "why is the new
	// node holding nothing" can see it, which is the fleet view, and NOT
	// where it would take the node out of rotation.
	//
	// Two integers rather than a bool: "3 of 5" and "0 of 2" are the two
	// readings an operator needs to tell apart, and a bool collapses
	// them.
	ProjectionsReady int
	ProjectionsTotal int

	// Features is every [Feature] this node's build honours — see
	// features.go for why a gesture asks the fleet before it is offered.
	//
	// EMPTY IS "HONOURS NONE", and that is the reading an older peer has
	// to get: a build that predates the field publishes a status without
	// it, and a gate reading that as "unknown" would refuse the gesture as
	// retryable for as long as the older node runs, instead of saying why.
	// A node that published NO STATUS at all is the other case, and
	// [StatusFromMeta] already reports it as absent.
	Features []Feature

	// MCP is what this node's MCP servers did when it last started them,
	// one row per configured server. Nil is "this node started none"
	// ONLY where [FeatureMCPStatus] is advertised; on an older peer it is
	// "did not say".
	//
	// On the heartbeat rather than asked for, for the reason the rest of
	// this struct is: every node already re-sends it, every peer already
	// reads it, and the settings screen asking each node in turn would be
	// the fan-out whose partial answers the file head refused.
	MCP []MCPServerStatus
}

// MCPServerStatus is one configured MCP server, as ONE node started it.
//
// AGGREGATED PER SERVER, NOT PER CHILD. A per-role server is a template with
// one child per seat this node holds, and a row per child would put a
// company's seat count times its per-role servers on a lease that is re-sent
// every heartbeat — to answer a question ("is the jira server working on this
// node?") the counts answer as well.
type MCPServerStatus struct {
	// Server is the config's own name for it — `mcp_servers[].name`, never
	// a per-seat instance name, since the row stands for every instance.
	Server string

	// Shared is true for the one company-wide child, false for a per-role
	// template.
	Shared bool

	// Started is how many instances started and listed their tools.
	// What happened to a child AFTER it started is not observed here:
	// the bridge learns of a child that died only when its next call
	// fails, and that call's own result is where that is reported.
	Started int

	// Failed is how many did not: a child that would not start or list
	// its tools, and a config whose ${VAR} references did not resolve
	// into a launchable spec.
	Failed int

	// Tools is how many tools one started instance serves — the largest,
	// where the instances of one template disagree.
	Tools int

	// Error is ONE failed instance's reason, and ErrorSeat the seat it
	// belonged to (empty for a shared server). One rather than all, for
	// the size argument above; the first by seat handle, so two beats
	// with the same failures publish the same row.
	Error     string
	ErrorSeat string
}

// MaxMCPErrorBytes bounds [MCPServerStatus.Error] on the wire.
//
// A start failure's text is a handshake error or a child's last stderr line,
// and the useful part — "command not found", "401", "connection refused" — is
// at its head. 240 bytes carries that for every server a company runs while
// keeping each row a small fraction of a lease payload re-sent every
// heartbeat; the full text is in the node's own log line
// (`mcp_server_failed`), which is where a person fixing it reads anyway.
const MaxMCPErrorBytes = 240

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
	if s.ProjectionsTotal > 0 {
		// OMITTED where the node runs none, so a peer on the vendor
		// backends reads absent rather than "0 of 0" — which a reader
		// would otherwise have to know is not a stalled projection.
		out["projections_ready"] = s.ProjectionsReady
		out["projections_total"] = s.ProjectionsTotal
	}
	if len(s.Features) > 0 {
		features := make([]string, len(s.Features))
		for i, f := range s.Features {
			features[i] = string(f)
		}
		out["features"] = features
	}
	if len(s.MCP) > 0 {
		rows := make([]map[string]any, len(s.MCP))
		for i, m := range s.MCP {
			row := map[string]any{
				"server":  m.Server,
				"shared":  m.Shared,
				"started": m.Started,
				"failed":  m.Failed,
				"tools":   m.Tools,
			}
			if m.Error != "" {
				row["error"] = textcut.Within(m.Error, MaxMCPErrorBytes)
			}
			if m.ErrorSeat != "" {
				row["error_seat"] = m.ErrorSeat
			}
			rows[i] = row
		}
		out["mcp"] = rows
	}
	return out
}

// StatusFromMeta reads a peer's status off a presence lease's Meta, and
// reports whether the node published one at all.
//
// # Absent is not zero
//
// A node that publishes no status (a peer running a build older than the
// field) is not a node with no work in flight. Reporting it as 0 would draw an
// idle row for a process that is simply not saying, which is the confident-zero
// mistake the whole surface is written to avoid.
func StatusFromMeta(meta map[string]any) (NodeStatus, bool) {
	raw, ok := meta[StatusKey].(map[string]any)
	if !ok {
		return NodeStatus{}, false
	}
	status := NodeStatus{
		InFlight:         intFromMeta(raw["in_flight"]),
		Posture:          stringFromMeta(raw["posture"]),
		ProjectionsReady: intFromMeta(raw["projections_ready"]),
		ProjectionsTotal: intFromMeta(raw["projections_total"]),
	}
	status.Draining, _ = raw["draining"].(bool)
	if at, err := time.Parse(time.RFC3339, stringFromMeta(raw["started_at"])); err == nil {
		status.StartedAt = at
	}
	for _, f := range listFromMeta(raw["features"]) {
		if name := stringFromMeta(f); name != "" {
			status.Features = append(status.Features, Feature(name))
		}
	}
	for _, r := range listFromMeta(raw["mcp"]) {
		row, ok := r.(map[string]any)
		if !ok {
			continue
		}
		server := stringFromMeta(row["server"])
		if server == "" {
			// A row naming no server describes nothing a reader could
			// attribute, so it is dropped rather than drawn unnamed.
			continue
		}
		m := MCPServerStatus{
			Server:    server,
			Started:   intFromMeta(row["started"]),
			Failed:    intFromMeta(row["failed"]),
			Tools:     intFromMeta(row["tools"]),
			Error:     stringFromMeta(row["error"]),
			ErrorSeat: stringFromMeta(row["error_seat"]),
		}
		m.Shared, _ = row["shared"].(bool)
		status.MCP = append(status.MCP, m)
	}
	return status, true
}

// listFromMeta accepts the typed slices this build writes and the []any a JSON
// round trip returns, for the reason [intFromMeta] accepts both numbers.
func listFromMeta(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	case []string:
		out := make([]any, len(l))
		for i, s := range l {
			out[i] = s
		}
		return out
	case []map[string]any:
		out := make([]any, len(l))
		for i, m := range l {
			out[i] = m
		}
		return out
	default:
		return nil
	}
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
