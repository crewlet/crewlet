package coord

import (
	"encoding/json"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// What a node reports about ITSELF rides its presence lease: only the node
// running a seat knows its in-flight turns, drain and config posture, and
// /health answers about whichever node a load balancer picked. Request/reply
// to the owner was refused: partial answers, a new trust edge (an operator's
// authority crossing nodes), and a second mechanism beside the heartbeat that
// already re-sends roles and labels. Freshness is a heartbeat; the fleet view
// prints each lease's time left. The Meta is held to [MaxLeaseMetaBytes],
// derived from every field's bound (the alarm table, [MaxBuildBytes], Tier A's
// labels), so a field added here re-derives it (internal/engine,
// TestThePresenceLeaseAtItsWorstFitsItsBound). Rows a company's configuration
// grows (MCP servers, model entries, runner findings, infra) are
// [NodeStatusRecord]'s, joined by epoch: see nodestatusrecord.go.

// StatusKey is where node status sits in a presence lease's Meta: its own key
// rather than top-level fields, because placement reads `roles` and `labels`
// off the same map and a shared namespace lets one shadow the other.
const StatusKey = "status"

// GitBackgroundKey is where a Gitlet node's presence lease carries its git
// cache's MAINTENANCE BUDGET: the memory every maintenance run on the node is
// admitted against, re-read every pass (internal/gitlet/cache,
// Cache.BackgroundBudget). Every Gitlet node weighs every other by it to place
// maintenance (internal/estate, Router.Maintainer), on the heartbeat for the
// reason status is. BESIDE THE STATUS, NEVER IN IT: a status whose hook
// overruns its share of the beat is dropped, which would weigh a node as
// holding nothing; the budget is in memory, so the seat host writes it on
// EVERY beat, as it does its held count (internal/seat/placement, HeldKey).
// ZERO IS WRITTEN, a claim of nothing to give; an absent key is a node not
// saying ([GitBackgroundFromMeta]). A node without the gitlet role writes none.
const GitBackgroundKey = "git_background_bytes"

// GitBackgroundFromMeta reads a peer's maintenance budget off its presence
// lease's Meta ([GitBackgroundKey]), reporting false where the row says none
// this build can act on: the key absent (a node not saying, never one with
// nothing to give), negative, or not a number. It reads through [intFromMeta]:
// every target is 64-bit (internal/store/platform.go), and a JSON round trip's
// float64 holds a budget exactly up to 2⁵³ bytes.
func GitBackgroundFromMeta(meta map[string]any) (int64, bool) {
	raw, present := meta[GitBackgroundKey]
	if !present {
		return 0, false
	}
	switch raw.(type) {
	case int, int64, float64:
	default:
		return 0, false
	}
	budget := int64(intFromMeta(raw))
	if budget < 0 {
		return 0, false
	}
	return budget, true
}

// NodeStatus is the live half of what a node is.
//
// It carries no struct tags: the wire shape is [NodeStatus.Meta] and
// [StatusFromMeta], which encode by hand because the lease's Meta is a
// `map[string]any` a peer merges into. A second encoding declared in tags
// would be one nothing calls and nothing keeps honest.
type NodeStatus struct {
	// InFlight is how many turns are running.
	InFlight int

	// Draining is true from the moment a drain begins. Reported though a
	// draining node DROPS its presence lease: it is what the last heartbeat
	// said, so a row draining and expiring is a clean shutdown, and one that
	// vanished without it a node that died.
	Draining bool

	// Posture is what this node concluded about its own config lag (serve,
	// wait, shed, isolated or stuck): the only place an operator sees WHY a
	// node left rotation, since /ready is a bare 503 either way. EMPTY when
	// the node could not read its control plane: the reader's fail-open serve
	// is right for its own probe and a confident lie on a peer's screen.
	Posture string

	// StartedAt is when the node's ENGINE was built, which is when the node
	// started; its API reports the same instant on its health body.
	StartedAt time.Time

	// Build is the engine build the node runs, as internal/version names it,
	// and "" where a node said none. [NodeStatus.Meta] carries at most
	// [MaxBuildBytes] of it. Whether every live node runs one build
	// is read off it: a fleet mid-upgrade hands work between builds by design.
	Build string

	// ParkedStateVersion is the version of a parked run's state this node's build
	// reads, zero where a node says none: a run parked at a lower version is
	// settled only once no live node reads it, or says nothing.
	ParkedStateVersion int

	// AppliersReady and AppliersTotal are how many of this node's state-log
	// appliers (one per registered domain, run on every data node) have
	// caught up, out of how many it runs; zero of zero is a node without
	// `data`. Here and not on /ready: a node catching up is HEALTHY and only
	// takes no NEW seats (a seat reading a copy that is behind acts on "no
	// such item"), so it belongs on the fleet view, not out of rotation. Two
	// integers, not a bool: "3 of 5" and "0 of 2" read differently.
	AppliersReady int
	AppliersTotal int

	// AdminPosture is this node's own `api.auth.admin`: `serve` grants admin
	// to those who hold it, `refuse` clamps admin away and keeps every app.
	// It applies per node at decision time and nothing is written when it
	// changes, so nodes that disagree are LEGAL (a rolling restart, a node
	// published to colleagues) yet invisible: an administrator could revoke a
	// credential on one request and not the next. Published so the fleet view
	// shows it. EMPTY IS A NODE NOT SAYING (one serving no API), never one
	// that refuses admin.
	AdminPosture iam.AdminPosture

	// Alarms are the alarms this node's own table has firing as of the beat:
	// ONE MARK PER KIND, longest-standing first, the order the node's health
	// envelope takes `worst` from. Meaningful only where AlarmsEvaluated is
	// true. On the heartbeat because only the node an alarm describes
	// evaluates it, and as the CURRENT set, which a stream of transitions
	// cannot be (a clear lost with its process). KIND AND SINCE, NEVER THE
	// MEASUREMENT: a detail moves every evaluation and is unbounded prose (the
	// node's `alarm_raised` line and retention report carry it); the alarm
	// table bounds the marks, so nothing here needs clipping.
	Alarms []AlarmMark

	// AlarmsEvaluated is whether this node has evaluated an alarm table:
	// false on a node without `data` (no table) and before its first
	// evaluation. A FLAG, NOT A NIL LIST: "nothing firing" and "cannot say"
	// are opposite claims, and a nil slice is also what collecting no marks
	// produces.
	AlarmsEvaluated bool
}

// AlarmMark is one alarm firing on a node: which, and since when.
type AlarmMark struct {
	// Kind is the alarm's name as the published table spells it
	// (docs/reference/alarms.md): `backup_age`, `apply_lag`. A STRING, not
	// the table's type: the table is the state-log framework's, above this
	// package, and a kind a newer build added is still shown by its name.
	Kind string

	// Since is when the node raised it, on its own clock, to the second; for
	// a kind standing on several logs the oldest, so a log joining does not
	// make it look newer. A raise soon after a clear keeps its run's first
	// (statelog.RelapseWindow).
	Since time.Time
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
	if s.Build != "" {
		// CUT to [MaxBuildBytes]: the one field of the core no table
		// bounds, and the presence's bound is a sum over every field.
		out["build"] = cutBuild(s.Build)
	}
	if s.ParkedStateVersion > 0 {
		out["parked_state_version"] = s.ParkedStateVersion
	}
	if s.AdminPosture != "" {
		// Omitted when unset: an empty string would read as a peer whose
		// posture differs from everybody's.
		out["admin"] = string(s.AdminPosture)
	}
	if s.AppliersTotal > 0 {
		// Omitted on a node without `data`, so it reads absent rather than
		// as a stalled "0 of 0".
		out["appliers_ready"] = s.AppliersReady
		out["appliers_total"] = s.AppliersTotal
	}
	if s.AlarmsEvaluated {
		// Present wherever the table was evaluated, as `[]` (never a nil
		// slice's `null`) when nothing fires; absent only where the node
		// cannot say.
		marks := make([]map[string]any, 0, len(s.Alarms))
		for _, m := range s.Alarms {
			mark := map[string]any{"kind": m.Kind}
			if !m.Since.IsZero() {
				mark["since"] = m.Since.UTC().Format(time.RFC3339)
			}
			marks = append(marks, mark)
		}
		out["alarms"] = marks
	}
	return out
}

// StatusFromMeta reads a peer's status off a presence lease's Meta, and
// reports whether the node published one at all. ABSENT IS NOT ZERO: a
// status hook that overran its share of the beat publishes none, and an
// idle row for it is the confident-zero mistake. A key this build does not
// know is IGNORED, not refused: a successor publishes what it adds beside
// what this build reads, and refusing would read it as absent.
func StatusFromMeta(meta map[string]any) (NodeStatus, bool) {
	raw, ok := meta[StatusKey].(map[string]any)
	if !ok {
		return NodeStatus{}, false
	}
	status := NodeStatus{
		InFlight:      intFromMeta(raw["in_flight"]),
		Posture:       stringFromMeta(raw["posture"]),
		AppliersReady: intFromMeta(raw["appliers_ready"]),
		AppliersTotal: intFromMeta(raw["appliers_total"]),
		AdminPosture:  iam.AdminPosture(stringFromMeta(raw["admin"])),
		Build:         stringFromMeta(raw["build"]),

		ParkedStateVersion: max(intFromMeta(raw["parked_state_version"]), 0),
	}
	status.Draining, _ = raw["draining"].(bool)
	if at, err := time.Parse(time.RFC3339, stringFromMeta(raw["started_at"])); err == nil {
		status.StartedAt = at
	}
	status.Alarms, status.AlarmsEvaluated = alarmsFromMeta(raw["alarms"])
	return status, true
}

// alarmsFromMeta reads a status's alarm marks, and whether it carried a list
// at all: an absent key or a non-list is a node not saying. A mark naming no
// kind is dropped, as an MCP row naming no server is; an unparsable since
// keeps the mark with no instant, since the firing is the fact.
func alarmsFromMeta(v any) ([]AlarmMark, bool) {
	switch v.(type) {
	case []any, []map[string]any:
	default:
		return nil, false
	}
	var out []AlarmMark
	for _, r := range listFromMeta(v) {
		mark, ok := r.(map[string]any)
		if !ok {
			continue
		}
		kind := stringFromMeta(mark["kind"])
		if kind == "" {
			continue
		}
		m := AlarmMark{Kind: kind}
		if at, err := time.Parse(time.RFC3339, stringFromMeta(mark["since"])); err == nil {
			m.Since = at
		}
		out = append(out, m)
	}
	return out, true
}

// listFromMeta accepts the typed slice this build writes and the []any a JSON
// round trip returns, for the reason [intFromMeta] accepts both numbers.
func listFromMeta(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
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

// intFromMeta accepts the int this build writes locally, the float64 a JSON
// round trip through the lease store returns and the json.Number a status
// row's decoder keeps ([rowMeta]): a reader knowing one would read every
// peer's in-flight count as zero. EXACT OR ZERO: a value that is no integer,
// or that int or a float64's exact integers cannot hold, reads as unsaid,
// never truncated or wrapped into a count nobody wrote.
func intFromMeta(v any) int {
	n, ok := int64FromMeta(v)
	if !ok || n < math.MinInt || n > math.MaxInt {
		return 0
	}
	return int(n)
}

// maxExactFloat is the largest integer every smaller one of which a float64
// holds exactly, 2^53.
const maxExactFloat = 1 << 53

// int64FromMeta is [intFromMeta] to int64's range, false for a value it reads
// as unsaid.
func int64FromMeta(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float64:
		if n != math.Trunc(n) || n < -maxExactFloat || n > maxExactFloat {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

// floatFromMeta reads a number a status row carries, false for anything else.
func floatFromMeta(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func stringFromMeta(v any) string {
	s, _ := v.(string)
	return s
}

// seatFromMeta reads a seat's agent id, and uuid.Nil for anything that is not
// one in canonical form — a value no build of this one wrote is a seat the
// row does not name, never a seat named wrongly.
func seatFromMeta(v any) uuid.UUID {
	s := stringFromMeta(v)
	id, err := uuid.Parse(s)
	if err != nil || id.String() != s {
		return uuid.Nil
	}
	return id
}

// stringsFromMeta accepts the []string this build writes and the []any a JSON
// round trip returns, keeping only the strings in it; nil when there are none,
// so a row that carried no list reads back as one that carried none.
func stringsFromMeta(v any) []string {
	switch l := v.(type) {
	case []string:
		if len(l) == 0 {
			return nil
		}
		return slices.Clone(l)
	case []any:
		var out []string
		for _, item := range l {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
