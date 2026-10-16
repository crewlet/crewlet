package coord

import (
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
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

	// AppliersReady and AppliersTotal are how many of this node's
	// state-log appliers — one per registered domain, run from boot on
	// every data node whatever its company runs — have caught up with
	// their logs, out of how many it runs. Zero of zero is a node without
	// `data`, which runs no log.
	//
	// # Why it is here and not on /ready
	//
	// A node still catching up is HEALTHY. It serves its dashboard,
	// answers its probes and runs the duties it holds; what it does not do
	// is take on NEW seats, because a seat whose tools read a copy that is
	// behind answers "there is no such item" — and a seat acts on that.
	// So the fact belongs where an operator asking "why is the new node
	// holding nothing" can see it, which is the fleet view, and NOT where
	// it would take the node out of rotation.
	//
	// Two integers rather than a bool: "3 of 5" and "0 of 2" are the two
	// readings an operator needs to tell apart, and a bool collapses
	// them.
	AppliersReady int
	AppliersTotal int

	// AdminPosture is this node's own `api.auth.admin`: whether it grants
	// admin to those who hold it (`serve`) or clamps admin away and keeps
	// every app (`refuse`).
	//
	// # Why a fleet publishes this at all
	//
	// Because the posture is applied at DECISION time, per node, per
	// request, and nothing is written when it changes. That is what makes
	// a fleet whose nodes disagree a LEGAL state rather than a broken one —
	// one node published to colleagues refusing admin, another an
	// administrator signs in on serving it, or a rolling restart halfway
	// through a change.
	//
	// Legal and invisible is the problem. Without this, whether an
	// administrator can run the company depends on which node a load
	// balancer sent them to, and the symptom is a person who can revoke a
	// credential on one request and cannot on the next, with nothing
	// anywhere saying why. One word per node turns that into a row on the
	// fleet view.
	//
	// THE VALUE ITSELF AND NOT A DIGEST, which is what the list of grants it
	// replaced had to publish: two values are one short word, so there is
	// nothing to compress.
	//
	// EMPTY IS A NODE THAT IS NOT SAYING, never a node that refuses admin:
	// a node that serves no API has no posture to state and publishes
	// nothing here, and reading that as "refuses" would draw every such node
	// as locked down.
	AdminPosture iam.AdminPosture

	// MCP is what this node's MCP servers did when it last started them,
	// one row per configured server. Nil is "this node started none": a
	// node that did not say is one whose heartbeat carried no status at
	// all, which [StatusFromMeta] reports as absent.
	//
	// On the heartbeat rather than asked for, for the reason the rest of
	// this struct is: every node already re-sends it, every peer already
	// reads it, and the settings screen asking each node in turn would be
	// the fan-out whose partial answers the file head refused.
	MCP []MCPServerStatus
}

// MCPServerStatus is one configured MCP server, as ONE node runs it at the
// beat.
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

	// Started is how many instances are serving: started, listed their
	// tools, and still up as of this beat. A child that died since is not
	// counted here but under Failed until its bridge has brought it back.
	Started int

	// Failed is how many are not: a child that would not start or list
	// its tools, one that died after starting and is being restarted, a
	// config that did not resolve into a launchable spec, and a seat's
	// child whose credential resolves to nothing that nothing will mint,
	// or that the integration minting it cannot mint until a person acts.
	Failed int

	// Waiting is how many this node holds back UNSTARTED, and on purpose:
	// a seat's child whose credential a provisioning pass mints for it and
	// has not sealed yet, where nothing that pass last reported stops it.
	// Neither started nor failed, because nothing is wrong — it starts
	// once the seal reaches the node — and counting it with either would
	// tell the person reading it the opposite of that.
	Waiting int

	// Tools is how many tools one started instance serves — the largest,
	// where the instances of one template disagree.
	Tools int

	// Error is ONE failed instance's reason, and ErrorSeat the seat it
	// belonged to, by its AGENT ID (uuid.Nil for a shared server). One
	// rather than all, for the size argument above; the first by agent id,
	// so two beats with the same failures publish the same row.
	//
	// THE ID RATHER THAN THE HANDLE, because this row is published on a
	// lease every peer reads, and a peer reads it against its OWN org: the
	// id names the seat the instance was launched for — the agent id
	// derived from its handle (ADR-0013) — and the reader resolves it to
	// that handle.
	Error     string
	ErrorSeat uuid.UUID

	// Wait is ONE waiting instance's sentence — the variables it waits for
	// and the integration that mints them — and WaitSeat its seat, chosen
	// and bounded as Error and ErrorSeat are.
	Wait     string
	WaitSeat uuid.UUID

	// Unlisted are the entry's tool_annotations keys that NO instance this
	// node started lists a tool for, sorted. Such a key overrides nothing
	// here. Only the row can say it: one instance's listing is not the
	// server's, because a per-role child serves what its seat's credentials
	// reach, and a key one seat's child lacks may be exactly right for the
	// next one's. Across nodes the same intersection is the status answer's.
	//
	// Whole keys and every one of them, unlike Error: the list is bounded by
	// the entry's own configuration, as the row count is, rather than by
	// anything a seat count multiplies — and a key cut short would name a
	// tool nobody wrote.
	Unlisted []string
}

