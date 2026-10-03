// What each configured MCP server did on each live node.

package queries

import (
	"cmp"
	"context"
	"maps"
	"slices"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
)

// MCPServerState is one server's condition across the fleet, decided here
// from every node's report so a screen never re-derives it.
//
// FIVE VALUES, because each calls for something different from the person
// reading it: nothing to do, a node to look at, a server to fix, a grant to
// write, and a fleet that has not said.
type MCPServerState string

const (
	// MCPRunning — every instance any node launched started and listed
	// its tools.
	MCPRunning MCPServerState = "running"
	// MCPPartial — some instances started and some did not: a node's
	// environment, or one seat's credentials, rather than the server.
	MCPPartial MCPServerState = "partial"
	// MCPFailing — instances were launched and none started: the server
	// itself, its command, its address or the credentials every seat
	// shares.
	MCPFailing MCPServerState = "failing"
	// MCPNotStarted — every node that reports started nothing for it: a
	// per-seat template no seat on a live node declares credentials for,
	// or a server no node has applied yet.
	MCPNotStarted MCPServerState = "not_started"
	// MCPUnreported — no live node publishes its MCP report: every one
	// runs a build older than the report. Not "not started", which would
	// be a claim about servers nobody was asked about.
	MCPUnreported MCPServerState = "unreported"
)

// MCPServerStates is every state, for the gate holding the dashboard's copy.
var MCPServerStates = []MCPServerState{MCPRunning, MCPPartial, MCPFailing, MCPNotStarted, MCPUnreported}

// Valid reports whether s is a state this build sends.
func (s MCPServerState) Valid() bool { return slices.Contains(MCPServerStates, s) }

// MCPStatusAnswer is `mcp_servers_status`.
type MCPStatusAnswer struct {
	// Servers in name order: every server the active configuration
	// declares, then any a node reports that it does not (a node still
	// on an older revision, mid-rollout). ALWAYS A LIST.
	Servers []MCPServerRow `json:"servers"`
	// Nodes are the live nodes in id order, each saying whether it
	// reports at all — the columns every server's row is drawn against.
	Nodes []MCPStatusNode `json:"nodes"`
}

// MCPStatusNode is one live node.
type MCPStatusNode struct {
	ID string `json:"id"`
	// Reported is whether the node publishes its MCP report. False is a
	// build older than the report, or a heartbeat that carried no status:
	// its cells are UNKNOWN, never zero.
	Reported bool `json:"reported"`
}

