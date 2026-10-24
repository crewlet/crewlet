// What each configured MCP server is doing on each live node.

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
// SIX VALUES, because each calls for something different from the person
// reading it: nothing to do, a node to look at, a server to fix, a grant to
// write, a provisioning pass to let finish, and a fleet that has not said.
type MCPServerState string

const (
	// MCPRunning — every instance any node launched is serving: it
	// started, listed its tools and was up at its node's last beat.
	MCPRunning MCPServerState = "running"
	// MCPPartial — some instances are serving and some are not: a node's
	// environment, one seat's credentials, or one child that died and is
	// being restarted, rather than the server.
	MCPPartial MCPServerState = "partial"
	// MCPFailing — instances were launched and none is serving: the
	// server itself, its command, its address or the credentials every
	// seat shares — or every instance went down and is being restarted.
	MCPFailing MCPServerState = "failing"
	// MCPWaiting — nothing was launched and nothing failed, and a node
	// holds back a seat's instance for a credential a provisioning pass
	// mints for that seat and has not sealed yet, where nothing that pass
	// last reported says it cannot. NOT a failure: it starts once the seal
	// reaches the node. A pass that cannot mint until a person acts makes
	// the instance failed instead, on the node's own judgement, so this
	// state is never a wait with no end. Ranked below a failure, so a
	// server one seat waits on while another's start failed is failing.
	MCPWaiting MCPServerState = "waiting"
	// MCPNotStarted — every node that reports started nothing for it: a
	// per-seat template no seat on a live node declares credentials for,
	// or a server no node has applied yet.
	MCPNotStarted MCPServerState = "not_started"
	// MCPUnreported — no live node's latest heartbeat carried its MCP
	// report (its status hook did not answer within its share of the beat,
	// or no node is live), so
	// nothing can be said about any server. Not "not started", which would
	// be a claim nobody made.
	MCPUnreported MCPServerState = "unreported"
)

// MCPServerStates is every state, for the gate holding the dashboard's copy.
var MCPServerStates = []MCPServerState{
	MCPRunning, MCPPartial, MCPFailing, MCPWaiting, MCPNotStarted, MCPUnreported,
}

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
	// Reported is whether the node's latest heartbeat carried its status.
	// False when its status hook did not answer within its share of the
	// beat, and its cells are UNKNOWN, never zero.
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
	// Started, Failed and Waiting are summed over the nodes that report;
	// Tools is the largest count one started instance serves. Waiting is
	// neither of the others: a seat's instance held back, unstarted, for a
	// credential a provisioning pass has not sealed yet.
	Started int `json:"started"`
	Failed  int `json:"failed"`
	Waiting int `json:"waiting"`
	Tools   int `json:"tools"`
	// UnlistedAnnotations are the entry's tool_annotations keys that no
	// instance started on a reporting node lists a tool for — by the
	// server's own name or with the entry's tool_prefix — sorted. Each
	// overrides nothing anywhere it runs: a misspelling, or a tool the
	// server no longer serves. A key only SOME instances lack is not here,
	// because a per-role child serves what its seat's credentials reach and
	// that key is right for the others. Empty where no instance started,
	// since nothing listed anything to judge against. ALWAYS A LIST.
	UnlistedAnnotations []string `json:"unlisted_annotations"`
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
	Waiting  int  `json:"waiting"`
	Tools    int  `json:"tools"`
	// Error is one failed instance's reason, bounded on the wire, and
	// ErrorSeat the seat it was launched for ("" for a shared server), by
	// its handle. The whole text is the node's `mcp_server_failed` log
	// line for a start that failed, its `mcp_server_credential_missing`
	// line for a seat's instance held back for a credential that will not
	// come without somebody acting, and its `server_exited` or
	// `server_restart_failed` line for an instance that went down after
	// starting.
	Error     string `json:"error"`
	ErrorSeat string `json:"error_seat"`
	// Wait is one waiting instance's sentence — the variables it waits for
	// and the integration that mints them — and WaitSeat its seat, by its
	// handle, chosen and bounded as Error and ErrorSeat are. The whole
	// sentence is the node's `mcp_server_awaiting_credential` log line.
	Wait     string `json:"wait"`
	WaitSeat string `json:"wait_seat"`
}