// MaxMCPErrorBytes bounds [MCPServerStatus.Error] and [MCPServerStatus.Wait]
// on the wire.
//
// Two kinds of text arrive here. A start failure's is a handshake error or a
// child's last stderr line, whose useful part — "command not found", "401",
// "connection refused" — is at its head. The engine's own sentence for a
// server it held back, for a value that resolves to nothing, is the other,
// and its useful part is all of it: the variable, then each remedy, the last
// naming the key to declare optional where a server can run without a host's
// setting. At 240 bytes that last remedy never arrived: the tools guide's own
// `NPM_CONFIG_REGISTRY` case is about 300 bytes, and a per-role child missing
// a setting while its seat's token is being minted about 380. 512 carries
// both under names of ordinary length. A row carries text only while
// an instance has failed or waits, so the bound costs a healthy server's row
// nothing on the heartbeat it is re-sent with; the full text is in the node's
// own log line either way (`mcp_server_failed`,
// `mcp_server_credential_missing`), which is where a person fixing it reads.
const MaxMCPErrorBytes = 512

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
	if s.AdminPosture != "" {
		// OMITTED WHEN UNSET, for [StatusFromMeta]'s reason: a node
		// that serves no API has no posture to state, and an empty
		// string on the wire would make it look like a peer whose
		// posture differs from everybody's.
		out["admin"] = string(s.AdminPosture)
	}
	if s.AppliersTotal > 0 {
		// OMITTED where the node runs none — a node without `data` — so
		// it reads absent rather than "0 of 0", which a reader would
		// otherwise have to know is not a stalled applier.
		out["appliers_ready"] = s.AppliersReady
		out["appliers_total"] = s.AppliersTotal
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
			if m.ErrorSeat != uuid.Nil {
				row["error_seat"] = m.ErrorSeat.String()
			}
			// OMITTED WHEN NOTHING WAITS, which is every row but a
			// seat's during provisioning: absent reads as zero.
			if m.Waiting > 0 {
				row["waiting"] = m.Waiting
			}
			if m.Wait != "" {
				row["wait"] = textcut.Within(m.Wait, MaxMCPErrorBytes)
			}
			if m.WaitSeat != uuid.Nil {
				row["wait_seat"] = m.WaitSeat.String()
			}
			if len(m.Unlisted) > 0 {
				row["unlisted"] = m.Unlisted
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
// A node that publishes no status — its status hook overran its budget on
// that beat, so its heartbeat carried the placement half alone — is not a node
// with no work in flight. Reporting it as 0 would draw an idle row for a
// process that is simply not saying, which is the confident-zero mistake the
// whole surface is written to avoid.
//
// A key this build does not know is IGNORED rather than refused: a successor
// sharing the fleet publishes what it adds beside what this build reads, and a
// status that failed to decode over it would read as absent for as long as the
// successor ran.
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
	}
	status.Draining, _ = raw["draining"].(bool)
	if at, err := time.Parse(time.RFC3339, stringFromMeta(raw["started_at"])); err == nil {
		status.StartedAt = at
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
			ErrorSeat: seatFromMeta(row["error_seat"]),
			Waiting:   intFromMeta(row["waiting"]),
			Wait:      stringFromMeta(row["wait"]),
			WaitSeat:  seatFromMeta(row["wait_seat"]),
			Unlisted:  stringsFromMeta(row["unlisted"]),
		}
		m.Shared, _ = row["shared"].(bool)
		status.MCP = append(status.MCP, m)
	}
	return status, true
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