// MCPServerRow is one server: what the configuration declares, and what the
// nodes did with it.
type MCPServerRow struct {
	Name string `json:"name"`
	// Configured is whether this node's active configuration declares
	// it. False only for a server a node reports and the configuration no
	// longer (or does not yet) carries.
	Configured bool `json:"configured"`
	// Shared is one instance for the company rather than one per seat
	// that declares credentials for it. From the configuration where it
	// declares the server, else from a node's report.
	Shared bool `json:"shared"`
	// Transport, Command, Args and URL are the launch as configured —
	// the parts that are not credentials (`env` and `headers` are, and
	// are not here). Empty on a server the configuration does not carry.
	Transport string   `json:"transport"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	URL       string   `json:"url"`

	State MCPServerState `json:"state"`
	// Started and Failed are summed over the nodes that report; Tools is
	// the largest count one started instance serves.
	Started int `json:"started"`
	Failed  int `json:"failed"`
	Tools   int `json:"tools"`
	// Nodes is one cell per live node, in [MCPStatusAnswer.Nodes] order.
	Nodes []MCPServerNode `json:"nodes"`
}

// MCPServerNode is what one node did with one server.
type MCPServerNode struct {
	Node string `json:"node"`
	// Reported mirrors the node's own [MCPStatusNode.Reported]: false
	// leaves every count below unknown rather than zero.
	Reported bool `json:"reported"`
	Started  int  `json:"started"`
	Failed   int  `json:"failed"`
	Tools    int  `json:"tools"`
	// Error is one failed instance's reason, bounded on the wire, and
	// ErrorSeat the seat it was launched for ("" for a shared server), by
	// the handle it answers to now. The whole text is the node's
	// `mcp_server_failed` log line.
	Error     string `json:"error"`
	ErrorSeat string `json:"error_seat"`
}

// mcpServersStatus answers what each MCP server did on each node.
//
// FROM THE HEARTBEATS, NOT A FAN-OUT. Every node re-publishes what its starts
// concluded on its presence lease ([coord.NodeStatus.MCP]), so one listing of
// the lease table is every node's answer at once — there is no node that did
// not answer in time, only one whose build does not report, and that is a
// column of its own rather than a row of zeros.
//
// ON THE CONFIGURATION READ: it names the launch commands and the first line
// of each failure, which is the configuration and what became of it rather
// than the company's work — `/config` shows the same reader the same commands.
func (s Sources) mcpServersStatus(ctx context.Context, _ Params) (any, error) {
	leases, err := s.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		// THE LEASE TABLE IS THE ANSWER, so an unreadable one is an
		// error rather than a fleet with no nodes: "no node started
		// github" drawn over a store blip is the wrong thing to act on.
		return nil, err
	}

	type report struct {
		node     string
		reported bool
		servers  map[string]coord.MCPServerStatus
	}
	reports := make([]report, 0, len(leases))
	for _, lease := range leases {
		r := report{node: nameIn(coord.ClassNode, lease.Resource)}
		if status, ok := coord.StatusFromMeta(lease.Meta); ok && slices.Contains(status.Features, coord.FeatureMCPStatus) {
			// ADVERTISED, so an empty list is "started none" — the
			// feature's own contract — rather than "did not say".
			r.reported = true
			r.servers = make(map[string]coord.MCPServerStatus, len(status.MCP))
			for _, m := range status.MCP {
				r.servers[m.Server] = m
			}
		}
		reports = append(reports, r)
	}
	slices.SortFunc(reports, func(a, b report) int { return cmp.Compare(a.node, b.node) })

	out := MCPStatusAnswer{
		Servers: []MCPServerRow{},
		Nodes:   make([]MCPStatusNode, 0, len(reports)),
	}
	for _, r := range reports {
		out.Nodes = append(out.Nodes, MCPStatusNode{ID: r.node, Reported: r.reported})
	}

	rows := map[string]*MCPServerRow{}
	var organization *org.Organization
	if s.Company != nil {
		company, roster := s.Company()
		if company != nil {
			for i := range company.MCPServers {
				rows[company.MCPServers[i].Name] = configuredRow(&company.MCPServers[i])
			}
			organization = roster
		}
	}
	// A SERVER ONLY A NODE KNOWS, which is a node on another revision: the
	// report is still true of that node, and dropping it would hide the
	// one server still running somewhere after it was removed.
	for _, r := range reports {
		for name, m := range r.servers {
			if _, known := rows[name]; !known {
				rows[name] = &MCPServerRow{Name: name, Shared: m.Shared, Args: []string{}}
			}
		}
	}

	anyReported := slices.ContainsFunc(reports, func(r report) bool { return r.reported })
	for _, name := range slices.Sorted(maps.Keys(rows)) {
		row := rows[name]
		row.Nodes = make([]MCPServerNode, 0, len(reports))
		for _, r := range reports {
			cell := MCPServerNode{Node: r.node, Reported: r.reported}
			if m, ok := r.servers[name]; ok {
				cell.Started, cell.Failed, cell.Tools = m.Started, m.Failed, m.Tools
				cell.Error = m.Error
				cell.ErrorSeat = failedSeat(organization, m.ErrorSeat)
				row.Started += m.Started
				row.Failed += m.Failed
				row.Tools = max(row.Tools, m.Tools)
			}
			row.Nodes = append(row.Nodes, cell)
		}
		row.State = mcpState(anyReported, row.Started, row.Failed)
		out.Servers = append(out.Servers, *row)
	}
	return out, nil
}

// failedSeat names the seat a failed instance was launched for by the handle
// it answers to now.
//
// A NODE REPORTS THE SEAT'S ID, which no rename moves, and the screen reads a
// handle; resolved here, on this answer's own chart reading, rather than by
// the reporting node, whose chart a rename may not have reached. A seat this
// chart no longer holds is named by its id — the honest fallback, as a lease's
// is in `fleet` — and a shared server's failure names none.
func failedSeat(organization *org.Organization, id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	if organization != nil {
		if role := organization.AgentSeatByID(id); role != nil {
			return role.Handle()
		}
	}
	return id.String()
}

// configuredRow is a server's launch as the configuration declares it, with
// no credential in it.
func configuredRow(m *config.MCPServer) *MCPServerRow {
	return &MCPServerRow{
		Name:       m.Name,
		Configured: true,
		Shared:     m.IsShared(),
		Transport:  string(m.Kind()),
		Command:    m.Command,
		Args:       append([]string{}, m.Args...),
		URL:        m.URL,
	}
}

// mcpState is the ONE reading of a server's counts.
func mcpState(anyReported bool, started, failed int) MCPServerState {
	switch {
	case !anyReported:
		return MCPUnreported
	case started > 0 && failed > 0:
		return MCPPartial
	case failed > 0:
		return MCPFailing
	case started > 0:
		return MCPRunning
	default:
		return MCPNotStarted
	}
}