// mcpServersStatus answers what each MCP server did on each node.
//
// FROM THE HEARTBEATS, NOT A FAN-OUT. Every node re-publishes its servers as
// they are at each beat on its presence lease ([coord.NodeStatus.MCP]) — a
// child that died since it started counts as failed until it is back — so one
// listing of the lease table is every node's answer at once. There is no node
// that did not answer in time, only one whose last heartbeat carried no
// status, and that is a column of its own rather than a row of zeros.
//
// ADMIN'S, AS THE CONFIGURATION IS (ADR-0033): it names the launch commands
// and the first line of each failure, which is the configuration and what
// became of it rather than the company's work — `/config` shows the same
// reader the same commands.
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
		if status, ok := coord.StatusFromMeta(lease.Meta); ok {
			// PUBLISHED, so an empty list is "started none" — the
			// node always reports what it started beside its status —
			// rather than "did not say".
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
		// The keys every node that started an instance names as listed by
		// none of its own — the node's row is already that intersection
		// over its instances, so this is the same question asked of the
		// fleet.
		var unlisted []string
		judged := false
		for _, r := range reports {
			cell := MCPServerNode{Node: r.node, Reported: r.reported}
			if m, ok := r.servers[name]; ok {
				cell.Started, cell.Failed, cell.Waiting, cell.Tools = m.Started, m.Failed, m.Waiting, m.Tools
				cell.Error = m.Error
				cell.ErrorSeat = instanceSeat(organization, m.ErrorSeat)
				cell.Wait = m.Wait
				cell.WaitSeat = instanceSeat(organization, m.WaitSeat)
				row.Started += m.Started
				row.Failed += m.Failed
				row.Waiting += m.Waiting
				row.Tools = max(row.Tools, m.Tools)
				if m.Started > 0 {
					if !judged {
						unlisted, judged = slices.Clone(m.Unlisted), true
					} else {
						unlisted = slices.DeleteFunc(unlisted, func(key string) bool {
							return !slices.Contains(m.Unlisted, key)
						})
					}
				}
			}
			row.Nodes = append(row.Nodes, cell)
		}
		slices.Sort(unlisted)
		row.UnlistedAnnotations = append([]string{}, unlisted...)
		row.State = mcpState(anyReported, row.Started, row.Failed, row.Waiting)
		out.Servers = append(out.Servers, *row)
	}
	return out, nil
}

// instanceSeat names the seat a failed instance was launched for, or a waiting
// one is held back for, by its handle.
//
// A NODE REPORTS THE SEAT'S ID, as a lease names its seat, and the screen reads
// a handle; resolved here, on this answer's own org. A seat this org no longer
// holds is named by its id — the honest fallback, as a lease's is in `fleet`
// — and a shared server's failure names none.
func instanceSeat(organization *org.Organization, id uuid.UUID) string {
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
//
// A WAITING INSTANCE DECIDES THE STATE ONLY WHERE NOTHING ELSE DOES: a server
// serving some seats while another's credential is minted is running, since
// nothing is wrong with it, and one with a failure is partial or failing
// whatever waits beside it, since the failure is what somebody has to act on.
func mcpState(anyReported bool, started, failed, waiting int) MCPServerState {
	switch {
	case !anyReported:
		return MCPUnreported
	case started > 0 && failed > 0:
		return MCPPartial
	case failed > 0:
		return MCPFailing
	case started > 0:
		return MCPRunning
	case waiting > 0:
		return MCPWaiting
	default:
		return MCPNotStarted
	}
}
